package agentapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"adc.dev/ce/internal/approval"
)

// fakeTicketRepo is a minimal in-memory approval.TicketRepo for client
// tests; Transition serializes check-and-apply under one mutex like the
// production PG single-statement update.
type fakeTicketRepo struct {
	mu      sync.Mutex
	tickets map[string]*approval.ApprovalTicket
	dedup   *approval.ApprovalTicket // returned with ErrTicketDedup when set
}

func newFakeTicketRepo() *fakeTicketRepo {
	return &fakeTicketRepo{tickets: make(map[string]*approval.ApprovalTicket)}
}

func (r *fakeTicketRepo) Create(_ context.Context, t *approval.ApprovalTicket) (*approval.ApprovalTicket, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.dedup != nil {
		existing := *r.dedup
		return &existing, approval.ErrTicketDedup
	}
	if t.TicketID == "" {
		id, err := approval.NewTicketID()
		if err != nil {
			return nil, err
		}
		t.TicketID = id
	}
	cp := *t
	r.tickets[cp.TicketID] = &cp
	return &cp, nil
}

func (r *fakeTicketRepo) Transition(_ context.Context, ticketID string, wantVersion int, cmd approval.TransitionCmd) (*approval.ApprovalTicket, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.tickets[ticketID]
	if !ok {
		return nil, approval.ErrTicketNotFound
	}
	if err := approval.ApplyTransition(t, cmd, wantVersion, time.Now().UTC()); err != nil {
		return nil, err
	}
	cp := *t
	return &cp, nil
}

func (r *fakeTicketRepo) Get(_ context.Context, ticketID string) (*approval.ApprovalTicket, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.tickets[ticketID]
	if !ok {
		return nil, approval.ErrTicketNotFound
	}
	cp := *t
	return &cp, nil
}

