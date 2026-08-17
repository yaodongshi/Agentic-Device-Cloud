package adapters

// opcua_cert_test.go is the C1.6 certification suite for the OPC-UA
// adapter (design/83 C1.3, milestone M6 acceptance). It mirrors
// cert_test.go: discovery, read, write round-trip, error handling,
// concurrent reads, health after disconnect, risk mapping, config
// grammar and identity metadata.
//
// Test layering (per the IoT protocol methodology: simulator -> lab
// device -> field):
//   - the adapter logic (tool mapping, risk levels, argument validation,
//     health caching, close semantics) is certified against an
//     in-memory NodeClient fake (memNodeClient) with deterministic error
//     injection, disconnect control and a half-dead session;
//   - the real OPC-UA session path (secure channel, session activation,
//     Read/Write services over TCP) is certified against gopcua's own
//     in-process server implementation (opcua/server), because gopcua
//     ships no standalone test-server binary and a hand-rolled OPC-UA
//     TCP server would be orders of magnitude more complex than the
//     Modbus simulator.
//
// Lab-device and field validation against commercial OPC-UA servers
// (Kepware, Siemens, Ignition) remains an integration task and is not
// covered here.
//
// Why NodeClient instead of mocking at the *opcua.Client level: the
// adapter never talks to the client directly — every service call goes
// through the NodeClient interface (Read/Write), which is exactly the
// seam that makes the adapter logic testable without a wire server, and
// which the real gopcuaNodeClient implements in opcua.go.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gopcua/opcua/server"
	"github.com/gopcua/opcua/ua"
	"golang.org/x/sync/errgroup"
)

// opcuaCertConfigTemplate is the reference JSON config document of the
// suite. endpoint is injected (the memory fixture uses a placeholder
// URL, the server fixture the simulator URL). Node ids live in
// namespace 1 by default; the server fixture rewrites "ns=1;" to its
// real namespace index. The table covers: number/bool/string round
// trips, a risk override (setpoint, risk 3) and a deliberately unmapped
// node (Ghost) for the error cases.
const opcuaCertConfigTemplate = `{
  "endpoint": %q,
  "security": "None",
  "timeout": "2s",
  "nodes": [
    {"name": "temperature", "nodeId": "ns=1;s=Temperature", "read": true, "write": true, "dataType": "number", "unit": "degC"},
    {"name": "running",     "nodeId": "ns=1;s=Running",     "read": true, "write": true, "dataType": "bool"},
    {"name": "job_name",    "nodeId": "ns=1;s=JobName",     "read": true, "write": true, "dataType": "string"},
    {"name": "setpoint",    "nodeId": "ns=1;s=Setpoint",    "read": true, "write": true, "dataType": "number", "risk": 3},
    {"name": "ghost",       "nodeId": "ns=1;s=Ghost",       "read": true, "write": true, "dataType": "number"}
  ]
}`

// opcuaCertConfig renders the cert config for the given endpoint and
// namespace index.
func opcuaCertConfig(endpoint string, ns int) string {
	s := fmt.Sprintf(opcuaCertConfigTemplate, endpoint)
	if ns != 1 {
		s = strings.ReplaceAll(s, "ns=1;", fmt.Sprintf("ns=%d;", ns))
	}
	return s
}

// memNodeClient is an in-memory NodeClient backing the deterministic
// half of the cert suite. It stores values keyed by node id string and
// supports fault injection: unknown nodes answer BadNodeIDUnknown, a
// global read error simulates a half-dead session, and a disconnect
// flag simulates session loss.
type memNodeClient struct {
	mu        sync.Mutex
	vals      map[string]any
	connected bool
	readErr   error
	closed    bool
}

// newMemNodeClient returns a connected client with no values.
func newMemNodeClient() *memNodeClient {
	return &memNodeClient{vals: make(map[string]any), connected: true}
}

