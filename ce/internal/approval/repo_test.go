package approval

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"sync"
	"testing"
	"time"
)

// fakeClock is a goroutine-safe clock seam so expiry tests can advance time
// deterministically.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock(t time.Time) *fakeClock { return &fakeClock{t: t} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// memTicketRepo is an in-memory TicketRepo. Every Transition serializes
// check-and-apply under one mutex, which emulates the PG single-statement
// conditional UPDATE (design/32 6.1): concurrent callers observe the
// already-mutated status/version of the winner, and only one decision can
// land. The decision logic itself is the production ApplyTransition.
type memTicketRepo struct {
	mu      sync.Mutex
	tickets map[string]*ApprovalTicket
	now     func() time.Time
}

func newMemTicketRepo(clock *fakeClock) *memTicketRepo {
	return &memTicketRepo{
		tickets: make(map[string]*ApprovalTicket),
		now:     clock.Now,
	}
}

func (r *memTicketRepo) Create(ctx context.Context, t *ApprovalTicket) (*ApprovalTicket, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	// Dedup mirror of uq_tickets_pending_dedup (design/32 6.3): only one
	// in-flight ticket per device/tool/params.
	for _, existing := range r.tickets {
		if existing.Status == StatusPending &&
			existing.DeviceID == t.DeviceID &&
			existing.ToolName == t.ToolName &&
			existing.ParamsHash == t.ParamsHash {
			cp := *existing
			return &cp, ErrTicketDedup
		}
	}
	cp := *t
	if cp.TicketID == "" {
		id, err := NewTicketID()
		if err != nil {
			return nil, err
		}
		cp.TicketID = id
	}
	r.tickets[cp.TicketID] = &cp
	return &cp, nil
}

func (r *memTicketRepo) Transition(ctx context.Context, ticketID string, wantVersion int, cmd TransitionCmd) (*ApprovalTicket, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.tickets[ticketID]
	if !ok {
		return nil, ErrTicketNotFound
	}
	if err := ApplyTransition(t, cmd, wantVersion, r.now()); err != nil {
		return nil, err
	}
	cp := *t
	return &cp, nil
}

func (r *memTicketRepo) Get(ctx context.Context, ticketID string) (*ApprovalTicket, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.tickets[ticketID]
	if !ok {
		return nil, ErrTicketNotFound
	}
	cp := *t
	return &cp, nil
}

