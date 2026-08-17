// OPC-UA client adapter (design/83 C1.3, milestone M6) built on
// github.com/gopcua/opcua (MIT license; see the module LICENSE file —
// gopcua is MIT, not MPL-2.0, which applies to some other OPC-UA stacks
// such as open62541).
//
// The adapter maps a JSON node table to MCP tools: every readable point
// becomes node_read_{name} (risk 0) and every writable point
// node_write_{name} (risk 2), with an optional per-point risk override.
// Reads go through the OPC-UA Read service and writes through the Write
// service, both on the node's Value attribute.
//
// Security: V1 supports only SecurityPolicy None / MessageSecurityMode
// None. Sign and sign-and-encrypt modes are rejected at config parse
// time with an explicit "not supported in V1" error, because they
// require certificate distribution, trust management and key rotation
// (platform security backlog). Endpoints exposed on untrusted networks
// are therefore out of scope for V1 and must be shielded by network
// segmentation.
//
// Data typing is coarse by design (iot-protocol methodology): every node
// declares one of bool/number/string, which is what the write-tool
// schema and argument validation need. Exact UA DataType information
// (ByteString, arrays, structures, custom types) is deliberately not
// modeled; the raw value a server returns is echoed as-is in read
// results.
//
// Concurrency: every transport operation holds the adapter mutex, which
// serializes session (re)connect, per-call deadline handling and Close
// (same pattern as the Modbus adapter).
//
// Health: OPC-UA has no dedicated ping service; Health checks the
// session state and then performs a real Read round-trip on the first
// readable node as the liveness probe, so a half-dead session reads as
// unhealthy.
//
// Test layering (opcua_cert_test.go): the OPC-UA service layer is
// abstracted behind the NodeClient interface. The certification suite
// exercises the adapter logic against an in-memory NodeClient
// (deterministic error injection, disconnect and closed-adapter cases)
// and, because gopcua ships a server implementation, additionally runs a
// full in-process client/server session round-trip against gopcua's own
// server package. Lab-device and field validation against commercial
// OPC-UA servers remains an integration task (simulator -> lab device ->
// field, per the IoT protocol methodology).
package adapters

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	sdkproto "adc.dev/core-sdk/protocol"
	"github.com/gopcua/opcua"
	"github.com/gopcua/opcua/ua"
)

// OPCUAProtocol is the registry key (AdapterInfo.Protocol) of the
// built-in OPC-UA adapter.
const OPCUAProtocol = "opc-ua"

const (
	// opcuaAdapterVersion is the implementation version of this adapter.
	opcuaAdapterVersion = "1.0.0"
	// defaultOPCUATimeout bounds one transport round-trip when neither
	// the config nor the ctx deadline specifies anything shorter.
	defaultOPCUATimeout = 5 * time.Second

	nodeReadToolPrefix  = "node_read_"
	nodeWriteToolPrefix = "node_write_"
)

// nodeNamePattern keeps generated tool names clean: letters, digits,
// '-' and '_', not starting with '-' or '_'.
var nodeNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)

// SecurityMode is the message security mode of the endpoint.
type SecurityMode string

// Security modes recognized by the config grammar. Only None is
// supported in V1; Sign and SignAndEncrypt are parsed but rejected so
// configs written against future versions fail loudly instead of
// silently downgrading to an unencrypted connection.
const (
	SecurityModeNone           SecurityMode = "none"
	SecurityModeSign           SecurityMode = "sign"
	SecurityModeSignAndEncrypt SecurityMode = "signAndEncrypt"
)

// NodeDataType is the coarse-grained data type of a node. It drives
// write argument validation, the write-tool input schema and the "type"
// field of tool results. Exact UA DataType information is intentionally
// not modeled in V1.
type NodeDataType string

const (
	DataTypeBool   NodeDataType = "bool"
	DataTypeNumber NodeDataType = "number"
	DataTypeString NodeDataType = "string"
)

