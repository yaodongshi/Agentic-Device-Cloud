package gateway

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// echoBackend is a fake upstream that records every request it receives.
type echoBackend struct {
	mu   sync.Mutex
	seen []*http.Request
	srv  *httptest.Server
}

func newEchoBackend(t *testing.T, name string) *echoBackend {
	t.Helper()
	b := &echoBackend{}
	b.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		b.seen = append(b.seen, r.Clone(r.Context()))
		b.mu.Unlock()
		w.Header().Set("X-Backend", name)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true,"backend":"`+name+`"}`)
	}))
	t.Cleanup(b.srv.Close)
	return b
}

func (b *echoBackend) url() string { return b.srv.URL }

func (b *echoBackend) requests() []*http.Request {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]*http.Request, len(b.seen))
	copy(out, b.seen)
	return out
}

// single asserts the backend received exactly one request and returns it.
func (b *echoBackend) single(t *testing.T) *http.Request {
	t.Helper()
	reqs := b.requests()
	if len(reqs) != 1 {
		t.Fatalf("backend got %d requests, want exactly 1", len(reqs))
	}
	return reqs[0]
}

// newGatewayTest runs the assembled gateway handler on an httptest server.
// The access-layer error log is discarded to keep test output clean.
func newGatewayTest(t *testing.T, cfg Config) *httptest.Server {
	t.Helper()
	cfg.ErrorLog = log.New(io.Discard, "", 0)
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("gateway.New: %v", err)
	}
	ts := httptest.NewServer(srv.Handler)
	t.Cleanup(ts.Close)
	return ts
}

func newFixedBackend(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

type gatewayError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func getError(t *testing.T, url string) (*http.Response, gatewayError) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	var body gatewayError
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	return resp, body
}

func TestRoutesHitConfiguredBackends(t *testing.T) {
	admin := newEchoBackend(t, "admin")
	agent := newEchoBackend(t, "agent")
	gw := newGatewayTest(t, Config{Backends: Backends{Admin: admin.url(), AgentAPI: agent.url()}})

	resp, err := http.Get(gw.URL + "/v1/admin/tenants")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	got := admin.single(t)
	if got.URL.Path != "/v1/admin/tenants" {
		t.Errorf("admin backend saw path %q, want /v1/admin/tenants", got.URL.Path)
	}

	resp, err = http.Get(gw.URL + "/v1/agent/mcp/tools")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	got = agent.single(t)
	if got.URL.Path != "/v1/agent/mcp/tools" {
		t.Errorf("agent backend saw path %q, want /v1/agent/mcp/tools", got.URL.Path)
	}
}

func TestTunnelRouteHitsConnector(t *testing.T) {
	conn := newEchoBackend(t, "connector")
	gw := newGatewayTest(t, Config{Backends: Backends{Connector: conn.url()}})

	resp, err := http.Get(gw.URL + "/v1/devices/tunnel")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := conn.single(t).URL.Path; got != "/v1/devices/tunnel" {
		t.Errorf("connector saw path %q, want /v1/devices/tunnel", got)
	}
}

func TestUnmatchedRouteReturns404WithCode10001(t *testing.T) {
	gw := newGatewayTest(t, Config{Backends: Backends{Admin: "http://127.0.0.1:1"}})
	for _, path := range []string{"/v1/unknown", "/unknown", "/v1/adminx/users", "/v1/devices"} {
		resp, body := getError(t, gw.URL+path)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", path, resp.StatusCode)
		}
		if body.Code != "10001" {
			t.Errorf("%s: code = %q, want 10001", path, body.Code)
		}
		if body.Message == "" {
			t.Errorf("%s: empty message", path)
		}
		if strings.Contains(body.Message, "127.0.0.1") {
			t.Errorf("%s: message %q leaks an internal address", path, body.Message)
		}
	}
}

func TestAuthHeadersPassedThroughVerbatim(t *testing.T) {
	admin := newEchoBackend(t, "admin")
	gw := newGatewayTest(t, Config{Backends: Backends{Admin: admin.url()}})

	req, err := http.NewRequest(http.MethodGet, gw.URL+"/v1/admin/tenants", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer adc_live_abc123")
	req.Header.Set("X-ADC-Key", "adc_xyz_secret")
	req.Header.Set("X-Device-ID", "dev-001")
	req.Header.Set("X-Device-Model", "T-800")
	req.Header.Set("Cookie", "adc_session=session-1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	got := admin.single(t)
	for name, want := range map[string]string{
		"Authorization":  "Bearer adc_live_abc123",
		"X-ADC-Key":      "adc_xyz_secret",
		"X-Device-ID":    "dev-001",
		"X-Device-Model": "T-800",
		"Cookie":         "adc_session=session-1",
	} {
		if v := got.Header.Get(name); v != want {
			t.Errorf("backend received %s = %q, want %q verbatim", name, v, want)
		}
	}
}

func TestA2AApplicationCredentialPassedThroughVerbatim(t *testing.T) {
	py := newEchoBackend(t, "py-agent")
	gw := newGatewayTest(t, Config{Backends: Backends{PyAgent: py.url()}})

	req, err := http.NewRequest(http.MethodPost, gw.URL+"/v2/agents/a2a/tasks", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-ADC-Application-Credential", "adc_app_once")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	got := py.single(t)
	if value := got.Header.Get("X-ADC-Application-Credential"); value != "adc_app_once" {
		t.Fatalf("py-agent received application credential %q, want verbatim value", value)
	}
}

func TestTraceIDGeneratedAndPropagated(t *testing.T) {
	admin := newEchoBackend(t, "admin")
	gw := newGatewayTest(t, Config{Backends: Backends{Admin: admin.url()}})

	resp, err := http.Get(gw.URL + "/v1/admin/tenants")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	backendReq := admin.single(t)
	traceID := backendReq.Header.Get("X-Trace-ID")
	if len(traceID) != 32 {
		t.Fatalf("backend saw X-Trace-ID %q, want 32 hex chars", traceID)
	}
	requestID := backendReq.Header.Get("X-ADC-Request-ID")
	if len(requestID) != 32 {
		t.Fatalf("backend saw X-ADC-Request-ID %q, want 32 hex chars", requestID)
	}
	if got := resp.Header.Get("X-Trace-ID"); got != traceID {
		t.Errorf("response X-Trace-ID = %q, want %q (chain consistent)", got, traceID)
	}
	if got := resp.Header.Get("X-ADC-Request-ID"); got != requestID {
		t.Errorf("response X-ADC-Request-ID = %q, want %q", got, requestID)
	}
	if got := resp.Header.Get("X-ADC-Version"); got != "2026-08" {
		t.Errorf("response X-ADC-Version = %q, want 2026-08", got)
	}
}

func TestTraceIDPassthroughUnchanged(t *testing.T) {
	admin := newEchoBackend(t, "admin")
	gw := newGatewayTest(t, Config{Backends: Backends{Admin: admin.url()}})

	req, err := http.NewRequest(http.MethodGet, gw.URL+"/v1/admin/tenants", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Trace-ID", "trace-from-client")
	req.Header.Set("X-ADC-Request-ID", "req-from-client")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if got := admin.single(t).Header.Get("X-Trace-ID"); got != "trace-from-client" {
		t.Errorf("backend saw X-Trace-ID %q, want trace-from-client", got)
	}
	if got := resp.Header.Get("X-Trace-ID"); got != "trace-from-client" {
		t.Errorf("response X-Trace-ID = %q, want trace-from-client", got)
	}
	if got := resp.Header.Get("X-ADC-Request-ID"); got != "req-from-client" {
		t.Errorf("response X-ADC-Request-ID = %q, want req-from-client", got)
	}
}

func TestUpstreamUnreachableNormalizedTo502(t *testing.T) {
	gw := newGatewayTest(t, Config{Backends: Backends{Admin: "http://127.0.0.1:1"}})

	resp, body := getError(t, gw.URL+"/v1/admin/tenants")
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	if body.Code != "10007" {
		t.Errorf("code = %q, want 10007", body.Code)
	}
	if resp.Header.Get("X-ADC-Request-ID") == "" {
		t.Error("502 response missing X-ADC-Request-ID (must stay searchable, LLD 3.5.6)")
	}
	if resp.Header.Get("X-Trace-ID") == "" {
		t.Error("502 response missing X-Trace-ID")
	}
}

func TestUpstreamInternalErrorNormalized(t *testing.T) {
	leaky := newFixedBackend(t, http.StatusInternalServerError, `{"error":"boom at /var/app/secret.go:42"}`)
	gw := newGatewayTest(t, Config{Backends: Backends{Admin: leaky.URL}})

	resp, body := getError(t, gw.URL+"/v1/admin/tenants")
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (GW-006 normalization)", resp.StatusCode)
	}
	if body.Code != "10007" {
		t.Errorf("code = %q, want 10007", body.Code)
	}
	if strings.Contains(body.Message, "/var/app") {
		t.Errorf("message %q leaks backend internals", body.Message)
	}
}

func TestRateLimitedReturns429(t *testing.T) {
	admin := newEchoBackend(t, "admin")
	// The global entry bucket allows; the per-prefix bucket denies. This
	// verifies both scopes are consulted (LLD 3.5.2: entry + per-prefix).
	lim := &mockLimiter{allow: true, denyScopes: map[string]bool{"/v1/admin": true},
		info: RateLimitInfo{Limit: 100, Reset: time.Now().Add(5 * time.Second)}}
	gw := newGatewayTest(t, Config{Backends: Backends{Admin: admin.url()}, RateLimiter: lim})

	resp, body := getError(t, gw.URL+"/v1/admin/tenants")
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", resp.StatusCode)
	}
	if body.Code != "10006" {
		t.Errorf("code = %q, want 10006", body.Code)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Error("429 response missing Retry-After")
	}
	if resp.Header.Get("X-RateLimit-Limit") != "100" {
		t.Errorf("X-RateLimit-Limit = %q, want 100", resp.Header.Get("X-RateLimit-Limit"))
	}
	if resp.Header.Get("X-RateLimit-Remaining") != "0" {
		t.Errorf("X-RateLimit-Remaining = %q, want 0", resp.Header.Get("X-RateLimit-Remaining"))
	}
	if len(admin.requests()) != 0 {
		t.Error("rate-limited request reached the backend")
	}

	scopes := lim.scopes(t)
	if len(scopes) != 2 || scopes[0] != "global:127.0.0.1" || scopes[1] != "/v1/admin:127.0.0.1" {
		t.Errorf("limiter scopes = %v, want [global:127.0.0.1 /v1/admin:127.0.0.1]", scopes)
	}
}

func TestGlobalBucketDenialShortCircuits(t *testing.T) {
	admin := newEchoBackend(t, "admin")
	lim := &mockLimiter{allow: false, info: RateLimitInfo{Limit: 1000, Reset: time.Now().Add(time.Minute)}}
	gw := newGatewayTest(t, Config{Backends: Backends{Admin: admin.url()}, RateLimiter: lim})

	resp, _ := getError(t, gw.URL+"/v1/admin/tenants")
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", resp.StatusCode)
	}
	if len(admin.requests()) != 0 {
		t.Error("denied request reached the backend")
	}
	// A global denial must not even consult the per-prefix bucket.
	if scopes := lim.scopes(t); len(scopes) != 1 || scopes[0] != "global:127.0.0.1" {
		t.Errorf("limiter scopes = %v, want [global:127.0.0.1]", scopes)
	}
}

func TestPyAgentRouteReservedReturns503(t *testing.T) {
	gw := newGatewayTest(t, Config{Backends: Backends{Admin: "http://127.0.0.1:1"}}) // PyAgent empty

	resp, body := getError(t, gw.URL+"/v2/agents/eval/runs")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (reserved route)", resp.StatusCode)
	}
	if body.Code != "10007" {
		t.Errorf("code = %q, want 10007", body.Code)
	}
}

func TestPyAgentRouteForwardsWhenConfigured(t *testing.T) {
	py := newEchoBackend(t, "py")
	gw := newGatewayTest(t, Config{Backends: Backends{PyAgent: py.url()}})

	resp, err := http.Get(gw.URL + "/v2/agents/eval/runs")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := py.single(t).URL.Path; got != "/v2/agents/eval/runs" {
		t.Errorf("py backend saw path %q, want /v2/agents/eval/runs", got)
	}
}

func TestHealthzAggregatesBackends(t *testing.T) {
	admin := newEchoBackend(t, "admin")
	cfg := Config{Backends: Backends{Admin: admin.url(), Connector: "http://127.0.0.1:1"}}
	gw := newGatewayTest(t, cfg)

	resp, err := http.Get(gw.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (gateway liveness never fails on backend state)", resp.StatusCode)
	}
	var report struct {
		Status   string            `json:"status"`
		Version  string            `json:"version"`
		Backends map[string]string `json:"backends"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&report); err != nil {
		t.Fatalf("decode healthz body: %v", err)
	}
	if report.Status != "ok" {
		t.Errorf("status = %q, want ok", report.Status)
	}
	if report.Version != "2026-08" {
		t.Errorf("version = %q, want 2026-08", report.Version)
	}
	if report.Backends["admin"] != "ok" {
		t.Errorf("backends.admin = %q, want ok", report.Backends["admin"])
	}
	if report.Backends["connector"] != "unreachable" {
		t.Errorf("backends.connector = %q, want unreachable", report.Backends["connector"])
	}
}

func TestTunnelWebSocketUpgradePassesThrough(t *testing.T) {
	upgrader := websocket.Upgrader{}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			mt, msg, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if err := conn.WriteMessage(mt, msg); err != nil {
				return
			}
		}
	}))
	t.Cleanup(backend.Close)

	gw := newGatewayTest(t, Config{Backends: Backends{Connector: backend.URL}})
	wsURL := "ws" + strings.TrimPrefix(gw.URL, "http") + "/v1/devices/tunnel"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("ws dial through gateway: %v", err)
	}
	defer conn.Close()

	msg := `{"jsonrpc":"2.0","id":1,"method":"ping"}`
	if err := conn.WriteMessage(websocket.TextMessage, []byte(msg)); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, got, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != msg {
		t.Errorf("echoed message = %q, want %q", got, msg)
	}
}
