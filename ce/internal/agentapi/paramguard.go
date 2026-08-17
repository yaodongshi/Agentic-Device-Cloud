package agentapi

// Parameter-level validation for tool calls (design/83 C5.2, LiteLLM
// Guardrails equivalent): before a call reaches Router.Call the
// arguments are checked against the tool's input_schema JSON Schema
// (required fields, coarse type checks) plus platform-side whitelist
// rules (min/max/enum) persisted in adc_device_tools.annotations JSONB
// under the adc_param_rules key. Violations answer 400 code 12004 with
// field-level errors; the seam is injected so the wire layer decides
// where the metadata comes from.

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"reflect"
	"strings"

	"adc.dev/ce/internal/httpx"
)

// CodeInvalidParams is the unified tool-call parameter validation error
// code (design/83 C5.2: 400 with field-level errors).
const CodeInvalidParams = "12004"

// FieldRule is one whitelist rule for a single argument field
// (annotations.adc_param_rules: min / max / enum).
type FieldRule struct {
	Min  *float64
	Max  *float64
	Enum []any
}

// ParamRules groups per-field whitelist rules.
type ParamRules struct {
	Fields map[string]FieldRule
}

// ParamContract is the validation contract for one tool: the raw MCP
// input_schema (design/32 3.6 input_schema JSONB) plus the optional
// whitelist rules from annotations.
type ParamContract struct {
	InputSchema map[string]any
	Rules       *ParamRules
}

// ParamRulesProvider loads the contract from tool metadata
// (adc_device_tools row: input_schema column, annotations JSONB). It is
// the seam between the guard and the persistence layer; a nil contract
// disables validation for that tool.
type ParamRulesProvider func(ctx context.Context, tenantID, deviceID, toolName string) (*ParamContract, error)

// ParamViolation is one field-level error (design/83 C5.2).
type ParamViolation struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

// ParamError aggregates the violations of one call. It is returned by
// ParamGuard.Validate as a non-nil value together with a nil error.
type ParamError struct {
	Violations []ParamViolation
}

func (e *ParamError) Error() string {
	if e == nil {
		return ""
	}
	parts := make([]string, 0, len(e.Violations))
	for _, v := range e.Violations {
		parts = append(parts, v.Field+": "+v.Message)
	}
	return "invalid tool call arguments: " + strings.Join(parts, "; ")
}

// ParamGuard validates tool call arguments before execution.
type ParamGuard interface {
	// Validate checks args against the tool contract. Returns
	// (*ParamError, nil) on violations, (nil, error) when the contract
	// cannot be loaded (internal failure), and (nil, nil) on success.
	Validate(ctx context.Context, tenantID string, ref ToolRef, args map[string]interface{}) (*ParamError, error)
}

// SchemaParamGuard is the V1.0 ParamGuard: JSON Schema coarse checks
// plus annotation whitelist rules (C5.2).
type SchemaParamGuard struct {
	Contract ParamRulesProvider
}

// Validate loads the contract and runs the schema and rule checks. A
// nil provider (or nil contract) skips validation entirely.
func (g SchemaParamGuard) Validate(ctx context.Context, tenantID string, ref ToolRef, args map[string]interface{}) (*ParamError, error) {
	if g.Contract == nil {
		return nil, nil
	}
	c, err := g.Contract(ctx, tenantID, ref.DeviceID, ref.ToolName)
	if err != nil {
		return nil, err
	}
	if c == nil {
		return nil, nil
	}
	v := validateArguments(c, args)
	if len(v) == 0 {
		return nil, nil
	}
	return &ParamError{Violations: v}, nil
}

// ParseParamRules extracts ParamRules from adc_device_tools.annotations
// (JSONB decoded to map[string]any): {"adc_param_rules": {"rpm":
// {"min": 0, "max": 12000}, "mode": {"enum": ["manual", "auto"]}}}.
// Malformed entries are skipped, never fatal: device-originated
// metadata must not take down the call path.
func ParseParamRules(annotations map[string]any) *ParamRules {
	if annotations == nil {
		return nil
	}
	raw, ok := annotations["adc_param_rules"].(map[string]any)
	if !ok || len(raw) == 0 {
		return nil
	}
	rules := &ParamRules{Fields: make(map[string]FieldRule, len(raw))}
	for field, v := range raw {
		fm, ok := v.(map[string]any)
		if !ok {
			continue
		}
		var fr FieldRule
		if mv, ok := fm["min"]; ok {
			if f, ok := asNumber(mv); ok {
				fr.Min = &f
			}
		}
		if mv, ok := fm["max"]; ok {
			if f, ok := asNumber(mv); ok {
				fr.Max = &f
			}
		}
		if ev, ok := fm["enum"]; ok {
			if list, ok := ev.([]any); ok {
				fr.Enum = list
			}
		}
		if fr.Min == nil && fr.Max == nil && len(fr.Enum) == 0 {
			continue
		}
		rules.Fields[field] = fr
	}
	if len(rules.Fields) == 0 {
		return nil
	}
	return rules
}

