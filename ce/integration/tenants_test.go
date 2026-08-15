// Contract tests for the tenant CRUD endpoints (design/33 3.1.2-3.1.5,
// FR-008): platform-admin exclusive create/update, quota validation,
// code uniqueness, tenant-admin self-listing and the RBAC rejection of
// role escalation. All writes hit the real PostgreSQL ledger (schema-level
// isolation is enforced by the tenant_id predicates in the repositories).
package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"
)

// testTenantCode derives a unique tenant code per test (design/40 3.3
// fictional codes; global uniqueness of adc_tenants.code is a hard
// contract, so retries of the same test must not collide).
func testTenantCode(t *testing.T, prefix string) string {
	t.Helper()
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}

// TestCreateTenantPlatformAdmin exercises POST /v1/admin/tenants with the
// full quota body: 201, echoed quota, and the row landing in PG.
func TestCreateTenantPlatformAdmin(t *testing.T) {
	e := envOrSkip(t)
	token := e.loginOK(t, fixturePlatformUser, fixturePlatformPass)
	code := testTenantCode(t, "it-tenant")

	rec := e.do(http.MethodPost, "/v1/admin/tenants", map[string]any{
		"name": "Integration Tenant",
		"code": code,
		"quota": map[string]any{
			"max_devices":          500,
			"max_agent_keys":       15,
			"monthly_call_limit":   750000,
			"audit_retention_days": 180,
		},
	}, authHdr(token))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%q)", rec.Code, rec.Body.String())
	}
	var resp struct {
		TenantID string `json:"tenant_id"`
		Code     string `json:"code"`
		Status   string `json:"status"`
		Quota    struct {
			MaxDevices         int   `json:"max_devices"`
			MaxAgentKeys       int   `json:"max_agent_keys"`
			MonthlyCallLimit   int64 `json:"monthly_call_limit"`
			AuditRetentionDays int   `json:"audit_retention_days"`
		} `json:"quota"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode tenant: %v", err)
	}
	if resp.TenantID == "" || resp.Code != code || resp.Status != "active" {
		t.Fatalf("tenant response = %+v", resp)
	}
	if resp.Quota.MaxDevices != 500 || resp.Quota.MaxAgentKeys != 15 ||
		resp.Quota.MonthlyCallLimit != 750000 || resp.Quota.AuditRetentionDays != 180 {
		t.Fatalf("quota echo = %+v", resp.Quota)
	}

	// Ledger assertion: the row exists in adc_tenants with the quota.
	var dbStatus string
	var quotaDevices int
	err := e.pool.QueryRow(context.Background(),
		`SELECT status, quota_devices FROM adc_tenants WHERE code = $1`, code).
		Scan(&dbStatus, &quotaDevices)
	if err != nil {
		t.Fatalf("tenant not persisted: %v", err)
	}
	if dbStatus != "ACTIVE" || quotaDevices != 500 {
		t.Fatalf("ledger row = status %q quota %d", dbStatus, quotaDevices)
	}
}

// TestCreateTenantDuplicateCode answers 409 code 13004 (design/33 3.1.3)
// and the unique partial index uq_tenants_code is the enforcement point.
func TestCreateTenantDuplicateCode(t *testing.T) {
	e := envOrSkip(t)
	token := e.loginOK(t, fixturePlatformUser, fixturePlatformPass)
	code := testTenantCode(t, "it-dup")
	body := map[string]any{"name": "Dup Tenant", "code": code}

	if rec := e.do(http.MethodPost, "/v1/admin/tenants", body, authHdr(token)); rec.Code != http.StatusCreated {
		t.Fatalf("first create = %d (body=%q)", rec.Code, rec.Body.String())
	}
	rec := e.do(http.MethodPost, "/v1/admin/tenants", body, authHdr(token))
	wantErr(t, rec, http.StatusConflict, "13004")
}

// TestCreateTenantValidation rejects malformed codes and quotas with 400
// code 10001 (SEC-20 charset whitelist, design/33 3.1.3).
func TestCreateTenantValidation(t *testing.T) {
	e := envOrSkip(t)
	token := e.loginOK(t, fixturePlatformUser, fixturePlatformPass)
	cases := []struct {
		name string
		body map[string]any
	}{
		{"code too short", map[string]any{"name": "X", "code": "x"}},
		{"code uppercase", map[string]any{"name": "X", "code": "BadCode"}},
		{"code with underscore", map[string]any{"name": "X", "code": "bad_code"}},
		{"empty name", map[string]any{"name": "", "code": testTenantCode(t, "it-val")}},
		{"negative devices", map[string]any{"name": "X", "code": testTenantCode(t, "it-val"),
			"quota": map[string]any{"max_devices": -1}}},
		{"devices above hard cap", map[string]any{"name": "X", "code": testTenantCode(t, "it-val"),
			"quota": map[string]any{"max_devices": 10001}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := e.do(http.MethodPost, "/v1/admin/tenants", c.body, authHdr(token))
			wantErr(t, rec, http.StatusBadRequest, "10001")
		})
	}
}

// TestTenantAdminCannotCreateTenant: tenant creation is platform_admin
// exclusive; a tenant_admin gets 403 code 10003 (design/33 1.2).
func TestTenantAdminCannotCreateTenant(t *testing.T) {
	e := envOrSkip(t)
	token := e.loginOK(t, fixtureAlphaAdmin, fixtureAlphaPass)
	rec := e.do(http.MethodPost, "/v1/admin/tenants",
		map[string]any{"name": "Escalation Attempt", "code": testTenantCode(t, "it-esc")},
		authHdr(token))
	wantErr(t, rec, http.StatusForbidden, "10003")
}

// TestTenantListRoleScoping: a platform admin sees the full list while a
// tenant_admin is locked to its own tenant row (design/33 3.1.2 note).
func TestTenantListRoleScoping(t *testing.T) {
	e := envOrSkip(t)

	platform := e.loginOK(t, fixturePlatformUser, fixturePlatformPass)
	rec := e.do(http.MethodGet, "/v1/admin/tenants", nil, authHdr(platform))
	if rec.Code != http.StatusOK {
		t.Fatalf("platform list = %d (body=%q)", rec.Code, rec.Body.String())
	}
	var list struct {
		Items []struct {
			TenantID string `json:"tenant_id"`
		} `json:"items"`
		Total int `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if list.Total < 3 {
		t.Fatalf("platform total = %d, want at least the 3 fixture tenants", list.Total)
	}

	alpha := e.loginOK(t, fixtureAlphaAdmin, fixtureAlphaPass)
	rec = e.do(http.MethodGet, "/v1/admin/tenants", nil, authHdr(alpha))
	if rec.Code != http.StatusOK {
		t.Fatalf("tenant-admin list = %d (body=%q)", rec.Code, rec.Body.String())
	}
	list = struct {
		Items []struct {
			TenantID string `json:"tenant_id"`
		} `json:"items"`
		Total int `json:"total"`
	}{}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if list.Total != 1 || len(list.Items) != 1 {
		t.Fatalf("tenant-admin sees total=%d items=%d, want exactly its own row", list.Total, len(list.Items))
	}
	if list.Items[0].TenantID != e.tenantID(t, fixtureAlphaTenant) {
		t.Fatal("tenant-admin row is not its own tenant")
	}
}