func (r *memTicketRepo) FindExpired(ctx context.Context, now time.Time, limit int) ([]ApprovalTicket, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []ApprovalTicket
	for _, t := range r.tickets {
		if t.Status == StatusPending && !now.Before(t.ExpireAt) {
			out = append(out, *t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ExpireAt.Before(out[j].ExpireAt) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

var _ TicketRepo = (*memTicketRepo)(nil)

func ctxTODO() context.Context { return context.Background() }

// seedTicket stores a fresh PENDING ticket and returns its id.
func seedTicket(t *testing.T, repo *memTicketRepo, clock *fakeClock) string {
	t.Helper()
	ticket := newTestTicket(t)
	ticket.CreatedAt = clock.Now()
	ticket.ExpireAt = clock.Now().Add(DefaultExpiry)
	created, err := repo.Create(ctxTODO(), ticket)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return created.TicketID
}

func TestMemRepoGetNotFound(t *testing.T) {
	repo := newMemTicketRepo(newFakeClock(fixedClock()))
	if _, err := repo.Get(ctxTODO(), "missing"); !errors.Is(err, ErrTicketNotFound) {
		t.Errorf("Get error = %v, want ErrTicketNotFound", err)
	}
	if _, err := repo.Transition(ctxTODO(), "missing", 1, TransitionCmd{Status: StatusApproved}); !errors.Is(err, ErrTicketNotFound) {
		t.Errorf("Transition error = %v, want ErrTicketNotFound", err)
	}
}

func TestMemRepoCreateGetRoundtrip(t *testing.T) {
	clock := newFakeClock(fixedClock())
	repo := newMemTicketRepo(clock)
	id := seedTicket(t, repo, clock)

	got, err := repo.Get(ctxTODO(), id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.TicketID != id || got.Status != StatusPending || got.Version != 1 {
		t.Errorf("rebuilt ticket wrong: %+v", got)
	}
	if got.DeviceID != "device-1" || got.ToolName != "power_off" || string(got.Arguments) != `{"force":true}` {
		t.Errorf("fields lost in roundtrip: %+v", got)
	}
	if got.SecretKey == "" || got.SecretHash == "" || got.ParamsHash == "" {
		t.Errorf("secret/hash fields lost in roundtrip: %+v", got)
	}
}

func TestMemRepoCreateDedup(t *testing.T) {
	clock := newFakeClock(fixedClock())
	repo := newMemTicketRepo(clock)
	id1 := seedTicket(t, repo, clock)

	// Same device/tool/params while a PENDING ticket exists: rejected with
	// the existing ticket (design/32 6.3, ErrTicketDedup -> HTTP 200).
	dup := newTestTicket(t)
	dup.CreatedAt = clock.Now()
	dup.ExpireAt = clock.Now().Add(DefaultExpiry)
	got, err := repo.Create(ctxTODO(), dup)
	if !errors.Is(err, ErrTicketDedup) {
		t.Fatalf("duplicate Create error = %v, want ErrTicketDedup", err)
	}
	if got.TicketID != id1 {
		t.Errorf("dedup returned ticket %s, want existing %s", got.TicketID, id1)
	}

	// Once the first ticket leaves PENDING, the same params are allowed again.
	if _, err := repo.Transition(ctxTODO(), id1, 1, TransitionCmd{Status: StatusApproved, Approver: "e1"}); err != nil {
		t.Fatalf("approve: %v", err)
	}
	again := newTestTicket(t)
	again.CreatedAt = clock.Now()
	again.ExpireAt = clock.Now().Add(DefaultExpiry)
	created, err := repo.Create(ctxTODO(), again)
	if err != nil {
		t.Fatalf("Create after resolution: %v", err)
	}
	if created.TicketID == id1 {
		t.Error("dedup blocked a ticket after the previous one was resolved")
	}
}

func TestMemRepoTransitionHappyPath(t *testing.T) {
	clock := newFakeClock(fixedClock())
	repo := newMemTicketRepo(clock)
	id := seedTicket(t, repo, clock)

	resolved, err := repo.Transition(ctxTODO(), id, 1, TransitionCmd{Status: StatusApproved, Approver: "e1001", Comment: "ok"})
	if err != nil {
		t.Fatalf("Transition: %v", err)
	}
	if resolved.Status != StatusApproved || resolved.Version != 2 {
		t.Errorf("Status/Version = %q/%d, want APPROVED/2", resolved.Status, resolved.Version)
	}
	if resolved.Approver != "e1001" || resolved.Comment != "ok" {
		t.Errorf("decision snapshot = %q/%q", resolved.Approver, resolved.Comment)
	}
	if resolved.DecidedAt == nil || !resolved.DecidedAt.Equal(clock.Now()) {
		t.Errorf("DecidedAt = %v, want %v", resolved.DecidedAt, clock.Now())
	}
	// The repo copy is authoritative.
	stored, _ := repo.Get(ctxTODO(), id)
	if stored.Status != StatusApproved || stored.Version != 2 {
		t.Errorf("stored ticket = %q/v%d", stored.Status, stored.Version)
	}
}

func TestMemRepoDoubleDecisionSequential(t *testing.T) {
	clock := newFakeClock(fixedClock())
	repo := newMemTicketRepo(clock)
	id := seedTicket(t, repo, clock)

	if _, err := repo.Transition(ctxTODO(), id, 1, TransitionCmd{Status: StatusApproved, Approver: "e1"}); err != nil {
		t.Fatalf("first approve: %v", err)
	}
	// Second decision, correct (bumped) version: status guard rejects it.
	if _, err := repo.Transition(ctxTODO(), id, 2, TransitionCmd{Status: StatusRejected, Approver: "e2"}); !errors.Is(err, ErrTicketHandled) {
		t.Errorf("second decision error = %v, want ErrTicketHandled", err)
	}
	// Third decision with the original version: also rejected.
	if _, err := repo.Transition(ctxTODO(), id, 1, TransitionCmd{Status: StatusApproved, Approver: "e3"}); !errors.Is(err, ErrTicketHandled) {
		t.Errorf("stale second decision error = %v, want ErrTicketHandled", err)
	}
}

func TestMemRepoDoubleDecisionConcurrent(t *testing.T) {
	// HITL-008: two (here: many) legitimate callbacks race; the state
	// machine must land exactly one decision. Run with -race.
	clock := newFakeClock(fixedClock())
	repo := newMemTicketRepo(clock)
	id := seedTicket(t, repo, clock)

	const goroutines = 8
	start := make(chan struct{})
	results := make(chan error, goroutines)
	decisions := []TransitionCmd{
		{Status: StatusApproved, Approver: "e-approve"},
		{Status: StatusRejected, Approver: "e-reject"},
	}
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, err := repo.Transition(ctxTODO(), id, 1, decisions[i%len(decisions)])
			results <- err
		}(i)
	}
	close(start)
	wg.Wait()
	close(results)

	successes := 0
	for err := range results {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrTicketHandled):
		default:
			t.Errorf("unexpected error: %v", err)
		}
	}
	if successes != 1 {
		t.Fatalf("successful transitions = %d, want exactly 1 (CAS violation)", successes)
	}

	stored, err := repo.Get(ctxTODO(), id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stored.Status != StatusApproved && stored.Status != StatusRejected {
		t.Errorf("final status = %q, want APPROVED or REJECTED", stored.Status)
	}
	if stored.Version != 2 {
		t.Errorf("final version = %d, want 2", stored.Version)
	}
	if stored.DecidedAt == nil {
		t.Error("DecidedAt missing on decided ticket")
	}
}

func TestMemRepoVersionOptimisticLock(t *testing.T) {
	clock := newFakeClock(fixedClock())
	repo := newMemTicketRepo(clock)
	id := seedTicket(t, repo, clock) // version 1

	// Stale version is rejected even though status is still PENDING.
	if _, err := repo.Transition(ctxTODO(), id, 2, TransitionCmd{Status: StatusApproved}); !errors.Is(err, ErrTicketHandled) {
		t.Errorf("stale version error = %v, want ErrTicketHandled", err)
	}
	// Current version wins.
	if _, err := repo.Transition(ctxTODO(), id, 1, TransitionCmd{Status: StatusApproved, Approver: "e1"}); err != nil {
		t.Fatalf("correct version: %v", err)
	}
	// The lock version advanced, so the old value no longer works.
	if _, err := repo.Transition(ctxTODO(), id, 1, TransitionCmd{Status: StatusRejected}); !errors.Is(err, ErrTicketHandled) {
		t.Errorf("reused version error = %v, want ErrTicketHandled", err)
	}
}

func TestMemRepoExpiredRejection(t *testing.T) {
	// SEC-11 / HITL-009: a callback past ExpireAt must be refused.
	clock := newFakeClock(fixedClock())
	repo := newMemTicketRepo(clock)
	id := seedTicket(t, repo, clock)

	clock.Advance(DefaultExpiry + time.Second)

	if _, err := repo.Transition(ctxTODO(), id, 1, TransitionCmd{Status: StatusApproved, Approver: "e1"}); !errors.Is(err, ErrTicketExpired) {
		t.Errorf("expired approve error = %v, want ErrTicketExpired", err)
	}
	if _, err := repo.Transition(ctxTODO(), id, 1, TransitionCmd{Status: StatusRejected, Approver: "e1"}); !errors.Is(err, ErrTicketExpired) {
		t.Errorf("expired reject error = %v, want ErrTicketExpired", err)
	}

	// The scanner path (SEC-11 fail-safe) still works: FindExpired lists the
	// ticket and the EXPIRED transition lands.
	expired, err := repo.FindExpired(ctxTODO(), clock.Now(), 100)
	if err != nil {
		t.Fatalf("FindExpired: %v", err)
	}
	if len(expired) != 1 || expired[0].TicketID != id {
		t.Fatalf("FindExpired = %+v, want the one expired ticket", expired)
	}
	if _, err := repo.Transition(ctxTODO(), id, 1, TransitionCmd{Status: StatusExpired}); err != nil {
		t.Fatalf("expire transition: %v", err)
	}
	// Once EXPIRED, callbacks are rejected as handled.
	if _, err := repo.Transition(ctxTODO(), id, 2, TransitionCmd{Status: StatusApproved}); !errors.Is(err, ErrTicketHandled) {
		t.Errorf("post-expire callback error = %v, want ErrTicketHandled", err)
	}
}

func TestMemRepoOneShotConsumption(t *testing.T) {
	// HITL-010 / SEC-01: a ticket is consumed exactly once; repeated
	// callbacks never re-fire the decision.
	clock := newFakeClock(fixedClock())
	repo := newMemTicketRepo(clock)
	id := seedTicket(t, repo, clock)

	if _, err := repo.Transition(ctxTODO(), id, 1, TransitionCmd{Status: StatusApproved, Approver: "e1001", Comment: "go"}); err != nil {
		t.Fatalf("first callback: %v", err)
	}
	// Identical and opposite repeat callbacks are both refused.
	for i, cmd := range []TransitionCmd{
		{Status: StatusApproved, Approver: "e1001", Comment: "go again"},
		{Status: StatusRejected, Approver: "e1001"},
	} {
		if _, err := repo.Transition(ctxTODO(), id, 2, cmd); !errors.Is(err, ErrTicketHandled) {
			t.Errorf("repeat callback %d error = %v, want ErrTicketHandled", i, err)
		}
	}
	stored, _ := repo.Get(ctxTODO(), id)
	if stored.Status != StatusApproved || stored.Approver != "e1001" || stored.Comment != "go" {
		t.Errorf("first decision lost: %+v", stored)
	}
}

func TestMemRepoFindExpired(t *testing.T) {
	clock := newFakeClock(fixedClock())
	repo := newMemTicketRepo(clock)
	id1 := seedTicket(t, repo, clock)

	// A second ticket with a shorter TTL and different parameters (the
	// pending-dedup index would reject identical params while id1 is
	// in flight).
	short := newTestTicket(t)
	short.Arguments = json.RawMessage(`{"force":false}`)
	short.ParamsHash = CanonicalArgsHash(short.DeviceID, short.ToolName, short.Arguments)
	short.CreatedAt = clock.Now()
	short.ExpireAt = clock.Now().Add(30 * time.Second)
	created, err := repo.Create(ctxTODO(), short)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	id2 := created.TicketID

	// Decide id1 before it expires: decided tickets are never listed even
	// once their expiry passes.
	if _, err := repo.Transition(ctxTODO(), id1, 1, TransitionCmd{Status: StatusApproved, Approver: "e1"}); err != nil {
		t.Fatalf("approve id1: %v", err)
	}

	clock.Advance(45 * time.Second) // id2 expired, id1 decided-but-expired
	expired, err := repo.FindExpired(ctxTODO(), clock.Now(), 10)
	if err != nil {
		t.Fatalf("FindExpired: %v", err)
	}
	if len(expired) != 1 || expired[0].TicketID != id2 {
		t.Errorf("FindExpired = %+v, want only the undecided expired ticket %s", expired, id2)
	}

	// Limit is honored.
	clock.Advance(time.Hour)
	limited, err := repo.FindExpired(ctxTODO(), clock.Now(), 0)
	if err != nil {
		t.Fatalf("FindExpired: %v", err)
	}
	if len(limited) != 0 {
		t.Errorf("limit 0 returned %d tickets", len(limited))
	}
}
