// Contract tests for the audit surface (design/33 3.1.16, FR-013,
// SEC-07): every executed tool call becomes a queryable adc_audit_logs
// row (AUD-001/002), the GET /v1/admin/audit-logs endpoint honors the
// filter set and the keyset cursor (design/33 1.6), invalid conditions
// answer 14001, the CSV export stream works, and no mutation route exists
// for the append-only store (FR-013 acceptance).
package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// runDirectCall executes one low-risk tool call end to end (device WSS +
// agent API) and returns the tool name it ran, so the audit assertions
// can pin the exact rows (SEC-07 / AUD-001: call -> row correlation).
func runDirectCall(t *testing.T, e *testEnv, tenantID string, token string) (deviceCode, toolName, requestID string) {
	t.Helper()
	deviceCode = testTenantCode(t, "it-aud")
	toolName = "get_spindle_status"

	status, raw := e.registerDevice(t, token, tenantID, deviceCode, "Audit Lathe")
	if status != http.StatusCreated {
		t.Fatalf("register = %d (body=%q)", status, raw)
	}
	deviceID, secret := decodeDeviceRegister(t, raw)
	// Seed the low-risk tool row (risk 0 -> no HITL, design/33 3.2.2).
	if _, err := e.pool.Exec(context.Background(), `INSERT INTO adc_device_tools
		(tenant_id, device_id, tool_name, description, input_schema, risk_level, is_enabled)
		VALUES ($1::uuid, $2::uuid, $3, 'Read spindle', '{"type":"object"}', 0, TRUE)`,
		tenantID, deviceID, toolName); err != nil {
		t.Fatalf("seed tool: %v", err)
	}

	dev := e.dialMockDevice(t, tenantID, deviceCode, secret, []mockTool{
		{name: toolName, description: "Read spindle", schema: `{"type":"object"}`},
	})
	defer dev.close()

	_, agentKey := e.issueKey(t, token, tenantID)
	rec := e.do(http.MethodPost, "/v1/agent/mcp/tools/call", map[string]any{
		"name":      deviceCode + "__" + toolName,
		"arguments": map[string]any{"rpm": 1000},
	}, map[string]string{"X-ADC-Key": agentKey})
	if rec.Code != http.StatusOK {
		t.Fatalf("direct call = %d (body=%q)", rec.Code, rec.Body.String())
	}
	var out struct {
		RequestID string `json:"request_id"`
		Status    string `json:"status"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode call: %v", err)
	}
	return deviceCode, toolName, out.RequestID
}

// waitAuditRows polls adc_audit_logs until rows matching the predicate
// exist (the audit pipeline is asynchronous: Valkey queue -> worker,
// SEC-07 at-least-once).
func (e *testEnv) waitAuditRows(t *testing.T, wantStatus, toolName string, atLeast int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM adc_audit_logs
			WHERE status = $1 AND tool_name = $2`, wantStatus, toolName).Scan(&n)
		if err == nil && n >= atLeast {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("audit rows (status=%s tool=%s) did not appear within 10s", wantStatus, toolName)
}

// TestAuditCallChainQueryable: a direct call produces audit rows and the
// admin endpoint serves them with the full field set (AUD-001/002).
func TestAuditCallChainQueryable(t *testing.T) {
	e := envOrSkip(t)
	token := e.loginOK(t, fixturePlatformUser, fixturePlatformPass)
	tenantID := e.tenantID(t, fixtureAlphaTenant)
	deviceCode, toolName, _ := runDirectCall(t, e, tenantID, token)

	e.waitAuditRows(t, "success", toolName, 1)

	rec := e.do(http.MethodGet,
		"/v1/admin/audit-logs?tenant_id="+tenantID+"&tool_name="+toolName+"&status=success",
		nil, authHdr(token))
	if rec.Code != http.StatusOK {
		t.Fatalf("audit query = %d (body=%q)", rec.Code, rec.Body.String())
	}
	var page struct {
		Items []struct {
			LogID       string         `json:"log_id"`
			EventType   string         `json:"event_type"`
			ActorType   string         `json:"actor_type"`
			ActorID     string         `json:"actor_id,omitempty"`
			DeviceCode  string         `json:"device_code,omitempty"`
			ToolName    string         `json:"tool_name,omitempty"`
			Status      string         `json:"status"`
			RequestID   string         `json:"request_id"`
			RequestParm map[string]any `json:"request_params,omitempty"`
			CreatedAt   string         `json:"created_at"`
		} `json:"items"`
		NextCursor *string `json:"next_cursor"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode audit page: %v", err)
	}
	if len(page.Items) < 1 {
		t.Fatalf("no audit items returned: %q", rec.Body.String())
	}
	item := page.Items[0]
	if item.EventType != "tool_call" || item.ActorType != "agent" ||
		item.ActorID != "agent-prod-worker" || item.DeviceCode != deviceCode ||
		item.ToolName != toolName || item.Status != "success" {
		t.Fatalf("audit item incomplete: %+v", item)
	}
	if item.RequestID == "" || item.LogID == "" || item.CreatedAt == "" {
		t.Fatalf("audit item missing correlator fields: %+v", item)
	}
}

// TestAuditQueryValidation: malformed conditions answer 400 code 14001
// (design/33 3.1.16): inverted time range, bad status, bad cursor.
func TestAuditQueryValidation(t *testing.T) {
	e := envOrSkip(t)
	token := e.loginOK(t, fixtureAlphaAdmin, fixtureAlphaPass)

	cases := []struct {
		name  string
		query string
	}{
		{"time_from after time_to", "?time_from=2026-08-15T00:00:00Z&time_to=2026-08-14T00:00:00Z"},
		{"bad time_from", "?time_from=yesterday"},
		{"bad status", "?status=maybe"},
		{"bad event_type", "?event_type=explosion"},
		{"bad limit", "?limit=0"},
		{"limit over cap", "?limit=501"},
		{"bad cursor", "?cursor=!!!not-base64!!!"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := e.do(http.MethodGet, "/v1/admin/audit-logs"+c.query, nil, authHdr(token))
			wantErr(t, rec, http.StatusBadRequest, "14001")
		})
	}
}

// TestAuditCursorPagination: keyset pagination walks synthetic rows in
// created_at DESC, id DESC order without duplicates (design/33 1.6).
func TestAuditCursorPagination(t *testing.T) {
	e := envOrSkip(t)
	token := e.loginOK(t, fixtureAlphaAdmin, fixtureAlphaPass)
	tenantID := e.tenantID(t, fixtureAlphaTenant)
	marker := testTenantCode(t, "it-cursor")

	// Synthetic data construction (design/40 3.3): five rows with
	// distinct timestamps so the cursor order is deterministic.
	for i := 0; i < 5; i++ {
		reqID := newUUID()
		_, err := e.pool.Exec(context.Background(), `
			INSERT INTO adc_audit_logs
				(id, tenant_id, event_type, actor_type, actor_id, tool_name,
				 risk_level, status, request_id, created_at)
			VALUES (gen_random_uuid(), $1::uuid, 'tool_call', 'agent', $2, $3, 0, 'success', $4::uuid, $5)`,
			tenantID, marker, "cursor_tool_"+fmt.Sprintf("%d", i), reqID,
			time.Now().Add(-time.Duration(i)*time.Minute))
		if err != nil {
			t.Fatalf("insert synthetic audit row: %v", err)
		}
	}

	seen := map[string]bool{}
	nextCursor := ""
	for {
		q := "/v1/admin/audit-logs?limit=2&keyword=" + marker
		if nextCursor != "" {
			q += "&cursor=" + nextCursor
		}
		rec := e.do(http.MethodGet, q, nil, authHdr(token))
		if rec.Code != http.StatusOK {
			t.Fatalf("audit page = %d (body=%q)", rec.Code, rec.Body.String())
		}
		var page struct {
			Items []struct {
				LogID string `json:"log_id"`
			} `json:"items"`
			NextCursor *string `json:"next_cursor"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
			t.Fatalf("decode page: %v", err)
		}
		if len(page.Items) == 0 {
			t.Fatal("empty page before the cursor reached the end")
		}
		for _, it := range page.Items {
			if seen[it.LogID] {
				t.Fatalf("log %s returned twice across pages", it.LogID)
			}
			seen[it.LogID] = true
		}
		if page.NextCursor == nil {
			break
		}
		nextCursor = *page.NextCursor
	}
	if len(seen) != 5 {
		t.Fatalf("pagination walked %d rows, want 5", len(seen))
	}
}

