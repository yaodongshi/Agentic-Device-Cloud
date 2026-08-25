package adminapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

type memApplicationCredential struct {
	id, hash, prefix string
	revokedAt        *time.Time
}

type memApplicationRepo struct {
	mu     sync.Mutex
	apps   map[string]*DeveloperApplication
	creds  map[string]*memApplicationCredential
	byName map[string]string
	next   int
	now    func() time.Time
}

func newMemApplicationRepo() *memApplicationRepo {
	return &memApplicationRepo{apps: map[string]*DeveloperApplication{}, creds: map[string]*memApplicationCredential{}, byName: map[string]string{}, now: time.Now}
}

func cloneApplication(a *DeveloperApplication) *DeveloperApplication {
	if a == nil {
		return nil
	}
	cp := *a
	cp.Scopes = append([]string(nil), a.Scopes...)
	return &cp
}

func (m *memApplicationRepo) Create(_ context.Context, n *NewDeveloperApplication) (*DeveloperApplication, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	nameKey := n.TenantID + "\x00" + stringsLower(n.Name)
	if _, ok := m.byName[nameKey]; ok {
		return nil, ErrApplicationConflict
	}
	m.next++
	id := uuidOf(700000 + m.next)
	a := &DeveloperApplication{ID: id, TenantID: n.TenantID, Name: n.Name, Purpose: n.Purpose, Status: applicationStatusActive,
		Scopes: append([]string(nil), n.Scopes...), CredentialID: n.CredentialID, CredentialPrefix: n.Prefix, CreatedAt: m.now(), UpdatedAt: m.now()}
	m.apps[id] = a
	m.creds[n.CredentialID] = &memApplicationCredential{id: n.CredentialID, hash: n.SecretHash, prefix: n.Prefix}
	m.byName[nameKey] = id
	return cloneApplication(a), nil
}

func stringsLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}

func (m *memApplicationRepo) Get(_ context.Context, id string) (*DeveloperApplication, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.apps[id]
	if !ok {
		return nil, ErrApplicationNotFound
	}
	return cloneApplication(a), nil
}
func (m *memApplicationRepo) List(_ context.Context, tenant string, p Page) ([]DeveloperApplication, int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var all []DeveloperApplication
	for _, a := range m.apps {
		if a.TenantID == tenant {
			all = append(all, *cloneApplication(a))
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].ID < all[j].ID })
	total := len(all)
	start := p.offset()
	if start > total {
		start = total
	}
	end := start + p.Size
	if end > total {
		end = total
	}
	return all[start:end], total, nil
}
func (m *memApplicationRepo) Disable(_ context.Context, id string) (*DeveloperApplication, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.apps[id]
	if !ok {
		return nil, ErrApplicationNotFound
	}
	a.Status = applicationStatusDisabled
	now := m.now()
	if c := m.creds[a.CredentialID]; c != nil {
		c.revokedAt = &now
		a.CredentialRevokedAt = &now
	}
	return cloneApplication(a), nil
}
func (m *memApplicationRepo) Delete(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.apps[id]
	if !ok {
		return ErrApplicationNotFound
	}
	delete(m.creds, a.CredentialID)
	delete(m.byName, a.TenantID+"\x00"+stringsLower(a.Name))
	delete(m.apps, id)
	return nil
}
func (m *memApplicationRepo) Revoke(_ context.Context, id string) (*DeveloperApplication, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.apps[id]
	if !ok {
		return nil, ErrApplicationNotFound
	}
	now := m.now()
	m.creds[a.CredentialID].revokedAt = &now
	a.CredentialRevokedAt = &now
	return cloneApplication(a), nil
}
func (m *memApplicationRepo) Rotate(_ context.Context, id string, n *NewApplicationCredential) (*DeveloperApplication, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.apps[id]
	if !ok {
		return nil, ErrApplicationNotFound
	}
	if a.Status != applicationStatusActive {
		return nil, ErrCredentialInvalid
	}
	now := m.now()
	m.creds[a.CredentialID].revokedAt = &now
	m.creds[n.ID] = &memApplicationCredential{id: n.ID, hash: n.SecretHash, prefix: n.Prefix}
	a.CredentialID = n.ID
	a.CredentialPrefix = n.Prefix
	a.CredentialRevokedAt = nil
	return cloneApplication(a), nil
}
func (m *memApplicationRepo) Authenticate(_ context.Context, raw, scope string) (*ApplicationPrincipal, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id, secret, ok := parseApplicationCredential(raw)
	if !ok {
		return nil, ErrCredentialInvalid
	}
	c, ok := m.creds[id]
	if !ok || c.revokedAt != nil {
		return nil, ErrCredentialInvalid
	}
	var app *DeveloperApplication
	for _, a := range m.apps {
		if a.CredentialID == id {
			app = a
			break
		}
	}
	if app == nil || app.Status != applicationStatusActive || subtle.ConstantTimeCompare([]byte(c.hash), []byte(hashApplicationSecret(secret))) != 1 {
		return nil, ErrCredentialInvalid
	}
	p := &ApplicationPrincipal{ApplicationID: app.ID, TenantID: app.TenantID, Scopes: append([]string(nil), app.Scopes...)}
	for _, s := range app.Scopes {
		if s == scope {
			return p, nil
		}
	}
	return p, ErrScopeForbidden
}

