package agentapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"adc.dev/ce/internal/httpx"
)

func numPtr(f float64) *float64 { return &f }

// spindleContract is the validation contract used across the tests: rpm
// is a required number between 0 and 12000, mode is an enum, enable is a
// boolean.
func spindleContract() *ParamContract {
	return &ParamContract{
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"rpm":    map[string]any{"type": "number"},
				"mode":   map[string]any{"type": "string"},
				"enable": map[string]any{"type": "boolean"},
			},
			"required": []any{"rpm"},
		},
		Rules: &ParamRules{Fields: map[string]FieldRule{
			"rpm":  {Min: numPtr(0), Max: numPtr(12000)},
			"mode": {Enum: []any{"manual", "auto"}},
		}},
	}
}

func TestValidateArgumentsRequiredMissing(t *testing.T) {
	c := spindleContract()
	v := validateArguments(c, map[string]interface{}{"mode": "auto"})
	if len(v) != 1 || v[0].Field != "rpm" || !strings.Contains(v[0].Message, "required") {
		t.Fatalf("violations = %+v, want a required error on rpm", v)
	}
}

func TestValidateArgumentsTypeMismatch(t *testing.T) {
	c := spindleContract()
	v := validateArguments(c, map[string]interface{}{"rpm": "fast"})
	if !hasViolation(v, "rpm", "number") {
		t.Fatalf("violations = %+v, want a type error on rpm", v)
	}
	// integer schema rejects fractional numbers.
	cInt := &ParamContract{InputSchema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"count": map[string]any{"type": "integer"},
		},
	}}
	v = validateArguments(cInt, map[string]interface{}{"count": 1.5})
	if len(v) != 1 || v[0].Field != "count" {
		t.Fatalf("violations = %+v, want an integer error on count", v)
	}
	// integer schema accepts whole numbers.
	if v = validateArguments(cInt, map[string]interface{}{"count": 4.0}); len(v) != 0 {
		t.Fatalf("whole float must pass the integer check: %+v", v)
	}
}

func TestValidateArgumentsRangeAndEnum(t *testing.T) {
	c := spindleContract()
	v := validateArguments(c, map[string]interface{}{"rpm": 99999.0})
	if len(v) != 1 || v[0].Field != "rpm" || !strings.Contains(v[0].Message, "maximum") {
		t.Fatalf("violations = %+v, want a maximum error on rpm", v)
	}
	v = validateArguments(c, map[string]interface{}{"rpm": -5.0})
	if len(v) != 1 || v[0].Field != "rpm" || !strings.Contains(v[0].Message, "minimum") {
		t.Fatalf("violations = %+v, want a minimum error on rpm", v)
	}
	v = validateArguments(c, map[string]interface{}{"rpm": 3000.0, "mode": "turbo"})
	if len(v) != 1 || v[0].Field != "mode" || !strings.Contains(v[0].Message, "enumeration") {
		t.Fatalf("violations = %+v, want an enum error on mode", v)
	}
	// A min/max rule on a non-numeric value is a violation.
	v = validateArguments(c, map[string]interface{}{"rpm": "full", "mode": "auto"})
	if !hasViolation(v, "rpm", "numeric") {
		t.Fatalf("violations = %+v, want a numeric-rule error on rpm", v)
	}
}

// hasViolation reports whether the field appears with a message
// containing the fragment.
func hasViolation(list []ParamViolation, field, fragment string) bool {
	for _, v := range list {
		if v.Field == field && strings.Contains(v.Message, fragment) {
			return true
		}
	}
	return false
}

func TestValidateArgumentsValidPasses(t *testing.T) {
	c := spindleContract()
	v := validateArguments(c, map[string]interface{}{"rpm": 3000.0, "mode": "auto", "enable": true})
	if len(v) != 0 {
		t.Fatalf("valid arguments rejected: %+v", v)
	}
	// Unknown keys pass the coarse check (C5.2 does not reject extras).
	v = validateArguments(c, map[string]interface{}{"rpm": 1.0, "custom": "anything"})
	if len(v) != 0 {
		t.Fatalf("unknown key rejected: %+v", v)
	}
}

func TestParseParamRules(t *testing.T) {
	annotations := map[string]any{
		"adc_param_rules": map[string]any{
			"rpm":      map[string]any{"min": float64(0), "max": float64(12000)},
			"mode":     map[string]any{"enum": []any{"manual", "auto"}},
			"broken":   map[string]any{"min": "not-a-number"},
			"nonsense": "not-a-map",
		},
		"i18n": map[string]any{"zh": "desc"},
	}
	rules := ParseParamRules(annotations)
	if rules == nil {
		t.Fatal("rules must parse")
	}
	if len(rules.Fields) != 2 {
		t.Fatalf("fields = %v, want rpm and mode only", rules.Fields)
	}
	if rules.Fields["rpm"].Min == nil || *rules.Fields["rpm"].Min != 0 ||
		rules.Fields["rpm"].Max == nil || *rules.Fields["rpm"].Max != 12000 {
		t.Fatalf("rpm rule = %+v, want min 0 max 12000", rules.Fields["rpm"])
	}
	if len(rules.Fields["mode"].Enum) != 2 {
		t.Fatalf("mode enum = %v, want 2 entries", rules.Fields["mode"].Enum)
	}
	if ParseParamRules(nil) != nil {
		t.Fatal("nil annotations must parse to nil")
	}
	if ParseParamRules(map[string]any{"other": 1}) != nil {
		t.Fatal("annotations without adc_param_rules must parse to nil")
	}
}

func TestSchemaParamGuardNilProviderSkips(t *testing.T) {
	g := SchemaParamGuard{}
	perr, err := g.Validate(context.Background(), "t", ToolRef{DeviceID: "d", ToolName: "n"}, map[string]interface{}{})
	if err != nil || perr != nil {
		t.Fatalf("nil provider must skip validation: perr=%v err=%v", perr, err)
	}
}

