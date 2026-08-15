// Package connector implements the device-facing connection layer for the
// WSS endpoint /v1/devices/tunnel: per-connection session lifecycle (read and
// write pumps, heartbeat, pending-request dispatch), generation-based kick-old
// (SEC-15), the Valkey routing registry with TTL renewal (SEC-14) and the
// Origin whitelist on the upgrade path (SEC-04).
package connector

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"adc.dev/core-sdk/protocol"

	"github.com/gorilla/websocket"
)

// Connection timing budget. read-side liveness is measured on ANY inbound
// frame (pong or data), so a device that only sends application heartbeats
// stays alive; the gateway also pings every pingPeriod and expects pongs.
const (
	writeWait          = 10 * time.Second
	pongWait           = 60 * time.Second
	pingPeriod         = pongWait * 9 / 10
	maxMessageSize     = 512 * 1024
	maxPendingRequests = 256
	heartbeatInterval  = 30 * time.Second
)

// In-tunnel JSON-RPC notification methods (design/33 3.3).
const (
	methodHeartbeat    = "heartbeat"
	methodHeartbeatAck = "heartbeat_ack"
	methodKick         = "kick"
)

var (
	// ErrSessionClosed is returned when the session is already closed.
	ErrSessionClosed = errors.New("connector: device session closed")
	// ErrPendingFull is returned when the per-session pending table is full.
	ErrPendingFull = errors.New("connector: pending request table full (max 256)")
	// ErrDeviceOffline is returned when no live session exists for a device.
	ErrDeviceOffline = errors.New("connector: device offline")
)

// DeviceSession is a single device connection. It owns exactly two goroutines
// while running: the read pump (the goroutine that called Run) and the write
// pump (spawned inside Run). All frames are serialized through writeMu so ping,
// heartbeat_ack, kick and RPC frames never interleave.
type DeviceSession struct {
	DeviceID string // device code from the credential record (SEC-03)
	TenantID string // tenant from the credential record, never from the request
	Conn     *websocket.Conn

	gen uint64 // generation assigned by the hub (SEC-15)

	writeMu sync.Mutex
	toolsMu sync.RWMutex
	tools   []protocol.MCPTool

	pendingMu sync.Mutex
	pending   map[string]chan *protocol.JSONRPCResponse // requestID -> response

	closeOnce  sync.Once
	onCloseOne sync.Once
	done       chan struct{}

	heartbeatFn       func(context.Context) // registry TTL renewal (SEC-14)
	heartbeatInterval time.Duration

	// overridable in tests
	maxPending int
	pingPeriod time.Duration
	pongWait   time.Duration
	writeWait  time.Duration

	log *slog.Logger
}

// NewDeviceSession creates a session with default timing. The session is not
// live until Run is called.
func NewDeviceSession(deviceID, tenantID string, conn *websocket.Conn) *DeviceSession {
	return &DeviceSession{
		DeviceID:          deviceID,
		TenantID:          tenantID,
		Conn:              conn,
		pending:           make(map[string]chan *protocol.JSONRPCResponse),
		done:              make(chan struct{}),
		heartbeatInterval: heartbeatInterval,
		maxPending:        maxPendingRequests,
		pingPeriod:        pingPeriod,
		pongWait:          pongWait,
		writeWait:         writeWait,
		log:               slog.Default(),
	}
}

// Gen returns the generation assigned at Register time (SEC-15).
func (s *DeviceSession) Gen() uint64 { return s.gen }

// IsClosed reports whether Close has been called (done closed).
func (s *DeviceSession) IsClosed() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

// SetTools replaces the cached tool list (result of tools/list sync).
func (s *DeviceSession) SetTools(tools []protocol.MCPTool) {
	s.toolsMu.Lock()
	defer s.toolsMu.Unlock()
	s.tools = tools
}

// GetTools returns the cached tool list.
func (s *DeviceSession) GetTools() []protocol.MCPTool {
	s.toolsMu.RLock()
	defer s.toolsMu.RUnlock()
	return s.tools
}

// SetHeartbeatFn installs the periodic callback (registry.Heartbeat, SEC-14).
func (s *DeviceSession) SetHeartbeatFn(fn func(context.Context)) {
	s.heartbeatFn = fn
}

// Close terminates the session: it closes done (waking all pumps and pending
// waiters) and the underlying connection. Close is idempotent and does NOT run
// the onClose callback - the callback is delivered by the read pump when it
// exits, so it always runs exactly once from a single goroutine. This keeps
// the hub lock safe: Register may call Close on the stale session without the
// risk of a synchronous Unregister re-entering the hub.
func (s *DeviceSession) Close() {
	s.closeOnce.Do(func() {
		close(s.done)
		if s.Conn != nil {
			_ = s.Conn.Close()
		}
	})
}

