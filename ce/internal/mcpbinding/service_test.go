package mcpbinding

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"adc.dev/ce/internal/auth"
)

// memBindingRepo mirrors the PGBindingRepo semantics (sentinels, CAS
// predicates, one-shot token consumption) so service tests run without a
// database.
type memBindingRepo struct {
	mu        sync.Mutex
	records   map[string]*Binding
	notClassA map[string]bool // device ids that exist but are class B
	nextID    int
}

func newMemBindingRepo() *memBindingRepo {
	return &memBindingRepo{records: map[string]*Binding{}, notClassA: map[string]bool{}}
}

func (r *memBindingRepo) seedClassA(deviceID, tenantID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextID++
	r.records[deviceID] = &Binding{
		DeviceID:    deviceID,
		TenantID:    tenantID,
		DeviceClass: "A",
		AuthType:    "oauth2_client_credentials",
		Status:      StatusRegistered,
		UpdatedAt:   time.Now().UTC(),
	}
}

func (r *memBindingRepo) seedClassB(deviceID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.notClassA[deviceID] = true
}

func (r *memBindingRepo) get(deviceID string) *Binding {
	b := r.records[deviceID]
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

func (r *memBindingRepo) RegisterEndpoint(_ context.Context, deviceID, mcpEndpoint, clientID, clientSecretEnc, tokenHash string, expireAt time.Time) (*Binding, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.notClassA[deviceID] {
		return nil, ErrNotClassA
	}
	b, ok := r.records[deviceID]
	if !ok {
		return nil, ErrBindingNotFound
	}
	if b.Status != StatusRegistered && b.Status != StatusRevoked {
		return nil, ErrStateConflict
	}
	b.McpEndpoint = mcpEndpoint
	b.AuthType = "oauth2_client_credentials"
	b.OAuthClientID = clientID
	b.OAuthClientSecretEnc = clientSecretEnc
	b.BindingTokenHash = tokenHash
	b.BindingExpireAt = &expireAt
	b.TokenEndpoint = ""
	b.ResourceIdentifier = ""
	b.BoundAt = nil
	b.RevokedAt = nil
	b.Status = StatusBinding
	b.UpdatedAt = time.Now().UTC()
	return r.get(deviceID), nil
}

func (r *memBindingRepo) Get(_ context.Context, deviceID string) (*Binding, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.notClassA[deviceID] {
		return nil, ErrNotClassA
	}
	if _, ok := r.records[deviceID]; !ok {
		return nil, ErrBindingNotFound
	}
	return r.get(deviceID), nil
}

func (r *memBindingRepo) ConsumeTokenAndBind(_ context.Context, deviceID, tokenHash, tokenEndpoint, resource string, now time.Time) (*Binding, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.notClassA[deviceID] {
		return nil, ErrNotClassA
	}
	b, ok := r.records[deviceID]
	if !ok {
		return nil, ErrBindingNotFound
	}
	switch b.Status {
	case StatusBinding:
		if b.BindingTokenHash == "" || b.BindingTokenHash != tokenHash {
			return nil, ErrTokenInvalid
		}
		if b.BindingExpireAt == nil || !now.Before(*b.BindingExpireAt) {
			return nil, ErrTokenExpired
		}
		b.BindingTokenHash = ""
		b.BindingExpireAt = nil
		b.TokenEndpoint = tokenEndpoint
		b.ResourceIdentifier = resource
		b.BoundAt = &now
		b.Status = StatusBound
		b.UpdatedAt = now
		return r.get(deviceID), nil
	case StatusBound:
		return nil, ErrTokenInvalid // replay: the token was consumed
	default:
		return nil, ErrStateConflict
	}
}

func (r *memBindingRepo) Revoke(_ context.Context, deviceID string, revokedAt time.Time) (*Binding, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.notClassA[deviceID] {
		return nil, ErrNotClassA
	}
	b, ok := r.records[deviceID]
	if !ok {
		return nil, ErrBindingNotFound
	}
	switch b.Status {
	case StatusRegistered, StatusBinding, StatusBound:
		b.Status = StatusRevoked
		b.BindingTokenHash = ""
		b.BindingExpireAt = nil
		b.RevokedAt = &revokedAt
		b.UpdatedAt = revokedAt
		return r.get(deviceID), nil
	default:
		return nil, ErrStateConflict
	}
}

// fakeOAuth implements the OAuthBinder seam with an in-memory behavior.
type fakeOAuth struct {
	mu         sync.Mutex
	result     *BindResult
	err        error
	calls      int
	lastID     string
	lastSecret string
}

func (f *fakeOAuth) Bind(_ context.Context, _, clientID, clientSecret string) (*BindResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.lastID = clientID
	f.lastSecret = clientSecret
	if f.err != nil {
		return nil, f.err
	}
	return f.result, nil
}

// fakeSyncer implements the ToolSyncer seam.
type fakeSyncer struct {
	mu         sync.Mutex
	n          int
	err        error
	calls      int
	lastToken  string
	lastDevice string
}

func (f *fakeSyncer) Sync(_ context.Context, deviceID, _, _, bearerToken string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.lastToken = bearerToken
	f.lastDevice = deviceID
	return f.n, f.err
}

const serviceDeviceID = "11111111-2222-3333-4444-555555555555"
const serviceTenantID = "99999999-8888-7777-6666-555555555555"

func newServiceFixture() (*Service, *memBindingRepo, *fakeOAuth, *fakeSyncer) {
	repo := newMemBindingRepo()
	repo.seedClassA(serviceDeviceID, serviceTenantID)
	oauth := &fakeOAuth{result: &BindResult{
		Resource:      "https://dev.example/mcp",
		TokenEndpoint: "https://as.example/token",
		Token:         &AccessToken{Value: "at-service", TokenType: "Bearer", ExpiresAt: time.Now().Add(time.Hour)},
	}}
	syncer := &fakeSyncer{n: 2}
	svc := NewService(repo, oauth, syncer, bytes32KEK())
	svc.Now = func() time.Time { return time.Date(2026, 8, 17, 8, 0, 0, 0, time.UTC) }
	return svc, repo, oauth, syncer
}

func bytes32KEK() []byte { return make([]byte, 32) }

func TestServiceInitiate(t *testing.T) {
	svc, _, _, _ := newServiceFixture()
	b, token, err := svc.Initiate(context.Background(), serviceDeviceID, InitiateRequest{
		McpEndpoint:  "https://dev.example/mcp",
		ClientID:     "platform-client",
		ClientSecret: "client-secret",
	})
	if err != nil {
		t.Fatalf("Initiate: %v", err)
	}
	if b.Status != StatusBinding || b.McpEndpoint != "https://dev.example/mcp" {
		t.Fatalf("binding = %+v", b)
	}
	// plaintext token exists exactly once; the repo keeps only the hash.
	if len(token) != 64 || b.BindingTokenHash == token {
		t.Fatalf("token/hash contract broken: %q %q", token, b.BindingTokenHash)
	}
	if b.BindingTokenHash != HashBindingToken(token) {
		t.Fatal("stored hash does not match the issued token")
	}
	if b.BindingExpireAt == nil || !b.BindingExpireAt.Equal(time.Date(2026, 8, 18, 8, 0, 0, 0, time.UTC)) {
		t.Fatalf("expire = %v, want default 24h TTL", b.BindingExpireAt)
	}
	// the client secret is KEK-encrypted at rest, never plaintext.
	if !strings.HasPrefix(b.OAuthClientSecretEnc, "encv1$") || strings.Contains(b.OAuthClientSecretEnc, "client-secret") {
		t.Fatalf("secret storage form = %q", b.OAuthClientSecretEnc)
	}
	plain, err := auth.DecryptSecret(b.OAuthClientSecretEnc, svc.KEK)
	if err != nil || plain != "client-secret" {
		t.Fatalf("decrypt secret = %q, %v", plain, err)
	}
	// second initiate while BINDING is a state conflict.
	if _, _, err := svc.Initiate(context.Background(), serviceDeviceID, InitiateRequest{
		McpEndpoint: "https://dev.example/mcp", ClientID: "c", ClientSecret: "s",
	}); !errors.Is(err, ErrStateConflict) {
		t.Fatalf("second initiate err = %v, want ErrStateConflict", err)
	}
}

func TestServiceInitiateValidation(t *testing.T) {
	svc, _, _, _ := newServiceFixture()
	badEndpoints := []string{"", "ftp://dev.example/mcp", "not a url", "://missing-scheme"}
	for _, ep := range badEndpoints {
		if _, _, err := svc.Initiate(context.Background(), serviceDeviceID, InitiateRequest{
			McpEndpoint: ep, ClientID: "c", ClientSecret: "s",
		}); !errors.Is(err, ErrEndpointInvalid) {
			t.Fatalf("endpoint %q: err = %v, want ErrEndpointInvalid", ep, err)
		}
	}
	if _, _, err := svc.Initiate(context.Background(), serviceDeviceID, InitiateRequest{
		McpEndpoint: "https://dev.example/mcp",
	}); err == nil {
		t.Fatal("missing client credentials must fail")
	}
	// missing KEK fails closed.
	svc.KEK = nil
	if _, _, err := svc.Initiate(context.Background(), serviceDeviceID, InitiateRequest{
		McpEndpoint: "https://dev.example/mcp", ClientID: "c", ClientSecret: "s",
	}); err == nil {
		t.Fatal("nil KEK must fail closed")
	}
}

func TestServiceCompleteHappyPath(t *testing.T) {
	svc, _, oauth, syncer := newServiceFixture()
	_, token, err := svc.Initiate(context.Background(), serviceDeviceID, InitiateRequest{
		McpEndpoint: "https://dev.example/mcp", ClientID: "platform-client", ClientSecret: "client-secret",
	})
	if err != nil {
		t.Fatalf("Initiate: %v", err)
	}
	b, n, syncErr := svc.Complete(context.Background(), serviceDeviceID, token)
	if syncErr != nil {
		t.Fatalf("sync error: %v", syncErr)
	}
	if b.Status != StatusBound || n != 2 {
		t.Fatalf("complete = %+v, n = %d", b, n)
	}
	if b.BindingTokenHash != "" || b.BindingExpireAt != nil {
		t.Fatalf("pairing token not consumed: %+v", b)
	}
	if b.TokenEndpoint != "https://as.example/token" || b.ResourceIdentifier != "https://dev.example/mcp" {
		t.Fatalf("discovery state missing: %+v", b)
	}
	// the decrypted secret reached the OAuth binder, the fresh access
	// token reached the syncer.
	if oauth.lastID != "platform-client" || oauth.lastSecret != "client-secret" {
		t.Fatalf("oauth credentials = %q/%q", oauth.lastID, oauth.lastSecret)
	}
	if syncer.lastToken != "at-service" || syncer.lastDevice != serviceDeviceID {
		t.Fatalf("syncer inputs = %q/%q", syncer.lastToken, syncer.lastDevice)
	}
	// replay: the same token cannot complete twice.
	if _, _, err := svc.Complete(context.Background(), serviceDeviceID, token); !errors.Is(err, ErrStateConflict) {
		t.Fatalf("replay err = %v, want ErrStateConflict (already BOUND)", err)
	}
}

func TestServiceCompleteWrongTokenNotConsumed(t *testing.T) {
	svc, _, _, _ := newServiceFixture()
	_, token, err := svc.Initiate(context.Background(), serviceDeviceID, InitiateRequest{
		McpEndpoint: "https://dev.example/mcp", ClientID: "c", ClientSecret: "s",
	})
	if err != nil {
		t.Fatalf("Initiate: %v", err)
	}
	wrong, _ := GenerateBindingToken()
	if wrong == token {
		t.Fatal("generated duplicate token (astronomically unlikely)")
	}
	if _, _, err := svc.Complete(context.Background(), serviceDeviceID, wrong); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("err = %v, want ErrTokenInvalid", err)
	}
	// the pairing survives a wrong token: the right one still works.
	b, _, err := svc.Complete(context.Background(), serviceDeviceID, token)
	if err != nil || b.Status != StatusBound {
		t.Fatalf("complete with the right token: %+v, %v", b, err)
	}
}

