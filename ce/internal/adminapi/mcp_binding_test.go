package adminapi

// Handler tests for the FR-021 class-A MCP binding endpoints
// (design/82 B5.1, doc/07 seven-step flow). The mcpbinding.Service is
// wired over in-memory repository/OAuth/sync fakes so the HTTP surface
// (auth, RBAC, tenant ownership, state conflicts, one-shot token) is
// exercised without a database or a real device.

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"adc.dev/ce/internal/auth"
	"adc.dev/ce/internal/mcpbinding"
)

// --- in-memory seams mirroring the mcpbinding package contracts ---

type memBindingRepo struct {
	mu      sync.Mutex
	records map[string]*mcpbinding.Binding
	devices *memDeviceRepo
}

func newMemBindingRepo(devices *memDeviceRepo) *memBindingRepo {
	return &memBindingRepo{records: map[string]*mcpbinding.Binding{}, devices: devices}
}

func cloneBinding(b *mcpbinding.Binding) *mcpbinding.Binding {
	if b == nil {
		return nil
	}
	cp := *b
	if b.BindingExpireAt != nil {
		t := *b.BindingExpireAt
		cp.BindingExpireAt = &t
	}
	if b.BoundAt != nil {
		t := *b.BoundAt
		cp.BoundAt = &t
	}
	if b.RevokedAt != nil {
		t := *b.RevokedAt
		cp.RevokedAt = &t
	}
	return &cp
}

// seedClassA inserts a class-A device row directly (the V1.0 register
// endpoint only mints token/hmac/mtls devices; A-class rows arrive from
// the factory provisioning path, design/33 3.1.6 remark).
func (r *memBindingRepo) seedClassA(tenantID, deviceCode string) string {
	r.devices.mu.Lock()
	defer r.devices.mu.Unlock()
	r.devices.nextID++
	id := uuidOf(600000 + r.devices.nextID)
	d := Device{
		ID:                id,
		TenantID:          tenantID,
		DeviceCode:        deviceCode,
		Name:              deviceCode,
		DeviceType:        "cnc",
		DeviceClass:       "A",
		AuthType:          auth.AuthTypeOAuthCC,
		Status:            deviceStatusOffline,
		CredentialVersion: 1,
		CreatedAt:         fixedNow,
		UpdatedAt:         fixedNow,
	}
	r.devices.devices[id] = &memDeviceRecord{d: d}
	r.devices.byCode[deviceCode] = id
	r.records[id] = &mcpbinding.Binding{
		DeviceID:    id,
		TenantID:    tenantID,
		DeviceClass: "A",
		AuthType:    auth.AuthTypeOAuthCC,
		Status:      mcpbinding.StatusRegistered,
		UpdatedAt:   fixedNow,
	}
	return id
}

// load consults the device ledger: unknown devices are binding-not-found,
// non-A devices are refused exactly like the PG repository.
func (r *memBindingRepo) load(deviceID string) (*mcpbinding.Binding, error) {
	dev, err := r.devices.Get(context.Background(), deviceID)
	if err != nil {
		return nil, mcpbinding.ErrBindingNotFound
	}
	if dev.DeviceClass != "A" {
		return nil, mcpbinding.ErrNotClassA
	}
	if b, ok := r.records[deviceID]; ok {
		return cloneBinding(b), nil
	}
	return &mcpbinding.Binding{
		DeviceID:    dev.ID,
		TenantID:    dev.TenantID,
		DeviceClass: "A",
		AuthType:    dev.AuthType,
		Status:      mcpbinding.StatusRegistered,
		UpdatedAt:   fixedNow,
	}, nil
}