func TestSchemaParamGuardProviderError(t *testing.T) {
	g := SchemaParamGuard{Contract: func(ctx context.Context, tenantID, deviceID, toolName string) (*ParamContract, error) {
		return nil, errors.New("db down")
	}}
	perr, err := g.Validate(context.Background(), "t", ToolRef{DeviceID: "d", ToolName: "n"}, nil)
	if err == nil {
		t.Fatal("provider error must surface as an internal error")
	}
	if perr != nil {
		t.Fatal("provider error is not a violation")
	}
}

// guardTestServer builds a server with the spindle contract wired as the
// ParamGuard; policy decides low-risk so valid calls execute directly.
func guardTestServer(contract *ParamContract, policyLevel int) *Server {
	policy := DBPolicy{Lookup: func(ctx context.Context, tenantID, deviceID, toolName string) (int, error) {
		return policyLevel, nil
	}}
	agg := DBAggregator{
		Rows: func(ctx context.Context, tenantID string) ([]ToolRow, error) { return nil, nil },
		Lookup: func(ctx context.Context, tenantID, deviceID, toolName string) (int, error) {
			return policyLevel, nil
		},
	}
	router := LocalRouter{Session: func(ctx context.Context, tenantID, deviceID string) (RPCClient, error) {
		return rpcStub{exec: func(ctx context.Context, args map[string]interface{}) (string, error) {
			return "executed", nil
		}}, nil
	}}
	s := NewServer(validatorStub{}, agg, router, policy, NewInMemoryHITLClient())
	if contract != nil {
		s.Guard = SchemaParamGuard{Contract: func(ctx context.Context, tenantID, deviceID, toolName string) (*ParamContract, error) {
			return contract, nil
		}}
	}
	return s
}

func callTool(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	return doReq(t, s, "POST", "/v1/agent/mcp/tools/call", body, "good-key")
}

func TestParamGuardRejectsMissingRequired(t *testing.T) {
	s := guardTestServer(spindleContract(), 0)
	w := callTool(t, s, `{"name":"cnc-01::set_rpm","arguments":{"mode":"auto"}}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d body=%s", w.Code, w.Body.String())
	}
	var body paramErrorBody
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not a param error body: %v", err)
	}
	if body.Code != CodeInvalidParams {
		t.Fatalf("code = %q, want %q", body.Code, CodeInvalidParams)
	}
	if len(body.Fields) != 1 || body.Fields[0].Field != "rpm" {
		t.Fatalf("fields = %+v, want one rpm violation", body.Fields)
	}
}

func TestParamGuardRejectsTypeError(t *testing.T) {
	s := guardTestServer(spindleContract(), 0)
	w := callTool(t, s, `{"name":"cnc-01::set_rpm","arguments":{"rpm":"fast"}}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), CodeInvalidParams) || !strings.Contains(w.Body.String(), "number") {
		t.Fatalf("body = %s, want code %s with a type message", w.Body.String(), CodeInvalidParams)
	}
}

func TestParamGuardRejectsRangeViolation(t *testing.T) {
	s := guardTestServer(spindleContract(), 0)
	w := callTool(t, s, `{"name":"cnc-01::set_rpm","arguments":{"rpm":99999}}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), CodeInvalidParams) || !strings.Contains(w.Body.String(), "maximum") {
		t.Fatalf("body = %s, want a maximum violation", w.Body.String())
	}
}

func TestParamGuardAllowsValidArguments(t *testing.T) {
	s := guardTestServer(spindleContract(), 0)
	w := callTool(t, s, `{"name":"cnc-01::set_rpm","arguments":{"rpm":3000,"mode":"auto"}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "executed") {
		t.Fatalf("router was not reached: %s", w.Body.String())
	}
}

// TestParamGuardBlocksBeforeHITLTicket: on the approval path the
// validation runs before Policy.Decide, so invalid arguments never
// create a ticket.
func TestParamGuardBlocksBeforeHITLTicket(t *testing.T) {
	s := guardTestServer(spindleContract(), 2) // high risk: approval required
	w := callTool(t, s, `{"name":"cnc-01::set_rpm","arguments":{"rpm":99999}}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d body=%s", w.Code, w.Body.String())
	}
	hitl, ok := s.HITL.(*InMemoryHITLClient)
	if !ok {
		t.Fatal("HITL client is not the in-memory fake")
	}
	if len(hitl.tickets) != 0 {
		t.Fatalf("invalid call created %d tickets, want 0", len(hitl.tickets))
	}
}

// TestNilGuardSkipsValidation: a server without ParamGuard keeps its
// legacy behavior (arguments pass through untouched).
func TestNilGuardSkipsValidation(t *testing.T) {
	s := guardTestServer(nil, 0)
	w := callTool(t, s, `{"name":"cnc-01::set_rpm","arguments":{"rpm":"anything"}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", w.Code, w.Body.String())
	}
}

// TestParamGuardProviderErrorIsInternal: contract loading failures are
// 500, not 400.
func TestParamGuardProviderErrorIsInternal(t *testing.T) {
	s := guardTestServer(nil, 0)
	s.Guard = SchemaParamGuard{Contract: func(ctx context.Context, tenantID, deviceID, toolName string) (*ParamContract, error) {
		return nil, errors.New("db down")
	}}
	w := callTool(t, s, `{"name":"cnc-01::set_rpm","arguments":{"rpm":1}}`)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d body=%s", w.Code, w.Body.String())
	}
	var body httpx.ErrorBody
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not an error body: %v", err)
	}
	if body.Code != "10006" {
		t.Fatalf("code = %q, want 10006", body.Code)
	}
}