func TestServiceCompleteExpiredToken(t *testing.T) {
	svc, _, _, _ := newServiceFixture()
	_, token, err := svc.Initiate(context.Background(), serviceDeviceID, InitiateRequest{
		McpEndpoint: "https://dev.example/mcp", ClientID: "c", ClientSecret: "s",
		TTL: time.Minute,
	})
	if err != nil {
		t.Fatalf("Initiate: %v", err)
	}
	// advance the service clock past the TTL.
	svc.Now = func() time.Time { return time.Date(2026, 8, 17, 8, 10, 0, 0, time.UTC) }
	if _, _, err := svc.Complete(context.Background(), serviceDeviceID, token); !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("err = %v, want ErrTokenExpired", err)
	}
}

func TestServiceCompleteStateConflict(t *testing.T) {
	svc, _, _, _ := newServiceFixture()
	// no initiate: REGISTERED cannot complete.
	if _, _, err := svc.Complete(context.Background(), serviceDeviceID, "whatever"); !errors.Is(err, ErrStateConflict) {
		t.Fatalf("err = %v, want ErrStateConflict", err)
	}
}

func TestServiceCompleteOAuthFailureKeepsPairing(t *testing.T) {
	svc, _, oauth, _ := newServiceFixture()
	_, token, err := svc.Initiate(context.Background(), serviceDeviceID, InitiateRequest{
		McpEndpoint: "https://dev.example/mcp", ClientID: "c", ClientSecret: "s",
	})
	if err != nil {
		t.Fatalf("Initiate: %v", err)
	}
	oauth.err = ErrOAuthFailed
	if _, _, err := svc.Complete(context.Background(), serviceDeviceID, token); !errors.Is(err, ErrOAuthFailed) {
		t.Fatalf("err = %v, want ErrOAuthFailed", err)
	}
	// the pairing token was not consumed: retry with a healthy AS works.
	oauth.err = nil
	b, _, err := svc.Complete(context.Background(), serviceDeviceID, token)
	if err != nil || b.Status != StatusBound {
		t.Fatalf("retry complete: %+v, %v", b, err)
	}
}

