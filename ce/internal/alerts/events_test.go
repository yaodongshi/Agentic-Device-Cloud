package alerts

import (
	"testing"
	"time"
)

func makeEvent(i int, tenantID string, at time.Time) Event {
	return Event{
		ID:       NewRuleID(),
		RuleID:   "11111111-2222-3333-4444-555555555555",
		RuleName: "pool watermark",
		TenantID: tenantID,
		Metric:   "adc_pg_pool_connections",
		Severity: SeverityP2,
		FiredAt:  at,
	}
}

func TestEventBufferCapacity(t *testing.T) {
	b := NewEventBuffer(100)
	start := fixedClock
	for i := 0; i < 150; i++ {
		b.Record(makeEvent(i, testTenantA, start.Add(time.Duration(i)*time.Second)))
	}
	if b.Len() != 100 {
		t.Fatalf("Len = %d, want 100 (cap enforced)", b.Len())
	}
	// Newest first: the last recorded event has the highest index.
	all := b.List(testTenantA, 100)
	if len(all) != 100 {
		t.Fatalf("List len = %d, want 100", len(all))
	}
	if !all[0].FiredAt.Equal(start.Add(149 * time.Second)) {
		t.Fatalf("newest event = %v, want the 150th tick", all[0].FiredAt)
	}
	if !all[99].FiredAt.Equal(start.Add(50 * time.Second)) {
		t.Fatalf("oldest retained event = %v, want the 51st tick", all[99].FiredAt)
	}
}

func TestEventBufferTenantFilterAndLimit(t *testing.T) {
	b := NewEventBuffer(100)
	for i := 0; i < 5; i++ {
		b.Record(makeEvent(i, testTenantA, fixedClock.Add(time.Duration(i)*time.Minute)))
		b.Record(makeEvent(i, testTenantB, fixedClock.Add(time.Duration(i)*time.Minute)))
	}
	got := b.List(testTenantA, 3)
	if len(got) != 3 {
		t.Fatalf("List(A, 3) len = %d, want 3", len(got))
	}
	for _, ev := range got {
		if ev.TenantID != testTenantA {
			t.Fatalf("List(A) leaked tenant %q", ev.TenantID)
		}
	}
	if b.List(testTenantA, 0) == nil {
		t.Fatal("List(A, 0) returned nil, want the clamped full page")
	}
	if got := b.List(testTenantA, 1000); len(got) != 5 {
		t.Fatalf("List(A, 1000) len = %d, want 5 (clamped)", len(got))
	}
}

func TestEventBufferZeroCapacityFallsBack(t *testing.T) {
	b := NewEventBuffer(0)
	if b.Capacity() != DefaultEventCapacity {
		t.Fatalf("Capacity = %d, want default %d", b.Capacity(), DefaultEventCapacity)
	}
	b.Record(makeEvent(1, testTenantA, fixedClock))
	if b.Len() != 1 {
		t.Fatalf("Len = %d, want 1", b.Len())
	}
}
