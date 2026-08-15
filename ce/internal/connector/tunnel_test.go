package connector

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"adc.dev/core-sdk/protocol"

	"github.com/gorilla/websocket"
)

// mockAuth implements DeviceAuth with injectable identity/error.
type mockAuth struct {
	mu     sync.Mutex
	ident  *DeviceIdentity
	err    error
	called int
}

func (m *mockAuth) Authenticate(_ context.Context, _ *Handshake) (*DeviceIdentity, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.called++
	if m.err != nil {
		return nil, m.err
	}
	return m.ident, nil
}

// mockRegistry implements SessionRegistry, recording every call.
type mockRegistry struct {
	mu           sync.Mutex
	registers    int
	lastNodeID   string
	lastTTL      time.Duration
	unregisters  int
	heartbeats   int
	failRegister error
}

func (m *mockRegistry) Register(_ context.Context, _, _ string, nodeID string, ttl time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.registers++
	m.lastNodeID = nodeID
	m.lastTTL = ttl
	if m.failRegister != nil {
		return m.failRegister
	}
	return nil
}

func (m *mockRegistry) Heartbeat(context.Context, string, string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.heartbeats++
	return nil
}

func (m *mockRegistry) Unregister(context.Context, string, string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.unregisters++
	return nil
}

func (m *mockRegistry) OnlineSet(context.Context, string, string, bool) error {
	return nil
}

// tunnelHarness wires a TunnelServer behind httptest with sane test defaults.
type tunnelHarness struct {
	srv   *httptest.Server
	auth  *mockAuth
	reg   *mockRegistry
	hub   *DeviceHub
	wsURL string
}

const testAllowedOrigin = "http://allowed.example"

func newTunnelHarness(t *testing.T, mods ...func(*TunnelServerConfig)) *tunnelHarness {
	t.Helper()
	auth := &mockAuth{ident: &DeviceIdentity{TenantID: "t1", DeviceCode: "dev-1"}}
	reg := &mockRegistry{}
	hub := NewDeviceHub()

	cfg := TunnelServerConfig{
		Auth:           auth,
		Hub:            hub,
		Registry:       reg,
		NodeID:         "node-1",
		DeviceTTL:      90 * time.Second,
		AllowedOrigins: []string{testAllowedOrigin},
		SyncTimeout:    2 * time.Second,
	}
	for _, m := range mods {
		m(&cfg)
	}

	srv := httptest.NewServer(NewTunnelServer(cfg))
	t.Cleanup(srv.Close)

	return &tunnelHarness{
		srv:   srv,
		auth:  auth,
		reg:   reg,
		hub:   hub,
		wsURL: "ws" + strings.TrimPrefix(srv.URL, "http"),
	}
}

// deviceHandshakeHeaders builds valid-looking SEC-03 handshake headers.
func deviceHandshakeHeaders(origin string) http.Header {
	h := http.Header{}
	h.Set(protocol.HeaderXDeviceID, "dev-1")
	h.Set(protocol.HeaderXDeviceTimestamp, strconv.FormatInt(time.Now().Unix(), 10))
	h.Set(protocol.HeaderXDeviceNonce, "nonce-abc-123")
	h.Set(protocol.HeaderXDeviceSignature, "sig")
	h.Set("Origin", origin)
	return h
}

// dialTunnel connects a device to the tunnel endpoint.
func dialTunnel(t *testing.T, wsURL, origin string) *websocket.Conn {
	t.Helper()
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, deviceHandshakeHeaders(origin))
	if err != nil {
		t.Fatalf("device dial: %v", err)
	}
	return conn
}

// serveDevice reads frames from the device side; it answers tools/list with
// the given tools and reports kick frames on kicked. It returns when the
// connection closes.
func serveDevice(t *testing.T, conn *websocket.Conn, tools []protocol.MCPTool, kicked chan<- struct{}) {
	t.Helper()
	defer func() { _ = conn.Close() }()
	for {
		_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
		_, msg, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var req protocol.JSONRPCRequest
		if err := json.Unmarshal(msg, &req); err != nil {
			continue
		}
		switch req.Method {
		case protocol.MethodToolsList:
			raw, err := json.Marshal(protocol.ToolsListResult{Tools: tools})
			if err != nil {
				continue
			}
			_ = conn.WriteJSON(protocol.JSONRPCResponse{
				JSONRPC: "2.0",
				ID:      req.ID,
				Result:  raw,
			})
		case methodKick:
			if kicked != nil {
				select {
				case kicked <- struct{}{}:
				default:
				}
			}
			return
		}
	}
}

// waitForSession polls the hub until the device is routable.
func waitForSession(t *testing.T, h *DeviceHub, tenant, device string) *DeviceSession {
	t.Helper()
	var s *DeviceSession
	waitFor(t, func() bool {
		got, err := h.GetDevice(tenant, device)
		if err != nil {
			return false
		}
		s = got
		return true
	}, 3*time.Second)
	return s
}

