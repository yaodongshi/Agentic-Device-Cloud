// Modbus TCP adapter (design/83 C1.2) built on github.com/goburrow/modbus
// (BSD-3-Clause license; see the module LICENSE file, redistribution is
// permitted with copyright notice, conditions list and disclaimer).
//
// The adapter maps a JSON register table to MCP tools: every readable
// point becomes register_read_{name} (risk 0) and every writable point
// register_write_{name} (risk 2), with an optional per-point risk
// override. Reads of holding registers go through ReadHoldingRegisters
// and writes through WriteSingleRegister, as specified for the M5 slice;
// the remaining Modbus data areas (coil, discrete input, input register)
// use their standard function codes so the config grammar stays honest.
//
// Concurrency: every transport operation holds the adapter mutex. The
// underlying TCP client already serializes requests on its single
// connection; the outer mutex additionally makes the per-call timeout
// window (derived from the ctx deadline) safe against data races and
// serializes Close with in-flight calls.
package adapters

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	sdkproto "adc.dev/core-sdk/protocol"
	"github.com/goburrow/modbus"
)

// ModbusProtocol is the registry key (AdapterInfo.Protocol) of the
// built-in Modbus TCP adapter.
const ModbusProtocol = "modbus-tcp"

const (
	// modbusAdapterVersion is the implementation version of this adapter.
	modbusAdapterVersion = "1.0.0"
	// defaultModbusTimeout bounds one transport round-trip when neither
	// the config nor the ctx deadline specifies anything shorter.
	defaultModbusTimeout = 5 * time.Second
	// toolSchemaVersion is the schema version stamped on every tool.
	toolSchemaVersion = "1.0"

	readToolPrefix  = "register_read_"
	writeToolPrefix = "register_write_"
)

// RegisterKind selects the Modbus data area of a configured point:
//
//	holding  — holding register (read FC 3, write FC 6)
//	input    — input register (read FC 4, read-only)
//	coil     — coil (read FC 1, write FC 5)
//	discrete — discrete input (read FC 2, read-only)
type RegisterKind string

const (
	KindHolding  RegisterKind = "holding"
	KindInput    RegisterKind = "input"
	KindCoil     RegisterKind = "coil"
	KindDiscrete RegisterKind = "discrete"
)

func (k RegisterKind) valid() bool {
	switch k {
	case KindHolding, KindInput, KindCoil, KindDiscrete:
		return true
	}
	return false
}

// registerNamePattern keeps generated tool names clean: letters, digits,
// '-' and '_', not starting with '-' or '_'.
var registerNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)

// RegisterDef is one configured point of the device.
type RegisterDef struct {
	// Name identifies the point; the tools are register_read_{name} /
	// register_write_{name}. Letters, digits, '-' and '_' only.
	Name string `json:"name"`
	// Address is the zero-based Modbus address of the point.
	Address uint16 `json:"address"`
	// Kind selects the Modbus data area (holding/input/coil/discrete).
	Kind RegisterKind `json:"type"`
	// Read exposes a read tool; Write exposes a write tool (holding and
	// coil only). At least one must be true.
	Read  bool `json:"read"`
	Write bool `json:"write"`
	// Unit is an optional engineering unit echoed in tool results.
	Unit string `json:"unit,omitempty"`
	// Risk overrides the default risk level (read 0, write 2) for both
	// tools of this point. Range 0-3.
	Risk *int `json:"risk,omitempty"`
}

// Duration is a time.Duration that unmarshals from JSON as either a Go
// duration string ("500ms", "2s") or a number of milliseconds (2000).
type Duration time.Duration

// UnmarshalJSON implements json.Unmarshaler.
func (d *Duration) UnmarshalJSON(b []byte) error {
	if len(b) == 0 || string(b) == "null" {
		*d = 0
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		v, perr := time.ParseDuration(s)
		if perr != nil {
			return fmt.Errorf("modbus: invalid timeout %q: %w", s, perr)
		}
		if v < 0 {
			return errors.New("modbus: timeout must not be negative")
		}
		*d = Duration(v)
		return nil
	}
	var ms float64
	if err := json.Unmarshal(b, &ms); err != nil {
		return errors.New("modbus: timeout must be a duration string or a number of milliseconds")
	}
	if ms < 0 {
		return errors.New("modbus: timeout must not be negative")
	}
	*d = Duration(time.Duration(ms) * time.Millisecond)
	return nil
}

