package connector

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"adc.dev/core-sdk/protocol"

	"github.com/gorilla/websocket"
)

// wsPair is a real WebSocket connection pair: the test acts as the device
// (dev), the server side runs a DeviceSession (sess) under a DeviceHub.
type wsPair struct {
	srv  *httptest.Server
	dev  *websocket.Conn
	sess *DeviceSession
	hub  *DeviceHub
}

// newWSPair starts an httptest WebSocket endpoint that upgrades the request,
// builds a DeviceSession (with opts applied before Run), registers it in the
// hub and runs it with the production-style gen-checked onClose.
func newWSPair(t *testing.T, opts ...func(*DeviceSession)) *wsPair {
	t.Helper()
	return newWSPairCtx(t, context.Background(), opts...)
}

func newWSPairCtx(t *testing.T, ctx context.Context, opts ...func(*DeviceSession)) *wsPair {
	t.Helper()
	upgrader := websocket.Upgrader{
		CheckOrigin:      func(*http.Request) bool { return true },
		WriteBufferSize:  4096,
		ReadBufferSize:   4096,
		HandshakeTimeout: 5 * time.Second,
	}
	hub := NewDeviceHub()
	sessCh := make(chan *DeviceSession, 1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		sess := NewDeviceSession("dev-1", "t1", conn)
		for _, o := range opts {
			o(sess)
		}
		hub.Register(sess)
		sessCh <- sess
		sess.Run(ctx, func(closed *DeviceSession) {
			hub.UnregisterIfCurrent(closed.TenantID, closed.DeviceID, closed.Gen())
		})
	}))
	t.Cleanup(srv.Close)

	dev, _, err := websocket.DefaultDialer.Dial(
		"ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("device dial: %v", err)
	}
	t.Cleanup(func() { _ = dev.Close() })

	sess := <-sessCh
	return &wsPair{srv: srv, dev: dev, sess: sess, hub: hub}
}

// waitFor polls cond until it is true or the timeout elapses.
func waitFor(t *testing.T, cond func() bool, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("condition not met within %v", timeout)
}

// TestSendRPCRoundTrip verifies the JSON-RPC request/response round trip over
// a real connection: method and params reach the device, the response is
// delivered back to the caller.
func TestSendRPCRoundTrip(t *testing.T) {
	p := newWSPair(t)
	defer p.dev.Close()

	reqCh := make(chan protocol.JSONRPCRequest, 1)
	go func() {
		_, msg, err := p.dev.ReadMessage()
		if err != nil {
			reqCh <- protocol.JSONRPCRequest{}
			return
		}
		var req protocol.JSONRPCRequest
		if err := json.Unmarshal(msg, &req); err != nil {
			reqCh <- protocol.JSONRPCRequest{}
			return
		}
		reqCh <- req
		_ = p.dev.WriteJSON(protocol.JSONRPCResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result:  json.RawMessage(`{"ok":true}`),
		})
	}()

	resp, err := p.sess.SendRPC(context.Background(), "tools/call",
		map[string]interface{}{"name": "read_sensor"})
	if err != nil {
		t.Fatalf("SendRPC: %v", err)
	}
	if string(resp.Result) != `{"ok":true}` {
		t.Fatalf("result = %s, want {\"ok\":true}", resp.Result)
	}

	select {
	case req := <-reqCh:
		if req.Method != "tools/call" {
			t.Errorf("method = %q, want tools/call", req.Method)
		}
		params, ok := req.Params.(map[string]interface{})
		if !ok || params["name"] != "read_sensor" {
			t.Errorf("params = %v, want name=read_sensor", req.Params)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("device never saw the request")
	}
}

