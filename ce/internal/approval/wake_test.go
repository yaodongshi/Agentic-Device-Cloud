package approval

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// waitForSubscribers blocks until the bus has registered n subscribers.
func waitForSubscribers(t *testing.T, bus *InMemoryEventBus, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		bus.mu.RLock()
		count := len(bus.subs)
		bus.mu.RUnlock()
		if count >= n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("bus has %d subscribers, want %d", func() int {
		bus.mu.RLock()
		defer bus.mu.RUnlock()
		return len(bus.subs)
	}(), n)
}

func TestInMemoryEventBusFanout(t *testing.T) {
	bus := NewInMemoryEventBus()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	type event struct {
		id     string
		status Status
	}
	got := make(chan event, 2)
	for i := 0; i < 2; i++ {
		go func() {
			_ = bus.SubscribeResolved(ctx, func(id string, status Status) {
				got <- event{id: id, status: status}
			})
		}()
	}
	waitForSubscribers(t, bus, 2)

	if err := bus.PublishResolved(ctx, "ticket-1", StatusApproved); err != nil {
		t.Fatalf("PublishResolved: %v", err)
	}
	for i := 0; i < 2; i++ {
		select {
		case e := <-got:
			if e.id != "ticket-1" || e.status != StatusApproved {
				t.Errorf("event = %+v, want ticket-1/APPROVED", e)
			}
		case <-time.After(time.Second):
			t.Fatalf("subscriber %d did not receive the event", i)
		}
	}
}

func TestInMemoryEventBusCancelledPublish(t *testing.T) {
	bus := NewInMemoryEventBus()
	ctx, cancel := context.WithCancel(context.Background())
	delivered := make(chan string, 1)
	go func() {
		_ = bus.SubscribeResolved(ctx, func(id string, _ Status) { delivered <- id })
	}()
	waitForSubscribers(t, bus, 1)

	cancel()
	if err := bus.PublishResolved(ctx, "ticket-1", StatusApproved); !errors.Is(err, context.Canceled) {
		t.Errorf("publish error = %v, want context.Canceled", err)
	}
	select {
	case id := <-delivered:
		t.Errorf("event delivered after cancellation: %s", id)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestInMemoryEventBusSubscriptionEndsOnCancel(t *testing.T) {
	bus := NewInMemoryEventBus()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- bus.SubscribeResolved(ctx, func(string, Status) {})
	}()
	waitForSubscribers(t, bus, 1)

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("SubscribeResolved returned %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("SubscribeResolved did not return after cancellation")
	}
}

func TestInMemoryEventBusConcurrentPublish(t *testing.T) {
	// Publishes from multiple goroutines must be race-free and lossless
	// (run with -race; SEC-10 wake reliability).
	bus := NewInMemoryEventBus()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const total = 200
	received := make(chan string, total)
	go func() {
		_ = bus.SubscribeResolved(ctx, func(id string, _ Status) { received <- id })
	}()
	waitForSubscribers(t, bus, 1)

	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < total/4; i++ {
				if err := bus.PublishResolved(ctx, "ticket", StatusApproved); err != nil {
					t.Errorf("publish: %v", err)
					return
				}
			}
		}(g)
	}
	wg.Wait()

	for i := 0; i < total; i++ {
		select {
		case <-received:
		case <-time.After(time.Second):
			t.Fatalf("received %d/%d events", i, total)
		}
	}
}
