package adapters

// cert_test.go is the C1.6 certification suite for the Modbus adapter
// (design/83 C1.6, milestone M5 acceptance): discovery, read, write
// round-trip, error handling, concurrent reads, health after disconnect
// and risk mapping. Every protocol adapter must pass the same matrix
// before it counts as supported; new adapters should mirror this file.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	sdkproto "adc.dev/core-sdk/protocol"
	"github.com/goburrow/modbus"
	"golang.org/x/sync/errgroup"
)

// certConfigTemplate is the reference JSON config document of the suite.
// host/port are injected because the simulator listens on an ephemeral
// port. The register table covers: holding read/write, a risk override,
// input register, coil, discrete input and a deliberately unmapped
// address (65535) for the error cases.
const certConfigTemplate = `{
  "host": %q,
  "port": %d,
  "timeout": "2s",
  "slaveId": 1,
  "registers": [
    {"name": "temperature",       "address": 0,     "type": "holding",  "read": true,  "write": true,  "unit": "degC"},
    {"name": "speed",             "address": 1,     "type": "holding",  "read": true,  "write": true,  "unit": "rpm"},
    {"name": "critical_setpoint", "address": 2,     "type": "holding",  "read": true,  "write": true,  "risk": 3},
    {"name": "firmware_build",    "address": 10,    "type": "input",    "read": true},
    {"name": "motor_on",          "address": 20,    "type": "coil",     "read": true,  "write": true},
    {"name": "door_closed",       "address": 30,    "type": "discrete", "read": true},
    {"name": "ghost",             "address": 65535, "type": "holding",  "read": true,  "write": true}
  ]
}`

// newCertFixture builds the certified adapter against a fresh simulator
// with known initial values.
func newCertFixture(t *testing.T) (*ModbusAdapter, *modbusSimulator) {
	t.Helper()
	sim := newModbusSimulator(t)
	host, port := sim.HostPort()
	cfg, err := ParseConfig([]byte(fmt.Sprintf(certConfigTemplate, host, port)))
	if err != nil {
		t.Fatalf("parse cert config: %v", err)
	}
	adapter, err := NewModbusAdapter(cfg)
	if err != nil {
		t.Fatalf("new modbus adapter: %v", err)
	}
	sim.SetHolding(0, 0x1234) // 4660
	sim.SetHolding(1, 42)
	sim.SetHolding(2, 7)
	sim.SetInput(10, 0x00FF) // 255
	sim.SetCoil(20, true)
	sim.SetDiscrete(30, false)
	t.Cleanup(func() { _ = adapter.Close() })
	return adapter, sim
}

// mustCall runs a tool and returns the decoded result JSON.
func mustCall(t *testing.T, a *ModbusAdapter, name string, args map[string]any) map[string]any {
	t.Helper()
	res, err := a.CallTool(context.Background(), name, args)
	if err != nil {
		t.Fatalf("CallTool(%s): %v", name, err)
	}
	return decodeResult(t, res)
}

// decodeResult unwraps the single JSON text block of a tool result.
func decodeResult(t *testing.T, res sdkproto.ToolCallResult) map[string]any {
	t.Helper()
	if res.IsError {
		t.Fatalf("unexpected tool error result: %+v", res.Content)
	}
	if len(res.Content) != 1 {
		t.Fatalf("want 1 content block, got %d", len(res.Content))
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(res.Content[0].Text), &m); err != nil {
		t.Fatalf("decode tool result %q: %v", res.Content[0].Text, err)
	}
	return m
}

