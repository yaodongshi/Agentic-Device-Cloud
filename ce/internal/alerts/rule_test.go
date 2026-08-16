package alerts

import (
	"strings"
	"testing"
)

func TestValidateRule(t *testing.T) {
	good := Rule{
		Name:        "gateway 5xx surge",
		Metric:      "adc_http_requests_total",
		Operator:    OpGreater,
		Threshold:   100,
		DurationSec: 60,
		Severity:    SeverityP2,
		Enabled:     true,
		Labels:      map[string]string{"code": "500"},
	}
	cases := []struct {
		name    string
		mutate  func(*Rule)
		wantErr string
	}{
		{"ok", func(*Rule) {}, ""},
		{"nil rule", nil, ""},
		{"empty name", func(r *Rule) { r.Name = "" }, "name"},
		{"name too long", func(r *Rule) { r.Name = strings.Repeat("n", MaxRuleNameLen+1) }, "name"},
		{"bad metric", func(r *Rule) { r.Metric = "adc_unknown_total" }, "whitelist"},
		{"bad operator", func(r *Rule) { r.Operator = "==" }, "operator"},
		{"nan threshold", func(r *Rule) { r.Threshold = float64NaN() }, "threshold"},
		{"negative threshold", func(r *Rule) { r.Threshold = -1 }, "threshold"},
		{"duration too large", func(r *Rule) { r.DurationSec = MaxDurationSec + 1 }, "duration"},
		{"bad severity", func(r *Rule) { r.Severity = "P0" }, "severity"},
		{"bad label key", func(r *Rule) { r.Labels = map[string]string{"code}": "x"} }, "label key"},
		{"bad id", func(r *Rule) { r.ID = "not-a-uuid" }, "uuid"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.mutate == nil {
				if err := ValidateRule(nil); err == nil {
					t.Fatal("ValidateRule(nil) = nil, want error")
				}
				return
			}
			r := good
			tc.mutate(&r)
			err := ValidateRule(&r)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("ValidateRule = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("ValidateRule = %v, want error containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestRuleMatches(t *testing.T) {
	cases := []struct {
		op    Operator
		thr   float64
		value float64
		want  bool
	}{
		{OpGreater, 10, 10, false},
		{OpGreater, 10, 10.1, true},
		{OpGreaterOrEqual, 10, 10, true},
		{OpGreaterOrEqual, 10, 9.9, false},
		{OpLess, 10, 9.9, true},
		{OpLess, 10, 10, false},
	}
	for _, tc := range cases {
		r := Rule{Operator: tc.op, Threshold: tc.thr}
		if got := r.Matches(tc.value); got != tc.want {
			t.Errorf("Matches(%v %s %v) = %v, want %v", tc.value, tc.op, tc.thr, got, tc.want)
		}
	}
}

func TestNewRuleIDShape(t *testing.T) {
	for i := 0; i < 100; i++ {
		id := NewRuleID()
		if !ruleIDRe.MatchString(id) {
			t.Fatalf("NewRuleID = %q, want uuid shape", id)
		}
	}
}

func TestAlertMetricNames(t *testing.T) {
	names := AlertMetricNames()
	if len(names) != len(alertMetrics) {
		t.Fatalf("AlertMetricNames len = %d, want %d", len(names), len(alertMetrics))
	}
	for _, n := range names {
		if !IsAlertMetric(n) {
			t.Fatalf("AlertMetricNames contains %q but IsAlertMetric rejects it", n)
		}
	}
}
