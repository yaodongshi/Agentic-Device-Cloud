package alerts

import (
	"context"
	"sync"
	"testing"
	"time"
)

// evalFixture assembles an evaluator over a static reader, a memory store,
// a memory deduper and a recording notifier with a fake clock.
type evalFixture struct {
	store    *MemoryRuleStore
	reader   *StaticReader
	dedup    *MemoryDeduper
	notifier *recordingNotifier
	events   *EventBuffer
	eval     *Evaluator
	now      time.Time
}

func newEvalFixture(rules []Rule, series []Series) *evalFixture {
	f := &evalFixture{
		store:    NewMemoryRuleStore(),
		reader:   &StaticReader{Series: series},
		dedup:    NewMemoryDeduper(),
		notifier: &recordingNotifier{},
		events:   NewEventBuffer(100),
		now:      fixedClock,
	}
	f.dedup.now = func() time.Time { return f.now }
	for i := range rules {
		rules[i].TenantID = testTenantA
		if rules[i].ID == "" {
			rules[i].ID = NewRuleID()
		}
	}
	_ = f.store.Replace(context.Background(), testTenantA, rules)
	f.eval = &Evaluator{
		Store:  f.store,
		Reader: f.reader,
		Notify: f.notifier,
		Dedup:  f.dedup,
		Events: f.events,
		Now:    func() time.Time { return f.now },
	}
	return f
}

func (f *evalFixture) tick() { f.eval.EvaluateOnce(context.Background()) }

func thresholdRule(name, metric string, thr float64, durSec int) Rule {
	return Rule{
		Name:        name,
		Metric:      metric,
		Operator:    OpGreater,
		Threshold:   thr,
		DurationSec: durSec,
		Severity:    SeverityP2,
		Enabled:     true,
	}
}

// TestEvaluatorFiresOnFirstCrossing covers FR-017 "1 分钟内发出告警":
// duration 0 fires on the very first evaluation after the threshold is
// crossed (≤30s with the default interval).
func TestEvaluatorFiresOnFirstCrossing(t *testing.T) {
	f := newEvalFixture([]Rule{thresholdRule("pool", "adc_pg_pool_connections", 0.8, 0)},
		[]Series{{Name: "adc_pg_pool_connections", Labels: map[string]string{"state": "acquired"}, Value: 9}})
	f.tick()
	if f.notifier.calls != 1 {
		t.Fatalf("notify calls = %d, want 1", f.notifier.calls)
	}
	if f.events.Len() != 1 {
		t.Fatalf("events = %d, want 1", f.events.Len())
	}
	evs := f.events.List(testTenantA, 10)
	if evs[0].Observed != 9 || evs[0].Severity != SeverityP2 || evs[0].RuleName != "pool" {
		t.Fatalf("event = %+v", evs[0])
	}
}

// TestEvaluatorDurationRequiresSustainedCondition: duration_sec=90 with 30s
// ticks fires only after three consecutive failing evaluations (the FR-017
// "连续 3 次失败" analog).
func TestEvaluatorDurationRequiresSustainedCondition(t *testing.T) {
	f := newEvalFixture([]Rule{thresholdRule("pool", "adc_pg_pool_connections", 0.8, 90)},
		[]Series{{Name: "adc_pg_pool_connections", Labels: map[string]string{"state": "acquired"}, Value: 9}})
	f.tick() // t+0s: hold begins
	if f.notifier.calls != 0 {
		t.Fatalf("notify calls after first tick = %d, want 0", f.notifier.calls)
	}
	f.now = f.now.Add(30 * time.Second)
	f.tick() // t+30s
	if f.notifier.calls != 0 {
		t.Fatalf("notify calls after second tick = %d, want 0", f.notifier.calls)
	}
	f.now = f.now.Add(30 * time.Second)
	f.tick() // t+60s
	if f.notifier.calls != 0 {
		t.Fatalf("notify calls after third tick = %d, want 0", f.notifier.calls)
	}
	f.now = f.now.Add(30 * time.Second)
	f.tick() // t+90s: duration reached
	if f.notifier.calls != 1 {
		t.Fatalf("notify calls after fourth tick = %d, want 1", f.notifier.calls)
	}
}