// TestAuditCSVExport: POST /v1/admin/audit-logs/export streams CSV with
// the documented header and one line per row (FR-013, AUD-006).
func TestAuditCSVExport(t *testing.T) {
	e := envOrSkip(t)
	token := e.loginOK(t, fixturePlatformUser, fixturePlatformPass)
	tenantID := e.tenantID(t, fixtureAlphaTenant)
	_, toolName, _ := runDirectCall(t, e, tenantID, token)
	e.waitAuditRows(t, "success", toolName, 1)

	now := time.Now().UTC()
	rec := e.do(http.MethodPost, "/v1/admin/audit-logs/export?tenant_id="+tenantID, map[string]any{
		"time_from": now.Add(-time.Hour).Format(time.RFC3339),
		"time_to":   now.Add(time.Hour).Format(time.RFC3339),
		"tool_name": toolName,
	}, authHdr(token))
	if rec.Code != http.StatusOK {
		t.Fatalf("export = %d (body=%q)", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/csv") {
		t.Fatalf("export content-type = %q", ct)
	}
	lines := strings.Split(strings.TrimRight(rec.Body.String(), "\n"), "\n")
	if len(lines) < 2 {
		t.Fatalf("export has %d lines, want header + rows", len(lines))
	}
	if !strings.HasPrefix(lines[0], "log_id,tenant_id,event_type") {
		t.Fatalf("export header = %q", lines[0])
	}
	found := false
	for _, line := range lines[1:] {
		if strings.Contains(line, toolName) {
			found = true
		}
	}
	if !found {
		t.Fatal("export misses the executed tool row")
	}

	// The time range is mandatory for export (design/80 B-06).
	rec = e.do(http.MethodPost, "/v1/admin/audit-logs/export?tenant_id="+tenantID, map[string]any{}, authHdr(token))
	wantErr(t, rec, http.StatusBadRequest, "14001")
}

// TestAuditAppendOnlySurface: no mutation route exists on the audit
// collection (FR-013 acceptance: append-only; attempts get 405).
func TestAuditAppendOnlySurface(t *testing.T) {
	e := envOrSkip(t)
	token := e.loginOK(t, fixturePlatformUser, fixturePlatformPass)
	for _, method := range []string{http.MethodPut, http.MethodPatch, http.MethodDelete} {
		rec := e.do(method, "/v1/admin/audit-logs", map[string]any{}, authHdr(token))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s audit-logs = %d, want 405", method, rec.Code)
		}
	}
}

