// Package adapters implements the protocol adapter framework of ADC
// (design/83 C1.1, milestone M5) together with the built-in Modbus TCP
// adapter (C1.2, modbus.go).
//
// An adapter turns a physical protocol into MCP tools. Every readable
// point of a device becomes a read tool (risk 0), every writable point a
// write tool (risk 2). Risk levels are metadata only: the platform HITL
// flow intercepts high-risk calls before they are ever routed back to an
// adapter (design/31 3.2.2), so adapters themselves never implement
// approval logic.
//
// The framework follows the frozen-seam style used across the codebase
// (design/31 1.2: interface plus registry injection). Registry is the
// single assembly point: cmd wiring constructs adapters, registers them
// by protocol name and exposes Registry.AdaptersStatus on the console
// page (design/83 C1.5).
//
// # Adding a new protocol adapter
//
//  1. Implement the Adapter interface:
//     Info      — protocol name, adapter version, cached health and a
//     human-readable detail (target address). The protocol name is the
//     registry identity; pick one string and keep it stable.
//     ListTools — map the protocol points to protocol.MCPTool values.
//     Read points are risk 0, write points risk 2 unless a point
//     overrides the level. Tool names must be unique per adapter.
//     CallTool  — execute one tool against the device. Honor ctx as a
//     deadline, never leak goroutines on return, and wrap transport
//     errors with %w so the platform can classify them (design/31
//     1.3.4).
//     Health    — perform a real device round-trip (not just a dial) so
//     a half-dead connection reads as unhealthy, and cache the outcome
//     for Info.
//     Close     — idempotent transport shutdown.
//  2. Register the instance at assembly time: reg.Register(adapter).
//     Duplicate protocol names are rejected with ErrDuplicateAdapter.
//  3. Expose it on the console through Registry.AdaptersStatus, which
//     sweeps Health over every registered adapter.
//  4. Certify the adapter with the C1.6 suite (design/83 C1.6): mirror
//     cert_test.go — discovery, read, write round-trip, error handling,
//     concurrent reads, health after disconnect, risk mapping. Per the
//     IoT protocol methodology an adapter counts as supported only
//     after its cert tests pass (simulator -> lab device -> field).
package adapters

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	sdkproto "adc.dev/core-sdk/protocol"
)

// AdapterInfo is the static identity of an adapter plus the cached last
// health outcome. The registry keys adapters by Protocol.
type AdapterInfo struct {
	Protocol string // canonical protocol name, e.g. "modbus-tcp"
	Version  string // adapter implementation version
	Healthy  bool   // outcome of the last Health run
	Detail   string // human-readable target, e.g. "192.168.1.10:502"
}

// AdapterStatus is one row of the console health view (design/83 C1.5):
// identity plus a fresh health outcome from the last sweep. Err carries
// the health error text when Healthy is false.
type AdapterStatus struct {
	Protocol string
	Version  string
	Detail   string
	Healthy  bool
	Err      string
}

// Adapter is the plugin contract every protocol implementation fulfills
// (design/83 C1.1). Implementations must be safe for concurrent use:
// CallTool, Health and Close may be invoked from multiple goroutines.
type Adapter interface {
	// Info returns identity metadata plus the cached last health outcome.
	Info() AdapterInfo
	// ListTools maps the configured protocol points to MCP tools
	// (read point = risk 0, write point = risk 2 unless overridden).
	ListTools() []sdkproto.MCPTool
	// CallTool executes one tool against the device. Failures are
	// returned as errors (wrapped with %w); ctx bounds the operation.
	CallTool(ctx context.Context, name string, args map[string]any) (sdkproto.ToolCallResult, error)
	// Health performs a real device round-trip and caches the outcome.
	Health(ctx context.Context) error
	// Close releases the transport. It must be idempotent.
	Close() error
}

// Sentinel errors shared by all adapters.
var (
	// ErrDuplicateAdapter is returned by Registry.Register when the
	// protocol name is already taken.
	ErrDuplicateAdapter = errors.New("adapters: protocol already registered")
	// ErrToolNotFound is returned by CallTool on an unknown tool name.
	ErrToolNotFound = errors.New("adapters: tool not found")
	// ErrAdapterClosed is returned by any operation on a closed adapter.
	ErrAdapterClosed = errors.New("adapters: adapter closed")
)

// Registry is the in-process adapter registry (design/83 C1.1). It keys
// adapters by protocol name and is safe for concurrent use.
type Registry struct {
	mu      sync.RWMutex
	byProto map[string]Adapter
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{byProto: make(map[string]Adapter)}
}

// Register adds an adapter under its protocol name. The adapter identity
// is Info().Protocol; registering a duplicate name fails with
// ErrDuplicateAdapter.
func (r *Registry) Register(a Adapter) error {
	if a == nil {
		return errors.New("adapters: nil adapter")
	}
	name := a.Info().Protocol
	if name == "" {
		return errors.New("adapters: adapter protocol name must not be empty")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.byProto[name]; exists {
		return fmt.Errorf("%w: %s", ErrDuplicateAdapter, name)
	}
	r.byProto[name] = a
	return nil
}

// Unregister removes the adapter registered under protocol, if any. The
// adapter is not closed here; the caller owns its lifecycle.
func (r *Registry) Unregister(protocol string) {
	r.mu.Lock()
	delete(r.byProto, protocol)
	r.mu.Unlock()
}

// Get returns the adapter registered under protocol.
func (r *Registry) Get(protocol string) (Adapter, bool) {
	r.mu.RLock()
	a, ok := r.byProto[protocol]
	r.mu.RUnlock()
	return a, ok
}

// List returns the static info of every registered adapter, sorted by
// protocol name for a stable console view. The Healthy field carries the
// cached last-health outcome; use AdaptersStatus for fresh values.
func (r *Registry) List() []AdapterInfo {
	r.mu.RLock()
	adapters := make([]Adapter, 0, len(r.byProto))
	for _, a := range r.byProto {
		adapters = append(adapters, a)
	}
	r.mu.RUnlock()
	out := make([]AdapterInfo, 0, len(adapters))
	for _, a := range adapters {
		out = append(out, a.Info())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Protocol < out[j].Protocol })
	return out
}

// healthTimeout bounds each adapter's Health call during a status sweep
// so one unreachable device cannot stall the console page.
const healthTimeout = 2 * time.Second

// AdaptersStatus aggregates a fresh health sweep over all registered
// adapters for the console page (design/83 C1.5). Each adapter's Health
// runs with its own timeout; a failing adapter is reported unhealthy
// with the error text, never failing the whole sweep.
func (r *Registry) AdaptersStatus(ctx context.Context) []AdapterStatus {
	if ctx == nil {
		ctx = context.Background()
	}
	r.mu.RLock()
	adapters := make([]Adapter, 0, len(r.byProto))
	for _, a := range r.byProto {
		adapters = append(adapters, a)
	}
	r.mu.RUnlock()
	out := make([]AdapterStatus, 0, len(adapters))
	for _, a := range adapters {
		info := a.Info()
		st := AdapterStatus{
			Protocol: info.Protocol,
			Version:  info.Version,
			Detail:   info.Detail,
		}
		hctx, cancel := context.WithTimeout(ctx, healthTimeout)
		if err := a.Health(hctx); err != nil {
			st.Err = err.Error()
		} else {
			st.Healthy = true
		}
		cancel()
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Protocol < out[j].Protocol })
	return out
}
