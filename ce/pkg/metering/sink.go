package metering

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// Valkey list keys for the metering pipeline. The queue carries events
// from producers to the worker; the processing list holds events in
// flight (BRPOPLPUSH crash guard, like the audit pipeline SEC-07): an
// item survives a worker crash until the PG insert commits and the
// payload is LREM'd.
const (
	QueueKey      = "adc:usage:queue"
	ProcessingKey = "adc:usage:processing"
)

// Queue abstracts the Valkey list transport so the meter and the worker
// can be tested against an in-memory fake. redisQueue adapts go-redis/v9.
type Queue interface {
	// RPush appends payload to the tail of the list at key.
	RPush(ctx context.Context, key string, payload []byte) error
	// BRPopLPush atomically pops the tail element of src and prepends it
	// to dst. A timeout > 0 blocks up to timeout and returns (nil, nil)
	// when no element arrives; timeout <= 0 polls once.
	BRPopLPush(ctx context.Context, src, dst string, timeout time.Duration) ([]byte, error)
	// LRem removes the first count occurrences of payload from key.
	LRem(ctx context.Context, key string, count int64, payload []byte) error
}

// ValkeyMeter implements Meter by RPUSHing JSON-encoded events onto
// QueueKey. Safe for concurrent use.
type ValkeyMeter struct {
	q Queue
}

// NewValkeyMeter builds the production meter backed by go-redis/v9.
func NewValkeyMeter(rdb *redis.Client) *ValkeyMeter {
	return NewMeter(redisQueue{rdb: rdb})
}

// NewMeter builds a meter over an arbitrary Queue (tests, or a future
// transport such as NATS JetStream, ADR-02).
func NewMeter(q Queue) *ValkeyMeter {
	return &ValkeyMeter{q: q}
}

// Record validates, JSON-encodes and RPUSHes the events. A non-nil error
// means none of the events were queued; producers treat recording as
// best-effort (metering must never block or fail a tool call).
func (m *ValkeyMeter) Record(ctx context.Context, evs ...*UsageEvent) error {
	if m == nil || m.q == nil {
		return errors.New("metering: nil meter queue")
	}
	for _, ev := range evs {
		if err := ev.Validate(); err != nil {
			return err
		}
	}
	for _, ev := range evs {
		payload, err := json.Marshal(ev)
		if err != nil {
			return fmt.Errorf("metering: encode event: %w", err)
		}
		if err := m.q.RPush(ctx, QueueKey, payload); err != nil {
			return fmt.Errorf("metering: rpush %s: %w", QueueKey, err)
		}
	}
	return nil
}

// redisQueue adapts a go-redis/v9 client to the Queue interface.
type redisQueue struct {
	rdb *redis.Client
}

func (q redisQueue) RPush(ctx context.Context, key string, payload []byte) error {
	return q.rdb.RPush(ctx, key, string(payload)).Err()
}

func (q redisQueue) BRPopLPush(ctx context.Context, src, dst string, timeout time.Duration) ([]byte, error) {
	var v string
	var err error
	if timeout <= 0 {
		v, err = q.rdb.RPopLPush(ctx, src, dst).Result() // non-blocking poll
	} else {
		v, err = q.rdb.BRPopLPush(ctx, src, dst, timeout).Result() // blocking wait
	}
	if errors.Is(err, redis.Nil) {
		return nil, nil // timeout or empty list: no element
	}
	if err != nil {
		return nil, err
	}
	return []byte(v), nil
}

func (q redisQueue) LRem(ctx context.Context, key string, count int64, payload []byte) error {
	return q.rdb.LRem(ctx, key, count, string(payload)).Err()
}
