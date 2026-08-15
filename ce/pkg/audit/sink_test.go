package audit

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// failingQueue fails every operation; it exercises sink error handling.
type failingQueue struct{ err error }

func (f failingQueue) RPush(context.Context, string, []byte) error { return f.err }
func (f failingQueue) BRPopLPush(context.Context, string, string, time.Duration) ([]byte, error) {
	return nil, f.err
}
func (f failingQueue) LRem(context.Context, string, int64, []byte) error { return f.err }

func TestSinkEnqueue(t *testing.T) {
	q := newMemQueue()
	s := NewSink(q)
	ev := testEvent("11111111-1111-1111-1111-111111111111")
	ev.TraceID = "trace-1"
	ev.Approver = "approver-1"

	if err := s.Enqueue(context.Background(), ev); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if got := q.len(QueueKey); got != 1 {
		t.Fatalf("queue length = %d, want 1", got)
	}

	var got AuditEvent
	if err := json.Unmarshal(q.list(QueueKey)[0], &got); err != nil {
		t.Fatalf("unmarshal queued payload: %v", err)
	}
	if got.EventID != ev.EventID || got.TenantID != ev.TenantID || got.Status != StatusSuccess {
		t.Errorf("queued event = %+v, want id/tenant/status of %+v", got, ev)
	}
	if got.TraceID != "trace-1" || got.Approver != "approver-1" {
		t.Errorf("queued event lost fields: trace=%q approver=%q", got.TraceID, got.Approver)
	}
	if got.Params == nil || got.Params["x"] != float64(1) {
		t.Errorf("params = %v, want map[x:1]", got.Params)
	}
}

func TestSinkRejectsInvalidEvent(t *testing.T) {
	tests := []struct {
		name string
		ev   *AuditEvent
	}{
		{"nil event", nil},
		{"missing event id", &AuditEvent{TenantID: "t", Status: StatusSuccess}},
		{"missing tenant", &AuditEvent{EventID: "e", Status: StatusSuccess}},
		{"bad status", &AuditEvent{EventID: "e", TenantID: "t", Status: "pending"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			q := newMemQueue()
			s := NewSink(q)
			if err := s.Enqueue(context.Background(), tc.ev); err == nil {
				t.Fatal("Enqueue must reject the invalid event")
			}
			if got := q.len(QueueKey); got != 0 {
				t.Fatalf("queue length = %d, want 0: invalid events must not be queued", got)
			}
		})
	}
}

func TestSinkNilQueue(t *testing.T) {
	s := NewSink(nil)
	if err := s.Enqueue(context.Background(), testEvent("id")); err == nil {
		t.Fatal("Enqueue with a nil queue must fail")
	}
}

func TestSinkPropagatesQueueError(t *testing.T) {
	want := errors.New("valkey down")
	s := NewSink(failingQueue{err: want})
	err := s.Enqueue(context.Background(), testEvent("id"))
	if err == nil {
		t.Fatal("Enqueue must propagate the queue error")
	}
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want wrapped %v", err, want)
	}
}

func TestConstructors(t *testing.T) {
	if NewValkeySink(nil) == nil {
		t.Fatal("NewValkeySink must not return nil")
	}
	if NewValkeyWorker(nil, nil) == nil {
		t.Fatal("NewValkeyWorker must not return nil")
	}
}
