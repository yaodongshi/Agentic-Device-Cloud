package gateway

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sync/singleflight"

	"adc.dev/ce/internal/httpx"
)

// Defaults (all overridable via Config; env injection via ConfigFromEnv).
const (
	defaultUpstreamTimeout    = 30 * time.Second // ADC_GW_UPSTREAM_TIMEOUT, LLD 3.5.4
	defaultHealthProbeTimeout = 2 * time.Second
	defaultReadHeaderTimeout  = 10 * time.Second
	defaultIdleTimeout        = 120 * time.Second
	defaultVersion            = "2026-08" // X-ADC-Version, design/33 1.8
)

// Backends maps path-prefix faces to upstream base URLs on the internal
// mutual-trust network (design/60: backends expose no public ports).
type Backends struct {
	// Admin serves the control plane (/v1/admin/* -> Admin API).
	Admin string
	// AgentAPI serves the agent plane (/v1/agent/* -> Agent API).
	AgentAPI string
	// Connector serves the device reverse tunnel (/v1/devices/tunnel, WSS).
	Connector string
	// Approval serves HITL callbacks (/v1/hitl/* -> Approval Service).
	Approval string
	// PyAgent serves the Python agent plane (/v2/agents/*). Empty keeps the
	// route reserved: requests return 503 until the plane is enabled
	// (LLD 3.5.5 item 4).
	PyAgent string
}

// Config assembles a gateway http.Server.
type Config struct {
	Backends Backends
	// Addr is the listen address handed to http.Server.Addr.
	Addr string
	// Version is the X-ADC-Version response header value (design/33 1.8).
	Version string
	// UpstreamTimeout bounds backend dial time and response-header wait
	// time (default 30s; must not be shorter than the device tool-call
	// timeout chain, SEC-16).
	UpstreamTimeout time.Duration
	// HealthProbeTimeout bounds each backend probe of the aggregated
	// /healthz (default 2s).
	HealthProbeTimeout time.Duration
	// RateLimiter is the optional gateway-level limiter (nil disables it).
	RateLimiter RateLimiter
	// ErrorLog receives gateway access-layer errors (upstream failures,
	// limiter infrastructure failures). Nil falls back to log.Default().
	ErrorLog *log.Logger

	// http.Server timeout fields, reused verbatim. ReadTimeout and
	// WriteTimeout default to 0 (disabled): the device tunnel route
	// carries long-lived WebSocket connections that any request-level
	// deadline would kill (LLD 3.5.4).
	ReadTimeout       time.Duration
	ReadHeaderTimeout time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
}

// New validates the configuration, assembles routes plus middleware and
// returns the ready-to-serve http.Server (route conflicts fail fast,
// LLD 3.5.6). The middleware chain is:
//
//	TraceID -> RateLimit -> PassAuth -> prefix router
func New(cfg Config) (*http.Server, error) {
	if cfg.UpstreamTimeout <= 0 {
		cfg.UpstreamTimeout = defaultUpstreamTimeout
	}
	if cfg.HealthProbeTimeout <= 0 {
		cfg.HealthProbeTimeout = defaultHealthProbeTimeout
	}
	if cfg.Version == "" {
		cfg.Version = defaultVersion
	}
	if cfg.ReadHeaderTimeout <= 0 {
		cfg.ReadHeaderTimeout = defaultReadHeaderTimeout
	}
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = defaultIdleTimeout
	}
	if cfg.ErrorLog == nil {
		cfg.ErrorLog = log.Default()
	}

	backends, err := cfg.Backends.parsed()
	if err != nil {
		return nil, err
	}
	table, err := buildTable(backends)
	if err != nil {
		return nil, err
	}
	transport := newTransport(cfg.UpstreamTimeout)
	health := newHealthHandler(backends, cfg.Version, cfg.HealthProbeTimeout)
	entry, err := table.assemble(transport, health, cfg.ErrorLog)
	if err != nil {
		return nil, err
	}
	var h http.Handler = entry
	h = PassAuth(h)
	h = RateLimit(cfg.RateLimiter, table.ScopeFor, cfg.ErrorLog)(h)
	h = TraceID(cfg.Version)(h)

	return &http.Server{
		Addr:              cfg.Addr,
		Handler:           h,
		ReadTimeout:       cfg.ReadTimeout,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
	}, nil
}

// ConfigFromEnv assembles a Config from ADC_GW_* environment variables
// (SEC-13: settings injected via env only, never hardcoded). Every value
// has a safe default, so a local gateway starts with zero configuration
// and serves 503 on routes whose backend is not configured.
func ConfigFromEnv() Config {
	get := func(key, def string) string {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			return v
		}
		return def
	}
	getSec := func(key string, def int) int {
		if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv(key))); err == nil && v > 0 {
			return v
		}
		return def
	}
	return Config{
		Backends: Backends{
			Admin:     get("ADC_GW_BACKEND_ADMIN", ""),
			AgentAPI:  get("ADC_GW_BACKEND_AGENT", ""),
			Connector: get("ADC_GW_BACKEND_CONNECTOR", ""),
			Approval:  get("ADC_GW_BACKEND_APPROVAL", ""),
			PyAgent:   get("ADC_GW_BACKEND_PYAGENT", ""),
		},
		Addr:               get("ADC_GW_ADDR", ":8080"),
		Version:            get("ADC_GW_VERSION", defaultVersion),
		UpstreamTimeout:    time.Duration(getSec("ADC_GW_UPSTREAM_TIMEOUT_SEC", 30)) * time.Second,
		HealthProbeTimeout: time.Duration(getSec("ADC_GW_HEALTH_PROBE_TIMEOUT_SEC", 2)) * time.Second,
	}
}

