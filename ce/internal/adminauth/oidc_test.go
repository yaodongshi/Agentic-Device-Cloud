package adminauth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type mockOIDCProvider struct {
	identity  *OIDCIdentity
	err       error
	mu        sync.Mutex
	state     string
	nonce     string
	challenge string
	verifier  string
}

func (m *mockOIDCProvider) AuthorizationURL(state, nonce, challenge, _ string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.state, m.nonce, m.challenge = state, nonce, challenge
	if m.err != nil {
		return "", m.err
	}
	return "https://idp.example/authorize?state=" + url.QueryEscape(state), nil
}

func (m *mockOIDCProvider) ExchangeCode(_ context.Context, code, _ string, verifier string) (*OIDCIdentity, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.verifier = verifier
	if m.err != nil || code != "good" {
		return nil, errors.New("exchange failed")
	}
	copy := *m.identity
	if copy.Nonce == "" {
		copy.Nonce = m.nonce
	}
	return &copy, nil
}

type mockIdentityStore struct {
	users map[string]*User
}

func (m *mockIdentityStore) GetByUsername(context.Context, string) (*User, error) {
	return nil, ErrUserNotFound
}

func (m *mockIdentityStore) GetByOIDCIdentity(_ context.Context, issuer, subject string) (*User, error) {
	u, ok := m.users[issuer+"\x00"+subject]
	if !ok {
		return nil, ErrUserNotFound
	}
	return u, nil
}

func (m *mockIdentityStore) CurrentAuthorization(_ context.Context, userID, tenantID string) (*AuthorizationSnapshot, error) {
	for _, user := range m.users {
		if user.ID == userID && user.TenantID == tenantID {
			return &AuthorizationSnapshot{UserStatus: user.Status, TenantStatus: user.TenantStatus, AuthzVersion: user.AuthzVersion, Roles: append([]string(nil), user.Roles...)}, nil
		}
	}
	return nil, ErrAuthorizationInvalid
}

type memoryOIDCStates struct {
	mu sync.Mutex
	tx map[string]*OIDCTransaction
}

type memoryOIDCAudit struct {
	events []OIDCAuditEvent
}

func (m *memoryOIDCAudit) RecordOIDCLogin(_ context.Context, event OIDCAuditEvent) error {
	m.events = append(m.events, event)
	return nil
}

func newMemoryOIDCStates() *memoryOIDCStates {
	return &memoryOIDCStates{tx: map[string]*OIDCTransaction{}}
}

func (m *memoryOIDCStates) Set(_ context.Context, state string, tx *OIDCTransaction, _ time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	copy := *tx
	m.tx[state] = &copy
	return nil
}

func (m *memoryOIDCStates) Consume(_ context.Context, state, binding string) (*OIDCTransaction, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	tx := m.tx[state]
	if tx == nil || tx.BrowserBinding != HashToken(binding) {
		return nil, nil
	}
	delete(m.tx, state)
	copy := *tx
	return &copy, nil
}

func buildOIDCTestHandler() (*Handler, *mockOIDCProvider, *memoryOIDCStates, *mockSessionStore) {
	issuer := "https://idp.example"
	provider := &mockOIDCProvider{identity: &OIDCIdentity{Issuer: issuer, Subject: "known"}}
	users := &mockIdentityStore{users: map[string]*User{issuer + "\x00known": {
		ID: "u1", Username: "known", TenantID: "t1", Status: userStatusActive, TenantStatus: tenantStatusActive, AuthzVersion: 1, Roles: []string{"tenant_admin"},
	}}}
	states := newMemoryOIDCStates()
	sessions := newMockSessionStore()
	h := NewHandler(users, sessions)
	h.OIDC = NewOIDCHandler(users, sessions, provider, states, OIDCConfig{Issuer: issuer, ClientID: "adc", RedirectURL: "https://console.example/v1/admin/auth/oidc/callback"})
	h.OIDC.FrontendRedirectURL = "/login?oidc=success"
	return h, provider, states, sessions
}