// TestEvaluatorRecoveryResetsHold: a dip below threshold cancels the
// pending hold and a later crossing starts the duration clock again.
func TestEvaluatorRecoveryResetsHold(t *testing.T) {
	f := newEvalFixture([]Rule{thresholdRule("pool", "adc_pg_pool_connections", 0.8, 60)},
		[]Series{{Name: "adc_pg_pool_connections", Labels: map[string]string{"state": "acquired"}, Value: 9}})
	f.tick() // hold starts
	f.reader.Series = []Series{{Name: "adc_pg_pool_connections", Labels: map[string]string{"state": "acquired"}, Value: 0.5}}
	f.now = f.now.Add(30 * time.Second)
	f.tick() // recovered: hold reset
	f.reader.Series = []Series{{Name: "adc_pg_pool_connections", Labels: map[string]string{"state": "acquired"}, Value: 9}}
	f.now = f.now.Add(30 * time.Second)
	f.tick() // crossing again: fresh hold, not yet due
	if f.notifier.calls != 0 {
		t.Fatalf("notify calls = %d, want 0 (hold restarted)", f.notifier.calls)
	}
	f.now = f.now.Add(60 * time.Second)
	f.tick()
	if f.notifier.calls != 1 {
		t.Fatalf("notify calls = %d, want 1 after fresh 60s hold", f.notifier.calls)
	}
}

// TestEvaluatorDedupWindowSuppressesRefires: FR-017 aggregation — the same
// rule emits at most one alert per 5 minutes while the condition keeps
// holding, then re-fires after the window.
func TestEvaluatorDedupWindowSuppressesRefires(t *testing.T) {
	f := newEvalFixture([]Rule{thresholdRule("pool", "adc_pg_pool_connections", 0.8, 0)},
		[]Series{{Name: "adc_pg_pool_connections", Labels: map[string]string{"state": "acquired"}, Value: 9}})
	f.tick() // fire #1
	if f.notifier.calls != 1 {
		t.Fatalf("notify calls = %d, want 1", f.notifier.calls)
	}
	for i := 0; i < 9; i++ { // +4:30 of ticks, all suppressed
		f.now = f.now.Add(30 * time.Second)
		f.tick()
	}
	if f.notifier.calls != 1 {
		t.Fatalf("notify calls during window = %d, want 1 (aggregated)", f.notifier.calls)
	}
	if f.events.Len() != 1 {
		t.Fatalf("events during window = %d, want 1 (aggregated)", f.events.Len())
	}
	f.now = f.now.Add(DefaultDedupWindow) // well past the 5m window
	f.tick()
	if f.notifier.calls != 2 {
		t.Fatalf("notify calls after window = %d, want 2 (re-fired)", f.notifier.calls)
	}
	if f.events.Len() != 2 {
		t.Fatalf("events after window = %d, want 2", f.events.Len())
	}
}

// TestEvaluatorDedupFailsOpen: a deduper error must not suppress the
// alert (delivery outranks storm suppression); the local window guard
// still caps the rate.
func TestEvaluatorDedupFailsOpen(t *testing.T) {
	f := newEvalFixture([]Rule{thresholdRule("pool", "adc_pg_pool_connections", 0.8, 0)},
		[]Series{{Name: "adc_pg_pool_connections", Labels: map[string]string{"state": "acquired"}, Value: 9}})
	f.eval.Dedup = &errDeduper{}
	f.tick()
	if f.notifier.calls != 1 {
		t.Fatalf("notify calls = %d, want 1 (fail-open)", f.notifier.calls)
	}
	f.now = f.now.Add(30 * time.Second)
	f.tick() // still inside the local 5m window: suppressed
	if f.notifier.calls != 1 {
		t.Fatalf("notify calls = %d, want 1 (local window cap)", f.notifier.calls)
	}
}

type errDeduper struct{}

func (*errDeduper) Claim(context.Context, string, time.Duration) (bool, error) {
	return false, context.DeadlineExceeded
}

// TestEvaluatorDisabledRulesAndUnknownMetrics: disabled rules and rules
// whose metric is not exported never fire.
func TestEvaluatorDisabledRulesAndUnknownMetrics(t *testing.T) {
	disabled := thresholdRule("off", "adc_pg_pool_connections", 0.8, 0)
	disabled.Enabled = false
	f := newEvalFixture([]Rule{disabled, thresholdRule("ghost", "adc_never_exported_total", 1, 0)},
		[]Series{{Name: "adc_pg_pool_connections", Labels: map[string]string{"state": "acquired"}, Value: 9}})
	f.tick()
	if f.notifier.calls != 0 || f.events.Len() != 0 {
		t.Fatalf("calls = %d, events = %d; want 0/0", f.notifier.calls, f.events.Len())
	}
}

// TestEvaluatorTenantFiltering: tenant-labeled families only aggregate the
// rule tenant's series.
func TestEvaluatorTenantFiltering(t *testing.T) {
	f := newEvalFixture([]Rule{thresholdRule("online drop", "adc_device_online_total", 2, 0)},
		[]Series{
			{Name: "adc_device_online_total", Labels: map[string]string{"tenant": testTenantA}, Value: 1},
			{Name: "adc_device_online_total", Labels: map[string]string{"tenant": testTenantB}, Value: 50},
		})
	f.tick() // tenant A: 1 > 2? no — threshold NOT crossed
	if f.notifier.calls != 0 {
		t.Fatalf("notify calls = %d, want 0 (tenant A value 1 does not cross)", f.notifier.calls)
	}
}

