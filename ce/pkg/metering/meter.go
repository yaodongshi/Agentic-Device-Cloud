// Package metering implements the FR-016 usage metering pipeline (B-11,
// design/32 3.11): producers record events onto a Valkey queue and a
// worker drains the queue into adc_usage_events, where the
// idempotency_key unique index makes redelivery harmless (design/32 6.3).
// V1.0 events feed reconciliation and dashboards only — billing activates
// in V1.5 — so the pipeline may trail up to 5 minutes (design/32 1.1).
package metering

import (
	"context"
	"fmt"
	"time"
)

// Kind values mirror the adc_usage_events.event_type CHECK constraint
// (design/32 3.11). Keep them in sync when the enum evolves.
const (
	KindDeviceDaily = "DEVICE_DAILY_PEAK"
	KindCall        = "TOOL_CALL"
	KindToken       = "TOKEN_USAGE"
)

// Source values for adc_usage_events.source.
const (
	SourceMCPGateway = "mcp_gateway"
	SourceLLMGateway = "llm_gateway"
	SourceA2AGateway = "a2a_gateway"
)

// Unit values for adc_usage_events.unit.
const (
	UnitCount = "count"
	UnitToken = "token"
)

// UsageEvent is the wire shape of one metering record, mapped onto
// adc_usage_events columns by the worker:
//
//	Kind           -> event_type
//	Source         -> source
//	MetricKey      -> metric_key (device_id / api_key_id / model)
//	Value          -> metric_value
//	Unit           -> unit
//	OccurredAt     -> occurred_at (business time, daily-peak aggregation)
//	IdempotencyKey -> idempotency_key (unique index, design/32 6.3)
//	Meta           -> meta
type UsageEvent struct {
	TenantID       string         `json:"tenant_id"`
	Kind           string         `json:"kind"`
	Source         string         `json:"source,omitempty"`
	MetricKey      string         `json:"metric_key,omitempty"`
	Value          int64          `json:"value"`
	Unit           string         `json:"unit,omitempty"`
	OccurredAt     time.Time      `json:"occurred_at"`
	IdempotencyKey string         `json:"idempotency_key"`
	Meta           map[string]any `json:"meta,omitempty"`
}

// Validate rejects events that would violate adc_usage_events NOT NULL
// constraints or the event_type CHECK constraint.
func (e *UsageEvent) Validate() error {
	if e == nil {
		return fmt.Errorf("metering: nil event")
	}
	if e.TenantID == "" {
		return fmt.Errorf("metering: tenant_id is required")
	}
	switch e.Kind {
	case KindDeviceDaily, KindCall, KindToken:
	default:
		return fmt.Errorf("metering: invalid kind %q (want DEVICE_DAILY_PEAK | TOOL_CALL | TOKEN_USAGE)", e.Kind)
	}
	if e.Value < 0 {
		return fmt.Errorf("metering: value must not be negative, got %d", e.Value)
	}
	if e.OccurredAt.IsZero() {
		return fmt.Errorf("metering: occurred_at is required")
	}
	if e.IdempotencyKey == "" {
		return fmt.Errorf("metering: idempotency_key is required (unique index, design/32 6.3)")
	}
	return nil
}

// Meter records usage events for asynchronous persistence (B-11).
// Implementations enqueue fast and never block the call hot path for the
// PG insert (design/32 1.2); Record errors mean the event was not queued
// and callers treat them as best-effort.
type Meter interface {
	Record(ctx context.Context, evs ...*UsageEvent) error
}
