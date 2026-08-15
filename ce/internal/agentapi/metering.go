package agentapi

import (
	"context"
	"time"

	"adc.dev/ce/pkg/metering"
)

// MeteringRouter decorates ToolRouter with usage metering (B-11,
// design/32 3.11): every tool call that reaches the router records one
// TOOL_CALL usage event after execution. Both call paths — the direct
// no-approval path and the post-approval execution — flow through
// ToolRouter.Call (design/31 3.2.5), so this single seam covers
// "免审与审批执行后" with zero changes to Server.
//
// Recording is best-effort and bounded: a metering outage must never
// change the call outcome (design/32 1.1 allows up to 5 minutes of
// metering lag; the idempotency_key unique index absorbs redelivery).
//
// cmd wiring shape (B-11):
//
//	meter := metering.NewValkeyMeter(rdb)
//	router = agentapi.MeteringRouter{Next: router, Meter: meter}
type MeteringRouter struct {
	Next  ToolRouter
	Meter metering.Meter // nil disables recording (tests, CE without metering)
}

// Call delegates to the inner router and records the usage event with a
// fresh idempotency key (design/32 6.3: producer-generated UUID).
func (r MeteringRouter) Call(ctx context.Context, tenantID string, ref ToolRef, args map[string]interface{}) (*CallResult, error) {
	result, err := r.Next.Call(ctx, tenantID, ref, args)
	r.record(tenantID, ref)
	return result, err
}

// record enqueues the TOOL_CALL event under a short timeout; failures are
// dropped (best-effort, never surfaces to the caller).
func (r MeteringRouter) record(tenantID string, ref ToolRef) {
	if r.Meter == nil {
		return
	}
	recCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = r.Meter.Record(recCtx, &metering.UsageEvent{
		TenantID:       tenantID,
		Kind:           metering.KindCall,
		Source:         metering.SourceMCPGateway,
		MetricKey:      ref.DeviceID,
		Value:          1,
		Unit:           metering.UnitCount,
		OccurredAt:     time.Now().UTC(),
		IdempotencyKey: newEventID(),
		Meta:           map[string]any{"tool": ref.ToolName},
	})
}