// Config is the JSON configuration of one Modbus TCP adapter instance.
type Config struct {
	Host      string        `json:"host"`
	Port      int           `json:"port"`
	Timeout   Duration      `json:"timeout"` // 0: defaultModbusTimeout
	SlaveID   byte          `json:"slaveId"` // 0: unit id 1 (Modbus default)
	Registers []RegisterDef `json:"registers"`
}

// ParseConfig decodes and validates a JSON config document.
func ParseConfig(data []byte) (*Config, error) {
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("modbus: parse config: %w", err)
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// validate checks the config and applies defaults (SlaveID). It may
// mutate c, so callers must not reuse a partially validated config.
func (c *Config) validate() error {
	if c.Host == "" {
		return errors.New("modbus: host must not be empty")
	}
	if c.Port < 1 || c.Port > 65535 {
		return fmt.Errorf("modbus: port %d out of range 1-65535", c.Port)
	}
	if c.Timeout < 0 {
		return errors.New("modbus: timeout must not be negative")
	}
	if c.SlaveID == 0 {
		c.SlaveID = 1
	}
	if len(c.Registers) == 0 {
		return errors.New("modbus: at least one register must be configured")
	}
	seen := make(map[string]bool, len(c.Registers))
	for i := range c.Registers {
		r := &c.Registers[i]
		if r.Name == "" {
			return fmt.Errorf("modbus: register %d: name must not be empty", i)
		}
		if !registerNamePattern.MatchString(r.Name) {
			return fmt.Errorf("modbus: register %d: invalid name %q (letters, digits, '-' and '_' only)", i, r.Name)
		}
		if seen[r.Name] {
			return fmt.Errorf("modbus: duplicate register name %q", r.Name)
		}
		seen[r.Name] = true
		if !r.Kind.valid() {
			return fmt.Errorf("modbus: register %q: unknown type %q (want holding/input/coil/discrete)", r.Name, r.Kind)
		}
		if !r.Read && !r.Write {
			return fmt.Errorf("modbus: register %q: at least one of read/write must be enabled", r.Name)
		}
		if r.Write && r.Kind != KindHolding && r.Kind != KindCoil {
			return fmt.Errorf("modbus: register %q: %s is read-only", r.Name, r.Kind)
		}
		if r.Risk != nil && (*r.Risk < 0 || *r.Risk > 3) {
			return fmt.Errorf("modbus: register %q: risk %d out of range 0-3", r.Name, *r.Risk)
		}
	}
	return nil
}

// Modbus-specific sentinel errors.
var (
	// ErrMissingValue: a write tool was called without the "value" argument.
	ErrMissingValue = errors.New("modbus: write requires a \"value\" argument")
	// ErrInvalidValue: the "value" argument is out of range or of the wrong type.
	ErrInvalidValue = errors.New("modbus: invalid value")
	// ErrReadOnly: a write was attempted against a read-only register kind.
	ErrReadOnly = errors.New("modbus: register kind is read-only")
)

// ModbusAdapter is the built-in Modbus TCP adapter (design/83 C1.2).
type ModbusAdapter struct {
	cfg       Config
	client    modbus.Client
	handler   *modbus.TCPClientHandler
	tools     []sdkproto.MCPTool
	byName    map[string]*RegisterDef
	detail    string
	timeout   time.Duration
	healthy   atomic.Bool
	closed    bool       // guarded by mu
	mu        sync.Mutex // serializes transport use, Timeout window and Close
	closeOnce sync.Once
	closeErr  error
}

// NewModbusAdapter builds an adapter from a validated config. It dials
// lazily: the TCP connection is established on the first transport call.
func NewModbusAdapter(cfg *Config) (*ModbusAdapter, error) {
	if cfg == nil {
		return nil, errors.New("modbus: nil config")
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	timeout := time.Duration(cfg.Timeout)
	if timeout == 0 {
		timeout = defaultModbusTimeout
	}
	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	handler := modbus.NewTCPClientHandler(addr)
	handler.Timeout = timeout
	handler.SlaveId = cfg.SlaveID
	// Disable the library's idle-close timer: the adapter owns the
	// connection lifecycle and closes it explicitly in Close.
	handler.IdleTimeout = 0

	regs := make([]RegisterDef, len(cfg.Registers))
	copy(regs, cfg.Registers)
	a := &ModbusAdapter{
		cfg:     Config{Host: cfg.Host, Port: cfg.Port, Timeout: cfg.Timeout, SlaveID: cfg.SlaveID, Registers: regs},
		client:  modbus.NewClient(handler),
		handler: handler,
		byName:  make(map[string]*RegisterDef, len(regs)*2),
		detail:  addr,
		timeout: timeout,
	}
	for i := range a.cfg.Registers {
		r := &a.cfg.Registers[i]
		if r.Read {
			name := readToolPrefix + r.Name
			a.byName[name] = r
			a.tools = append(a.tools, a.readTool(name, r))
		}
		if r.Write {
			name := writeToolPrefix + r.Name
			a.byName[name] = r
			a.tools = append(a.tools, a.writeTool(name, r))
		}
	}
	return a, nil
}

// Info implements Adapter.
func (a *ModbusAdapter) Info() AdapterInfo {
	return AdapterInfo{
		Protocol: ModbusProtocol,
		Version:  modbusAdapterVersion,
		Healthy:  a.healthy.Load(),
		Detail:   a.detail,
	}
}

// ListTools implements Adapter. The returned slice is a copy, safe to
// hold and mutate by the caller.
func (a *ModbusAdapter) ListTools() []sdkproto.MCPTool {
	return append([]sdkproto.MCPTool(nil), a.tools...)
}

// CallTool implements Adapter. Read tools take no arguments; write tools
// take {"value": <number>} (holding register) or {"value": <bool>} (coil).
func (a *ModbusAdapter) CallTool(ctx context.Context, name string, args map[string]any) (sdkproto.ToolCallResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return sdkproto.ToolCallResult{}, ErrAdapterClosed
	}
	r, ok := a.byName[name]
	if !ok {
		return sdkproto.ToolCallResult{}, fmt.Errorf("%w: %s", ErrToolNotFound, name)
	}
	if err := ctx.Err(); err != nil {
		return sdkproto.ToolCallResult{}, err
	}
	if strings.HasPrefix(name, writeToolPrefix) {
		reg, coilOn, err := parseWriteValue(r, args)
		if err != nil {
			return sdkproto.ToolCallResult{}, err
		}
		if err := a.write(ctx, r, reg, coilOn); err != nil {
			return sdkproto.ToolCallResult{}, fmt.Errorf("modbus: write %s: %w", r.Name, err)
		}
		value := any(reg)
		if r.Kind == KindCoil {
			value = coilOn
		}
		return a.result(r, value), nil
	}
	value, err := a.read(ctx, r)
	if err != nil {
		return sdkproto.ToolCallResult{}, fmt.Errorf("modbus: read %s: %w", r.Name, err)
	}
	return a.result(r, value), nil
}

// Health implements Adapter: it performs a real read of the first
// readable register so a half-dead connection reports unhealthy. A
// write-only register table cannot be health-checked and is reported as
// an error.
func (a *ModbusAdapter) Health(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return ErrAdapterClosed
	}
	probe, ok := a.firstReadable()
	if !ok {
		return errors.New("modbus: no readable register configured for health probe")
	}
	if _, err := a.read(ctx, probe); err != nil {
		a.healthy.Store(false)
		return fmt.Errorf("modbus: health: %w", err)
	}
	a.healthy.Store(true)
	return nil
}

