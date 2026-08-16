package adminapi

import (
	"net/http"
	"testing"
	"time"

	"adc.dev/ce/internal/alerts"
)

// setupAlerts wires the alert seams onto the test env and rebuilds the
// handler (newTestEnv builds the handler before the seams are set).
func (e *testEnv) setupAlerts(t *testing.T) (*alerts.MemoryRuleStore, *alerts.EventBuffer) {
	t.Helper()
	store := alerts.NewMemoryRuleStore()
	events := alerts.NewEventBuffer(alerts.DefaultEventCapacity)
	e.srv.AlertRules = store
	e.srv.AlertEvents = events
	e.handler = e.srv.Handler()
	return store, events
}

type putRuleRequest struct {
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

func putAlertRulesBody(rules []putRuleRequest, reason string) map[string]any {
	return map[string]any{"rules": rules, "change_reason": reason}
}

func poolRule(name string) putRuleRequest {
	return putRuleRequest{
		Name:        name,
		Metric:      "adc_pg_pool_connections",
		Operator:    alerts.OpGreater,
		Threshold:   0.8,
		DurationSec: 300,
		Severity:    alerts.SeverityP2,
		Enabled:     true,
	}
}

// TestAlertRulesCRUD exercises the full PUT -> GET -> PUT replace cycle
// (FR-017 rule configuration).
func TestAlertRulesCRUD(t *testing.T) {
	env := newTestEnv(t)
	_, _ = env.setupAlerts(t)
	tr := env.createTenant(t, "acme", "Acme", nil)
	tok := env.token(t, []string{"tenant_admin"}, tr.TenantID)

	// PUT two rules.
	rec := env.do(http.MethodPut, "/v1/admin/alerts/rules", tok,
		putAlertRulesBody([]putRuleRequest{poolRule("pool watermark"), poolRule("pool critical")}, "seed rules"))
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d; body: %s", rec.Code, rec.Body.String())
	}
	var putResp struct {
		Rules []struct {
			ID        string `json:"id"`
			Name      string `json:"name"`
			CreatedAt string `json:"created_at"`
			UpdatedAt string `json:"updated_at"`
		} `json:"rules"`
	}
	env.decode(t, rec, &putResp)
	if len(putResp.Rules) != 2 {
		t.Fatalf("PUT returned %d rules, want 2", len(putResp.Rules))
	}
	if putResp.Rules[0].ID == "" || putResp.Rules[0].CreatedAt == "" {
		t.Fatalf("PUT rule missing id/created_at: %+v", putResp.Rules[0])
	}
	firstID := putResp.Rules[0].ID

	// GET echoes the stored set.
	rec = env.do(http.MethodGet, "/v1/admin/alerts/rules", tok, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d; body: %s", rec.Code, rec.Body.String())
	}
	var getResp struct {
		Rules []struct {
			ID       string `json:"id"`
			Metric   string `json:"metric"`
			Severity string `json:"severity"`
			Enabled  bool   `json:"enabled"`
		} `json:"rules"`
	}
	env.decode(t, rec, &getResp)
	if len(getResp.Rules) != 2 || getResp.Rules[0].Severity != "P2" || !getResp.Rules[0].Enabled {
		t.Fatalf("GET rules = %+v", getResp.Rules)
	}

	// PUT replace: only one rule survives (full-replace semantics) and the
	// echoed id stays stable (dedup key stability).
	rec = env.do(http.MethodPut, "/v1/admin/alerts/rules", tok,
		putAlertRulesBody([]putRuleRequest{{ID: firstID, Name: "pool watermark", Metric: "adc_pg_pool_connections",
			Operator: alerts.OpGreaterOrEqual, Threshold: 0.9, DurationSec: 60,
			Severity: alerts.SeverityP1, Enabled: true}}, "tune threshold"))
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT replace status = %d; body: %s", rec.Code, rec.Body.String())
	}
	env.decode(t, rec, &putResp)
	if len(putResp.Rules) != 1 || putResp.Rules[0].ID != firstID {
		t.Fatalf("PUT replace rules = %+v, want the same id %s", putResp.Rules, firstID)
	}
	rec = env.do(http.MethodGet, "/v1/admin/alerts/rules", tok, nil)
	env.decode(t, rec, &getResp)
	if len(getResp.Rules) != 1 {
		t.Fatalf("GET after replace = %+v, want 1 rule", getResp.Rules)
	}

	// The mutation hit the audit trail with the change reason.
	op, ok := env.audit.last()
	if !ok || op.Action != "alert_rules.update" || op.Reason != "tune threshold" {
		t.Fatalf("audit op = %+v", op)
	}
}

