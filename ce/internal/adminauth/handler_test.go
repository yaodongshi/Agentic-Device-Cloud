package adminauth

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"adc.dev/ce/internal/httpx"
)

// mockUserStore serves adc_users rows from a map; a missing username yields
// ErrUserNotFound, and err simulates a storage outage.
type mockUserStore struct {
	users map[string]*User
	err   error
}

func (m *mockUserStore) GetByUsername(_ context.Context, username string) (*User, error) {
	if m.err != nil {
		return nil, m.err
	}
	u, ok := m.users[username]
	if !ok {
		return nil, ErrUserNotFound
	}
	return u, nil
}

// mockSessionStore keeps sessions in memory. It is shared by the handler
// and RBAC middleware tests.
type mockSessionStore struct {
	mu         sync.Mutex
	sessions   map[string]*Session
	createErr  error
	getErr     error
	deleteErr  error
	createdTTL time.Duration
	authz      *AuthorizationSnapshot
	authzErr   error
}

func newMockSessionStore() *mockSessionStore {
	return &mockSessionStore{sessions: make(map[string]*Session)}
}

func (m *mockSessionStore) Create(_ context.Context, s *Session, ttl time.Duration) error {
	if m.createErr != nil {
		return m.createErr
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *s
	cp.Roles = append([]string(nil), s.Roles...)
	m.sessions[s.TokenHash] = &cp
	m.createdTTL = ttl
	return nil
}

func (m *mockSessionStore) Get(_ context.Context, tokenHash string) (*Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.getErr != nil {
		return nil, m.getErr
	}
	s, ok := m.sessions[tokenHash]
	if !ok {
		return nil, ErrSessionNotFound
	}
	cp := *s
	cp.Roles = append([]string(nil), s.Roles...)
	return &cp, nil
}

func (m *mockSessionStore) Delete(_ context.Context, tokenHash string) error {
	if m.deleteErr != nil {
		return m.deleteErr
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, tokenHash)
	return nil
}

func (m *mockSessionStore) CurrentAuthorization(_ context.Context, userID, tenantID string) (*AuthorizationSnapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.authzErr != nil {
		return nil, m.authzErr
	}
	if m.authz != nil {
		copy := *m.authz
		copy.Roles = append([]string(nil), m.authz.Roles...)
		return &copy, nil
	}
	for _, s := range m.sessions {
		if s.UserID == userID && s.TenantID == tenantID {
			return &AuthorizationSnapshot{UserStatus: userStatusActive, TenantStatus: tenantStatusActive, AuthzVersion: s.AuthzVersion, Roles: append([]string(nil), s.Roles...)}, nil
		}
	}
	return nil, ErrAuthorizationInvalid
}

// testUser builds an ACTIVE user whose password is testPassword, with the
// given role list.
func testUser(t *testing.T, username string, status string, roles ...string) *User {
	t.Helper()
	hash, err := HashPassword(testPassword)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	return &User{
		ID:           "u_9f8e7d6c",
		Username:     username,
		PasswordHash: hash,
		TenantID:     "t_1a2b3c4d",
		Status:       status,
		TenantStatus: tenantStatusActive,
		AuthzVersion: 1,
		Roles:        roles,
	}
}

const testPassword = "s3cret-admin-pass"

func loginBody(username, password string) io.Reader {
	return strings.NewReader(`{"username":` + jsonString(username) + `,"password":` + jsonString(password) + `}`)
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func postLogin(t *testing.T, h http.Handler, username, password string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/auth/login", loginBody(username, password))
	h.ServeHTTP(rec, req)
	return rec
}

func TestLoginSuccess(t *testing.T) {
	users := &mockUserStore{users: map[string]*User{
		"itadmin": testUser(t, "itadmin", "ACTIVE", "tenant_admin"),
	}}
	sessions := newMockSessionStore()
	h := NewHandler(users, sessions).Routes()

	rec := postLogin(t, h, "itadmin", testPassword)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	var body loginResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not a loginResponse: %v", err)
	}
	if body.Token == "" {
		t.Fatal("login response has no token")
	}
	if body.UserID != "u_9f8e7d6c" || body.TenantID != "t_1a2b3c4d" || body.Role != "tenant_admin" {
		t.Fatalf("login response = %+v, want user/tenant/role fields filled", body)
	}
	if body.ExpiresAt.IsZero() {
		t.Fatal("login response has no expires_at")
	}

	// The session must be stored under the token hash with the default TTL.
	sessions.mu.Lock()
	defer sessions.mu.Unlock()
	stored, ok := sessions.sessions[HashToken(body.Token)]
	if !ok {
		t.Fatal("session was not stored under the token hash")
	}
	if stored.UserID != "u_9f8e7d6c" || stored.TenantID != "t_1a2b3c4d" {
		t.Fatalf("stored session = %+v, want user/tenant from the record", stored)
	}
	if stored.AuthzVersion != 1 {
		t.Fatalf("stored authz version = %d, want 1", stored.AuthzVersion)
	}
	if len(stored.Roles) != 1 || stored.Roles[0] != "tenant_admin" {
		t.Fatalf("stored session roles = %v, want [tenant_admin]", stored.Roles)
	}
	if sessions.createdTTL != SessionTTL {
		t.Fatalf("session TTL = %v, want %v", sessions.createdTTL, SessionTTL)
	}
	// The stored value is only the hash, never the plaintext token.
	if _, ok := sessions.sessions[body.Token]; ok {
		t.Fatal("plaintext token was stored as a key; only hashes may be stored")
	}

	cookies := rec.Result().Cookies()
	var sessionCookie *http.Cookie
	for _, c := range cookies {
		if c.Name == CookieSession {
			sessionCookie = c
		}
	}
	if sessionCookie == nil {
		t.Fatal("no adc_session cookie set")
	}
	if sessionCookie.Value != body.Token {
		t.Fatalf("cookie value = %q, want the response token", sessionCookie.Value)
	}
	if !sessionCookie.HttpOnly || !sessionCookie.Secure || sessionCookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("cookie flags = HttpOnly:%v Secure:%v SameSite:%v, want all hardened",
			sessionCookie.HttpOnly, sessionCookie.Secure, sessionCookie.SameSite)
	}
}

