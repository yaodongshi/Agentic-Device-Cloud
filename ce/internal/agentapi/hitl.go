package agentapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"adc.dev/ce/internal/approval"
)

// TicketRequest / TicketRef / TicketDecision / HITLClient implement the
// I8 seam (design/31 3.2.2). V1.0 uses InMemoryHITLClient for tests;
// production wiring delegates to the approval service via cmd.
type TicketRequest struct {
	TenantID  string
	AgentID   string
	DeviceID  string
	ToolName  string
	Arguments map[string]interface{}
	RiskLevel int
}

type TicketRef struct {
	TicketID  string
	RequestID string
}

type TicketDecision struct {
	Status string // APPROVED / REJECTED / EXPIRED
	By     string
	Reason string
}

// Decision statuses shared with the approval state machine.
const (
	DecisionApproved = "APPROVED"
	DecisionRejected = "REJECTED"
	DecisionExpired  = "EXPIRED"
)

// HITLClient creates approval tickets and awaits decisions (I8).
type HITLClient interface {
	CreateTicket(ctx context.Context, req *TicketRequest) (*TicketRef, error)
	AwaitDecision(ctx context.Context, ticketID string, timeout time.Duration) (*TicketDecision, error)
}

// InMemoryHITLClient stores pending decisions in a map keyed by ticket id.
// Tests resolve tickets via DecideTicket; production uses the approval
// service (ce/internal/approval) through the same interface.
type InMemoryHITLClient struct {
	mu        sync.Mutex
	tickets   map[string]*TicketRequest
	decisions map[string]TicketDecision
	byReqID   map[string]*TicketRef
}

// NewInMemoryHITLClient builds an empty in-memory client.
func NewInMemoryHITLClient() *InMemoryHITLClient {
	return &InMemoryHITLClient{
		tickets:   make(map[string]*TicketRequest),
		decisions: make(map[string]TicketDecision),
		byReqID:   make(map[string]*TicketRef),
	}
}

// CreateTicket records the request and returns synthetic UUID ids
// (request_id must be UUID-shaped for adc_audit_logs.request_id, design/32).
func (c *InMemoryHITLClient) CreateTicket(_ context.Context, req *TicketRequest) (*TicketRef, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ticketID := "ticket-" + newEventID()
	requestID := newEventID()
	c.tickets[ticketID] = req
	ref := &TicketRef{TicketID: ticketID, RequestID: requestID}
	c.byReqID[requestID] = ref
	return ref, nil
}

// DecideTicket injects a decision (test helper / approval callback seam).
func (c *InMemoryHITLClient) DecideTicket(ticketID string, d TicketDecision) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.decisions[ticketID] = d
}

// AwaitDecision blocks until a decision is injected or the timeout elapses.
// On timeout the decision is EXPIRED (fail closed, SEC-11 semantics).
func (c *InMemoryHITLClient) AwaitDecision(ctx context.Context, ticketID string, timeout time.Duration) (*TicketDecision, error) {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		c.mu.Lock()
		d, ok := c.decisions[ticketID]
		c.mu.Unlock()
		if ok {
			return &d, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
			d := TicketDecision{Status: DecisionExpired, By: "system", Reason: "timeout"}
			return &d, nil
		case <-ticker.C:
		}
	}
}

// LookupByRequest resolves a TicketRef by its request id.
func (c *InMemoryHITLClient) LookupByRequest(requestID string) (*TicketRef, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ref, ok := c.byReqID[requestID]
	return ref, ok
}

// pollInterval is the repository poll cadence of ApprovalHITLClient's await
// loop. The event bus is the fast wake path (SEC-10); polling is the
// rebuild path for missed events, process restarts and bus outages
// (design/31 3.3.6).
const pollInterval = time.Second

// ApprovalHITLConfig wires the production HITL client (design/80 B-07):
// tickets live in the approval service's TicketRepo (ADR-05), decisions
// arrive through the TicketEventBus wake channel with a repository poll
// fallback (SEC-10), and approvers are reached through the Notifier card
// channel (SEC-18).
type ApprovalHITLConfig struct {
	Repo        approval.TicketRepo
	Bus         approval.TicketEventBus
	Notify      approval.Notifier // nil disables card delivery (dev without webhooks)
	BaseURL     string            // public origin of the landing page (design/33)
	CallbackKey string            // platform callback signing key, ADC_HITL_CALLBACK_KEY (SEC-13)
	TicketTTL   time.Duration     // ticket lifetime; 0 uses approval.DefaultExpiry

	// UUIDLookup resolves adc_devices.id from device_code; the ticket's
	// dedup/FK columns store the UUID while cards display the device code.
	// May be nil (tests, aggregates without a DB lookup).
	UUIDLookup DeviceUUIDLookup
}