// TestPatchTenantQuotaAndStatus: PATCH updates quota and status with the
// mandatory change_reason (design/33 3.1.5); a missing reason is 400 and
// dropping quota below usage is 403 code 13003.
func TestPatchTenantQuotaAndStatus(t *testing.T) {
	e := envOrSkip(t)
	token := e.loginOK(t, fixturePlatformUser, fixturePlatformPass)
	code := testTenantCode(t, "it-patch")
	rec := e.do(http.MethodPost, "/v1/admin/tenants",
		map[string]any{"name": "Patch Tenant", "code": code}, authHdr(token))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d (body=%q)", rec.Code, rec.Body.String())
	}
	var created struct {
		TenantID string `json:"tenant_id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)

	// Missing change_reason -> 400.
	rec = e.do(http.MethodPatch, "/v1/admin/tenants/"+created.TenantID,
		map[string]any{"quota": map[string]any{"max_devices": 2000}}, authHdr(token))
	wantErr(t, rec, http.StatusBadRequest, "10001")

	// Valid partial update -> 200 with the new quota.
	rec = e.do(http.MethodPatch, "/v1/admin/tenants/"+created.TenantID, map[string]any{
		"quota":         map[string]any{"max_devices": 2000},
		"change_reason": "capacity expansion",
	}, authHdr(token))
	if rec.Code != http.StatusOK {
		t.Fatalf("patch = %d (body=%q)", rec.Code, rec.Body.String())
	}
	var updated struct {
		Quota struct {
			MaxDevices int `json:"max_devices"`
		} `json:"quota"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &updated)
	if updated.Quota.MaxDevices != 2000 {
		t.Fatalf("quota after patch = %d, want 2000", updated.Quota.MaxDevices)
	}

	// Suspend -> status suspended; then a device registration must be
	// refused with 13002 (suspended tenant, design/33 3.1.6).
	rec = e.do(http.MethodPatch, "/v1/admin/tenants/"+created.TenantID, map[string]any{
		"status":        "suspended",
		"change_reason": "billing arrears",
	}, authHdr(token))
	if rec.Code != http.StatusOK {
		t.Fatalf("suspend = %d (body=%q)", rec.Code, rec.Body.String())
	}
	rec = e.do(http.MethodPost, "/v1/admin/devices?tenant_id="+created.TenantID, map[string]any{
		"device_code": testTenantCode(t, "it-dev"),
		"name":        "Frozen Out",
		"device_type": "cnc",
		"auth_type":   "hmac",
	}, authHdr(token))
	wantErr(t, rec, http.StatusForbidden, "13002")

	// Re-activate for hygiene.
	rec = e.do(http.MethodPatch, "/v1/admin/tenants/"+created.TenantID, map[string]any{
		"status":        "active",
		"change_reason": "billing settled",
	}, authHdr(token))
	if rec.Code != http.StatusOK {
		t.Fatalf("reactivate = %d (body=%q)", rec.Code, rec.Body.String())
	}
}