// Close implements Adapter. It waits for in-flight calls (they hold the
// transport mutex) and then closes the connection; further calls fail
// with ErrAdapterClosed. Calling Close twice is safe.
func (a *ModbusAdapter) Close() error {
	a.closeOnce.Do(func() {
		a.mu.Lock()
		a.closed = true
		a.closeErr = a.handler.Close()
		a.healthy.Store(false)
		a.mu.Unlock()
	})
	return a.closeErr
}

// firstReadable returns the first read-enabled point, used as the health
// probe.
func (a *ModbusAdapter) firstReadable() (*RegisterDef, bool) {
	for i := range a.cfg.Registers {
		if a.cfg.Registers[i].Read {
			return &a.cfg.Registers[i], true
		}
	}
	return nil, false
}

// read performs one point read and returns the decoded value (uint16 for
// register kinds, bool for bit kinds). The caller must hold a.mu.
func (a *ModbusAdapter) read(ctx context.Context, r *RegisterDef) (any, error) {
	var value any
	err := a.withTransportTimeout(ctx, func() error {
		switch r.Kind {
		case KindHolding:
			b, err := a.client.ReadHoldingRegisters(r.Address, 1)
			if err != nil {
				return err
			}
			value = binary.BigEndian.Uint16(b)
		case KindInput:
			b, err := a.client.ReadInputRegisters(r.Address, 1)
			if err != nil {
				return err
			}
			value = binary.BigEndian.Uint16(b)
		case KindCoil:
			b, err := a.client.ReadCoils(r.Address, 1)
			if err != nil {
				return err
			}
			value = b[0]&0x01 == 0x01
		case KindDiscrete:
			b, err := a.client.ReadDiscreteInputs(r.Address, 1)
			if err != nil {
				return err
			}
			value = b[0]&0x01 == 0x01
		}
		return nil
	})
	return value, err
}

