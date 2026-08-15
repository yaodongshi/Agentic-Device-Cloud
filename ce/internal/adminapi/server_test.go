package adminapi

import (
	"net/http"
	"testing"
	"time"
)

// TestRouteMatrix verifies the assembled middleware chain: every endpoint
// group requires a session and the RBAC matrix (design/33 1.2).
func TestRouteMatrix(t *testing.T) {
	env := newTestEnv(t)
	tr := env.createTenant(t, "acme", "Acme", nil)
	roles := []struct {
		name   string
		roles  []string
		method string
		path   string
		status int
	}{
		{"tenant_admin lists tenants", []string{"tenant_admin"}, http.MethodGet, "/v1/admin/tenants", http.StatusOK},
		{"tenant_admin creates tenants denied", []string{"tenant_admin"}, http.MethodPost, "/v1/admin/tenants", http.StatusForbidden},
		{"approver lists devices", []string{"approver"}, http.MethodGet, "/v1/admin/devices", http.StatusOK},
		{"approver registers devices denied", []string{"approver"}, http.MethodPost, "/v1/admin/devices", http.StatusForbidden},
		{"approver keys denied", []string{"approver"}, http.MethodGet, "/v1/admin/agent-keys", http.StatusForbidden},
		{"auditor tenants denied", []string{"auditor"}, http.MethodGet, "/v1/admin/tenants", http.StatusForbidden},
		{"auditor devices denied", []string{"auditor"}, http.MethodGet, "/v1/admin/devices", http.StatusForbidden},
		{"auditor keys denied", []string{"auditor"}, http.MethodGet, "/v1/admin/agent-keys", http.StatusForbidden},
		{"no session tenants", nil, http.MethodGet, "/v1/admin/tenants", http.StatusUnauthorized},
		{"no session devices", nil, http.MethodGet, "/v1/admin/devices", http.StatusUnauthorized},
		{"no session keys", nil, http.MethodGet, "/v1/admin/agent-keys", http.StatusUnauthorized},
		{"no session patch", nil, http.MethodPatch, "/v1/admin/devices/" + uuidOf(1), http.StatusUnauthorized},
		{"unknown role denied", []string{"superuser"}, http.MethodGet, "/v1/admin/tenants", http.StatusForbidden},
	}
	for _, tt := range roles {
		t.Run(tt.name, func(t *testing.T) {
			tok := ""
			if tt.roles != nil {
				tok = env.token(t, tt.roles, tr.TenantID)
			}
			rec := env.do(tt.method, tt.path, tok, nil)
			if rec.Code != tt.status {
				t.Fatalf("status = %d, want %d; body: %s", rec.Code, tt.status, rec.Body.String())
			}
		})
	}
}

// TestUnknownRoute serves 404 through the mux default handler.
func TestUnknownRoute(t *testing.T) {
	env := newTestEnv(t)
	tok := env.token(t, []string{rolePlatformAdmin}, "")
	rec := env.do(http.MethodGet, "/v1/admin/nope", tok, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// TestPlatformAdminCrossTenantDeviceOps verifies the platform_admin
// tenant_id query parameter path (design/33 1.2).
func TestPlatformAdminCrossTenantDeviceOps(t *testing.T) {
	env := newTestEnv(t)
	a := env.createTenant(t, "acme-a", "Acme A", nil)
	env.createTenant(t, "acme-b", "Acme B", nil)
	ptok := env.token(t, []string{rolePlatformAdmin}, "")

	rec := env.do(http.MethodPost, "/v1/admin/devices?tenant_id="+a.TenantID, ptok, registerBody("plat-dev", "token"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("platform register: %d; body: %s", rec.Code, rec.Body.String())
	}
	rec = env.do(http.MethodGet, "/v1/admin/devices?tenant_id="+a.TenantID, ptok, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("platform list: %d; body: %s", rec.Code, rec.Body.String())
	}
	rec = env.do(http.MethodGet, "/v1/admin/devices?tenant_id=not-a-uuid", ptok, nil)
	env.assertError(t, rec, http.StatusBadRequest, codeBadRequest)
}

// TestAuditNilSinkIsSafe runs one mutating flow with the audit sink absent.
func TestAuditNilSinkIsSafe(t *testing.T) {
	env := newTestEnv(t)
	srv := NewServer(env.store.tenants, env.store.devices, env.store.keys, env.sessions)
	srv.Now = func() time.Time { return fixedNow }
	tr := env.createTenant(t, "acme", "Acme", nil)
	tok := env.token(t, []string{"tenant_admin"}, tr.TenantID)
	rec := doRaw(srv.Handler(), http.MethodPost, "/v1/admin/devices", tok, registerBody("dev-nil-audit", "token"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d; body: %s", rec.Code, rec.Body.String())
	}
}
