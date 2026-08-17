// OAuth 2.1 client credentials binding (doc/07 steps 2-3, ADR-N02). The
// platform acts as the OAuth client against the device-side authorization
// server: it discovers the protected-resource metadata
// (/.well-known/oauth-protected-resource, RFC 9728) from the device MCP
// endpoint, resolves the authorization server metadata (RFC 8414) and
// requests an access token with grant_type=client_credentials carrying
// the RFC8707 resource parameter (audience bound to the device). Tokens
// are short-lived and cached in memory only, never persisted (doc/07 2.2
// step 5/7 and NFR-004); refresh is a new client credentials request.

package mcpbinding

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// HTTPClient is the minimal HTTP surface shared by the discovery, token
// and tool-sync clients. *http.Client satisfies it; tests inject
// httptest servers' clients.
type HTTPClient interface {
	Do(req *http.Request) (*http.Response, error)
}

const (
	// wellKnownProtectedResourcePath is the RFC 9728 protected-resource
	// metadata path served by the device MCP server (doc/07 step 2).
	wellKnownProtectedResourcePath = "/.well-known/oauth-protected-resource"
	// wellKnownAuthorizationServerPath is the RFC 8414 authorization
	// server metadata path served at the authorization server issuer.
	wellKnownAuthorizationServerPath = "/.well-known/oauth-authorization-server"
	// defaultTokenExpirySec is the assumed lifetime when the token
	// response omits expires_in (RFC 6749 4.4.3 allows omission).
	defaultTokenExpirySec = 3600
	// refreshWindow is how early an expiring cached token triggers a
	// proactive refresh so a token never expires mid-request.
	refreshWindow = 30 * time.Second
	// maxMetaBytes caps metadata/token response bodies (SEC-19 style).
	maxMetaBytes = 1 << 20
)

// ProtectedResourceMetadata is the RFC 9728 metadata document.
type ProtectedResourceMetadata struct {
	Resource             string   `json:"resource"`
	AuthorizationServers []string `json:"authorization_servers"`
}

// AuthorizationServerMetadata is the subset of the RFC 8414 metadata
// document this binder needs.
type AuthorizationServerMetadata struct {
	Issuer        string `json:"issuer"`
	TokenEndpoint string `json:"token_endpoint"`
}

// DiscoveryResult carries the outcomes of doc/07 step 2.
type DiscoveryResult struct {
	// Resource is the RFC8707 resource identifier (the audience the
	// device AS expects on every token).
	Resource string
	// TokenEndpoint is the discovered RFC 8414 token endpoint.
	TokenEndpoint string
}

// DiscoveryClient resolves the device authorization server metadata.
type DiscoveryClient struct {
	HTTP HTTPClient
}

// NewDiscoveryClient builds a discovery client; a nil client falls back
// to http.DefaultClient.
func NewDiscoveryClient(hc HTTPClient) *DiscoveryClient {
	if hc == nil {
		hc = http.DefaultClient
	}
	return &DiscoveryClient{HTTP: hc}
}

// Discover implements doc/07 step 2: GET
// {mcpEndpoint}/.well-known/oauth-protected-resource, then GET
// {issuer}/.well-known/oauth-authorization-server. Failures are wrapped
// in ErrOAuthFailed so the admin API maps them uniformly.
func (d *DiscoveryClient) Discover(ctx context.Context, mcpEndpoint string) (*DiscoveryResult, error) {
	rsURL := strings.TrimRight(mcpEndpoint, "/") + wellKnownProtectedResourcePath
	var rs ProtectedResourceMetadata
	if err := d.getJSON(ctx, rsURL, &rs); err != nil {
		return nil, fmt.Errorf("%w: protected resource metadata: %v", ErrOAuthFailed, err)
	}
	if len(rs.AuthorizationServers) == 0 {
		return nil, fmt.Errorf("%w: protected resource metadata lists no authorization_servers", ErrOAuthFailed)
	}
	asURL := strings.TrimRight(rs.AuthorizationServers[0], "/") + wellKnownAuthorizationServerPath
	var asm AuthorizationServerMetadata
	if err := d.getJSON(ctx, asURL, &asm); err != nil {
		return nil, fmt.Errorf("%w: authorization server metadata: %v", ErrOAuthFailed, err)
	}
	if asm.TokenEndpoint == "" {
		return nil, fmt.Errorf("%w: authorization server metadata missing token_endpoint", ErrOAuthFailed)
	}
	return &DiscoveryResult{Resource: rs.Resource, TokenEndpoint: asm.TokenEndpoint}, nil
}

func (d *DiscoveryClient) getJSON(ctx context.Context, u string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := d.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("metadata endpoint %s answered %d", u, resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxMetaBytes)).Decode(dst); err != nil {
		return fmt.Errorf("decode metadata from %s: %w", u, err)
	}
	return nil
}

// OAuthConfig is the per-device token request configuration.
type OAuthConfig struct {
	ClientID      string
	ClientSecret  string
	TokenEndpoint string
	// Resource is the RFC8707 resource parameter (audience); omitted
	// when the device AS published no resource identifier.
	Resource string
	Scopes   []string
}

// AccessToken is one short-lived OAuth 2.1 access token. It exists in
// memory only: the cache holds it, PostgreSQL never sees it (doc/07 2.2,
// NFR-004).
type AccessToken struct {
	Value     string
	TokenType string
	ExpiresAt time.Time
	Scope     string
}

// TokenClient performs the client credentials grant (doc/07 step 3).
type TokenClient struct {
	HTTP HTTPClient
	Now  func() time.Time
}

