// Full-chain agent flow tests (design/33 3.2-3.4, FR-001/003/005/006,
// E2E-01/02/03/04 reduced to the backend): a mock device connects the
// real WSS tunnel with a SEC-03 HMAC handshake, answers tools/list and
// tools/call JSON-RPC frames, and the Agent API runs the full arc —
// aggregation, low-risk direct call (200), high-risk HITL interception
// (202), signed callback approve (execution) and reject
// (BLOCKED_BY_HITL), nonce replay rejection and revoked-credential
// lockout. Same shape as scripts/dev-smoke.sh, but in-process with real
// handlers and real databases.
package integration

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"adc.dev/core-sdk/protocol"

	"adc.dev/ce/internal/approval"

	"github.com/gorilla/websocket"
)

// mockTool is the tool set a mock device advertises over tools/list.
type mockTool struct {
	name        string
	description string
	schema      string
}

// mockDevice is the scripted B-class device (design/40 3.4 device
// simulator reduced to the integration need): one websocket, tools/list
// and tools/call answers, kick detection, execution log.
type mockDevice struct {
	t      *testing.T
	conn   *websocket.Conn
	code   string
	tools  []mockTool
	mu     sync.Mutex
	kicked bool
	calls  []string // tools/call names received, in order
	done   chan struct{}
}

// deviceHMAC builds the SEC-03 handshake signature:
// hex(HMAC-SHA256(secret, deviceCode + "\n" + timestamp + "\n" + nonce)),
// design/33 1.3.
func deviceHMAC(secret, deviceCode string, ts int64, nonce string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(mac, "%s\n%d\n%s", deviceCode, ts, nonce)
	return hex.EncodeToString(mac.Sum(nil))
}

// dialMockDevice connects a mock device to the real tunnel endpoint with
// a fresh valid handshake and starts the frame-serving loop.
func (e *testEnv) dialMockDevice(t *testing.T, tenantID, code, secret string, tools []mockTool) *mockDevice {
	t.Helper()
	ts := time.Now().Unix()
	nonce := randomHex(16)
	h := http.Header{}
	h.Set(protocol.HeaderXDeviceID, code)
	h.Set(protocol.HeaderXDeviceTimestamp, strconv.FormatInt(ts, 10))
	h.Set(protocol.HeaderXDeviceNonce, nonce)
	h.Set(protocol.HeaderXDeviceSignature, deviceHMAC(secret, code, ts, nonce))

	conn, resp, err := websocket.DefaultDialer.Dial(e.wsURL(), h)
	if err != nil {
		if resp != nil {
			t.Fatalf("device dial failed: %v (status=%d)", err, resp.StatusCode)
		}
		t.Fatalf("device dial failed: %v", err)
	}
	md := &mockDevice{
		t: t, conn: conn, code: code, tools: tools, done: make(chan struct{}),
	}
	go md.serve()
	e.waitDeviceOnline(t, tenantID, code)
	return md
}

// dialDeviceRawWithHeaders dials the tunnel with caller-supplied headers
// (replay / tamper tests) and reports the HTTP status on failure.
func (e *testEnv) dialDeviceRawWithHeaders(headers http.Header) (*websocket.Conn, int, error) {
	conn, resp, err := websocket.DefaultDialer.Dial(e.wsURL(), headers)
	if err != nil {
		if resp != nil {
			return nil, resp.StatusCode, err
		}
		return nil, 0, err
	}
	return conn, 0, nil
}

