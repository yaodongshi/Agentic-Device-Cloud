package adminapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// setupToolDevice registers one device in tenant "acme" and seeds two
// tools: get_status (risk 0) and set_speed (risk 2). It returns the
// registered device, the tenant id and a tenant_admin token.
func (e *testEnv) setupToolDevice(t *testing.T, code string) (deviceRegisterResponse, string, string) {
	t.Helper()
	tr := e.createTenant(t, "acme", "Acme", nil)
	tok := e.token(t, []string{"tenant_admin"}, tr.TenantID)
	rec := e.do(http.MethodPost, "/v1/admin/devices", tok, registerBody(code, "token"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("register %q: %d; body: %s", code, rec.Code, rec.Body.String())
	}
	var reg deviceRegisterResponse
	e.decode(t, rec, &reg)
	e.store.tools.seedTool(reg.DeviceID, tr.TenantID, "get_status", 0, true, "read status")
	e.store.tools.seedTool(reg.DeviceID, tr.TenantID, "set_speed", 2, true, "set spindle speed")
	return reg, tr.TenantID, tok
}

// doHeader is do() plus extra request headers (X-ADC-Confirm).
func (e *testEnv) doHeader(method, path, token string, body any, hdr map[string]string) *httptest.ResponseRecorder {
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			panic(err)
		}
		rd = strings.NewReader(string(raw))
	}
	req := httptest.NewRequest(method, path, rd)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	return rec
}

func patchToolsBody(changes []map[string]any, reason string) map[string]any {
	return map[string]any{"changes": changes, "change_reason": reason}
}

func TestListDeviceToolsWithRiskLevels(t *testing.T) {
	env := newTestEnv(t)
	reg, _, tok := env.setupToolDevice(t, "cnc-tools")
	rec := env.do(http.MethodGet, "/v1/admin/devices/"+reg.DeviceID+"/tools", tok, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body: %s", rec.Code, rec.Body.String())
	}
	var out deviceToolListResponse
	env.decode(t, rec, &out)
	if out.Total != 2 || len(out.Items) != 2 {
		t.Fatalf("total = %d items = %d, want 2/2", out.Total, len(out.Items))
	}
	// deterministic name order: get_status before set_speed.
	if out.Items[0].Name != "get_status" || out.Items[0].RiskLevel != 0 || !out.Items[0].IsEnabled {
		t.Fatalf("item[0] = %+v", out.Items[0])
	}
	if out.Items[1].Name != "set_speed" || out.Items[1].RiskLevel != 2 {
		t.Fatalf("item[1] = %+v", out.Items[1])
	}
	// device not found.
	rec = env.do(http.MethodGet, "/v1/admin/devices/"+uuidOf(777)+"/tools", tok, nil)
	env.assertError(t, rec, http.StatusNotFound, codeDeviceNotFound)
}

