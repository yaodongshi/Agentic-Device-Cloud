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
	"testing"
	"time"

	"adc.dev/ce/internal/httpx"
)

// mockOIDCProvider is a scriptable OIDCProvider: any code other than
// "good-code" fails the exchange; exchanges are recorded.
type mockOIDCProvider struct {
	identity    *OIDCIdentity
	exchangeErr error
	authURLs    []string
	codes       []string
}

func (m *mockOIDCProvider) AuthorizationURL(state, redirect string) string {
	m.authURLs = append(m.authURLs, state)
	return "https://idp.example.com/authorize?state=" + url.QueryEscape(state) + "&redirect_uri=" + url.QueryEscape(redirect)
}

func (m *mockOIDCProvider) ExchangeCode(_ context.Context, code, _ string) (*OIDCIdentity, error) {
	m.codes = append(m.codes, code)
	if m.exchangeErr != nil {
		return nil, m.exchangeErr
	}
	if code != "good-code" {
		return nil, errors.New("idp: unknown code")
	}
	return m.identity, nil
}

// mockSubjectStore serves users by username and by OIDC subject.
type mockSubjectStore struct {
	users     map[string]*User
	bySubject map[string]*User
	err       error
}

func newMockSubjectStore() *mockSubjectStore {
	return &mockSubjectStore{users: map[string]*User{}, bySubject: map[string]*User{}}
}

func (m *mockSubjectStore) GetByUsername(_ context.Context, username string) (*User, error) {
	if m.err != nil {
		return nil, m.err
	}
	u, ok := m.users[username]
	if !ok {
		return nil, ErrUserNotFound
	}
	return u, nil
}

func (m *mockSubjectStore) GetBySubject(_ context.Context, subject string) (*User, error) {
	if m.err != nil {
		return nil, m.err
	}
	u, ok := m.bySubject[subject]
	if !ok {
		return nil, ErrUserNotFound
	}
	return u, nil
}

// mockStateStore keeps OIDC nonces in memory.
type mockStateStore struct {
	mu     sync.Mutex
	states map[string]time.Duration
	setErr error
}

func newMockStateStore() *mockStateStore {
	return &mockStateStore{states: make(map[string]time.Duration)}
}

func (m *mockStateStore) Set(_ context.Context, state string, ttl time.Duration) error {
	if m.setErr != nil {
		return m.setErr
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.states[state] = ttl
	return nil
}

func (m *mockStateStore) Consume(_ context.Context, state string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.states[state]
	if !ok {
		return false, nil
	}
	delete(m.states, state)
	return true, nil
}

// mockProvisioner records every auto-provision call.
type mockProvisioner struct {
	user    *User
	err     error
	records []struct {
		ID     *OIDCIdentity
		Tenant string
		Roles  []string
	}
}

func (m *mockProvisioner) Provision(_ context.Context, id *OIDCIdentity, tenant string, roles []string) (*User, error) {
	m.records = append(m.records, struct {
		ID     *OIDCIdentity
		Tenant string
		Roles  []string
	}{ID: id, Tenant: tenant, Roles: append([]string(nil), roles...)})
	if m.err != nil {
		return nil, m.err
	}
	return m.user, nil
}

// buildOIDCHandler wires a handler with the OIDC endpoints enabled.
func buildOIDCHandler(provider *mockOIDCProvider, subjects *mockSubjectStore, states *mockStateStore, prov *mockProvisioner) (*Handler, *mockSessionStore) {
	sessions := newMockSessionStore()
	h := NewHandler(subjects, sessions)
	h.OIDC = NewOIDCHandler(subjects, sessions, provider, states, OIDCConfig{
		Issuer:      "https://idp.example.com",
		ClientID:    "adc-console",
		RedirectURL: "https://console.adc.dev/v1/admin/auth/oidc/callback",
		Scope:       "openid email profile",
	})
	h.OIDC.Provisioner = prov.Provision
	h.OIDC.DefaultTenantID = "t_default"
	h.OIDC.DefaultRoles = []string{"tenant_admin"}
	h.OIDC.FrontendRedirectURL = "https://console.adc.dev/dashboard"
	return h, sessions
}

func getOIDC(t *testing.T, h *Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	h.Routes().ServeHTTP(rec, req)
	return rec
}

func TestOIDCDisabledEndpoints404(t *testing.T) {
	// No OIDC wired at all: both endpoints answer 404 code 10004.
	h := NewHandler(&mockUserStore{}, newMockSessionStore())
	for _, path := range []string{
		"/v1/admin/auth/oidc/start",
		"/v1/admin/auth/oidc/callback?state=s&code=c",
	} {
		rec := getOIDC(t, h, path)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s: status = %d, want 404 (body: %s)", path, rec.Code, rec.Body.String())
		}
		var body httpx.ErrorBody
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("response is not an error body: %v", err)
		}
		if body.Code != codeNotFound {
			t.Fatalf("error code = %q, want %q", body.Code, codeNotFound)
		}
	}
}