// ApprovalHITLClient implements HITLClient over the approval service core
// (design/31 3.2.2 I8). One background goroutine subscribes the wake bus and
// routes events to local waiters; Close stops it.
type ApprovalHITLClient struct {
	repo        approval.TicketRepo
	bus         approval.TicketEventBus
	notify      approval.Notifier
	baseURL     string
	callbackKey string
	ticketTTL   time.Duration
	uuidLookup  DeviceUUIDLookup

	mu        sync.Mutex
	waiters   map[string]chan approval.Status
	subCancel context.CancelFunc
}

// NewApprovalHITLClient assembles the client and starts the wake subscriber.
func NewApprovalHITLClient(cfg ApprovalHITLConfig) *ApprovalHITLClient {
	ttl := cfg.TicketTTL
	if ttl <= 0 {
		ttl = approval.DefaultExpiry
	}
	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = "http://localhost:8080"
	}
	c := &ApprovalHITLClient{
		repo:        cfg.Repo,
		bus:         cfg.Bus,
		notify:      cfg.Notify,
		baseURL:     baseURL,
		callbackKey: cfg.CallbackKey,
		ticketTTL:   ttl,
		uuidLookup:  cfg.UUIDLookup,
		waiters:     make(map[string]chan approval.Status),
	}
	if c.bus != nil {
		ctx, cancel := context.WithCancel(context.Background())
		c.subCancel = cancel
		go func() {
			_ = c.bus.SubscribeResolved(ctx, c.routeResolved)
		}()
	}
	return c
}

// Close stops the wake subscriber goroutine.
func (c *ApprovalHITLClient) Close() {
	if c.subCancel != nil {
		c.subCancel()
	}
}

// routeResolved fans a wake event to the local waiter of the ticket. The
// send is non-blocking: the waiter also polls the repo, so a missed event
// cannot strand the call.
func (c *ApprovalHITLClient) routeResolved(ticketID string, status approval.Status) {
	c.mu.Lock()
	ch := c.waiters[ticketID]
	c.mu.Unlock()
	if ch == nil {
		return
	}
	select {
	case ch <- status:
	default:
	}
}

// CreateTicket persists the ticket through the approval repository and
// delivers the signed approval card (SEC-13). A duplicate pending ticket
// (ErrTicketDedup, design/32 6.3) reuses the existing one and re-sends the
// card, which also retries a previously failed delivery. Card delivery
// failure is surfaced as an error (design/33 12008 semantics): the ticket
// still expires fail-safe via the scanner, and an Agent retry reuses it.
func (c *ApprovalHITLClient) CreateTicket(ctx context.Context, req *TicketRequest) (*TicketRef, error) {
	argsJSON, err := json.Marshal(req.Arguments)
	if err != nil {
		return nil, fmt.Errorf("agentapi: marshal ticket arguments: %w", err)
	}
	if len(argsJSON) == 0 || string(argsJSON) == "null" {
		argsJSON = json.RawMessage(`{}`)
	}
	deviceID := req.DeviceID
	if c.uuidLookup != nil {
		if uuid, uerr := c.uuidLookup(ctx, req.TenantID, req.DeviceID); uerr == nil && uuid != "" {
			deviceID = uuid
		}
	}
	ticket, err := approval.NewPending(req.TenantID, req.AgentID, deviceID, req.ToolName, argsJSON, req.RiskLevel,
		time.Now().UTC().Add(c.ticketTTL))
	if err != nil {
		return nil, fmt.Errorf("agentapi: build approval ticket: %w", err)
	}
	created, err := c.repo.Create(ctx, ticket)
	if err != nil && !errors.Is(err, approval.ErrTicketDedup) {
		return nil, fmt.Errorf("agentapi: create approval ticket: %w", err)
	}
	if created == nil {
		return nil, errors.New("agentapi: approval repo returned no ticket")
	}
	if err := c.sendCard(ctx, created, req.DeviceID); err != nil {
		return nil, err
	}
	return &TicketRef{TicketID: created.TicketID, RequestID: newEventID()}, nil
}