// TestParseHandshakeValid verifies header extraction.
func TestParseHandshakeValid(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/v1/devices/tunnel", nil)
	for k, v := range map[string]string{
		protocol.HeaderXDeviceID:        "dev-1",
		protocol.HeaderXDeviceTimestamp: "1700000000",
		protocol.HeaderXDeviceNonce:     "n1",
		protocol.HeaderXDeviceSignature: "s1",
	} {
		r.Header.Set(k, v)
	}

	hs, err := parseHandshake(r)
	if err != nil {
		t.Fatalf("parseHandshake: %v", err)
	}
	if hs.DeviceID != "dev-1" || hs.Timestamp != 1700000000 || hs.Nonce != "n1" || hs.Signature != "s1" {
		t.Fatalf("handshake = %+v", hs)
	}
}

// TestParseHandshakeRejectsMissingHeaders verifies incomplete handshakes are
// rejected before any authentication runs.
func TestParseHandshakeRejectsMissingHeaders(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/v1/devices/tunnel", nil)
	r.Header.Set(protocol.HeaderXDeviceID, "dev-1")
	if _, err := parseHandshake(r); err == nil {
		t.Fatal("missing headers accepted")
	}

	r.Header.Set(protocol.HeaderXDeviceTimestamp, "not-a-number")
	if _, err := parseHandshake(r); err == nil {
		t.Fatal("bad timestamp accepted")
	}
}

// TestOriginChecker verifies the SEC-04 whitelist semantics: requests without
// an Origin (device SDK clients) pass, whitelisted origins pass, anything
// else is rejected, and blank entries are ignored.
func TestOriginChecker(t *testing.T) {
	check := originChecker([]string{"http://allowed.example", " ", "https://b.example"})

	cases := []struct {
		name   string
		origin string
		want   bool
	}{
		{"no origin (non-browser client)", "", true},
		{"exact whitelist match", "http://allowed.example", true},
		{"second whitelist entry", "https://b.example", true},
		{"unknown origin", "http://evil.example", false},
		{"scheme variant", "https://allowed.example", false},
		{"path suffix", "http://allowed.example.evil.io", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			if c.origin != "" {
				r.Header.Set("Origin", c.origin)
			}
			if got := check(r); got != c.want {
				t.Fatalf("origin %q allowed = %v, want %v", c.origin, got, c.want)
			}
		})
	}
}

// TestServeHTTPRejectsBadHandshake verifies 401 without upgrade when headers
// are missing (SEC-03).
func TestServeHTTPRejectsBadHandshake(t *testing.T) {
	h := newTunnelHarness(t)

	req := httptest.NewRequest(http.MethodGet, "/v1/devices/tunnel", nil)
	rec := httptest.NewRecorder()
	h.srv.Config.Handler.(*TunnelServer).ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	var body struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Code != "device_auth_failed" {
		t.Fatalf("body = %s, want device_auth_failed", rec.Body.String())
	}
	if h.auth.called != 0 {
		t.Fatal("auth called for invalid handshake")
	}
}

// TestServeHTTPAuthFailure verifies auth failure returns 401 and no upgrade.
func TestServeHTTPAuthFailure(t *testing.T) {
	h := newTunnelHarness(t)
	h.auth.err = errors.New("bad signature")

	req := httptest.NewRequest(http.MethodGet, "/v1/devices/tunnel", nil)
	req.Header = deviceHandshakeHeaders(testAllowedOrigin)
	rec := httptest.NewRecorder()
	h.srv.Config.Handler.(*TunnelServer).ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if h.hub.Size() != 0 {
		t.Fatal("session registered despite auth failure")
	}
	if h.reg.registers != 0 {
		t.Fatal("registry written despite auth failure")
	}
}

// TestServeHTTPRejectsDisallowedOrigin verifies SEC-04 end-to-end: an Origin
// outside the whitelist gets 403 from the upgrader, no session, no registry
// write.
func TestServeHTTPRejectsDisallowedOrigin(t *testing.T) {
	h := newTunnelHarness(t)

	conn, resp, err := websocket.DefaultDialer.Dial(h.wsURL, deviceHandshakeHeaders("http://evil.example"))
	if err == nil {
		_ = conn.Close()
		t.Fatal("dial succeeded for disallowed origin")
	}
	if resp != nil && resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	if h.hub.Size() != 0 {
		t.Fatal("session registered for disallowed origin")
	}
	if h.reg.registers != 0 {
		t.Fatal("registry written for disallowed origin")
	}
}