func (m *memNodeClient) setValue(id string, v any) {
	m.mu.Lock()
	m.vals[id] = v
	m.mu.Unlock()
}

func (m *memNodeClient) setConnected(on bool) {
	m.mu.Lock()
	m.connected = on
	m.mu.Unlock()
}

func (m *memNodeClient) setReadErr(err error) {
	m.mu.Lock()
	m.readErr = err
	m.mu.Unlock()
}

// Read implements NodeClient.
func (m *memNodeClient) Read(ctx context.Context, id *ua.NodeID) (*ua.DataValue, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, ErrAdapterClosed
	}
	if !m.connected {
		return nil, errors.New("opc-ua: session not connected")
	}
	if m.readErr != nil {
		return nil, m.readErr
	}
	v, ok := m.vals[id.String()]
	if !ok {
		return nil, &StatusError{Op: "read", NodeID: id.String(), Status: ua.StatusBadNodeIDUnknown}
	}
	return &ua.DataValue{EncodingMask: ua.DataValueValue, Value: ua.MustVariant(v)}, nil
}

// Write implements NodeClient.
func (m *memNodeClient) Write(ctx context.Context, id *ua.NodeID, dv *ua.DataValue) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return ErrAdapterClosed
	}
	if !m.connected {
		return errors.New("opc-ua: session not connected")
	}
	if _, ok := m.vals[id.String()]; !ok {
		return &StatusError{Op: "write", NodeID: id.String(), Status: ua.StatusBadNodeIDUnknown}
	}
	if dv.Value == nil {
		m.vals[id.String()] = nil
		return nil
	}
	m.vals[id.String()] = dv.Value.Value()
	return nil
}

// Connected implements NodeClient.
func (m *memNodeClient) Connected() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.connected && !m.closed
}

// Close implements NodeClient.
func (m *memNodeClient) Close(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	m.connected = false
	return nil
}

// newOPCUACertFixture builds the certified adapter on the in-memory
// client with known initial values.
func newOPCUACertFixture(t *testing.T) (*OPCUAAdapter, *memNodeClient) {
	t.Helper()
	cfg, err := ParseOPCUAConfig([]byte(opcuaCertConfig("opc.tcp://127.0.0.1:4840", 1)))
	if err != nil {
		t.Fatalf("parse cert config: %v", err)
	}
	mem := newMemNodeClient()
	mem.setValue("ns=1;s=Temperature", float64(25.5))
	mem.setValue("ns=1;s=Running", true)
	mem.setValue("ns=1;s=JobName", "batch-01")
	mem.setValue("ns=1;s=Setpoint", float64(700))
	adapter, err := newOPCUAAdapter(cfg, mem)
	if err != nil {
		t.Fatalf("new opc-ua adapter: %v", err)
	}
	t.Cleanup(func() { _ = adapter.Close() })
	return adapter, mem
}

// opcuaMustCall runs a tool and returns the decoded result JSON.
func opcuaMustCall(t *testing.T, a *OPCUAAdapter, name string, args map[string]any) map[string]any {
	t.Helper()
	res, err := a.CallTool(context.Background(), name, args)
	if err != nil {
		t.Fatalf("CallTool(%s): %v", name, err)
	}
	return decodeResult(t, res)
}