// sendCard signs the approve/reject landing URLs with the ticket secret
// derived from the platform callback key (SEC-13) and delivers the card.
// The card displays the human-readable device code while the persisted
// ticket keeps the device UUID for FK/dedup columns.
func (c *ApprovalHITLClient) sendCard(ctx context.Context, ticket *approval.ApprovalTicket, deviceCode string) error {
	if c.notify == nil {
		return nil
	}
	secret := approval.DeriveTicketSecret(c.callbackKey, ticket.TicketID)
	approveURL := approval.BuildActionURL(c.baseURL, ticket.TicketID, approval.DecisionApprove, ticket.ExpireAt, secret)
	rejectURL := approval.BuildActionURL(c.baseURL, ticket.TicketID, approval.DecisionReject, ticket.ExpireAt, secret)
	display := *ticket
	display.DeviceID = deviceCode
	if err := c.notify.SendApprovalCard(ctx, &display, approveURL, rejectURL); err != nil {
		return fmt.Errorf("agentapi: send approval card: %w", err)
	}
	return nil
}

// AwaitDecision blocks until the ticket is resolved. The wake event is the
// fast path; the repository is polled as the rebuild path; on timeout the
// decision is EXPIRED (fail closed, SEC-11) and the ticket is best-effort
// expired in the repository so the scanner and other waiters observe it.
func (c *ApprovalHITLClient) AwaitDecision(ctx context.Context, ticketID string, timeout time.Duration) (*TicketDecision, error) {
	ch := make(chan approval.Status, 1)
	c.mu.Lock()
	c.waiters[ticketID] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.waiters, ticketID)
		c.mu.Unlock()
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	poll := time.NewTicker(pollInterval)
	defer poll.Stop()

	for {
		if d, decided := c.loadDecision(ctx, ticketID); decided {
			return d, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
			c.expireTicket(ticketID)
			return &TicketDecision{Status: DecisionExpired, By: "system", Reason: "timeout"}, nil
		case status := <-ch:
			if d := c.decisionFor(ctx, ticketID, status); d != nil {
				return d, nil
			}
		case <-poll.C:
		}
	}
}

// loadDecision reads the ticket from the repository; a non-PENDING status
// resolves immediately, any read failure keeps waiting (the timeout is the
// final fallback).
func (c *ApprovalHITLClient) loadDecision(ctx context.Context, ticketID string) (*TicketDecision, bool) {
	if c.repo == nil {
		return nil, false
	}
	t, err := c.repo.Get(ctx, ticketID)
	if err != nil || t.Status == approval.StatusPending {
		return nil, false
	}
	return c.decisionFor(ctx, ticketID, t.Status), true
}

// decisionFor maps a landed status to a TicketDecision and enriches it with
// the approver snapshot and comment from the repository when available.
func (c *ApprovalHITLClient) decisionFor(ctx context.Context, ticketID string, status approval.Status) *TicketDecision {
	var d *TicketDecision
	switch status {
	case approval.StatusApproved:
		d = &TicketDecision{Status: DecisionApproved, By: "approver"}
	case approval.StatusRejected:
		d = &TicketDecision{Status: DecisionRejected, By: "approver"}
	case approval.StatusExpired:
		d = &TicketDecision{Status: DecisionExpired, By: "system", Reason: "expired"}
	default:
		return nil
	}
	if c.repo == nil {
		return d
	}
	if t, err := c.repo.Get(ctx, ticketID); err == nil && t.Status != approval.StatusPending {
		if t.Approver != "" {
			d.By = t.Approver
		}
		d.Reason = t.Comment
	}
	return d
}

// expireTicket best-effort voids the ticket and publishes the wake event so
// other waiters observe the expiry (SEC-11 fail-safe; the scanner is the
// authoritative expiry path, design/31 3.3.7).
func (c *ApprovalHITLClient) expireTicket(ticketID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if c.repo == nil {
		return
	}
	cur, err := c.repo.Get(ctx, ticketID)
	if err != nil {
		return
	}
	updated, err := c.repo.Transition(ctx, ticketID, cur.Version, approval.TransitionCmd{Status: approval.StatusExpired})
	if err != nil || c.bus == nil {
		return
	}
	_ = c.bus.PublishResolved(ctx, updated.TicketID, updated.Status)
}
