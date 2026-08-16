package alerts

import (
	"context"
	"math"
	"testing"
	"time"
)

// float64NaN returns a NaN via math to keep go vet quiet about direct
// NaN comparisons.
func float64NaN() float64 { return math.NaN() }

const (
	testTenantA = "00000000-0000-4000-8000-000000000001"
	testTenantB = "00000000-0000-4000-8000-000000000002"
)

var fixedClock = time.Date(2026, 8, 14, 8, 0, 0, 0, time.UTC)

func sampleRule(name string, tenantID string) Rule {
	return Rule{
		Name:        name,
		Metric:      "adc_http_requests_total",
		Operator:    OpGreater,
		Threshold:   100,
		DurationSec: 0,
		Severity:    SeverityP2,
		Enabled:     true,
		TenantID:    tenantID,
	}
}

func TestMemoryRuleStoreReplaceListAll(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryRuleStore()
	s.now = func() time.Time { return fixedClock }

	if err := s.Replace(ctx, testTenantA, []Rule{sampleRule("rule-a1", ""), sampleRule("rule-a2", "")}); err != nil {
		t.Fatalf("Replace A: %v", err)
	}
	if err := s.Replace(ctx, testTenantB, []Rule{sampleRule("rule-b1", "")}); err != nil {
		t.Fatalf("Replace B: %v", err)
	}

	listA, err := s.List(ctx, testTenantA)
	if err != nil {
		t.Fatalf("List A: %v", err)
	}
	if len(listA) != 2 || listA[0].Name != "rule-a1" || listA[1].Name != "rule-a2" {
		t.Fatalf("List A = %+v", listA)
	}
	for _, r := range listA {
		if r.TenantID != testTenantA {
			t.Fatalf("stored rule tenant = %q, want %q", r.TenantID, testTenantA)
		}
		if r.ID == "" || !ruleIDRe.MatchString(r.ID) {
			t.Fatalf("stored rule id = %q, want uuid", r.ID)
		}
		if !r.CreatedAt.Equal(fixedClock) || !r.UpdatedAt.Equal(fixedClock) {
			t.Fatalf("stored rule timestamps = %v/%v, want fixed clock", r.CreatedAt, r.UpdatedAt)
		}
	}

	all, err := s.All(ctx)
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("All len = %d, want 3", len(all))
	}

	// Replace with a subset: full-replace semantics drop rule-a2.
	if err := s.Replace(ctx, testTenantA, []Rule{sampleRule("rule-a1", "")}); err != nil {
		t.Fatalf("Replace A subset: %v", err)
	}
	listA, _ = s.List(ctx, testTenantA)
	if len(listA) != 1 || listA[0].Name != "rule-a1" {
		t.Fatalf("List A after subset replace = %+v", listA)
	}
	// Empty replace clears the tenant.
	if err := s.Replace(ctx, testTenantB, nil); err != nil {
		t.Fatalf("Replace B empty: %v", err)
	}
	listB, _ := s.List(ctx, testTenantB)
	if len(listB) != 0 {
		t.Fatalf("List B after clear = %+v, want empty", listB)
	}
}

// TestMemoryRuleStoreIsolation verifies the returned slices are copies:
// mutating them must not corrupt the store.
func TestMemoryRuleStoreIsolation(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryRuleStore()
	rule := sampleRule("a", "")
	rule.Labels = map[string]string{"code": "200"}
	if err := s.Replace(ctx, testTenantA, []Rule{rule}); err != nil {
		t.Fatal(err)
	}
	list, _ := s.List(ctx, testTenantA)
	list[0].Name = "mutated"
	list[0].Labels["code"] = "500"
	again, _ := s.List(ctx, testTenantA)
	if again[0].Name != "a" {
		t.Fatalf("store leaked mutated name %q", again[0].Name)
	}
	if again[0].Labels["code"] != "200" {
		t.Fatalf("store leaked mutated labels %v", again[0].Labels)
	}
}