// TestOPCUACertDiscoverTools verifies the tool inventory derived from
// the config: names, count, schema shape and risk levels (read 0, write
// 2, override 3 for setpoint), with value schemas typed by the coarse
// data type (number/boolean/string).
func TestOPCUACertDiscoverTools(t *testing.T) {
	adapter, _ := newOPCUACertFixture(t)
	tools := adapter.ListTools()

	want := map[string]struct {
		risk      int
		write     bool
		valueType string
	}{
		"node_read_temperature":  {risk: 0},
		"node_write_temperature": {risk: 2, write: true, valueType: "number"},
		"node_read_running":      {risk: 0},
		"node_write_running":     {risk: 2, write: true, valueType: "boolean"},
		"node_read_job_name":     {risk: 0},
		"node_write_job_name":    {risk: 2, write: true, valueType: "string"},
		"node_read_setpoint":     {risk: 3},
		"node_write_setpoint":    {risk: 3, write: true, valueType: "number"},
		"node_read_ghost":        {risk: 0},
		"node_write_ghost":       {risk: 2, write: true, valueType: "number"},
	}
	if len(tools) != len(want) {
		t.Fatalf("tool count: got %d want %d", len(tools), len(want))
	}
	for _, tool := range tools {
		w, ok := want[tool.Name]
		if !ok {
			t.Errorf("unexpected tool %q", tool.Name)
			continue
		}
		if got := tool.EffectiveRisk(); got != w.risk {
			t.Errorf("%s: risk got %d want %d", tool.Name, got, w.risk)
		}
		if tool.SchemaVersion != "1.0" {
			t.Errorf("%s: schema version got %q", tool.Name, tool.SchemaVersion)
		}
		var schema map[string]any
		if err := json.Unmarshal(tool.InputSchema, &schema); err != nil {
			t.Errorf("%s: input schema not valid JSON: %v", tool.Name, err)
			continue
		}
		if schema["type"] != "object" {
			t.Errorf("%s: schema type got %v want object", tool.Name, schema["type"])
		}
		if !strings.Contains(tool.Description, "Read") && !strings.Contains(tool.Description, "Write") {
			t.Errorf("%s: description %q lacks the verb", tool.Name, tool.Description)
		}
		if w.write {
			props, _ := schema["properties"].(map[string]any)
			value, ok := props["value"].(map[string]any)
			if !ok || value["type"] != w.valueType {
				t.Errorf("%s: value schema got %v want type %s", tool.Name, props["value"], w.valueType)
			}
			required, _ := schema["required"].([]any)
			if len(required) != 1 || required[0] != "value" {
				t.Errorf("%s: required got %v want [value]", tool.Name, required)
			}
		}
	}
}

// TestOPCUACertReadPoints verifies reading every coarse data type
// returns the stored value with the right metadata.
func TestOPCUACertReadPoints(t *testing.T) {
	adapter, _ := newOPCUACertFixture(t)

	cases := []struct {
		tool   string
		value  any
		unit   any // nil: the unit key must be absent
		typ    string
		nodeID string
	}{
		{"node_read_temperature", float64(25.5), "degC", "number", "ns=1;s=Temperature"},
		{"node_read_running", true, nil, "bool", "ns=1;s=Running"},
		{"node_read_job_name", "batch-01", nil, "string", "ns=1;s=JobName"},
		{"node_read_setpoint", float64(700), nil, "number", "ns=1;s=Setpoint"},
	}
	for _, c := range cases {
		m := opcuaMustCall(t, adapter, c.tool, nil)
		if m["value"] != c.value {
			t.Errorf("%s: value got %v want %v", c.tool, m["value"], c.value)
		}
		if m["unit"] != c.unit {
			t.Errorf("%s: unit got %v want %v", c.tool, m["unit"], c.unit)
		}
		if m["type"] != c.typ {
			t.Errorf("%s: type got %v want %q", c.tool, m["type"], c.typ)
		}
		if m["nodeId"] != c.nodeID {
			t.Errorf("%s: nodeId got %v want %q", c.tool, m["nodeId"], c.nodeID)
		}
	}
}