func TestOIDCDisabledWithNilProvider(t *testing.T) {
	subjects := newMockSubjectStore()
	sessions := newMockSessionStore()
	h := NewHandler(subjects, sessions)
	h.OIDC = NewOIDCHandler(subjects, sessions, nil, newMockStateStore(), OIDCConfig{})
	rec := getOIDC(t, h, "/v1/admin/auth/oidc/start")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body: %s)", rec.Code, rec.Body.String())
	}
}

func TestOIDCStartRedirectsToIdP(t *testing.T) {
	provider := &mockOIDCProvider{identity: &OIDCIdentity{Subject: "sub-1", Email: "a@b.c", Name: "A"}}
	subjects := newMockSubjectStore()
	states := newMockStateStore()
	h, _ := buildOIDCHandler(provider, subjects, states, &mockProvisioner{})

	rec := getOIDC(t, h, "/v1/admin/auth/oidc/start")
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302 (body: %s)", rec.Code, rec.Body.String())
	}
	loc, err := rec.Result().Location()
	if err != nil || loc == nil {
		t.Fatalf("no Location header: %v", err)
	}
	if !strings.HasPrefix(loc.String(), "https://idp.example.com/authorize?state=") {
		t.Fatalf("Location = %q, want the IdP authorization URL", loc.String())
	}
	// The state must exist in the store with the OIDC TTL.
	if len(states.states) != 1 {
		t.Fatalf("state store has %d entries, want 1", len(states.states))
	}
	for state, ttl := range states.states {
		if !strings.Contains(loc.String(), url.QueryEscape(state)) {
			t.Fatalf("redirect carries a state not stored (or vice versa)")
		}
		if ttl != OIDCStateTTL {
			t.Fatalf("state TTL = %v, want %v", ttl, OIDCStateTTL)
		}
	}
}

func TestOIDCStartStateStoreError(t *testing.T) {
	provider := &mockOIDCProvider{}
	subjects := newMockSubjectStore()
	states := newMockStateStore()
	states.setErr = errors.New("valkey down")
	h, _ := buildOIDCHandler(provider, subjects, states, &mockProvisioner{})

	rec := getOIDC(t, h, "/v1/admin/auth/oidc/start")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (body: %s)", rec.Code, rec.Body.String())
	}
}

func TestOIDCCallbackStateCSRFRejected(t *testing.T) {
	provider := &mockOIDCProvider{identity: &OIDCIdentity{Subject: "sub-1"}}
	subjects := newMockSubjectStore()
	states := newMockStateStore()
	h, sessions := buildOIDCHandler(provider, subjects, states, &mockProvisioner{})

	for _, path := range []string{
		"/v1/admin/auth/oidc/callback?state=forged&code=good-code",
		"/v1/admin/auth/oidc/callback?code=good-code",
	} {
		rec := getOIDC(t, h, path)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s: status = %d, want 401 (body: %s)", path, rec.Code, rec.Body.String())
		}
		var body httpx.ErrorBody
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("response is not an error body: %v", err)
		}
		if body.Code != codeUnauthorized {
			t.Fatalf("error code = %q, want %q", body.Code, codeUnauthorized)
		}
	}
	if len(provider.codes) != 0 {
		t.Fatal("code was exchanged despite a bad state")
	}
	if len(sessions.sessions) != 0 {
		t.Fatal("CSRF-rejected callback created a session")
	}
}

func TestOIDCCallbackExchangeFailure(t *testing.T) {
	provider := &mockOIDCProvider{identity: &OIDCIdentity{Subject: "sub-1"}}
	subjects := newMockSubjectStore()
	states := newMockStateStore()
	h, sessions := buildOIDCHandler(provider, subjects, states, &mockProvisioner{})

	// A valid state but a code the IdP rejects.
	state := seedState(t, states)
	rec := getOIDC(t, h, "/v1/admin/auth/oidc/callback?state="+url.QueryEscape(state)+"&code=stale-code")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (body: %s)", rec.Code, rec.Body.String())
	}
	if len(sessions.sessions) != 0 {
		t.Fatal("failed exchange created a session")
	}
}

