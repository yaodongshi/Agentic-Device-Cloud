// Package gateway implements the unified ADC API gateway (ADR-19, M10):
// the single entry point every client-facing request flows through. It
// routes by path prefix to internal backend services on the mutual-trust
// network, forwards client credentials verbatim, injects trace ids and
// applies gateway-level rate limiting.
//
// The gateway is a pure forwarding and access layer (LLD 3.5.1): no
// business logic, no business state, no business audit events. Backends
// are reachable only inside the internal network (design/60).
package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"
	"strings"

	"adc.dev/ce/internal/httpx"
)

// Route is one entry of the gateway routing table (I21 GatewayRouter,
// LLD 3.5.3).
type Route struct {
	// Prefix is the URL path prefix matched by this route, e.g. "/v1/admin".
	// Matching is segment-aware: "/v1/admin" matches "/v1/admin/users" but
	// never "/v1/administrator".
	Prefix string
	// Backend is the upstream base URL on the internal mutual-trust network
	// (e.g. "http://admin-api:8080"). Empty marks a reserved route that is
	// registered but not enabled: requests match and are rejected with 503
	// (e.g. /v2/agents during V1.0, LLD 3.5.5 item 4).
	Backend string
	// WSS marks routes that carry WebSocket upgrades which must pass
	// through untouched (e.g. /v1/devices/tunnel, design/33 3.3).
	WSS bool
	// Contract names the OpenAPI document used for inbound validation
	// (single contract source, LLD 3.5.3). Reserved for the contract
	// middleware; not enforced in this skeleton.
	Contract string
}

// Table is the prefix routing table. Routes are kept sorted by prefix
// length (longest first) so the first match wins. Add fails fast on
// conflicting prefixes; the table must not be mutated while serving.
type Table struct {
	routes []Route
}

// NewTable returns an empty routing table.
func NewTable() *Table { return &Table{} }

// Add registers a route and fails fast on invalid or conflicting
// prefixes (LLD 3.5.6: conflicting registration must fail fast).
func (t *Table) Add(r Route) error {
	if !strings.HasPrefix(r.Prefix, "/") || r.Prefix == "/" || strings.HasSuffix(r.Prefix, "/") {
		return fmt.Errorf("gateway: invalid route prefix %q (must start with / and have no trailing slash)", r.Prefix)
	}
	for _, e := range t.routes {
		if e.Prefix == r.Prefix {
			return fmt.Errorf("gateway: conflicting route prefix %q (already registered)", r.Prefix)
		}
	}
	i := sort.Search(len(t.routes), func(i int) bool {
		return len(t.routes[i].Prefix) < len(r.Prefix)
	})
	t.routes = append(t.routes, Route{})
	copy(t.routes[i+1:], t.routes[i:])
	t.routes[i] = r
	return nil
}

// Match returns the route whose prefix matches path. Longest prefix wins
// and the boundary must fall on a "/" segment (segment-aware matching).
func (t *Table) Match(path string) (Route, bool) {
	for _, r := range t.routes {
		if segmentMatch(r.Prefix, path) {
			return r, true
		}
	}
	return Route{}, false
}

// ScopeFor resolves the routing scope of a request path for the rate
// limiter (per-prefix bucket, LLD 3.5.2). The empty string means the path
// matches no route (global entry bucket only).
func (t *Table) ScopeFor(path string) string {
	if r, ok := t.Match(path); ok {
		return r.Prefix
	}
	return ""
}

// segmentMatch reports whether path is under prefix at a segment boundary:
// "/v1/admin" matches "/v1/admin" and "/v1/admin/users" but never
// "/v1/administrator" or "/v1/adm".
func segmentMatch(prefix, path string) bool {
	if path == prefix {
		return true
	}
	return strings.HasPrefix(path, prefix) && strings.HasPrefix(path[len(prefix):], "/")
}

// compiled pairs a route with its ready-to-serve reverse proxy.
type compiled struct {
	Route
	proxy *httputil.ReverseProxy
}

