package audit

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// errInjected is a transient failure used for injection.
type errInjected struct{}

func (errInjected) Error() string   { return "audit: injected transient failure" }
func (errInjected) Retryable() bool { return true }

// fakeInserter simulates adc_audit_logs: rows are deduplicated by
// EventID, mirroring the request_id unique index (design/32 6.3).
type fakeInserter struct {
	mu      sync.Mutex
	rows    map[string]*AuditEvent
	calls   int
	failFor int // fail the next N InsertBatch calls with errInjected
}

func newFakeInserter() *fakeInserter {
	return &fakeInserter{rows: make(map[string]*AuditEvent)}
}

func (f *fakeInserter) InsertBatch(_ context.Context, evs []*AuditEvent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.failFor > 0 {
		f.failFor--
		return errInjected{}
	}
	for _, ev := range evs {
		if _, ok := f.rows[ev.EventID]; !ok {
			f.rows[ev.EventID] = ev
		}
	}
	return nil
}

func (f *fakeInserter) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeInserter) rowCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.rows)
}

func (f *fakeInserter) has(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.rows[id]
	return ok
}

// permanentFailInserter always fails with a non-retryable error.
type permanentFailInserter struct {
	mu sync.Mutex
	n  int
}

type permanentError struct{}

func (permanentError) Error() string { return "audit: permanent failure" }

func (p *permanentFailInserter) InsertBatch(context.Context, []*AuditEvent) error {
	p.mu.Lock()
	p.n++
	p.mu.Unlock()
	return permanentError{}
}

func (p *permanentFailInserter) calls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.n
}

func testEvent(id string) *AuditEvent {
	return &AuditEvent{
		EventID:   id,
		TenantID:  "aaaaaaaa-0000-0000-0000-000000000001",
		AgentID:   "agent-1",
		DeviceID:  "bbbbbbbb-0000-0000-0000-000000000002",
		ToolName:  "tool-1",
		Params:    map[string]any{"x": 1},
		Status:    StatusSuccess,
		CreatedAt: time.Now(),
	}
}

// eventually polls cond until it holds or the deadline passes.
func eventually(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition not met within timeout")
}

// startWorker runs w in the background; the returned cancel stops it and
// a cleanup hook asserts that Run returns nil on cancellation.
func startWorker(t *testing.T, w *Worker) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("worker.Run returned %v, want nil on cancellation", err)
			}
		case <-time.After(2 * time.Second):
			t.Error("worker did not stop after cancellation")
		}
	})
	return cancel
}

func newTestWorker(q Queue, ins Inserter) *Worker {
	return NewWorker(WorkerConfig{
		Queue:       q,
		Inserter:    ins,
		PollTimeout: 20 * time.Millisecond,
		Backoff:     func(int) time.Duration { return time.Millisecond },
	})
}

func mustEnqueue(t *testing.T, q Queue, ev *AuditEvent) {
	t.Helper()
	s := NewSink(q)
	if err := s.Enqueue(context.Background(), ev); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
}

