package route

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"adc.dev/core-sdk/protocol"

	"adc.dev/ce/internal/agentapi"
	"adc.dev/ce/pkg/clusterbus"
)

var testNodeKey = []byte("test-node-signing-key")

// stubLocalRouter returns a canned result and records invocations.
type stubLocalRouter struct {
	calls   atomic.Int64
	offline bool // return agentapi.ErrDeviceOffline
	result  *agentapi.CallResult
}

func (s *stubLocalRouter) Call(ctx context.Context, tenantID string, ref agentapi.ToolRef, args map[string]interface{}) (*agentapi.CallResult, error) {
	s.calls.Add(1)
	if s.offline {
		return nil, agentapi.ErrDeviceOffline
	}
	return s.result, nil
}

type mapLocator map[string]string // locKey -> nodeID

func (m mapLocator) Locate(ctx context.Context, tenantID, deviceCode string) (string, error) {
	v, ok := m[clusterbus.LocKey(tenantID, deviceCode)]
	if !ok {
		return "", clusterbus.ErrNotFound
	}
	return v, nil
}

func newTestBus(nodeID string, backend clusterbus.Backend) *clusterbus.ValkeyBus {
	return clusterbus.NewValkeyBus(clusterbus.BusConfig{
		NodeID:  nodeID,
		NodeKey: testNodeKey,
		Backend: backend,
		MaxSkew: time.Minute,
	})
}

// countingLocator records Lookup invocations so tests can assert the fast
// path never consults the index.
type countingLocator struct {
	calls atomic.Int64
	m     mapLocator
}

func (c *countingLocator) Locate(ctx context.Context, tenantID, deviceCode string) (string, error) {
	c.calls.Add(1)
	return c.m.Locate(ctx, tenantID, deviceCode)
}

func TestClusterRouterLocalFastPath(t *testing.T) {
	local := &stubLocalRouter{result: &agentapi.CallResult{
		Content: []protocol.ToolContent{{Type: "text", Text: "ok"}},
	}}
	locator := &countingLocator{m: mapLocator{clusterbus.LocKey("t1", "dev-1"): "node-b"}}
	r := &ClusterRouter{
		Local:   local,
		Locator: locator,
		Bus:     newTestBus("node-a", clusterbus.NewMemBackend()),
		NodeID:  "node-a",
	}

	res, err := r.Call(context.Background(), "t1", agentapi.ToolRef{DeviceID: "dev-1", ToolName: "run"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Content) != 1 || res.Content[0].Text != "ok" {
		t.Fatalf("unexpected result: %+v", res)
	}
	if local.calls.Load() != 1 {
		t.Fatalf("local router called %d times, want 1", local.calls.Load())
	}
	// The fast path must not consult the loc index: the message layer
	// (and the index) may be down while local devices keep working
	// (doc/05 chapter 8).
	if locator.calls.Load() != 0 {
		t.Fatalf("locator consulted %d times on the fast path, want 0", locator.calls.Load())
	}
}

func TestClusterRouterCrossNode(t *testing.T) {
	backend := clusterbus.NewMemBackend()
	busA := newTestBus("node-a", backend)
	busB := newTestBus("node-b", backend)
	defer busA.Close()
	defer busB.Close()

	remoteLocal := &stubLocalRouter{result: &agentapi.CallResult{
		Content:  []protocol.ToolContent{{Type: "text", Text: "remote-ok"}},
		Accepted: true,
		TaskID:   "task-7",
	}}
	remote := &ClusterRouter{
		Local:  remoteLocal,
		Bus:    busB,
		NodeID: "node-b",
	}
	ctxB, cancelB := context.WithCancel(context.Background())
	defer cancelB()
	go func() { _ = remote.Serve(ctxB) }()
	if !backend.WaitSubscription(clusterbus.RequestChannel("node-b"), 1, 2*time.Second) {
		t.Fatal("node-b never subscribed its request channel")
	}

	local := &stubLocalRouter{offline: true}
	locator := mapLocator{clusterbus.LocKey("t1", "dev-1"): "node-b"}
	r := &ClusterRouter{
		Local:   local,
		Locator: locator,
		Bus:     busA,
		NodeID:  "node-a",
	}

	res, err := r.Call(context.Background(), "t1",
		agentapi.ToolRef{DeviceID: "dev-1", ToolName: "run"}, map[string]interface{}{"rpm": 1200})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Content) != 1 || res.Content[0].Text != "remote-ok" {
		t.Fatalf("unexpected remote result: %+v", res)
	}
	if !res.Accepted || res.TaskID != "task-7" {
		t.Fatalf("long-task fields lost across nodes: %+v", res)
	}
	if local.calls.Load() != 1 {
		t.Fatalf("local fast path attempted %d times, want 1", local.calls.Load())
	}
	if remoteLocal.calls.Load() != 1 {
		t.Fatalf("remote node executed %d calls, want 1", remoteLocal.calls.Load())
	}
}