// serve answers JSON-RPC frames until the connection closes.
func (md *mockDevice) serve() {
	defer close(md.done)
	defer func() { _ = md.conn.Close() }()
	for {
		_ = md.conn.SetReadDeadline(time.Now().Add(30 * time.Second))
		_, msg, err := md.conn.ReadMessage()
		if err != nil {
			return
		}
		var req protocol.JSONRPCRequest
		if err := json.Unmarshal(msg, &req); err != nil {
			continue
		}
		switch req.Method {
		case protocol.MethodToolsList:
			tools := make([]protocol.MCPTool, 0, len(md.tools))
			for _, mt := range md.tools {
				tools = append(tools, protocol.MCPTool{
					Name:        mt.name,
					Description: mt.description,
					InputSchema: json.RawMessage(mt.schema),
				})
			}
			raw, err := json.Marshal(protocol.ToolsListResult{Tools: tools})
			if err != nil {
				continue
			}
			_ = md.conn.WriteJSON(protocol.JSONRPCResponse{
				JSONRPC: "2.0", ID: req.ID, Result: raw,
			})
		case protocol.MethodToolsCall:
			var params protocol.ToolCallParams
			if raw, err := json.Marshal(req.Params); err == nil {
				_ = json.Unmarshal(raw, &params)
			}
			md.mu.Lock()
			md.calls = append(md.calls, params.Name)
			md.mu.Unlock()
			argsJSON, _ := json.Marshal(params.Arguments)
			raw, err := json.Marshal(protocol.ToolCallResult{
				Content: []protocol.ToolContent{{
					Type: "text",
					Text: fmt.Sprintf("%s executed on %s with args %s", params.Name, md.code, argsJSON),
				}},
				IsError: false,
			})
			if err != nil {
				continue
			}
			_ = md.conn.WriteJSON(protocol.JSONRPCResponse{
				JSONRPC: "2.0", ID: req.ID, Result: raw,
			})
		case "kick":
			md.mu.Lock()
			md.kicked = true
			md.mu.Unlock()
			return
		}
	}
}

func (md *mockDevice) close() {
	_ = md.conn.Close()
	select {
	case <-md.done:
	case <-time.After(5 * time.Second):
		md.t.Fatal("mock device serve loop did not exit after close")
	}
}

// waitDeviceOnline polls the hub until the device session of the given
// tenant is registered (FR-001: the tunnel establishes the route; the
// gateway issues tools/list within 5s of connect, design/33 3.3).
func (e *testEnv) waitDeviceOnline(t *testing.T, tenantID, deviceCode string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if sess, err := e.hub.GetDevice(tenantID, deviceCode); err == nil && !sess.IsClosed() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("device %s never registered in hub", deviceCode)
}

// seedTool inserts one adc_device_tools row for a registered device
// (the ledger the aggregator reads; SEC-09 risk is DB-authoritative).
func (e *testEnv) seedTool(t *testing.T, tenantID, deviceID, name string, risk int) {
	t.Helper()
	_, err := e.pool.Exec(context.Background(), `INSERT INTO adc_device_tools
		(tenant_id, device_id, tool_name, description, input_schema, risk_level, is_enabled)
		VALUES ($1::uuid, $2::uuid, $3, $4, '{"type":"object"}', $5, TRUE)`,
		tenantID, deviceID, name, name, risk)
	if err != nil {
		t.Fatalf("seed tool %s: %v", name, err)
	}
}

// signedCallback builds the design/33 3.4.1 callback body for a ticket,
// signed with the ticket-level key derived from the platform callback key
// (SEC-13).
func (e *testEnv) signedCallback(t *testing.T, ticketID, decision, approver, comment string) map[string]any {
	t.Helper()
	ticket, err := e.ticketRepo.Get(context.Background(), ticketID)
	if err != nil {
		t.Fatalf("load ticket %s: %v", ticketID, err)
	}
	secret := approval.DeriveTicketSecret(e.callbackKey, ticketID)
	sig := approval.SignCallback(secret, ticketID, decision, ticket.ExpireAt)
	return map[string]any{
		"ticket_id": ticketID,
		"decision":  decision,
		"signature": sig,
		"expire":    ticket.ExpireAt.Unix(),
		"approver":  approver,
		"comment":   comment,
	}
}

// pollCallStatus polls GET /v1/agent/mcp/tools/call/{requestID} until the
// status matches wantStatus, returning the final recorder.
func (e *testEnv) pollCallStatus(t *testing.T, agentKey, requestID string, wantStatus int) (status int, body string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		rec := e.do(http.MethodGet, "/v1/agent/mcp/tools/call/"+requestID, nil,
			map[string]string{"X-ADC-Key": agentKey})
		if rec.Code == wantStatus {
			return rec.Code, rec.Body.String()
		}
		time.Sleep(200 * time.Millisecond)
	}
	rec := e.do(http.MethodGet, "/v1/agent/mcp/tools/call/"+requestID, nil,
		map[string]string{"X-ADC-Key": agentKey})
	return rec.Code, rec.Body.String()
}

