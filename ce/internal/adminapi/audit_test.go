package adminapi

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// seedAuditLogs plants a deterministic audit set in one tenant:
//
//	2000-01-01 08:00Z  set_speed    success          agent-prod cnc-01
//	2000-01-01 08:01Z  set_speed    blocked_by_hitl  agent-prod cnc-01
//	2000-01-01 08:02Z  get_status   success          agent-qa   cnc-02
//	2000-01-01 08:03Z  set_speed    failed           agent-prod cnc-01
//	2000-01-01 08:04Z  admin_op     (no tool)        admin
//
// created_at uses fixedNow-relative offsets so ordering is
// deterministic and stable against the fixed test clock.
func seedAuditLogs(env *testEnv, tenantID string) {
	base := fixedNow
	for _, l := range []AuditLog{
		{ID: "l_0001", TenantID: tenantID, EventType: "tool_call", ActorType: "agent", ActorID: "agent-prod", DeviceID: uuidOf(400001), DeviceCode: "cnc-01", ToolName: "set_speed", RiskLevel: intPtr(2), Status: "success", RequestID: uuidOf(500001), CreatedAt: base.Add(-4 * time.Minute)},
		{ID: "l_0002", TenantID: tenantID, EventType: "tool_call", ActorType: "agent", ActorID: "agent-prod", DeviceID: uuidOf(400001), DeviceCode: "cnc-01", ToolName: "set_speed", RiskLevel: intPtr(2), Status: "blocked_by_hitl", HitlApprover: "emp_zhangwei", RequestID: uuidOf(500002), CreatedAt: base.Add(-3 * time.Minute)},
		{ID: "l_0003", TenantID: tenantID, EventType: "tool_call", ActorType: "agent", ActorID: "agent-qa", DeviceID: uuidOf(400002), DeviceCode: "cnc-02", ToolName: "get_status", RiskLevel: intPtr(0), Status: "success", RequestID: uuidOf(500003), CreatedAt: base.Add(-2 * time.Minute)},
		{ID: "l_0004", TenantID: tenantID, EventType: "tool_call", ActorType: "agent", ActorID: "agent-prod", DeviceID: uuidOf(400001), DeviceCode: "cnc-01", ToolName: "set_speed", RiskLevel: intPtr(2), Status: "failed", RequestID: uuidOf(500004), CreatedAt: base.Add(-1 * time.Minute)},
		{ID: "l_0005", TenantID: tenantID, EventType: "admin_op", ActorType: "user", ActorID: "itadmin", Status: "success", RequestID: uuidOf(500005), CreatedAt: base},
	} {
		env.store.auditLogs.seed(l)
	}
}

func TestAuditLogsFilters(t *testing.T) {
	env := newTestEnv(t)
	tr := env.createTenant(t, "acme", "Acme", nil)
	seedAuditLogs(env, tr.TenantID)
	tok := env.token(t, []string{"tenant_admin"}, tr.TenantID)
	path := "/v1/admin/audit-logs"

	cases := []struct {
		query string
		want  int
	}{
		{"", 5},
		{"?tool_name=set_speed", 3},
		{"?status=blocked_by_hitl", 1},
		{"?status=success&tool_name=set_speed", 1},
		{"?device_code=cnc-01", 3},
		{"?device_id=" + uuidOf(400002), 1},
		{"?event_type=admin_op", 1},
		{"?agent_id=agent-qa", 1},
		{"?keyword=speed", 3},    // matches tool_name
		{"?keyword=zhangwei", 1}, // matches hitl_approver
		{"?time_from=" + fixedNow.Add(-90*time.Second).UTC().Format(time.RFC3339), 2},
		{"?time_to=" + fixedNow.Add(-150*time.Second).UTC().Format(time.RFC3339), 2},
	}
	for _, c := range cases {
		rec := env.do(http.MethodGet, path+c.query, tok, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("query %q: %d; body: %s", c.query, rec.Code, rec.Body.String())
		}
		var out auditLogListResponse
		env.decode(t, rec, &out)
		if len(out.Items) != c.want {
			t.Fatalf("query %q: items = %d, want %d", c.query, len(out.Items), c.want)
		}
	}
}