// TestPatchTenantQuotaBelowUsage: after one device exists, dropping
// max_devices to 0 is refused with 403 code 13003 (design/33 3.1.5).
func TestPatchTenantQuotaBelowUsage(t *testing.T) {
	e := envOrSkip(t)
	token := e.loginOK(t, fixturePlatformUser, fixturePlatformPass)
	code := testTenantCode(t, "it-quota")
	rec := e.do(http.MethodPost, "/v1/admin/tenants",
		map[string]any{"name": "Quota Tenant", "code": code}, authHdr(token))
	var created struct {
		TenantID string `json:"tenant_id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)

	rec = e.do(http.MethodPost, "/v1/admin/devices?tenant_id="+created.TenantID, map[string]any{
		"device_code": testTenantCode(t, "it-dev"),
		"name":        "One Device",
		"device_type": "plc",
		"auth_type":   "token",
	}, authHdr(token))
	if rec.Code != http.StatusCreated {
		t.Fatalf("device create = %d (body=%q)", rec.Code, rec.Body.String())
	}

	rec = e.do(http.MethodPatch, "/v1/admin/tenants/"+created.TenantID, map[string]any{
		"quota":         map[string]any{"max_devices": 0},
		"change_reason": "attempt to undercut usage",
	}, authHdr(token))
	wantErr(t, rec, http.StatusForbidden, "13003")
}

// TestPatchTenantByTenantAdminForbidden: PATCH is platform_admin exclusive;
// a tenant_admin gets 403 code 10003 even for its own tenant.
func TestPatchTenantByTenantAdminForbidden(t *testing.T) {
	e := envOrSkip(t)
	token := e.loginOK(t, fixtureAlphaAdmin, fixtureAlphaPass)
	rec := e.do(http.MethodPatch, "/v1/admin/tenants/"+e.tenantID(t, fixtureAlphaTenant), map[string]any{
		"status":        "suspended",
		"change_reason": "self-service attempt",
	}, authHdr(token))
	wantErr(t, rec, http.StatusForbidden, "10003")
}