// TestAlertRulesValidation rejects malformed rules with 15001 and overlong
// lists with 15002.
func TestAlertRulesValidation(t *testing.T) {
	env := newTestEnv(t)
	_, _ = env.setupAlerts(t)
	tr := env.createTenant(t, "acme", "Acme", nil)
	tok := env.token(t, []string{"tenant_admin"}, tr.TenantID)

	bad := poolRule("bad")
	bad.Metric = "adc_not_a_metric_total"
	rec := env.do(http.MethodPut, "/v1/admin/alerts/rules", tok, putAlertRulesBody([]putRuleRequest{bad}, "try"))
	env.assertError(t, rec, http.StatusBadRequest, codeAlertRuleInvalid)

	bad = poolRule("bad")
	bad.Severity = "P0"
	rec = env.do(http.MethodPut, "/v1/admin/alerts/rules", tok, putAlertRulesBody([]putRuleRequest{bad}, "try"))
	env.assertError(t, rec, http.StatusBadRequest, codeAlertRuleInvalid)

	bad = poolRule("bad")
	bad.Threshold = -5
	rec = env.do(http.MethodPut, "/v1/admin/alerts/rules", tok, putAlertRulesBody([]putRuleRequest{bad}, "try"))
	env.assertError(t, rec, http.StatusBadRequest, codeAlertRuleInvalid)

	many := make([]putRuleRequest, alerts.MaxRulesPerTenant+1)
	for i := range many {
		many[i] = poolRule("rule")
	}
	rec = env.do(http.MethodPut, "/v1/admin/alerts/rules", tok, putAlertRulesBody(many, "too many"))
	env.assertError(t, rec, http.StatusBadRequest, codeAlertRuleLimit)

	// change_reason is mandatory (audit discipline).
	rec = env.do(http.MethodPut, "/v1/admin/alerts/rules", tok, map[string]any{"rules": []putRuleRequest{poolRule("r")}})
	env.assertError(t, rec, http.StatusBadRequest, codeBadRequest)
}

// TestAlertRulesPermissionMatrix: the alert routes are admin-only; the
// approver and unauthenticated callers are rejected by the RBAC chain.
func TestAlertRulesPermissionMatrix(t *testing.T) {
	env := newTestEnv(t)
	_, _ = env.setupAlerts(t)
	tr := env.createTenant(t, "acme", "Acme", nil)

	approverTok := env.token(t, []string{"approver"}, tr.TenantID)
	rec := env.do(http.MethodGet, "/v1/admin/alerts/rules", approverTok, nil)
	env.assertError(t, rec, http.StatusForbidden, codeForbidden)
	rec = env.do(http.MethodPut, "/v1/admin/alerts/rules", approverTok, putAlertRulesBody([]putRuleRequest{poolRule("r")}, "try"))
	env.assertError(t, rec, http.StatusForbidden, codeForbidden)
	rec = env.do(http.MethodGet, "/v1/admin/alerts/events", approverTok, nil)
	env.assertError(t, rec, http.StatusForbidden, codeForbidden)

	rec = env.do(http.MethodGet, "/v1/admin/alerts/rules", "", nil)
	env.assertError(t, rec, http.StatusUnauthorized, codeUnauthorized)
}

// TestAlertRulesPlatformAdminNeedsTenantID: platform admins must name the
// tenant via tenant_id (design/33 1.2).
func TestAlertRulesPlatformAdminNeedsTenantID(t *testing.T) {
	env := newTestEnv(t)
	_, _ = env.setupAlerts(t)
	tr := env.createTenant(t, "acme", "Acme", nil)
	tok := env.token(t, []string{rolePlatformAdmin}, "")

	rec := env.do(http.MethodGet, "/v1/admin/alerts/rules", tok, nil)
	env.assertError(t, rec, http.StatusBadRequest, codeBadRequest)

	rec = env.do(http.MethodGet, "/v1/admin/alerts/rules?tenant_id="+tr.TenantID, tok, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("platform GET with tenant_id = %d; body: %s", rec.Code, rec.Body.String())
	}
}