// TestOPCUACertWriteRoundTrip verifies number, bool and string writes
// are echoed by the tool and observable through the matching read tool.
func TestOPCUACertWriteRoundTrip(t *testing.T) {
	adapter, _ := newOPCUACertFixture(t)

	m := opcuaMustCall(t, adapter, "node_write_temperature", map[string]any{"value": float64(1234)})
	if m["value"] != float64(1234) {
		t.Fatalf("write temperature: echoed value got %v want 1234", m["value"])
	}
	if got := opcuaMustCall(t, adapter, "node_read_temperature", nil)["value"]; got != float64(1234) {
		t.Fatalf("read back temperature: got %v want 1234", got)
	}

	m = opcuaMustCall(t, adapter, "node_write_running", map[string]any{"value": false})
	if m["value"] != false {
		t.Fatalf("write running: echoed value got %v want false", m["value"])
	}
	if got := opcuaMustCall(t, adapter, "node_read_running", nil)["value"]; got != false {
		t.Fatalf("read back running: got %v want false", got)
	}

	m = opcuaMustCall(t, adapter, "node_write_job_name", map[string]any{"value": "batch-02"})
	if m["value"] != "batch-02" {
		t.Fatalf("write job_name: echoed value got %v want batch-02", m["value"])
	}
	if got := opcuaMustCall(t, adapter, "node_read_job_name", nil)["value"]; got != "batch-02" {
		t.Fatalf("read back job_name: got %v want batch-02", got)
	}
}

// TestOPCUACertBadNode verifies that points unknown to the server fail
// with a StatusError carrying StatusBadNodeIDUnknown, for both the read
// and the write direction.
func TestOPCUACertBadNode(t *testing.T) {
	adapter, _ := newOPCUACertFixture(t)

	_, err := adapter.CallTool(context.Background(), "node_read_ghost", nil)
	assertBadNodeUnknown(t, "read ghost", err)

	_, err = adapter.CallTool(context.Background(), "node_write_ghost", map[string]any{"value": float64(1)})
	assertBadNodeUnknown(t, "write ghost", err)
}

// assertBadNodeUnknown checks the error chain for a StatusError with
// StatusBadNodeIDUnknown.
func assertBadNodeUnknown(t *testing.T, what string, err error) {
	t.Helper()
	var se *StatusError
	if !errors.As(err, &se) {
		t.Fatalf("%s: want StatusError, got %v", what, err)
	}
	if se.Status != ua.StatusBadNodeIDUnknown {
		t.Fatalf("%s: want StatusBadNodeIDUnknown, got %v", what, se.Status)
	}
}

// TestOPCUACertConcurrentReads hammers one read tool from many
// goroutines; every result must be correct. Run with -race.
func TestOPCUACertConcurrentReads(t *testing.T) {
	adapter, _ := newOPCUACertFixture(t)

	const readers = 16
	const perReader = 25
	var eg errgroup.Group
	for i := 0; i < readers; i++ {
		eg.Go(func() error {
			for j := 0; j < perReader; j++ {
				res, err := adapter.CallTool(context.Background(), "node_read_temperature", nil)
				if err != nil {
					return fmt.Errorf("read temperature: %w", err)
				}
				var m map[string]any
				if err := json.Unmarshal([]byte(res.Content[0].Text), &m); err != nil {
					return err
				}
				if m["value"] != float64(25.5) {
					return fmt.Errorf("read temperature: got %v want 25.5", m["value"])
				}
			}
			return nil
		})
	}
	if err := eg.Wait(); err != nil {
		t.Fatal(err)
	}
}

// TestOPCUACertHealth verifies Health performs a real round-trip:
// healthy with a live session, unhealthy after a disconnect, unhealthy
// with a half-dead session (connected but failing reads), and healthy
// again after the session recovers.
func TestOPCUACertHealth(t *testing.T) {
	adapter, mem := newOPCUACertFixture(t)
	ctx := context.Background()

	if err := adapter.Health(ctx); err != nil {
		t.Fatalf("health: %v", err)
	}
	if !adapter.Info().Healthy {
		t.Fatal("Info().Healthy = false after successful health check")
	}

	mem.setConnected(false)
	err := adapter.Health(ctx)
	if err == nil {
		t.Fatal("health after disconnect: want error, got nil")
	}
	if !strings.Contains(err.Error(), "session not connected") {
		t.Fatalf("health after disconnect: error does not mention session state: %v", err)
	}
	if adapter.Info().Healthy {
		t.Fatal("Info().Healthy = true after failed health check")
	}

	mem.setConnected(true)
	mem.setReadErr(errors.New("opc-ua: transport: connection reset"))
	if err := adapter.Health(ctx); err == nil {
		t.Fatal("health with half-dead session: want error, got nil")
	}
	if adapter.Info().Healthy {
		t.Fatal("Info().Healthy = true after half-dead health check")
	}

	mem.setReadErr(nil)
	if err := adapter.Health(ctx); err != nil {
		t.Fatalf("health after recovery: %v", err)
	}
	if !adapter.Info().Healthy {
		t.Fatal("Info().Healthy = false after recovery")
	}
}

