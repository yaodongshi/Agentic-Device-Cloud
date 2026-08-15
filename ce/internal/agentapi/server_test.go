package agentapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"adc.dev/core-sdk/protocol"

	"adc.dev/ce/internal/agentauth"
)

func TestParseToolRef(t *testing.T) {
	tests := []struct {
		in   string
		want ToolRef
		err  bool
	}{
		{"cnc-01::set_rpm", ToolRef{DeviceID: "cnc-01", ToolName: "set_rpm"}, false},
		{"cnc-01__set_rpm", ToolRef{DeviceID: "cnc-01", ToolName: "set_rpm"}, false},
		{"bad", ToolRef{}, true},
		{"::x", ToolRef{}, true},
		{"x::", ToolRef{}, true},
	}
	for _, tt := range tests {
		got, err := ParseToolRef(tt.in)
		if tt.err && !errors.Is(err, ErrInvalidToolRef) {
			t.Fatalf("%q: want ErrInvalidToolRef, got %v", tt.in, err)
		}
		if !tt.err && (err != nil || got != tt.want) {
			t.Fatalf("%q: got %+v err %v, want %+v", tt.in, got, err, tt.want)
		}
	}
}

func TestDBPolicyDefaultLevelTwo(t *testing.T) {
	p := DBPolicy{Lookup: func(ctx context.Context, tenantID, deviceID, toolName string) (int, error) {
		return 0, RiskNotFound
	}}
	d, err := p.Decide(context.Background(), "t", ToolRef{DeviceID: "d", ToolName: "n"})
	if err != nil {
		t.Fatal(err)
	}
	if d.Level != 2 || !d.RequireApproval {
		t.Fatalf("want default level 2 requiring approval, got %+v", d)
	}
}

func TestDBPolicyDBLevelWins(t *testing.T) {
	p := DBPolicy{Lookup: func(ctx context.Context, tenantID, deviceID, toolName string) (int, error) {
		return 0, nil
	}}
	d, _ := p.Decide(context.Background(), "t", ToolRef{DeviceID: "d", ToolName: "stop"})
	if d.RequireApproval {
		t.Fatalf("level 0 must not require approval regardless of tool name (SEC-09): %+v", d)
	}
}

func buildTestServer(hitl HITLClient, exec func(ctx context.Context, args map[string]interface{}) (string, error)) *Server {
	policy := DBPolicy{Lookup: func(ctx context.Context, tenantID, deviceID, toolName string) (int, error) {
		return 0, RiskNotFound
	}}
	agg := DBAggregator{
		Rows: func(ctx context.Context, tenantID string) ([]ToolRow, error) {
			lvl := 2
			return []ToolRow{{
				DeviceCode: "cnc-01", ToolName: "set_rpm",
				Description: "set spindle rpm",
				InputSchema: json.RawMessage(`{"type":"object"}`),
				RiskLevel:   lvl,
			}}, nil
		},
		Lookup: func(ctx context.Context, tenantID, deviceID, toolName string) (int, error) {
			return 2, nil
		},
	}
	router := LocalRouter{Session: func(ctx context.Context, tenantID, deviceID string) (RPCClient, error) {
		if exec == nil {
			return nil, ErrDeviceOffline
		}
		return rpcStub{exec: exec}, nil
	}}
	s := NewServer(validatorStub{}, agg, router, policy, hitl)
	s.AwaitWindow = 2 * time.Second
	return s
}

type validatorStub struct{}

func (validatorStub) Validate(ctx context.Context, token string) (*agentauth.Principal, error) {
	if token == "good-key" {
		return &agentauth.Principal{KeyID: "k1", TenantID: "tenant-1", AgentID: "agent-1", Scopes: []string{"tools.list", "tools.call"}}, nil
	}
	return nil, agentauth.ErrKeyNotFound
}

type rpcStub struct {
	exec func(ctx context.Context, args map[string]interface{}) (string, error)
}

