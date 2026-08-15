package agentapi

import (
	"context"
	"encoding/json"
	"fmt"

	"adc.dev/core-sdk/protocol"
)

// ToolRows lists enabled tools of a tenant (design/31 3.2.3). Injected for
// testability; the concrete implementation queries adc_device_tools.
type ToolRows func(ctx context.Context, tenantID string) ([]ToolRow, error)

// ToolRow is a flattened row of adc_device_tools joined with the device code.
type ToolRow struct {
	DeviceCode  string
	ToolName    string
	Description string
	InputSchema json.RawMessage
	RiskLevel   int
	LongRunning bool
}

// DBAggregator is the V1.0 I5 implementation: DB is truth, cache is an
// optimization added later (design/31 3.2.3).
type DBAggregator struct {
	Rows       ToolRows
	Lookup     RiskLookup
	UUIDLookup DeviceUUIDLookup // fills DeviceUUID for audit (may be nil)
	OwnerCheck DeviceOwnerCheck // tenant ownership gate (may be nil)
}

// DeviceUUIDLookup resolves adc_devices.id by device_code.
type DeviceUUIDLookup func(ctx context.Context, tenantID, deviceCode string) (string, error)

// List returns tenant tools in the MCP shape. The qualified name uses the
// "__" display form for ecosystem compatibility (design/33).
func (a DBAggregator) List(ctx context.Context, tenantID string) ([]protocol.MCPTool, error) {
	rows, err := a.Rows(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("agentapi: list tools: %w", err)
	}
	out := make([]protocol.MCPTool, 0, len(rows))
	for _, r := range rows {
		out = append(out, protocol.MCPTool{
			Name:          r.DeviceCode + "__" + r.ToolName,
			Description:   r.Description,
			InputSchema:   r.InputSchema,
			RiskLevel:     &r.RiskLevel,
			SchemaVersion: "1",
		})
	}
	return out, nil
}

// Resolve parses the qualified name, rejects devices outside the caller's
// tenant (design/33 13007, SEC-02) and loads the persisted risk level
// (default 2, SEC-09).
func (a DBAggregator) Resolve(ctx context.Context, tenantID, qualifiedName string) (ToolRef, error) {
	ref, err := ParseToolRef(qualifiedName)
	if err != nil {
		return ToolRef{}, err
	}
	if a.OwnerCheck != nil {
		if err := a.OwnerCheck(ctx, tenantID, ref.DeviceID); err != nil {
			return ToolRef{}, err
		}
	}
	level, err := a.Lookup(ctx, tenantID, ref.DeviceID, ref.ToolName)
	if err != nil || level < 0 || level > 3 {
		level = 2
	}
	ref.RiskLevel = level
	if a.UUIDLookup != nil {
		if uuid, uerr := a.UUIDLookup(ctx, tenantID, ref.DeviceID); uerr == nil {
			ref.DeviceUUID = uuid
		}
	}
	return ref, nil
}

// ErrDeviceOffline marks a session lookup failure (device not connected).
var ErrDeviceOffline = fmt.Errorf("agentapi: device offline or not found")

// LocalRouter executes calls against the live session on this node
// (design/31 3.2.4 fast path; cross-node via MessageBus is wired in cmd).
type LocalRouter struct {
	Session SessionLookup
}

// Call sends tools/call to the device session and maps the JSON-RPC response.
func (r LocalRouter) Call(ctx context.Context, tenantID string, ref ToolRef, args map[string]interface{}) (*CallResult, error) {
	sess, err := r.Session(ctx, tenantID, ref.DeviceID)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrDeviceOffline, ref.DeviceID)
	}
	resp, err := sess.SendRPC(ctx, protocol.MethodToolsCall, protocol.ToolCallParams{
		Name:      ref.ToolName,
		Arguments: args,
	})
	if err != nil {
		return nil, fmt.Errorf("agentapi: rpc call: %w", err)
	}
	if resp.Error != nil {
		return &CallResult{IsError: true, Content: []protocol.ToolContent{{Type: "text", Text: resp.Error.Message}}}, nil
	}
	var out protocol.ToolCallResult
	if err := json.Unmarshal(resp.Result, &out); err != nil {
		return nil, fmt.Errorf("agentapi: decode tool result: %w", err)
	}
	return &CallResult{Content: out.Content, IsError: out.IsError}, nil
}
