package adminapi

// Handler tests for the FR-016 billing endpoints (design/82 B3.2). The
// billing.Service is the production implementation; only its repository
// and aggregation seams are in-memory fakes, so the idempotent generate
// flow and the audit reconciliation assertion run end to end without a
// database.

import (
	"context"
	"net/http"
	"sort"
	"sync"
	"testing"
	"time"

	"adc.dev/ce/internal/adminauth"
	"adc.dev/ce/internal/billing"
)

// memBillingRepo is the in-memory billing.BillRepo mirroring the PG
// semantics the handlers rely on (sentinels, period uniqueness,
// newest-first list order).
type memBillingRepo struct {
	mu       sync.Mutex
	stmts    map[string]*billing.Statement
	byPeriod map[[2]int]string
	nextID   int
}

func newMemBillingRepo() *memBillingRepo {
	return &memBillingRepo{stmts: map[string]*billing.Statement{}, byPeriod: map[[2]int]string{}}
}

func cloneBillStatement(st *billing.Statement) *billing.Statement {
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

func (r *memBillingRepo) ListStatements(ctx context.Context, tenantID string, p billing.Page) ([]*billing.Statement, int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var all []*billing.Statement
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
	start := (p.Number - 1) * p.Size
	if start > len(all) {
		start = len(all)
	}
	end := start + p.Size
	if end > len(all) {
		end = len(all)
	}
	out := make([]*billing.Statement, 0, end-start)
	for _, st := range all[start:end] {
		out = append(out, cloneBillStatement(st))
	}
	return out, total, nil
}

func (r *memBillingRepo) GetStatement(ctx context.Context, statementID string) (*billing.Statement, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st, ok := r.stmts[statementID]
	if !ok {
		return nil, billing.ErrStatementNotFound
	}
	return cloneBillStatement(st), nil
}

func (r *memBillingRepo) FindByPeriod(ctx context.Context, tenantID string, year, month int) (*billing.Statement, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id, ok := r.byPeriod[[2]int{year, month}]
	if !ok {
		return nil, billing.ErrStatementNotFound
	}
	st := r.stmts[id]
	if st.TenantID != tenantID {
		return nil, billing.ErrStatementNotFound
	}
	return cloneBillStatement(st), nil
}

func (r *memBillingRepo) InsertStatement(ctx context.Context, st *billing.Statement) (*billing.Statement, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := [2]int{st.Year, st.Month}
	if _, exists := r.byPeriod[key]; exists {
		return nil, billing.ErrStatementExists
	}
	r.nextID++
	st.ID = uuidOf(700000 + r.nextID)
	r.stmts[st.ID] = cloneBillStatement(st)
	r.byPeriod[key] = st.ID
	return cloneBillStatement(st), nil
}

func (r *memBillingRepo) HasOverdue(ctx context.Context, tenantID string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, st := range r.stmts {
		if st.TenantID == tenantID && st.Status == billing.StatusOverdue {
			return true, nil
		}
	}
	return false, nil
}

// seed inserts a statement row directly (simulating a previously
// generated or externally marked statement).
func (r *memBillingRepo) seed(st *billing.Statement) *billing.Statement {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextID++
	st.ID = uuidOf(700000 + r.nextID)
	r.stmts[st.ID] = cloneBillStatement(st)
	r.byPeriod[[2]int{st.Year, st.Month}] = st.ID
	return cloneBillStatement(st)
}

// fakeBillingAgg is a fixed-summary billing.Aggregator: 100 device peak,
// 1234 tool calls, 8M tokens — the doc/04 typical-customer shape
// (100 devices, ~800 万 tokens/month).
type fakeBillingAgg struct{ sum billing.UsageSummary }

func (f *fakeBillingAgg) Aggregate(ctx context.Context, tenantID string, from, to time.Time) (*billing.UsageSummary, error) {
	cp := f.sum
	return &cp, nil
}

// fakeBillingAudit returns a fixed audit-log tool_call count.
type fakeBillingAudit struct{ calls int64 }

func (f *fakeBillingAudit) CountToolCalls(ctx context.Context, tenantID string, from, to time.Time) (int64, error) {
	return f.calls, nil
}

// defaultBillingAgg mirrors the fakeBillingAgg fixture values used by the
// env builder.
var defaultBillingAgg = fakeBillingAgg{sum: billing.UsageSummary{DevicePeak: 100, ToolCalls: 1234, Tokens: 8000000}}

// newBillingTestEnv builds a Server with the production billing.Service
// wired over in-memory seams. fixedNow (2026-08-14) makes the future
// period rejection deterministic.
func newBillingTestEnv(t *testing.T) (*testEnv, *memBillingRepo) {
	t.Helper()
	st := newMemStore()
	sess := &memSessions{m: map[string]*adminauth.Session{}}
	aud := &memAudit{}
	srv := NewServer(st.tenants, st.devices, st.keys, sess)
	srv.Audit = aud
	srv.Now = func() time.Time { return fixedNow }
	repo := newMemBillingRepo()
	svc := billing.NewService(billing.NewEngine(&defaultBillingAgg), repo)
	svc.SetAudit(&fakeBillingAudit{calls: 1234})
	srv.Billing = svc
	return &testEnv{store: st, sessions: sess, audit: aud, srv: srv, handler: srv.Handler()}, repo
}

func TestBillingGenerateIdempotent(t *testing.T) {
	e, repo := newBillingTestEnv(t)
	tenant := e.createTenant(t, "bill-tenant", "Bill Tenant", nil)
	tok := e.token(t, []string{rolePlatformAdmin}, "")

	body := generateBillingRequest{TenantID: tenant.TenantID, Period: "2026-08"}
	rec := e.do(http.MethodPost, "/v1/admin/billing/generate", tok, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body: %s", rec.Code, rec.Body.String())
	}
	var out billingGenerateResponse
	e.decode(t, rec, &out)
	if !out.Created {
		t.Fatal("want created=true")
	}
	// doc/04 pricing for the fixture: 100 devices -> 20 included + 80 at
	// 400 yuan/year -> 2666.67 yuan/month; 8M tokens -> 800 yuan/month;
	// subscription 3316.67 yuan/month.
	st := out.Statement
	if st.Period != "2026-08" || st.DevicePeak != 100 || st.ToolCalls != 1234 || st.Tokens != 8000000 {
		t.Fatalf("statement usage = %+v", st)
	}
	if st.SubscriptionFeeFen != 331667 || st.DeviceFeeFen != 266667 || st.TokenFeeFen != 80000 || st.TotalFeeFen != 678334 {
		t.Fatalf("statement fees = %+v, want 331667/266667/80000/678334", st)
	}
	if st.Breakdown.Tier400Devices != 80 || st.Breakdown.IncludedDevices != 20 {
		t.Fatalf("breakdown = %+v", st.Breakdown)
	}

	// Repeating the same month returns the existing statement (200,
	// created=false) instead of a second row.
	rec = e.do(http.MethodPost, "/v1/admin/billing/generate", tok, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("repeat status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	e.decode(t, rec, &out)
	if out.Created || out.Statement.ID != st.ID {
		t.Fatalf("repeat = created %v id %q, want created=false id %q", out.Created, out.Statement.ID, st.ID)
	}
	repo.mu.Lock()
	n := len(repo.stmts)
	repo.mu.Unlock()
	if n != 1 {
		t.Fatalf("repo holds %d statements, want 1", n)
	}
}

func TestBillingGenerateValidation(t *testing.T) {
	e, _ := newBillingTestEnv(t)
	tenant := e.createTenant(t, "bill-tenant-2", "Bill Tenant 2", nil)

	// Generation is a platform_admin exclusive.
	tenantTok := e.token(t, []string{"tenant_admin"}, tenant.TenantID)
	rec := e.do(http.MethodPost, "/v1/admin/billing/generate", tenantTok,
		generateBillingRequest{TenantID: tenant.TenantID, Period: "2026-08"})
	e.assertError(t, rec, http.StatusForbidden, codeForbidden)

	tok := e.token(t, []string{rolePlatformAdmin}, "")
	cases := []struct {
		name string
		body generateBillingRequest
		code string
	}{
		{"bad tenant id", generateBillingRequest{TenantID: "nope", Period: "2026-08"}, codeBadRequest},
		{"malformed period", generateBillingRequest{TenantID: tenant.TenantID, Period: "08-2026"}, codeBillingPeriodInvalid},
		{"month 13", generateBillingRequest{TenantID: tenant.TenantID, Period: "2026-13"}, codeBillingPeriodInvalid},
		{"future period", generateBillingRequest{TenantID: tenant.TenantID, Period: "2026-09"}, codeBillingPeriodInvalid},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := e.do(http.MethodPost, "/v1/admin/billing/generate", tok, c.body)
			e.assertError(t, rec, http.StatusBadRequest, c.code)
		})
	}
}

func TestBillingListDetailReconcile(t *testing.T) {
	e, _ := newBillingTestEnv(t)
	tenant := e.createTenant(t, "bill-tenant-3", "Bill Tenant 3", nil)
	tok := e.token(t, []string{rolePlatformAdmin}, "")

	rec := e.do(http.MethodPost, "/v1/admin/billing/generate", tok,
		generateBillingRequest{TenantID: tenant.TenantID, Period: "2026-08"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("generate status = %d; body: %s", rec.Code, rec.Body.String())
	}

	rec = e.do(http.MethodGet, "/v1/admin/billing/statements?tenant_id="+tenant.TenantID, tok, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d; body: %s", rec.Code, rec.Body.String())
	}
	var list billingListResponse
	e.decode(t, rec, &list)
	if list.Total != 1 || len(list.Items) != 1 || list.Items[0].Period != "2026-08" {
		t.Fatalf("list = %+v, want one 2026-08 statement", list)
	}

	// Detail carries the FR-016 reconciliation assertion: metered calls
	// (1234) match the audit-log count (1234).
	rec = e.do(http.MethodGet, "/v1/admin/billing/statements/"+list.Items[0].ID, tok, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("detail status = %d; body: %s", rec.Code, rec.Body.String())
	}
	var detail billingDetailResponse
	e.decode(t, rec, &detail)
	if detail.Reconciliation == nil || !detail.Reconciliation.Matched {
		t.Fatalf("reconciliation = %+v, want matched", detail.Reconciliation)
	}
	if detail.Reconciliation.ToolCallsUsage != 1234 || detail.Reconciliation.ToolCallsAudit != 1234 {
		t.Fatalf("reconciliation counts = %+v, want 1234/1234", detail.Reconciliation)
	}
}

func TestBillingDetailScoping(t *testing.T) {
	e, repo := newBillingTestEnv(t)
	tenantA := e.createTenant(t, "bill-tenant-a", "Bill Tenant A", nil)
	e.createTenant(t, "bill-tenant-b", "Bill Tenant B", nil)
	tok := e.token(t, []string{rolePlatformAdmin}, "")

	st := repo.seed(&billing.Statement{
		TenantID:           tenantA.TenantID,
		Year:               2026,
		Month:              7,
		SubscriptionFeeFen: 331667,
		TotalFeeFen:        331667,
		Status:             billing.StatusGenerated,
	})

	// A tenant admin of tenant B cannot read tenant A's statement.
	tenantBTok := e.token(t, []string{"tenant_admin"}, uuidOf(2))
	rec := e.do(http.MethodGet, "/v1/admin/billing/statements/"+st.ID, tenantBTok, nil)
	e.assertError(t, rec, http.StatusForbidden, codeCrossTenant)

	// Unknown statement ids answer 404 code 16002.
	rec = e.do(http.MethodGet, "/v1/admin/billing/statements/"+uuidOf(9999), tok, nil)
	e.assertError(t, rec, http.StatusNotFound, codeBillingStatementNotFound)
}

func TestBillingOverdue(t *testing.T) {
	e, repo := newBillingTestEnv(t)
	tenant := e.createTenant(t, "bill-tenant-4", "Bill Tenant 4", nil)
	tok := e.token(t, []string{rolePlatformAdmin}, "")

	// No OVERDUE statements yet.
	rec := e.do(http.MethodGet, "/v1/admin/billing/overdue?tenant_id="+tenant.TenantID, tok, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Overdue bool `json:"overdue"`
	}
	e.decode(t, rec, &out)
	if out.Overdue {
		t.Fatal("want overdue=false")
	}

	// Mark the previous month OVERDUE (payment ledger simulation) and the
	// seam flips — this is the flag the gateway quota soft-limit polls
	// (B3.3; degradation logic not implemented).
	repo.seed(&billing.Statement{
		TenantID:           tenant.TenantID,
		Year:               2026,
		Month:              7,
		SubscriptionFeeFen: 331667,
		TotalFeeFen:        331667,
		Status:             billing.StatusOverdue,
	})
	rec = e.do(http.MethodGet, "/v1/admin/billing/overdue?tenant_id="+tenant.TenantID, tok, nil)
	e.decode(t, rec, &out)
	if !out.Overdue {
		t.Fatal("want overdue=true")
	}

	// A tenant admin resolves their own tenant without naming it.
	tenantTok := e.token(t, []string{"tenant_admin"}, tenant.TenantID)
	rec = e.do(http.MethodGet, "/v1/admin/billing/overdue", tenantTok, nil)
	e.decode(t, rec, &out)
	if !out.Overdue {
		t.Fatal("want overdue=true for tenant-scoped admin")
	}
}

func TestBillingServiceNotWired(t *testing.T) {
	e, _ := newBillingTestEnv(t)
	e.srv.Billing = nil
	e.handler = e.srv.Handler()
	tok := e.token(t, []string{rolePlatformAdmin}, "")

	rec := e.do(http.MethodGet, "/v1/admin/billing/overdue?tenant_id="+uuidOf(1), tok, nil)
	e.assertError(t, rec, http.StatusInternalServerError, codeInternal)
}
