// Package route wires the cross-node tool call path of the Agent API
// (design/31 3.2.5, B-08): the local session is the fast path — message
// layer outages never affect devices connected to this node (doc/05
// chapter 8) — and only a session that is not local falls back to the
// Valkey routing index (adc:loc:{tenant}:{deviceCode}) and a signed
// cross-node Request over the MessageBus (SEC-06).
package route

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"adc.dev/core-sdk/protocol"

	"adc.dev/ce/internal/agentapi"
	"adc.dev/ce/pkg/clusterbus"
)

// TopicToolCall is the cross-node tool call topic (design/31 1.3.7: the
// wire channel is adc:req:{node}, the topic is carried in the envelope).
const TopicToolCall = "adc:tools:call"

// DefaultCallTimeout mirrors the configurable ADC_TOOL_CALL_TIMEOUT
// default (design/31 3.2.6).
const DefaultCallTimeout = 15 * time.Second

// ErrNodeTimeout marks a cross-node call whose target node never
// answered (design/31 3.2.8: 504, retryable — the device command was
// never confirmed, the caller decides whether to resend).
var ErrNodeTimeout = errors.New("route: target node did not respond in time")

// Locator resolves the node owning a device session from the routing
// index; clusterbus.NodeRegistry is the Valkey implementation.
type Locator interface {
	Locate(ctx context.Context, tenantID, deviceCode string) (string, error)
}

// ClusterRouter implements agentapi.ToolRouter with local-first routing
// (B-08). CallResult semantics stay identical to LocalRouter: content,
// is_error, and the long-task fields (ADR-10 seam) all survive the
// round-trip.
//
// Serve must run on every node (a background goroutine in cmd wiring) so
// the node answers remote tool calls; without it the node only sends.
type ClusterRouter struct {
	Local   agentapi.ToolRouter // local fast path (LocalRouter over the hub)
	Locator Locator             // loc-index node resolution
	Bus     clusterbus.MessageBus
	NodeID  string // this node's id (the value stored in its loc keys)

	// CallTimeout bounds the cross-node wait; 0 selects DefaultCallTimeout.
	CallTimeout time.Duration
}

func (r *ClusterRouter) timeout() time.Duration {
	if r.CallTimeout > 0 {
		return r.CallTimeout
	}
	return DefaultCallTimeout
}

// Call routes a tool call per design/31 3.2.5. A local session hit never
// touches the message layer; a miss resolves the owning node from the loc
// index and, when it is another node, forwards via the signed bus
// Request. No response means ErrNodeTimeout (504, no automatic retry).
func (r *ClusterRouter) Call(ctx context.Context, tenantID string, ref agentapi.ToolRef, args map[string]interface{}) (*agentapi.CallResult, error) {
	result, err := r.Local.Call(ctx, tenantID, ref, args)
	if err == nil || !errors.Is(err, agentapi.ErrDeviceOffline) {
		return result, err
	}
	node, err := r.Locator.Locate(ctx, tenantID, ref.DeviceID)
	if err != nil || node == "" || node == r.NodeID {
		// No route at all, or the index points back here: the device is
		// offline from this node's perspective (SEC-14: index TTL
		// guarantees misjudgement is rare).
		return nil, fmt.Errorf("%w: %s", agentapi.ErrDeviceOffline, ref.DeviceID)
	}
	payload, err := json.Marshal(callRequest{
		TenantID:  tenantID,
		DeviceID:  ref.DeviceID,
		ToolName:  ref.ToolName,
		Arguments: args,
	})
	if err != nil {
		return nil, fmt.Errorf("route: marshal call request: %w", err)
	}
	raw, err := r.Bus.Request(ctx, node, TopicToolCall, payload, r.timeout())
	if errors.Is(err, clusterbus.ErrTimeout) {
		return nil, ErrNodeTimeout
	}
	if err != nil {
		return nil, fmt.Errorf("route: cross-node call: %w", err)
	}
	var resp callResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("route: decode remote call result: %w", err)
	}
	return &agentapi.CallResult{
		Content:  resp.Content,
		IsError:  resp.IsError,
		Accepted: resp.Accepted,
		TaskID:   resp.TaskID,
	}, nil
}

// Serve registers this node's tool call handler on the bus and blocks
// until ctx is done (cmd wiring runs it as a goroutine). The bus must
// implement clusterbus.Responder (ValkeyBus does).
func (r *ClusterRouter) Serve(ctx context.Context) error {
	responder, ok := r.Bus.(clusterbus.Responder)
	if !ok {
		return errors.New("route: bus does not support request handlers (need clusterbus.Responder)")
	}
	return responder.RegisterHandler(ctx, TopicToolCall, r.handleRemoteCall)
}

// callRequest is the cross-node payload for a tool call.
type callRequest struct {
	TenantID  string                 `json:"tenant_id"`
	DeviceID  string                 `json:"device_id"`
	ToolName  string                 `json:"tool_name"`
	Arguments map[string]interface{} `json:"arguments"`
}

// callResponse mirrors agentapi.CallResult on the wire.
type callResponse struct {
	Content  []protocol.ToolContent `json:"content,omitempty"`
	IsError  bool                   `json:"is_error,omitempty"`
	Accepted bool                   `json:"accepted,omitempty"`
	TaskID   string                 `json:"task_id,omitempty"`
}

// handleRemoteCall executes a call arriving from another node on the
// local fast path. The signature check already happened in the bus
// (SEC-06); the tenant/device pair is resolved against the local hub.
func (r *ClusterRouter) handleRemoteCall(ctx context.Context, payload []byte) ([]byte, error) {
	var req callRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return nil, fmt.Errorf("route: decode remote call request: %w", err)
	}
	ref := agentapi.ToolRef{DeviceID: req.DeviceID, ToolName: req.ToolName}
	result, err := r.Local.Call(ctx, req.TenantID, ref, req.Arguments)
	if err != nil {
		return nil, err
	}
	out := callResponse{
		IsError:  result.IsError,
		Accepted: result.Accepted,
		TaskID:   result.TaskID,
	}
	for _, c := range result.Content {
		out.Content = append(out.Content, protocol.ToolContent{Type: c.Type, Text: c.Text})
	}
	return json.Marshal(out)
}
