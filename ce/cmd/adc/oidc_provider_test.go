package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
)

type oidcTestIssuer struct {
	server *httptest.Server
	key    *rsa.PrivateKey
	mu     sync.Mutex
	token  string
}

func newOIDCTestIssuer(t *testing.T) *oidcTestIssuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	i := &oidcTestIssuer{key: key}
	mux := http.NewServeMux()
	i.server = httptest.NewServer(mux)
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer": i.server.URL, "authorization_endpoint": i.server.URL + "/authorize",
			"token_endpoint": i.server.URL + "/token", "jwks_uri": i.server.URL + "/jwks",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "test-key", Algorithm: "RS256", Use: "sig"}}})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil || r.Form.Get("code_verifier") != "pkce-verifier" {
			http.Error(w, "bad verifier", http.StatusBadRequest)
			return
		}
		i.mu.Lock()
		defer i.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "access", "token_type": "Bearer", "id_token": i.token})
	})
	t.Cleanup(i.server.Close)
	return i
}

func (i *oidcTestIssuer) sign(t *testing.T, key *rsa.PrivateKey, audience any, azp string, expiry time.Time) string {
	t.Helper()
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: key, KeyID: "test-key"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	claims := map[string]any{
		"iss": i.server.URL, "sub": "subject-1", "aud": audience,
		"iat": time.Now().Add(-time.Minute).Unix(), "exp": expiry.Unix(), "nonce": "nonce-1",
	}
	if azp != "" {
		claims["azp"] = azp
	}
	payload, _ := json.Marshal(claims)
	jws, err := signer.Sign(payload)
	if err != nil {
		t.Fatal(err)
	}
	serialized, err := jws.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	return serialized
}

func TestGenericOIDCProviderVerifiesIDToken(t *testing.T) {
	issuer := newOIDCTestIssuer(t)
	provider, err := newGenericOIDCProvider(context.Background(), issuer.server.URL, "adc-client", "secret", "https://console.example/callback", "openid", true)
	if err != nil {
		t.Fatal(err)
	}
	valid := issuer.sign(t, issuer.key, "adc-client", "", time.Now().Add(time.Hour))
	otherKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	tests := []struct {
		name  string
		token string
		ok    bool
	}{
		{name: "valid", token: valid, ok: true},
		{name: "valid multiple audiences and azp", token: issuer.sign(t, issuer.key, []string{"adc-client", "other-client"}, "adc-client", time.Now().Add(time.Hour)), ok: true},
		{name: "multiple audiences missing azp", token: issuer.sign(t, issuer.key, []string{"adc-client", "other-client"}, "", time.Now().Add(time.Hour))},
		{name: "multiple audiences wrong azp", token: issuer.sign(t, issuer.key, []string{"adc-client", "other-client"}, "other-client", time.Now().Add(time.Hour))},
		{name: "forged signature", token: issuer.sign(t, otherKey, "adc-client", "", time.Now().Add(time.Hour))},
		{name: "expired", token: issuer.sign(t, issuer.key, "adc-client", "", time.Now().Add(-time.Hour))},
		{name: "wrong audience", token: issuer.sign(t, issuer.key, "other-client", "", time.Now().Add(time.Hour))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			issuer.mu.Lock()
			issuer.token = tt.token
			issuer.mu.Unlock()
			identity, err := provider.ExchangeCode(context.Background(), "code", "https://console.example/callback", "pkce-verifier")
			if tt.ok && (err != nil || identity.Subject != "subject-1" || identity.Nonce != "nonce-1" || len(identity.Audience) == 0) {
				t.Fatalf("valid exchange = %+v, %v", identity, err)
			}
			if !tt.ok && err == nil {
				t.Fatalf("invalid token was accepted: %+v", identity)
			}
		})
	}
}

func TestOIDCEndpointPolicy(t *testing.T) {
	for _, tc := range []struct {
		name, endpoint string
		allowLoopback  bool
		valid          bool
	}{
		{name: "production HTTPS", endpoint: "https://idp.example", valid: true},
		{name: "production HTTP", endpoint: "http://idp.example"},
		{name: "loopback HTTP disabled", endpoint: "http://127.0.0.1:8080"},
		{name: "loopback HTTP explicit", endpoint: "http://127.0.0.1:8080", allowLoopback: true, valid: true},
		{name: "link local HTTP", endpoint: "http://169.254.169.254", allowLoopback: true},
		{name: "link local HTTPS SSRF", endpoint: "https://169.254.169.254"},
		{name: "CGNAT HTTPS SSRF", endpoint: "https://100.100.100.200"},
		{name: "current network reserved", endpoint: "https://0.1.2.3"},
		{name: "protocol assignment reserved", endpoint: "https://192.0.0.1"},
		{name: "legal public HTTPS", endpoint: "https://100.63.255.255", valid: true},
		{name: "legal protocol assignment exception", endpoint: "https://192.0.0.9", valid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := validateOIDCEndpoint(tc.endpoint, tc.allowLoopback)
			if (err == nil) != tc.valid {
				t.Fatalf("validateOIDCEndpoint(%q) error = %v", tc.endpoint, err)
			}
		})
	}
}

func TestOIDCDiscoveryRejectsIssuerMismatchAndCrossOrigin(t *testing.T) {
	for _, metadata := range []func(string) map[string]any{
		func(origin string) map[string]any {
			return map[string]any{"issuer": origin + "/wrong", "authorization_endpoint": origin + "/auth", "token_endpoint": origin + "/token", "jwks_uri": origin + "/jwks", "id_token_signing_alg_values_supported": []string{"RS256"}}
		},
		func(origin string) map[string]any {
			return map[string]any{"issuer": origin, "authorization_endpoint": "https://evil.example/auth", "token_endpoint": origin + "/token", "jwks_uri": origin + "/jwks", "id_token_signing_alg_values_supported": []string{"RS256"}}
		},
	} {
		var server *httptest.Server
		server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(metadata(server.URL))
		}))
		_, err := newGenericOIDCProvider(context.Background(), server.URL, "client", "secret", "https://console.example/callback", "openid", true)
		server.Close()
		if err == nil || (!strings.Contains(err.Error(), "issuer mismatch") && !strings.Contains(err.Error(), "cross-origin")) {
			t.Fatalf("malicious discovery was not rejected: %v", err)
		}
	}
}