// NewTokenClient builds a token client; a nil client falls back to
// http.DefaultClient.
func NewTokenClient(hc HTTPClient) *TokenClient {
	if hc == nil {
		hc = http.DefaultClient
	}
	return &TokenClient{HTTP: hc}
}

func (t *TokenClient) now() time.Time {
	if t.Now != nil {
		return t.Now()
	}
	return time.Now()
}

type tokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int    `json:"expires_in"`
	Scope       string `json:"scope"`
}

type tokenErrorResponse struct {
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// RequestToken posts the client credentials grant. Client authentication
// uses client_secret_basic (RFC 6749 2.3.1) and the resource parameter
// binds the audience per RFC8707 — the AS must verify it (doc/07: token
// passthrough forbidden, S4).
func (t *TokenClient) RequestToken(ctx context.Context, cfg OAuthConfig) (*AccessToken, error) {
	form := url.Values{}
	form.Set("grant_type", "client_credentials")
	if cfg.Resource != "" {
		form.Set("resource", cfg.Resource)
	}
	if len(cfg.Scopes) > 0 {
		form.Set("scope", strings.Join(cfg.Scopes, " "))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.SetBasicAuth(cfg.ClientID, cfg.ClientSecret)

	resp, err := t.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxMetaBytes))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		var oauthErr tokenErrorResponse
		if json.Unmarshal(body, &oauthErr) == nil && oauthErr.Error != "" {
			return nil, fmt.Errorf("token endpoint answered %d: %s: %s",
				resp.StatusCode, oauthErr.Error, oauthErr.ErrorDescription)
		}
		return nil, fmt.Errorf("token endpoint answered %d", resp.StatusCode)
	}
	var tr tokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return nil, fmt.Errorf("decode token response: %w", err)
	}
	if tr.AccessToken == "" {
		return nil, errors.New("token response missing access_token")
	}
	expiresIn := tr.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = defaultTokenExpirySec
	}
	if tr.TokenType == "" {
		tr.TokenType = "Bearer"
	}
	return &AccessToken{
		Value:     tr.AccessToken,
		TokenType: tr.TokenType,
		ExpiresAt: t.now().UTC().Add(time.Duration(expiresIn) * time.Second),
		Scope:     tr.Scope,
	}, nil
}

// TokenCache is the in-memory access token cache. It deliberately offers
// no eviction beyond overwrite: the token set is bounded by the number of
// class-A devices and every entry is small.
type TokenCache struct {
	mu      sync.Mutex
	entries map[string]*AccessToken
}

// NewTokenCache builds an empty cache.
func NewTokenCache() *TokenCache {
	return &TokenCache{entries: map[string]*AccessToken{}}
}

// Get returns a copy of the cached token for the key.
func (c *TokenCache) Get(key string) (*AccessToken, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.entries[key]
	if !ok {
		return nil, false
	}
	cp := *t
	return &cp, true
}

// Put stores the token under the key.
func (c *TokenCache) Put(key string, t *AccessToken) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cp := *t
	c.entries[key] = &cp
}

// BindResult is the outcome of a successful OAuth bind.
type BindResult struct {
	Resource      string
	TokenEndpoint string
	Token         *AccessToken
}

// oauthBinder ties discovery, token requests and the token cache
// together; it implements the OAuthBinder seam the service consumes.
type oauthBinder struct {
	Discovery *DiscoveryClient
	Tokens    *TokenClient
	Cache     *TokenCache
	Now       func() time.Time
}

// NewOAuthBinder wires a binder over one HTTP client.
func NewOAuthBinder(hc HTTPClient) OAuthBinder {
	if hc == nil {
		hc = http.DefaultClient
	}
	return &oauthBinder{
		Discovery: NewDiscoveryClient(hc),
		Tokens:    NewTokenClient(hc),
		Cache:     NewTokenCache(),
	}
}

func (b *oauthBinder) now() time.Time {
	if b.Now != nil {
		return b.Now()
	}
	return time.Now()
}

// Bind runs the full doc/07 step 2-3 sequence and caches the token keyed
// by the MCP endpoint.
func (b *oauthBinder) Bind(ctx context.Context, mcpEndpoint, clientID, clientSecret string) (*BindResult, error) {
	disc, err := b.Discovery.Discover(ctx, mcpEndpoint)
	if err != nil {
		return nil, err
	}
	tok, err := b.Tokens.RequestToken(ctx, OAuthConfig{
		ClientID:      clientID,
		ClientSecret:  clientSecret,
		TokenEndpoint: disc.TokenEndpoint,
		Resource:      disc.Resource,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrOAuthFailed, err)
	}
	res := &BindResult{Resource: disc.Resource, TokenEndpoint: disc.TokenEndpoint, Token: tok}
	if b.Cache != nil {
		b.Cache.Put(mcpEndpoint, tok)
	}
	return res, nil
}

// GetToken returns a live token: the cached one while it stays outside
// the refresh window, otherwise a freshly requested one (client
// credentials grant has no refresh token; refresh is a new grant,
// doc/07 2.2).
func (b *oauthBinder) GetToken(ctx context.Context, mcpEndpoint, clientID, clientSecret string) (*AccessToken, error) {
	if b.Cache != nil {
		if tok, ok := b.Cache.Get(mcpEndpoint); ok && b.now().UTC().Add(refreshWindow).Before(tok.ExpiresAt) {
			return tok, nil
		}
	}
	res, err := b.Bind(ctx, mcpEndpoint, clientID, clientSecret)
	if err != nil {
		return nil, err
	}
	return res.Token, nil
}
