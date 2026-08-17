package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"adc.dev/ce/internal/adminauth"
)

// genericOIDCProvider implements adminauth.OIDCProvider against any
// RFC 8414-discoverable OIDC IdP (C4.1). It is intentionally small: the
// full SSO surface (PKCE, groups, logout) evolves in later releases.
type genericOIDCProvider struct {
	issuer       string
	clientID     string
	clientSecret string
	redirectURL  string
	httpClient   *http.Client

	authEndpoint  string
	tokenEndpoint string
}

func newGenericOIDCProvider(issuer, clientID, clientSecret, redirect string) *genericOIDCProvider {
	return &genericOIDCProvider{
		issuer:       strings.TrimRight(issuer, "/"),
		clientID:     clientID,
		clientSecret: clientSecret,
		redirectURL:  redirect,
		httpClient:   &http.Client{Timeout: 10 * time.Second},
	}
}

func (p *genericOIDCProvider) discover(ctx context.Context) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, p.issuer+"/.well-known/openid-configuration", nil)
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("oidc discovery: %d", resp.StatusCode)
	}
	var meta struct {
		AuthorizationEndpoint string `json:"authorization_endpoint"`
		TokenEndpoint         string `json:"token_endpoint"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&meta); err != nil {
		return err
	}
	if meta.AuthorizationEndpoint == "" || meta.TokenEndpoint == "" {
		return fmt.Errorf("oidc discovery: incomplete metadata")
	}
	p.authEndpoint = meta.AuthorizationEndpoint
	p.tokenEndpoint = meta.TokenEndpoint
	return nil
}

func (p *genericOIDCProvider) AuthorizationURL(state, redirect string) string {
	if p.authEndpoint == "" {
		if err := p.discover(context.Background()); err != nil {
			return ""
		}
	}
	q := url.Values{}
	q.Set("client_id", p.clientID)
	q.Set("response_type", "code")
	q.Set("scope", "openid email profile")
	q.Set("redirect_uri", p.redirectURL)
	q.Set("state", state)
	return p.authEndpoint + "?" + q.Encode()
}

func (p *genericOIDCProvider) ExchangeCode(ctx context.Context, code, redirect string) (*adminauth.OIDCIdentity, error) {
	if p.tokenEndpoint == "" {
		if err := p.discover(ctx); err != nil {
			return nil, err
		}
	}
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", p.redirectURL)
	form.Set("client_id", p.clientID)
	form.Set("client_secret", p.clientSecret)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, p.tokenEndpoint, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("oidc token: %d", resp.StatusCode)
	}
	var tok struct {
		IDToken string `json:"id_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&tok); err != nil {
		return nil, err
	}
	return parseIDTokenClaims(tok.IDToken)
}

// parseIDTokenClaims decodes the payload segment of a JWT without verifying
// the signature; signature verification happens against the IdP JWKS by the
// caller in production hardening (documented limitation of the V1 provider).
func parseIDTokenClaims(idToken string) (*adminauth.OIDCIdentity, error) {
	parts := strings.Split(idToken, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("oidc: malformed id_token")
	}
	payload, err := base64URLDecode(parts[1])
	if err != nil {
		return nil, err
	}
	var claims struct {
		Sub   string `json:"sub"`
		Email string `json:"email"`
		Name  string `json:"name"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, err
	}
	if claims.Sub == "" {
		return nil, fmt.Errorf("oidc: id_token missing sub")
	}
	return &adminauth.OIDCIdentity{Subject: claims.Sub, Email: claims.Email, Name: claims.Name}, nil
}

func base64URLDecode(s string) ([]byte, error) {
	s = strings.TrimRight(s, "=")
	s = strings.ReplaceAll(s, "-", "+")
	s = strings.ReplaceAll(s, "_", "/")
	return base64.StdEncoding.DecodeString(s)
}
