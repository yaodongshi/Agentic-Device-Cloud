package mcpbinding

import (
	"context"
	"encoding/base64"
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

// deviceAS is a httptest-simulated device-side authorization server per
// doc/07: protected-resource metadata (RFC 9728) served from the MCP
// origin, RFC 8414 metadata at the AS issuer and a client credentials
// token endpoint validating Basic auth and the RFC8707 resource
// parameter.
type deviceAS struct {
	mu            sync.Mutex
	tokenCount    atomic.Int64
	lastBasic     string
	lastForm      url.Values
	accessToken   string
	expiresIn     int
	tokenStatus   int
	tokenError    string
	failProtected bool
	failAS        bool
	omitToken     bool
	server        *httptest.Server
	mux           *http.ServeMux
}

func newDeviceAS(t *testing.T) *deviceAS {
	t.Helper()
	as := &deviceAS{
		accessToken: "at-" + strings.Repeat("x", 32),
		expiresIn:   3600,
		tokenStatus: http.StatusOK,
		mux:         http.NewServeMux(),
	}
	// RFC 9728: the protected-resource metadata lives at
	// {resource}/.well-known/oauth-protected-resource; the MCP endpoint
	// is the resource, so the path nests under /mcp.
	as.mux.HandleFunc("/mcp/.well-known/oauth-protected-resource", func(w http.ResponseWriter, r *http.Request) {
		if as.failProtected {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(ProtectedResourceMetadata{
			Resource:             as.server.URL + "/mcp",
			AuthorizationServers: []string{as.server.URL},
		})
	})
	as.mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		if as.failAS {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(AuthorizationServerMetadata{
			Issuer:        as.server.URL,
			TokenEndpoint: as.server.URL + "/token",
		})
	})
	as.mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		as.tokenCount.Add(1)
		as.lastBasic = r.Header.Get("Authorization")
		if err := r.ParseForm(); err == nil {
			as.lastForm = r.Form
		}
		if as.tokenStatus != http.StatusOK {
			w.WriteHeader(as.tokenStatus)
			_ = json.NewEncoder(w).Encode(tokenErrorResponse{Error: as.tokenError, ErrorDescription: "test"})
			return
		}
		body := map[string]any{"access_token": as.accessToken, "token_type": "Bearer", "expires_in": as.expiresIn}
		if as.omitToken {
			delete(body, "access_token")
		}
		_ = json.NewEncoder(w).Encode(body)
	})
	as.server = httptest.NewServer(as.mux)
	t.Cleanup(as.server.Close)
	return as
}

func (as *deviceAS) endpoint() string { return as.server.URL + "/mcp" }

func TestOAuthBinderBindFullDiscovery(t *testing.T) {
	as := newDeviceAS(t)
	binder := NewOAuthBinder(as.server.Client()).(*oauthBinder)

	res, err := binder.Bind(context.Background(), as.endpoint(), "platform-client", "client-secret")
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if res.Resource != as.server.URL+"/mcp" {
		t.Fatalf("resource = %q", res.Resource)
	}
	if res.TokenEndpoint != as.server.URL+"/token" {
		t.Fatalf("token endpoint = %q", res.TokenEndpoint)
	}
	if res.Token == nil || res.Token.Value != as.accessToken || res.Token.TokenType != "Bearer" {
		t.Fatalf("token = %+v", res.Token)
	}
	if as.tokenCount.Load() != 1 {
		t.Fatalf("token requests = %d, want 1", as.tokenCount.Load())
	}
	// client_secret_basic auth header.
	if want := "Basic " + basicAuth("platform-client", "client-secret"); as.lastBasic != want {
		t.Fatalf("Authorization = %q, want %q", as.lastBasic, want)
	}
	// grant_type + RFC8707 resource parameter.
	if got := as.lastForm.Get("grant_type"); got != "client_credentials" {
		t.Fatalf("grant_type = %q", got)
	}
	if got := as.lastForm.Get("resource"); got != as.server.URL+"/mcp" {
		t.Fatalf("resource param = %q, want %q", got, as.server.URL+"/mcp")
	}
	// the token landed in the cache keyed by the MCP endpoint.
	if tok, ok := binder.Cache.Get(as.endpoint()); !ok || tok.Value != as.accessToken {
		t.Fatalf("cache = %+v, %v", tok, ok)
	}
}