func TestOIDCCallbackMissingCode(t *testing.T) {
	provider := &mockOIDCProvider{identity: &OIDCIdentity{Subject: "sub-1"}}
	subjects := newMockSubjectStore()
	states := newMockStateStore()
	h, _ := buildOIDCHandler(provider, subjects, states, &mockProvisioner{})

	state := seedState(t, states)
	rec := getOIDC(t, h, "/v1/admin/auth/oidc/callback?state="+url.QueryEscape(state))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body: %s)", rec.Code, rec.Body.String())
	}
}

func TestOIDCCallbackAutoProvisionsUser(t *testing.T) {
	provider := &mockOIDCProvider{identity: &OIDCIdentity{Subject: "sub-new", Email: "new@corp.com", Name: "New User"}}
	subjects := newMockSubjectStore()
	states := newMockStateStore()
	prov := &mockProvisioner{user: &User{
		ID:       "u_oidc_new",
		Username: "new@corp.com",
		TenantID: "t_default",
		Status:   userStatusActive,
		Roles:    []string{"tenant_admin"},
	}}
	h, sessions := buildOIDCHandler(provider, subjects, states, prov)

	state := seedState(t, states)
	rec := getOIDC(t, h, "/v1/admin/auth/oidc/callback?state="+url.QueryEscape(state)+"&code=good-code")
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302 (body: %s)", rec.Code, rec.Body.String())
	}
	loc := rec.Result().Header.Get("Location")
	if loc != "https://console.adc.dev/dashboard" {
		t.Fatalf("Location = %q, want the frontend redirect", loc)
	}
	if len(prov.records) != 1 {
		t.Fatalf("provisioner calls = %d, want 1", len(prov.records))
	}
	rec0 := prov.records[0]
	if rec0.ID.Subject != "sub-new" || rec0.ID.Email != "new@corp.com" {
		t.Fatalf("provisioned identity = %+v, want subject sub-new", rec0.ID)
	}
	if rec0.Tenant != "t_default" {
		t.Fatalf("provisioned tenant = %q, want t_default", rec0.Tenant)
	}
	if len(rec0.Roles) != 1 || rec0.Roles[0] != "tenant_admin" {
		t.Fatalf("provisioned roles = %v, want [tenant_admin]", rec0.Roles)
	}
	// A session for the provisioned user exists and the cookie is set.
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
	stored, err := sessions.Get(context.Background(), HashToken(sessionCookie.Value))
	if err != nil {
		t.Fatalf("session missing: %v", err)
	}
	if stored.UserID != "u_oidc_new" || stored.TenantID != "t_default" {
		t.Fatalf("stored session = %+v, want the provisioned user", stored)
	}
}

func TestOIDCCallbackMapsExistingUser(t *testing.T) {
	provider := &mockOIDCProvider{identity: &OIDCIdentity{Subject: "sub-known"}}
	subjects := newMockSubjectStore()
	subjects.bySubject["sub-known"] = &User{
		ID:       "u_known",
		Username: "known@corp.com",
		TenantID: "t_existing",
		Status:   userStatusActive,
		Roles:    []string{"approver"},
	}
	states := newMockStateStore()
	prov := &mockProvisioner{user: &User{ID: "u_should_not_use"}}
	h, sessions := buildOIDCHandler(provider, subjects, states, prov)

	state := seedState(t, states)
	rec := getOIDC(t, h, "/v1/admin/auth/oidc/callback?state="+url.QueryEscape(state)+"&code=good-code")
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302 (body: %s)", rec.Code, rec.Body.String())
	}
	if len(prov.records) != 0 {
		t.Fatal("provisioner must not run for an already-mapped subject")
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookies = %v, want the session cookie", cookies)
	}
	stored, err := sessions.Get(context.Background(), HashToken(cookies[0].Value))
	if err != nil {
		t.Fatalf("session missing: %v", err)
	}
	if stored.UserID != "u_known" || stored.TenantID != "t_existing" {
		t.Fatalf("stored session = %+v, want the existing user mapping", stored)
	}
	if len(stored.Roles) != 1 || stored.Roles[0] != "approver" {
		t.Fatalf("stored roles = %v, want [approver]", stored.Roles)
	}
}