func startOIDC(t *testing.T, h *Handler) (*httptest.ResponseRecorder, string, *http.Cookie) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/admin/auth/oidc/start", nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("start status = %d, body=%s", rec.Code, rec.Body.String())
	}
	location, _ := rec.Result().Location()
	state := location.Query().Get("state")
	var binding *http.Cookie
	for _, cookie := range rec.Result().Cookies() {
		if cookie.Name == CookieOIDCTransaction {
			binding = cookie
		}
	}
	if state == "" || binding == nil {
		t.Fatal("start did not issue state and transaction cookie")
	}
	return rec, state, binding
}

func callbackOIDC(h *Handler, state string, binding *http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/auth/oidc/callback?state="+url.QueryEscape(state)+"&code=good", nil)
	if binding != nil {
		req.AddCookie(binding)
	}
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, req)
	return rec
}

func TestOIDCStatus(t *testing.T) {
	disabled := NewHandler(&mockIdentityStore{}, newMockSessionStore())
	rec := httptest.NewRecorder()
	disabled.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/admin/auth/oidc/status", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"enabled":false`) {
		t.Fatalf("disabled status: %d %s", rec.Code, rec.Body.String())
	}
	h, _, _, _ := buildOIDCTestHandler()
	rec = httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/admin/auth/oidc/status", nil))
	if !strings.Contains(rec.Body.String(), `"enabled":true`) {
		t.Fatalf("enabled status: %s", rec.Body.String())
	}
}

func TestOIDCStartUsesNoncePKCES256AndBindingCookie(t *testing.T) {
	h, provider, states, _ := buildOIDCTestHandler()
	rec, state, cookie := startOIDC(t, h)
	provider.mu.Lock()
	nonce, challenge := provider.nonce, provider.challenge
	provider.mu.Unlock()
	if nonce == "" || challenge == "" || challenge == nonce {
		t.Fatalf("nonce=%q challenge=%q", nonce, challenge)
	}
	if !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("insecure transaction cookie: %+v", cookie)
	}
	tx := states.tx[state]
	if tx == nil || tx.Nonce != nonce || tx.PKCEVerifier == "" || tx.BrowserBinding != HashToken(cookie.Value) {
		t.Fatalf("incomplete transaction: %+v", tx)
	}
	if strings.Contains(rec.Header().Get("Location"), nonce) {
		t.Fatal("mock redirect unexpectedly exposed nonce")
	}
}

func TestOIDCCallbackCreatesCookieOnlySession(t *testing.T) {
	h, provider, _, sessions := buildOIDCTestHandler()
	_, state, binding := startOIDC(t, h)
	rec := callbackOIDC(h, state, binding)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/login?oidc=success" {
		t.Fatalf("callback = %d %q %s", rec.Code, rec.Header().Get("Location"), rec.Body.String())
	}
	if rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("missing callback security headers: %v", rec.Header())
	}
	if strings.Contains(rec.Body.String(), "adc_session") || strings.Contains(rec.Header().Get("Location"), "token") {
		t.Fatal("callback leaked session token")
	}
	var sessionCookie *http.Cookie
	for _, cookie := range rec.Result().Cookies() {
		if cookie.Name == CookieSession {
			sessionCookie = cookie
		}
	}
	if sessionCookie == nil || !sessionCookie.HttpOnly || len(sessions.sessions) != 1 {
		t.Fatalf("session cookie/store missing: %+v", sessionCookie)
	}
	for _, session := range sessions.sessions {
		if session.AuthzVersion != 1 {
			t.Fatalf("OIDC session authz version = %d, want 1", session.AuthzVersion)
		}
	}
	provider.mu.Lock()
	if provider.verifier == "" {
		t.Fatal("PKCE verifier not sent to exchange")
	}
	provider.mu.Unlock()

	req := httptest.NewRequest(http.MethodGet, "/v1/admin/auth/session", nil)
	req.AddCookie(sessionCookie)
	current := httptest.NewRecorder()
	h.Routes().ServeHTTP(current, req)
	var body currentSessionResponse
	if current.Code != http.StatusOK || json.Unmarshal(current.Body.Bytes(), &body) != nil || body.UserID != "u1" {
		t.Fatalf("current session = %d %s", current.Code, current.Body.String())
	}
}

