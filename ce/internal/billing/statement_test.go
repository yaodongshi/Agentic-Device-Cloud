package billing

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"
)

// memBillRepo is the in-memory BillRepo for service tests, mirroring the
// PG repository semantics (sentinels, period uniqueness, newest-first
// list order) so the idempotent generate flow runs without a database.
type memBillRepo struct {
	mu       sync.Mutex
	stmts    map[string]*Statement
	byPeriod map[[2]int]string // (year, month) -> id
	nextID   int
	// raceMode simulates a concurrent generate that wins the insert
	// between our FindByPeriod and InsertStatement calls: the first
	// FindByPeriod misses and InsertStatement reports the duplicate.
	raceMode bool
	findMiss bool
}

func newMemBillRepo() *memBillRepo {
	return &memBillRepo{stmts: map[string]*Statement{}, byPeriod: map[[2]int]string{}}
}

func cloneStatement(st *Statement) *Statement {
	if st == nil {
		return nil
	}
	cp := *st
	if st.PaidAt != nil {
		t := *st.PaidAt
		cp.PaidAt = &t
	}
	return &cp
}

func (r *memBillRepo) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.stmts)
}

func (r *memBillRepo) ListStatements(ctx context.Context, tenantID string, p Page) ([]*Statement, int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var all []*Statement
	for _, st := range r.stmts {
		if st.TenantID == tenantID {
			all = append(all, st)
		}
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].Year != all[j].Year {
			return all[i].Year > all[j].Year
		}
		return all[i].Month > all[j].Month
	})
	total := len(all)
	start := p.offset()
	if start > len(all) {
		start = len(all)
	}
	end := start + p.Size
	if end > len(all) {
		end = len(all)
	}
	out := make([]*Statement, 0, end-start)
	for _, st := range all[start:end] {
		out = append(out, cloneStatement(st))
	}
	return out, total, nil
}

func (r *memBillRepo) GetStatement(ctx context.Context, statementID string) (*Statement, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st, ok := r.stmts[statementID]
	if !ok {
		return nil, ErrStatementNotFound
	}
	return cloneStatement(st), nil
}

func (r *memBillRepo) FindByPeriod(ctx context.Context, tenantID string, year, month int) (*Statement, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.raceMode && r.findMiss {
		r.findMiss = false
		return nil, ErrStatementNotFound
	}
	id, ok := r.byPeriod[[2]int{year, month}]
	if !ok {
		return nil, ErrStatementNotFound
	}
	st := r.stmts[id]
	if st.TenantID != tenantID {
		return nil, ErrStatementNotFound
	}
	return cloneStatement(st), nil
}

func (r *memBillRepo) InsertStatement(ctx context.Context, st *Statement) (*Statement, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.raceMode {
		return nil, ErrStatementExists
	}
	key := [2]int{st.Year, st.Month}
	if _, exists := r.byPeriod[key]; exists {
		return nil, ErrStatementExists
	}
	r.nextID++
	st.ID = testBillID + "-" + strconv.Itoa(r.nextID)
	r.stmts[st.ID] = cloneStatement(st)
	r.byPeriod[key] = st.ID
	return cloneStatement(st), nil
}

func (r *memBillRepo) HasOverdue(ctx context.Context, tenantID string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, st := range r.stmts {
		if st.TenantID == tenantID && st.Status == StatusOverdue {
			return true, nil
		}
	}
	return false, nil
}

func newTestService(agg Aggregator, repo *memBillRepo) *Service {
	s := NewService(NewEngine(agg), repo)
	s.now = func() time.Time {
		return time.Date(2026, 8, 14, 8, 0, 0, 0, time.UTC)
	}
	return s
}

func TestServiceGenerateIdempotent(t *testing.T) {
	repo := newMemBillRepo()
	svc := newTestService(&fakeAggregator{sum: &UsageSummary{DevicePeak: 45, ToolCalls: 500, Tokens: 7000}}, repo)

	st1, created, err := svc.Generate(context.Background(), "tenant-1", 2026, 8)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if !created || st1.ID == "" {
		t.Fatalf("first generate = created %v id %q, want created true with id", created, st1.ID)
	}
	if st1.Status != StatusGenerated || st1.TotalFeeFen != 331667+83333+70 {
		t.Fatalf("statement = %+v", st1)
	}
	// 45 devices: 20 included + 25 at 400 yuan/year -> 1M fen/year ->
	// 833.33 yuan/month (83333 fen); tokens 7000 -> 70 fen.
	if st1.DeviceFeeFen != 83333 || st1.TokenFeeFen != 70 {
		t.Fatalf("fees = device %d token %d, want 83333/70", st1.DeviceFeeFen, st1.TokenFeeFen)
	}

	st2, created2, err := svc.Generate(context.Background(), "tenant-1", 2026, 8)
	if err != nil {
		t.Fatalf("second Generate: %v", err)
	}
	if created2 || st2.ID != st1.ID {
		t.Fatalf("second generate = created %v id %q, want created false id %q", created2, st2.ID, st1.ID)
	}
	if repo.count() != 1 {
		t.Fatalf("repo holds %d statements, want 1", repo.count())
	}
}