// assemble builds the single-entry router handler: exact /healthz is
// delegated to the aggregated health handler; every other path is matched
// against the prefix table; unmatched paths get 404 code 10001 (openspec
// api-gateway scenario "未匹配路由返回标准错误", design/33 1.5).
func (t *Table) assemble(transport http.RoundTripper, health http.Handler, errorLog *log.Logger) (http.Handler, error) {
	routes := make([]compiled, 0, len(t.routes))
	for _, r := range t.routes {
		c := compiled{Route: r}
		if r.Backend != "" {
			u, err := url.Parse(r.Backend)
			if err != nil {
				return nil, fmt.Errorf("gateway: route %s: invalid backend %q: %w", r.Prefix, r.Backend, err)
			}
			c.proxy = newProxy(u, transport, errorLog)
		}
		routes = append(routes, c)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			health.ServeHTTP(w, r)
			return
		}
		for i := range routes {
			c := &routes[i]
			if !segmentMatch(c.Prefix, r.URL.Path) {
				continue
			}
			if c.proxy == nil {
				httpx.WriteError(w, http.StatusServiceUnavailable, "10007", "route reserved: backend not enabled", requestID(r))
				return
			}
			c.proxy.ServeHTTP(w, r)
			return
		}
		httpx.WriteError(w, http.StatusNotFound, "10001", "route not found", requestID(r))
	}), nil
}

// newProxy wraps httputil.ReverseProxy for one route (LLD 3.5.3 proxy.go):
// one-way Host rewrite to the backend internal name, verbatim credential
// forwarding, immediate flushing (WSS upgrade + streaming pass-through),
// 5xx normalization (GW-006) and error normalization to 502/504 with the
// X-ADC-Request-ID echoed.
func newProxy(backend *url.URL, transport http.RoundTripper, errorLog *log.Logger) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(backend)
			// One-way host rewrite: the outbound Host is always the
			// backend internal name; the client-supplied Host never
			// reaches the backend (LLD 3.5.4).
			pr.Out.Host = backend.Host
			if pr.In.Header.Get("X-Forwarded-Proto") == "" {
				proto := "http"
				if pr.In.TLS != nil {
					proto = "https"
				}
				pr.Out.Header.Set("X-Forwarded-Proto", proto)
			}
			applyAuthHeaders(pr.In.Context(), pr.Out.Header)
			// Re-assert trace identifiers so no transformation can drop
			// them (X-Trace-ID / X-ADC-Request-ID chain, LLD 3.5.1).
			if v := pr.In.Header.Get("X-Trace-ID"); v != "" {
				pr.Out.Header.Set("X-Trace-ID", v)
			}
			if v := pr.In.Header.Get("X-ADC-Request-ID"); v != "" {
				pr.Out.Header.Set("X-ADC-Request-ID", v)
			}
		},
		Transport:     transport,
		FlushInterval: -1, // flush immediately: WSS upgrade + streaming pass-through
		ModifyResponse: func(resp *http.Response) error {
			if resp.StatusCode >= 500 {
				// GW-006 error normalization: backend internal errors are
				// rewritten to the unified gateway error shape; no backend
				// stack or internal path leaks to clients.
				body := fmt.Sprintf(`{"code":"10007","message":"upstream error","trace_id":%q}`,
					resp.Request.Header.Get("X-ADC-Request-ID"))
				resp.StatusCode = http.StatusBadGateway
				resp.Status = http.StatusText(http.StatusBadGateway)
				resp.Header.Del("Content-Length")
				resp.Header.Set("Content-Type", "application/json")
				resp.Body = io.NopCloser(strings.NewReader(body))
				resp.ContentLength = int64(len(body))
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			status := http.StatusBadGateway
			msg := "upstream unreachable"
			if errors.Is(err, context.DeadlineExceeded) || isTimeoutError(err) {
				status = http.StatusGatewayTimeout
				msg = "upstream timeout"
			}
			if errorLog != nil {
				errorLog.Printf("gateway: upstream error on %s: %v", r.URL.Path, err)
			}
			httpx.WriteError(w, status, "10007", msg, requestID(r))
		},
	}
}

// isTimeoutError reports whether err is a timeout (net.Error semantics).
func isTimeoutError(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}
