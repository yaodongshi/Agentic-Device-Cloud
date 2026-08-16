package alerts

import (
	"sync"
	"time"
)

// DefaultEventCapacity is the event history cap (design/82 B2: the Admin
// API serves the most recent 100 events).
const DefaultEventCapacity = 100

// Event is one fired alert recorded in the history. Suppressed (deduped)
// triggers never reach the history: FR-017 aggregation means the history
// holds distinct alerts, not every evaluation tick.
type Event struct {
	ID        string    `json:"id"`
	RuleID    string    `json:"rule_id"`
	RuleName  string    `json:"rule_name"`
	TenantID  string    `json:"tenant_id"`
	Metric    string    `json:"metric"`
	Operator  Operator  `json:"operator"`
	Threshold float64   `json:"threshold"`
	Observed  float64   `json:"observed"`
	Severity  Severity  `json:"severity"`
	FiredAt   time.Time `json:"fired_at"`
}

// EventReader serves the alert history (Admin API seam). List returns the
// newest events of one tenant first; limit is clamped to [1, capacity].
type EventReader interface {
	List(tenantID string, limit int) []Event
}

// EventBuffer is a bounded, newest-first in-memory event history (FR-017:
// "最近 100 条"). Persistence to Valkey is deliberately out of the B2
// slice: the buffer is per-process and alert events are operational
// ephemera; the audit log remains the durable trail of system state
// changes.
type EventBuffer struct {
	mu     sync.Mutex
	cap    int
	events []Event // index 0 = newest
}

// NewEventBuffer builds a buffer holding at most capacity events
// (DefaultEventCapacity when capacity <= 0).
func NewEventBuffer(capacity int) *EventBuffer {
	if capacity <= 0 {
		capacity = DefaultEventCapacity
	}
	return &EventBuffer{cap: capacity}
}

// Record prepends one event and trims the tail past the capacity.
func (b *EventBuffer) Record(ev Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.events = append([]Event{ev}, b.events...)
	if len(b.events) > b.cap {
		b.events = b.events[:b.cap]
	}
}

// List returns the newest events for the tenant, at most limit items.
// An empty tenantID matches all tenants (evaluator diagnostics).
func (b *EventBuffer) List(tenantID string, limit int) []Event {
	b.mu.Lock()
	defer b.mu.Unlock()
	if limit <= 0 || limit > b.cap {
		limit = b.cap
	}
	out := make([]Event, 0, min(limit, len(b.events)))
	for _, ev := range b.events {
		if tenantID != "" && ev.TenantID != tenantID {
			continue
		}
		out = append(out, ev)
		if len(out) == limit {
			break
		}
	}
	return out
}

// Len reports the total number of stored events (all tenants).
func (b *EventBuffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.events)
}

// Capacity reports the configured cap.
func (b *EventBuffer) Capacity() int { return b.cap }