// WriteJSON writes a single frame (used for kick and heartbeat_ack
// notifications). Returns ErrSessionClosed if the session is already done.
func (s *DeviceSession) WriteJSON(v interface{}) error {
	select {
	case <-s.done:
		return ErrSessionClosed
	default:
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.Conn.SetWriteDeadline(time.Now().Add(s.writeWait))
	return s.Conn.WriteJSON(v)
}

// SendRPC writes a JSON-RPC request and waits for the matching response.
// The request is registered in the pending table (bounded at maxPending, 256
// by default); a write failure is treated as a broken connection and triggers
// the cleanup chain (design/31 3.1.9).
func (s *DeviceSession) SendRPC(ctx context.Context, method string, params interface{}) (*protocol.JSONRPCResponse, error) {
	select {
	case <-s.done:
		return nil, ErrSessionClosed
	default:
	}

	reqID := newRequestID()
	respChan := make(chan *protocol.JSONRPCResponse, 1)

	s.pendingMu.Lock()
	if len(s.pending) >= s.maxPending {
		s.pendingMu.Unlock()
		return nil, ErrPendingFull
	}
	s.pending[reqID] = respChan
	s.pendingMu.Unlock()

	defer func() {
		s.pendingMu.Lock()
		delete(s.pending, reqID)
		s.pendingMu.Unlock()
	}()

	req := protocol.JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      reqID,
		Method:  method,
		Params:  params,
	}

	s.writeMu.Lock()
	s.Conn.SetWriteDeadline(time.Now().Add(s.writeWait))
	err := s.Conn.WriteJSON(req)
	s.writeMu.Unlock()
	if err != nil {
		// Write failure means the connection is gone; close to wake the read
		// pump and run the cleanup chain (hub + registry).
		s.Close()
		return nil, fmt.Errorf("connector: write rpc %s: %w", method, err)
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.done:
		return nil, ErrSessionClosed
	case resp := <-respChan:
		return resp, nil
	}
}

// Run starts the session. The caller's goroutine becomes the read pump; a
// second goroutine (write pump) is spawned for ping and registry heartbeat.
// When the session ends, onClose is invoked exactly once with the session
// (carrying its gen for the hub's stale-generation check, SEC-15).
func (s *DeviceSession) Run(ctx context.Context, onClose func(*DeviceSession)) {
	defer func() {
		s.closeOnce.Do(func() {
			close(s.done)
			if s.Conn != nil {
				_ = s.Conn.Close()
			}
		})
		s.onCloseOne.Do(func() {
			if onClose != nil {
				onClose(s)
			}
		})
	}()

	if s.Conn == nil {
		return
	}

	s.Conn.SetReadLimit(maxMessageSize)
	s.Conn.SetReadDeadline(time.Now().Add(s.pongWait))
	s.Conn.SetPongHandler(func(string) error {
		s.Conn.SetReadDeadline(time.Now().Add(s.pongWait))
		return nil
	})

	go s.writePump(ctx)

	for {
		_, msg, err := s.Conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				s.log.Warn("device connection read error",
					"tenant_id", s.TenantID, "device_id", s.DeviceID, "err", err)
			}
			return
		}
		// Any inbound frame counts as liveness (design/31 3.1.4).
		s.Conn.SetReadDeadline(time.Now().Add(s.pongWait))
		s.dispatch(msg)
	}
}

// writePump is the second goroutine per connection: it emits WS pings and
// drives registry TTL renewal (SEC-14). It exits when done closes or a ping
// write fails (in which case the session is closed to trigger cleanup).
func (s *DeviceSession) writePump(ctx context.Context) {
	pingTicker := time.NewTicker(s.pingPeriod)
	defer pingTicker.Stop()
	hbTicker := time.NewTicker(s.heartbeatInterval)
	defer hbTicker.Stop()

	for {
		select {
		case <-s.done:
			return
		case <-ctx.Done():
			s.Close()
			return
		case <-pingTicker.C:
			s.writeMu.Lock()
			s.Conn.SetWriteDeadline(time.Now().Add(writeWait))
			err := s.Conn.WriteMessage(websocket.PingMessage, nil)
			s.writeMu.Unlock()
			if err != nil {
				s.Close()
				return
			}
		case <-hbTicker.C:
			if s.heartbeatFn != nil {
				s.heartbeatFn(ctx)
			}
		}
	}
}

// dispatch routes an inbound frame: JSON-RPC responses wake the pending
// waiter by requestID; device heartbeats get an ack notification.
func (s *DeviceSession) dispatch(msg []byte) {
	var resp protocol.JSONRPCResponse
	if err := json.Unmarshal(msg, &resp); err == nil && resp.ID != "" {
		s.pendingMu.Lock()
		ch, ok := s.pending[resp.ID]
		s.pendingMu.Unlock()
		if ok {
			select {
			case ch <- &resp:
			default:
			}
			return
		}
	}

	var req protocol.JSONRPCRequest
	if err := json.Unmarshal(msg, &req); err == nil && req.Method != "" {
		if req.Method == methodHeartbeat {
			_ = s.WriteJSON(protocol.JSONRPCRequest{
				JSONRPC: "2.0",
				Method:  methodHeartbeatAck,
				Params:  map[string]interface{}{"server_time": time.Now().UTC().Format(time.RFC3339)},
			})
		}
		return
	}

	s.log.Warn("unrecognized frame from device",
		"tenant_id", s.TenantID, "device_id", s.DeviceID, "size", len(msg))
}

// newRequestID returns a random hex request ID (16 bytes of crypto/rand).
func newRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("t%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}