func TestLoginWrongPassword(t *testing.T) {
	users := &mockUserStore{users: map[string]*User{
		"itadmin": testUser(t, "itadmin", "ACTIVE", "tenant_admin"),
	}}
	sessions := newMockSessionStore()
	h := NewHandler(users, sessions).Routes()

	rec := postLogin(t, h, "itadmin", "wrong-password")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (body: %s)", rec.Code, rec.Body.String())
	}
	var body httpx.ErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not an error body: %v", err)
	}
	if body.Code != codeUnauthorized {
		t.Fatalf("error code = %q, want %q", body.Code, codeUnauthorized)
	}
	if len(sessions.sessions) != 0 {
		t.Fatal("failed login created a session")
	}
}

func TestLoginUnknownUser(t *testing.T) {
	h := NewHandler(&mockUserStore{users: map[string]*User{}}, newMockSessionStore()).Routes()

	rec := postLogin(t, h, "ghost", testPassword)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (body: %s)", rec.Code, rec.Body.String())
	}
	var body httpx.ErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not an error body: %v", err)
	}
	if body.Code != codeUnauthorized {
		t.Fatalf("error code = %q, want %q", body.Code, codeUnauthorized)
	}
}

func TestLoginDisabledUser(t *testing.T) {
	users := &mockUserStore{users: map[string]*User{
		"itadmin": testUser(t, "itadmin", "DISABLED", "tenant_admin"),
	}}
	h := NewHandler(users, newMockSessionStore()).Routes()

	rec := postLogin(t, h, "itadmin", testPassword)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (body: %s)", rec.Code, rec.Body.String())
	}
	var body httpx.ErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not an error body: %v", err)
	}
	if body.Code != codeUnauthorized {
		t.Fatalf("error code = %q, want %q", body.Code, codeUnauthorized)
	}
}

func TestLoginLockedUser(t *testing.T) {
	users := &mockUserStore{users: map[string]*User{
		"itadmin": testUser(t, "itadmin", "LOCKED", "tenant_admin"),
	}}
	h := NewHandler(users, newMockSessionStore()).Routes()

	rec := postLogin(t, h, "itadmin", testPassword)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (body: %s)", rec.Code, rec.Body.String())
	}
}

// TestLoginFailuresAreIndistinguishable proves no account enumeration: a
// wrong password, an unknown username and a disabled user all produce the
// byte-identical response.
func TestLoginFailuresAreIndistinguishable(t *testing.T) {
	users := &mockUserStore{users: map[string]*User{
		"itadmin": testUser(t, "itadmin", "ACTIVE", "tenant_admin"),
		"blocked": testUser(t, "blocked", "DISABLED", "tenant_admin"),
	}}
	h := NewHandler(users, newMockSessionStore()).Routes()

	wrong := postLogin(t, h, "itadmin", "wrong-password")
	unknown := postLogin(t, h, "ghost", "wrong-password")
	disabled := postLogin(t, h, "blocked", testPassword)

	for _, pair := range [][2]*httptest.ResponseRecorder{
		{wrong, unknown}, {wrong, disabled}, {unknown, disabled},
	} {
		if pair[0].Code != pair[1].Code || pair[0].Body.String() != pair[1].Body.String() {
			t.Fatalf("failure responses differ: %d %q vs %d %q",
				pair[0].Code, pair[0].Body.String(), pair[1].Code, pair[1].Body.String())
		}
	}
}

func TestLoginMalformedBody(t *testing.T) {
	h := NewHandler(&mockUserStore{}, newMockSessionStore()).Routes()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/auth/login", strings.NewReader("{not-json"))
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body: %s)", rec.Code, rec.Body.String())
	}
	var body httpx.ErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not an error body: %v", err)
	}
	if body.Code != codeBadRequest {
		t.Fatalf("error code = %q, want %q", body.Code, codeBadRequest)
	}
}