func wireApplications(env *testEnv) *memApplicationRepo {
	repo := newMemApplicationRepo()
	repo.now = func() time.Time { return fixedNow }
	env.srv.Applications = repo
	env.handler = env.srv.Handler()
	return repo
}

func createApplication(t *testing.T, env *testEnv, tok string, scopes []string) applicationResponse {
	t.Helper()
	rec := env.do(http.MethodPost, "/v1/admin/developer-applications", tok, map[string]any{"name": "scheduler", "purpose": "A2A integration", "scopes": scopes})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out applicationResponse
	env.decode(t, rec, &out)
	return out
}

func TestDeveloperApplicationLifecycleAndIsolation(t *testing.T) {
	env := newTestEnv(t)
	wireApplications(env)
	a := env.createTenant(t, "app-a", "App A", nil)
	b := env.createTenant(t, "app-b", "App B", nil)
	atok := env.token(t, []string{"tenant_admin"}, a.TenantID)
	btok := env.token(t, []string{"tenant_admin"}, b.TenantID)
	created := createApplication(t, env, atok, []string{ApplicationScopeTasksRead, ApplicationScopeTasksWrite})
	if created.Secret == "" {
		t.Fatal("create must return secret once")
	}
	_, secretPart, ok := parseApplicationCredential(created.Secret)
	if !ok {
		t.Fatal("created credential has invalid shape")
	}
	repo := env.srv.Applications.(*memApplicationRepo)
	stored := repo.creds[created.CredentialID]
	if stored.hash == secretPart || len(stored.hash) != sha256.Size*2 {
		t.Fatalf("repository did not retain only a SHA-256 hash: %q", stored.hash)
	}
	list := env.do(http.MethodGet, "/v1/admin/developer-applications", atok, nil)
	if list.Code != 200 || containsBody(list.Body.String(), created.Secret) {
		t.Fatalf("list leaked secret: %s", list.Body.String())
	}
	cross := env.do(http.MethodGet, "/v1/admin/developer-applications/"+created.ID, btok, nil)
	env.assertError(t, cross, http.StatusForbidden, codeCrossTenant)
	rot := env.do(http.MethodPost, "/v1/admin/developer-applications/"+created.ID+"/credentials/rotate", atok, nil)
	if rot.Code != http.StatusCreated {
		t.Fatalf("rotate: %s", rot.Body.String())
	}
	var rotated applicationResponse
	env.decode(t, rot, &rotated)
	if rec := connectivity(env, created.Secret); rec.Code != http.StatusUnauthorized {
		t.Fatalf("old credential after rotate=%d", rec.Code)
	}
	if rec := connectivity(env, rotated.Secret); rec.Code != http.StatusOK {
		t.Fatalf("new credential=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := env.do(http.MethodPost, "/v1/admin/developer-applications/"+created.ID+"/credentials/revoke", atok, nil); rec.Code != 200 {
		t.Fatalf("revoke=%d", rec.Code)
	}
	if rec := connectivity(env, rotated.Secret); rec.Code != http.StatusUnauthorized {
		t.Fatalf("revoked credential=%d", rec.Code)
	}
	if op, ok := env.audit.last(); !ok || op.Action != "developer_application.revoke" {
		t.Fatalf("missing audit: %+v", op)
	}
}

func TestDeveloperApplicationRBACScopeAndDisable(t *testing.T) {
	env := newTestEnv(t)
	wireApplications(env)
	tenant := env.createTenant(t, "scope", "Scope", nil)
	admin := env.token(t, []string{"tenant_admin"}, tenant.TenantID)
	if rec := env.do(http.MethodGet, "/v1/admin/developer-applications", env.token(t, []string{"auditor"}, tenant.TenantID), nil); rec.Code != http.StatusForbidden {
		t.Fatalf("auditor status=%d", rec.Code)
	}
	app := createApplication(t, env, admin, []string{ApplicationScopeTasksWrite})
	writeHandler := RequireApplicationScope(env.srv.Applications, ApplicationScopeTasksWrite, env.audit)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if principal, ok := ApplicationPrincipalFromContext(r.Context()); !ok || principal.ApplicationID != app.ID {
			t.Error("write middleware did not inject the application principal")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	writeReq := httptest.NewRequest(http.MethodPost, "/v2/agents/a2a/tasks", nil)
	writeReq.Header.Set("X-ADC-Application-Credential", app.Secret)
	writeRec := httptest.NewRecorder()
	writeHandler.ServeHTTP(writeRec, writeReq)
	if writeRec.Code != http.StatusNoContent {
		t.Fatalf("write scope status=%d body=%s", writeRec.Code, writeRec.Body.String())
	}
	if rec := connectivity(env, app.Secret); rec.Code != http.StatusForbidden {
		t.Fatalf("read with write-only=%d", rec.Code)
	}
	if op, ok := env.audit.last(); !ok || op.Action != "developer_application.scope_denied" {
		t.Fatalf("missing scope denial audit: %+v", op)
	}
	if rec := env.do(http.MethodPost, "/v1/admin/developer-applications/"+app.ID+"/disable", admin, nil); rec.Code != 200 {
		t.Fatalf("disable=%d", rec.Code)
	}
	if rec := connectivity(env, app.Secret); rec.Code != http.StatusUnauthorized {
		t.Fatalf("disabled credential=%d", rec.Code)
	}
}

func TestApplicationIntrospectionScopesAndRevocation(t *testing.T) {
	env := newTestEnv(t)
	wireApplications(env)
	tenant := env.createTenant(t, "introspection", "Introspection", nil)
	admin := env.token(t, []string{"tenant_admin"}, tenant.TenantID)
	readApp := createApplication(t, env, admin, []string{ApplicationScopeTasksRead})

	if rec := introspect(env, "", ApplicationScopeTasksRead); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := introspect(env, readApp.Secret, ApplicationScopeTasksWrite); rec.Code != http.StatusForbidden {
		t.Fatalf("write with read-only status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec := introspect(env, readApp.Secret, ApplicationScopeTasksRead)
	if rec.Code != http.StatusOK {
		t.Fatalf("read status=%d body=%s", rec.Code, rec.Body.String())
	}
	var principal map[string]any
	env.decode(t, rec, &principal)
	if principal["application_id"] != readApp.ID || principal["tenant_id"] != tenant.TenantID || principal["required_scope"] != ApplicationScopeTasksRead {
		t.Fatalf("unexpected principal: %#v", principal)
	}
	if rec := introspect(env, readApp.Secret, "admin:write"); rec.Code != http.StatusBadRequest {
		t.Fatalf("unsupported scope status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := env.do(http.MethodPost, "/v1/admin/developer-applications/"+readApp.ID+"/credentials/revoke", admin, nil); rec.Code != http.StatusOK {
		t.Fatalf("revoke status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := introspect(env, readApp.Secret, ApplicationScopeTasksRead); rec.Code != http.StatusUnauthorized {
		t.Fatalf("revoked status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestApplicationIntrospectionWriteScope(t *testing.T) {
	env := newTestEnv(t)
	wireApplications(env)
	tenant := env.createTenant(t, "introspection-write", "Introspection Write", nil)
	admin := env.token(t, []string{"tenant_admin"}, tenant.TenantID)
	app := createApplication(t, env, admin, []string{ApplicationScopeTasksWrite})
	if rec := introspect(env, app.Secret, ApplicationScopeTasksWrite); rec.Code != http.StatusOK {
		t.Fatalf("write status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := introspect(env, app.Secret, ApplicationScopeTasksRead); rec.Code != http.StatusForbidden {
		t.Fatalf("read with write-only status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func introspect(env *testEnv, secret, scope string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]string{"required_scope": scope})
	req := httptest.NewRequest(http.MethodPost, "/v1/developer/introspection", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if secret != "" {
		req.Header.Set("X-ADC-Application-Credential", secret)
	}
	rec := httptest.NewRecorder()
	env.handler.ServeHTTP(rec, req)
	return rec
}

func connectivity(env *testEnv, secret string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/v1/developer/connectivity", nil)
	req.Header.Set("X-ADC-Application-Credential", secret)
	rec := httptest.NewRecorder()
	env.handler.ServeHTTP(rec, req)
	return rec
}
func containsBody(body, value string) bool {
	return value != "" && len(body) >= len(value) && strings.Contains(body, value)
}
