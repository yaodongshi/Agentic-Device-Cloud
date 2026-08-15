package connector

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"adc.dev/ce/internal/httpx"
	"adc.dev/core-sdk/protocol"

	"github.com/gorilla/websocket"
)

// Handshake is the device authentication material carried in the upgrade
// headers (SEC-03, protocol.HeaderXDevice*). It is parsed here so the
// DeviceAuth implementation stays independent of HTTP.
type Handshake struct {
	DeviceID  string // X-Device-ID
	Timestamp int64  // X-Device-Timestamp (unix seconds)
	Nonce     string // X-Device-Nonce
	Signature string // X-Device-Signature
}

// parseHandshake extracts and validates the presence of the auth headers.
func parseHandshake(r *http.Request) (*Handshake, error) {
	ts, err := strconv.ParseInt(r.Header.Get(protocol.HeaderXDeviceTimestamp), 10, 64)
	if err != nil {
		return nil, errors.New("missing or invalid X-Device-Timestamp")
	}
	hs := &Handshake{
		DeviceID:  r.Header.Get(protocol.HeaderXDeviceID),
		Timestamp: ts,
		Nonce:     r.Header.Get(protocol.HeaderXDeviceNonce),
		Signature: r.Header.Get(protocol.HeaderXDeviceSignature),
	}
	if hs.DeviceID == "" || hs.Nonce == "" || hs.Signature == "" {
		return nil, errors.New("missing device authentication headers")
	}
	return hs, nil
}

// DeviceAuth is the seam the connector depends on for device handshake
// authentication (SEC-03). It is deliberately an interface in this package:
// the implementation lives in the future internal/auth package, which avoids
// a package-level import cycle. The tenant is taken from the credential
// record by the implementation, never from the request.
type DeviceAuth interface {
	Authenticate(ctx context.Context, hs *Handshake) (*DeviceIdentity, error)
}

// DeviceIdentity is the authenticated identity of a device.
type DeviceIdentity struct {
	TenantID   string
	DeviceCode string
}

// TunnelServer serves the WSS endpoint /v1/devices/tunnel (design/33 3.3):
// Origin whitelist (SEC-04) -> auth, fail 401 without upgrade (SEC-03) ->
// upgrade -> hub.Register kick-old (SEC-15) -> registry.Register (SEC-14) ->
// tools/list sync (5s) -> pumps -> disconnect cleanup chain.
type TunnelServer struct {
	upgrader    websocket.Upgrader
	auth        DeviceAuth
	hub         *DeviceHub
	registry    SessionRegistry
	nodeID      string
	ttl         time.Duration
	syncTimeout time.Duration
	log         *slog.Logger
}

// TunnelServerConfig wires the tunnel dependencies.
type TunnelServerConfig struct {
	Auth           DeviceAuth
	Hub            *DeviceHub
	Registry       SessionRegistry
	NodeID         string
	DeviceTTL      time.Duration // registry loc TTL (config DeviceTTLSeconds)
	AllowedOrigins []string      // Origin whitelist (SEC-04)
	SyncTimeout    time.Duration // tools/list per-attempt timeout (default 5s)
	Logger         *slog.Logger
}

// NewTunnelServer assembles the tunnel handler.
func NewTunnelServer(cfg TunnelServerConfig) *TunnelServer {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.SyncTimeout <= 0 {
		cfg.SyncTimeout = 5 * time.Second
	}
	return &TunnelServer{
		upgrader: websocket.Upgrader{
			ReadBufferSize:   4096,
			WriteBufferSize:  4096,
			HandshakeTimeout: 10 * time.Second,
			// SEC-04: only whitelisted browser origins may upgrade. Requests
			// without an Origin header are non-browser device SDK clients and
			// are allowed; a browser cross-site request would always send one.
			CheckOrigin: originChecker(cfg.AllowedOrigins),
		},
		auth:        cfg.Auth,
		hub:         cfg.Hub,
		registry:    cfg.Registry,
		nodeID:      cfg.NodeID,
		ttl:         cfg.DeviceTTL,
		syncTimeout: cfg.SyncTimeout,
		log:         cfg.Logger,
	}
}

// originChecker builds the CheckOrigin whitelist closure (SEC-04).
func originChecker(allowed []string) func(*http.Request) bool {
	whitelist := make(map[string]struct{}, len(allowed))
	for _, o := range allowed {
		if o = strings.TrimSpace(o); o != "" {
			whitelist[o] = struct{}{}
		}
	}
	return func(r *http.Request) bool {
		origin := r.Header.Get("Origin")
		if origin == "" {
			return true
		}
		_, ok := whitelist[origin]
		return ok
	}
}