func TestLoginMissingCredentials(t *testing.T) {
	h := NewHandler(&mockUserStore{}, newMockSessionStore()).Routes()

	for _, body := range []string{`{}`, `{"username":"u"}`, `{"password":"p"}`} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/admin/auth/login", strings.NewReader(body))
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("body %s: status = %d, want 400", body, rec.Code)
		}
	}
}

func TestLoginUserStoreError(t *testing.T) {
	users := &mockUserStore{err: errors.New("pg down")}
	h := NewHandler(users, newMockSessionStore()).Routes()

	rec := postLogin(t, h, "itadmin", testPassword)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (body: %s)", rec.Code, rec.Body.String())
	}
	var body httpx.ErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not an error body: %v", err)
	}
	if body.Code != codeInternal {
		t.Fatalf("error code = %q, want %q", body.Code, codeInternal)
	}
}

func TestLoginSessionCreateError(t *testing.T) {
	users := &mockUserStore{users: map[string]*User{
		"itadmin": testUser(t, "itadmin", "ACTIVE", "tenant_admin"),
	}}
	sessions := newMockSessionStore()
	sessions.createErr = errors.New("valkey down")
	h := NewHandler(users, sessions).Routes()

	rec := postLogin(t, h, "itadmin", testPassword)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (body: %s)", rec.Code, rec.Body.String())
	}
}

func TestLogoutInvalidatesSession(t *testing.T) {
	users := &mockUserStore{users: map[string]*User{
		"itadmin": testUser(t, "itadmin", "ACTIVE", "tenant_admin"),
	}}
	sessions := newMockSessionStore()
	h := NewHandler(users, sessions).Routes()

	rec := postLogin(t, h, "itadmin", testPassword)
	var login loginResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &login); err != nil {
		t.Fatalf("decode login response: %v", err)
	}

	// The token authenticates before logout.
	if _, err := sessions.Get(context.Background(), HashToken(login.Token)); err != nil {
		t.Fatalf("session missing right after login: %v", err)
	}

	logout := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/auth/logout", nil)
	req.Header.Set("Authorization", "Bearer "+login.Token)
	h.ServeHTTP(logout, req)
	if logout.Code != http.StatusNoContent {
		t.Fatalf("logout status = %d, want 204 (body: %s)", logout.Code, logout.Body.String())
	}

	// The session is gone and the middleware chain rejects the token.
	if _, err := sessions.Get(context.Background(), HashToken(login.Token)); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("session still present after logout: %v", err)
	}

	protected := Authorize(sessions)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodGet, "/v1/admin/tenants", nil)
	req2.Header.Set("Authorization", "Bearer "+login.Token)
	protected.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusUnauthorized {
		t.Fatalf("protected endpoint after logout = %d, want 401", rec2.Code)
	}
}

func TestLogoutViaCookie(t *testing.T) {
	users := &mockUserStore{users: map[string]*User{
		"itadmin": testUser(t, "itadmin", "ACTIVE", "tenant_admin"),
	}}
	sessions := newMockSessionStore()
	h := NewHandler(users, sessions).Routes()

	rec := postLogin(t, h, "itadmin", testPassword)
	var login loginResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &login); err != nil {
		t.Fatalf("decode login response: %v", err)
	}

	logout := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/auth/logout", nil)
	req.AddCookie(&http.Cookie{Name: CookieSession, Value: login.Token})
	h.ServeHTTP(logout, req)
	if logout.Code != http.StatusNoContent {
		t.Fatalf("logout status = %d, want 204 (body: %s)", logout.Code, logout.Body.String())
	}
	if _, err := sessions.Get(context.Background(), HashToken(login.Token)); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("session still present after cookie logout: %v", err)
	}
}

func TestLogoutIdempotent(t *testing.T) {
	h := NewHandler(&mockUserStore{}, newMockSessionStore()).Routes()

	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/admin/auth/logout", nil)
		req.Header.Set("Authorization", "Bearer some-token-that-was-never-created")
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("logout #%d status = %d, want 204", i+1, rec.Code)
		}
	}
}

func TestLogoutWithoutToken(t *testing.T) {
	h := NewHandler(&mockUserStore{}, newMockSessionStore()).Routes()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/auth/logout", nil)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (body: %s)", rec.Code, rec.Body.String())
	}
	var body httpx.ErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not an error body: %v", err)
	}
	if body.Code != codeUnauthorized {
		t.Fatalf("error code = %q, want %q", body.Code, codeUnauthorized)
	}
}

func TestLogoutStoreError(t *testing.T) {
	sessions := newMockSessionStore()
	sessions.deleteErr = errors.New("valkey down")
	h := NewHandler(&mockUserStore{}, sessions).Routes()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/auth/logout", nil)
	req.Header.Set("Authorization", "Bearer some-token")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (body: %s)", rec.Code, rec.Body.String())
	}
}
