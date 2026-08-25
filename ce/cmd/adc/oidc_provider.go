package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	oidc "github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"adc.dev/ce/internal/adminauth"
)

var allowedOIDCSigningAlgorithms = []string{oidc.RS256}

type genericOIDCProvider struct {
	issuer      string
	clientID    string
	redirectURL string
	scope       []string
	oauth       oauth2.Config
	verifier    *oidc.IDTokenVerifier
	httpClient  *http.Client
}

type oidcDiscovery struct {
	Issuer                string   `json:"issuer"`
	AuthorizationEndpoint string   `json:"authorization_endpoint"`
	TokenEndpoint         string   `json:"token_endpoint"`
	JWKSURI               string   `json:"jwks_uri"`
	SigningAlgorithms     []string `json:"id_token_signing_alg_values_supported"`
}

func newGenericOIDCProvider(ctx context.Context, issuer, clientID, clientSecret, redirect, scope string, allowLoopbackHTTP bool) (*genericOIDCProvider, error) {
	issuer = strings.TrimRight(issuer, "/")
	if issuer == "" || clientID == "" || redirect == "" {
		return nil, errors.New("oidc: issuer, client id and redirect url are required")
	}
	issuerURL, err := validateOIDCEndpoint(issuer, allowLoopbackHTTP)
	if err != nil {
		return nil, fmt.Errorf("oidc issuer: %w", err)
	}
	if _, err := validateOIDCEndpoint(redirect, allowLoopbackHTTP); err != nil {
		return nil, fmt.Errorf("oidc redirect URL: %w", err)
	}
	httpClient := secureOIDCHTTPClient(issuerURL, allowLoopbackHTTP)
	ctx = oidc.ClientContext(ctx, httpClient)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, issuer+"/.well-known/openid-configuration", nil)
	if err != nil {
		return nil, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("oidc discovery: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("oidc discovery: status %d", resp.StatusCode)
	}
	var metadata oidcDiscovery
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&metadata); err != nil {
		return nil, fmt.Errorf("oidc discovery: %w", err)
	}
	if metadata.Issuer != issuer {
		return nil, errors.New("oidc discovery: issuer mismatch")
	}
	for name, endpoint := range map[string]string{
		"authorization_endpoint": metadata.AuthorizationEndpoint,
		"token_endpoint":         metadata.TokenEndpoint,
		"jwks_uri":               metadata.JWKSURI,
	} {
		endpointURL, err := validateOIDCEndpoint(endpoint, allowLoopbackHTTP)
		if err != nil {
			return nil, fmt.Errorf("oidc discovery %s: %w", name, err)
		}
		if !sameOrigin(issuerURL, endpointURL) {
			return nil, fmt.Errorf("oidc discovery %s: cross-origin endpoint rejected", name)
		}
	}
	if !containsAny(metadata.SigningAlgorithms, allowedOIDCSigningAlgorithms) {
		return nil, errors.New("oidc discovery: no allowed signing algorithm")
	}
	keySet := oidc.NewRemoteKeySet(ctx, metadata.JWKSURI)
	verifier := oidc.NewVerifier(issuer, keySet, &oidc.Config{ClientID: clientID, SupportedSigningAlgs: allowedOIDCSigningAlgorithms})
	scopes := strings.Fields(scope)
	if len(scopes) == 0 {
		scopes = []string{oidc.ScopeOpenID, "email", "profile"}
	}
	return &genericOIDCProvider{
		issuer: issuer, clientID: clientID, redirectURL: redirect, scope: scopes, verifier: verifier, httpClient: httpClient,
		oauth: oauth2.Config{ClientID: clientID, ClientSecret: clientSecret, RedirectURL: redirect, Endpoint: oauth2.Endpoint{AuthURL: metadata.AuthorizationEndpoint, TokenURL: metadata.TokenEndpoint}, Scopes: scopes},
	}, nil
}

func (p *genericOIDCProvider) AuthorizationURL(state, nonce, codeChallenge, redirect string) (string, error) {
	if redirect != p.redirectURL || state == "" || nonce == "" || codeChallenge == "" {
		return "", errors.New("oidc: invalid authorization transaction")
	}
	return p.oauth.AuthCodeURL(state,
		oauth2.SetAuthURLParam("nonce", nonce),
		oauth2.SetAuthURLParam("code_challenge", codeChallenge),
		oauth2.SetAuthURLParam("code_challenge_method", "S256")), nil
}

