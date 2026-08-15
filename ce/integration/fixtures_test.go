// Fixtures and HTTP test helpers shared by every contract test group.
// Fixtures follow design/40 3.3: fictional codes, no production data;
// seeding is idempotent (WHERE NOT EXISTS / ON CONFLICT) because TestMain
// runs it on a freshly migrated schema exactly once per test binary.
package integration

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"adc.dev/ce/internal/adminauth"
)

// Fixed fixture credentials (design/40 3.3: synthetic accounts only).
const (
	fixturePlatformTenant = "platform-ops"
	fixturePlatformUser   = "itadmin-platform"
	fixturePlatformPass   = "platform-admin-password"

	fixtureAlphaTenant   = "alpha"
	fixtureAlphaAdmin    = "alpha-admin"
	fixtureAlphaPass     = "alpha-admin-password"
	fixtureAlphaApprover = "alpha-approver"
	fixtureAlphaAuditor  = "alpha-auditor"

	fixtureBetaTenant = "beta"
	fixtureBetaAdmin  = "beta-admin"
	fixtureBetaPass   = "beta-admin-password"

	fixtureDisabledUser = "alpha-disabled"
	fixtureDisabledPass = "alpha-disabled-password"
)

// seedFixtures inserts the platform and tenant skeleton: tenants, system
// roles and users with bcrypt-hashed passwords (adminauth.HashPassword).
// Everything is idempotent for repeated runs against the same database.
func (e *testEnv) seedFixtures(ctx context.Context) error {
	platformPass, err := adminauth.HashPassword(fixturePlatformPass)
	if err != nil {
		return err
	}
	alphaPass, err := adminauth.HashPassword(fixtureAlphaPass)
	if err != nil {
		return err
	}
	betaPass, err := adminauth.HashPassword(fixtureBetaPass)
	if err != nil {
		return err
	}
	disabledPass, err := adminauth.HashPassword(fixtureDisabledPass)
	if err != nil {
		return err
	}

	steps := []struct {
		sql  string
		args []any
	}{
		// Tenants (status column stores uppercase per design/32 3.1).
		{`INSERT INTO adc_tenants (code, name, status, quota_devices, quota_calls_monthly)
			SELECT 'platform-ops', 'Platform Operations', 'ACTIVE', 1000, 1000000
			WHERE NOT EXISTS (SELECT 1 FROM adc_tenants WHERE code = 'platform-ops')`, nil},
		{`INSERT INTO adc_tenants (code, name, status, quota_devices, quota_calls_monthly)
			SELECT 'alpha', 'Alpha Factory', 'ACTIVE', 1000, 1000000
			WHERE NOT EXISTS (SELECT 1 FROM adc_tenants WHERE code = 'alpha')`, nil},
		{`INSERT INTO adc_tenants (code, name, status, quota_devices, quota_calls_monthly)
			SELECT 'beta', 'Beta Factory', 'ACTIVE', 1000, 1000000
			WHERE NOT EXISTS (SELECT 1 FROM adc_tenants WHERE code = 'beta')`, nil},
		// System roles (design/33 3.1.18 names).
		{`INSERT INTO adc_roles (tenant_id, role_code, scope, permissions, is_system)
			VALUES (NULL, 'platform_admin', 'PLATFORM', '[]', TRUE)
			ON CONFLICT DO NOTHING`, nil},
		{`INSERT INTO adc_roles (tenant_id, role_code, scope, permissions, is_system)
			SELECT id, 'tenant_admin', 'TENANT', '[]', TRUE FROM adc_tenants
			WHERE code IN ('alpha', 'beta') ON CONFLICT DO NOTHING`, nil},
		{`INSERT INTO adc_roles (tenant_id, role_code, scope, permissions, is_system)
			SELECT id, 'approver', 'TENANT', '[]', TRUE FROM adc_tenants
			WHERE code IN ('alpha', 'beta') ON CONFLICT DO NOTHING`, nil},
		{`INSERT INTO adc_roles (tenant_id, role_code, scope, permissions, is_system)
			SELECT id, 'auditor', 'TENANT', '[]', TRUE FROM adc_tenants
			WHERE code IN ('alpha', 'beta') ON CONFLICT DO NOTHING`, nil},
		// Users.
		{`INSERT INTO adc_users (tenant_id, username, display_name, password_hash, auth_source, status)
			SELECT id, 'itadmin-platform', 'Platform Admin', $1, 'LOCAL', 'ACTIVE'
			FROM adc_tenants WHERE code = 'platform-ops' ON CONFLICT DO NOTHING`, []any{platformPass}},
		{`INSERT INTO adc_users (tenant_id, username, display_name, password_hash, auth_source, status)
			SELECT id, 'alpha-admin', 'Alpha Admin', $1, 'LOCAL', 'ACTIVE'
			FROM adc_tenants WHERE code = 'alpha' ON CONFLICT DO NOTHING`, []any{alphaPass}},
		{`INSERT INTO adc_users (tenant_id, username, display_name, password_hash, auth_source, status)
			SELECT id, 'alpha-approver', 'Alpha Approver', $1, 'LOCAL', 'ACTIVE'
			FROM adc_tenants WHERE code = 'alpha' ON CONFLICT DO NOTHING`, []any{alphaPass}},
		{`INSERT INTO adc_users (tenant_id, username, display_name, password_hash, auth_source, status)
			SELECT id, 'alpha-auditor', 'Alpha Auditor', $1, 'LOCAL', 'ACTIVE'
			FROM adc_tenants WHERE code = 'alpha' ON CONFLICT DO NOTHING`, []any{alphaPass}},
		{`INSERT INTO adc_users (tenant_id, username, display_name, password_hash, auth_source, status)
			SELECT id, 'beta-admin', 'Beta Admin', $1, 'LOCAL', 'ACTIVE'
			FROM adc_tenants WHERE code = 'beta' ON CONFLICT DO NOTHING`, []any{betaPass}},
		{`INSERT INTO adc_users (tenant_id, username, display_name, password_hash, auth_source, status)
			SELECT id, 'alpha-disabled', 'Alpha Disabled', $1, 'LOCAL', 'DISABLED'
			FROM adc_tenants WHERE code = 'alpha' ON CONFLICT DO NOTHING`, []any{disabledPass}},
		// Role grants.
		{`INSERT INTO adc_user_roles (user_id, role_id, tenant_id)
			SELECT u.id, r.id, u.tenant_id FROM adc_users u
			JOIN adc_roles r ON r.role_code = 'platform_admin' AND r.scope = 'PLATFORM'
			WHERE u.username = 'itadmin-platform' ON CONFLICT DO NOTHING`, nil},
		{`INSERT INTO adc_user_roles (user_id, role_id, tenant_id)
			SELECT u.id, r.id, u.tenant_id FROM adc_users u
			JOIN adc_tenants t ON t.id = u.tenant_id
			JOIN adc_roles r ON r.role_code = 'tenant_admin' AND r.tenant_id = u.tenant_id
			WHERE u.username IN ('alpha-admin', 'beta-admin') ON CONFLICT DO NOTHING`, nil},
		{`INSERT INTO adc_user_roles (user_id, role_id, tenant_id)
			SELECT u.id, r.id, u.tenant_id FROM adc_users u
			JOIN adc_roles r ON r.role_code = 'approver' AND r.tenant_id = u.tenant_id
			WHERE u.username = 'alpha-approver' ON CONFLICT DO NOTHING`, nil},
		{`INSERT INTO adc_user_roles (user_id, role_id, tenant_id)
			SELECT u.id, r.id, u.tenant_id FROM adc_users u
			JOIN adc_roles r ON r.role_code = 'auditor' AND r.tenant_id = u.tenant_id
			WHERE u.username = 'alpha-auditor' ON CONFLICT DO NOTHING`, nil},
		{`INSERT INTO adc_user_roles (user_id, role_id, tenant_id)
			SELECT u.id, r.id, u.tenant_id FROM adc_users u
			JOIN adc_roles r ON r.role_code = 'tenant_admin' AND r.tenant_id = u.tenant_id
			WHERE u.username = 'alpha-disabled' ON CONFLICT DO NOTHING`, nil},
	}
	for _, s := range steps {
		if _, err := e.pool.Exec(ctx, s.sql, s.args...); err != nil {
			return fmt.Errorf("seed: %w", err)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// HTTP helpers
// ---------------------------------------------------------------------------

// apiError is the unified error body of design/33 1.5.
type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	TraceID string `json:"trace_id,omitempty"`
}

// do issues an HTTP request against the in-process stack.
func (e *testEnv) do(method, path string, body any, hdr map[string]string) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	e.srv.Config.Handler.ServeHTTP(rec, req)
	return rec
}

// decodeErr decodes a unified error body and fails the test on mismatch.
func decodeErr(t *testing.T, rec *httptest.ResponseRecorder) apiError {
	t.Helper()
	var e apiError
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("response is not a unified error body: %s (body=%q)", err, rec.Body.String())
	}
	if e.Code == "" {
		t.Fatalf("error body missing code: %q", rec.Body.String())
	}
	return e
}

