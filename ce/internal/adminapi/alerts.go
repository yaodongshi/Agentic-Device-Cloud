package adminapi

// FR-017 alert rule configuration and event history handlers (design/82
// B2). Endpoints:
//
//	GET /v1/admin/alerts/rules      list the tenant's threshold rules
//	PUT /v1/admin/alerts/rules      full-replace the tenant's rule set
//	GET /v1/admin/alerts/events     most recent fired alerts (≤100)
//
// Rules are tenant-scoped configuration persisted by alerts.RuleStore
// (adc_tenants.metadata "alert_rules" JSONB in production); the handlers
// stay thin — validation, evaluation and notification semantics live in
// internal/alerts. PUT is a full replace like the approval policy (any
// omitted rule is deleted); rule ids returned by GET must be echoed back
// to keep evaluator dedup state stable, new rows may omit the id.

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"adc.dev/ce/internal/adminauth"
	"adc.dev/ce/internal/alerts"
	"adc.dev/ce/internal/httpx"
)

// alert error codes extend the unified business error set (design/33 1.5).
const (
	codeAlertRuleInvalid = "15001"
	codeAlertRuleLimit   = "15002"
)

// maxAlertEventsList caps GET /v1/admin/alerts/events (design/82 B2: the
// history serves the most recent 100 events at most).
const maxAlertEventsList = 100

// alertRuleResponse is the wire shape of one rule (alerts.Rule without the
// tenant column, which the endpoint scopes implicitly).
type alertRuleResponse struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Metric      string            `json:"metric"`
	Operator    alerts.Operator   `json:"operator"`
	Threshold   float64           `json:"threshold"`
	DurationSec int               `json:"duration_sec"`
	Severity    alerts.Severity   `json:"severity"`
	Enabled     bool              `json:"enabled"`
	Labels      map[string]string `json:"labels,omitempty"`
	CreatedAt   time.Time         `json:"created_at"`
	UpdatedAt   time.Time         `json:"updated_at"`
}

func newAlertRuleResponse(r alerts.Rule) alertRuleResponse {
	return alertRuleResponse{
		ID:          r.ID,
		Name:        r.Name,
		Metric:      r.Metric,
		Operator:    r.Operator,
		Threshold:   r.Threshold,
		DurationSec: r.DurationSec,
		Severity:    r.Severity,
		Enabled:     r.Enabled,
		Labels:      r.Labels,
		CreatedAt:   r.CreatedAt.UTC(),
		UpdatedAt:   r.UpdatedAt.UTC(),
	}
}

// putAlertRulesRequest is the PUT body: the full replacement rule set plus
// the mandatory audit reason (change discipline shared with policies).
type putAlertRulesRequest struct {
	Rules        []alertRuleRequest `json:"rules"`
	ChangeReason string             `json:"change_reason"`
}

// alertRuleRequest mirrors the rule body; id is optional on create rows.
type alertRuleRequest struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Metric      string            `json:"metric"`
	Operator    alerts.Operator   `json:"operator"`
	Threshold   float64           `json:"threshold"`
	DurationSec int               `json:"duration_sec"`
	Severity    alerts.Severity   `json:"severity"`
	Enabled     bool              `json:"enabled"`
	Labels      map[string]string `json:"labels"`
}

// handleGetAlertRules serves GET /v1/admin/alerts/rules. Tenant scoping
// follows resolveTenant (design/33 1.2): tenant-scoped admins are locked
// to the session tenant, platform admins name the tenant_id.
func (s *Server) handleGetAlertRules(w http.ResponseWriter, r *http.Request) {
	p, ok := adminauth.FromContext(r.Context())
	if !ok {
		writeError(w, r, http.StatusUnauthorized, codeUnauthorized, "unauthenticated")
		return
	}
	tenantID, ok := s.resolveTenant(w, r, p)
	if !ok {
		return
	}
	if s.AlertRules == nil {
		writeError(w, r, http.StatusInternalServerError, codeInternal, "alert rule repository not wired")
		return
	}
	rules, err := s.AlertRules.List(r.Context(), tenantID)
	if err != nil {
		if errors.Is(err, alerts.ErrTenantNotFound) {
			writeError(w, r, http.StatusNotFound, codeTenantNotFound, "tenant not found")
			return
		}
		writeError(w, r, http.StatusInternalServerError, codeInternal, "load alert rules failed")
		return
	}
	out := make([]alertRuleResponse, 0, len(rules))
	for _, rule := range rules {
		out = append(out, newAlertRuleResponse(rule))
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"rules": out})
}

