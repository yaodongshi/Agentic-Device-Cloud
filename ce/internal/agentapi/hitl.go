package agentapi

import (
	"context"
	"sync"
	"time"
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
