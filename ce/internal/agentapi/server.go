package agentapi

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"adc.dev/core-sdk/protocol"

	"adc.dev/ce/internal/agentauth"
	"adc.dev/ce/internal/httpx"
	"adc.dev/ce/pkg/audit"
)

// BLOCKED_BY_HITL is the unified blocked-call error code (design/33 12006).
// CodeCrossTenant is the tenant isolation error (design/33 13007, SEC-02).
const (
	CodeBlockedByHITL = "12006"
	CodeToolNotFound  = "12004"
	CodeCrossTenant   = "13007"
)

// toolCallEnvelope wraps an executed tool call result with its request_id
// (design/33 3.2.2): the direct 200 response carries the same envelope field
// as the 202 HITL response so clients can trace every call uniformly.
type toolCallEnvelope struct {
	RequestID string                 `json:"request_id"`
	Content   []protocol.ToolContent `json:"content"`
	IsError   bool                   `json:"is_error"`
	TaskID    string                 `json:"task_id,omitempty"`
	Accepted  bool                   `json:"accepted,omitempty"`
}

// envelopeResult wraps a CallResult in the wire envelope.
func envelopeResult(requestID string, r *CallResult) toolCallEnvelope {
	env := toolCallEnvelope{RequestID: requestID, IsError: r.IsError, TaskID: r.TaskID, Accepted: r.Accepted}
	for _, c := range r.Content {
		env.Content = append(env.Content, protocol.ToolContent{Type: c.Type, Text: c.Text})
	}
	return env
}

// Server wires the Agent API HTTP endpoints (design/33 3.2).
type Server struct {
	Auth       agentauth.ApiKeyValidator
	Aggregator ToolAggregator
	Router     ToolRouter
	Policy     RiskPolicy
	HITL       HITLClient
	Audit      audit.AuditSink // nil disables audit emission (tests)

	// Guard validates call arguments before Policy.Decide (design/83
	// C5.2). nil disables parameter validation entirely.
	Guard ParamGuard

	// AwaitWindow bounds how long a pending call waits for approval.
	AwaitWindow time.Duration

	mu      sync.Mutex
	pending map[string]*pendingCall
}

type pendingCall struct {
	ref      TicketRef
	call     protocol.ToolCallParams
	tool     ToolRef
	tenantID string
	agentID  string
	state    string // PENDING / EXECUTING / BLOCKED / DONE
	out      *CallResult
}

// NewServer builds a Server with sane defaults.
func NewServer(auth agentauth.ApiKeyValidator, agg ToolAggregator, router ToolRouter, policy RiskPolicy, hitl HITLClient) *Server {
	return &Server{
		Auth:        auth,
		Aggregator:  agg,
		Router:      router,
		Policy:      policy,
		HITL:        hitl,
		AwaitWindow: 5 * time.Minute,
		pending:     make(map[string]*pendingCall),
	}
}

// Routes registers the Agent API endpoints on mux.
func (s *Server) Routes(mux *http.ServeMux) {
	mux.Handle("GET /v1/agent/mcp/tools", agentauth.Authorize(s.Auth)(http.HandlerFunc(s.handleToolsList)))
	mux.Handle("POST /v1/agent/mcp/tools/call", agentauth.Authorize(s.Auth)(http.HandlerFunc(s.handleToolCall)))
	mux.Handle("GET /v1/agent/mcp/tools/call/{requestID}", agentauth.Authorize(s.Auth)(http.HandlerFunc(s.handleCallStatus)))
}

func (s *Server) handleToolsList(w http.ResponseWriter, r *http.Request) {
	p, _ := agentauth.FromContext(r.Context())
	tools, err := s.Aggregator.List(r.Context(), p.TenantID)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "10006", "list tools failed", httpx.TraceIDFrom(r))
		return
	}
	httpx.WriteJSON(w, http.StatusOK, protocol.ToolsListResult{Tools: tools})
}

func (s *Server) handleToolCall(w http.ResponseWriter, r *http.Request) {
	p, _ := agentauth.FromContext(r.Context())

	var req protocol.ToolCallParams
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "10003", "invalid json", httpx.TraceIDFrom(r))
		return
	}
	ref, err := s.Aggregator.Resolve(r.Context(), p.TenantID, req.Name)
	if err != nil {
		if errors.Is(err, ErrDeviceNotOwned) {
			// Tenant isolation (design/33 13007, SEC-02): a device that
			// does not exist in the caller's tenant must be an explicit
			// 403, never a 500 hiding the reason.
			httpx.WriteError(w, http.StatusForbidden, CodeCrossTenant, err.Error(), httpx.TraceIDFrom(r))
			return
		}
		httpx.WriteError(w, http.StatusBadRequest, CodeToolNotFound, err.Error(), httpx.TraceIDFrom(r))
		return
	}
	// C5.2: validate arguments before any risk decision, so both the
	// free-run path and the HITL path reject malformed calls (400 code
	// 12004) and no ticket is ever created for them.
	if s.Guard != nil {
		perr, err := s.Guard.Validate(r.Context(), p.TenantID, ref, req.Arguments)
		if err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, "10006", "parameter validation failed", httpx.TraceIDFrom(r))
			return
		}
		if perr != nil {
			writeParamError(w, r, perr)
			return
		}
	}
	decision, err := s.Policy.Decide(r.Context(), p.TenantID, ref)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "10006", "risk decision failed", httpx.TraceIDFrom(r))
		return
	}

	if !decision.RequireApproval {
		result, err := s.Router.Call(r.Context(), p.TenantID, ref, req.Arguments)
		if err != nil {
			httpx.WriteError(w, http.StatusBadGateway, "12005", err.Error(), httpx.TraceIDFrom(r))
			return
		}
		requestID := newEventID()
		s.emitAudit(r, p, ref, req, requestID, audit.StatusSuccess, "")
		httpx.WriteJSON(w, http.StatusOK, envelopeResult(requestID, result))
		return
	}

	// High risk: create a ticket, register a pending call, return 202.
	ticket, err := s.HITL.CreateTicket(r.Context(), &TicketRequest{
		TenantID:  p.TenantID,
		AgentID:   p.AgentID,
		DeviceID:  ref.DeviceID,
		ToolName:  ref.ToolName,
		Arguments: req.Arguments,
		RiskLevel: decision.Level,
	})
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "10006", "create ticket failed", httpx.TraceIDFrom(r))
		return
	}

	s.mu.Lock()
	s.pending[ticket.RequestID] = &pendingCall{ref: *ticket, call: req, tool: ref, tenantID: p.TenantID, agentID: p.AgentID, state: "PENDING"}
	s.mu.Unlock()

	go s.awaitDecision(p.TenantID, ticket)

	httpx.WriteJSON(w, http.StatusAccepted, map[string]string{
		"request_id": ticket.RequestID,
		"ticket_id":  ticket.TicketID,
		"polling":    "/v1/agent/mcp/tools/call/" + ticket.RequestID,
	})
}