// TestSendRPCErrorResponse verifies error responses are delivered, not
// swallowed.
func TestSendRPCErrorResponse(t *testing.T) {
	p := newWSPair(t)
	defer p.dev.Close()

	go func() {
		_, msg, err := p.dev.ReadMessage()
		if err != nil {
			return
		}
		var req protocol.JSONRPCRequest
		_ = json.Unmarshal(msg, &req)
		_ = p.dev.WriteJSON(protocol.JSONRPCResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Error:   &protocol.JSONRPCError{Code: -32601, Message: "unknown method"},
		})
	}()

	resp, err := p.sess.SendRPC(context.Background(), "nope", nil)
	if err != nil {
		t.Fatalf("SendRPC: %v", err)
	}
	if resp.Error == nil || resp.Error.Code != -32601 {
		t.Fatalf("error = %+v, want code -32601", resp.Error)
	}
}

// TestHeartbeatAck verifies the device heartbeat gets a heartbeat_ack
// notification (design/33 3.3).
func TestHeartbeatAck(t *testing.T) {
	p := newWSPair(t)
	defer p.dev.Close()

	if err := p.dev.WriteJSON(protocol.JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      "hb-1",
		Method:  methodHeartbeat,
	}); err != nil {
		t.Fatalf("device heartbeat write: %v", err)
	}

	_ = p.dev.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, msg, err := p.dev.ReadMessage()
	if err != nil {
		t.Fatalf("read heartbeat_ack: %v", err)
	}
	var ack protocol.JSONRPCRequest
	if err := json.Unmarshal(msg, &ack); err != nil {
		t.Fatalf("unmarshal ack: %v", err)
	}
	if ack.Method != methodHeartbeatAck {
		t.Fatalf("method = %q, want %q", ack.Method, methodHeartbeatAck)
	}
	if _, ok := ack.Params.(map[string]interface{})["server_time"]; !ok {
		t.Fatalf("ack params missing server_time: %v", ack.Params)
	}
}

// TestKickFrame verifies WriteJSON delivers a kick notification to the device
// (design/33 3.3 kick message).
func TestKickFrame(t *testing.T) {
	p := newWSPair(t)
	defer p.dev.Close()

	if err := p.sess.WriteJSON(protocol.JSONRPCRequest{
		JSONRPC: "2.0",
		Method:  methodKick,
		Params:  map[string]interface{}{"reason": "replaced_by_new_connection"},
	}); err != nil {
		t.Fatalf("WriteJSON kick: %v", err)
	}

	_ = p.dev.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, msg, err := p.dev.ReadMessage()
	if err != nil {
		t.Fatalf("read kick frame: %v", err)
	}
	var req protocol.JSONRPCRequest
	if err := json.Unmarshal(msg, &req); err != nil {
		t.Fatalf("unmarshal kick: %v", err)
	}
	if req.Method != methodKick {
		t.Fatalf("method = %q, want %q", req.Method, methodKick)
	}
}

// TestPendingTableDefaultCapacity verifies the production default of 256
// (design/31 3.1.4: 单会话 pending 表上限 256).
func TestPendingTableDefaultCapacity(t *testing.T) {
	p := newWSPair(t)
	defer p.dev.Close()
	if p.sess.maxPending != maxPendingRequests {
		t.Fatalf("maxPending = %d, want %d", p.sess.maxPending, maxPendingRequests)
	}
}