// TestAlertEventsEndpoint: the history serves the newest tenant events,
// clamped to the limit (FR-017 "最近 100 条").
func TestAlertEventsEndpoint(t *testing.T) {
	env := newTestEnv(t)
	_, events := env.setupAlerts(t)
	tr := env.createTenant(t, "acme", "Acme", nil)
	tr2 := env.createTenant(t, "other", "Other", nil)
	tok := env.token(t, []string{"tenant_admin"}, tr.TenantID)

	base := time.Date(2026, 8, 14, 8, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		events.Record(alerts.Event{
			ID:       "e-" + string(rune('0'+i)),
			RuleID:   "r1",
			RuleName: "pool",
			TenantID: tr.TenantID,
			Metric:   "adc_pg_pool_connections",
			Severity: alerts.SeverityP2,
			FiredAt:  base.Add(time.Duration(i) * time.Minute),
		})
	}
	events.Record(alerts.Event{
		ID:       "e-x",
		RuleID:   "r2",
		RuleName: "other tenant",
		TenantID: tr2.TenantID,
		Metric:   "adc_pg_pool_connections",
		Severity: alerts.SeverityP1,
		FiredAt:  base.Add(10 * time.Minute),
	})

	rec := env.do(http.MethodGet, "/v1/admin/alerts/events?limit=3", tok, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("events status = %d; body: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Events []struct {
			ID       string `json:"id"`
			Severity string `json:"severity"`
			FiredAt  string `json:"fired_at"`
		} `json:"events"`
	}
	env.decode(t, rec, &resp)
	if len(resp.Events) != 3 {
		t.Fatalf("events = %d, want 3 (limit)", len(resp.Events))
	}
	// Newest first and tenant-scoped: no other-tenant event may leak.
	for _, ev := range resp.Events {
		if ev.ID == "e-x" {
			t.Fatal("other tenant's event leaked into the list")
		}
	}
	if resp.Events[0].ID != "e-4" {
		t.Fatalf("newest event = %q, want e-4", resp.Events[0].ID)
	}

	// Invalid limit is rejected.
	rec = env.do(http.MethodGet, "/v1/admin/alerts/events?limit=1000", tok, nil)
	env.assertError(t, rec, http.StatusBadRequest, codeBadRequest)
	rec = env.do(http.MethodGet, "/v1/admin/alerts/events?limit=nope", tok, nil)
	env.assertError(t, rec, http.StatusBadRequest, codeBadRequest)
}

// TestAlertSeamsUnwiredFailClosed: a missing seam answers 500 code 10007
// rather than panicking (design/31 3.4.2).
func TestAlertSeamsUnwiredFailClosed(t *testing.T) {
	env := newTestEnv(t)
	tr := env.createTenant(t, "acme", "Acme", nil)
	tok := env.token(t, []string{"tenant_admin"}, tr.TenantID)

	rec := env.do(http.MethodGet, "/v1/admin/alerts/rules", tok, nil)
	env.assertError(t, rec, http.StatusInternalServerError, codeInternal)
	rec = env.do(http.MethodPut, "/v1/admin/alerts/rules", tok, putAlertRulesBody([]putRuleRequest{poolRule("r")}, "try"))
	env.assertError(t, rec, http.StatusInternalServerError, codeInternal)
	rec = env.do(http.MethodGet, "/v1/admin/alerts/events", tok, nil)
	env.assertError(t, rec, http.StatusInternalServerError, codeInternal)
}

// TestAlertRulesLabelRoundTrip: label filters survive the wire.
func TestAlertRulesLabelRoundTrip(t *testing.T) {
	env := newTestEnv(t)
	_, _ = env.setupAlerts(t)
	tr := env.createTenant(t, "acme", "Acme", nil)
	tok := env.token(t, []string{"tenant_admin"}, tr.TenantID)

	rule := poolRule("wecom push")
	rule.Metric = "adc_hitl_notify_failures_total"
	rule.Labels = map[string]string{"channel": "wecom"}
	rec := env.do(http.MethodPut, "/v1/admin/alerts/rules", tok, putAlertRulesBody([]putRuleRequest{rule}, "labels"))
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d; body: %s", rec.Code, rec.Body.String())
	}
	rec = env.do(http.MethodGet, "/v1/admin/alerts/rules", tok, nil)
	var resp struct {
		Rules []struct {
			Labels map[string]string `json:"labels"`
		} `json:"rules"`
	}
	env.decode(t, rec, &resp)
	if len(resp.Rules) != 1 || resp.Rules[0].Labels["channel"] != "wecom" {
		t.Fatalf("label round trip = %+v", resp.Rules)
	}
	// A malformed label key is rejected server-side.
	rule.Labels = map[string]string{"bad key!": "x"}
	rec = env.do(http.MethodPut, "/v1/admin/alerts/rules", tok, putAlertRulesBody([]putRuleRequest{rule}, "labels"))
	env.assertError(t, rec, http.StatusBadRequest, codeAlertRuleInvalid)
}