func TestServiceCompleteSyncFailureKeepsBinding(t *testing.T) {
	svc, _, _, syncer := newServiceFixture()
	_, token, err := svc.Initiate(context.Background(), serviceDeviceID, InitiateRequest{
		McpEndpoint: "https://dev.example/mcp", ClientID: "c", ClientSecret: "s",
	})
	if err != nil {
		t.Fatalf("Initiate: %v", err)
	}
	syncer.err = errors.New("sync boom")
	b, _, syncErr := svc.Complete(context.Background(), serviceDeviceID, token)
	if syncErr == nil {
		t.Fatal("want sync error")
	}
	if b == nil || b.Status != StatusBound {
		t.Fatalf("binding must stay BOUND on sync failure: %+v", b)
	}
}

func TestServiceRevokeAndRebind(t *testing.T) {
	svc, _, _, _ := newServiceFixture()
	// revoke directly from REGISTERED.
	b, err := svc.Revoke(context.Background(), serviceDeviceID)
	if err != nil || b.Status != StatusRevoked {
		t.Fatalf("revoke = %+v, %v", b, err)
	}
	// double revoke is a state conflict.
	if _, err := svc.Revoke(context.Background(), serviceDeviceID); !errors.Is(err, ErrStateConflict) {
		t.Fatalf("double revoke err = %v", err)
	}
	// re-binding starts a fresh pairing (REVOKED -> BINDING).
	_, token, err := svc.Initiate(context.Background(), serviceDeviceID, InitiateRequest{
		McpEndpoint: "https://dev.example/mcp", ClientID: "c", ClientSecret: "s",
	})
	if err != nil {
		t.Fatalf("re-initiate: %v", err)
	}
	b, _, err = svc.Complete(context.Background(), serviceDeviceID, token)
	if err != nil || b.Status != StatusBound {
		t.Fatalf("re-complete: %+v, %v", b, err)
	}
	// revoke from BOUND clears the pairing remnants.
	b, err = svc.Revoke(context.Background(), serviceDeviceID)
	if err != nil || b.Status != StatusRevoked || b.BindingTokenHash != "" {
		t.Fatalf("final revoke = %+v, %v", b, err)
	}
}

func TestServiceNotClassA(t *testing.T) {
	svc, repo, _, _ := newServiceFixture()
	repo.seedClassB(serviceDeviceID)
	if _, _, err := svc.Initiate(context.Background(), serviceDeviceID, InitiateRequest{
		McpEndpoint: "https://dev.example/mcp", ClientID: "c", ClientSecret: "s",
	}); !errors.Is(err, ErrNotClassA) {
		t.Fatalf("initiate err = %v, want ErrNotClassA", err)
	}
	if _, err := svc.Get(context.Background(), serviceDeviceID); !errors.Is(err, ErrNotClassA) {
		t.Fatalf("get err = %v, want ErrNotClassA", err)
	}
}
