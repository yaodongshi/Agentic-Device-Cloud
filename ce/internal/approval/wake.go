// Cross-node wake channel (SEC-10, design/31 3.3.6). The production adapter
// publishes on the unified Valkey channel adc:hitl:resolve so an approval
// decided on node B wakes the suspended call waiting on node A (the PoC's
// per-ticket channels had no subscribers and silently lost events).
// InMemoryEventBus is the in-process fallback for single-instance and
// degraded modes where the message layer is unavailable (HLD: local fast
// path keeps working; wake degrades to the timeout fallback).

package approval

import (
	"context"
	"sync"
)

// TicketEventBus is the wake seam: PublishResolved announces a resolved
// ticket, SubscribeResolved registers a long-running consumer.
type TicketEventBus interface {
	PublishResolved(ctx context.Context, ticketID string, status Status) error
	SubscribeResolved(ctx context.Context, fn func(ticketID string, status Status)) error
}

// InMemoryEventBus fans published events out to every subscriber registered
// so far. Publish snapshots the subscriber list under a read lock so a slow
// or re-entrant subscriber cannot block publishers or deadlock.
type InMemoryEventBus struct {
	mu   sync.RWMutex
	subs []func(ticketID string, status Status)
}

// NewInMemoryEventBus returns an empty in-process bus.
func NewInMemoryEventBus() *InMemoryEventBus {
	return &InMemoryEventBus{}
}

// PublishResolved delivers the event to all registered subscribers, on the
// calling goroutine. It returns ctx.Err() when the context is already done.
func (b *InMemoryEventBus) PublishResolved(ctx context.Context, ticketID string, status Status) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	b.mu.RLock()
	subs := make([]func(string, Status), len(b.subs))
	copy(subs, b.subs)
	b.mu.RUnlock()
	for _, fn := range subs {
		fn(ticketID, status)
	}
	return nil
}

// SubscribeResolved registers fn and blocks until ctx is done. The
// subscription lives for the duration of the context; it returns ctx.Err()
// on cancellation, matching the long-running subscription loop of the Valkey
// adapter.
func (b *InMemoryEventBus) SubscribeResolved(ctx context.Context, fn func(ticketID string, status Status)) error {
	b.mu.Lock()
	b.subs = append(b.subs, fn)
	b.mu.Unlock()
	<-ctx.Done()
	return ctx.Err()
}
