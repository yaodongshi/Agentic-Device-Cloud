// Contract tests for POST /v1/admin/auth/login (design/33 3.1.1, FR-014):
// session issuance on success, the uniform 401 code 10002 for wrong
// passwords and disabled users (the three failure modes are intentionally
// indistinguishable, SEC-19-style enumeration defense), and the 400 for
// malformed bodies. Sessions live in real Valkey; users in real PG.
package integration

import (
	"encoding/json"
	"net/http"
	"testing"
)

// TestLoginSuccess issues a session for the seeded platform admin: 200,
// token + user fields, Set-Cookie, and the token actually authorizes an
// Admin API call (session round-trip through real Valkey).
func TestLoginSuccess(t *testing.T) {
	e := envOrSkip(t)
	rec := e.login(t, fixturePlatformUser, fixturePlatformPass)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%q)", rec.Code, rec.Body.String())
	}
	var resp struct {
		Token     string `json:"token"`
		UserID    string `json:"user_id"`
		TenantID  string `json:"tenant_id"`
		Role      string `json:"role"`
		ExpiresAt string `json:"expires_at"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode login response: %v (body=%q)", err, rec.Body.String())
	}
	if resp.Token == "" || resp.UserID == "" || resp.TenantID == "" || resp.Role == "" {
		t.Fatalf("incomplete login response: %+v", resp)
	}
	if resp.Role != "platform_admin" {
		t.Fatalf("role = %q, want platform_admin", resp.Role)
	}
	hasCookie := false
	for _, c := range rec.Result().Cookies() {
		if c.Name == "adc_session" && c.Value != "" {
			hasCookie = true
		}
	}
	if !hasCookie {
		t.Fatal("Set-Cookie adc_session missing (design/33 3.1.1)")
	}

	// The issued token must authorize a real Admin API request.
	auth := rec.Result().Cookies()
	_ = auth
	req := e.do(http.MethodGet, "/v1/admin/tenants", nil, authHdr(resp.Token))
	if req.Code != http.StatusOK {
		t.Fatalf("authenticated tenants list = %d, want 200 (body=%q)", req.Code, req.Body.String())
	}
}

// TestLoginWrongPassword answers the uniform 401 code 10002 and the same
// generic message as an unknown user (no account enumeration, design/33
// 3.1.1).
func TestLoginWrongPassword(t *testing.T) {
	e := envOrSkip(t)
	rec := e.login(t, fixturePlatformUser, "wrong-password")
	wantErr(t, rec, http.StatusUnauthorized, "10002")
	if rec.Body.String() != "" {
		var body struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if body.Message != "invalid username or password" {
			t.Fatalf("message = %q, want the generic credential message", body.Message)
		}
	}
}

// TestLoginUnknownUser is the enumeration-defense twin of the wrong
// password case: identical status, code and message.
func TestLoginUnknownUser(t *testing.T) {
	e := envOrSkip(t)
	rec := e.login(t, "no-such-user", "whatever")
	wantErr(t, rec, http.StatusUnauthorized, "10002")
}

// TestLoginDisabledUser rejects a seeded DISABLED user with the same
// uniform 401 code 10002 (design/33 3.1.1: user status never leaks).
func TestLoginDisabledUser(t *testing.T) {
	e := envOrSkip(t)
	rec := e.login(t, fixtureDisabledUser, fixtureDisabledPass)
	wantErr(t, rec, http.StatusUnauthorized, "10002")
}

// TestLoginMalformedBody answers 400 code 10001 on missing credentials
// and non-JSON bodies (design/33 3.1.1).
func TestLoginMalformedBody(t *testing.T) {
	e := envOrSkip(t)
	for _, body := range []map[string]string{
		{},                       // both fields missing
		{"username": "someone"},  // password missing
		{"password": "whatever"}, // username missing
	} {
		rec := e.do(http.MethodPost, "/v1/admin/auth/login", body, nil)
		wantErr(t, rec, http.StatusBadRequest, "10001")
	}
}

// TestAdminAPIWithoutSession verifies the Admin API rejects anonymous
// requests with 401 code 10002 (session auth gate, design/33 1.2).
func TestAdminAPIWithoutSession(t *testing.T) {
	e := envOrSkip(t)
	rec := e.do(http.MethodGet, "/v1/admin/tenants", nil, nil)
	wantErr(t, rec, http.StatusUnauthorized, "10002")
}

// TestLogoutInvalidatesSession: logout deletes the session and the token
// stops working (204, then 401 on reuse).
func TestLogoutInvalidatesSession(t *testing.T) {
	e := envOrSkip(t)
	token := e.loginOK(t, fixtureAlphaAdmin, fixtureAlphaPass)
	rec := e.do(http.MethodPost, "/v1/admin/auth/logout", nil, authHdr(token))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("logout = %d, want 204 (body=%q)", rec.Code, rec.Body.String())
	}
	rec = e.do(http.MethodGet, "/v1/admin/tenants", nil, authHdr(token))
	wantErr(t, rec, http.StatusUnauthorized, "10002")
}
