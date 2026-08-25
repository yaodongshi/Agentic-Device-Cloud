// Package audit implements the SEC-07 audit trail (design/31 I10,
// design/32 3.10): producers enqueue events onto a Valkey list with
// at-least-once semantics, and a consumer worker drains the list into
// adc_audit_logs, where the request_id unique index makes redelivery
// idempotent (design/32 6.3).
package audit

import (
	"fmt"
	"time"
)

// Status values mirrored by the adc_audit_logs.status CHECK constraint
// (design/32 3.10). Keep them in sync when the enum evolves.
const (
	StatusSuccess       = "success"
	StatusFailed        = "failed"
	StatusBlockedByHITL = "blocked_by_hitl"
)

// event_type and actor_type are NOT NULL columns of adc_audit_logs
// (design/32 3.10). V1.0 producers emit tool-call-shaped events, so the
// worker fills these constants at insert time; additional event types
// join when approval/admin producers adopt the package.
const (
	EventTypeToolCall = "tool_call"
	EventTypeAdminOp  = "admin_op"
	EventTypeAuth     = "auth_event"
	ActorTypeAgent    = "agent"
	ActorTypeUser     = "user"
)

// AuditEvent is the wire shape of one audit record. It is JSON-encoded on
// the queue and mapped onto adc_audit_logs columns by the worker:
//
//	EventID   -> id and request_id (idempotency key, design/32 6.3)
//	TenantID  -> tenant_id
//	AgentID   -> actor_id (actor_type = agent, SEC-21)
//	DeviceID  -> device_id
//	ToolName  -> tool_name
//	Params    -> request_params
//	Result    -> response_payload
//	Status    -> status
//	Approver  -> hitl_approver
//	RiskLevel -> risk_level (0-3, design/32 2.4)
//	CreatedAt -> created_at (zero values are replaced by now())
//
// TraceID is carried for log correlation only (design/31 1.3.5):
// adc_audit_logs has no trace_id column.
type AuditEvent struct {
	EventID   string         `json:"event_id"`
	TenantID  string         `json:"tenant_id"`
	EventType string         `json:"event_type,omitempty"`
	ActorType string         `json:"actor_type,omitempty"`
	AgentID   string         `json:"agent_id,omitempty"`
	DeviceID  string         `json:"device_id,omitempty"`
	ToolName  string         `json:"tool_name,omitempty"`
	Params    map[string]any `json:"params,omitempty"`
	Result    any            `json:"result,omitempty"`
	Status    string         `json:"status"`
	Approver  string         `json:"approver,omitempty"`
	TraceID   string         `json:"trace_id,omitempty"`
	RiskLevel int            `json:"risk_level,omitempty"`
	CreatedAt time.Time      `json:"created_at"`
}

// Validate rejects events that would violate adc_audit_logs NOT NULL
// constraints or the status CHECK constraint.
func (e *AuditEvent) Validate() error {
	if e == nil {
		return fmt.Errorf("audit: nil event")
	}
	if e.EventID == "" {
		return fmt.Errorf("audit: event_id is required (request_id idempotency key, design/32 6.3)")
	}
	if e.TenantID == "" {
		return fmt.Errorf("audit: tenant_id is required")
	}
	switch e.EventType {
	case "", EventTypeToolCall, EventTypeAdminOp, EventTypeAuth:
	default:
		return fmt.Errorf("audit: invalid event_type %q", e.EventType)
	}
	switch e.ActorType {
	case "", ActorTypeAgent, ActorTypeUser:
	default:
		return fmt.Errorf("audit: invalid actor_type %q", e.ActorType)
	}
	switch e.Status {
	case StatusSuccess, StatusFailed, StatusBlockedByHITL:
	default:
		return fmt.Errorf("audit: invalid status %q (want success | failed | blocked_by_hitl)", e.Status)
	}
	return nil
}