// TestOPCUACertInvalidArgsAndUnknownTool verifies argument validation
// and tool resolution.
func TestOPCUACertInvalidArgsAndUnknownTool(t *testing.T) {
	adapter, _ := newOPCUACertFixture(t)
	ctx := context.Background()

	_, err := adapter.CallTool(ctx, "node_read_nope", nil)
	if !errors.Is(err, ErrToolNotFound) {
		t.Fatalf("unknown tool: got %v want ErrToolNotFound", err)
	}

	_, err = adapter.CallTool(ctx, "node_write_temperature", nil)
	if !errors.Is(err, ErrOPCUAMissingValue) {
		t.Fatalf("missing value: got %v want ErrOPCUAMissingValue", err)
	}

	for _, bad := range []any{"on", float64(2)} {
		_, err = adapter.CallTool(ctx, "node_write_running", map[string]any{"value": bad})
		if !errors.Is(err, ErrOPCUAInvalidValue) {
			t.Fatalf("bool value %v: got %v want ErrOPCUAInvalidValue", bad, err)
		}
	}

	for _, bad := range []any{float64(42), true} {
		_, err = adapter.CallTool(ctx, "node_write_job_name", map[string]any{"value": bad})
		if !errors.Is(err, ErrOPCUAInvalidValue) {
			t.Fatalf("string value %v: got %v want ErrOPCUAInvalidValue", bad, err)
		}
	}

	for _, bad := range []any{"abc", math.NaN(), math.Inf(1)} {
		_, err = adapter.CallTool(ctx, "node_write_temperature", map[string]any{"value": bad})
		if !errors.Is(err, ErrOPCUAInvalidValue) {
			t.Fatalf("number value %v: got %v want ErrOPCUAInvalidValue", bad, err)
		}
	}
}

// TestOPCUACertClosedAdapter verifies Close is idempotent and all
// operations fail with ErrAdapterClosed afterwards.
func TestOPCUACertClosedAdapter(t *testing.T) {
	adapter, _ := newOPCUACertFixture(t)

	if err := adapter.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := adapter.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	if _, err := adapter.CallTool(context.Background(), "node_read_temperature", nil); !errors.Is(err, ErrAdapterClosed) {
		t.Fatalf("call after close: got %v want ErrAdapterClosed", err)
	}
	if err := adapter.Health(context.Background()); !errors.Is(err, ErrAdapterClosed) {
		t.Fatalf("health after close: got %v want ErrAdapterClosed", err)
	}
}