// TestServeHTTPHappyPath verifies the full connection lifecycle: upgrade,
// hub register, registry register with nodeID/TTL, tools/list sync, kick-old
// on reconnect (SEC-15) and cleanup on disconnect.
func TestServeHTTPHappyPath(t *testing.T) {
	h := newTunnelHarness(t)
	tool := protocol.MCPTool{Name: "read_sensor", InputSchema: json.RawMessage(`{"type":"object"}`)}

	// First device connects.
	dev1 := dialTunnel(t, h.wsURL, testAllowedOrigin)
	kicked := make(chan struct{}, 1)
	go serveDevice(t, dev1, []protocol.MCPTool{tool}, kicked)

	s1 := waitForSession(t, h.hub, "t1", "dev-1")
	if s1 == nil {
		t.Fatal("first session not registered")
	}
	waitFor(t, func() bool {
		s, err := h.hub.GetDevice("t1", "dev-1")
		return err == nil && len(s.GetTools()) == 1
	}, 3*time.Second)

	h.reg.mu.Lock()
	registers := h.reg.registers
	nodeID := h.reg.lastNodeID
	ttl := h.reg.lastTTL
	h.reg.mu.Unlock()
	if registers != 1 || nodeID != "node-1" || ttl != 90*time.Second {
		t.Fatalf("registry register = %d node=%q ttl=%v, want 1 node-1 90s",
			registers, nodeID, ttl)
	}

	// Second connection with the same identity kicks the old one (SEC-15).
	dev2 := dialTunnel(t, h.wsURL, testAllowedOrigin)
	go serveDevice(t, dev2, nil, nil)

	select {
	case <-kicked:
	case <-time.After(3 * time.Second):
		t.Fatal("old connection never received the kick frame")
	}

	s2 := waitForSession(t, h.hub, "t1", "dev-1")
	if s2 == s1 {
		t.Fatal("hub still routes to the kicked session")
	}
	if s2.Gen() <= s1.Gen() {
		t.Fatalf("new gen = %d, old gen = %d, want new > old", s2.Gen(), s1.Gen())
	}
	if h.hub.Size() != 1 {
		t.Fatalf("hub size = %d, want 1", h.hub.Size())
	}

	// Disconnect cleanup: the new session's onClose removes the route and
	// unregisters from the registry exactly once.
	_ = dev2.Close()
	waitFor(t, func() bool { return h.hub.Size() == 0 }, 3*time.Second)
	h.reg.mu.Lock()
	unregs := h.reg.unregisters
	h.reg.mu.Unlock()
	if unregs != 1 {
		t.Fatalf("registry unregisters = %d, want 1 (old session must not double-unregister)", unregs)
	}
}

// TestServeHTTPRegistryFailureRefusesConnection verifies fail-closed registry
// registration (design/31 3.1.9): the upgrade happens, then the connection is
// torn down and no route survives.
func TestServeHTTPRegistryFailureRefusesConnection(t *testing.T) {
	h := newTunnelHarness(t)
	h.reg.failRegister = errors.New("valkey down")

	conn, _, err := websocket.DefaultDialer.Dial(h.wsURL, deviceHandshakeHeaders(testAllowedOrigin))
	if err != nil {
		t.Fatalf("upgrade should still succeed: %v", err)
	}
	defer conn.Close()

	// The server closes the connection right after the failed register.
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, _, err := conn.ReadMessage(); err == nil {
		t.Fatal("expected connection to be closed after registry failure")
	}
	waitFor(t, func() bool { return h.hub.Size() == 0 }, 3*time.Second)
}

// TestServeHTTPRegistryNilKeepsSession verifies a nil registry (unit-test /
// degraded config) does not break the connection and does not panic in the
// write pump (regression: the heartbeat closure used to dereference nil).
func TestServeHTTPRegistryNilKeepsSession(t *testing.T) {
	h := newTunnelHarness(t, func(cfg *TunnelServerConfig) { cfg.Registry = nil })

	dev1 := dialTunnel(t, h.wsURL, testAllowedOrigin)
	go serveDevice(t, dev1, nil, nil)

	s := waitForSession(t, h.hub, "t1", "dev-1")
	if s == nil || s.IsClosed() {
		t.Fatal("session not alive with nil registry")
	}
}

// TestServeHTTPToolsSyncFailureKeepsSession verifies a device that never
// answers tools/list is not disconnected (design/31 3.1.6: 失败不拆除会话).
func TestServeHTTPToolsSyncFailureKeepsSession(t *testing.T) {
	h := newTunnelHarness(t, func(cfg *TunnelServerConfig) { cfg.SyncTimeout = 300 * time.Millisecond })

	dev1 := dialTunnel(t, h.wsURL, testAllowedOrigin)
	defer dev1.Close()

	s := waitForSession(t, h.hub, "t1", "dev-1")
	time.Sleep(600 * time.Millisecond) // let the sync attempt time out
	if s.IsClosed() {
		t.Fatal("session closed after tools/list timeout")
	}
	if tools := s.GetTools(); len(tools) != 0 {
		t.Fatalf("tools = %v, want empty", tools)
	}
}
