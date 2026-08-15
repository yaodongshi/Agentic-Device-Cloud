package adminapi

import (
	"net/http"
	"testing"
)

func TestCreateTenant(t *testing.T) {
	env := newTestEnv(t)
	tok := env.token(t, []string{rolePlatformAdmin}, "")
	rec := env.do(http.MethodPost, "/v1/admin/tenants", tok, map[string]any{
		"name":  "华东精密制造有限公司",
		"code":  "huadong-precision",
		"quota": map[string]any{"max_devices": 1000},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d; body: %s", rec.Code, rec.Body.String())
	}
	var out tenantResponse
	env.decode(t, rec, &out)
	if out.TenantID == "" || !isValidUUID(out.TenantID) {
		t.Fatalf("tenant_id = %q", out.TenantID)
	}
	if out.Name != "华东精密制造有限公司" || out.Code != "huadong-precision" || out.Status != tenantStatusActive {
		t.Fatalf("unexpected tenant: %+v", out)
	}
	if out.Quota.MaxDevices != 1000 {
		t.Fatalf("max_devices = %d, want 1000", out.Quota.MaxDevices)
	}
	// fields not provided take the defaults.
	if out.Quota.MaxAgentKeys != 20 || out.Quota.MonthlyCallLimit != 100000 || out.Quota.AuditRetentionDays != 180 {
		t.Fatalf("default quota not applied: %+v", out.Quota)
	}
	// the create action is audited.
	op, ok := env.audit.last()
	if !ok || op.Action != "tenant.create" || op.Target != out.TenantID {
		t.Fatalf("audit = %+v, want tenant.create", op)
	}
}

func TestCreateTenantDuplicateCode(t *testing.T) {
	env := newTestEnv(t)
	env.createTenant(t, "acme", "Acme", nil)
	tok := env.token(t, []string{rolePlatformAdmin}, "")
	rec := env.do(http.MethodPost, "/v1/admin/tenants", tok, map[string]any{
		"name": "Acme 2",
		"code": "acme",
	})
	env.assertError(t, rec, http.StatusConflict, codeTenantCodeExists)
}

func TestCreateTenantInvalidCode(t *testing.T) {
	env := newTestEnv(t)
	tok := env.token(t, []string{rolePlatformAdmin}, "")
	for _, code := range []string{"", "BadCode", "has space", "a"} {
		rec := env.do(http.MethodPost, "/v1/admin/tenants", tok, map[string]any{"name": "x", "code": code})
		env.assertError(t, rec, http.StatusBadRequest, codeBadRequest)
	}
}

func TestCreateTenantRequiresPlatformAdmin(t *testing.T) {
	env := newTestEnv(t)
	tr := env.createTenant(t, "acme", "Acme", nil)
	tok := env.token(t, []string{"tenant_admin"}, tr.TenantID)
	rec := env.do(http.MethodPost, "/v1/admin/tenants", tok, map[string]any{"name": "x", "code": "y"})
	env.assertError(t, rec, http.StatusForbidden, codeForbidden)
}

func TestCreateTenantInvalidQuota(t *testing.T) {
	env := newTestEnv(t)
	tok := env.token(t, []string{rolePlatformAdmin}, "")
	rec := env.do(http.MethodPost, "/v1/admin/tenants", tok, map[string]any{
		"name":  "x",
		"code":  "acme",
		"quota": map[string]any{"max_devices": 10001},
	})
	env.assertError(t, rec, http.StatusBadRequest, codeBadRequest)
}

func TestListTenants(t *testing.T) {
	env := newTestEnv(t)
	a := env.createTenant(t, "acme-a", "Acme A", nil)
	b := env.createTenant(t, "acme-b", "Beta", nil)
	// device/key counts feed the list items.
	ta := env.token(t, []string{"tenant_admin"}, a.TenantID)
	if rec := env.do(http.MethodPost, "/v1/admin/devices", ta, map[string]any{
		"device_code": "dev-1", "name": "D1", "device_type": "cnc", "auth_type": "token",
	}); rec.Code != http.StatusCreated {
		t.Fatalf("register device: %d; body: %s", rec.Code, rec.Body.String())
	}
	if rec := env.do(http.MethodPost, "/v1/admin/agent-keys", ta, map[string]any{"name": "agent"}); rec.Code != http.StatusCreated {
		t.Fatalf("issue key: %d; body: %s", rec.Code, rec.Body.String())
	}

	tok := env.token(t, []string{rolePlatformAdmin}, "")
	rec := env.do(http.MethodGet, "/v1/admin/tenants?page=1&page_size=20", tok, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body: %s", rec.Code, rec.Body.String())
	}
	var out tenantListResponse
	env.decode(t, rec, &out)
	if out.Total != 2 || len(out.Items) != 2 || out.Page != 1 || out.PageSize != 20 {
		t.Fatalf("unexpected list: %+v", out)
	}
	// newest first (both share the fixed clock; tie breaks by id desc? by id asc).
	if out.Items[0].TenantID != a.TenantID || out.Items[1].TenantID != b.TenantID {
		t.Fatalf("unexpected order: %+v", out.Items)
	}
	if out.Items[0].DeviceCount != 1 || out.Items[0].AgentKeyCount != 1 {
		t.Fatalf("counts not populated: %+v", out.Items[0])
	}
	// keyword and status filters.
	rec = env.do(http.MethodGet, "/v1/admin/tenants?keyword=beta", tok, nil)
	var filtered tenantListResponse
	env.decode(t, rec, &filtered)
	if filtered.Total != 1 || filtered.Items[0].TenantID != b.TenantID {
		t.Fatalf("keyword filter: %+v", filtered)
	}
	rec = env.do(http.MethodGet, "/v1/admin/tenants?page=1&page_size=1", tok, nil)
	var paged tenantListResponse
	env.decode(t, rec, &paged)
	if paged.Total != 2 || len(paged.Items) != 1 {
		t.Fatalf("pagination: total=%d items=%d", paged.Total, len(paged.Items))
	}
}

func TestListTenantsTenantAdminSeesOwnRowOnly(t *testing.T) {
	env := newTestEnv(t)
	a := env.createTenant(t, "acme-a", "Acme A", nil)
	env.createTenant(t, "acme-b", "Acme B", nil)
	tok := env.token(t, []string{"tenant_admin"}, a.TenantID)
	rec := env.do(http.MethodGet, "/v1/admin/tenants", tok, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body: %s", rec.Code, rec.Body.String())
	}
	var out tenantListResponse
	env.decode(t, rec, &out)
	if out.Total != 1 || out.Items[0].TenantID != a.TenantID {
		t.Fatalf("tenant_admin must see only its own row: %+v", out)
	}
}

func TestListTenantsInvalidPage(t *testing.T) {
	env := newTestEnv(t)
	tok := env.token(t, []string{rolePlatformAdmin}, "")
	rec := env.do(http.MethodGet, "/v1/admin/tenants?page=0", tok, nil)
	env.assertError(t, rec, http.StatusBadRequest, codeBadRequest)
	rec = env.do(http.MethodGet, "/v1/admin/tenants?page_size=201", tok, nil)
	env.assertError(t, rec, http.StatusBadRequest, codeBadRequest)
}

func TestPatchTenantQuota(t *testing.T) {
	env := newTestEnv(t)
	tr := env.createTenant(t, "acme", "Acme", nil)
	tok := env.token(t, []string{rolePlatformAdmin}, "")
	rec := env.do(http.MethodPatch, "/v1/admin/tenants/"+tr.TenantID, tok, map[string]any{
		"quota":         map[string]any{"max_devices": 2000},
		"change_reason": "扩容",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body: %s", rec.Code, rec.Body.String())
	}
	var out tenantResponse
	env.decode(t, rec, &out)
	if out.Quota.MaxDevices != 2000 {
		t.Fatalf("max_devices = %d, want 2000", out.Quota.MaxDevices)
	}
	if out.Quota.MonthlyCallLimit != 100000 {
		t.Fatalf("partial quota update must keep untouched fields: %+v", out.Quota)
	}
	op, ok := env.audit.last()
	if !ok || op.Action != "tenant.update" || op.Reason != "扩容" {
		t.Fatalf("audit = %+v", op)
	}
}

func TestPatchTenantSuspend(t *testing.T) {
	env := newTestEnv(t)
	tr := env.createTenant(t, "acme", "Acme", nil)
	tok := env.token(t, []string{rolePlatformAdmin}, "")
	rec := env.do(http.MethodPatch, "/v1/admin/tenants/"+tr.TenantID, tok, map[string]any{
		"status":        "suspended",
		"change_reason": "欠费停用",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body: %s", rec.Code, rec.Body.String())
	}
	var out tenantResponse
	env.decode(t, rec, &out)
	if out.Status != tenantStatusSuspended {
		t.Fatalf("status = %q, want suspended", out.Status)
	}
	// suspended tenants refuse new devices and keys.
	ta := env.token(t, []string{"tenant_admin"}, tr.TenantID)
	rec = env.do(http.MethodPost, "/v1/admin/devices", ta, map[string]any{
		"device_code": "dev-1", "name": "D1", "device_type": "cnc", "auth_type": "token",
	})
	env.assertError(t, rec, http.StatusForbidden, codeTenantSuspended)
}

func TestPatchTenantQuotaBelowUsage(t *testing.T) {
	env := newTestEnv(t)
	q := &quotaRequest{MaxDevices: intPtr(2)}
	tr := env.createTenant(t, "acme", "Acme", q)
	ta := env.token(t, []string{"tenant_admin"}, tr.TenantID)
	for _, code := range []string{"dev-1", "dev-2"} {
		rec := env.do(http.MethodPost, "/v1/admin/devices", ta, map[string]any{
			"device_code": code, "name": code, "device_type": "cnc", "auth_type": "token",
		})
		if rec.Code != http.StatusCreated {
			t.Fatalf("register %s: %d; body: %s", code, rec.Code, rec.Body.String())
		}
	}
	tok := env.token(t, []string{rolePlatformAdmin}, "")
	rec := env.do(http.MethodPatch, "/v1/admin/tenants/"+tr.TenantID, tok, map[string]any{
		"quota":         map[string]any{"max_devices": 1},
		"change_reason": "降配额",
	})
	env.assertError(t, rec, http.StatusForbidden, codeTenantQuota)
}

func TestPatchTenantValidation(t *testing.T) {
	env := newTestEnv(t)
	tr := env.createTenant(t, "acme", "Acme", nil)
	tok := env.token(t, []string{rolePlatformAdmin}, "")

	// change_reason is mandatory.
	rec := env.do(http.MethodPatch, "/v1/admin/tenants/"+tr.TenantID, tok, map[string]any{
		"quota": map[string]any{"max_devices": 2000},
	})
	env.assertError(t, rec, http.StatusBadRequest, codeBadRequest)

	// status must be active or suspended.
	rec = env.do(http.MethodPatch, "/v1/admin/tenants/"+tr.TenantID, tok, map[string]any{
		"status": "disabled", "change_reason": "x",
	})
	env.assertError(t, rec, http.StatusBadRequest, codeBadRequest)

	// unknown tenant.
	rec = env.do(http.MethodPatch, "/v1/admin/tenants/"+uuidOf(999), tok, map[string]any{
		"status": "active", "change_reason": "x",
	})
	env.assertError(t, rec, http.StatusNotFound, codeTenantNotFound)

	// malformed uuid.
	rec = env.do(http.MethodPatch, "/v1/admin/tenants/not-a-uuid", tok, map[string]any{
		"status": "active", "change_reason": "x",
	})
	env.assertError(t, rec, http.StatusBadRequest, codeBadRequest)
}

func TestPatchTenantRequiresPlatformAdmin(t *testing.T) {
	env := newTestEnv(t)
	tr := env.createTenant(t, "acme", "Acme", nil)
	tok := env.token(t, []string{"tenant_admin"}, tr.TenantID)
	rec := env.do(http.MethodPatch, "/v1/admin/tenants/"+tr.TenantID, tok, map[string]any{
		"status": "suspended", "change_reason": "x",
	})
	env.assertError(t, rec, http.StatusForbidden, codeForbidden)
}

func TestTenantsUnauthenticated(t *testing.T) {
	env := newTestEnv(t)
	rec := env.do(http.MethodGet, "/v1/admin/tenants", "", nil)
	env.assertError(t, rec, http.StatusUnauthorized, codeUnauthorized)
}

func TestTenantsApproverForbidden(t *testing.T) {
	env := newTestEnv(t)
	tok := env.token(t, []string{"approver"}, uuidOf(1))
	rec := env.do(http.MethodGet, "/v1/admin/tenants", tok, nil)
	env.assertError(t, rec, http.StatusForbidden, codeForbidden)
}

func intPtr(v int) *int { return &v }