// validateArguments applies the JSON Schema coarse checks (required +
// type) and the whitelist rules (min/max/enum). Unknown argument keys
// are not rejected (coarse check, C5.2): the schema declares types only
// for known properties.
func validateArguments(c *ParamContract, args map[string]interface{}) []ParamViolation {
	var out []ParamViolation
	if schema := c.InputSchema; schema != nil {
		if props, ok := schema["properties"].(map[string]any); ok {
			if req, ok := schema["required"].([]any); ok {
				for _, r := range req {
					name, ok := r.(string)
					if !ok {
						continue
					}
					if _, present := args[name]; !present {
						out = append(out, ParamViolation{Field: name, Message: "required field is missing"})
					}
				}
			}
			for name, v := range args {
				ps, ok := props[name].(map[string]any)
				if !ok {
					continue
				}
				if msg, bad := typeMismatch(v, ps["type"]); bad {
					out = append(out, ParamViolation{Field: name, Message: msg})
				}
			}
		}
	}
	if c.Rules != nil {
		for name, v := range args {
			rule, ok := c.Rules.Fields[name]
			if !ok {
				continue
			}
			out = append(out, applyFieldRule(name, v, rule)...)
		}
	}
	return out
}

// typeMismatch reports a coarse type failure for one of the JSON Schema
// scalar types (C5.2 covers number/string/boolean/integer; other types
// pass through untouched).
func typeMismatch(v interface{}, typ any) (string, bool) {
	s, ok := typ.(string)
	if !ok || s == "" {
		return "", false
	}
	switch s {
	case "string":
		if _, ok := v.(string); !ok {
			return "expected a string", true
		}
	case "boolean":
		if _, ok := v.(bool); !ok {
			return "expected a boolean", true
		}
	case "number":
		if _, ok := asNumber(v); !ok {
			return "expected a number", true
		}
	case "integer":
		if _, ok := asInteger(v); !ok {
			return "expected an integer", true
		}
	}
	return "", false
}

// asNumber coerces JSON numeric spellings (float64 from encoding/json,
// json.Number, Go ints) to float64.
func asNumber(v interface{}) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		if err != nil {
			return 0, false
		}
		return f, true
	}
	return 0, false
}

// asInteger accepts numbers with no fractional part.
func asInteger(v interface{}) (int64, bool) {
	f, ok := asNumber(v)
	if !ok || math.Trunc(f) != f {
		return 0, false
	}
	return int64(f), true
}

// applyFieldRule enforces min/max (numeric fields) and enum whitelists.
func applyFieldRule(name string, v interface{}, r FieldRule) []ParamViolation {
	var out []ParamViolation
	if r.Min != nil || r.Max != nil {
		f, ok := asNumber(v)
		if !ok {
			out = append(out, ParamViolation{Field: name, Message: "min/max rule requires a numeric value"})
			return out
		}
		if r.Min != nil && f < *r.Min {
			out = append(out, ParamViolation{Field: name, Message: fmt.Sprintf("value %v is below the minimum %v", v, *r.Min)})
		}
		if r.Max != nil && f > *r.Max {
			out = append(out, ParamViolation{Field: name, Message: fmt.Sprintf("value %v is above the maximum %v", v, *r.Max)})
		}
	}
	if len(r.Enum) > 0 && !enumContains(r.Enum, v) {
		out = append(out, ParamViolation{Field: name, Message: "value is not in the allowed enumeration"})
	}
	return out
}

// enumContains compares whitelist entries with numeric equality for
// numbers and deep equality otherwise.
func enumContains(enum []any, v interface{}) bool {
	for _, e := range enum {
		if equalValues(e, v) {
			return true
		}
	}
	return false
}

func equalValues(a, b interface{}) bool {
	if af, aok := asNumber(a); aok {
		bf, bok := asNumber(b)
		return bok && af == bf
	}
	return reflect.DeepEqual(a, b)
}

// paramErrorBody extends the unified error envelope (design/33) with the
// field-level violations of C5.2.
type paramErrorBody struct {
	Code    string           `json:"code"`
	Message string           `json:"message"`
	TraceID string           `json:"trace_id,omitempty"`
	Fields  []ParamViolation `json:"fields,omitempty"`
}

// writeParamError emits the 400 code 12004 answer.
func writeParamError(w http.ResponseWriter, r *http.Request, perr *ParamError) {
	body := paramErrorBody{
		Code:    CodeInvalidParams,
		Message: "invalid tool call arguments",
		TraceID: httpx.TraceIDFrom(r),
	}
	if perr != nil {
		body.Fields = perr.Violations
	}
	httpx.WriteJSON(w, http.StatusBadRequest, body)
}