func (t NodeDataType) valid() bool {
	switch t {
	case DataTypeBool, DataTypeNumber, DataTypeString:
		return true
	}
	return false
}

// schemaValueType maps the coarse node type to a JSON schema type.
func (t NodeDataType) schemaValueType() string {
	switch t {
	case DataTypeBool:
		return "boolean"
	case DataTypeString:
		return "string"
	default:
		return "number"
	}
}

// OPCUANodeDef is one configured point (node) of the server.
type OPCUANodeDef struct {
	// Name identifies the point; the tools are node_read_{name} /
	// node_write_{name}. Letters, digits, '-' and '_' only.
	Name string `json:"name"`
	// NodeID is the OPC-UA node id string, e.g. "ns=2;s=Temperature"
	// or "i=85". Parsed with ua.ParseNodeID at validation time.
	NodeID string `json:"nodeId"`
	// Read exposes a read tool; Write exposes a write tool. At least
	// one must be true.
	Read  bool `json:"read"`
	Write bool `json:"write"`
	// DataType declares the coarse type (bool/number/string). Required
	// for every node in V1.
	DataType NodeDataType `json:"dataType"`
	// Unit is an optional engineering unit echoed in tool results.
	Unit string `json:"unit,omitempty"`
	// Risk overrides the default risk level (read 0, write 2) for both
	// tools of this point. Range 0-3.
	Risk *int `json:"risk,omitempty"`
}

// opcuaDuration is a time.Duration that unmarshals from JSON as either a
// Go duration string ("500ms", "2s") or a number of milliseconds (2000).
// It mirrors the Modbus adapter grammar but reports opc-ua errors.
type opcuaDuration time.Duration

// UnmarshalJSON implements json.Unmarshaler.
func (d *opcuaDuration) UnmarshalJSON(b []byte) error {
	if len(b) == 0 || string(b) == "null" {
		*d = 0
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		v, perr := time.ParseDuration(s)
		if perr != nil {
			return fmt.Errorf("opc-ua: invalid timeout %q: %w", s, perr)
		}
		if v < 0 {
			return errors.New("opc-ua: timeout must not be negative")
		}
		*d = opcuaDuration(v)
		return nil
	}
	var ms float64
	if err := json.Unmarshal(b, &ms); err != nil {
		return errors.New("opc-ua: timeout must be a duration string or a number of milliseconds")
	}
	if ms < 0 {
		return errors.New("opc-ua: timeout must not be negative")
	}
	*d = opcuaDuration(time.Duration(ms) * time.Millisecond)
	return nil
}

// OPCUAConfig is the JSON configuration of one OPC-UA adapter instance.
type OPCUAConfig struct {
	// Endpoint is the opc.tcp://host:port URL of the server.
	Endpoint string `json:"endpoint"`
	// Security selects the message security mode. Only "none" is
	// supported in V1; anything else is rejected at parse time.
	Security SecurityMode `json:"security,omitempty"`
	// Timeout bounds one transport round-trip. 0: defaultOPCUATimeout.
	Timeout opcuaDuration `json:"timeout,omitempty"`
	// Nodes is the node mapping table.
	Nodes []OPCUANodeDef `json:"nodes"`
}