func TestPatchDeviceToolsDowngradeRequiresConfirmAndAudits(t *testing.T) {
	env := newTestEnv(t)
	reg, _, tok := env.setupToolDevice(t, "cnc-down")
	body := patchToolsBody([]map[string]any{{"name": "set_speed", "risk_level": 0}}, "安全评审决议降级")

	// downgrade 2 -> 0 without the confirm header: 400.
	rec := env.do(http.MethodPatch, "/v1/admin/devices/"+reg.DeviceID+"/tools", tok, body)
	env.assertError(t, rec, http.StatusBadRequest, codeBadRequest)
	// the ledger is untouched.
	got, _ := env.store.tools.get(reg.DeviceID, "set_speed")
	if got.RiskLevel != 2 {
		t.Fatalf("risk changed without confirmation: %d", got.RiskLevel)
	}

	// with X-ADC-Confirm: true the downgrade applies.
	rec = env.doHeader(http.MethodPatch, "/v1/admin/devices/"+reg.DeviceID+"/tools", tok, body,
		map[string]string{confirmHeader: "true"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body: %s", rec.Code, rec.Body.String())
	}
	var out patchToolsResponse
	env.decode(t, rec, &out)
	if out.Changed != 1 || len(out.Failed) != 0 {
		t.Fatalf("response = %+v", out)
	}
	// DB semantics: risk persisted and attributable (design/32 3.6).
	got, _ = env.store.tools.get(reg.DeviceID, "set_speed")
	if got.RiskLevel != 0 || got.RiskChangedBy != uuidOf(900000) {
		t.Fatalf("stored tool = %+v, want risk 0 changed_by %s", got, uuidOf(900000))
	}
	// audit event carries before/after plus the reason (design/31 3.4.4).
	op, ok := env.audit.last()
	if !ok || op.Action != "tool.configure" || op.Reason == "" {
		t.Fatalf("audit = %+v, want tool.configure with reason", op)
	}
	if op.Details["risk_before"] != 2 || op.Details["risk_after"] != 0 {
		t.Fatalf("audit details = %+v", op.Details)
	}
	if op.Details["tool_name"] != "set_speed" {
		t.Fatalf("audit details = %+v", op.Details)
	}
}

func TestPatchDeviceToolsUpgradeAuditedEqually(t *testing.T) {
	env := newTestEnv(t)
	reg, _, tok := env.setupToolDevice(t, "cnc-up")
	// upgrade 2 -> 3 needs no second confirmation but the same audit
	// strength (design/31 3.4.4).
	rec := env.do(http.MethodPatch, "/v1/admin/devices/"+reg.DeviceID+"/tools", tok,
		patchToolsBody([]map[string]any{{"name": "set_speed", "risk_level": 3}}, "升级为极危"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body: %s", rec.Code, rec.Body.String())
	}
	got, _ := env.store.tools.get(reg.DeviceID, "set_speed")
	if got.RiskLevel != 3 || got.RiskChangedBy != uuidOf(900000) {
		t.Fatalf("stored tool = %+v", got)
	}
	op, ok := env.audit.last()
	if !ok || op.Details["risk_before"] != 2 || op.Details["risk_after"] != 3 {
		t.Fatalf("audit = %+v", op)
	}
}

func TestPatchDeviceToolsToggleEnabled(t *testing.T) {
	env := newTestEnv(t)
	reg, _, tok := env.setupToolDevice(t, "cnc-en")
	rec := env.do(http.MethodPatch, "/v1/admin/devices/"+reg.DeviceID+"/tools", tok,
		patchToolsBody([]map[string]any{{"name": "get_status", "is_enabled": false}}, "临时禁用"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body: %s", rec.Code, rec.Body.String())
	}
	got, _ := env.store.tools.get(reg.DeviceID, "get_status")
	if got.IsEnabled || got.RiskChangedBy != "" {
		t.Fatalf("stored tool = %+v: enabled flip must not touch risk attribution", got)
	}
	op, _ := env.audit.last()
	if op.Details["is_enabled_before"] != true || op.Details["is_enabled_after"] != false {
		t.Fatalf("audit details = %+v", op.Details)
	}
}

func TestPatchDeviceToolsReasonRequired(t *testing.T) {
	env := newTestEnv(t)
	reg, _, tok := env.setupToolDevice(t, "cnc-noreason")
	rec := env.do(http.MethodPatch, "/v1/admin/devices/"+reg.DeviceID+"/tools", tok,
		map[string]any{"changes": []map[string]any{{"name": "set_speed", "risk_level": 3}}})
	env.assertError(t, rec, http.StatusBadRequest, codeBadRequest)
	rec = env.do(http.MethodPatch, "/v1/admin/devices/"+reg.DeviceID+"/tools", tok,
		patchToolsBody([]map[string]any{{"name": "set_speed", "risk_level": 3}}, "   "))
	env.assertError(t, rec, http.StatusBadRequest, codeBadRequest)
}

func TestPatchDeviceToolsValidationAndPartialFailure(t *testing.T) {
	env := newTestEnv(t)
	reg, _, tok := env.setupToolDevice(t, "cnc-val")

	bad := []map[string]any{
		patchToolsBody([]map[string]any{{"name": "set_speed", "risk_level": 4}}, "x"),              // out of range
		patchToolsBody([]map[string]any{{"name": "set_speed", "risk_level": -1}}, "x"),             // out of range
		patchToolsBody([]map[string]any{{"name": "set_speed"}}, "x"),                               // no field set
		patchToolsBody([]map[string]any{}, "x"),                                                    // empty batch
		patchToolsBody([]map[string]any{{"name": "", "risk_level": 1}}, "x"),                       // empty name
		patchToolsBody([]map[string]any{{"name": strings.Repeat("t", 129), "risk_level": 1}}, "x"), // too long
	}
	for i, body := range bad {
		rec := env.do(http.MethodPatch, "/v1/admin/devices/"+reg.DeviceID+"/tools", tok, body)
		env.assertError(t, rec, http.StatusBadRequest, codeBadRequest)
		t.Logf("case %d rejected as expected", i)
	}

	// unknown tool name rides in failed[], the rest of the batch applies.
	rec := env.do(http.MethodPatch, "/v1/admin/devices/"+reg.DeviceID+"/tools", tok,
		patchToolsBody([]map[string]any{
			{"name": "get_status", "risk_level": 1},
			{"name": "no_such_tool", "risk_level": 0},
		}, "批量配置"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body: %s", rec.Code, rec.Body.String())
	}
	var out patchToolsResponse
	env.decode(t, rec, &out)
	if out.Changed != 1 || len(out.Failed) != 1 {
		t.Fatalf("response = %+v", out)
	}
	if out.Failed[0].Name != "no_such_tool" || out.Failed[0].Code != codeToolNotFound {
		t.Fatalf("failed = %+v", out.Failed[0])
	}
	got, _ := env.store.tools.get(reg.DeviceID, "get_status")
	if got.RiskLevel != 1 {
		t.Fatalf("partial batch must still apply the valid change: %+v", got)
	}
}

func TestPatchDeviceToolsCrossTenantAndNotFound(t *testing.T) {
	env := newTestEnv(t)
	reg, _, tok := env.setupToolDevice(t, "cnc-x")
	other := env.createTenant(t, "other", "Other", nil)
	otok := env.token(t, []string{"tenant_admin"}, other.TenantID)

	rec := env.do(http.MethodPatch, "/v1/admin/devices/"+reg.DeviceID+"/tools", otok,
		patchToolsBody([]map[string]any{{"name": "set_speed", "risk_level": 0}}, "x"))
	env.assertError(t, rec, http.StatusForbidden, codeCrossTenant)
	// list is equally tenant-guarded.
	rec = env.do(http.MethodGet, "/v1/admin/devices/"+reg.DeviceID+"/tools", otok, nil)
	env.assertError(t, rec, http.StatusForbidden, codeCrossTenant)

	rec = env.do(http.MethodPatch, "/v1/admin/devices/"+uuidOf(777)+"/tools", tok,
		patchToolsBody([]map[string]any{{"name": "set_speed", "risk_level": 0}}, "x"))
	env.assertError(t, rec, http.StatusNotFound, codeDeviceNotFound)
	rec = env.do(http.MethodPatch, "/v1/admin/devices/not-a-uuid/tools", tok,
		patchToolsBody([]map[string]any{{"name": "set_speed", "risk_level": 0}}, "x"))
	env.assertError(t, rec, http.StatusBadRequest, codeBadRequest)
}

func TestDeviceToolsRoleMatrix(t *testing.T) {
	env := newTestEnv(t)
	reg, tenantID, _ := env.setupToolDevice(t, "cnc-role")
	// approver may read devices but not tool configuration.
	app := env.token(t, []string{"approver"}, tenantID)
	rec := env.do(http.MethodGet, "/v1/admin/devices/"+reg.DeviceID+"/tools", app, nil)
	env.assertError(t, rec, http.StatusForbidden, codeForbidden)
	// unauthenticated.
	rec = env.do(http.MethodGet, "/v1/admin/devices/"+reg.DeviceID+"/tools", "", nil)
	env.assertError(t, rec, http.StatusUnauthorized, codeUnauthorized)
}