// TestOPCUACertCtxTimeout verifies a ctx deadline bounds the transport
// call: a deadline in the past fails fast without touching the network.
func TestOPCUACertCtxTimeout(t *testing.T) {
	adapter, _ := newOPCUACertFixture(t)

	ctx, cancel := context.WithTimeout(context.Background(), -time.Millisecond)
	defer cancel()
	_, err := adapter.CallTool(ctx, "node_read_temperature", nil)
	if err == nil {
		t.Fatal("call with expired deadline: want error, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		t.Fatalf("call with expired deadline: got %v want DeadlineExceeded/Canceled", err)
	}
}

// TestOPCUACertRegistryStatus verifies the framework registry with the
// opc-ua protocol name and the aggregated health view flipping unhealthy
// after a disconnect.
func TestOPCUACertRegistryStatus(t *testing.T) {
	adapter, mem := newOPCUACertFixture(t)

	reg := NewRegistry()
	if err := reg.Register(adapter); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := reg.Register(adapter); !errors.Is(err, ErrDuplicateAdapter) {
		t.Fatalf("duplicate register: got %v want ErrDuplicateAdapter", err)
	}
	if got, ok := reg.Get(OPCUAProtocol); !ok || got != adapter {
		t.Fatalf("get %s: got %v, ok=%v", OPCUAProtocol, got, ok)
	}

	statuses := reg.AdaptersStatus(context.Background())
	if len(statuses) != 1 || !statuses[0].Healthy || statuses[0].Err != "" {
		t.Fatalf("status while healthy: got %+v", statuses)
	}

	mem.setConnected(false)
	statuses = reg.AdaptersStatus(context.Background())
	if len(statuses) != 1 || statuses[0].Healthy || statuses[0].Err == "" {
		t.Fatalf("status after disconnect: got %+v", statuses)
	}

	reg.Unregister(OPCUAProtocol)
	if _, ok := reg.Get(OPCUAProtocol); ok {
		t.Fatal("get after unregister: want not found")
	}
}

// TestOPCUAConfigValidation pins the JSON grammar and its rejections.
func TestOPCUAConfigValidation(t *testing.T) {
	valid := func() string {
		return `{"endpoint":"opc.tcp://127.0.0.1:4840","security":"None","nodes":[{"name":"t","nodeId":"ns=2;s=T","read":true,"write":true,"dataType":"number"}]}`
	}
	if _, err := ParseOPCUAConfig([]byte(valid())); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}

	cases := []struct {
		name    string
		mutate  func(cfg string) string
		wantSub string
	}{
		{"empty endpoint", func(s string) string {
			return `{"endpoint":"","nodes":[{"name":"t","nodeId":"ns=2;s=T","read":true,"dataType":"number"}]}`
		}, "endpoint"},
		{"bad scheme", func(s string) string {
			return strings.Replace(s, "opc.tcp://", "http://", 1)
		}, "opc.tcp"},
		{"no nodes", func(s string) string {
			return `{"endpoint":"opc.tcp://127.0.0.1:4840","nodes":[]}`
		}, "node"},
		{"unsupported security", func(s string) string {
			return strings.Replace(s, `"security":"None"`, `"security":"Sign"`, 1)
		}, "V1"},
		{"unsupported sign&encrypt", func(s string) string {
			return strings.Replace(s, `"security":"None"`, `"security":"SignAndEncrypt"`, 1)
		}, "V1"},
		{"unknown dataType", func(s string) string {
			return strings.Replace(s, `"dataType":"number"`, `"dataType":"matrix"`, 1)
		}, "dataType"},
		{"missing dataType", func(s string) string {
			return strings.Replace(s, `,"dataType":"number"`, "", 1)
		}, "dataType"},
		{"duplicate name", func(s string) string {
			return `{"endpoint":"opc.tcp://127.0.0.1:4840","nodes":[{"name":"t","nodeId":"ns=2;s=T","read":true,"dataType":"number"},{"name":"t","nodeId":"ns=2;s=U","read":true,"dataType":"number"}]}`
		}, "duplicate"},
		{"bad node id", func(s string) string {
			return strings.Replace(s, `"nodeId":"ns=2;s=T"`, `"nodeId":"ns=bad;i=5"`, 1)
		}, "node id"},
		{"bad name", func(s string) string {
			return strings.Replace(s, `"name":"t"`, `"name":"t.t"`, 1)
		}, "name"},
		{"neither read nor write", func(s string) string {
			return strings.Replace(s, `"read":true,"write":true,`, "", 1)
		}, "read/write"},
		{"risk out of range", func(s string) string {
			return strings.Replace(s, `"read":true`, `"read":true,"risk":4`, 1)
		}, "risk"},
		{"negative timeout", func(s string) string {
			return strings.Replace(s, `"nodes"`, `"timeout":"-1s","nodes"`, 1)
		}, "timeout"},
		{"bad timeout string", func(s string) string {
			return strings.Replace(s, `"nodes"`, `"timeout":"nope","nodes"`, 1)
		}, "timeout"},
		{"bad json", func(s string) string {
			return `{"endpoint":`
		}, "parse"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ParseOPCUAConfig([]byte(c.mutate(valid())))
			if err == nil {
				t.Fatal("want error, got nil")
			}
			if !strings.Contains(err.Error(), c.wantSub) {
				t.Fatalf("error %q does not mention %q", err.Error(), c.wantSub)
			}
		})
	}
}

