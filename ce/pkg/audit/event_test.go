package audit

import (
	"testing"
	"time"
)

// The status constants must stay byte-identical to the adc_audit_logs
// status CHECK constraint (design/32 3.10).
func TestStatusConstants(t *testing.T) {
	if StatusSuccess != "success" || StatusFailed != "failed" || StatusBlockedByHITL != "blocked_by_hitl" {
		t.Fatalf("status constants drifted: %q %q %q",
			StatusSuccess, StatusFailed, StatusBlockedByHITL)
	}
}

func TestAuditEventValidate(t *testing.T) {
	tests := []struct {
		name    string
		ev      *AuditEvent
		wantErr bool
	}{
		{"nil event", nil, true},
		{"missing event id", &AuditEvent{TenantID: "t", Status: StatusSuccess}, true},
		{"missing tenant", &AuditEvent{EventID: "e", Status: StatusSuccess}, true},
		{"empty status", &AuditEvent{EventID: "e", TenantID: "t"}, true},
		{"bad status", &AuditEvent{EventID: "e", TenantID: "t", Status: "pending"}, true},
		{"success", &AuditEvent{EventID: "e", TenantID: "t", Status: StatusSuccess}, false},
		{"failed", &AuditEvent{EventID: "e", TenantID: "t", Status: StatusFailed}, false},
		{"blocked by hitl", &AuditEvent{EventID: "e", TenantID: "t", Status: StatusBlockedByHITL}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.ev.Validate()
			if (err != nil) != tc.wantErr {
				t.Fatalf("Validate() err = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}

func TestDefaultBackoff(t *testing.T) {
	tests := map[int]time.Duration{
		1: time.Second,
		2: 2 * time.Second,
		3: 4 * time.Second,
	}
	for attempt, want := range tests {
		if got := defaultBackoff(attempt); got != want {
			t.Errorf("defaultBackoff(%d) = %v, want %v", attempt, got, want)
		}
	}
}

func TestTransientErrorClassification(t *testing.T) {
	err := &transientError{err: &permanentError{}}
	if !isRetryable(err) {
		t.Fatal("transientError must be retryable")
	}
	if isRetryable(permanentError{}) {
		t.Fatal("plain errors must not be retryable")
	}
}