// ParseOPCUAConfig decodes and validates a JSON config document.
func ParseOPCUAConfig(data []byte) (*OPCUAConfig, error) {
	var c OPCUAConfig
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("opc-ua: parse config: %w", err)
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// validate checks the config and normalizes the security mode. It may
// mutate c, so callers must not reuse a partially validated config.
func (c *OPCUAConfig) validate() error {
	if c.Endpoint == "" {
		return errors.New("opc-ua: endpoint must not be empty")
	}
	u, err := url.Parse(c.Endpoint)
	if err != nil || u.Scheme != "opc.tcp" || u.Host == "" {
		return fmt.Errorf("opc-ua: endpoint %q must be an opc.tcp://host:port URL", c.Endpoint)
	}
	switch strings.ToLower(string(c.Security)) {
	case "", "none":
		c.Security = SecurityModeNone
	default:
		return fmt.Errorf("opc-ua: security mode %q is not supported in V1: only \"none\" (SecurityPolicy None) is available; sign and sign&encrypt need certificate infrastructure and are deferred", c.Security)
	}
	if c.Timeout < 0 {
		return errors.New("opc-ua: timeout must not be negative")
	}
	if len(c.Nodes) == 0 {
		return errors.New("opc-ua: at least one node must be configured")
	}
	seen := make(map[string]bool, len(c.Nodes))
	for i := range c.Nodes {
		n := &c.Nodes[i]
		if n.Name == "" {
			return fmt.Errorf("opc-ua: node %d: name must not be empty", i)
		}
		if !nodeNamePattern.MatchString(n.Name) {
			return fmt.Errorf("opc-ua: node %d: invalid name %q (letters, digits, '-' and '_' only)", i, n.Name)
		}
		if seen[n.Name] {
			return fmt.Errorf("opc-ua: duplicate node name %q", n.Name)
		}
		seen[n.Name] = true
		if _, err := ua.ParseNodeID(n.NodeID); err != nil {
			return fmt.Errorf("opc-ua: node %q: invalid node id %q: %w", n.Name, n.NodeID, err)
		}
		if !n.Read && !n.Write {
			return fmt.Errorf("opc-ua: node %q: at least one of read/write must be enabled", n.Name)
		}
		if !n.DataType.valid() {
			return fmt.Errorf("opc-ua: node %q: dataType %q must be one of bool/number/string", n.Name, n.DataType)
		}
		if n.Risk != nil && (*n.Risk < 0 || *n.Risk > 3) {
			return fmt.Errorf("opc-ua: node %q: risk %d out of range 0-3", n.Name, *n.Risk)
		}
	}
	return nil
}

// OPC-UA specific sentinel errors.
var (
	// ErrOPCUAMissingValue: a write tool was called without the "value" argument.
	ErrOPCUAMissingValue = errors.New("opc-ua: write requires a \"value\" argument")
	// ErrOPCUAInvalidValue: the "value" argument is of the wrong type or not finite.
	ErrOPCUAInvalidValue = errors.New("opc-ua: invalid value")
)

// StatusError wraps a non-OK StatusCode the server returned for one node
// operation, so callers can classify device-side failures (bad node id,
// access denied, ...).
type StatusError struct {
	Op     string // "read" or "write"
	NodeID string
	Status ua.StatusCode
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("opc-ua: %s node %s: %v", e.Op, e.NodeID, e.Status)
}

// NodeClient abstracts the OPC-UA service calls the adapter needs: a
// single-node Read (Read service on the Value attribute), a single-node
// Write and the session state. The production implementation
// (gopcuaNodeClient) wraps *opcua.Client; opcua_cert_test.go injects an
// in-memory fake to drive adapter logic deterministically. Real session
// behavior (secure channel, session activation, reconnects) is covered
// by the in-process gopcua server round-trip in the cert suite.
type NodeClient interface {
	// Read reads the Value attribute of one node. The returned DataValue
	// carries an OK status; a non-OK node status is returned as *StatusError.
	Read(ctx context.Context, nodeID *ua.NodeID) (*ua.DataValue, error)
	// Write writes the Value attribute of one node. A non-OK node status
	// is returned as *StatusError.
	Write(ctx context.Context, nodeID *ua.NodeID, value *ua.DataValue) error
	// Connected reports whether a live session is established.
	Connected() bool
	// Close tears down the session and secure channel. Idempotent.
	Close(ctx context.Context) error
}

// gopcuaNodeClient adapts *opcua.Client to the NodeClient interface. It
// connects lazily on the first operation and drops a broken session on
// transport errors so the next operation reconnects from scratch. All
// connect/close transitions are serialized by connMu; the enclosing
// adapter mutex already serializes Read/Write calls, and the gopcua
// secure channel serializes request dispatch.
type gopcuaNodeClient struct {
	c      *opcua.Client
	connMu sync.Mutex
}

// connect establishes the session if needed. The caller must not hold
// connMu.
func (g *gopcuaNodeClient) connect(ctx context.Context) error {
	g.connMu.Lock()
	defer g.connMu.Unlock()
	if g.c.State() == opcua.Connected {
		return nil
	}
	if err := g.c.Connect(ctx); err != nil {
		return fmt.Errorf("opc-ua: connect: %w", err)
	}
	return nil
}

// Connected implements NodeClient.
func (g *gopcuaNodeClient) Connected() bool {
	return g.c.State() == opcua.Connected
}

// Read implements NodeClient via the OPC-UA Read service.
func (g *gopcuaNodeClient) Read(ctx context.Context, nodeID *ua.NodeID) (*ua.DataValue, error) {
	if err := g.connect(ctx); err != nil {
		return nil, err
	}
	resp, err := g.c.Read(ctx, &ua.ReadRequest{
		NodesToRead: []*ua.ReadValueID{{NodeID: nodeID, AttributeID: ua.AttributeIDValue}},
	})
	if err != nil {
		g.reset()
		return nil, fmt.Errorf("opc-ua: read node %s: %w", nodeID, err)
	}
	if len(resp.Results) != 1 {
		return nil, fmt.Errorf("opc-ua: read node %s: server returned %d results, want 1", nodeID, len(resp.Results))
	}
	if dv := resp.Results[0]; dv.Status != ua.StatusOK {
		return nil, &StatusError{Op: "read", NodeID: nodeID.String(), Status: dv.Status}
	}
	return resp.Results[0], nil
}

// Write implements NodeClient via the OPC-UA Write service.
func (g *gopcuaNodeClient) Write(ctx context.Context, nodeID *ua.NodeID, value *ua.DataValue) error {
	if err := g.connect(ctx); err != nil {
		return err
	}
	resp, err := g.c.Write(ctx, &ua.WriteRequest{
		NodesToWrite: []*ua.WriteValue{{
			NodeID:      nodeID,
			AttributeID: ua.AttributeIDValue,
			Value:       value,
		}},
	})
	if err != nil {
		g.reset()
		return fmt.Errorf("opc-ua: write node %s: %w", nodeID, err)
	}
	if len(resp.Results) != 1 {
		return fmt.Errorf("opc-ua: write node %s: server returned %d results, want 1", nodeID, len(resp.Results))
	}
	if st := resp.Results[0]; st != ua.StatusOK {
		return &StatusError{Op: "write", NodeID: nodeID.String(), Status: st}
	}
	return nil
}

// reset tears down a dead session best-effort so the next operation
// reconnects cleanly. It runs only after a transport error, i.e. when
// the channel is already unusable; the bounded context keeps a
// half-open Close from blocking the caller.
func (g *gopcuaNodeClient) reset() {
	g.connMu.Lock()
	defer g.connMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	_ = g.c.Close(ctx)
	cancel()
}

// Close implements NodeClient. It is idempotent (gopcua Close returns
// nil for an already-closed client).
func (g *gopcuaNodeClient) Close(ctx context.Context) error {
	g.connMu.Lock()
	defer g.connMu.Unlock()
	return g.c.Close(ctx)
}

var _ NodeClient = (*gopcuaNodeClient)(nil)

// OPCUAAdapter is the built-in OPC-UA client adapter (design/83 C1.3).
type OPCUAAdapter struct {
	cfg       OPCUAConfig
	client    NodeClient
	tools     []sdkproto.MCPTool
	byName    map[string]*OPCUANodeDef
	nodeIDs   map[string]*ua.NodeID
	detail    string
	healthy   atomic.Bool
	closed    bool // guarded by mu
	mu        sync.Mutex
	closeOnce sync.Once
	closeErr  error
}

// NewOPCUAAdapter builds an adapter with a real gopcua client from a
// validated config. The session is established lazily on the first
// transport call.
func NewOPCUAAdapter(cfg *OPCUAConfig) (*OPCUAAdapter, error) {
	if cfg == nil {
		return nil, errors.New("opc-ua: nil config")
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	timeout := time.Duration(cfg.Timeout)
	if timeout == 0 {
		timeout = defaultOPCUATimeout
	}
	c, err := opcua.NewClient(cfg.Endpoint,
		opcua.SecurityMode(ua.MessageSecurityModeNone),
		opcua.SecurityPolicy(ua.SecurityPolicyURINone),
		opcua.RequestTimeout(timeout),
		// The adapter owns the session lifecycle; the library-level
		// auto-reconnect loop would fight the adapter's own
		// reset/connect logic.
		opcua.AutoReconnect(false),
	)
	if err != nil {
		return nil, fmt.Errorf("opc-ua: create client: %w", err)
	}
	return newOPCUAAdapter(cfg, &gopcuaNodeClient{c: c})
}

// newOPCUAAdapter wires an adapter onto any NodeClient; the cert suite
// injects an in-memory fake here. The caller must have validated cfg.
func newOPCUAAdapter(cfg *OPCUAConfig, client NodeClient) (*OPCUAAdapter, error) {
	nodes := make([]OPCUANodeDef, len(cfg.Nodes))
	copy(nodes, cfg.Nodes)
	a := &OPCUAAdapter{
		cfg:     OPCUAConfig{Endpoint: cfg.Endpoint, Security: cfg.Security, Timeout: cfg.Timeout, Nodes: nodes},
		client:  client,
		byName:  make(map[string]*OPCUANodeDef, len(nodes)*2),
		nodeIDs: make(map[string]*ua.NodeID, len(nodes)),
		detail:  cfg.Endpoint,
	}
	for i := range a.cfg.Nodes {
		n := &a.cfg.Nodes[i]
		nid, err := ua.ParseNodeID(n.NodeID)
		if err != nil {
			return nil, fmt.Errorf("opc-ua: node %q: %w", n.Name, err)
		}
		a.nodeIDs[n.Name] = nid
		if n.Read {
			name := nodeReadToolPrefix + n.Name
			a.byName[name] = n
			a.tools = append(a.tools, a.readTool(name, n))
		}
		if n.Write {
			name := nodeWriteToolPrefix + n.Name
			a.byName[name] = n
			a.tools = append(a.tools, a.writeTool(name, n))
		}
	}
	return a, nil
}

// Info implements Adapter.
func (a *OPCUAAdapter) Info() AdapterInfo {
	return AdapterInfo{
		Protocol: OPCUAProtocol,
		Version:  opcuaAdapterVersion,
		Healthy:  a.healthy.Load(),
		Detail:   a.detail,
	}
}

// ListTools implements Adapter. The returned slice is a copy, safe to
// hold and mutate by the caller.
func (a *OPCUAAdapter) ListTools() []sdkproto.MCPTool {
	return append([]sdkproto.MCPTool(nil), a.tools...)
}

// CallTool implements Adapter. Read tools take no arguments; write tools
// take {"value": ...} typed by the node's coarse data type.
func (a *OPCUAAdapter) CallTool(ctx context.Context, name string, args map[string]any) (sdkproto.ToolCallResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return sdkproto.ToolCallResult{}, ErrAdapterClosed
	}
	n, ok := a.byName[name]
	if !ok {
		return sdkproto.ToolCallResult{}, fmt.Errorf("%w: %s", ErrToolNotFound, name)
	}
	if err := ctx.Err(); err != nil {
		return sdkproto.ToolCallResult{}, err
	}
	if strings.HasPrefix(name, nodeWriteToolPrefix) {
		value, err := parseOPCUAWriteValue(n, args)
		if err != nil {
			return sdkproto.ToolCallResult{}, err
		}
		variant, err := ua.NewVariant(value)
		if err != nil {
			return sdkproto.ToolCallResult{}, fmt.Errorf("%w: %v", ErrOPCUAInvalidValue, err)
		}
		if err := a.client.Write(ctx, a.nodeIDs[n.Name], &ua.DataValue{
			EncodingMask: ua.DataValueValue,
			Value:        variant,
		}); err != nil {
			return sdkproto.ToolCallResult{}, fmt.Errorf("opc-ua: write %s: %w", n.Name, err)
		}
		return a.result(n, value), nil
	}
	value, err := a.read(ctx, n)
	if err != nil {
		return sdkproto.ToolCallResult{}, fmt.Errorf("opc-ua: read %s: %w", n.Name, err)
	}
	return a.result(n, value), nil
}

// Health implements Adapter: it checks the session state and performs a
// real Read round-trip on the first readable node (OPC-UA has no ping
// service; the read doubles as the liveness ping), so a half-dead
// session reports unhealthy. A write-only node table cannot be
// health-checked and is reported as an error.
func (a *OPCUAAdapter) Health(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return ErrAdapterClosed
	}
	probe, ok := a.firstReadable()
	if !ok {
		return errors.New("opc-ua: no readable node configured for health probe")
	}
	if _, err := a.read(ctx, probe); err != nil {
		a.healthy.Store(false)
		if !a.client.Connected() {
			return fmt.Errorf("opc-ua: health: session not connected: %w", err)
		}
		return fmt.Errorf("opc-ua: health: %w", err)
	}
	a.healthy.Store(true)
	return nil
}