// write performs one point write (FC 6 for holding, FC 5 for coil). The
// caller must hold a.mu.
func (a *ModbusAdapter) write(ctx context.Context, r *RegisterDef, reg uint16, coilOn bool) error {
	return a.withTransportTimeout(ctx, func() error {
		switch r.Kind {
		case KindHolding:
			_, err := a.client.WriteSingleRegister(r.Address, reg)
			return err
		case KindCoil:
			v := uint16(0x0000)
			if coilOn {
				v = 0xFF00
			}
			_, err := a.client.WriteSingleCoil(r.Address, v)
			return err
		default:
			return fmt.Errorf("%w: %s", ErrReadOnly, r.Kind)
		}
	})
}

// withTransportTimeout bounds one transport round-trip by the tighter of
// the configured timeout and the ctx deadline. The caller must hold a.mu
// (every transport call does), so writing handler.Timeout never races
// the library's internal connection mutex.
func (a *ModbusAdapter) withTransportTimeout(ctx context.Context, fn func() error) error {
	timeout := a.timeout
	if dl, ok := ctx.Deadline(); ok {
		if rem := time.Until(dl); rem < timeout {
			timeout = rem
		}
	}
	if timeout <= 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		return context.DeadlineExceeded
	}
	if timeout < time.Millisecond {
		timeout = time.Millisecond
	}
	a.handler.Timeout = timeout
	if err := fn(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}

// result wraps a decoded value in the JSON text block every tool returns.
func (a *ModbusAdapter) result(r *RegisterDef, value any) sdkproto.ToolCallResult {
	b, err := json.Marshal(registerReading{
		Name:    r.Name,
		Address: r.Address,
		Type:    string(r.Kind),
		Unit:    r.Unit,
		Value:   value,
	})
	if err != nil {
		// Impossible for these shapes; keep the result well-formed anyway.
		return sdkproto.ToolCallResult{
			Content: []sdkproto.ToolContent{{Type: "text", Text: fmt.Sprintf("modbus: encode result: %v", err)}},
			IsError: true,
		}
	}
	return sdkproto.ToolCallResult{
		Content: []sdkproto.ToolContent{{Type: "text", Text: string(b)}},
		IsError: false,
	}
}