func TestOIDCCallbackRejectsUnknownNonceAndBrowser(t *testing.T) {
	for _, mutate := range []func(*mockOIDCProvider, *http.Cookie){
		func(_ *mockOIDCProvider, c *http.Cookie) { c.Value = "other-browser" },
		func(p *mockOIDCProvider, _ *http.Cookie) { p.identity.Nonce = "forged-nonce" },
	} {
		h, provider, _, sessions := buildOIDCTestHandler()
		_, state, binding := startOIDC(t, h)
		mutate(provider, binding)
		rec := callbackOIDC(h, state, binding)
		if rec.Code != http.StatusUnauthorized || len(sessions.sessions) != 0 {
			t.Fatalf("rejection = %d sessions=%d", rec.Code, len(sessions.sessions))
		}
	}
}

func TestOIDCCallbackUnknownIdentityDenied(t *testing.T) {
	h, provider, _, sessions := buildOIDCTestHandler()
	audit := &memoryOIDCAudit{}
	h.OIDC.Audit = audit
	provider.identity.Subject = "unknown"
	_, state, binding := startOIDC(t, h)
	rec := callbackOIDC(h, state, binding)
	if rec.Code != http.StatusUnauthorized || len(sessions.sessions) != 0 {
		t.Fatalf("unknown identity = %d sessions=%d", rec.Code, len(sessions.sessions))
	}
	if len(audit.events) != 1 || audit.events[0].Result != "rejected" || audit.events[0].Subject != "unknown" {
		t.Fatalf("rejected callback audit = %+v", audit.events)
	}
}

func TestOIDCCallbackAuditsSuccessWithoutCredentials(t *testing.T) {
	h, _, _, _ := buildOIDCTestHandler()
	audit := &memoryOIDCAudit{}
	h.OIDC.Audit = audit
	_, state, binding := startOIDC(t, h)
	if rec := callbackOIDC(h, state, binding); rec.Code != http.StatusFound {
		t.Fatalf("callback status = %d", rec.Code)
	}
	if len(audit.events) != 1 || audit.events[0].Result != "succeeded" || audit.events[0].UserID != "u1" || audit.events[0].TenantID != "t1" {
		t.Fatalf("successful callback audit = %+v", audit.events)
	}
}

func TestOIDCTransactionConcurrentSingleUse(t *testing.T) {
	h, _, _, _ := buildOIDCTestHandler()
	_, state, binding := startOIDC(t, h)
	var successes atomic.Int32
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			copy := *binding
			if callbackOIDC(h, state, &copy).Code == http.StatusFound {
				successes.Add(1)
			}
		}()
	}
	wg.Wait()
	if successes.Load() != 1 {
		t.Fatalf("successful callbacks = %d, want 1", successes.Load())
	}
}

func TestValkeyOIDCStateStoreAtomicBinding(t *testing.T) {
	rdb := newFakeValkey()
	store := NewValkeyOIDCStateStore(rdb)
	tx := &OIDCTransaction{Nonce: "n", PKCEVerifier: "v", BrowserBinding: HashToken("browser")}
	if err := store.Set(context.Background(), "state", tx, OIDCStateTTL); err != nil {
		t.Fatal(err)
	}
	if got, err := store.Consume(context.Background(), "state", "attacker"); err != nil || got != nil {
		t.Fatalf("attacker consume = %+v, %v", got, err)
	}
	if got, err := store.Consume(context.Background(), "state", "browser"); err != nil || got == nil {
		t.Fatalf("bound consume = %+v, %v", got, err)
	}
	if got, _ := store.Consume(context.Background(), "state", "browser"); got != nil {
		t.Fatal("transaction replay succeeded")
	}
}
