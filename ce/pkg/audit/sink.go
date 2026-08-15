package audit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// Valkey list keys for the audit pipeline. The queue carries events from
// producers to the worker; the processing list holds events in flight
// (BRPOPLPUSH crash guard, SEC-07: an item survives a worker crash until
// the PG insert commits and the payload is LREM'd).
const (
	QueueKey      = "adc:audit:queue"
	ProcessingKey = "adc:audit:processing"
)

// Queue abstracts the Valkey list transport so the sink and the worker
// can be tested against an in-memory fake. redisQueue adapts go-redis/v9.
type Queue interface {
	// RPush appends payload to the tail of the list at key.
	RPush(ctx context.Context, key string, payload []byte) error
	// BRPopLPush atomically pops the tail element of src and prepends it
	// to dst. A timeout > 0 blocks up to timeout and returns (nil, nil)
	// when no element arrives; timeout <= 0 polls once and returns
	// immediately.
	BRPopLPush(ctx context.Context, src, dst string, timeout time.Duration) ([]byte, error)
	// LRem removes the first count occurrences of payload from key.
	LRem(ctx context.Context, key string, count int64, payload []byte) error
}

// AuditSink enqueues audit events onto the Valkey queue (design/31 I10).
// Enqueue stays off the hot path: producers emit before the business
// commit and never wait for the PG insert (ADR-06, T6).
type AuditSink interface {
	Enqueue(ctx context.Context, ev *AuditEvent) error
}

// ValkeySink implements AuditSink by RPUSHing the JSON-encoded event onto
// QueueKey. Safe for concurrent use.
type ValkeySink struct {
	q Queue
}

// NewValkeySink builds the production sink backed by go-redis/v9.
func NewValkeySink(rdb *redis.Client) *ValkeySink {
	return &ValkeySink{q: redisQueue{rdb: rdb}}
}

// NewSink builds a sink over an arbitrary Queue (tests, or a future
// transport such as NATS JetStream, ADR-02).
func NewSink(q Queue) *ValkeySink {
	return &ValkeySink{q: q}
}

// Enqueue validates, JSON-encodes and RPUSHes the event. A non-nil error
// means the event was not queued; callers treat enqueueing as
// best-effort and must not block shutdown on it (design/31 1.3.7).
func (s *ValkeySink) Enqueue(ctx context.Context, ev *AuditEvent) error {
	if s == nil || s.q == nil {
		return errors.New("audit: nil sink queue")
	}
	if err := ev.Validate(); err != nil {
		return err
	}
	payload, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("audit: encode event: %w", err)
	}
	if err := s.q.RPush(ctx, QueueKey, payload); err != nil {
		return fmt.Errorf("audit: rpush %s: %w", QueueKey, err)
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
