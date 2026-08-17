// Orchestration layer tying the repository, the OAuth binder and the
// tool syncer into the doc/07 seven-step binding flow. The admin API
// handlers consume this seam; it owns the pairing token lifecycle
// (generate once, hash to the repository, verify and consume exactly
// once) and the KEK-encrypted OAuth client secret (NFR-004).

package mcpbinding

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"adc.dev/ce/internal/auth"
)

// OAuthBinder is the seam for the OAuth 2.1 client credentials binding.
// OAuthBinder (oauth.go) implements it.
type OAuthBinder interface {
	Bind(ctx context.Context, mcpEndpoint, clientID, clientSecret string) (*BindResult, error)
}

// Service orchestrates the binding lifecycle over its seams. All fields
// are exported so assemblies can swap fakes in tests.
type Service struct {
	Repo  BindingRepo
	OAuth OAuthBinder
	Sync  ToolSyncer
	KEK   []byte
	Now   func() time.Time
}

// NewService builds the binding service. KEK must be a 32-byte key
// (auth.LoadKEK output); a nil KEK fails Initiate/Complete closed.
func NewService(repo BindingRepo, oauth OAuthBinder, syncer ToolSyncer, kek []byte) *Service {
	return &Service{Repo: repo, OAuth: oauth, Sync: syncer, KEK: kek}
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// InitiateRequest is the pairing initiation payload (doc/07 step 1-2
// control-plane part).
type InitiateRequest struct {
	McpEndpoint  string
	ClientID     string
	ClientSecret string
	// TTL overrides the pairing token lifetime; zero means
	// DefaultBindingTokenTTL.
	TTL time.Duration
}

// ValidateEndpoint checks the MCP endpoint shape: absolute http(s) URL
// with a host (SEC-20 spirit; localhost/http allowed for lab devices).
func ValidateEndpoint(raw string) error {
	if raw == "" {
		return ErrEndpointInvalid
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ErrEndpointInvalid
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return ErrEndpointInvalid
	}
	return nil
}

// Initiate registers the MCP endpoint, stores the KEK-encrypted client
// secret and issues the pairing token: the plaintext is returned exactly
// once here (NFR-004), the repository keeps only its hash. The binding
// moves to BINDING.
func (s *Service) Initiate(ctx context.Context, deviceID string, req InitiateRequest) (*Binding, string, error) {
	if err := ValidateEndpoint(req.McpEndpoint); err != nil {
		return nil, "", err
	}
	if strings.TrimSpace(req.ClientID) == "" || req.ClientSecret == "" {
		return nil, "", errors.New("mcpbinding: oauth client_id and client_secret are required")
	}
	if len(s.KEK) != 32 {
		return nil, "", errors.New("mcpbinding: KEK not configured (fail closed)")
	}
	token, err := GenerateBindingToken()
	if err != nil {
		return nil, "", err
	}
	ttl := req.TTL
	if ttl <= 0 {
		ttl = DefaultBindingTokenTTL
	}
	enc, err := auth.EncryptSecret(req.ClientSecret, s.KEK)
	if err != nil {
		return nil, "", fmt.Errorf("mcpbinding: encrypt oauth client secret: %w", err)
	}
	b, err := s.Repo.RegisterEndpoint(ctx, deviceID, req.McpEndpoint, req.ClientID, enc, HashBindingToken(token), s.now().UTC().Add(ttl))
	if err != nil {
		return nil, "", err
	}
	return b, token, nil
}

// Complete consumes the pairing token (verified in constant time, then
// CAS-consumed in the repository so it can never be used twice), runs
// the OAuth 2.1 client credentials binding and syncs the tool catalog.
// A tool-sync failure does not roll back the binding: the returned
// binding is BOUND and the sync error travels in the third return value
// (the handler reports it as a warning, doc/07 step 5 degradation).
func (s *Service) Complete(ctx context.Context, deviceID, token string) (*Binding, int, error) {
	b, err := s.Repo.Get(ctx, deviceID)
	if err != nil {
		return nil, 0, err
	}
	if b.Status != StatusBinding {
		return nil, 0, fmt.Errorf("%w: binding status is %s", ErrStateConflict, b.Status)
	}
	want := HashBindingToken(token)
	if b.BindingTokenHash == "" || subtle.ConstantTimeCompare([]byte(b.BindingTokenHash), []byte(want)) != 1 {
		return nil, 0, ErrTokenInvalid
	}
	if b.BindingExpireAt == nil || !s.now().UTC().Before(*b.BindingExpireAt) {
		return nil, 0, ErrTokenExpired
	}
	secret, err := auth.DecryptSecret(b.OAuthClientSecretEnc, s.KEK)
	if err != nil {
		return nil, 0, fmt.Errorf("mcpbinding: decrypt oauth client secret: %w", err)
	}
	res, err := s.OAuth.Bind(ctx, b.McpEndpoint, b.OAuthClientID, secret)
	if err != nil {
		return nil, 0, err
	}
	now := s.now().UTC()
	bound, err := s.Repo.ConsumeTokenAndBind(ctx, deviceID, want, res.TokenEndpoint, res.Resource, now)
	if err != nil {
		return nil, 0, err
	}
	n, syncErr := s.Sync.Sync(ctx, deviceID, b.TenantID, b.McpEndpoint, res.Token.Value)
	return bound, n, syncErr
}

// Revoke unbinds the device: pairing token cleared, status REVOKED
// (doc/07 step 7, platform-side revoke).
func (s *Service) Revoke(ctx context.Context, deviceID string) (*Binding, error) {
	return s.Repo.Revoke(ctx, deviceID, s.now().UTC())
}

// Get loads the current binding state for the status endpoint.
func (s *Service) Get(ctx context.Context, deviceID string) (*Binding, error) {
	return s.Repo.Get(ctx, deviceID)
}