// ServeHTTP implements the tunnel endpoint.
func (s *TunnelServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	traceID := httpx.TraceIDFrom(r)

	hs, err := parseHandshake(r)
	if err != nil {
		httpx.WriteError(w, http.StatusUnauthorized, "device_auth_failed",
			"device authentication headers missing or invalid", traceID)
		return
	}

	ident, err := s.auth.Authenticate(ctx, hs)
	if err != nil {
		s.log.Warn("device authentication failed",
			"device_id", hs.DeviceID, "err", err, "trace_id", traceID)
		httpx.WriteError(w, http.StatusUnauthorized, "device_auth_failed",
			"device authentication failed", traceID)
		return
	}

	// Origin whitelist is enforced inside Upgrade (SEC-04); a rejected origin
	// yields 403 from the upgrader without a session.
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}

	sess := NewDeviceSession(ident.DeviceCode, ident.TenantID, conn)

	// Kick-old: the previous generation must not outlive the new one (SEC-15).
	if old := s.hub.Register(sess); old != nil {
		s.kick(old)
	}

	// Registry registration with retry; fail-closed when Valkey is down
	// (design/31 3.1.9).
	if s.registry != nil {
		if err := s.registry.Register(ctx, ident.TenantID, ident.DeviceCode, s.nodeID, s.ttl); err != nil {
			s.log.Error("refusing device connection: registry register failed",
				"tenant_id", ident.TenantID, "device_id", ident.DeviceCode, "err", err)
			sess.Close()
			s.hub.UnregisterIfCurrent(ident.TenantID, ident.DeviceCode, sess.Gen())
			return
		}
	}

	// Renew the routing TTL every 30s while connected (SEC-14). The callback
	// is only installed when a registry exists: with a nil registry there is
	// nothing to renew and the write pump must not dereference it.
	if s.registry != nil {
		sess.SetHeartbeatFn(func(hctx context.Context) {
			if err := s.registry.Heartbeat(hctx, ident.TenantID, ident.DeviceCode); err != nil {
				s.log.Warn("registry heartbeat failed",
					"tenant_id", ident.TenantID, "device_id", ident.DeviceCode, "err", err)
			}
		})
	}

	// Disconnect cleanup chain: gen-checked hub removal, then registry
	// cleanup (SEC-15/SEC-14). The registry unregister only runs when the hub
	// removal actually happened: a kicked stale session must not tear down the
	// new session's routing index (GAP-11).
	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		sess.Run(ctx, func(closed *DeviceSession) {
			if !s.hub.UnregisterIfCurrent(closed.TenantID, closed.DeviceID, closed.Gen()) {
				return
			}
			if s.registry != nil {
				if err := s.registry.Unregister(context.Background(), closed.TenantID, closed.DeviceID); err != nil {
					s.log.Warn("registry unregister failed",
						"tenant_id", closed.TenantID, "device_id", closed.DeviceID, "err", err)
				}
			}
		})
	}()

	// The read pump must already be live here: syncTools blocks on SendRPC
	// and the device's response can only be dispatched by the read pump.
	s.syncTools(ctx, sess)

	// Keep the handler alive for the lifetime of the connection.
	<-runDone
}

// kick notifies the stale session and closes it (design/33 3.3 kick message).
func (s *TunnelServer) kick(old *DeviceSession) {
	_ = old.WriteJSON(protocol.JSONRPCRequest{
		JSONRPC: "2.0",
		Method:  methodKick,
		Params:  map[string]interface{}{"reason": "replaced_by_new_connection"},
	})
	old.Close()
}

// syncTools issues tools/list with a bounded timeout and caches the result.
func (s *TunnelServer) syncTools(ctx context.Context, sess *DeviceSession) {
	syncCtx, cancel := context.WithTimeout(ctx, s.syncTimeout)
	defer cancel()

	resp, err := sess.SendRPC(syncCtx, protocol.MethodToolsList, nil)
	if err != nil {
		s.log.Warn("tools/list sync failed",
			"tenant_id", sess.TenantID, "device_id", sess.DeviceID, "err", err)
		return
	}
	if resp.Error != nil {
		s.log.Warn("tools/list rejected by device",
			"tenant_id", sess.TenantID, "device_id", sess.DeviceID, "err", resp.Error)
		return
	}

	var res protocol.ToolsListResult
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		s.log.Warn("tools/list result malformed",
			"tenant_id", sess.TenantID, "device_id", sess.DeviceID, "err", err)
		return
	}
	sess.SetTools(res.Tools)
	s.log.Info("device tools synced",
		"tenant_id", sess.TenantID, "device_id", sess.DeviceID, "tools", len(res.Tools))
}