// handlePutAlertRules serves PUT /v1/admin/alerts/rules (full replace).
func (s *Server) handlePutAlertRules(w http.ResponseWriter, r *http.Request) {
	p, ok := adminauth.FromContext(r.Context())
	if !ok {
		writeError(w, r, http.StatusUnauthorized, codeUnauthorized, "unauthenticated")
		return
	}
	tenantID, ok := s.resolveTenant(w, r, p)
	if !ok {
		return
	}
	if s.AlertRules == nil {
		writeError(w, r, http.StatusInternalServerError, codeInternal, "alert rule repository not wired")
		return
	}
	var req putAlertRulesRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.ChangeReason) == "" {
		writeError(w, r, http.StatusBadRequest, codeBadRequest, "change_reason is required")
		return
	}
	if len(req.Rules) > alerts.MaxRulesPerTenant {
		writeError(w, r, http.StatusBadRequest, codeAlertRuleLimit, "at most 50 alert rules per tenant")
		return
	}
	rules := make([]alerts.Rule, 0, len(req.Rules))
	for _, in := range req.Rules {
		rule := alerts.Rule{
			ID:          strings.TrimSpace(in.ID),
			Name:        in.Name,
			Metric:      in.Metric,
			Operator:    in.Operator,
			Threshold:   in.Threshold,
			DurationSec: in.DurationSec,
			Severity:    in.Severity,
			Enabled:     in.Enabled,
			Labels:      in.Labels,
		}
		if err := alerts.ValidateRule(&rule); err != nil {
			writeError(w, r, http.StatusBadRequest, codeAlertRuleInvalid, err.Error())
			return
		}
		rules = append(rules, rule)
	}
	// Keep rule identity stable across PUTs: ids echoed back from GET are
	// reused (stable dedup keys, stable history references); new rows get
	// fresh ids. created_at is preserved for known ids, stamped now for
	// new rows, updated_at always now.
	current, err := s.AlertRules.List(r.Context(), tenantID)
	if err != nil && !errors.Is(err, alerts.ErrTenantNotFound) {
		writeError(w, r, http.StatusInternalServerError, codeInternal, "load alert rules failed")
		return
	}
	known := make(map[string]alerts.Rule, len(current))
	for _, c := range current {
		known[c.ID] = c
	}
	now := s.now().UTC()
	for i := range rules {
		if rules[i].ID == "" {
			rules[i].ID = alerts.NewRuleID()
			rules[i].CreatedAt = now
		} else if prev, exists := known[rules[i].ID]; exists && !prev.CreatedAt.IsZero() {
			rules[i].CreatedAt = prev.CreatedAt
		} else if rules[i].CreatedAt.IsZero() {
			rules[i].CreatedAt = now
		}
		rules[i].UpdatedAt = now
		rules[i].TenantID = tenantID
	}
	if err := s.AlertRules.Replace(r.Context(), tenantID, rules); err != nil {
		if errors.Is(err, alerts.ErrTenantNotFound) {
			writeError(w, r, http.StatusNotFound, codeTenantNotFound, "tenant not found")
			return
		}
		writeError(w, r, http.StatusInternalServerError, codeInternal, "update alert rules failed")
		return
	}
	s.recordAudit(r.Context(), AdminOp{
		EventID:  newEventID(),
		TenantID: tenantID,
		ActorID:  p.UserID,
		Action:   "alert_rules.update",
		Target:   tenantID,
		Reason:   req.ChangeReason,
		Details: map[string]any{
			"rule_count": len(rules),
		},
		TraceID: httpx.TraceIDFrom(r),
	})
	out := make([]alertRuleResponse, 0, len(rules))
	for _, rule := range rules {
		out = append(out, newAlertRuleResponse(rule))
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"rules": out})
}

// alertEventResponse is the wire shape of one fired alert.
type alertEventResponse struct {
	ID        string          `json:"id"`
	RuleID    string          `json:"rule_id"`
	RuleName  string          `json:"rule_name"`
	Metric    string          `json:"metric"`
	Operator  alerts.Operator `json:"operator"`
	Threshold float64         `json:"threshold"`
	Observed  float64         `json:"observed"`
	Severity  alerts.Severity `json:"severity"`
	FiredAt   time.Time       `json:"fired_at"`
}

// handleGetAlertEvents serves GET /v1/admin/alerts/events?limit=N (N in
// [1,100], default 20; design/82 B2 history cap).
func (s *Server) handleGetAlertEvents(w http.ResponseWriter, r *http.Request) {
	p, ok := adminauth.FromContext(r.Context())
	if !ok {
		writeError(w, r, http.StatusUnauthorized, codeUnauthorized, "unauthenticated")
		return
	}
	tenantID, ok := s.resolveTenant(w, r, p)
	if !ok {
		return
	}
	if s.AlertEvents == nil {
		writeError(w, r, http.StatusInternalServerError, codeInternal, "alert event store not wired")
		return
	}
	limit := 20
	if v := strings.TrimSpace(r.URL.Query().Get("limit")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxAlertEventsList {
			writeError(w, r, http.StatusBadRequest, codeBadRequest, "limit must be between 1 and 100")
			return
		}
		limit = n
	}
	events := s.AlertEvents.List(tenantID, limit)
	out := make([]alertEventResponse, 0, len(events))
	for _, ev := range events {
		out = append(out, alertEventResponse{
			ID:        ev.ID,
			RuleID:    ev.RuleID,
			RuleName:  ev.RuleName,
			Metric:    ev.Metric,
			Operator:  ev.Operator,
			Threshold: ev.Threshold,
			Observed:  ev.Observed,
			Severity:  ev.Severity,
			FiredAt:   ev.FiredAt.UTC(),
		})
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"events": out})
}