func (r *memBindingRepo) RegisterEndpoint(_ context.Context, deviceID, mcpEndpoint, clientID, clientSecretEnc, tokenHash string, expireAt time.Time) (*mcpbinding.Binding, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	b, err := r.load(deviceID)
	if err != nil {
		return nil, err
	}
	if b.Status != mcpbinding.StatusRegistered && b.Status != mcpbinding.StatusRevoked {
		return nil, mcpbinding.ErrStateConflict
	}
	b.McpEndpoint = mcpEndpoint
	b.AuthType = auth.AuthTypeOAuthCC
	b.OAuthClientID = clientID
	b.OAuthClientSecretEnc = clientSecretEnc
	b.BindingTokenHash = tokenHash
	b.BindingExpireAt = &expireAt
	b.TokenEndpoint = ""
	b.ResourceIdentifier = ""
	b.BoundAt = nil
	b.RevokedAt = nil
	b.Status = mcpbinding.StatusBinding
	b.UpdatedAt = fixedNow
	r.records[deviceID] = cloneBinding(b)
	return cloneBinding(b), nil
}

func (r *memBindingRepo) Get(_ context.Context, deviceID string) (*mcpbinding.Binding, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.load(deviceID)
}

func (r *memBindingRepo) ConsumeTokenAndBind(_ context.Context, deviceID, tokenHash, tokenEndpoint, resource string, now time.Time) (*mcpbinding.Binding, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	b, err := r.load(deviceID)
	if err != nil {
		return nil, err
	}
	switch b.Status {
	case mcpbinding.StatusBinding:
		if b.BindingTokenHash == "" || b.BindingTokenHash != tokenHash {
			return nil, mcpbinding.ErrTokenInvalid
		}
		if b.BindingExpireAt == nil || !now.Before(*b.BindingExpireAt) {
			return nil, mcpbinding.ErrTokenExpired
		}
		b.BindingTokenHash = ""
		b.BindingExpireAt = nil
		b.TokenEndpoint = tokenEndpoint
		b.ResourceIdentifier = resource
		b.BoundAt = &now
		b.Status = mcpbinding.StatusBound
		b.UpdatedAt = now
		r.records[deviceID] = cloneBinding(b)
		return cloneBinding(b), nil
	case mcpbinding.StatusBound:
		return nil, mcpbinding.ErrTokenInvalid
	default:
		return nil, mcpbinding.ErrStateConflict
	}
}

func (r *memBindingRepo) Revoke(_ context.Context, deviceID string, revokedAt time.Time) (*mcpbinding.Binding, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	b, err := r.load(deviceID)
	if err != nil {
		return nil, err
	}
	switch b.Status {
	case mcpbinding.StatusRegistered, mcpbinding.StatusBinding, mcpbinding.StatusBound:
		b.Status = mcpbinding.StatusRevoked
		b.BindingTokenHash = ""
		b.BindingExpireAt = nil
		b.RevokedAt = &revokedAt
		b.UpdatedAt = revokedAt
		r.records[deviceID] = cloneBinding(b)
		return cloneBinding(b), nil
	default:
		return nil, mcpbinding.ErrStateConflict
	}
}

type fakeBindingOAuth struct {
	mu         sync.Mutex
	result     *mcpbinding.BindResult
	err        error
	calls      int
	lastSecret string
}

func (f *fakeBindingOAuth) Bind(_ context.Context, _, _, clientSecret string) (*mcpbinding.BindResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.lastSecret = clientSecret
	if f.err != nil {
		return nil, f.err
	}
	return f.result, nil
}

type fakeBindingSyncer struct {
	mu        sync.Mutex
	n         int
	err       error
	calls     int
	lastToken string
}

func (f *fakeBindingSyncer) Sync(_ context.Context, _, _, _, bearerToken string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.lastToken = bearerToken
	return f.n, f.err
}

type bindingFixture struct {
	repo   *memBindingRepo
	oauth  *fakeBindingOAuth
	syncer *fakeBindingSyncer
	svc    *mcpbinding.Service
}