// TestCertDiscoverTools verifies the tool inventory derived from the
// config: names, count, schema shape and risk levels (read 0, write 2,
// override 3 for critical_setpoint).
func TestCertDiscoverTools(t *testing.T) {
	adapter, _ := newCertFixture(t)
	tools := adapter.ListTools()

	want := map[string]struct {
		risk      int
		write     bool
		valueType string
	}{
		"register_read_temperature":        {risk: 0},
		"register_write_temperature":       {risk: 2, write: true, valueType: "number"},
		"register_read_speed":              {risk: 0},
		"register_write_speed":             {risk: 2, write: true, valueType: "number"},
		"register_read_critical_setpoint":  {risk: 3},
		"register_write_critical_setpoint": {risk: 3, write: true, valueType: "number"},
		"register_read_firmware_build":     {risk: 0},
		"register_read_motor_on":           {risk: 0},
		"register_write_motor_on":          {risk: 2, write: true, valueType: "boolean"},
		"register_read_door_closed":        {risk: 0},
		"register_read_ghost":              {risk: 0},
		"register_write_ghost":             {risk: 2, write: true, valueType: "number"},
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

// TestCertReadPoints verifies reading every kind of point returns the
// simulated value with the right unit metadata.
func TestCertReadPoints(t *testing.T) {
	adapter, _ := newCertFixture(t)

	cases := []struct {
		tool  string
		value any
		unit  any // nil: the unit key must be absent
		kind  string
		addr  float64
	}{
		{"register_read_temperature", float64(4660), "degC", "holding", 0},
		{"register_read_speed", float64(42), "rpm", "holding", 1},
		{"register_read_firmware_build", float64(255), nil, "input", 10},
		{"register_read_motor_on", true, nil, "coil", 20},
		{"register_read_door_closed", false, nil, "discrete", 30},
	}
	for _, c := range cases {
		m := mustCall(t, adapter, c.tool, nil)
		if m["value"] != c.value {
			t.Errorf("%s: value got %v want %v", c.tool, m["value"], c.value)
		}
		if m["unit"] != c.unit {
			t.Errorf("%s: unit got %v want %q", c.tool, m["unit"], c.unit)
		}
		if m["type"] != c.kind {
			t.Errorf("%s: type got %v want %q", c.tool, m["type"], c.kind)
		}
		if m["address"] != c.addr {
			t.Errorf("%s: address got %v want %v", c.tool, m["address"], c.addr)
		}
	}
}

// TestCertWriteRoundTrip verifies a register write and a coil write are
// echoed by the tool and observable through the matching read tool.
func TestCertWriteRoundTrip(t *testing.T) {
	adapter, _ := newCertFixture(t)

	m := mustCall(t, adapter, "register_write_speed", map[string]any{"value": float64(1234)})
	if m["value"] != float64(1234) {
		t.Fatalf("write speed: echoed value got %v want 1234", m["value"])
	}
	if got := mustCall(t, adapter, "register_read_speed", nil)["value"]; got != float64(1234) {
		t.Fatalf("read back speed: got %v want 1234", got)
	}

	m = mustCall(t, adapter, "register_write_motor_on", map[string]any{"value": false})
	if m["value"] != false {
		t.Fatalf("write motor_on: echoed value got %v want false", m["value"])
	}
	if got := mustCall(t, adapter, "register_read_motor_on", nil)["value"]; got != false {
		t.Fatalf("read back motor_on: got %v want false", got)
	}
}

// TestCertBadAddress verifies that points outside the device address
// space fail with the Modbus illegal-data-address exception (code 2).
func TestCertBadAddress(t *testing.T) {
	adapter, _ := newCertFixture(t)

	_, err := adapter.CallTool(context.Background(), "register_read_ghost", nil)
	var me *modbus.ModbusError
	if !errors.As(err, &me) || me.ExceptionCode != modbus.ExceptionCodeIllegalDataAddress {
		t.Fatalf("read ghost: want illegal data address, got %v", err)
	}

	_, err = adapter.CallTool(context.Background(), "register_write_ghost", map[string]any{"value": float64(1)})
	if !errors.As(err, &me) || me.ExceptionCode != modbus.ExceptionCodeIllegalDataAddress {
		t.Fatalf("write ghost: want illegal data address, got %v", err)
	}
}

// TestCertConcurrentReads hammers one read tool from many goroutines;
// every result must be correct. Run with -race.
func TestCertConcurrentReads(t *testing.T) {
	adapter, _ := newCertFixture(t)

	const readers = 16
	const perReader = 25
	var eg errgroup.Group
	for i := 0; i < readers; i++ {
		eg.Go(func() error {
			for j := 0; j < perReader; j++ {
				res, err := adapter.CallTool(context.Background(), "register_read_temperature", nil)
				if err != nil {
					return fmt.Errorf("read temperature: %w", err)
				}
				var m map[string]any
				if err := json.Unmarshal([]byte(res.Content[0].Text), &m); err != nil {
					return err
				}
				if m["value"] != float64(4660) {
					return fmt.Errorf("read temperature: got %v want 4660", m["value"])
				}
			}
			return nil
		})
	}
	if err := eg.Wait(); err != nil {
		t.Fatal(err)
	}
}

// TestCertHealthAfterDisconnect verifies Health performs a real device
// round-trip: healthy while the simulator runs, unhealthy (and cached as
// such) once every connection is torn down.
func TestCertHealthAfterDisconnect(t *testing.T) {
	adapter, sim := newCertFixture(t)

	ctx := context.Background()
	if err := adapter.Health(ctx); err != nil {
		t.Fatalf("health before disconnect: %v", err)
	}
	if !adapter.Info().Healthy {
		t.Fatal("Info().Healthy = false after successful health check")
	}

	sim.Close()

	if err := adapter.Health(ctx); err == nil {
		t.Fatal("health after disconnect: want error, got nil")
	}
	if adapter.Info().Healthy {
		t.Fatal("Info().Healthy = true after failed health check")
	}
}

// TestCertInvalidArgsAndUnknownTool verifies argument validation and
// tool resolution.
func TestCertInvalidArgsAndUnknownTool(t *testing.T) {
	adapter, _ := newCertFixture(t)
	ctx := context.Background()

	_, err := adapter.CallTool(ctx, "register_read_nope", nil)
	if !errors.Is(err, ErrToolNotFound) {
		t.Fatalf("unknown tool: got %v want ErrToolNotFound", err)
	}

	_, err = adapter.CallTool(ctx, "register_write_speed", nil)
	if !errors.Is(err, ErrMissingValue) {
		t.Fatalf("missing value: got %v want ErrMissingValue", err)
	}

	for _, bad := range []any{float64(70000), float64(-1), float64(3.5)} {
		_, err = adapter.CallTool(ctx, "register_write_speed", map[string]any{"value": bad})
		if !errors.Is(err, ErrInvalidValue) {
			t.Fatalf("value %v: got %v want ErrInvalidValue", bad, err)
		}
	}

	_, err = adapter.CallTool(ctx, "register_write_motor_on", map[string]any{"value": "on"})
	if !errors.Is(err, ErrInvalidValue) {
		t.Fatalf("coil string value: got %v want ErrInvalidValue", err)
	}
}

// TestCertClosedAdapter verifies Close is idempotent and all operations
// fail with ErrAdapterClosed afterwards.
func TestCertClosedAdapter(t *testing.T) {
	adapter, _ := newCertFixture(t)

	if err := adapter.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := adapter.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	if _, err := adapter.CallTool(context.Background(), "register_read_temperature", nil); !errors.Is(err, ErrAdapterClosed) {
		t.Fatalf("call after close: got %v want ErrAdapterClosed", err)
	}
	if err := adapter.Health(context.Background()); !errors.Is(err, ErrAdapterClosed) {
		t.Fatalf("health after close: got %v want ErrAdapterClosed", err)
	}
}

// TestCertRegistryStatus verifies the framework registry: registration
// by protocol name, duplicate rejection, lookup, and the aggregated
// health view flipping unhealthy after a disconnect.
func TestCertRegistryStatus(t *testing.T) {
	adapter, sim := newCertFixture(t)

	reg := NewRegistry()
	if err := reg.Register(adapter); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := reg.Register(adapter); !errors.Is(err, ErrDuplicateAdapter) {
		t.Fatalf("duplicate register: got %v want ErrDuplicateAdapter", err)
	}
	if got, ok := reg.Get(ModbusProtocol); !ok || got != adapter {
		t.Fatalf("get %s: got %v, ok=%v", ModbusProtocol, got, ok)
	}
	if _, ok := reg.Get("opc-ua"); ok {
		t.Fatal("get unknown protocol: want not found")
	}

	infos := reg.List()
	if len(infos) != 1 || infos[0].Protocol != ModbusProtocol || infos[0].Version == "" {
		t.Fatalf("list: got %+v", infos)
	}

	statuses := reg.AdaptersStatus(context.Background())
	if len(statuses) != 1 || !statuses[0].Healthy || statuses[0].Err != "" {
		t.Fatalf("status while healthy: got %+v", statuses)
	}

	sim.Close()
	statuses = reg.AdaptersStatus(context.Background())
	if len(statuses) != 1 || statuses[0].Healthy || statuses[0].Err == "" {
		t.Fatalf("status after disconnect: got %+v", statuses)
	}

	reg.Unregister(ModbusProtocol)
	if _, ok := reg.Get(ModbusProtocol); ok {
		t.Fatal("get after unregister: want not found")
	}
}

// TestCertCtxTimeout verifies a ctx deadline bounds the transport call:
// a deadline in the past fails fast without touching the network.
func TestCertCtxTimeout(t *testing.T) {
	adapter, _ := newCertFixture(t)

	ctx, cancel := context.WithTimeout(context.Background(), -time.Millisecond)
	defer cancel()
	_, err := adapter.CallTool(ctx, "register_read_temperature", nil)
	if err == nil {
		t.Fatal("call with expired deadline: want error, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		t.Fatalf("call with expired deadline: got %v want DeadlineExceeded/Canceled", err)
	}
}

// TestParseConfigValidation pins the JSON grammar and its rejections.
func TestParseConfigValidation(t *testing.T) {
	valid := func() string {
		return `{"host":"127.0.0.1","port":502,"registers":[{"name":"t","address":0,"type":"holding","read":true,"write":true}]}`
	}
	if _, err := ParseConfig([]byte(valid())); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}

	cases := []struct {
		name    string
		mutate  func(cfg string) string
		wantSub string
	}{
		{"empty host", func(s string) string {
			return `{"host":"","port":502,"registers":[{"name":"t","address":0,"type":"holding","read":true}]}`
		}, "host"},
		{"bad port", func(s string) string {
			return strings.Replace(s, `"port":502`, `"port":0`, 1)
		}, "port"},
		{"no registers", func(s string) string {
			return `{"host":"127.0.0.1","port":502,"registers":[]}`
		}, "register"},
		{"unknown type", func(s string) string {
			return strings.Replace(s, `"type":"holding"`, `"type":"mystery"`, 1)
		}, "type"},
		{"duplicate name", func(s string) string {
			return `{"host":"127.0.0.1","port":502,"registers":[{"name":"t","address":0,"type":"holding","read":true},{"name":"t","address":1,"type":"holding","read":true}]}`
		}, "duplicate"},
		{"write on input", func(s string) string {
			return strings.Replace(s, `"type":"holding"`, `"type":"input"`, 1)
		}, "read-only"},
		{"neither read nor write", func(s string) string {
			return `{"host":"127.0.0.1","port":502,"registers":[{"name":"t","address":0,"type":"holding"}]}`
		}, "read/write"},
		{"bad name", func(s string) string {
			return strings.Replace(s, `"name":"t"`, `"name":"t.t"`, 1)
		}, "name"},
		{"risk out of range", func(s string) string {
			return `{"host":"127.0.0.1","port":502,"registers":[{"name":"t","address":0,"type":"holding","read":true,"risk":4}]}`
		}, "risk"},
		{"negative timeout", func(s string) string {
			return strings.Replace(s, `"registers"`, `"timeout":"-1s","registers"`, 1)
		}, "timeout"},
		{"bad timeout string", func(s string) string {
			return strings.Replace(s, `"registers"`, `"timeout":"nope","registers"`, 1)
		}, "timeout"},
		{"bad json", func(s string) string {
			return `{"host":`
		}, "parse"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ParseConfig([]byte(c.mutate(valid())))
			if err == nil {
				t.Fatal("want error, got nil")
			}
			if !strings.Contains(err.Error(), c.wantSub) {
				t.Fatalf("error %q does not mention %q", err.Error(), c.wantSub)
			}
		})
	}
}

// TestCertAdapterDetailAndInfo pins the identity metadata shown on the
// console page.
func TestCertAdapterDetailAndInfo(t *testing.T) {
	adapter, sim := newCertFixture(t)

	info := adapter.Info()
	if info.Protocol != ModbusProtocol {
		t.Fatalf("protocol: got %q want %q", info.Protocol, ModbusProtocol)
	}
	if info.Version == "" {
		t.Fatal("version must not be empty")
	}
	if _, port := sim.HostPort(); !strings.HasSuffix(info.Detail, fmt.Sprintf(":%d", port)) {
		t.Fatalf("detail %q should mention the simulator port", info.Detail)
	}
}

var _ Adapter = (*ModbusAdapter)(nil)
