package clusterbus

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

var testNodeKey = []byte("test-node-signing-key")

func newTestBus(nodeID string, backend Backend) *ValkeyBus {
	return NewValkeyBus(BusConfig{
		NodeID:  nodeID,
		NodeKey: testNodeKey,
		Backend: backend,
		MaxSkew: time.Minute,
	})
}

func startHandler(t *testing.T, bus *ValkeyBus, backend *MemBackend, topic string, fn HandlerFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = bus.RegisterHandler(ctx, topic, fn) }()
	if !backend.WaitSubscription(RequestChannel(bus.cfg.NodeID), 1, 2*time.Second) {
		t.Fatal("bus never subscribed its request channel")
	}
}

func TestRequestResponseCorrelation(t *testing.T) {
	backend := NewMemBackend()
	busA := newTestBus("node-a", backend)
	busB := newTestBus("node-b", backend)
	defer busA.Close()
	defer busB.Close()

	startHandler(t, busB, backend, "topic.echo", func(ctx context.Context, payload []byte) ([]byte, error) {
		return append([]byte("echo:"), payload...), nil
	})

	resp, err := busA.Request(context.Background(), "node-b", "topic.echo", []byte("ping"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if string(resp) != "echo:ping" {
		t.Fatalf("got %q, want %q", resp, "echo:ping")
	}
}

func TestRequestHandlerErrorPropagates(t *testing.T) {
	backend := NewMemBackend()
	busA := newTestBus("node-a", backend)
	busB := newTestBus("node-b", backend)
	defer busA.Close()
	defer busB.Close()

	startHandler(t, busB, backend, "topic.err", func(ctx context.Context, payload []byte) ([]byte, error) {
		return nil, errors.New("boom")
	})

	_, err := busA.Request(context.Background(), "node-b", "topic.err", nil, time.Second)
	if err == nil || err.Error() != "boom" {
		t.Fatalf("want handler error %q, got %v", "boom", err)
	}
}

func TestRequestTimeoutWhenNoResponder(t *testing.T) {
	backend := NewMemBackend()
	busA := newTestBus("node-a", backend)
	defer busA.Close()

	start := time.Now()
	_, err := busA.Request(context.Background(), "node-ghost", "topic.x", []byte("ping"), 100*time.Millisecond)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("want ErrTimeout, got %v", err)
	}
	if elapsed := time.Since(start); elapsed < 90*time.Millisecond || elapsed > time.Second {
		t.Fatalf("timeout fired too early or too late: %v", elapsed)
	}
}

func TestRequestFailsFastWithoutHandler(t *testing.T) {
	backend := NewMemBackend()
	busA := newTestBus("node-a", backend)
	busB := newTestBus("node-b", backend)
	defer busA.Close()
	defer busB.Close()

	startHandler(t, busB, backend, "topic.unused", func(ctx context.Context, payload []byte) ([]byte, error) {
		return nil, nil
	})

	_, err := busA.Request(context.Background(), "node-b", "topic.missing", nil, 200*time.Millisecond)
	if err == nil {
		t.Fatal("want error for unhandled topic")
	}
	if errors.Is(err, ErrTimeout) {
		t.Fatalf("unhandled topic should fail fast, not time out: %v", err)
	}
}

func TestRequestRejectsForgedSignature(t *testing.T) {
	backend := NewMemBackend()
	busB := newTestBus("node-b", backend)
	defer busB.Close()

	calls := make(chan struct{}, 4)
	startHandler(t, busB, backend, "topic.secret", func(ctx context.Context, payload []byte) ([]byte, error) {
		calls <- struct{}{}
		return nil, nil
	})

	forged := &message{
		RequestID: newRequestID(),
		FromNode:  "node-a",
		Topic:     "topic.secret",
		TS:        time.Now().Unix(),
		Payload:   encodePayload([]byte("hijack")),
	}
	forged.Sig = SignMessage([]byte("attacker-key"), forged) // wrong key
	body, err := json.Marshal(forged)
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.Publish(context.Background(), RequestChannel("node-b"), body); err != nil {
		t.Fatal(err)
	}

	select {
	case <-calls:
		t.Fatal("handler executed a request with a forged signature (SEC-06)")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestRequestRejectsStaleTimestamp(t *testing.T) {
	backend := NewMemBackend()
	busB := newTestBus("node-b", backend)
	defer busB.Close()

	calls := make(chan struct{}, 4)
	startHandler(t, busB, backend, "topic.secret", func(ctx context.Context, payload []byte) ([]byte, error) {
		calls <- struct{}{}
		return nil, nil
	})

	stale := &message{
		RequestID: newRequestID(),
		FromNode:  "node-a",
		Topic:     "topic.secret",
		TS:        time.Now().Add(-10 * time.Minute).Unix(), // outside MaxSkew
		Payload:   encodePayload([]byte("replay")),
	}
	stale.Sig = SignMessage(testNodeKey, stale) // correctly signed, but old
	body, err := json.Marshal(stale)
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.Publish(context.Background(), RequestChannel("node-b"), body); err != nil {
		t.Fatal(err)
	}

	select {
	case <-calls:
		t.Fatal("handler executed a replayed request outside the freshness window")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestPublishReachesMultipleSubscribers(t *testing.T) {
	backend := NewMemBackend()
	bus := newTestBus("node-a", backend)
	defer bus.Close()

	got := make(chan string, 8)
	sub := func(id string) func([]byte) {
		return func(payload []byte) { got <- id + ":" + string(payload) }
	}
	ctx1, cancel1 := context.WithCancel(context.Background())
	defer cancel1()
	go func() { _ = bus.Subscribe(ctx1, "topic.broadcast", sub("s1")) }()
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	go func() { _ = bus.Subscribe(ctx2, "topic.broadcast", sub("s2")) }()
	if !backend.WaitSubscription("topic.broadcast", 1, 2*time.Second) {
		t.Fatal("topic never gained its shared backend subscription")
	}

	if err := bus.Publish(context.Background(), "topic.broadcast", []byte("hi")); err != nil {
		t.Fatal(err)
	}

	seen := map[string]bool{}
	for i := 0; i < 2; i++ {
		select {
		case g := <-got:
			seen[g] = true
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for subscriber deliveries, saw %v", seen)
		}
	}
	if !seen["s1:hi"] || !seen["s2:hi"] {
		t.Fatalf("both subscribers must receive the payload, got %v", seen)
	}
}

func TestRequestContextCancellation(t *testing.T) {
	backend := NewMemBackend()
	busA := newTestBus("node-a", backend)
	defer busA.Close()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	_, err := busA.Request(ctx, "node-ghost", "topic.x", []byte("ping"), time.Second)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}

func TestSignVerifyRoundTrip(t *testing.T) {
	env := &message{
		RequestID: newRequestID(),
		FromNode:  "node-a",
		Topic:     "t",
		TS:        time.Now().Unix(),
		Payload:   encodePayload([]byte(`{"a":1}`)),
	}
	env.Sig = SignMessage(testNodeKey, env)
	if err := VerifyMessage(testNodeKey, env, time.Now(), time.Minute); err != nil {
		t.Fatalf("valid signature rejected: %v", err)
	}
	env.Payload = encodePayload([]byte(`{"a":2}`)) // tamper after signing
	if err := VerifyMessage(testNodeKey, env, time.Now(), time.Minute); !errors.Is(err, ErrSignature) {
		t.Fatalf("tampered payload must fail signature check, got %v", err)
	}
}

func TestNodeRegistryLocate(t *testing.T) {
	r := NewNodeRegistryGetter(mapGetter{
		LocKey("t1", "dev-1"): "node-b",
	})
	node, err := r.Locate(context.Background(), "t1", "dev-1")
	if err != nil || node != "node-b" {
		t.Fatalf("got (%q, %v), want (node-b, nil)", node, err)
	}
	_, err = r.Locate(context.Background(), "t1", "dev-missing")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

type mapGetter map[string]string

func (m mapGetter) Get(ctx context.Context, key string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	v, ok := m[key]
	if !ok {
		return "", errNotFound{}
	}
	return v, nil
}

// errNotFound mimics redis.Nil for the registry lookup.
type errNotFound struct{}

func (errNotFound) Error() string { return "redis: nil" }

// Is maps the fake onto redis.Nil so NodeRegistry classifies it as
// ErrNotFound.
func (errNotFound) Is(target error) bool { return target == redis.Nil }