func (p *genericOIDCProvider) ExchangeCode(ctx context.Context, code, redirect, codeVerifier string) (*adminauth.OIDCIdentity, error) {
	if redirect != p.redirectURL || code == "" || codeVerifier == "" {
		return nil, errors.New("oidc: invalid code exchange")
	}
	ctx = oidc.ClientContext(ctx, p.httpClient)
	token, err := p.oauth.Exchange(ctx, code, oauth2.SetAuthURLParam("code_verifier", codeVerifier))
	if err != nil {
		return nil, fmt.Errorf("oidc token exchange: %w", err)
	}
	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok || rawIDToken == "" {
		return nil, errors.New("oidc: token response missing id_token")
	}
	idToken, err := p.verifier.Verify(ctx, rawIDToken)
	if err != nil {
		return nil, fmt.Errorf("oidc: verify id_token: %w", err)
	}
	var claims struct {
		Subject string `json:"sub"`
		AZP     string `json:"azp"`
		Email   string `json:"email"`
		Name    string `json:"name"`
		Nonce   string `json:"nonce"`
	}
	if err := idToken.Claims(&claims); err != nil || claims.Subject == "" {
		return nil, errors.New("oidc: invalid id_token claims")
	}
	if !containsAny(idToken.Audience, []string{p.clientID}) || (len(idToken.Audience) > 1 && claims.AZP != p.clientID) {
		return nil, errors.New("oidc: invalid id_token audience or authorized party")
	}
	return &adminauth.OIDCIdentity{Issuer: p.issuer, Subject: claims.Subject, Audience: append([]string(nil), idToken.Audience...), AZP: claims.AZP, Email: claims.Email, Name: claims.Name, Nonce: claims.Nonce}, nil
}

func validateOIDCEndpoint(raw string, allowLoopbackHTTP bool) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" {
		return nil, errors.New("invalid absolute URL")
	}
	host := u.Hostname()
	if ip := net.ParseIP(host); ip != nil && isForbiddenOIDCIP(ip) && !(u.Scheme == "http" && allowLoopbackHTTP && ip.IsLoopback()) {
		return nil, errors.New("private or local endpoint rejected")
	}
	if u.Scheme == "https" {
		return u, nil
	}
	if u.Scheme == "http" && allowLoopbackHTTP && isLoopbackHost(host) {
		return u, nil
	}
	return nil, errors.New("HTTPS is required; loopback HTTP requires ADC_OIDC_ALLOW_LOOPBACK_HTTP=true")
}

func secureOIDCHTTPClient(origin *url.URL, allowLoopback bool) *http.Client {
	dialer := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		for _, resolved := range ips {
			if isForbiddenOIDCIP(resolved.IP) && !(allowLoopback && resolved.IP.IsLoopback()) {
				return nil, errors.New("oidc: private or local endpoint rejected")
			}
		}
		return dialer.DialContext(ctx, network, net.JoinHostPort(ips[0].IP.String(), port))
	}
	return &http.Client{Transport: transport, Timeout: 10 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 3 || !sameOrigin(origin, req.URL) {
			return errors.New("oidc: cross-origin redirect rejected")
		}
		return nil
	}}
}

func sameOrigin(a, b *url.URL) bool {
	return strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(a.Host, b.Host)
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func isForbiddenOIDCIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() {
		return true
	}
	for _, cidr := range []string{
		"0.0.0.0/8",       // current network
		"100.64.0.0/10",   // shared address space (CGNAT)
		"192.0.0.0/29",    // IETF protocol assignments
		"192.0.0.170/31",  // NAT64/DNS64 discovery
		"192.0.2.0/24",    // documentation
		"198.18.0.0/15",   // benchmarking
		"198.51.100.0/24", // documentation
		"203.0.113.0/24",  // documentation
		"240.0.0.0/4",     // reserved
		"2001:db8::/32",   // documentation
	} {
		_, network, _ := net.ParseCIDR(cidr)
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

func containsAny(values, allowed []string) bool {
	for _, value := range values {
		for _, candidate := range allowed {
			if value == candidate {
				return true
			}
		}
	}
	return false
}
