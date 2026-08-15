package clusterbus

import (
	"context"
	"sync"
	"time"
)

// MemBackend is an in-memory Backend for tests and degraded
// single-process deployments. It mirrors Pub/Sub best-effort semantics:
// messages published with no subscriber are dropped, and a lagging
// subscriber loses messages instead of blocking the publisher.
//
// Subscription registration is synchronous, so callers can publish
// immediately after Subscribe returns without goroutine-scheduling
// races; WaitSubscription exists for the one case where the subscription
// is registered from another goroutine (RegisterHandler starts the bus
// loops lazily).
type MemBackend struct {
	mu   sync.Mutex
	subs map[string][]chan []byte
}

// NewMemBackend returns an empty in-memory backend.
func NewMemBackend() *MemBackend {
	return &MemBackend{subs: make(map[string][]chan []byte)}
}

// Publish delivers a copy of payload to every subscription of channel.
func (m *MemBackend) Publish(ctx context.Context, channel string, payload []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	chans := append([]chan []byte(nil), m.subs[channel]...)
	m.mu.Unlock()
	for _, ch := range chans {
		select {
		case ch <- append([]byte(nil), payload...):
		default: // best-effort drop, mirrors Pub/Sub
		}
	}
	return nil
}

// Subscribe registers fn for channel and delivers published payloads from
// a per-subscription goroutine until ctx is done. It returns immediately:
// registration is synchronous, delivery is asynchronous.
func (m *MemBackend) Subscribe(ctx context.Context, channel string, fn func(payload []byte)) error {
	ch := make(chan []byte, 64)
	m.mu.Lock()
	m.subs[channel] = append(m.subs[channel], ch)
	m.mu.Unlock()
	go func() {
		defer func() {
			m.mu.Lock()
			subs := m.subs[channel]
			for i, c := range subs {
				if c == ch {
					m.subs[channel] = append(subs[:i], subs[i+1:]...)
					break
				}
			}
			m.mu.Unlock()
		}()
		for {
			select {
			case <-ctx.Done():
				return
			case payload := <-ch:
				fn(payload)
			}
		}
	}()
	return nil
}

// WaitSubscription blocks until channel has at least n subscribers or the
// deadline elapses (test helper for lazily started bus loops).
func (m *MemBackend) WaitSubscription(channel string, n int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		m.mu.Lock()
		got := len(m.subs[channel])
		m.mu.Unlock()
		if got >= n {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return false
}