// wantErr asserts status code and business code in one check.
func wantErr(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d, want %d (body=%q)", rec.Code, status, rec.Body.String())
	}
	if code != "" {
		e := decodeErr(t, rec)
		if e.Code != code {
			t.Fatalf("business code = %q, want %q (body=%q)", e.Code, code, rec.Body.String())
		}
	}
}

// login posts to /v1/admin/auth/login and returns the bearer token on
// success (design/33 3.1.1: the session also arrives as a cookie, but
// Authorization Bearer is the cleaner test vehicle).
func (e *testEnv) login(t *testing.T, username, password string) *httptest.ResponseRecorder {
	t.Helper()
	return e.do(http.MethodPost, "/v1/admin/auth/login",
		map[string]string{"username": username, "password": password}, nil)
}

// loginOK logs in and fails the test on any non-200 outcome.
func (e *testEnv) loginOK(t *testing.T, username, password string) string {
	t.Helper()
	rec := e.login(t, username, password)
	if rec.Code != http.StatusOK {
		t.Fatalf("login %s: status = %d body=%q", username, rec.Code, rec.Body.String())
	}
	var resp struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.Token == "" {
		t.Fatalf("login response without token: %q", rec.Body.String())
	}
	return resp.Token
}

// authHdr wraps a session token into the Authorization header.
func authHdr(token string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + token}
}

// tenantID loads a fixture tenant's uuid for tenant_id query parameters.
func (e *testEnv) tenantID(t *testing.T, code string) string {
	t.Helper()
	var id string
	err := e.pool.QueryRow(context.Background(),
		`SELECT id::text FROM adc_tenants WHERE code = $1`, code).Scan(&id)
	if err != nil {
		t.Fatalf("load tenant %s: %v", code, err)
	}
	return id
}

// newUUID returns a random RFC 4122 UUIDv4 string (crypto/rand only) for
// synthetic audit rows and other UUID-shaped test payloads (design/32 1.3).
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