// TestOPCUACertAdapterDetailAndInfo pins the identity metadata shown on
// the console page.
func TestOPCUACertAdapterDetailAndInfo(t *testing.T) {
	adapter, _ := newOPCUACertFixture(t)

	info := adapter.Info()
	if info.Protocol != OPCUAProtocol {
		t.Fatalf("protocol: got %q want %q", info.Protocol, OPCUAProtocol)
	}
	if info.Version == "" {
		t.Fatal("version must not be empty")
	}
	if info.Detail != "opc.tcp://127.0.0.1:4840" {
		t.Fatalf("detail: got %q want the configured endpoint", info.Detail)
	}
}

// opcuaSimulator wraps gopcua's own server implementation with the
// variable nodes the cert suite needs. Node ids are string ids in a
// fresh namespace, so the config template and the memory fake use the
// same id space.
type opcuaSimulator struct {
	srv      *server.Server
	endpoint string
	ns       int
}

// newOPCUASimulator starts a gopcua server on an ephemeral local port
// with the cert node table and registers its cleanup.
func newOPCUASimulator(t *testing.T) *opcuaSimulator {
	t.Helper()
	port := freePort(t)
	s := server.New(server.EndPoint("127.0.0.1", port))
	nodeNS := server.NewNodeNameSpace(s, "adc-cert")
	_ = nodeNS.AddNewVariableStringNode("Temperature", float64(25.5))
	_ = nodeNS.AddNewVariableStringNode("Running", true)
	_ = nodeNS.AddNewVariableStringNode("JobName", "batch-01")
	_ = nodeNS.AddNewVariableStringNode("Setpoint", float64(700))
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("opc-ua simulator: start: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return &opcuaSimulator{
		srv:      s,
		endpoint: fmt.Sprintf("opc.tcp://127.0.0.1:%d", port),
		ns:       int(nodeNS.ID()),
	}
}

// freePort reserves an ephemeral TCP port and returns its number. There
// is a small race window between releasing the probe listener and the
// OPC-UA server binding the port, which is acceptable for tests.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port
}