// flowFixture provisions one tenant + hmac device + low/high risk tools +
// agent key, and connects a mock device over the real tunnel. It returns
// everything the chain tests need.
type flowFixture struct {
	tenantID string
	deviceID string
	code     string
	secret   string
	agentKey string
	dev      *mockDevice
}

func (e *testEnv) newFlowFixture(t *testing.T, lowRiskTool, highRiskTool string) *flowFixture {
	t.Helper()
	adminToken := e.loginOK(t, fixturePlatformUser, fixturePlatformPass)
	tenantID := e.tenantID(t, fixtureAlphaTenant)
	code := testTenantCode(t, "it-flow")

	status, raw := e.registerDevice(t, adminToken, tenantID, code, "Flow CNC")
	if status != http.StatusCreated {
		t.Fatalf("register = %d (body=%q)", status, raw)
	}
	deviceID, secret := decodeDeviceRegister(t, raw)
	e.seedTool(t, tenantID, deviceID, lowRiskTool, 0)
	e.seedTool(t, tenantID, deviceID, highRiskTool, 2)

	_, agentKey := e.issueKey(t, adminToken, tenantID)
	dev := e.dialMockDevice(t, tenantID, code, secret, []mockTool{
		{name: lowRiskTool, description: "low risk read", schema: `{"type":"object"}`},
		{name: highRiskTool, description: "high risk write", schema: `{"type":"object","properties":{"rpm":{"type":"number"}}}`},
	})
	t.Cleanup(dev.close)
	return &flowFixture{
		tenantID: tenantID, deviceID: deviceID, code: code,
		secret: secret, agentKey: agentKey, dev: dev,
	}
}