func TestAuditLogsCursorPagination(t *testing.T) {
	env := newTestEnv(t)
	tr := env.createTenant(t, "acme", "Acme", nil)
	seedAuditLogs(env, tr.TenantID)
	tok := env.token(t, []string{"tenant_admin"}, tr.TenantID)

	seen := map[string]bool{}
	cursor := ""
	for {
		path := "/v1/admin/audit-logs?limit=2"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		rec := env.do(http.MethodGet, path, tok, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d; body: %s", rec.Code, rec.Body.String())
		}
		var out auditLogListResponse
		env.decode(t, rec, &out)
		for _, it := range out.Items {
			if seen[it.LogID] {
				t.Fatalf("cursor page repeats log %s", it.LogID)
			}
			seen[it.LogID] = true
		}
		if out.NextCursor == nil {
			break
		}
		cursor = *out.NextCursor
	}
	if len(seen) != 5 {
		t.Fatalf("pages covered %d logs, want 5", len(seen))
	}
	// descending order must hold across the full walk.
	ids := []string{"l_0005", "l_0004", "l_0003", "l_0002", "l_0001"}
	for _, id := range ids {
		if !seen[id] {
			t.Fatalf("log %s missing from the walk", id)
		}
	}
}

func TestAuditLogsInvalidConditions(t *testing.T) {
	env := newTestEnv(t)
	tr := env.createTenant(t, "acme", "Acme", nil)
	seedAuditLogs(env, tr.TenantID)
	tok := env.token(t, []string{"tenant_admin"}, tr.TenantID)

	base := fixedNow.UTC().Format(time.RFC3339)
	bad := []string{
		"?time_from=" + base + "&time_to=" + fixedNow.Add(-time.Hour).UTC().Format(time.RFC3339), // from > to
		"?time_from=2026-13-45T00:00:00Z", // malformed time
		"?status=bogus",                   // unknown status
		"?event_type=bogus",               // unknown event type
		"?device_id=not-a-uuid",           // malformed device id
		"?limit=0",
		"?limit=501",
		"?cursor=!!!not-base64!!!",
		"?cursor=" + encodeAuditCursor(&AuditCursor{CreatedAt: fixedNow, ID: ""}), // empty id inside a valid envelope
	}
	for i, q := range bad {
		rec := env.do(http.MethodGet, "/v1/admin/audit-logs"+q, tok, nil)
		env.assertError(t, rec, http.StatusBadRequest, codeAuditQueryInvalid)
		t.Logf("case %d rejected as expected", i)
	}
}

func TestAuditLogsRoleAndTenantScope(t *testing.T) {
	env := newTestEnv(t)
	a := env.createTenant(t, "acme-a", "Acme A", nil)
	b := env.createTenant(t, "acme-b", "Acme B", nil)
	seedAuditLogs(env, a.TenantID)
	seedAuditLogs(env, b.TenantID)

	// auditor may read, but only its own tenant's rows.
	aud := env.token(t, []string{"auditor"}, a.TenantID)
	rec := env.do(http.MethodGet, "/v1/admin/audit-logs", aud, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("auditor query: %d; body: %s", rec.Code, rec.Body.String())
	}
	var out auditLogListResponse
	env.decode(t, rec, &out)
	for _, it := range out.Items {
		if it.LogID == "" {
			t.Fatalf("malformed item: %+v", it)
		}
	}
	if len(out.Items) != 5 {
		t.Fatalf("auditor saw %d rows, want only its tenant's 5", len(out.Items))
	}

	// auditor export is denied by the read-only matrix (design/33 1.2
	// intends auditor export; widening the matrix is a follow-up in
	// adminauth, see server.go route comment).
	rec = env.do(http.MethodPost, "/v1/admin/audit-logs/export?tenant_id="+a.TenantID, aud,
		map[string]any{"time_from": fixedNow.Add(-time.Hour).UTC().Format(time.RFC3339), "time_to": fixedNow.Format(time.RFC3339)})
	env.assertError(t, rec, http.StatusForbidden, codeForbidden)

	// tenant_admin cannot switch tenants on the read surface either.
	tok := env.token(t, []string{"tenant_admin"}, a.TenantID)
	rec = env.do(http.MethodGet, "/v1/admin/audit-logs?tenant_id="+b.TenantID, tok, nil)
	env.assertError(t, rec, http.StatusForbidden, codeCrossTenant)

	// platform admin must name the target tenant.
	ptok := env.token(t, []string{rolePlatformAdmin}, "")
	rec = env.do(http.MethodGet, "/v1/admin/audit-logs", ptok, nil)
	env.assertError(t, rec, http.StatusBadRequest, codeBadRequest)
	rec = env.do(http.MethodGet, "/v1/admin/audit-logs?tenant_id="+b.TenantID, ptok, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("platform admin cross-tenant query: %d; body: %s", rec.Code, rec.Body.String())
	}

	// unauthenticated.
	rec = env.do(http.MethodGet, "/v1/admin/audit-logs", "", nil)
	env.assertError(t, rec, http.StatusUnauthorized, codeUnauthorized)
}

func exportBody(from, to string, extra map[string]any) map[string]any {
	body := map[string]any{"time_from": from, "time_to": to}
	for k, v := range extra {
		body[k] = v
	}
	return body
}