func mustMarshal(t *testing.T, ev *AuditEvent) []byte {
	t.Helper()
	b, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func TestWorkerPersistsEvents(t *testing.T) {
	q := newMemQueue()
	ins := newFakeInserter()
	startWorker(t, newTestWorker(q, ins))
	mustEnqueue(t, q, testEvent("11111111-1111-1111-1111-111111111111"))
	mustEnqueue(t, q, testEvent("22222222-2222-2222-2222-222222222222"))

	eventually(t, time.Second, func() bool {
		return ins.rowCount() == 2 && q.len(QueueKey) == 0 && q.len(ProcessingKey) == 0
	})
}

func TestWorkerIdempotentOnRedelivery(t *testing.T) {
	q := newMemQueue()
	ins := newFakeInserter()
	startWorker(t, newTestWorker(q, ins))

	// The same request_id twice: the first insert wins, the redelivery
	// must be acknowledged without creating a second row (design/32 6.3).
	mustEnqueue(t, q, testEvent("33333333-3333-3333-3333-333333333333"))
	mustEnqueue(t, q, testEvent("33333333-3333-3333-3333-333333333333"))

	eventually(t, time.Second, func() bool {
		return ins.rowCount() == 1 &&
			q.len(QueueKey) == 0 &&
			q.len(ProcessingKey) == 0
	})
	if !ins.has("33333333-3333-3333-3333-333333333333") {
		t.Fatal("event missing after processing")
	}
}

func TestWorkerRetriesTransientFailures(t *testing.T) {
	q := newMemQueue()
	ins := newFakeInserter()
	ins.failFor = 2 // first two attempts fail, the third succeeds
	startWorker(t, newTestWorker(q, ins))
	mustEnqueue(t, q, testEvent("44444444-4444-4444-4444-444444444444"))

	eventually(t, time.Second, func() bool {
		return ins.rowCount() == 1 && q.len(ProcessingKey) == 0
	})
	if got := ins.callCount(); got != 3 {
		t.Fatalf("InsertBatch calls = %d, want 3 (1 initial + 2 retries)", got)
	}
}

func TestWorkerRequeuesAfterRetryExhaustion(t *testing.T) {
	q := newMemQueue()
	ins := newFakeInserter()
	ins.failFor = 1000 // keeps failing: every dequeue cycle exhausts its retries
	cancel := startWorker(t, newTestWorker(q, ins))
	mustEnqueue(t, q, testEvent("55555555-5555-5555-5555-555555555555"))

	// calls >= 4 means one dequeue cycle burned 1 initial attempt + 3
	// retries; pushCount >= 2 proves the batch was pushed back (requeue).
	eventually(t, 2*time.Second, func() bool {
		return ins.callCount() >= 4 && q.pushCount(QueueKey) >= 2
	})
	cancel()

	if ins.rowCount() != 0 {
		t.Fatalf("rows = %d, want 0: a permanently failing insert must not persist", ins.rowCount())
	}
}

func TestWorkerSkipsRetriesForPermanentErrors(t *testing.T) {
	q := newMemQueue()
	ins := &permanentFailInserter{}
	var backoffCalls int32
	w := NewWorker(WorkerConfig{
		Queue:       q,
		Inserter:    ins,
		PollTimeout: 20 * time.Millisecond,
		Backoff: func(attempt int) time.Duration {
			atomic.AddInt32(&backoffCalls, 1)
			return time.Millisecond
		},
	})
	cancel := startWorker(t, w)
	mustEnqueue(t, q, testEvent("66666666-6666-6666-6666-666666666666"))

	// One full cycle: dequeue -> insert fails (permanent) -> requeue.
	// Both counters are monotonic, so no timing race here.
	eventually(t, 2*time.Second, func() bool {
		return ins.calls() >= 1 && q.pushCount(QueueKey) >= 2
	})
	cancel()

	if got := atomic.LoadInt32(&backoffCalls); got != 0 {
		t.Fatalf("backoff called %d times, want 0: permanent errors must skip the retry loop", got)
	}
}

func TestWorkerDropsMalformedPayload(t *testing.T) {
	q := newMemQueue()
	ins := newFakeInserter()
	startWorker(t, newTestWorker(q, ins))
	if err := q.RPush(context.Background(), QueueKey, []byte("not-json")); err != nil {
		t.Fatalf("RPush: %v", err)
	}

	eventually(t, time.Second, func() bool {
		return q.len(QueueKey) == 0 && q.len(ProcessingKey) == 0 && ins.callCount() == 0
	})
}

func TestWorkerRecoversStrandedProcessing(t *testing.T) {
	q := newMemQueue()
	ins := newFakeInserter()

	// Simulate a crash between BRPOPLPUSH and LRem: the event is
	// stranded in the processing list when the worker starts.
	if err := q.RPush(context.Background(), ProcessingKey,
		mustMarshal(t, testEvent("77777777-7777-7777-7777-777777777777"))); err != nil {
		t.Fatalf("RPush: %v", err)
	}
	startWorker(t, newTestWorker(q, ins))

	eventually(t, time.Second, func() bool {
		return ins.rowCount() == 1 && q.len(ProcessingKey) == 0
	})
}

func TestWorkerBatchesInserts(t *testing.T) {
	q := newMemQueue()
	ins := newFakeInserter()
	w := newTestWorker(q, ins)
	w.batchSize = 2

	// Enqueue before starting the worker: both events are already
	// available when the first dequeue happens, so one drain cycle must
	// collect them into a single batch.
	mustEnqueue(t, q, testEvent("88888888-8888-8888-8888-888888888888"))
	mustEnqueue(t, q, testEvent("99999999-9999-9999-9999-999999999999"))
	startWorker(t, w)

	eventually(t, time.Second, func() bool { return ins.rowCount() == 2 })
	if got := ins.callCount(); got != 1 {
		t.Fatalf("InsertBatch calls = %d, want 1: two queued events must land in one batch", got)
	}
}

func TestWorkerGracefulShutdown(t *testing.T) {
	q := newMemQueue()
	ins := newFakeInserter()
	cancel := startWorker(t, newTestWorker(q, ins))
	cancel() // empty queue: Run must return nil promptly (asserted by cleanup)
}

func TestWorkerDefaults(t *testing.T) {
	w := NewWorker(WorkerConfig{})
	if w.pollTimeout != defaultPollTimeout || w.batchSize != defaultBatchSize || w.maxRetries != defaultMaxRetries {
		t.Fatalf("defaults not applied: poll=%v batch=%d retries=%d",
			w.pollTimeout, w.batchSize, w.maxRetries)
	}
	if w.backoff == nil || w.logger == nil {
		t.Fatal("backoff and logger must be defaulted")
	}
	if w.srcKey != QueueKey || w.procKey != ProcessingKey {
		t.Fatalf("keys = (%s, %s), want (%s, %s)", w.srcKey, w.procKey, QueueKey, ProcessingKey)
	}
}

func TestPGInserterNilPool(t *testing.T) {
	p := &pgInserter{pool: nil}
	if err := p.InsertBatch(context.Background(), []*AuditEvent{testEvent("id")}); err == nil {
		t.Fatal("nil pool must fail")
	}
}