// registerReading is the JSON shape of every tool result.
type registerReading struct {
	Name    string `json:"name"`
	Address uint16 `json:"address"`
	Type    string `json:"type"`
	Unit    string `json:"unit,omitempty"`
	Value   any    `json:"value"`
}

// parseWriteValue validates the "value" argument of a write tool against
// the point kind. Register kinds require an integer 0-65535; coil
// accepts a bool (or 0/1, as produced by JSON decoders that only know
// numbers).
func parseWriteValue(r *RegisterDef, args map[string]any) (uint16, bool, error) {
	raw, ok := args["value"]
	if !ok {
		return 0, false, ErrMissingValue
	}
	if r.Kind == KindCoil {
		switch v := raw.(type) {
		case bool:
			return 0, v, nil
		case float64:
			if v == 0 {
				return 0, false, nil
			}
			if v == 1 {
				return 0, true, nil
			}
		}
		return 0, false, fmt.Errorf("%w: coil value must be a boolean (or 0/1), got %v of type %T", ErrInvalidValue, raw, raw)
	}
	var n float64
	switch v := raw.(type) {
	case float64:
		n = v
	case float32:
		n = float64(v)
	case int:
		n = float64(v)
	case int32:
		n = float64(v)
	case int64:
		n = float64(v)
	case uint:
		n = float64(v)
	case uint16:
		n = float64(v)
	case uint32:
		n = float64(v)
	case uint64:
		n = float64(v)
	case json.Number:
		var err error
		n, err = v.Float64()
		if err != nil {
			return 0, false, fmt.Errorf("%w: %v", ErrInvalidValue, err)
		}
	default:
		return 0, false, fmt.Errorf("%w: value must be a number, got %v of type %T", ErrInvalidValue, raw, raw)
	}
	if math.IsNaN(n) || math.IsInf(n, 0) || n != math.Trunc(n) || n < 0 || n > 0xFFFF {
		return 0, false, fmt.Errorf("%w: value %v must be an integer between 0 and 65535", ErrInvalidValue, raw)
	}
	return uint16(n), false, nil
}

// readTool builds the MCP tool for a readable point (risk 0 unless the
// point overrides it). Read tools take no arguments.
func (a *ModbusAdapter) readTool(name string, r *RegisterDef) sdkproto.MCPTool {
	risk := 0
	if r.Risk != nil {
		risk = *r.Risk
	}
	return sdkproto.MCPTool{
		Name:          name,
		Description:   r.describe("Read"),
		InputSchema:   mustSchema(map[string]any{"type": "object", "properties": map[string]any{}}),
		RiskLevel:     &risk,
		SchemaVersion: toolSchemaVersion,
	}
}

// writeTool builds the MCP tool for a writable point (risk 2 unless the
// point overrides it). Write tools take {"value": ...}; the schema type
// depends on the register kind (number for registers, boolean for coil).
func (a *ModbusAdapter) writeTool(name string, r *RegisterDef) sdkproto.MCPTool {
	risk := 2
	if r.Risk != nil {
		risk = *r.Risk
	}
	valueType := "number"
	if r.Kind == KindCoil {
		valueType = "boolean"
	}
	return sdkproto.MCPTool{
		Name:        name,
		Description: r.describe("Write"),
		InputSchema: mustSchema(map[string]any{
			"type":       "object",
			"properties": map[string]any{"value": map[string]any{"type": valueType}},
			"required":   []string{"value"},
		}),
		RiskLevel:     &risk,
		SchemaVersion: toolSchemaVersion,
	}
}

// describe renders the human-readable tool description.
func (r *RegisterDef) describe(verb string) string {
	s := fmt.Sprintf("%s %s (%s register at address %d)", verb, r.Name, r.Kind, r.Address)
	if r.Unit != "" {
		s += ", unit: " + r.Unit
	}
	return s
}

// mustSchema marshals a tool input schema. The shapes used here cannot
// fail to marshal; a failure would be a programming error.
func mustSchema(v map[string]any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("adapters: marshal tool schema: %v", err))
	}
	return b
}