func TestOAuthBinderDiscoverProtectedResourceFailure(t *testing.T) {
	as := newDeviceAS(t)
	as.failProtected = true
	binder := NewOAuthBinder(as.server.Client())
	_, err := binder.Bind(context.Background(), as.endpoint(), "c", "s")
	if !errors.Is(err, ErrOAuthFailed) {
		t.Fatalf("err = %v, want ErrOAuthFailed", err)
	}
}

func TestOAuthBinderDiscoverAuthorizationServerFailure(t *testing.T) {
	as := newDeviceAS(t)
	as.failAS = true
	binder := NewOAuthBinder(as.server.Client())
	_, err := binder.Bind(context.Background(), as.endpoint(), "c", "s")
	if !errors.Is(err, ErrOAuthFailed) {
		t.Fatalf("err = %v, want ErrOAuthFailed", err)
	}
}

func TestOAuthBinderTokenEndpointErrorPath(t *testing.T) {
	as := newDeviceAS(t)
	as.tokenStatus = http.StatusBadRequest
	as.tokenError = "invalid_client"
	binder := NewOAuthBinder(as.server.Client())
	_, err := binder.Bind(context.Background(), as.endpoint(), "c", "s")
	if err == nil || !strings.Contains(err.Error(), "invalid_client") {
		t.Fatalf("err = %v, want invalid_client detail", err)
	}
	if !errors.Is(err, ErrOAuthFailed) {
		t.Fatalf("err = %v, want ErrOAuthFailed", err)
	}
}

func TestOAuthBinderTokenResponseMissingAccessToken(t *testing.T) {
	as := newDeviceAS(t)
	as.omitToken = true
	binder := NewOAuthBinder(as.server.Client())
	_, err := binder.Bind(context.Background(), as.endpoint(), "c", "s")
	if err == nil || !strings.Contains(err.Error(), "access_token") {
		t.Fatalf("err = %v, want missing access_token error", err)
	}
}

func TestOAuthBinderCacheHitAndRefresh(t *testing.T) {
	as := newDeviceAS(t)
	binder := NewOAuthBinder(as.server.Client()).(*oauthBinder)
	now := time.Date(2026, 8, 17, 8, 0, 0, 0, time.UTC)
	binder.Now = func() time.Time { return now }
	binder.Tokens.Now = func() time.Time { return now }

	tok, err := binder.GetToken(context.Background(), as.endpoint(), "platform-client", "client-secret")
	if err != nil {
		t.Fatalf("GetToken: %v", err)
	}
	if as.tokenCount.Load() != 1 {
		t.Fatalf("token requests = %d, want 1", as.tokenCount.Load())
	}
	// cache hit: no new token request.
	tok2, err := binder.GetToken(context.Background(), as.endpoint(), "platform-client", "client-secret")
	if err != nil || tok2.Value != tok.Value {
		t.Fatalf("cached token = %+v, %v", tok2, err)
	}
	if as.tokenCount.Load() != 1 {
		t.Fatalf("token requests = %d, want 1 (cache hit)", as.tokenCount.Load())
	}
	// advance inside the refresh window: a fresh grant is issued.
	now = now.Add(time.Hour)
	if _, err := binder.GetToken(context.Background(), as.endpoint(), "platform-client", "client-secret"); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if as.tokenCount.Load() != 2 {
		t.Fatalf("token requests = %d, want 2 (refresh)", as.tokenCount.Load())
	}
}

func TestOAuthBinderDefaultsExpiryWhenExpiresInAbsent(t *testing.T) {
	as := newDeviceAS(t)
	as.expiresIn = 0
	binder := NewOAuthBinder(as.server.Client()).(*oauthBinder)
	now := time.Date(2026, 8, 17, 8, 0, 0, 0, time.UTC)
	binder.Tokens.Now = func() time.Time { return now }

	res, err := binder.Bind(context.Background(), as.endpoint(), "c", "s")
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if want := now.Add(defaultTokenExpirySec * time.Second); !res.Token.ExpiresAt.Equal(want) {
		t.Fatalf("expires at %v, want %v", res.Token.ExpiresAt, want)
	}
}

// basicAuth computes the RFC 7617 Basic authorization header value used
// to assert client_secret_basic authentication on the test AS.
func basicAuth(user, pass string) string {
	return base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
}