// buildTable registers the fixed V1.0 prefix table (LLD 3.5.1):
//
//	/v1/admin/*        -> Admin API
//	/v1/agent/*        -> Agent API
//	/v1/devices/tunnel -> Device Connector (WSS)
//	/v1/hitl/*         -> Approval Service
//	/v2/agents/*       -> Python agent plane (reserved)
//	/healthz           -> aggregated locally (assembled in Table.assemble)
func buildTable(backends map[string]*url.URL) (*Table, error) {
	t := NewTable()
	specs := []struct {
		prefix string
		key    string
		wss    bool
	}{
		{"/v1/admin", "admin", false},
		{"/v1/agent", "agentapi", false},
		{"/v1/devices/tunnel", "connector", true},
		{"/v1/hitl", "approval", false},
		{"/v2/agents", "pyagent", false},
		// A2A discovery endpoint (FR-022): the standard well-known location
		// must reach the agent plane through the unified entry.
		{"/.well-known", "pyagent", false},
	}
	for _, s := range specs {
		r := Route{Prefix: s.prefix, WSS: s.wss}
		if u := backends[s.key]; u != nil {
			r.Backend = u.String()
		}
		if err := t.Add(r); err != nil {
			return nil, err
		}
	}
	return t, nil
}

// parsed validates and parses the backend URLs (scheme and host required;
// failures surface at startup, not on the first request).
func (b Backends) parsed() (map[string]*url.URL, error) {
	raw := map[string]string{
		"admin":     b.Admin,
		"agentapi":  b.AgentAPI,
		"connector": b.Connector,
		"approval":  b.Approval,
		"pyagent":   b.PyAgent,
	}
	out := make(map[string]*url.URL, len(raw))
	for key, v := range raw {
		if v == "" {
			continue
		}
		u, err := url.Parse(v)
		if err != nil {
			return nil, fmt.Errorf("gateway: backend %s: %w", key, err)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return nil, fmt.Errorf("gateway: backend %s: unsupported scheme %q", key, u.Scheme)
		}
		if u.Host == "" {
			return nil, fmt.Errorf("gateway: backend %s: missing host in %q", key, v)
		}
		out[key] = u
	}
	return out, nil
}

// newTransport builds the shared Transport for all proxies: one connection
// pool with dial timeout and response-header timeout bound by
// UpstreamTimeout (LLD 3.5.4/3.5.6).
func newTransport(upstreamTimeout time.Duration) *http.Transport {
	d := &net.Dialer{Timeout: upstreamTimeout, KeepAlive: 30 * time.Second}
	return &http.Transport{
		DialContext:           d.DialContext,
		ResponseHeaderTimeout: upstreamTimeout,
		MaxIdleConns:          512,
		MaxIdleConnsPerHost:   128,
		IdleConnTimeout:       90 * time.Second,
		ForceAttemptHTTP2:     true,
	}
}

// healthHandler serves the aggregated /healthz (LLD 3.5.1 route table,
// design/33 endpoint 25): gateway liveness plus per-backend reachability.
// The gateway always answers 200 — a broken backend must not restart the
// gateway (k8s liveness semantics); dependency gating belongs to /readyz
// (design/60 6.4). Concurrent probes of the same backend are coalesced
// with singleflight so probe storms collapse into one upstream request.
type healthHandler struct {
	version  string
	backends map[string]*url.URL
	client   *http.Client
	sf       singleflight.Group
}

func newHealthHandler(backends map[string]*url.URL, version string, probeTimeout time.Duration) *healthHandler {
	return &healthHandler{
		version:  version,
		backends: backends,
		client:   &http.Client{Timeout: probeTimeout},
	}
}

func (h *healthHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	report := struct {
		Status   string            `json:"status"`
		Version  string            `json:"version"`
		Backends map[string]string `json:"backends"`
	}{Status: "ok", Version: h.version, Backends: make(map[string]string, len(h.backends))}
	for name, base := range h.backends {
		report.Backends[name] = h.probe(base)
	}
	httpx.WriteJSON(w, http.StatusOK, report)
}

// probe checks one backend's /healthz; probe errors are folded into the
// status string so the gateway liveness itself is never affected.
func (h *healthHandler) probe(base *url.URL) string {
	v, _, _ := h.sf.Do(base.String(), func() (interface{}, error) {
		req, err := http.NewRequest(http.MethodGet, base.String()+"/healthz", nil)
		if err != nil {
			return "unreachable", nil
		}
		resp, err := h.client.Do(req)
		if err != nil {
			return "unreachable", nil
		}
		resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return "unhealthy", nil
		}
		return "ok", nil
	})
	s, _ := v.(string)
	return s
}