// Close implements Adapter. It waits for in-flight calls (they hold the
// transport mutex) and then closes the session; further calls fail with
// ErrAdapterClosed. Calling Close twice is safe.
func (a *OPCUAAdapter) Close() error {
	a.closeOnce.Do(func() {
		a.mu.Lock()
		a.closed = true
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		a.closeErr = a.client.Close(ctx)
		cancel()
		a.healthy.Store(false)
		a.mu.Unlock()
	})
	return a.closeErr
}

// firstReadable returns the first read-enabled point, used as the health
// probe.
func (a *OPCUAAdapter) firstReadable() (*OPCUANodeDef, bool) {
	for i := range a.cfg.Nodes {
		if a.cfg.Nodes[i].Read {
			return &a.cfg.Nodes[i], true
		}
	}
	return nil, false
}

// read performs one point read and returns the raw server value. The
// caller must hold a.mu.
func (a *OPCUAAdapter) read(ctx context.Context, n *OPCUANodeDef) (any, error) {
	dv, err := a.client.Read(ctx, a.nodeIDs[n.Name])
	if err != nil {
		return nil, err
	}
	if dv.Value == nil {
		return nil, nil
	}
	return dv.Value.Value(), nil
}

// parseOPCUAWriteValue validates the "value" argument against the node's
// declared coarse data type: bool accepts a boolean (or 0/1, as produced
// by JSON decoders that only know numbers), number accepts any finite
// JSON number, string accepts a string.
func parseOPCUAWriteValue(n *OPCUANodeDef, args map[string]any) (any, error) {
	raw, ok := args["value"]
	if !ok {
		return nil, ErrOPCUAMissingValue
	}
	switch n.DataType {
	case DataTypeBool:
		switch v := raw.(type) {
		case bool:
			return v, nil
		case float64:
			if v == 0 {
				return false, nil
			}
			if v == 1 {
				return true, nil
			}
		}
		return nil, fmt.Errorf("%w: bool value must be a boolean (or 0/1), got %v of type %T", ErrOPCUAInvalidValue, raw, raw)
	case DataTypeString:
		if v, ok := raw.(string); ok {
			return v, nil
		}
		return nil, fmt.Errorf("%w: string value must be a string, got %v of type %T", ErrOPCUAInvalidValue, raw, raw)
	case DataTypeNumber:
		var f float64
		switch v := raw.(type) {
		case float64:
			f = v
		case float32:
			f = float64(v)
		case int:
			f = float64(v)
		case int8:
			f = float64(v)
		case int16:
			f = float64(v)
		case int32:
			f = float64(v)
		case int64:
			f = float64(v)
		case uint:
			f = float64(v)
		case uint8:
			f = float64(v)
		case uint16:
			f = float64(v)
		case uint32:
			f = float64(v)
		case uint64:
			f = float64(v)
		case json.Number:
			var err error
			f, err = v.Float64()
			if err != nil {
				return nil, fmt.Errorf("%w: %v", ErrOPCUAInvalidValue, err)
			}
		default:
			return nil, fmt.Errorf("%w: number value must be a number, got %v of type %T", ErrOPCUAInvalidValue, raw, raw)
		}
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return nil, fmt.Errorf("%w: value %v must be finite", ErrOPCUAInvalidValue, raw)
		}
		return f, nil
	}
	return nil, fmt.Errorf("%w: node data type %q", ErrOPCUAInvalidValue, n.DataType)
}