func TestClusterRouterIndexMissStaysOffline(t *testing.T) {
	r := &ClusterRouter{
		Local:   &stubLocalRouter{offline: true},
		Locator: mapLocator{}, // nothing indexed
		Bus:     newTestBus("node-a", clusterbus.NewMemBackend()),
		NodeID:  "node-a",
	}
	_, err := r.Call(context.Background(), "t1", agentapi.ToolRef{DeviceID: "dev-x"}, nil)
	if !errors.Is(err, agentapi.ErrDeviceOffline) {
		t.Fatalf("want ErrDeviceOffline, got %v", err)
	}
}

func TestClusterRouterNodeTimeout(t *testing.T) {
	backend := clusterbus.NewMemBackend()
	busA := newTestBus("node-a", backend)
	defer busA.Close()

	r := &ClusterRouter{
		Local:       &stubLocalRouter{offline: true},
		Locator:     mapLocator{clusterbus.LocKey("t1", "dev-1"): "node-ghost"},
		Bus:         busA,
		NodeID:      "node-a",
		CallTimeout: 50 * time.Millisecond,
	}
	_, err := r.Call(context.Background(), "t1", agentapi.ToolRef{DeviceID: "dev-1"}, nil)
	if !errors.Is(err, ErrNodeTimeout) {
		t.Fatalf("want ErrNodeTimeout, got %v", err)
	}
}

func TestClusterRouterRemoteCallError(t *testing.T) {
	backend := clusterbus.NewMemBackend()
	busA := newTestBus("node-a", backend)
	busB := newTestBus("node-b", backend)
	defer busA.Close()
	defer busB.Close()

	// Remote node executes but the device is offline there: the handler
	// error travels back to the caller.
	remote := &ClusterRouter{
		Local:  &stubLocalRouter{offline: true},
		Bus:    busB,
		NodeID: "node-b",
	}
	ctxB, cancelB := context.WithCancel(context.Background())
	defer cancelB()
	go func() { _ = remote.Serve(ctxB) }()
	if !backend.WaitSubscription(clusterbus.RequestChannel("node-b"), 1, 2*time.Second) {
		t.Fatal("node-b never subscribed its request channel")
	}

	r := &ClusterRouter{
		Local:   &stubLocalRouter{offline: true},
		Locator: mapLocator{clusterbus.LocKey("t1", "dev-1"): "node-b"},
		Bus:     busA,
		NodeID:  "node-a",
	}
	_, err := r.Call(context.Background(), "t1", agentapi.ToolRef{DeviceID: "dev-1"}, nil)
	if err == nil {
		t.Fatal("want remote handler error")
	}
	if errors.Is(err, ErrNodeTimeout) {
		t.Fatalf("remote error must not be masked as a timeout: %v", err)
	}
}

// noResponderBus implements only the frozen MessageBus interface.
type noResponderBus struct{ clusterbus.MessageBus }

func TestServeRequiresResponder(t *testing.T) {
	r := &ClusterRouter{Bus: noResponderBus{}}
	err := r.Serve(context.Background())
	if err == nil {
		t.Fatal("Serve must fail when the bus cannot host handlers")
	}
}
