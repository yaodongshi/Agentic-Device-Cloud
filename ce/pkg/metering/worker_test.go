package metering

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"
)

func sampleEvent(key string) *UsageEvent {
	return &UsageEvent{
		TenantID:       "tenant-1",
		Kind:           KindCall,
		Source:         SourceMCPGateway,
		MetricKey:      "dev-1",
		Value:          1,
		Unit:           UnitCount,
		OccurredAt:     time.Now().UTC(),
		IdempotencyKey: key,
		Meta:           map[string]any{"tool": "set_rpm"},
	}
}

func TestRecordEnqueuesEvents(t *testing.T) {
	q := newMemQueue()
	m := NewMeter(q)
	if err := m.Record(context.Background(), sampleEvent("k1"), sampleEvent("k2")); err != nil {
		t.Fatal(err)
	}
	if q.len(QueueKey) != 2 {
		t.Fatalf("queue depth %d, want 2", q.len(QueueKey))
	}
}

func TestRecordRejectsInvalidEvent(t *testing.T) {
	q := newMemQueue()
	m := NewMeter(q)
	ev := sampleEvent("k1")
	ev.Kind = "BOGUS"
	if err := m.Record(context.Background(), ev); err == nil {
		t.Fatal("want validation error for bogus kind")
	}
	if q.len(QueueKey) != 0 {
		t.Fatalf("invalid events must not be queued, depth %d", q.len(QueueKey))
	}
}

func TestValidate(t *testing.T) {
	base := sampleEvent("k1")
	bad := *base
	bad.TenantID = ""
	if err := bad.Validate(); err == nil {
		t.Fatal("empty tenant_id must be rejected")
	}
	bad = *base
	bad.OccurredAt = time.Time{}
	if err := bad.Validate(); err == nil {
		t.Fatal("zero occurred_at must be rejected")
	}
	bad = *base
	bad.IdempotencyKey = ""
	if err := bad.Validate(); err == nil {
		t.Fatal("empty idempotency_key must be rejected")
	}
	bad = *base
	bad.Value = -1
	if err := bad.Validate(); err == nil {
		t.Fatal("negative value must be rejected")
	}
}

// dedupInserter emulates the uq_usage_idempotency unique index contract:
// the first event per idempotency_key lands, duplicates are skipped, like
// ON CONFLICT (idempotency_key) DO NOTHING in the real inserter.
type dedupInserter struct {
	mu      sync.Mutex
	stored  map[string]*UsageEvent
	batches [][]*UsageEvent
}

func newDedupInserter() *dedupInserter {
	return &dedupInserter{stored: make(map[string]*UsageEvent)}
}

func (d *dedupInserter) InsertBatch(ctx context.Context, evs []*UsageEvent) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.batches = append(d.batches, append([]*UsageEvent(nil), evs...))
	for _, ev := range evs {
		if _, exists := d.stored[ev.IdempotencyKey]; !exists {
			d.stored[ev.IdempotencyKey] = ev
		}
	}
	return nil
}

func (d *dedupInserter) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.stored)
}

func (d *dedupInserter) has(key string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, ok := d.stored[key]
	return ok
}

// runWorkerOnce drains the queue through one processing cycle: the batch
// insert, then the acks.
func runWorkerOnce(t *testing.T, w *Worker) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = w.Run(ctx)
		close(done)
	}()
	// Wait until the queue and the processing list are both empty (the
	// worker acked everything), then stop.
	deadline := time.Now().Add(3 * time.Second)
	q := w.q.(*memQueue)
	for time.Now().Before(deadline) {
		if q.len(QueueKey) == 0 && q.len(ProcessingKey) == 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("worker did not stop after cancel")
	}
}

func TestWorkerIdempotentInsert(t *testing.T) {
	q := newMemQueue()
	ins := newDedupInserter()
	w := NewWorker(WorkerConfig{Queue: q, Inserter: ins, PollTimeout: 20 * time.Millisecond})

	// The same idempotency key is delivered three times (redelivery after
	// a crash between RPUSH and LRem, design/32 6.3): exactly one row may
	// land.
	for _, key := range []string{"k-dup", "k-dup", "k-dup", "k-other"} {
		ev := sampleEvent(key)
		payload, err := json.Marshal(ev)
		if err != nil {
			t.Fatal(err)
		}
		if err := q.RPush(context.Background(), QueueKey, payload); err != nil {
			t.Fatal(err)
		}
	}

	runWorkerOnce(t, w)

	if ins.count() != 2 {
		t.Fatalf("stored %d rows, want 2 (duplicates deduped by idempotency_key)", ins.count())
	}
	if !ins.has("k-dup") || !ins.has("k-other") {
		t.Fatalf("expected keys missing: %v", ins.stored)
	}
	if q.len(QueueKey) != 0 || q.len(ProcessingKey) != 0 {
		t.Fatalf("queue not drained: queue=%d processing=%d", q.len(QueueKey), q.len(ProcessingKey))
	}
}

func TestWorkerDropsMalformedPayload(t *testing.T) {
	q := newMemQueue()
	ins := newDedupInserter()
	w := NewWorker(WorkerConfig{Queue: q, Inserter: ins, PollTimeout: 20 * time.Millisecond})

	if err := q.RPush(context.Background(), QueueKey, []byte(`{not-json`)); err != nil {
		t.Fatal(err)
	}
	ev := sampleEvent("k-good")
	payload, _ := json.Marshal(ev)
	if err := q.RPush(context.Background(), QueueKey, payload); err != nil {
		t.Fatal(err)
	}

	runWorkerOnce(t, w)

	if ins.count() != 1 || !ins.has("k-good") {
		t.Fatalf("want only the valid event persisted, got %d rows", ins.count())
	}
	if q.len(QueueKey) != 0 || q.len(ProcessingKey) != 0 {
		t.Fatalf("queue not drained: queue=%d processing=%d", q.len(QueueKey), q.len(ProcessingKey))
	}
}