// readTool builds the MCP tool for a readable point (risk 0 unless the
// point overrides it). Read tools take no arguments.
func (a *OPCUAAdapter) readTool(name string, n *OPCUANodeDef) sdkproto.MCPTool {
	risk := 0
	if n.Risk != nil {
		risk = *n.Risk
	}
	return sdkproto.MCPTool{
		Name:          name,
		Description:   n.describe("Read"),
		InputSchema:   mustSchema(map[string]any{"type": "object", "properties": map[string]any{}}),
		RiskLevel:     &risk,
		SchemaVersion: toolSchemaVersion,
	}
}

// writeTool builds the MCP tool for a writable point (risk 2 unless the
// point overrides it). Write tools take {"value": ...}; the schema type
// depends on the node's coarse data type.
func (a *OPCUAAdapter) writeTool(name string, n *OPCUANodeDef) sdkproto.MCPTool {
	risk := 2
	if n.Risk != nil {
		risk = *n.Risk
	}
	return sdkproto.MCPTool{
		Name:        name,
		Description: n.describe("Write"),
		InputSchema: mustSchema(map[string]any{
			"type":       "object",
			"properties": map[string]any{"value": map[string]any{"type": n.DataType.schemaValueType()}},
			"required":   []string{"value"},
		}),
		RiskLevel:     &risk,
		SchemaVersion: toolSchemaVersion,
	}
}