// TestAgentFlowFullChain runs the dev-smoke arc in-process: device
// connect + tools/list sync, agent aggregation, low-risk direct call,
// high-risk interception (202), signed approve -> execution result, then
// reject -> BLOCKED_BY_HITL, with the audit trail verified after each
// outcome (SEC-07).
func TestAgentFlowFullChain(t *testing.T) {
	e := envOrSkip(t)
	fx := e.newFlowFixture(t, "get_spindle_status", "set_spindle_speed")

	// 1. Aggregated tool list via the Agent API (FR-003: qualified names
	//    deviceCode__toolName).
	rec := e.do(http.MethodGet, "/v1/agent/mcp/tools", nil,
		map[string]string{"X-ADC-Key": fx.agentKey})
	if rec.Code != http.StatusOK {
		t.Fatalf("tools list = %d (body=%q)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), fx.code+"__get_spindle_status") ||
		!strings.Contains(rec.Body.String(), fx.code+"__set_spindle_speed") {
		t.Fatalf("aggregated tools incomplete: %q", rec.Body.String())
	}

	// 2. Low-risk direct call -> 200, device executed it. The direct 200
	//    response carries the request_id envelope together with the result
	//    content (design/33 3.2.2).
	rec = e.do(http.MethodPost, "/v1/agent/mcp/tools/call", map[string]any{
		"name":      fx.code + "__get_spindle_status",
		"arguments": map[string]any{"sample": "status"},
	}, map[string]string{"X-ADC-Key": fx.agentKey})
	if rec.Code != http.StatusOK {
		t.Fatalf("low-risk call = %d (body=%q)", rec.Code, rec.Body.String())
	}
	var direct struct {
		RequestID string `json:"request_id"`
		IsError   bool   `json:"is_error"`
		Content   []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &direct); err != nil {
		t.Fatalf("decode direct call: %v", err)
	}
	if direct.RequestID == "" {
		t.Fatalf("direct call missing request_id envelope: %q", rec.Body.String())
	}
	if direct.IsError {
		t.Fatalf("direct call = %+v", direct)
	}
	if len(direct.Content) == 0 || !strings.Contains(direct.Content[0].Text, "get_spindle_status executed") {
		t.Fatalf("direct call content = %+v", direct.Content)
	}

	// 3. High-risk call -> 202 + ticket_id, pending poll stays 202.
	rec = e.do(http.MethodPost, "/v1/agent/mcp/tools/call", map[string]any{
		"name":      fx.code + "__set_spindle_speed",
		"arguments": map[string]any{"rpm": 3000},
	}, map[string]string{"X-ADC-Key": fx.agentKey})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("high-risk call = %d, want 202 (body=%q)", rec.Code, rec.Body.String())
	}
	var pending struct {
		RequestID string `json:"request_id"`
		TicketID  string `json:"ticket_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &pending); err != nil {
		t.Fatalf("decode 202: %v", err)
	}
	if pending.RequestID == "" || pending.TicketID == "" {
		t.Fatalf("202 missing ids: %+v", pending)
	}
	status, body := e.pollCallStatus(t, fx.agentKey, pending.RequestID, http.StatusAccepted)
	if status != http.StatusAccepted {
		t.Fatalf("pending poll = %d (body=%q)", status, body)
	}

	// 4. Signed approve callback -> executed -> completed result.
	rec = e.do(http.MethodPost, "/v1/hitl/callback",
		e.signedCallback(t, pending.TicketID, "approve", "it-approver", "verified"),
		nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("approve callback = %d (body=%q)", rec.Code, rec.Body.String())
	}
	var cbResp struct {
		TicketID string `json:"ticket_id"`
		Status   string `json:"status"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &cbResp)
	if cbResp.Status != "approved" {
		t.Fatalf("callback status = %q", cbResp.Status)
	}
	status, body = e.pollCallStatus(t, fx.agentKey, pending.RequestID, http.StatusOK)
	if status != http.StatusOK || !strings.Contains(body, "set_spindle_speed executed") {
		t.Fatalf("approved execution = %d (body=%q)", status, body)
	}

	// 5. Reject path: a second high-risk call with different args (fresh
	//    dedup hash) -> signed reject -> BLOCKED_BY_HITL 403 code 12006.
	rec = e.do(http.MethodPost, "/v1/agent/mcp/tools/call", map[string]any{
		"name":      fx.code + "__set_spindle_speed",
		"arguments": map[string]any{"rpm": 9999},
	}, map[string]string{"X-ADC-Key": fx.agentKey})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("second high-risk call = %d (body=%q)", rec.Code, rec.Body.String())
	}
	var pending2 struct {
		RequestID string `json:"request_id"`
		TicketID  string `json:"ticket_id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &pending2)
	rec = e.do(http.MethodPost, "/v1/hitl/callback",
		e.signedCallback(t, pending2.TicketID, "reject", "it-approver", "params mismatch"),
		nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("reject callback = %d (body=%q)", rec.Code, rec.Body.String())
	}
	status, body = e.pollCallStatus(t, fx.agentKey, pending2.RequestID, http.StatusForbidden)
	if status != http.StatusForbidden || !strings.Contains(body, "12006") {
		t.Fatalf("blocked poll = %d (body=%q)", status, body)
	}

	// 6. Audit trail: success + blocked rows for the executed tools
	//    (SEC-07, AUD-001); the approved row carries the approver.
	e.waitAuditRows(t, "success", "set_spindle_speed", 1)
	e.waitAuditRows(t, "blocked_by_hitl", "set_spindle_speed", 1)
	var approver string
	err := e.pool.QueryRow(context.Background(), `SELECT hitl_approver FROM adc_audit_logs
		WHERE tool_name = 'set_spindle_speed' AND status = 'success'
		ORDER BY created_at DESC LIMIT 1`).Scan(&approver)
	if err != nil {
		t.Fatalf("load approver from audit: %v", err)
	}
	if approver != "it-approver" {
		t.Fatalf("audit approver = %q, want it-approver (SEC-21 real subject)", approver)
	}
}

// TestAgentFlowCallbackSecurity: the signed callback surface rejects
// unsigned, badly-signed and replayed callbacks (SEC-01/SEC-13, HITL-005/
// 006/010) and unknown tickets (12001).
func TestAgentFlowCallbackSecurity(t *testing.T) {
	e := envOrSkip(t)
	fx := e.newFlowFixture(t, "read_status", "write_param")

	rec := e.do(http.MethodPost, "/v1/agent/mcp/tools/call", map[string]any{
		"name":      fx.code + "__write_param",
		"arguments": map[string]any{"val": 1},
	}, map[string]string{"X-ADC-Key": fx.agentKey})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("high-risk call = %d (body=%q)", rec.Code, rec.Body.String())
	}
	var pending struct {
		RequestID string `json:"request_id"`
		TicketID  string `json:"ticket_id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &pending)
	ticket, err := e.ticketRepo.Get(context.Background(), pending.TicketID)
	if err != nil {
		t.Fatalf("load ticket: %v", err)
	}

	// No signature at all -> 401 code 12004 (design/33: a missing
	// signature is an authentication failure, SEC-01; unsigned callbacks
	// never reach the state machine).
	rec = e.do(http.MethodPost, "/v1/hitl/callback", map[string]any{
		"ticket_id": pending.TicketID, "decision": "approve", "expire": ticket.ExpireAt.Unix(),
	}, nil)
	wantErr(t, rec, http.StatusUnauthorized, "12004")

	// Tampered signature -> 401 code 12004 (SEC-13).
	bad := e.signedCallback(t, pending.TicketID, "approve", "it-approver", "x")
	bad["signature"] = "deadbeef"
	rec = e.do(http.MethodPost, "/v1/hitl/callback", bad, nil)
	wantErr(t, rec, http.StatusUnauthorized, "12004")

	// Signature over a different decision -> 401 code 12004.
	wrong := e.signedCallback(t, pending.TicketID, "reject", "it-approver", "x")
	wrong["decision"] = "approve"
	rec = e.do(http.MethodPost, "/v1/hitl/callback", wrong, nil)
	wantErr(t, rec, http.StatusUnauthorized, "12004")

	// Valid approve lands; the replay is 409 code 12002 (HITL-010).
	rec = e.do(http.MethodPost, "/v1/hitl/callback",
		e.signedCallback(t, pending.TicketID, "approve", "it-approver", "ok"), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("first approve = %d (body=%q)", rec.Code, rec.Body.String())
	}
	rec = e.do(http.MethodPost, "/v1/hitl/callback",
		e.signedCallback(t, pending.TicketID, "approve", "it-approver", "again"), nil)
	wantErr(t, rec, http.StatusConflict, "12002")

	// Unknown ticket -> 404 code 12001 (signature built manually: the
	// helper loads the ticket from the repo and cannot serve unknowns).
	unknownID := "00000000-0000-0000-0000-000000000000"
	secret := approval.DeriveTicketSecret(e.callbackKey, unknownID)
	sig := approval.SignCallback(secret, unknownID, "approve", time.Now().Add(time.Minute))
	rec = e.do(http.MethodPost, "/v1/hitl/callback", map[string]any{
		"ticket_id": unknownID,
		"decision":  "approve",
		"signature": sig,
		"expire":    time.Now().Add(time.Minute).Unix(),
	}, nil)
	wantErr(t, rec, http.StatusNotFound, "12001")
}

// TestAgentFlowTunnelSecurity: SEC-03 nonce replay and revoked
// credentials are rejected at the tunnel with 401 (AUTH-003/006).
func TestAgentFlowTunnelSecurity(t *testing.T) {
	e := envOrSkip(t)
	fx := e.newFlowFixture(t, "read_status", "write_param")

	// Replay: the exact same handshake material must be refused the
	// second time (nonce one-time consumption, SEC-03).
	ts := time.Now().Unix()
	nonce := randomHex(16)
	h := http.Header{}
	h.Set(protocol.HeaderXDeviceID, fx.code)
	h.Set(protocol.HeaderXDeviceTimestamp, strconv.FormatInt(ts, 10))
	h.Set(protocol.HeaderXDeviceNonce, nonce)
	h.Set(protocol.HeaderXDeviceSignature, deviceHMAC(fx.secret, fx.code, ts, nonce))
	conn, status, err := e.dialDeviceRawWithHeaders(h)
	if err != nil {
		t.Fatalf("first dial with fresh nonce should succeed: %v (status=%d)", err, status)
	}
	_ = conn.Close()
	conn, status, err = e.dialDeviceRawWithHeaders(h.Clone())
	if err == nil {
		_ = conn.Close()
		t.Fatal("nonce replay accepted")
	}
	if status != http.StatusUnauthorized {
		t.Fatalf("replay status = %d, want 401", status)
	}

	// Wrong signature -> 401 without upgrade (SEC-03).
	h2 := http.Header{}
	ts2 := time.Now().Unix()
	nonce2 := randomHex(16)
	h2.Set(protocol.HeaderXDeviceID, fx.code)
	h2.Set(protocol.HeaderXDeviceTimestamp, strconv.FormatInt(ts2, 10))
	h2.Set(protocol.HeaderXDeviceNonce, nonce2)
	h2.Set(protocol.HeaderXDeviceSignature, deviceHMAC("wrong-secret", fx.code, ts2, nonce2))
	if conn, status, err = e.dialDeviceRawWithHeaders(h2); err == nil {
		_ = conn.Close()
		t.Fatal("bad signature accepted")
	} else if status != http.StatusUnauthorized {
		t.Fatalf("bad signature status = %d, want 401", status)
	}

	// Revoke the credential via the Admin API, then the same secret can
	// never connect again (FR-010 revoke semantics, AUTH-006).
	adminToken := e.loginOK(t, fixturePlatformUser, fixturePlatformPass)
	rec := e.do(http.MethodPatch, "/v1/admin/devices/"+fx.deviceID, map[string]any{
		"op": "revoke_credential", "change_reason": "decommission",
	}, authHdr(adminToken))
	if rec.Code != http.StatusOK {
		t.Fatalf("revoke = %d (body=%q)", rec.Code, rec.Body.String())
	}
	h3 := http.Header{}
	ts3 := time.Now().Unix()
	nonce3 := randomHex(16)
	h3.Set(protocol.HeaderXDeviceID, fx.code)
	h3.Set(protocol.HeaderXDeviceTimestamp, strconv.FormatInt(ts3, 10))
	h3.Set(protocol.HeaderXDeviceNonce, nonce3)
	h3.Set(protocol.HeaderXDeviceSignature, deviceHMAC(fx.secret, fx.code, ts3, nonce3))
	if conn, status, err = e.dialDeviceRawWithHeaders(h3); err == nil {
		_ = conn.Close()
		t.Fatal("revoked credential still connects")
	} else if status != http.StatusUnauthorized {
		t.Fatalf("revoked credential status = %d, want 401", status)
	}
}

// TestAgentFlowOfflineDevice: calling a tool of a device without a live
// session answers 502 code 12005 (design/33 3.2.2: 11002 family handled
// as transport failure by the V1 agent handler).
func TestAgentFlowOfflineDevice(t *testing.T) {
	e := envOrSkip(t)
	adminToken := e.loginOK(t, fixturePlatformUser, fixturePlatformPass)
	tenantID := e.tenantID(t, fixtureAlphaTenant)
	code := testTenantCode(t, "it-off")

	status, raw := e.registerDevice(t, adminToken, tenantID, code, "Offline Unit")
	if status != http.StatusCreated {
		t.Fatalf("register = %d (body=%q)", status, raw)
	}
	deviceID, _ := decodeDeviceRegister(t, raw)
	e.seedTool(t, tenantID, deviceID, "read_status", 0)
	_, agentKey := e.issueKey(t, adminToken, tenantID)

	rec := e.do(http.MethodPost, "/v1/agent/mcp/tools/call", map[string]any{
		"name":      code + "__read_status",
		"arguments": map[string]any{},
	}, map[string]string{"X-ADC-Key": agentKey})
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("offline call = %d, want 502 (body=%q)", rec.Code, rec.Body.String())
	}
	e2 := decodeErr(t, rec)
	if e2.Code != "12005" {
		t.Fatalf("code = %q, want 12005", e2.Code)
	}
}