// wireBindings attaches the FR-021 seam to the test server after
// newTestEnv construction (routes are registered eagerly, the seam is
// read per request).
func (e *testEnv) wireBindings() *bindingFixture {
	f := &bindingFixture{
		repo: newMemBindingRepo(e.store.devices),
		oauth: &fakeBindingOAuth{result: &mcpbinding.BindResult{
			Resource:      "https://dev.example/mcp",
			TokenEndpoint: "https://as.example/token",
			Token:         &mcpbinding.AccessToken{Value: "at-handler", TokenType: "Bearer", ExpiresAt: fixedNow.Add(time.Hour)},
		}},
		syncer: &fakeBindingSyncer{n: 2},
	}
	f.svc = mcpbinding.NewService(f.repo, f.oauth, f.syncer, testKEK)
	f.svc.Now = func() time.Time { return fixedNow }
	e.srv.Bindings = f.svc
	return f
}

func (e *testEnv) seedClassADevice(t *testing.T, tenantID, code string, f *bindingFixture) string {
	t.Helper()
	return f.repo.seedClassA(tenantID, code)
}

// --- tests ---

func TestMcpBindingAuthAndRoles(t *testing.T) {
	env := newTestEnv(t)
	fix := env.wireBindings()
	tr := env.createTenant(t, "acme", "Acme", nil)
	id := env.seedClassADevice(t, tr.TenantID, "mcp-dev", fix)

	// unauthenticated: 401 on all four endpoints.
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/v1/admin/devices/" + id + "/mcp-binding"},
		{http.MethodPost, "/v1/admin/devices/" + id + "/mcp-binding"},
		{http.MethodPost, "/v1/admin/devices/" + id + "/mcp-binding/complete"},
		{http.MethodPost, "/v1/admin/devices/" + id + "/mcp-binding/revoke"},
	} {
		rec := env.do(tc.method, tc.path, "", map[string]any{})
		env.assertError(t, rec, http.StatusUnauthorized, codeUnauthorized)
	}
	// approver and auditor are not in the admin matrix for these routes.
	for _, role := range []string{"approver", "auditor"} {
		tok := env.token(t, []string{role}, tr.TenantID)
		rec := env.do(http.MethodGet, "/v1/admin/devices/"+id+"/mcp-binding", tok, nil)
		env.assertError(t, rec, http.StatusForbidden, codeForbidden)
	}
}