func TestAuditExportCSV(t *testing.T) {
	env := newTestEnv(t)
	tr := env.createTenant(t, "acme", "Acme", nil)
	seedAuditLogs(env, tr.TenantID)
	tok := env.token(t, []string{"tenant_admin"}, tr.TenantID)

	from := fixedNow.Add(-10 * time.Minute).UTC().Format(time.RFC3339)
	to := fixedNow.Add(time.Minute).UTC().Format(time.RFC3339)
	rec := env.do(http.MethodPost, "/v1/admin/audit-logs/export", tok, exportBody(from, to, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/csv") {
		t.Fatalf("content-type = %q", ct)
	}
	body := rec.Body.String()
	lines := strings.Split(strings.TrimRight(body, "\n"), "\n")
	if len(lines) != 6 { // header + 5 rows
		t.Fatalf("csv lines = %d, want 6; body: %s", len(lines), body)
	}
	if !strings.HasPrefix(lines[0], "log_id") || !strings.Contains(lines[0], "risk_level") {
		t.Fatalf("csv header = %q", lines[0])
	}
	if !strings.Contains(body, "l_0001") || !strings.Contains(body, "cnc-01") || !strings.Contains(body, "set_speed") {
		t.Fatalf("csv rows missing data: %s", body)
	}

	// filters narrow the export (device + tool).
	rec = env.do(http.MethodPost, "/v1/admin/audit-logs/export", tok,
		exportBody(from, to, map[string]any{"device_code": "cnc-02"}))
	if rec.Code != http.StatusOK {
		t.Fatalf("filtered export: %d; body: %s", rec.Code, rec.Body.String())
	}
	if got := strings.Split(strings.TrimRight(rec.Body.String(), "\n"), "\n"); len(got) != 2 {
		t.Fatalf("filtered csv lines = %d, want 2; body: %s", len(got), rec.Body.String())
	}

	// the export action itself is audited.
	op, ok := env.audit.last()
	if !ok || op.Action != "audit.export" {
		t.Fatalf("audit = %+v, want audit.export", op)
	}
}

func TestAuditExportRowCap413(t *testing.T) {
	env := newTestEnv(t)
	tr := env.createTenant(t, "acme", "Acme", nil)
	seedAuditLogs(env, tr.TenantID)
	env.srv.ExportMaxRows = 3
	tok := env.token(t, []string{"tenant_admin"}, tr.TenantID)

	from := fixedNow.Add(-10 * time.Minute).UTC().Format(time.RFC3339)
	to := fixedNow.Add(time.Minute).UTC().Format(time.RFC3339)
	// 5 rows over a 3-row cap: 413 with a narrowing hint.
	rec := env.do(http.MethodPost, "/v1/admin/audit-logs/export", tok, exportBody(from, to, nil))
	env.assertError(t, rec, http.StatusRequestEntityTooLarge, codeAuditExportLimit)
	if !strings.Contains(rec.Body.String(), "narrow") {
		t.Fatalf("413 body must hint at narrowing the range: %s", rec.Body.String())
	}
	// the over-limit attempt is not recorded as a successful export.
	op, _ := env.audit.last()
	if op.Action == "audit.export" {
		t.Fatalf("over-limit attempt must not audit as export: %+v", op)
	}

	// inside the cap the same request succeeds (tool filter trims to 3).
	rec = env.do(http.MethodPost, "/v1/admin/audit-logs/export", tok,
		exportBody(from, to, map[string]any{"tool_name": "set_speed"}))
	if rec.Code != http.StatusOK {
		t.Fatalf("capped export: %d; body: %s", rec.Code, rec.Body.String())
	}
	if got := strings.Split(strings.TrimRight(rec.Body.String(), "\n"), "\n"); len(got) != 4 {
		t.Fatalf("capped csv lines = %d, want 4; body: %s", len(got), rec.Body.String())
	}
}

func TestAuditExportRequiresTimeRange(t *testing.T) {
	env := newTestEnv(t)
	tr := env.createTenant(t, "acme", "Acme", nil)
	seedAuditLogs(env, tr.TenantID)
	tok := env.token(t, []string{"tenant_admin"}, tr.TenantID)

	rec := env.do(http.MethodPost, "/v1/admin/audit-logs/export", tok, map[string]any{})
	env.assertError(t, rec, http.StatusBadRequest, codeAuditQueryInvalid)

	rec = env.do(http.MethodPost, "/v1/admin/audit-logs/export", tok,
		map[string]any{"time_from": fixedNow.Format(time.RFC3339), "time_to": fixedNow.Add(-time.Hour).Format(time.RFC3339)})
	env.assertError(t, rec, http.StatusBadRequest, codeAuditQueryInvalid)
}