// TestAgentFlowToolResolution: a structurally broken qualified name
// fails resolution with 400 code 12004 (design/33 3.2.2), while an
// unknown tool name resolves to the fail-safe default risk level 2
// (SEC-09 "先审后用") and enters HITL with 202.
func TestAgentFlowToolResolution(t *testing.T) {
	e := envOrSkip(t)
	fx := e.newFlowFixture(t, "read_status", "write_param")

	// Broken name (no separator) -> 400 code 12004.
	rec := e.do(http.MethodPost, "/v1/agent/mcp/tools/call", map[string]any{
		"name":      "no-separator-here",
		"arguments": map[string]any{},
	}, map[string]string{"X-ADC-Key": fx.agentKey})
	wantErr(t, rec, http.StatusBadRequest, "12004")

	// Unknown tool -> fail-safe HITL interception (202 + ticket), the
	// DBPolicy default level 2 for unconfigured tools (FR-006/RISK-004).
	rec = e.do(http.MethodPost, "/v1/agent/mcp/tools/call", map[string]any{
		"name":      fx.code + "__ghost_tool",
		"arguments": map[string]any{"n": 1},
	}, map[string]string{"X-ADC-Key": fx.agentKey})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("unknown tool call = %d, want 202 (body=%q)", rec.Code, rec.Body.String())
	}
	var resp struct {
		RequestID string `json:"request_id"`
		TicketID  string `json:"ticket_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode 202: %v", err)
	}
	if resp.TicketID == "" {
		t.Fatal("fail-safe call produced no ticket")
	}
}

// TestAgentFlowTenantIsolation: an agent key of tenant alpha can only
// see alpha tools; calling a beta tool resolves nothing (12004), and a
// forged X-Tenant-ID header changes nothing (SEC-02 / TEN-001/002).
func TestAgentFlowTenantIsolation(t *testing.T) {
	e := envOrSkip(t)
	adminToken := e.loginOK(t, fixturePlatformUser, fixturePlatformPass)
	betaID := e.tenantID(t, fixtureBetaTenant)
	betaCode := testTenantCode(t, "it-beta")
	status, raw := e.registerDevice(t, adminToken, betaID, betaCode, "Beta Asset")
	if status != http.StatusCreated {
		t.Fatalf("register beta device = %d (body=%q)", status, raw)
	}
	betaDeviceID, _ := decodeDeviceRegister(t, raw)
	e.seedTool(t, betaID, betaDeviceID, "beta_only_tool", 0)

	alphaID := e.tenantID(t, fixtureAlphaTenant)
	_, alphaKey := e.issueKey(t, adminToken, alphaID)

	// alpha's tool list must not contain beta's tool (FR-003 isolation).
	rec := e.do(http.MethodGet, "/v1/agent/mcp/tools", nil,
		map[string]string{"X-ADC-Key": alphaKey})
	if rec.Code != http.StatusOK {
		t.Fatalf("tools = %d (body=%q)", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "beta_only_tool") {
		t.Fatal("alpha key sees beta tools (tenant isolation broken)")
	}

	// Calling beta's tool with alpha's key (with or without a forged
	// tenant header) must never execute it (SEC-02: tenant context comes
	// from the key; the forged X-Tenant-ID header is ignored). The device
	// ownership check rejects the call explicitly with 403 code 13007
	// (design/33 tenant isolation, previously fail-closed as 500).
	for _, hdr := range []map[string]string{
		{"X-ADC-Key": alphaKey},
		{"X-ADC-Key": alphaKey, "X-Tenant-ID": betaID},
	} {
		rec = e.do(http.MethodPost, "/v1/agent/mcp/tools/call", map[string]any{
			"name":      betaCode + "__beta_only_tool",
			"arguments": map[string]any{},
		}, hdr)
		if rec.Code == http.StatusOK {
			t.Fatalf("cross-tenant call executed: %q", rec.Body.String())
		}
		wantErr(t, rec, http.StatusForbidden, "13007")
	}
}

// randomHex returns n random bytes hex-encoded (device nonces).
func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