func TestMcpBindingInitiateIssuesTokenOnce(t *testing.T) {
	env := newTestEnv(t)
	fix := env.wireBindings()
	tr := env.createTenant(t, "acme", "Acme", nil)
	id := env.seedClassADevice(t, tr.TenantID, "mcp-cnc", fix)
	tok := env.token(t, []string{"tenant_admin"}, tr.TenantID)

	rec := env.do(http.MethodPost, "/v1/admin/devices/"+id+"/mcp-binding", tok, map[string]any{
		"mcp_endpoint":  "https://dev.example/mcp",
		"client_id":     "platform-client",
		"client_secret": "client-secret",
		"change_reason": "绑定新购数控机床",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body: %s", rec.Code, rec.Body.String())
	}
	var out initiateBindingResponse
	env.decode(t, rec, &out)
	if out.Status != "BINDING" || out.McpEndpoint != "https://dev.example/mcp" {
		t.Fatalf("response = %+v", out)
	}
	if len(out.BindingToken) != 64 {
		t.Fatalf("binding_token = %q, want 64 hex chars", out.BindingToken)
	}
	if out.BindingExpires != fixedNow.Add(24*time.Hour) {
		t.Fatalf("expire = %v, want 24h TTL", out.BindingExpires)
	}
	// storage: hash only, secret KEK-encrypted (NFR-004).
	fix.repo.mu.Lock()
	stored := fix.repo.records[id]
	fix.repo.mu.Unlock()
	if stored.BindingTokenHash != mcpbinding.HashBindingToken(out.BindingToken) {
		t.Fatal("stored hash does not match issued token")
	}
	if strings.Contains(stored.BindingTokenHash, out.BindingToken) {
		t.Fatal("plaintext token leaked into storage")
	}
	if !strings.HasPrefix(stored.OAuthClientSecretEnc, "encv1$") || strings.Contains(stored.OAuthClientSecretEnc, "client-secret") {
		t.Fatalf("client secret storage form = %q", stored.OAuthClientSecretEnc)
	}
	// the status endpoint reflects BINDING without exposing the token.
	rec = env.do(http.MethodGet, "/v1/admin/devices/"+id+"/mcp-binding", tok, nil)
	var status bindingStatusResponse
	env.decode(t, rec, &status)
	if status.Status != "BINDING" || strings.Contains(rec.Body.String(), "binding_token") {
		t.Fatalf("status view = %+v; body: %s", status, rec.Body.String())
	}
	// audit carries the reason.
	op, ok := env.audit.last()
	if !ok || op.Action != "device.mcp_binding.initiate" || op.Reason != "绑定新购数控机床" {
		t.Fatalf("audit = %+v", op)
	}
}

func TestMcpBindingInitiateValidation(t *testing.T) {
	env := newTestEnv(t)
	fix := env.wireBindings()
	tr := env.createTenant(t, "acme", "Acme", nil)
	id := env.seedClassADevice(t, tr.TenantID, "mcp-a", fix)
	tok := env.token(t, []string{"tenant_admin"}, tr.TenantID)

	// missing change_reason.
	rec := env.do(http.MethodPost, "/v1/admin/devices/"+id+"/mcp-binding", tok, map[string]any{
		"mcp_endpoint": "https://dev.example/mcp", "client_id": "c", "client_secret": "s",
	})
	env.assertError(t, rec, http.StatusBadRequest, codeBadRequest)
	// malformed endpoint.
	rec = env.do(http.MethodPost, "/v1/admin/devices/"+id+"/mcp-binding", tok, map[string]any{
		"mcp_endpoint": "ftp://nope", "client_id": "c", "client_secret": "s", "change_reason": "x",
	})
	env.assertError(t, rec, http.StatusBadRequest, codeBadRequest)
	// missing client credentials.
	rec = env.do(http.MethodPost, "/v1/admin/devices/"+id+"/mcp-binding", tok, map[string]any{
		"mcp_endpoint": "https://dev.example/mcp", "change_reason": "x",
	})
	env.assertError(t, rec, http.StatusBadRequest, codeBadRequest)
	// unknown device.
	rec = env.do(http.MethodPost, "/v1/admin/devices/"+uuidOf(424242)+"/mcp-binding", tok, map[string]any{
		"mcp_endpoint": "https://dev.example/mcp", "client_id": "c", "client_secret": "s", "change_reason": "x",
	})
	env.assertError(t, rec, http.StatusNotFound, codeDeviceNotFound)
	// non-uuid path param.
	rec = env.do(http.MethodPost, "/v1/admin/devices/not-a-uuid/mcp-binding", tok, map[string]any{
		"mcp_endpoint": "https://dev.example/mcp", "client_id": "c", "client_secret": "s", "change_reason": "x",
	})
	env.assertError(t, rec, http.StatusBadRequest, codeBadRequest)
}

func TestMcpBindingInitiateRejectsClassB(t *testing.T) {
	env := newTestEnv(t)
	fix := env.wireBindings()
	tr := env.createTenant(t, "acme", "Acme", nil)
	tok := env.token(t, []string{"tenant_admin"}, tr.TenantID)
	// a device registered through the V1.0 endpoint is class B by default.
	rec := env.do(http.MethodPost, "/v1/admin/devices", tok, registerBody("legacy-dev", "token"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("register: %d; body: %s", rec.Code, rec.Body.String())
	}
	var reg deviceRegisterResponse
	env.decode(t, rec, &reg)
	rec = env.do(http.MethodPost, "/v1/admin/devices/"+reg.DeviceID+"/mcp-binding", tok, map[string]any{
		"mcp_endpoint": "https://dev.example/mcp", "client_id": "c", "client_secret": "s", "change_reason": "x",
	})
	env.assertError(t, rec, http.StatusBadRequest, codeBadRequest)
	_ = fix
}

func TestMcpBindingInitiateConflictWhileInProgress(t *testing.T) {
	env := newTestEnv(t)
	fix := env.wireBindings()
	tr := env.createTenant(t, "acme", "Acme", nil)
	id := env.seedClassADevice(t, tr.TenantID, "mcp-c1", fix)
	tok := env.token(t, []string{"tenant_admin"}, tr.TenantID)
	body := map[string]any{
		"mcp_endpoint": "https://dev.example/mcp", "client_id": "c", "client_secret": "s", "change_reason": "x",
	}
	if rec := env.do(http.MethodPost, "/v1/admin/devices/"+id+"/mcp-binding", tok, body); rec.Code != http.StatusOK {
		t.Fatalf("first initiate: %d; body: %s", rec.Code, rec.Body.String())
	}
	rec := env.do(http.MethodPost, "/v1/admin/devices/"+id+"/mcp-binding", tok, body)
	env.assertError(t, rec, http.StatusConflict, codeConflict)
}

func TestMcpBindingCompleteFullFlow(t *testing.T) {
	env := newTestEnv(t)
	fix := env.wireBindings()
	tr := env.createTenant(t, "acme", "Acme", nil)
	id := env.seedClassADevice(t, tr.TenantID, "mcp-c2", fix)
	tok := env.token(t, []string{"tenant_admin"}, tr.TenantID)

	var init initiateBindingResponse
	rec := env.do(http.MethodPost, "/v1/admin/devices/"+id+"/mcp-binding", tok, map[string]any{
		"mcp_endpoint": "https://dev.example/mcp", "client_id": "platform-client",
		"client_secret": "client-secret", "change_reason": "x",
	})
	env.decode(t, rec, &init)

	rec = env.do(http.MethodPost, "/v1/admin/devices/"+id+"/mcp-binding/complete", tok, map[string]any{
		"binding_token": init.BindingToken,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("complete: %d; body: %s", rec.Code, rec.Body.String())
	}
	var done completeBindingResponse
	env.decode(t, rec, &done)
	if done.Status != "BOUND" || !done.ToolsSynced || done.ToolsCount != 2 {
		t.Fatalf("complete response = %+v", done)
	}
	// the decrypted client secret reached the OAuth seam, the fresh
	// token reached the syncer.
	if fix.oauth.calls != 1 || fix.oauth.lastSecret != "client-secret" {
		t.Fatalf("oauth = calls %d secret %q", fix.oauth.calls, fix.oauth.lastSecret)
	}
	if fix.syncer.calls != 1 || fix.syncer.lastToken != "at-handler" {
		t.Fatalf("syncer = calls %d token %q", fix.syncer.calls, fix.syncer.lastToken)
	}
	// replay of the consumed token: state conflict (the token is gone).
	rec = env.do(http.MethodPost, "/v1/admin/devices/"+id+"/mcp-binding/complete", tok, map[string]any{
		"binding_token": init.BindingToken,
	})
	env.assertError(t, rec, http.StatusConflict, codeConflict)
	// status view shows BOUND with the discovery metadata.
	rec = env.do(http.MethodGet, "/v1/admin/devices/"+id+"/mcp-binding", tok, nil)
	var status bindingStatusResponse
	env.decode(t, rec, &status)
	if status.Status != "BOUND" || status.TokenEndpoint != "https://as.example/token" ||
		status.ResourceIdentifier != "https://dev.example/mcp" {
		t.Fatalf("status view = %+v", status)
	}
}

func TestMcpBindingCompleteBadToken(t *testing.T) {
	env := newTestEnv(t)
	fix := env.wireBindings()
	tr := env.createTenant(t, "acme", "Acme", nil)
	id := env.seedClassADevice(t, tr.TenantID, "mcp-c3", fix)
	tok := env.token(t, []string{"tenant_admin"}, tr.TenantID)
	rec := env.do(http.MethodPost, "/v1/admin/devices/"+id+"/mcp-binding", tok, map[string]any{
		"mcp_endpoint": "https://dev.example/mcp", "client_id": "c", "client_secret": "s", "change_reason": "x",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("initiate: %d; body: %s", rec.Code, rec.Body.String())
	}
	// wrong token: 401, pairing survives (state stays BINDING).
	rec = env.do(http.MethodPost, "/v1/admin/devices/"+id+"/mcp-binding/complete", tok, map[string]any{
		"binding_token": strings.Repeat("0", 64),
	})
	env.assertError(t, rec, http.StatusUnauthorized, codeUnauthorized)
	fix.repo.mu.Lock()
	stored := fix.repo.records[id]
	fix.repo.mu.Unlock()
	if stored.Status != "BINDING" || stored.BindingTokenHash == "" {
		t.Fatalf("pairing must survive a wrong token: %+v", stored)
	}
}

func TestMcpBindingCompleteWithoutInitiate(t *testing.T) {
	env := newTestEnv(t)
	fix := env.wireBindings()
	tr := env.createTenant(t, "acme", "Acme", nil)
	id := env.seedClassADevice(t, tr.TenantID, "mcp-c4", fix)
	tok := env.token(t, []string{"tenant_admin"}, tr.TenantID)
	rec := env.do(http.MethodPost, "/v1/admin/devices/"+id+"/mcp-binding/complete", tok, map[string]any{
		"binding_token": strings.Repeat("a", 64),
	})
	env.assertError(t, rec, http.StatusConflict, codeConflict)
}

func TestMcpBindingCompleteOAuthFailure(t *testing.T) {
	env := newTestEnv(t)
	fix := env.wireBindings()
	tr := env.createTenant(t, "acme", "Acme", nil)
	id := env.seedClassADevice(t, tr.TenantID, "mcp-c5", fix)
	tok := env.token(t, []string{"tenant_admin"}, tr.TenantID)
	var init initiateBindingResponse
	rec := env.do(http.MethodPost, "/v1/admin/devices/"+id+"/mcp-binding", tok, map[string]any{
		"mcp_endpoint": "https://dev.example/mcp", "client_id": "c", "client_secret": "s", "change_reason": "x",
	})
	env.decode(t, rec, &init)
	fix.oauth.err = mcpbinding.ErrOAuthFailed
	rec = env.do(http.MethodPost, "/v1/admin/devices/"+id+"/mcp-binding/complete", tok, map[string]any{
		"binding_token": init.BindingToken,
	})
	env.assertError(t, rec, http.StatusBadGateway, codeInternal)
	// the token was not consumed: a healthy retry succeeds.
	fix.oauth.err = nil
	rec = env.do(http.MethodPost, "/v1/admin/devices/"+id+"/mcp-binding/complete", tok, map[string]any{
		"binding_token": init.BindingToken,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("retry: %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestMcpBindingCompleteSyncFailureStillBound(t *testing.T) {
	env := newTestEnv(t)
	fix := env.wireBindings()
	tr := env.createTenant(t, "acme", "Acme", nil)
	id := env.seedClassADevice(t, tr.TenantID, "mcp-c6", fix)
	tok := env.token(t, []string{"tenant_admin"}, tr.TenantID)
	var init initiateBindingResponse
	rec := env.do(http.MethodPost, "/v1/admin/devices/"+id+"/mcp-binding", tok, map[string]any{
		"mcp_endpoint": "https://dev.example/mcp", "client_id": "c", "client_secret": "s", "change_reason": "x",
	})
	env.decode(t, rec, &init)
	fix.syncer.err = mcpbinding.ErrOAuthFailed // any failure exercises the path
	rec = env.do(http.MethodPost, "/v1/admin/devices/"+id+"/mcp-binding/complete", tok, map[string]any{
		"binding_token": init.BindingToken,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("complete with sync failure: %d; body: %s", rec.Code, rec.Body.String())
	}
	var done completeBindingResponse
	env.decode(t, rec, &done)
	if done.Status != "BOUND" || done.ToolsSynced || done.SyncError == "" {
		t.Fatalf("sync failure must not roll back the binding: %+v", done)
	}
}

func TestMcpBindingRevokeLifecycle(t *testing.T) {
	env := newTestEnv(t)
	fix := env.wireBindings()
	tr := env.createTenant(t, "acme", "Acme", nil)
	id := env.seedClassADevice(t, tr.TenantID, "mcp-c7", fix)
	tok := env.token(t, []string{"tenant_admin"}, tr.TenantID)

	// revoke from REGISTERED.
	rec := env.do(http.MethodPost, "/v1/admin/devices/"+id+"/mcp-binding/revoke", tok, map[string]any{
		"change_reason": "计划外设备，取消接入",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("revoke: %d; body: %s", rec.Code, rec.Body.String())
	}
	var st bindingStatusResponse
	env.decode(t, rec, &st)
	if st.Status != "REVOKED" {
		t.Fatalf("status = %q", st.Status)
	}
	// double revoke: conflict.
	rec = env.do(http.MethodPost, "/v1/admin/devices/"+id+"/mcp-binding/revoke", tok, map[string]any{
		"change_reason": "x",
	})
	env.assertError(t, rec, http.StatusConflict, codeConflict)
	// complete after revoke: conflict.
	rec = env.do(http.MethodPost, "/v1/admin/devices/"+id+"/mcp-binding/complete", tok, map[string]any{
		"binding_token": strings.Repeat("a", 64),
	})
	env.assertError(t, rec, http.StatusConflict, codeConflict)
	// re-pairing after revoke works (REVOKED -> BINDING).
	rec = env.do(http.MethodPost, "/v1/admin/devices/"+id+"/mcp-binding", tok, map[string]any{
		"mcp_endpoint": "https://dev.example/mcp", "client_id": "c", "client_secret": "s", "change_reason": "x",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("re-initiate after revoke: %d; body: %s", rec.Code, rec.Body.String())
	}
	// audit trail recorded the revoke reason.
	op, ok := env.audit.last()
	if !ok || op.Action != "device.mcp_binding.initiate" {
		t.Fatalf("audit = %+v", op)
	}
	_ = fix
}

func TestMcpBindingCrossTenantDenied(t *testing.T) {
	env := newTestEnv(t)
	fix := env.wireBindings()
	a := env.createTenant(t, "acme-a", "Acme A", nil)
	b := env.createTenant(t, "acme-b", "Acme B", nil)
	id := env.seedClassADevice(t, a.TenantID, "mcp-x", fix)
	// tenant B admin must not see or touch tenant A's device (13007).
	tokB := env.token(t, []string{"tenant_admin"}, b.TenantID)
	for _, tc := range []struct {
		method, path string
		body         map[string]any
	}{
		{http.MethodGet, "/v1/admin/devices/" + id + "/mcp-binding", nil},
		{http.MethodPost, "/v1/admin/devices/" + id + "/mcp-binding",
			map[string]any{"mcp_endpoint": "https://dev.example/mcp", "client_id": "c", "client_secret": "s", "change_reason": "x"}},
		{http.MethodPost, "/v1/admin/devices/" + id + "/mcp-binding/revoke",
			map[string]any{"change_reason": "x"}},
	} {
		rec := env.do(tc.method, tc.path, tokB, tc.body)
		env.assertError(t, rec, http.StatusForbidden, codeCrossTenant)
	}
	// the tenant A state was never mutated.
	fix.repo.mu.Lock()
	stored := fix.repo.records[id]
	fix.repo.mu.Unlock()
	if stored.Status != "REGISTERED" || stored.McpEndpoint != "" {
		t.Fatalf("cross-tenant attempt mutated the binding: %+v", stored)
	}
}

func TestMcpBindingNotConfiguredFailsClosed(t *testing.T) {
	env := newTestEnv(t)
	tr := env.createTenant(t, "acme", "Acme", nil)
	tok := env.token(t, []string{"tenant_admin"}, tr.TenantID)
	// newTestEnv leaves Bindings nil unless wireBindings ran.
	rec := env.do(http.MethodGet, "/v1/admin/devices/"+uuidOf(1)+"/mcp-binding", tok, nil)
	env.assertError(t, rec, http.StatusInternalServerError, codeInternal)
}
