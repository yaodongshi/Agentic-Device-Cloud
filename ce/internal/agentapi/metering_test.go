package agentapi

import (
	"context"
	"errors"
	"sync"
	"testing"

	"adc.dev/core-sdk/protocol"

	"adc.dev/ce/pkg/metering"
)

// spyMeter records events without any transport.
type spyMeter struct {
	mu     sync.Mutex
	events []*metering.UsageEvent
}

func (s *spyMeter) Record(ctx context.Context, evs ...*metering.UsageEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, evs...)
	return nil
}

func (s *spyMeter) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.events)
}

type resultRouter struct {
	result *CallResult
	err    error
}

func (r resultRouter) Call(ctx context.Context, tenantID string, ref ToolRef, args map[string]interface{}) (*CallResult, error) {
	return r.result, r.err
}

func TestMeteringRouterRecordsAfterCall(t *testing.T) {
	spy := &spyMeter{}
	r := MeteringRouter{
		Next:  resultRouter{result: &CallResult{Content: []protocol.ToolContent{{Type: "text", Text: "ok"}}}},
		Meter: spy,
	}
	res, err := r.Call(context.Background(), "tenant-1",
		ToolRef{DeviceID: "dev-1", ToolName: "set_rpm"}, map[string]interface{}{"rpm": 100})
	if err != nil || len(res.Content) != 1 {
		t.Fatalf("result must pass through: %+v err=%v", res, err)
	}
	if spy.count() != 1 {
		t.Fatalf("recorded %d events, want 1", spy.count())
	}
	ev := spy.events[0]
	if ev.TenantID != "tenant-1" || ev.Kind != metering.KindCall ||
		ev.Source != metering.SourceMCPGateway || ev.MetricKey != "dev-1" ||
		ev.Value != 1 || ev.Unit != metering.UnitCount || ev.IdempotencyKey == "" {
		t.Fatalf("unexpected usage event: %+v", ev)
	}
}

func TestMeteringRouterRecordsOnFailure(t *testing.T) {
	spy := &spyMeter{}
	r := MeteringRouter{
		Next:  resultRouter{err: ErrDeviceOffline},
		Meter: spy,
	}
	_, err := r.Call(context.Background(), "tenant-1", ToolRef{DeviceID: "dev-1"}, nil)
	if !errors.Is(err, ErrDeviceOffline) {
		t.Fatalf("inner error must pass through, got %v", err)
	}
	if spy.count() != 1 {
		t.Fatalf("failed calls are still usage (billing truth), recorded %d", spy.count())
	}
}

func TestMeteringRouterNilMeterIsNoop(t *testing.T) {
	r := MeteringRouter{Next: resultRouter{result: &CallResult{}}}
	if _, err := r.Call(context.Background(), "t", ToolRef{DeviceID: "d"}, nil); err != nil {
		t.Fatal(err)
	}
}
