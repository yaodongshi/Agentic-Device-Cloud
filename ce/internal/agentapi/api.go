// Package agentapi implements the Agent API (MCP tools endpoints) per
// design/31 3.2: aggregated tool listing, structured tool ref resolution,
// DB-backed risk policy (SEC-09) and the HITL-pending call flow with
// BLOCKED_BY_HITL (design/33).
package agentapi

import (
	"context"
	"errors"
	"strings"

	"adc.dev/core-sdk/protocol"
)

// ToolRef is the structured device/tool reference (design/31 3.2.2).
type ToolRef struct {
	DeviceID    string // device business code (device_code)
	DeviceUUID  string // adc_devices.id, for audit FK-shaped columns
	ToolName    string
	RiskLevel   int
	LongRunning bool
}

// ErrInvalidToolRef marks a qualified name that cannot be parsed.
var ErrInvalidToolRef = errors.New("agentapi: invalid tool reference")

// ParseToolRef resolves a qualified name. Both "deviceCode::toolName"
// (structured, SEC-24) and the legacy "__" separator are accepted.
func ParseToolRef(qualifiedName string) (ToolRef, error) {
	sep := "::"
	if !strings.Contains(qualifiedName, sep) {
		sep = "__"
	}
	parts := strings.SplitN(qualifiedName, sep, 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return ToolRef{}, ErrInvalidToolRef
	}
	return ToolRef{DeviceID: parts[0], ToolName: parts[1]}, nil
}

// Decision is the risk policy outcome (design/31 3.2.2).
type Decision struct {
	Level           int
	RequireApproval bool
	Reason          string
}

// RiskPolicy decides whether a tool call needs HITL (I7, SEC-09).
type RiskPolicy interface {
	Decide(ctx context.Context, tenantID string, ref ToolRef) (Decision, error)
}

// RiskLookup loads the persisted risk level for a tool (SEC-09: DB is the
// single source of truth, devices only suggest). It is injected so tests can
// mock the DB access without importing a concrete repository.
type RiskLookup func(ctx context.Context, tenantID, deviceID, toolName string) (int, error)

// RiskNotFound is returned by RiskLookup when no level is persisted.
var RiskNotFound = errors.New("agentapi: risk level not configured")

// DBPolicy is the V1.0 RiskPolicy: DB metadata with default level 2
// ("先审后用", SEC-09). OPA replaces it in phase 2 (ADR-07 seam).
type DBPolicy struct {
	Lookup RiskLookup
}

// Decide returns level>=2 requiring approval. Missing rows default to 2.
func (p DBPolicy) Decide(ctx context.Context, tenantID string, ref ToolRef) (Decision, error) {
	level, err := p.Lookup(ctx, tenantID, ref.DeviceID, ref.ToolName)
	if err != nil {
		if errors.Is(err, RiskNotFound) {
			level = 2
		} else {
			return Decision{}, err
		}
	}
	if level < 0 || level > 3 {
		level = 2
	}
	d := Decision{Level: level, RequireApproval: level >= 2}
	d.Reason = "db_risk_level"
	return d, nil
}

// ToolAggregator lists and resolves a tenant's enabled tools (I5).
type ToolAggregator interface {
	List(ctx context.Context, tenantID string) ([]protocol.MCPTool, error)
	Resolve(ctx context.Context, tenantID, qualifiedName string) (ToolRef, error)
}

// RPCClient is the minimal session interface the router needs (design/31
// 3.2.2); connector.DeviceSession satisfies it.
type RPCClient interface {
	SendRPC(ctx context.Context, method string, params interface{}) (*protocol.JSONRPCResponse, error)
}

// SessionLookup locates the live session for a device.
type SessionLookup func(ctx context.Context, tenantID, deviceID string) (RPCClient, error)

// CallResult wraps a device tool call outcome (I6).
type CallResult struct {
	Content  []protocol.ToolContent
	IsError  bool
	Accepted bool
	TaskID   string
}

// ToolRouter executes a tool call (I6).
type ToolRouter interface {
	Call(ctx context.Context, tenantID string, ref ToolRef, args map[string]interface{}) (*CallResult, error)
}