// TestAuditRoleScoping: the auditor role may read the audit surface, the
// approver role is outside the audit matrix (403 code 10003), and a
// tenant_admin pinned to alpha querying beta's logs is refused with 13007
// (design/33 1.2).
func TestAuditRoleScoping(t *testing.T) {
	e := envOrSkip(t)

	// approver role: audit-logs is NOT in its matrix -> 403 code 10003.
	approver := e.loginOK(t, fixtureAlphaApprover, fixtureAlphaPass)
	rec := e.do(http.MethodGet, "/v1/admin/audit-logs", nil, authHdr(approver))
	wantErr(t, rec, http.StatusForbidden, "10003")

	// auditor role: allowed read on its own tenant.
	auditor := e.loginOK(t, fixtureAlphaAuditor, fixtureAlphaPass)
	rec = e.do(http.MethodGet, "/v1/admin/audit-logs", nil, authHdr(auditor))
	if rec.Code != http.StatusOK {
		t.Fatalf("auditor read = %d (body=%q)", rec.Code, rec.Body.String())
	}

	// tenant_admin pinned to alpha querying beta's logs -> 403 13007.
	alpha := e.loginOK(t, fixtureAlphaAdmin, fixtureAlphaPass)
	rec = e.do(http.MethodGet, "/v1/admin/audit-logs?tenant_id="+e.tenantID(t, fixtureBetaTenant),
		nil, authHdr(alpha))
	wantErr(t, rec, http.StatusForbidden, "13007")
}