func (r rpcStub) SendRPC(ctx context.Context, method string, params interface{}) (*protocol.JSONRPCResponse, error) {
	call, _ := params.(protocol.ToolCallParams)
	out, err := r.exec(ctx, call.Arguments)
	if err != nil {
		return &protocol.JSONRPCResponse{JSONRPC: "2.0", ID: "1",
			Error: &protocol.JSONRPCError{Code: -32000, Message: err.Error()}}, nil
	}
	b, _ := json.Marshal(protocol.ToolCallResult{
		Content: []protocol.ToolContent{{Type: "text", Text: out}},
	})
	return &protocol.JSONRPCResponse{JSONRPC: "2.0", ID: "1", Result: b}, nil
}

func doReq(t *testing.T, s *Server, method, path, body, key string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	s.Routes(mux)
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	req.Header.Set("X-ADC-Key", key)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w
}

func TestToolsListUnauthorized(t *testing.T) {
	s := buildTestServer(nil, nil)
	w := doReq(t, s, "GET", "/v1/agent/mcp/tools", "", "bad-key")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", w.Code)
	}
}

func TestToolsListOK(t *testing.T) {
	s := buildTestServer(nil, nil)
	w := doReq(t, s, "GET", "/v1/agent/mcp/tools", "", "good-key")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", w.Code, w.Body.String())
	}
	var res protocol.ToolsListResult
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Tools) != 1 || res.Tools[0].Name != "cnc-01__set_rpm" {
		t.Fatalf("unexpected tools: %+v", res.Tools)
	}
}

func TestHighRiskCallReturns202ThenExecutesOnApprove(t *testing.T) {
	hitl := NewInMemoryHITLClient()
	s := buildTestServer(hitl, func(ctx context.Context, args map[string]interface{}) (string, error) {
		return "rpm=3000", nil
	})
	w := doReq(t, s, "POST", "/v1/agent/mcp/tools/call", `{"name":"cnc-01::set_rpm","arguments":{"rpm":3000}}`, "good-key")
	if w.Code != http.StatusAccepted {
		t.Fatalf("want 202, got %d body=%s", w.Code, w.Body.String())
	}
	var acc map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &acc)
	requestID := acc["request_id"]
	if requestID == "" {
		t.Fatal("missing request_id")
	}
	// Poll before decision: still pending.
	w2 := doReq(t, s, "GET", "/v1/agent/mcp/tools/call/"+requestID, "", "good-key")
	if w2.Code != http.StatusAccepted {
		t.Fatalf("pending poll want 202, got %d", w2.Code)
	}
	// Approve, wait for async execution, then poll again.
	ref, _ := hitl.LookupByRequest(requestID)
	hitl.DecideTicket(ref.TicketID, TicketDecision{Status: DecisionApproved, By: "eng"})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		w3 := doReq(t, s, "GET", "/v1/agent/mcp/tools/call/"+requestID, "", "good-key")
		if w3.Code == http.StatusOK {
			if !strings.Contains(w3.Body.String(), "rpm=3000") {
				t.Fatalf("executed body wrong: %s", w3.Body.String())
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("approved call never completed")
}

func TestHighRiskCallBlockedOnReject(t *testing.T) {
	hitl := NewInMemoryHITLClient()
	s := buildTestServer(hitl, nil)
	w := doReq(t, s, "POST", "/v1/agent/mcp/tools/call", `{"name":"cnc-01::set_rpm","arguments":{"rpm":9999}}`, "good-key")
	var acc map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &acc)
	ref, _ := hitl.LookupByRequest(acc["request_id"])
	hitl.DecideTicket(ref.TicketID, TicketDecision{Status: DecisionRejected, By: "eng", Reason: "unsafe"})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		w2 := doReq(t, s, "GET", "/v1/agent/mcp/tools/call/"+acc["request_id"], "", "good-key")
		if w2.Code == http.StatusForbidden {
			if !strings.Contains(w2.Body.String(), CodeBlockedByHITL) {
				t.Fatalf("want code %s in body: %s", CodeBlockedByHITL, w2.Body.String())
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("rejected call never blocked")
}

func TestHighRiskCallExpires(t *testing.T) {
	hitl := NewInMemoryHITLClient()
	s := buildTestServer(hitl, nil)
	s.AwaitWindow = 200 * time.Millisecond
	w := doReq(t, s, "POST", "/v1/agent/mcp/tools/call", `{"name":"cnc-01::set_rpm","arguments":{"rpm":1}}`, "good-key")
	var acc map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &acc)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		w2 := doReq(t, s, "GET", "/v1/agent/mcp/tools/call/"+acc["request_id"], "", "good-key")
		if w2.Code == http.StatusForbidden {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("expired call never blocked")
}

func TestUnknownRequestID(t *testing.T) {
	s := buildTestServer(NewInMemoryHITLClient(), nil)
	w := doReq(t, s, "GET", "/v1/agent/mcp/tools/call/nope", "", "good-key")
	if w.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", w.Code)
	}
}

func TestInvalidToolRef(t *testing.T) {
	s := buildTestServer(NewInMemoryHITLClient(), nil)
	w := doReq(t, s, "POST", "/v1/agent/mcp/tools/call", `{"name":"garbage"}`, "good-key")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", w.Code)
	}
}