// A losing concurrent generate must return the winner's row, not fail.
func TestServiceGenerateRace(t *testing.T) {
	repo := newMemBillRepo()
	repo.raceMode = true
	repo.findMiss = true
	// The winner's row is already stored for the period.
	repo.stmts["winner-id"] = &Statement{
		ID: "winner-id", TenantID: "tenant-1", Year: 2026, Month: 8,
		Status: StatusGenerated, TotalFeeFen: 123,
	}
	repo.byPeriod[[2]int{2026, 8}] = "winner-id"
	svc := newTestService(&fakeAggregator{sum: &UsageSummary{}}, repo)

	st, created, err := svc.Generate(context.Background(), "tenant-1", 2026, 8)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if created || st.ID != "winner-id" {
		t.Fatalf("race generate = created %v id %q, want created false id winner-id", created, st.ID)
	}
}

func TestServiceGenerateErrors(t *testing.T) {
	repo := newMemBillRepo()
	svc := newTestService(&fakeAggregator{}, repo)

	if _, _, err := svc.Generate(context.Background(), "t", 2026, 13); !errors.Is(err, ErrInvalidPeriod) {
		t.Fatalf("want ErrInvalidPeriod, got %v", err)
	}
	if _, _, err := svc.Generate(context.Background(), "t", 1999, 1); !errors.Is(err, ErrInvalidPeriod) {
		t.Fatalf("want ErrInvalidPeriod, got %v", err)
	}
	boom := errors.New("agg down")
	svc.engine.agg = &fakeAggregator{err: boom}
	if _, _, err := svc.Generate(context.Background(), "t", 2026, 8); !errors.Is(err, boom) {
		t.Fatalf("want aggregator error, got %v", err)
	}
}

func TestServiceOverdue(t *testing.T) {
	repo := newMemBillRepo()
	svc := newTestService(&fakeAggregator{}, repo)
	repo.stmts["d"] = &Statement{ID: "d", TenantID: "tenant-1", Year: 2026, Month: 7, Status: StatusOverdue}

	overdue, err := svc.Overdue(context.Background(), "tenant-1")
	if err != nil {
		t.Fatalf("Overdue: %v", err)
	}
	if !overdue {
		t.Fatal("want overdue true")
	}
	overdue, err = svc.Overdue(context.Background(), "tenant-2")
	if err != nil {
		t.Fatalf("Overdue: %v", err)
	}
	if overdue {
		t.Fatal("want overdue false for tenant-2")
	}
}

type fakeAudit struct{ calls int64 }

func (f *fakeAudit) CountToolCalls(ctx context.Context, tenantID string, from, to time.Time) (int64, error) {
	return f.calls, nil
}

func TestServiceReconcile(t *testing.T) {
	repo := newMemBillRepo()
	svc := newTestService(&fakeAggregator{sum: &UsageSummary{ToolCalls: 100}}, repo)

	if _, err := svc.Reconcile(context.Background(), "t", 2026, 8); !errors.Is(err, ErrAuditNotWired) {
		t.Fatalf("want ErrAuditNotWired, got %v", err)
	}

	svc.SetAudit(&fakeAudit{calls: 100})
	rec, err := svc.Reconcile(context.Background(), "t", 2026, 8)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !rec.Matched || rec.ToolCallsUsage != 100 || rec.ToolCallsAudit != 100 {
		t.Fatalf("reconciliation = %+v, want matched 100/100", rec)
	}
	if rec.Period != "2026-08" {
		t.Fatalf("period = %q, want 2026-08", rec.Period)
	}

	svc.SetAudit(&fakeAudit{calls: 97})
	rec, err = svc.Reconcile(context.Background(), "t", 2026, 8)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if rec.Matched || rec.ToolCallsAudit != 97 {
		t.Fatalf("reconciliation = %+v, want mismatched 100/97", rec)
	}
}
