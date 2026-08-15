package protocol

// Wire-contract tests for the Go protocol types (design/80 E-01).
//
// Every sample payload below is hardcoded and pinned to the Python
// implementation (core-sdk/python/adc_core_sdk/protocol.py) and to
// core-sdk/python/tests/test_wire_contract.py. Field names, omission rules
// and defaults must never drift between the two languages.

import (
	"encoding/json"
	"reflect"
	"testing"
)

func assertWireJSON(t *testing.T, got []byte, want string) {
	t.Helper()
	if string(got) != want {
		t.Fatalf("wire shape mismatch\n got: %s\nwant: %s", got, want)
	}
}

func TestDeviceAuthHeaderConstants(t *testing.T) {
	// Pinned against Python HEADER_X_* constants in protocol.py.
	if HeaderXDeviceID != "X-Device-ID" {
		t.Fatalf("HeaderXDeviceID = %q", HeaderXDeviceID)
	}
	if HeaderXDeviceTimestamp != "X-Device-Timestamp" {
		t.Fatalf("HeaderXDeviceTimestamp = %q", HeaderXDeviceTimestamp)
	}
	if HeaderXDeviceNonce != "X-Device-Nonce" {
		t.Fatalf("HeaderXDeviceNonce = %q", HeaderXDeviceNonce)
	}
	if HeaderXDeviceSignature != "X-Device-Signature" {
		t.Fatalf("HeaderXDeviceSignature = %q", HeaderXDeviceSignature)
	}
	if AuthTimeWindowSec != 300 {
		t.Fatalf("AuthTimeWindowSec = %d", AuthTimeWindowSec)
	}
}

func TestJSONRPCRequestWireShapeWithVersion(t *testing.T) {
	req := JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      "req-1",
		Method:  MethodToolsList,
		Version: "1.0",
	}
	got, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	assertWireJSON(t, got, `{"jsonrpc":"2.0","id":"req-1","method":"tools/list","version":"1.0"}`)
}

func TestJSONRPCRequestMissingVersionDefaultsTo1_0(t *testing.T) {
	// Backward compatibility: a request without "version" speaks 1.0.
	sample := []byte(`{"jsonrpc":"2.0","id":"req-1","method":"tools/list"}`)
	var req JSONRPCRequest
	if err := json.Unmarshal(sample, &req); err != nil {
		t.Fatal(err)
	}
	if req.Version != "" {
		t.Fatalf("Version = %q, want empty", req.Version)
	}
	if got := req.EffectiveVersion(); got != ProtocolVersion {
		t.Fatalf("EffectiveVersion() = %q, want %q", got, ProtocolVersion)
	}
	if _, err := json.Marshal(req); err != nil {
		t.Fatal(err)
	}
	if got, _ := json.Marshal(req); string(got) != string(sample) {
		t.Fatalf("roundtrip changed the wire shape: %s", got)
	}
}

func TestToolsListResponseWireShape(t *testing.T) {
	tool := MCPTool{
		Name:        "dev-01::reboot",
		Description: "Reboot the device",
		InputSchema: json.RawMessage(
			`{"type":"object","properties":{"delay_ms":{"type":"integer"}}}`,
		),
		RiskLevel:     intPtr(3),
		SchemaVersion: "v1",
	}
	res := ToolsListResult{Tools: []MCPTool{tool}}
	got, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	assertWireJSON(t, got, `{"tools":[{"name":"dev-01::reboot","description":"Reboot the device",`+
		`"inputSchema":{"type":"object","properties":{"delay_ms":{"type":"integer"}}},`+
		`"riskLevel":3,"schemaVersion":"v1"}]}`)

	var parsed ToolsListResult
	if err := json.Unmarshal(got, &parsed); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(parsed.Tools, res.Tools) {
		t.Fatalf("roundtrip mismatch: %+v", parsed.Tools)
	}
}

func TestToolsCallRequestAndResultWireShape(t *testing.T) {
	req := JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      "req-2",
		Method:  MethodToolsCall,
		Params: ToolCallParams{
			Name:      "dev-01::reboot",
			Arguments: map[string]interface{}{"delay_ms": 500},
		},
	}
	got, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	assertWireJSON(t, got, `{"jsonrpc":"2.0","id":"req-2","method":"tools/call",`+
		`"params":{"name":"dev-01::reboot","arguments":{"delay_ms":500}}}`)

	result := ToolCallResult{
		Content: []ToolContent{
			{Type: "text", Text: "reboot scheduled"},
			{Type: "text"},
		},
		IsError: false,
	}
	got, err = json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	assertWireJSON(t, got,
		`{"content":[{"type":"text","text":"reboot scheduled"},{"type":"text"}],"isError":false}`)
}

func TestErrorResponseWireShape(t *testing.T) {
	resp := JSONRPCResponse{
		JSONRPC: "2.0",
		ID:      "req-3",
		Error: &JSONRPCError{
			Code:    ErrInvalidArg,
			Message: "Invalid params",
			Data:    map[string]interface{}{"field": "name"},
		},
	}
	got, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	assertWireJSON(t, got, `{"jsonrpc":"2.0","id":"req-3",`+
		`"error":{"code":-32602,"message":"Invalid params","data":{"field":"name"}}}`)
}

func TestHandshakeWireShape(t *testing.T) {
	handshake := Handshake{
		ProtocolVersion: "1.0",
		Capabilities:    []string{"tools", "handshake"},
	}
	got, err := json.Marshal(handshake)
	if err != nil {
		t.Fatal(err)
	}
	assertWireJSON(t, got, `{"protocolVersion":"1.0","capabilities":["tools","handshake"]}`)

	var parsed Handshake
	if err := json.Unmarshal(got, &parsed); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(parsed.Capabilities, handshake.Capabilities) {
		t.Fatalf("capabilities roundtrip mismatch: %v", parsed.Capabilities)
	}
}

func TestHandshakeOmitsEmptyCapabilities(t *testing.T) {
	got, err := json.Marshal(Handshake{ProtocolVersion: "1.0"})
	if err != nil {
		t.Fatal(err)
	}
	assertWireJSON(t, got, `{"protocolVersion":"1.0"}`)
}

func TestVersionNegotiation(t *testing.T) {
	cases := []struct {
		name    string
		version string
		wantErr bool
	}{
		{name: "missing version treated as 1.0", version: "", wantErr: false},
		{name: "exact 1.0 accepted", version: "1.0", wantErr: false},
		{name: "older version rejected", version: "0.9", wantErr: true},
		{name: "newer version rejected", version: "2.0", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errObj := NegotiateVersion(tc.version)
			if tc.wantErr {
				if errObj == nil {
					t.Fatal("NegotiateVersion() = nil, want error")
				}
				if errObj.Code != ErrVersionUnsupported {
					t.Fatalf("code = %d, want %d (-32001 frozen wire value)", errObj.Code, ErrVersionUnsupported)
				}
			} else if errObj != nil {
				t.Fatalf("NegotiateVersion(%q) = %v, want nil", tc.version, errObj)
			}
		})
	}
}

func TestVersionUnsupportedErrorResponseWireShape(t *testing.T) {
	resp := JSONRPCResponse{
		JSONRPC: "2.0",
		ID:      "hs-1",
		Error:   NegotiateVersion("0.9"),
	}
	got, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	assertWireJSON(t, got, `{"jsonrpc":"2.0","id":"hs-1",`+
		`"error":{"code":-32001,"message":"unsupported protocol version: 0.9"}}`)
}

func intPtr(v int) *int { return &v }