// TestLowRiskCallEnvelope: the direct (no-approval) 200 response carries
// the request_id envelope field together with the result content, matching
// the 202 HITL envelope (design/33 3.2.2).
func TestLowRiskCallEnvelope(t *testing.T) {
	policy := DBPolicy{Lookup: func(ctx context.Context, tenantID, deviceID, toolName string) (int, error) {
		return 0, nil // level 0: no approval required
	}}
	agg := DBAggregator{
		Lookup: func(ctx context.Context, tenantID, deviceID, toolName string) (int, error) {
			return 0, nil
		},
	}
	router := LocalRouter{Session: func(ctx context.Context, tenantID, deviceID string) (RPCClient, error) {
		return rpcStub{exec: func(ctx context.Context, args map[string]interface{}) (string, error) {
			return "ok", nil
		}}, nil
	}}
	s := NewServer(validatorStub{}, agg, router, policy, NewInMemoryHITLClient())
	w := doReq(t, s, "POST", "/v1/agent/mcp/tools/call", `{"name":"cnc-01::read_status","arguments":{}}`, "good-key")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", w.Code, w.Body.String())
	}
	var env struct {
		RequestID string `json:"request_id"`
		Content   []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"is_error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env.RequestID == "" {
		t.Fatalf("missing request_id envelope: %s", w.Body.String())
	}
	if len(env.Content) == 0 || env.Content[0].Text != "ok" || env.IsError {
		t.Fatalf("unexpected envelope result: %+v", env)
	}
}

// TestCrossTenantCallRejected: a device outside the caller's tenant is an
// explicit 403 code 13007 (design/33, SEC-02), not a 500.
func TestCrossTenantCallRejected(t *testing.T) {
	policy := DBPolicy{Lookup: func(ctx context.Context, tenantID, deviceID, toolName string) (int, error) {
		return 0, nil
	}}
	agg := DBAggregator{
		Lookup: func(ctx context.Context, tenantID, deviceID, toolName string) (int, error) {
			return 0, nil
		},
		OwnerCheck: func(ctx context.Context, tenantID, deviceCode string) error {
			return ErrDeviceNotOwned
		},
	}
	router := LocalRouter{Session: func(ctx context.Context, tenantID, deviceID string) (RPCClient, error) {
		t.Fatal("router must not be reached for a cross-tenant device")
		return nil, nil
	}}
	s := NewServer(validatorStub{}, agg, router, policy, NewInMemoryHITLClient())
	w := doReq(t, s, "POST", "/v1/agent/mcp/tools/call", `{"name":"other-tenant-cnc::set_rpm","arguments":{}}`, "good-key")
	if w.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), CodeCrossTenant) {
		t.Fatalf("want code %s in body: %s", CodeCrossTenant, w.Body.String())
	}
}
