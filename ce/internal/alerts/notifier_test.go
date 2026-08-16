package alerts

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestWebhookNotifierDeliversPayload(t *testing.T) {
	var mu sync.Mutex
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		_ = json.Unmarshal(body, &got)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"errcode":0}`))
	}))
	defer srv.Close()

	ev := Event{RuleID: "r1", RuleName: "pool", Severity: SeverityP1, Metric: "adc_pg_pool_connections", FiredAt: fixedClock}
	n := NewWebhookNotifier(srv.URL)
	if err := n.SendAlert(context.Background(), &ev); err != nil {
		t.Fatalf("SendAlert: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if got["rule_id"] != "r1" || got["severity"] != "P1" {
		t.Fatalf("delivered payload = %v", got)
	}
}

func TestWebhookNotifierRejectsHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	n := NewWebhookNotifier(srv.URL)
	if err := n.SendAlert(context.Background(), &Event{}); err == nil {
		t.Fatal("SendAlert on HTTP 500 returned nil error")
	}
}

func TestEmailNotifierPlaceholder(t *testing.T) {
	n := &EmailNotifier{}
	if err := n.SendAlert(context.Background(), &Event{}); err != nil {
		t.Fatalf("EmailNotifier placeholder returned %v, want nil (log-only)", err)
	}
}

func TestMultiAlertNotifierFansOutAndJoinsErrors(t *testing.T) {
	ok1 := &recordingNotifier{}
	ok2 := &recordingNotifier{}
	bad := &failingNotifier{}
	m := NewMultiAlertNotifier(ok1, bad, ok2)
	err := m.SendAlert(context.Background(), &Event{})
	if err == nil {
		t.Fatal("MultiAlertNotifier with a broken channel returned nil error")
	}
	if ok1.calls != 1 || ok2.calls != 1 {
		t.Fatalf("fan-out calls = %d/%d, want 1/1", ok1.calls, ok2.calls)
	}
}

func TestRetryAlertNotifierBackoff(t *testing.T) {
	var mu sync.Mutex
	var sleeps []time.Duration
	bad := &failingNotifier{}
	r := NewRetryAlertNotifier(bad)
	r.Sleep = func(_ context.Context, d time.Duration) error {
		mu.Lock()
		sleeps = append(sleeps, d)
		mu.Unlock()
		return nil
	}
	if err := r.SendAlert(context.Background(), &Event{}); err == nil {
		t.Fatal("RetryAlertNotifier around a broken channel returned nil error")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(sleeps) != 2 || sleeps[0] != time.Second || sleeps[1] != 2*time.Second {
		t.Fatalf("backoffs = %v, want [1s 2s]", sleeps)
	}
}

// TestRetryAlertNotifierRecovers verifies the retry wrapper returns nil as
// soon as one attempt succeeds.
func TestRetryAlertNotifierRecovers(t *testing.T) {
	flaky := &flakyNotifier{failFirst: 1}
	r := NewRetryAlertNotifier(flaky)
	r.Sleep = func(context.Context, time.Duration) error { return nil }
	if err := r.SendAlert(context.Background(), &Event{}); err != nil {
		t.Fatalf("SendAlert = %v, want nil after recovery", err)
	}
	if flaky.calls != 2 {
		t.Fatalf("calls = %d, want 2", flaky.calls)
	}
}

type recordingNotifier struct {
	mu    sync.Mutex
	calls int
}

func (n *recordingNotifier) SendAlert(context.Context, *Event) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.calls++
	return nil
}

type failingNotifier struct{}

func (*failingNotifier) SendAlert(context.Context, *Event) error {
	return context.DeadlineExceeded
}

type flakyNotifier struct {
	mu        sync.Mutex
	failFirst int
	calls     int
}

func (n *flakyNotifier) SendAlert(context.Context, *Event) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.calls++
	if n.failFirst > 0 {
		n.failFirst--
		return context.DeadlineExceeded
	}
	return nil
}