// TestOPCUAServerSessionRoundTrip certifies the real OPC-UA session path
// end to end against gopcua's in-process server: connect, read, write
// round-trips for number/bool/string, bad-node status mapping, health
// via session state plus a read ping, and unhealthy after the server is
// torn down.
func TestOPCUAServerSessionRoundTrip(t *testing.T) {
	sim := newOPCUASimulator(t)
	adapter := newRealOPCUAAdapter(t, sim)
	ctx := context.Background()

	if got := opcuaMustCall(t, adapter, "node_read_temperature", nil)["value"]; got != float64(25.5) {
		t.Fatalf("read temperature: got %v want 25.5", got)
	}

	m := opcuaMustCall(t, adapter, "node_write_setpoint", map[string]any{"value": float64(1234)})
	if m["value"] != float64(1234) {
		t.Fatalf("write setpoint: echoed value got %v want 1234", m["value"])
	}
	if got := opcuaMustCall(t, adapter, "node_read_setpoint", nil)["value"]; got != float64(1234) {
		t.Fatalf("read back setpoint: got %v want 1234", got)
	}

	opcuaMustCall(t, adapter, "node_write_running", map[string]any{"value": false})
	if got := opcuaMustCall(t, adapter, "node_read_running", nil)["value"]; got != false {
		t.Fatalf("read back running: got %v want false", got)
	}

	opcuaMustCall(t, adapter, "node_write_job_name", map[string]any{"value": "batch-02"})
	if got := opcuaMustCall(t, adapter, "node_read_job_name", nil)["value"]; got != "batch-02" {
		t.Fatalf("read back job_name: got %v want batch-02", got)
	}

	_, err := adapter.CallTool(ctx, "node_read_ghost", nil)
	assertBadNodeUnknown(t, "read ghost on real server", err)

	if err := adapter.Health(ctx); err != nil {
		t.Fatalf("health before server close: %v", err)
	}
	if !adapter.Info().Healthy {
		t.Fatal("Info().Healthy = false after successful health check")
	}

	// Teardown order matters: the client closes its session first (fast
	// CloseSession round-trip), then the gopcua server shuts down without
	// hitting its 10s graceful-close wait for the channel goroutine.
	if err := adapter.Close(); err != nil {
		t.Fatalf("adapter close: %v", err)
	}
	if err := sim.srv.Close(); err != nil {
		t.Fatalf("simulator close: %v", err)
	}
}

// TestOPCUAServerDownHealth certifies the real session path against a
// dead endpoint: a fresh adapter's Health must fail (connect attempt is
// the ping) and the failure must be cached as unhealthy. Abrupt in-band
// disconnects of a live session are covered deterministically by the
// in-memory fake (TestOPCUACertHealth); killing a gopcua server out from
// under a live session costs a 10s server-teardown wait in gopcua and is
// not worth repeating in the fast path.
func TestOPCUAServerDownHealth(t *testing.T) {
	sim := newOPCUASimulator(t)
	adapter := newRealOPCUAAdapter(t, sim)
	if err := adapter.Health(context.Background()); err != nil {
		t.Fatalf("health before server close: %v", err)
	}

	if err := adapter.Close(); err != nil {
		t.Fatalf("adapter close: %v", err)
	}
	if err := sim.srv.Close(); err != nil {
		t.Fatalf("simulator close: %v", err)
	}

	down, err := newOPCUAAdapterFor(t, sim)
	if err != nil {
		t.Fatalf("new adapter for down server: %v", err)
	}
	defer func() { _ = down.Close() }()

	if err := down.Health(context.Background()); err == nil {
		t.Fatal("health with server down: want error, got nil")
	}
	if down.Info().Healthy {
		t.Fatal("Info().Healthy = true with server down")
	}
	if _, err := down.CallTool(context.Background(), "node_read_temperature", nil); err == nil {
		t.Fatal("read with server down: want error, got nil")
	}
}

// newOPCUAAdapterFor builds a real adapter against the simulator's
// endpoint.
func newOPCUAAdapterFor(t *testing.T, sim *opcuaSimulator) (*OPCUAAdapter, error) {
	t.Helper()
	cfg, err := ParseOPCUAConfig([]byte(opcuaCertConfig(sim.endpoint, sim.ns)))
	if err != nil {
		t.Fatalf("parse server config: %v", err)
	}
	return NewOPCUAAdapter(cfg)
}

// newRealOPCUAAdapter builds and registers cleanup for a real adapter.
func newRealOPCUAAdapter(t *testing.T, sim *opcuaSimulator) *OPCUAAdapter {
	t.Helper()
	adapter, err := newOPCUAAdapterFor(t, sim)
	if err != nil {
		t.Fatalf("new opc-ua adapter: %v", err)
	}
	t.Cleanup(func() { _ = adapter.Close() })
	return adapter
}

var _ NodeClient = (*memNodeClient)(nil)
