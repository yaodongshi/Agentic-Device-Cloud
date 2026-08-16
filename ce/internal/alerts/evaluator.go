package alerts

// Evaluator is the FR-017 threshold evaluation loop (design/82 B2): every
// Interval (30s) it sweeps all enabled rules, reads the metric snapshot
// from the MetricReader and fires alerts whose condition held continuously
// for the rule's duration_sec. Firing means: dedup claim (5 minute
// aggregation window, FR-017), notification and event history record.
//
// Acceptance criteria mapping:
//   - "阈值告警 1 分钟内发出": interval 30s + duration 0 fires on the
//     first evaluation after the threshold is crossed (≤30s).
//   - "连续 3 次失败摘除" analog: a rule with duration_sec=90 evaluated
//     every 30s only fires after three consecutive failing evaluations;
//     load-balancer removal itself is out of this slice.
//   - "告警风暴需聚合去重": the DedupWindow suppresses re-alerts of the
//     same rule for 5 minutes (Deduper + local last-fired guard).

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// DefaultInterval is the evaluation cadence: 30s keeps the FR-017
// "1 minute" delivery promise with margin for the scrape and the notify
// round-trip.
const DefaultInterval = 30 * time.Second

// Evaluator drives one alert evaluation loop.
type Evaluator struct {
	Store       RuleStore        // rule source (All)
	Reader      MetricReader     // series snapshot source
	Notify      AlertNotifier    // nil: events recorded but not pushed
	Dedup       Deduper          // nil: no cross-process dedup
	Events      *EventBuffer     // nil: no history
	Interval    time.Duration    // 0: DefaultInterval
	DedupWindow time.Duration    // 0: DefaultDedupWindow
	Now         func() time.Time // clock seam (tests)
	Log         *slog.Logger     // nil: slog.Default

	mu     sync.Mutex
	states map[string]*ruleState // ruleID -> condition state
}

// ruleState tracks one rule's condition hold time and last fire.
type ruleState struct {
	since     time.Time // first evaluation the condition held (zero = not holding)
	lastFired time.Time // last time an alert fired for this rule
}

// Run blocks until ctx is done, evaluating on the interval. The first
// evaluation happens after one interval so callers can start the loop
// before their HTTP server is listening (the self-scrape reader targets
// the process's own /metrics endpoint).
func (e *Evaluator) Run(ctx context.Context) {
	t := time.NewTicker(e.interval())
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			e.EvaluateOnce(ctx)
		}
	}
}

// EvaluateOnce performs one sweep. It never panics: a failing store,
// reader or notifier is logged and the next sweep retries.
func (e *Evaluator) EvaluateOnce(ctx context.Context) {
	log := e.log()
	now := e.now()
	rules, err := e.Store.All(ctx)
	if err != nil {
		log.Warn("alerts: rule sweep failed", "err", err)
		return
	}
	series, err := e.Reader.Read(ctx)
	if err != nil {
		log.Warn("alerts: metric read failed", "err", err)
		return
	}
	idx := seriesByName(series)

	e.mu.Lock()
	if e.states == nil {
		e.states = map[string]*ruleState{}
	}
	// Drop state of deleted rules.
	live := make(map[string]bool, len(rules))
	for _, r := range rules {
		live[r.ID] = true
	}
	for id := range e.states {
		if !live[id] {
			delete(e.states, id)
		}
	}
	e.mu.Unlock()

	for _, r := range rules {
		if !r.Enabled {
			continue
		}
		family, ok := idx[r.Metric]
		if !ok {
			continue // metric family not exported (yet): nothing to compare
		}
		value, matched := sumMatching(family, r.TenantID, r.Labels)
		if !matched {
			continue // filters exclude every series: no data for this rule
		}
		if !r.Matches(value) {
			e.mu.Lock()
			delete(e.states, r.ID)
			e.mu.Unlock()
			continue
		}
		// Condition holds: track the hold start, then fire once the
		// duration and the dedup window both pass.
		fired := e.trackHold(now, r, value, ctx, log)
		if fired {
			log.Info("alerts: fired", "rule", r.Name, "severity", r.Severity,
				"metric", r.Metric, "observed", value, "threshold", r.Threshold, "tenant", r.TenantID)
		}
	}
}

// trackHold updates the hold state for one firing rule and emits the event
// when due. Returns true when an alert fired.
func (e *Evaluator) trackHold(now time.Time, r Rule, value float64, ctx context.Context, log *slog.Logger) bool {
	window := e.DedupWindow
	if window <= 0 {
		window = DefaultDedupWindow
	}
	e.mu.Lock()
	st := e.states[r.ID]
	if st == nil {
		st = &ruleState{}
		e.states[r.ID] = st
	}
	if st.since.IsZero() {
		st.since = now
	}
	due := now.Sub(st.since) >= time.Duration(r.DurationSec)*time.Second
	windowPassed := st.lastFired.IsZero() || now.Sub(st.lastFired) >= window
	if !due || !windowPassed {
		e.mu.Unlock()
		return false
	}
	ev := Event{
		ID:        NewRuleID(),
		RuleID:    r.ID,
		RuleName:  r.Name,
		TenantID:  r.TenantID,
		Metric:    r.Metric,
		Operator:  r.Operator,
		Threshold: r.Threshold,
		Observed:  value,
		Severity:  r.Severity,
		FiredAt:   now.UTC(),
	}
	e.mu.Unlock()

	if e.Dedup != nil {
		ok, err := e.Dedup.Claim(ctx, DedupKey(r.ID), window)
		if err != nil {
			// Fail-open: alert delivery outranks storm suppression; the
			// local windowPassed guard still caps the process-local rate.
			log.Warn("alerts: dedup claim failed, firing anyway", "rule", r.Name, "err", err)
		} else if !ok {
			e.mu.Lock()
			st.lastFired = now // keep the local window in sync with the store
			e.mu.Unlock()
			return false
		}
	}
	if e.Notify != nil {
		if err := e.Notify.SendAlert(ctx, &ev); err != nil {
			log.Warn("alerts: notify failed", "rule", r.Name, "err", err)
		}
	}
	if e.Events != nil {
		e.Events.Record(ev)
	}
	e.mu.Lock()
	st.lastFired = now
	e.mu.Unlock()
	return true
}

func (e *Evaluator) interval() time.Duration {
	if e.Interval > 0 {
		return e.Interval
	}
	return DefaultInterval
}

func (e *Evaluator) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func (e *Evaluator) log() *slog.Logger {
	if e.Log != nil {
		return e.Log
	}
	return slog.Default()
}