func (s *Server) awaitDecision(tenantID string, ticket *TicketRef) {
	ctx, cancel := context.WithTimeout(context.Background(), s.AwaitWindow)
	defer cancel()
	decision, err := s.HITL.AwaitDecision(ctx, ticket.TicketID, s.AwaitWindow)
	s.mu.Lock()
	defer s.mu.Unlock()
	pc, ok := s.pending[ticket.RequestID]
	if !ok {
		return
	}
	if err != nil || decision == nil || decision.Status != DecisionApproved {
		pc.state = "BLOCKED"
		s.emitAuditEvent(pc, audit.StatusBlockedByHITL, decisionApprover(decision))
		return
	}
	// Approved: execute via the router, then mark DONE.
	result, callErr := s.Router.Call(context.Background(), tenantID, pc.tool, pc.call.Arguments)
	if callErr != nil {
		pc.state = "BLOCKED"
		pc.out = &CallResult{
			Content: []protocol.ToolContent{{Type: "text", Text: callErr.Error()}},
			IsError: true,
		}
		s.emitAuditEvent(pc, audit.StatusFailed, decisionApprover(decision))
		return
	}
	pc.state = "DONE"
	pc.out = result
	s.emitAuditEvent(pc, audit.StatusSuccess, decisionApprover(decision))
}

func decisionApprover(d *TicketDecision) string {
	if d == nil {
		return "system"
	}
	if d.By == "" {
		return "system"
	}
	return d.By
}

// emitAudit enqueues an audit event for a call (nil sink = no-op). The
// requestID doubles as the audit idempotency key and the response envelope
// field so the two can be correlated (design/32 6.3).
func (s *Server) emitAudit(r *http.Request, p *agentauth.Principal, ref ToolRef, req protocol.ToolCallParams, requestID, status, approver string) {
	if s.Audit == nil {
		return
	}
	_ = s.Audit.Enqueue(r.Context(), &audit.AuditEvent{
		EventID:  requestID,
		TenantID: p.TenantID, AgentID: p.AgentID,
		DeviceID: ref.DeviceUUID, ToolName: ref.ToolName,
		Params: req.Arguments, Status: status, Approver: approver,
		RiskLevel: ref.RiskLevel, TraceID: httpx.TraceIDFrom(r),
	})
}

// emitAuditEvent emits for async approval completions (no request context).
func (s *Server) emitAuditEvent(pc *pendingCall, status, approver string) {
	if s.Audit == nil {
		return
	}
	_ = s.Audit.Enqueue(context.Background(), &audit.AuditEvent{
		EventID:  pc.ref.RequestID,
		TenantID: pc.tenantID, AgentID: pc.agentID,
		DeviceID: pc.tool.DeviceUUID, ToolName: pc.tool.ToolName,
		Params: pc.call.Arguments, Status: status, Approver: approver,
		RiskLevel: pc.tool.RiskLevel,
	})
}

// newEventID returns a UUIDv4 string without third-party deps.
func newEventID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("evt-%d", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func (s *Server) handleCallStatus(w http.ResponseWriter, r *http.Request) {
	requestID := r.PathValue("requestID")
	s.mu.Lock()
	pc, ok := s.pending[requestID]
	if !ok {
		s.mu.Unlock()
		httpx.WriteError(w, http.StatusNotFound, "12004", "unknown request_id", httpx.TraceIDFrom(r))
		return
	}
	state := pc.state
	out := pc.out
	s.mu.Unlock()
	switch state {
	case "BLOCKED":
		httpx.WriteError(w, http.StatusForbidden, CodeBlockedByHITL, "BLOCKED_BY_HITL: operation not approved", httpx.TraceIDFrom(r))
	case "DONE":
		httpx.WriteJSON(w, http.StatusOK, envelopeResult(requestID, out))
	default:
		httpx.WriteJSON(w, http.StatusAccepted, map[string]string{
			"request_id": requestID,
			"state":      state,
		})
	}
}