func (r *fakeTicketRepo) FindExpired(_ context.Context, now time.Time, limit int) ([]approval.ApprovalTicket, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []approval.ApprovalTicket
	for _, t := range r.tickets {
		if t.Status == approval.StatusPending && !now.Before(t.ExpireAt) {
			out = append(out, *t)
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// recordingNotifier captures the card deliveries without any network.
type recordingNotifier struct {
	mu    sync.Mutex
	calls []notifyCall
	err   error
}

type notifyCall struct {
	ticket     *approval.ApprovalTicket
	approveURL string
	rejectURL  string
}

func (n *recordingNotifier) SendApprovalCard(_ context.Context, t *approval.ApprovalTicket, approveURL, rejectURL string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.calls = append(n.calls, notifyCall{ticket: t, approveURL: approveURL, rejectURL: rejectURL})
	return n.err
}

func (n *recordingNotifier) last() (notifyCall, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if len(n.calls) == 0 {
		return notifyCall{}, false
	}
	return n.calls[len(n.calls)-1], true
}

// newTestClient assembles a client over fakes; the bus subscriber is real so
// wake routing is exercised end to end.
func newTestClient(repo *fakeTicketRepo, notifier *recordingNotifier) *ApprovalHITLClient {
	return NewApprovalHITLClient(ApprovalHITLConfig{
		Repo:        repo,
		Bus:         approval.NewInMemoryEventBus(),
		Notify:      notifier,
		BaseURL:     "https://adc.example.com",
		CallbackKey: "test-callback-key-0123456789abcdef",
		TicketTTL:   5 * time.Minute,
	})
}

func testTicketRequest() *TicketRequest {
	return &TicketRequest{
		TenantID:  "tenant-1",
		AgentID:   "agent-1",
		DeviceID:  "cnc-01",
		ToolName:  "set_spindle_speed",
		Arguments: map[string]interface{}{"rpm": 4200},
		RiskLevel: 2,
	}
}

// waitForWaiter blocks until the client registers a waiter for ticketID.
func waitForWaiter(t *testing.T, c *ApprovalHITLClient, ticketID string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		_, ok := c.waiters[ticketID]
		c.mu.Unlock()
		if ok {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("waiter for %s never registered", ticketID)
}

func TestApprovalHITLClientFullFlowApprove(t *testing.T) {
	repo := newFakeTicketRepo()
	notifier := &recordingNotifier{}
	client := newTestClient(repo, notifier)
	defer client.Close()

	ref, err := client.CreateTicket(context.Background(), testTicketRequest())
	if err != nil {
		t.Fatalf("CreateTicket: %v", err)
	}
	if ref.TicketID == "" || ref.RequestID == "" {
		t.Fatalf("ref = %+v", ref)
	}

	// The card must carry signed action URLs over the derived ticket secret
	// (SEC-13).
	call, ok := notifier.last()
	if !ok {
		t.Fatal("notifier was not called")
	}
	ticket, err := repo.Get(context.Background(), ref.TicketID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if ticket.Status != approval.StatusPending || ticket.DeviceID != "cnc-01" {
		t.Errorf("ticket = %+v", ticket)
	}
	secret := approval.DeriveTicketSecret(client.callbackKey, ref.TicketID)
	wantApprove := approval.BuildActionURL("https://adc.example.com", ref.TicketID, approval.DecisionApprove, ticket.ExpireAt, secret)
	wantReject := approval.BuildActionURL("https://adc.example.com", ref.TicketID, approval.DecisionReject, ticket.ExpireAt, secret)
	if call.approveURL != wantApprove || call.rejectURL != wantReject {
		t.Errorf("card urls = %q / %q, want %q / %q", call.approveURL, call.rejectURL, wantApprove, wantReject)
	}
	if call.ticket.DeviceID != "cnc-01" || call.ticket.ToolName != "set_spindle_speed" {
		t.Errorf("card ticket = %+v", call.ticket)
	}
	// URL signatures must verify against the handler-side derivation.
	u, err := url.Parse(call.approveURL)
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	q := u.Query()
	if !approval.VerifyCallbackSignature(secret, ref.TicketID, q.Get("decision"), q.Get("sig"), ticket.ExpireAt) {
		t.Error("card URL signature does not verify (SEC-13)")
	}

	// AwaitDecision wakes through the bus publish, enriched from the repo.
	decisionCh := make(chan *TicketDecision, 1)
	errCh := make(chan error, 1)
	go func() {
		d, err := client.AwaitDecision(context.Background(), ref.TicketID, 5*time.Second)
		decisionCh <- d
		errCh <- err
	}()
	waitForWaiter(t, client, ref.TicketID)

	resolved, err := repo.Transition(context.Background(), ref.TicketID, 1, approval.TransitionCmd{
		Status:   approval.StatusApproved,
		Approver: "emp_zhangwei",
		Comment:  "已核实工艺单，放行",
	})
	if err != nil {
		t.Fatalf("Transition: %v", err)
	}
	if err := client.bus.PublishResolved(context.Background(), resolved.TicketID, resolved.Status); err != nil {
		t.Fatalf("publish: %v", err)
	}

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("AwaitDecision: %v", err)
		}
		// Both channels are ready on success; the errCh pick above must
		// not mask the decision itself.
		if d := <-decisionCh; d == nil || d.Status != DecisionApproved || d.By != "emp_zhangwei" || d.Reason != "已核实工艺单，放行" {
			t.Errorf("decision = %+v, want APPROVED by emp_zhangwei", d)
		}
	case d := <-decisionCh:
		if d == nil || d.Status != DecisionApproved || d.By != "emp_zhangwei" || d.Reason != "已核实工艺单，放行" {
			t.Errorf("decision = %+v, want APPROVED by emp_zhangwei", d)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("AwaitDecision did not return after wake (SEC-10)")
	}
}

func TestApprovalHITLClientReject(t *testing.T) {
	repo := newFakeTicketRepo()
	client := newTestClient(repo, &recordingNotifier{})
	defer client.Close()

	ref, err := client.CreateTicket(context.Background(), testTicketRequest())
	if err != nil {
		t.Fatalf("CreateTicket: %v", err)
	}
	go func() {
		time.Sleep(20 * time.Millisecond)
		if _, err := repo.Transition(context.Background(), ref.TicketID, 1, approval.TransitionCmd{Status: approval.StatusRejected, Approver: "e1"}); err != nil {
			t.Errorf("Transition: %v", err)
		}
	}()
	// No bus publish: the repository poll is the rebuild path that must
	// resolve the wait anyway.
	d, err := client.AwaitDecision(context.Background(), ref.TicketID, 3*time.Second)
	if err != nil {
		t.Fatalf("AwaitDecision: %v", err)
	}
	if d.Status != DecisionRejected || d.By != "e1" {
		t.Errorf("decision = %+v, want REJECTED by e1", d)
	}
}

func TestApprovalHITLClientTimeoutExpiresTicket(t *testing.T) {
	repo := newFakeTicketRepo()
	client := newTestClient(repo, &recordingNotifier{})
	defer client.Close()

	ref, err := client.CreateTicket(context.Background(), testTicketRequest())
	if err != nil {
		t.Fatalf("CreateTicket: %v", err)
	}
	d, err := client.AwaitDecision(context.Background(), ref.TicketID, 80*time.Millisecond)
	if err != nil {
		t.Fatalf("AwaitDecision: %v", err)
	}
	if d.Status != DecisionExpired || d.By != "system" {
		t.Errorf("decision = %+v, want EXPIRED by system (fail closed, SEC-11)", d)
	}
	// The client best-effort voids the ticket so other waiters and the
	// scanner observe the expiry.
	stored, err := repo.Get(context.Background(), ref.TicketID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stored.Status != approval.StatusExpired {
		t.Errorf("status = %q, want EXPIRED after client timeout", stored.Status)
	}
}

func TestApprovalHITLClientContextCancel(t *testing.T) {
	repo := newFakeTicketRepo()
	client := newTestClient(repo, &recordingNotifier{})
	defer client.Close()

	ref, err := client.CreateTicket(context.Background(), testTicketRequest())
	if err != nil {
		t.Fatalf("CreateTicket: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.AwaitDecision(ctx, ref.TicketID, 5*time.Second); !errors.Is(err, context.Canceled) {
		t.Errorf("AwaitDecision error = %v, want context.Canceled", err)
	}
}

func TestApprovalHITLClientDedupReusesExistingTicket(t *testing.T) {
	repo := newFakeTicketRepo()
	existing := &approval.ApprovalTicket{
		TicketID:  "3fa85f64-5717-4562-b3fc-2c963f66afa6",
		TenantID:  "tenant-1",
		AgentID:   "agent-1",
		DeviceID:  "cnc-01",
		ToolName:  "set_spindle_speed",
		Arguments: json.RawMessage(`{"rpm":4200}`),
		RiskLevel: 2,
		Status:    approval.StatusPending,
		Version:   1,
		CreatedAt: time.Now().UTC(),
		ExpireAt:  time.Now().UTC().Add(5 * time.Minute),
	}
	repo.dedup = existing
	notifier := &recordingNotifier{}
	client := newTestClient(repo, notifier)
	defer client.Close()

	ref, err := client.CreateTicket(context.Background(), testTicketRequest())
	if err != nil {
		t.Fatalf("CreateTicket with dedup: %v", err)
	}
	if ref.TicketID != existing.TicketID {
		t.Errorf("ticket id = %s, want existing %s (design/32 6.3)", ref.TicketID, existing.TicketID)
	}
	// The card is re-delivered so a retried create retries a failed push.
	call, ok := notifier.last()
	if !ok {
		t.Fatal("dedup path did not re-notify")
	}
	if !strings.Contains(call.approveURL, existing.TicketID) {
		t.Errorf("approve url = %s, want existing ticket id", call.approveURL)
	}
}

func TestApprovalHITLClientUUIDLookupUsedForTicket(t *testing.T) {
	repo := newFakeTicketRepo()
	notifier := &recordingNotifier{}
	client := NewApprovalHITLClient(ApprovalHITLConfig{
		Repo:        repo,
		Bus:         approval.NewInMemoryEventBus(),
		Notify:      notifier,
		BaseURL:     "https://adc.example.com",
		CallbackKey: "cbk",
		TicketTTL:   time.Minute,
		UUIDLookup: func(ctx context.Context, tenantID, deviceCode string) (string, error) {
			return "bbbbbbbb-0000-0000-0000-000000000002", nil
		},
	})
	defer client.Close()

	ref, err := client.CreateTicket(context.Background(), testTicketRequest())
	if err != nil {
		t.Fatalf("CreateTicket: %v", err)
	}
	ticket, err := repo.Get(context.Background(), ref.TicketID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if ticket.DeviceID != "bbbbbbbb-0000-0000-0000-000000000002" {
		t.Errorf("ticket DeviceID = %s, want the device UUID", ticket.DeviceID)
	}
	call, _ := notifier.last()
	if call.ticket.DeviceID != "cnc-01" {
		t.Errorf("card shows DeviceID %q, want the human-readable device code", call.ticket.DeviceID)
	}
}

func TestApprovalHITLClientNotifyFailureSurfaces(t *testing.T) {
	repo := newFakeTicketRepo()
	notifier := &recordingNotifier{err: errors.New("webhook down")}
	client := newTestClient(repo, notifier)
	defer client.Close()

	if _, err := client.CreateTicket(context.Background(), testTicketRequest()); err == nil ||
		!strings.Contains(err.Error(), "send approval card") {
		t.Errorf("CreateTicket error = %v, want card delivery failure surfaced (SEC-18)", err)
	}
	// The ticket is still persisted and expires fail-safe via the scanner.
	repo.mu.Lock()
	count, pending := 0, 0
	for _, tk := range repo.tickets {
		count++
		if tk.Status == approval.StatusPending {
			pending++
		}
	}
	repo.mu.Unlock()
	if count != 1 || pending != 1 {
		t.Errorf("tickets = %d (pending %d), want 1 PENDING ticket left to expire fail-safe", count, pending)
	}
}

func TestApprovalHITLClientArgumentsStoredCanonically(t *testing.T) {
	repo := newFakeTicketRepo()
	client := newTestClient(repo, &recordingNotifier{})
	defer client.Close()

	req := testTicketRequest()
	ref, err := client.CreateTicket(context.Background(), req)
	if err != nil {
		t.Fatalf("CreateTicket: %v", err)
	}
	ticket, err := repo.Get(context.Background(), ref.TicketID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !json.Valid(ticket.Arguments) {
		t.Errorf("arguments = %s, want valid JSON", ticket.Arguments)
	}
	var args map[string]float64
	if err := json.Unmarshal(ticket.Arguments, &args); err != nil || args["rpm"] != 4200 {
		t.Errorf("arguments = %s, err=%v", ticket.Arguments, err)
	}
	if ticket.ParamsHash != approval.CanonicalArgsHash(ticket.DeviceID, ticket.ToolName, ticket.Arguments) {
		t.Error("params hash mismatch (design/32 6.3)")
	}
	ttl := ticket.ExpireAt.Sub(ticket.CreatedAt)
	if ttl < 5*time.Minute-time.Second || ttl > 5*time.Minute+time.Second {
		t.Errorf("ttl = %v, want 5m (ADC_HITL_TIMEOUT_SEC default)", ttl)
	}
}