// describe renders the human-readable tool description.
func (n *OPCUANodeDef) describe(verb string) string {
	s := fmt.Sprintf("%s node %s (%s, data type %s)", verb, n.Name, n.NodeID, n.DataType)
	if n.Unit != "" {
		s += ", unit: " + n.Unit
	}
	return s
}

// result wraps a decoded value in the JSON text block every tool returns.
func (a *OPCUAAdapter) result(n *OPCUANodeDef, value any) sdkproto.ToolCallResult {
	b, err := json.Marshal(nodeReading{
		Name:   n.Name,
		NodeID: n.NodeID,
		Type:   string(n.DataType),
		Unit:   n.Unit,
		Value:  value,
	})
	if err != nil {
		// Impossible for these shapes; keep the result well-formed anyway.
		return sdkproto.ToolCallResult{
			Content: []sdkproto.ToolContent{{Type: "text", Text: fmt.Sprintf("opc-ua: encode result: %v", err)}},
			IsError: true,
		}
	}
	return sdkproto.ToolCallResult{
		Content: []sdkproto.ToolContent{{Type: "text", Text: string(b)}},
		IsError: false,
	}
}

// nodeReading is the JSON shape of every tool result.
type nodeReading struct {
	Name   string `json:"name"`
	NodeID string `json:"nodeId"`
	Type   string `json:"type"`
	Unit   string `json:"unit,omitempty"`
	Value  any    `json:"value"`
}

var _ Adapter = (*OPCUAAdapter)(nil)
