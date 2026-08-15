// Valkey wake transport (SEC-10, design/31 3.3.6). PublishResolved emits the
// ticket decision onto the unified channel adc:hitl:resolve so an approval
// decided on node B wakes the suspended call waiting on node A; the PoC's
// per-ticket channels had no subscribers and silently lost events.
// SubscribeResolved is the long-running consumer loop fed by a single
// goroutine per process (design/31 3.3.8). InMemoryEventBus stays the
// in-process fallback for tests and degraded single-instance mode.

package approval

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/redis/go-redis/v9"
)

// WakeChannel is the unified resolve channel (design/31 1.3.7).
const WakeChannel = "adc:hitl:resolve"

// wakeEvent is the payload shape on the channel: ticket id plus the landed
// status. Waiters without a local registration drop the event (the repo poll
// and timeout fallbacks still resolve the call).
type wakeEvent struct {
	TicketID string `json:"ticket_id"`
	Status   Status `json:"status"`
}

// ValkeyEventBus implements TicketEventBus over Valkey Pub/Sub.
type ValkeyEventBus struct {
	rdb     *redis.Client
	channel string
}

// NewValkeyEventBus builds a bus publishing on WakeChannel.
func NewValkeyEventBus(rdb *redis.Client) *ValkeyEventBus {
	return &ValkeyEventBus{rdb: rdb, channel: WakeChannel}
}

// PublishResolved implements TicketEventBus. It fails fast when the message
// layer is down; the caller treats the wake as best-effort because waiters
// also poll the repository and time out (design/31 3.3.6).
func (b *ValkeyEventBus) PublishResolved(ctx context.Context, ticketID string, status Status) error {
	payload, err := json.Marshal(wakeEvent{TicketID: ticketID, Status: status})
	if err != nil {
		return fmt.Errorf("approval: marshal wake event: %w", err)
	}
	return b.rdb.Publish(ctx, b.channel, payload).Err()
}

// SubscribeResolved implements TicketEventBus: it registers fn and blocks
// until ctx is done, mirroring the InMemoryEventBus contract. Malformed
// payloads are skipped without tearing the subscription down.
func (b *ValkeyEventBus) SubscribeResolved(ctx context.Context, fn func(ticketID string, status Status)) error {
	pubsub := b.rdb.Subscribe(ctx, b.channel)
	defer pubsub.Close()
	ch := pubsub.Channel()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case msg, ok := <-ch:
			if !ok {
				return nil
			}
			var ev wakeEvent
			if err := json.Unmarshal([]byte(msg.Payload), &ev); err == nil && ev.TicketID != "" {
				fn(ev.TicketID, ev.Status)
			}
		}
	}
}