// TestSendRPCPendingTableFull verifies the bounded pending table: with
// capacity N and N+1 concurrent calls the device never answers, exactly one
// call is rejected with ErrPendingFull and the rest stay pending until the
// session closes.
func TestSendRPCPendingTableFull(t *testing.T) {
	p := newWSPair(t, func(s *DeviceSession) { s.maxPending = 4 })

	const calls = 5
	errs := make(chan error, calls)
	for i := 0; i < calls; i++ {
		go func() {
			_, err := p.sess.SendRPC(context.Background(), "tools/call",
				map[string]interface{}{"name": "x"})
			errs <- err
		}()
	}

	// Inserts are serialized by pendingMu, so exactly one of the 5 calls is
	// rejected; the other 4 park waiting for a response.
	select {
	case err := <-errs:
		if !errors.Is(err, ErrPendingFull) {
			t.Fatalf("err = %v, want ErrPendingFull", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no ErrPendingFull within 3s")
	}

	// The four successful calls are still parked in the pending table.
	waitFor(t, func() bool {
		p.sess.pendingMu.Lock()
		defer p.sess.pendingMu.Unlock()
		return len(p.sess.pending) == 4
	}, 2*time.Second)

	// Drain the device-side frames so every parked call has completed its
	// write before Close; otherwise a mid-write call would legitimately
	// return a write error instead of ErrSessionClosed.
	frames := make(chan struct{}, 4)
	go func() {
		for i := 0; i < 4; i++ {
			_, _, err := p.dev.ReadMessage()
			if err != nil {
				return
			}
			frames <- struct{}{}
		}
	}()
	for i := 0; i < 4; i++ {
		select {
		case <-frames:
		case <-time.After(2 * time.Second):
			t.Fatal("request frame never reached the device")
		}
	}

	// Release the parked callers; they must exit with ErrSessionClosed.
	p.sess.Close()
	for i := 0; i < 4; i++ {
		select {
		case err := <-errs:
			if !errors.Is(err, ErrSessionClosed) {
				t.Fatalf("parked call err = %v, want ErrSessionClosed", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("parked SendRPC still blocked after Close")
		}
	}
}

// TestSendRPCAfterClose verifies SendRPC on a closed session fails fast.
func TestSendRPCAfterClose(t *testing.T) {
	p := newWSPair(t)
	p.sess.Close()

	_, err := p.sess.SendRPC(context.Background(), "tools/call", nil)
	if !errors.Is(err, ErrSessionClosed) {
		t.Fatalf("err = %v, want ErrSessionClosed", err)
	}
}

// TestWriteJSONAfterClose verifies WriteJSON on a closed session fails fast.
func TestWriteJSONAfterClose(t *testing.T) {
	p := newWSPair(t)
	p.sess.Close()

	if err := p.sess.WriteJSON(map[string]interface{}{"m": 1}); !errors.Is(err, ErrSessionClosed) {
		t.Fatalf("err = %v, want ErrSessionClosed", err)
	}
}

// TestCloseWakesPendingWaiters verifies Close releases blocked SendRPC
// callers (the kick path must not leak goroutines).
func TestCloseWakesPendingWaiters(t *testing.T) {
	p := newWSPair(t)

	resCh := make(chan error, 1)
	go func() {
		_, err := p.sess.SendRPC(context.Background(), "tools/call", nil)
		resCh <- err
	}()

	// Consume the request frame on the device side so the write has completed
	// before we close (otherwise the write itself races with Close).
	writeDone := make(chan struct{}, 1)
	go func() {
		_, _, err := p.dev.ReadMessage()
		if err == nil {
			writeDone <- struct{}{}
		}
	}()

	waitFor(t, func() bool {
		p.sess.pendingMu.Lock()
		defer p.sess.pendingMu.Unlock()
		return len(p.sess.pending) == 1
	}, 2*time.Second)
	select {
	case <-writeDone:
	case <-time.After(2 * time.Second):
		t.Fatal("request frame never reached the device")
	}

	p.sess.Close()

	select {
	case err := <-resCh:
		if !errors.Is(err, ErrSessionClosed) {
			t.Fatalf("err = %v, want ErrSessionClosed", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("SendRPC still blocked after Close")
	}
}

// TestSendRPCWriteTimeoutDisconnects verifies the write-timeout path
// (design/31 3.1.4 写超时即断连): a device that never reads fills the socket
// buffer, the write deadline fires, the session closes itself and the hub
// route is removed by the cleanup chain.
func TestSendRPCWriteTimeoutDisconnects(t *testing.T) {
	p := newWSPair(t, func(s *DeviceSession) { s.writeWait = 200 * time.Millisecond })

	_, err := p.sess.SendRPC(context.Background(), "tools/call",
		map[string]interface{}{"payload": strings.Repeat("x", 8<<20)})
	if err == nil {
		t.Fatal("SendRPC succeeded, want write-timeout error")
	}
	if !p.sess.IsClosed() {
		t.Fatal("session must be closed after write timeout")
	}
	waitFor(t, func() bool { return p.hub.Size() == 0 }, 2*time.Second)
}

// TestOversizedMessageDisconnects verifies the 512KB inbound limit
// (design/31 3.1.4): a frame above maxMessageSize kills the connection and
// cleans up the route.
func TestOversizedMessageDisconnects(t *testing.T) {
	p := newWSPair(t)

	if err := p.dev.WriteMessage(websocket.BinaryMessage, make([]byte, maxMessageSize+1)); err != nil {
		t.Fatalf("device write: %v", err)
	}
	waitFor(t, func() bool { return p.sess.IsClosed() }, 2*time.Second)
	waitFor(t, func() bool { return p.hub.Size() == 0 }, 2*time.Second)
}

// TestWritePumpPingsDevice verifies the gateway ping liveness path with a
// shortened period: the device side must observe ping frames (via a custom
// PingHandler; gorilla consumes control frames internally) while the session
// stays alive.
func TestWritePumpPingsDevice(t *testing.T) {
	p := newWSPair(t, func(s *DeviceSession) { s.pingPeriod = 30 * time.Millisecond })
	defer p.dev.Close()

	pings := make(chan struct{}, 8)
	p.dev.SetPingHandler(func(appData string) error {
		select {
		case pings <- struct{}{}:
		default:
		}
		return p.dev.WriteControl(websocket.PongMessage, []byte(appData),
			time.Now().Add(2*time.Second))
	})

	// Drive the socket so the client-side handler runs.
	go func() {
		for {
			_ = p.dev.SetReadDeadline(time.Now().Add(5 * time.Second))
			if _, _, err := p.dev.ReadMessage(); err != nil {
				return
			}
		}
	}()

	for i := 0; i < 3; i++ {
		select {
		case <-pings:
		case <-time.After(2 * time.Second):
			t.Fatalf("ping %d not received within 2s", i+1)
		}
	}
	if p.sess.IsClosed() {
		t.Fatal("session closed despite healthy pong traffic")
	}
}

// TestHeartbeatCallbackRenewsTTL verifies the SEC-14 loop: the write pump
// invokes the registry heartbeat callback on every heartbeat interval.
func TestHeartbeatCallbackRenewsTTL(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	p := newWSPair(t, func(s *DeviceSession) {
		s.heartbeatInterval = 20 * time.Millisecond
		s.SetHeartbeatFn(func(context.Context) {
			mu.Lock()
			calls++
			mu.Unlock()
		})
	})
	defer p.dev.Close()

	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return calls >= 2
	}, 2*time.Second)

	if p.sess.IsClosed() {
		t.Fatal("session closed while heartbeat loop active")
	}
}

// TestCtxCancelClosesSession verifies writePump exits and closes the session
// when the request context is canceled (server shutdown path).
func TestCtxCancelClosesSession(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	p := newWSPairCtx(t, ctx)
	defer p.dev.Close()

	cancel()
	waitFor(t, func() bool { return p.sess.IsClosed() }, 2*time.Second)
	waitFor(t, func() bool { return p.hub.Size() == 0 }, 2*time.Second)
}

// TestSessionLifecycleCleanup verifies the full end-to-end cleanup chain: on
// device disconnect the read pump exits, onClose runs and the gen-checked hub
// removal leaves the hub empty.
func TestSessionLifecycleCleanup(t *testing.T) {
	p := newWSPair(t)
	if p.hub.Size() != 1 {
		t.Fatalf("size = %d, want 1", p.hub.Size())
	}

	p.dev.Close()
	waitFor(t, func() bool { return p.hub.Size() == 0 }, 2*time.Second)
}