func TestOIDCCallbackStateSingleUse(t *testing.T) {
	provider := &mockOIDCProvider{identity: &OIDCIdentity{Subject: "sub-1"}}
	subjects := newMockSubjectStore()
	subjects.bySubject["sub-1"] = &User{
		ID: "u_1", Username: "u", TenantID: "t1", Status: userStatusActive, Roles: []string{"tenant_admin"},
	}
	states := newMockStateStore()
	h, _ := buildOIDCHandler(provider, subjects, states, &mockProvisioner{})

	state := seedState(t, states)
	first := getOIDC(t, h, "/v1/admin/auth/oidc/callback?state="+url.QueryEscape(state)+"&code=good-code")
	if first.Code != http.StatusFound {
		t.Fatalf("first use status = %d, want 302 (body: %s)", first.Code, first.Body.String())
	}
	replay := getOIDC(t, h, "/v1/admin/auth/oidc/callback?state="+url.QueryEscape(state)+"&code=good-code")
	if replay.Code != http.StatusUnauthorized {
		t.Fatalf("replay status = %d, want 401 (body: %s)", replay.Code, replay.Body.String())
	}
}

func TestOIDCCallbackDisabledUser(t *testing.T) {
	provider := &mockOIDCProvider{identity: &OIDCIdentity{Subject: "sub-off"}}
	subjects := newMockSubjectStore()
	subjects.bySubject["sub-off"] = &User{
		ID: "u_off", Username: "off", TenantID: "t1", Status: "DISABLED", Roles: []string{"tenant_admin"},
	}
	states := newMockStateStore()
	h, sessions := buildOIDCHandler(provider, subjects, states, &mockProvisioner{})

	state := seedState(t, states)
	rec := getOIDC(t, h, "/v1/admin/auth/oidc/callback?state="+url.QueryEscape(state)+"&code=good-code")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (body: %s)", rec.Code, rec.Body.String())
	}
	if len(sessions.sessions) != 0 {
		t.Fatal("disabled user created a session")
	}
}

func TestOIDCCallbackUnknownSubjectWithoutProvisioner(t *testing.T) {
	provider := &mockOIDCProvider{identity: &OIDCIdentity{Subject: "sub-ghost"}}
	subjects := newMockSubjectStore()
	states := newMockStateStore()
	h, _ := buildOIDCHandler(provider, subjects, states, &mockProvisioner{})
	h.OIDC.Provisioner = nil

	state := seedState(t, states)
	rec := getOIDC(t, h, "/v1/admin/auth/oidc/callback?state="+url.QueryEscape(state)+"&code=good-code")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (body: %s)", rec.Code, rec.Body.String())
	}
}

func TestOIDCCallbackProvisionerError(t *testing.T) {
	provider := &mockOIDCProvider{identity: &OIDCIdentity{Subject: "sub-new"}}
	subjects := newMockSubjectStore()
	states := newMockStateStore()
	prov := &mockProvisioner{err: errors.New("pg down")}
	h, _ := buildOIDCHandler(provider, subjects, states, prov)

	state := seedState(t, states)
	rec := getOIDC(t, h, "/v1/admin/auth/oidc/callback?state="+url.QueryEscape(state)+"&code=good-code")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (body: %s)", rec.Code, rec.Body.String())
	}
}

func TestValkeyOIDCStateStore(t *testing.T) {
	rdb := newFakeValkey()
	store := NewValkeyOIDCStateStore(rdb)

	if err := store.Set(context.Background(), "st-1", OIDCStateTTL); err != nil {
		t.Fatalf("Set: %v", err)
	}
	ok, err := store.Consume(context.Background(), "st-1")
	if err != nil || !ok {
		t.Fatalf("Consume = %v, %v; want true, nil", ok, err)
	}
	// Single use: the second consume misses.
	ok, err = store.Consume(context.Background(), "st-1")
	if err != nil || ok {
		t.Fatalf("second Consume = %v, %v; want false, nil", ok, err)
	}
	// Unknown nonce: not an error, just absent.
	ok, err = store.Consume(context.Background(), "never-stored")
	if err != nil || ok {
		t.Fatalf("unknown Consume = %v, %v; want false, nil", ok, err)
	}
}

func seedState(t *testing.T, states *mockStateStore) string {
	t.Helper()
	state, err := NewToken()
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}
	if err := states.Set(context.Background(), state, OIDCStateTTL); err != nil {
		t.Fatalf("seed state: %v", err)
	}
	return state
}