// TestEvaluatorLabelFilters: filters narrow the matching series before the
// comparison.
func TestEvaluatorLabelFilters(t *testing.T) {
	rule := thresholdRule("wecom push", "adc_hitl_notify_failures_total", 10, 0)
	rule.Labels = map[string]string{"channel": "wecom"}
	f := newEvalFixture([]Rule{rule},
		[]Series{
			{Name: "adc_hitl_notify_failures_total", Labels: map[string]string{"channel": "wecom"}, Value: 12},
			{Name: "adc_hitl_notify_failures_total", Labels: map[string]string{"channel": "dingtalk"}, Value: 0},
		})
	f.tick()
	if f.notifier.calls != 1 {
		t.Fatalf("notify calls = %d, want 1 (wecom 12 > 10)", f.notifier.calls)
	}
}

// TestEvaluatorDeletedRuleStatePruned: removing a rule drops its evaluator
// state so re-adding it starts a fresh hold.
func TestEvaluatorDeletedRuleStatePruned(t *testing.T) {
	rule := thresholdRule("pool", "adc_pg_pool_connections", 0.8, 60)
	f := newEvalFixture([]Rule{rule},
		[]Series{{Name: "adc_pg_pool_connections", Labels: map[string]string{"state": "acquired"}, Value: 9}})
	f.tick() // hold starts
	_ = f.store.Replace(context.Background(), testTenantA, nil)
	f.tick() // rule gone: state pruned, nothing fires
	_ = f.store.Replace(context.Background(), testTenantA, []Rule{rule})
	f.now = f.now.Add(30 * time.Second)
	f.tick() // fresh hold, not yet due
	if f.notifier.calls != 0 {
		t.Fatalf("notify calls = %d, want 0 (fresh hold after re-add)", f.notifier.calls)
	}
}

// TestEvaluatorNoDataSkips: no matching series means no comparison and no
// stale firing.
func TestEvaluatorNoDataSkips(t *testing.T) {
	f := newEvalFixture([]Rule{thresholdRule("pool", "adc_pg_pool_connections", 0.8, 0)}, nil)
	f.tick()
	if f.notifier.calls != 0 || f.events.Len() != 0 {
		t.Fatalf("calls = %d, events = %d; want 0/0", f.notifier.calls, f.events.Len())
	}
}

// TestEvaluatorStoreFailureSkipsCycle: a failing store aborts the sweep
// without firing.
func TestEvaluatorStoreFailureSkipsCycle(t *testing.T) {
	f := newEvalFixture([]Rule{thresholdRule("pool", "adc_pg_pool_connections", 0.8, 0)},
		[]Series{{Name: "adc_pg_pool_connections", Labels: map[string]string{"state": "acquired"}, Value: 9}})
	f.eval.Store = &errStore{}
	f.tick()
	if f.notifier.calls != 0 {
		t.Fatalf("notify calls = %d, want 0", f.notifier.calls)
	}
}

type errStore struct{}

func (*errStore) List(context.Context, string) ([]Rule, error) {
	return nil, context.DeadlineExceeded
}
func (*errStore) Replace(context.Context, string, []Rule) error {
	return context.DeadlineExceeded
}
func (*errStore) All(context.Context) ([]Rule, error) {
	return nil, context.DeadlineExceeded
}

// TestEvaluatorRunTicksFirst: Run waits one interval before the first
// sweep (callers start the loop before their HTTP server listens), then
// keeps sweeping until the context is cancelled.
func TestEvaluatorRunTicksFirst(t *testing.T) {
	f := newEvalFixture([]Rule{thresholdRule("pool", "adc_pg_pool_connections", 0.8, 0)},
		[]Series{{Name: "adc_pg_pool_connections", Labels: map[string]string{"state": "acquired"}, Value: 9}})
	f.eval.Interval = 5 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	f.eval.Run(ctx)
	if f.notifier.calls != 1 {
		t.Fatalf("notify calls = %d, want 1 (sweeps run, dedup aggregates)", f.notifier.calls)
	}
}

// TestEvaluatorConcurrentTicks: EvaluateOnce must be safe under concurrent
// ticks (the Run loop plus a manual refresh).
func TestEvaluatorConcurrentTicks(t *testing.T) {
	f := newEvalFixture([]Rule{thresholdRule("pool", "adc_pg_pool_connections", 0.8, 0)},
		[]Series{{Name: "adc_pg_pool_connections", Labels: map[string]string{"state": "acquired"}, Value: 9}})
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				f.eval.EvaluateOnce(context.Background())
			}
		}()
	}
	wg.Wait()
	// Exactly one alert: the dedup window aggregates everything.
	if f.notifier.calls != 1 || f.events.Len() != 1 {
		t.Fatalf("calls = %d, events = %d; want 1/1", f.notifier.calls, f.events.Len())
	}
}
