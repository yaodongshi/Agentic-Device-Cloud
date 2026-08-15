package approval

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// fixedClock is a stable reference time for deterministic expiry tests.
func fixedClock() time.Time {
	return time.Date(2026, 8, 15, 8, 0, 0, 0, time.UTC)
}

// newTestTicket builds a valid PENDING ticket through NewPending, anchored to
// fixedClock so expiry-dependent tests are deterministic.
func newTestTicket(t *testing.T) *ApprovalTicket {
	t.Helper()
	ticket, err := NewPending(
		"tenant-1", "agent-1", "device-1", "power_off",
		json.RawMessage(`{"force":true}`), RiskHigh, time.Time{},
	)
	if err != nil {
		t.Fatalf("NewPending: %v", err)
	}
	ticket.TicketID, err = NewTicketID()
	if err != nil {
		t.Fatalf("NewTicketID: %v", err)
	}
	ticket.CreatedAt = fixedClock()
	ticket.ExpireAt = fixedClock().Add(DefaultExpiry)
	return ticket
}

func TestValidateTransition(t *testing.T) {
	cases := []struct {
		name string
		from Status
		to   Status
		want bool
	}{
		{"pending to approved", StatusPending, StatusApproved, true},
		{"pending to rejected", StatusPending, StatusRejected, true},
		{"pending to expired", StatusPending, StatusExpired, true},
		{"pending to pending", StatusPending, StatusPending, false},
		{"pending to empty", StatusPending, "", false},
		{"approved to approved", StatusApproved, StatusApproved, false},
		{"approved to rejected", StatusApproved, StatusRejected, false},
		{"approved to expired", StatusApproved, StatusExpired, false},
		{"rejected to approved", StatusRejected, StatusApproved, false},
		{"rejected to expired", StatusRejected, StatusExpired, false},
		{"expired to approved", StatusExpired, StatusApproved, false},
		{"expired to expired", StatusExpired, StatusExpired, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ValidateTransition(c.from, c.to); got != c.want {
				t.Errorf("ValidateTransition(%q, %q) = %v, want %v", c.from, c.to, got, c.want)
			}
		})
	}
}

func TestDecisionStatus(t *testing.T) {
	cases := []struct {
		decision string
		want     Status
		wantOK   bool
	}{
		{"approve", StatusApproved, true},
		{"reject", StatusRejected, true},
		{"", "", false},
		{"Approve", "", false},
		{"approve ", "", false},
		{"approve\n", "", false},
		{"expire", "", false},
	}
	for _, c := range cases {
		t.Run(c.decision, func(t *testing.T) {
			got, ok := DecisionStatus(c.decision)
			if ok != c.wantOK || got != c.want {
				t.Errorf("DecisionStatus(%q) = (%q, %v), want (%q, %v)", c.decision, got, ok, c.want, c.wantOK)
			}
		})
	}
}

func TestNewPendingDefaults(t *testing.T) {
	ticket, err := NewPending("t1", "a1", "d1", "tool", json.RawMessage(`{"k":1}`), RiskHigh, time.Time{})
	if err != nil {
		t.Fatalf("NewPending: %v", err)
	}
	if ticket.Status != StatusPending {
		t.Errorf("Status = %q, want PENDING", ticket.Status)
	}
	if ticket.Version != 1 {
		t.Errorf("Version = %d, want 1", ticket.Version)
	}
	if ticket.RiskLevel != RiskHigh {
		t.Errorf("RiskLevel = %d, want %d", ticket.RiskLevel, RiskHigh)
	}
	if ticket.TenantID != "t1" || ticket.AgentID != "a1" || ticket.DeviceID != "d1" || ticket.ToolName != "tool" {
		t.Errorf("identity fields not preserved: %+v", ticket)
	}
	if got := string(ticket.Arguments); got != `{"k":1}` {
		t.Errorf("Arguments = %s", got)
	}
	// Default TTL is 5 minutes (FR-005); expiry is measured from the ticket
	// creation time, not from the caller's clock.
	created := ticket.CreatedAt
	if ticket.ExpireAt.Sub(created) != DefaultExpiry {
		t.Errorf("ExpireAt = %v, want CreatedAt + DefaultExpiry = %v", ticket.ExpireAt, created.Add(DefaultExpiry))
	}
	// One-shot callback key: 64 hex chars of a 256-bit random value.
	if len(ticket.SecretKey) != 64 {
		t.Errorf("SecretKey length = %d, want 64", len(ticket.SecretKey))
	}
	if ticket.SecretHash != HashSecret(ticket.SecretKey) {
		t.Error("SecretHash does not match HashSecret(SecretKey)")
	}
	if len(ticket.ParamsHash) != 64 {
		t.Errorf("ParamsHash length = %d, want 64", len(ticket.ParamsHash))
	}
}

func TestNewPendingExplicitExpiry(t *testing.T) {
	expire := time.Date(2026, 8, 15, 9, 30, 0, 0, time.FixedZone("CST", 8*3600))
	ticket, err := NewPending("t1", "a1", "d1", "tool", nil, RiskLow, expire)
	if err != nil {
		t.Fatalf("NewPending: %v", err)
	}
	if !ticket.ExpireAt.Equal(expire.UTC()) {
		t.Errorf("ExpireAt = %v, want %v", ticket.ExpireAt, expire.UTC())
	}
	if ticket.ExpireAt.Location() != time.UTC {
		t.Errorf("ExpireAt location = %v, want UTC", ticket.ExpireAt.Location())
	}
}

func TestNewPendingValidation(t *testing.T) {
	cases := []struct {
		name      string
		riskLevel int
		args      json.RawMessage
		wantErr   error
	}{
		{"risk below range", -1, json.RawMessage(`{}`), ErrInvalidRiskLevel},
		{"risk above range", 4, json.RawMessage(`{}`), ErrInvalidRiskLevel},
		{"malformed json", RiskHigh, json.RawMessage(`{"a":`), ErrInvalidArguments},
		{"whitespace only json", RiskHigh, json.RawMessage(`   `), ErrInvalidArguments},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := NewPending("t1", "a1", "d1", "tool", c.args, c.riskLevel, time.Time{})
			if !errors.Is(err, c.wantErr) {
				t.Errorf("NewPending error = %v, want %v", err, c.wantErr)
			}
		})
	}
	// nil and JSON null arguments are valid (JSONB NOT NULL in PG, and the
	// scanner path has no arguments at all).
	for _, args := range []json.RawMessage{nil, json.RawMessage("null")} {
		if _, err := NewPending("t1", "a1", "d1", "tool", args, RiskHigh, time.Time{}); err != nil {
			t.Errorf("NewPending with args %q: unexpected error %v", args, err)
		}
	}
}

func TestApplyTransitionApprove(t *testing.T) {
	ticket := newTestTicket(t)
	now := fixedClock()
	cmd := TransitionCmd{Status: StatusApproved, Approver: "e1001", Comment: "ok, proceed"}
	if err := ApplyTransition(ticket, cmd, 1, now); err != nil {
		t.Fatalf("ApplyTransition: %v", err)
	}
	if ticket.Status != StatusApproved {
		t.Errorf("Status = %q, want APPROVED", ticket.Status)
	}
	if ticket.Version != 2 {
		t.Errorf("Version = %d, want 2", ticket.Version)
	}
	if ticket.Approver != "e1001" || ticket.Comment != "ok, proceed" {
		t.Errorf("decision snapshot not stored: approver=%q comment=%q", ticket.Approver, ticket.Comment)
	}
	if ticket.DecidedAt == nil || !ticket.DecidedAt.Equal(now.UTC()) {
		t.Errorf("DecidedAt = %v, want %v", ticket.DecidedAt, now.UTC())
	}
}

func TestApplyTransitionReject(t *testing.T) {
	ticket := newTestTicket(t)
	if err := ApplyTransition(ticket, TransitionCmd{Status: StatusRejected, Approver: "e1002", Comment: "no"}, 1, fixedClock()); err != nil {
		t.Fatalf("ApplyTransition: %v", err)
	}
	if ticket.Status != StatusRejected || ticket.Version != 2 {
		t.Errorf("Status/Version = %q/%d, want REJECTED/2", ticket.Status, ticket.Version)
	}
}

func TestApplyTransitionExpire(t *testing.T) {
	now := fixedClock()
	ticket := newTestTicket(t)
	// Force the ticket past its expiry so the scanner path (EXPIRED target)
	// is realistic.
	ticket.ExpireAt = now.Add(-time.Minute)
	if err := ApplyTransition(ticket, TransitionCmd{Status: StatusExpired}, 1, now); err != nil {
		t.Fatalf("ApplyTransition: %v", err)
	}
	if ticket.Status != StatusExpired {
		t.Errorf("Status = %q, want EXPIRED", ticket.Status)
	}
	if ticket.Version != 2 {
		t.Errorf("Version = %d, want 2", ticket.Version)
	}
	if ticket.DecidedAt != nil {
		t.Errorf("DecidedAt = %v, want nil for EXPIRED", ticket.DecidedAt)
	}
}

func TestApplyTransitionExpireOnValidTicketAllowed(t *testing.T) {
	// EXPIRED is the fail-safe status: device-offline invalidation
	// (HITL-011) must be able to expire a still-valid ticket, so the clock
	// check is exempt for this target.
	ticket := newTestTicket(t)
	if err := ApplyTransition(ticket, TransitionCmd{Status: StatusExpired}, 1, fixedClock()); err != nil {
		t.Errorf("expiring a valid ticket should be allowed, got %v", err)
	}
}

func TestApplyTransitionExpiredDecisionRejected(t *testing.T) {
	now := fixedClock()
	for _, to := range []Status{StatusApproved, StatusRejected} {
		t.Run(string(to), func(t *testing.T) {
			ticket := newTestTicket(t)
			ticket.ExpireAt = now.Add(-time.Second) // expired
			err := ApplyTransition(ticket, TransitionCmd{Status: to}, 1, now)
			if !errors.Is(err, ErrTicketExpired) {
				t.Errorf("expired decision error = %v, want ErrTicketExpired", err)
			}
			if ticket.Status != StatusPending {
				t.Errorf("ticket mutated to %q on failure", ticket.Status)
			}
		})
	}
}

func TestApplyTransitionExpiryBoundary(t *testing.T) {
	// The guard is strict: now == ExpireAt is already too late (expires_at >
	// now() in the PG predicate).
	now := fixedClock()
	ticket := newTestTicket(t)
	ticket.ExpireAt = now
	if err := ApplyTransition(ticket, TransitionCmd{Status: StatusApproved}, 1, now); !errors.Is(err, ErrTicketExpired) {
		t.Errorf("boundary error = %v, want ErrTicketExpired", err)
	}
}

func TestApplyTransitionVersionMismatch(t *testing.T) {
	ticket := newTestTicket(t) // version 1
	err := ApplyTransition(ticket, TransitionCmd{Status: StatusApproved}, 2, fixedClock())
	if !errors.Is(err, ErrTicketHandled) {
		t.Errorf("stale version error = %v, want ErrTicketHandled", err)
	}
}

func TestApplyTransitionAlreadyDecided(t *testing.T) {
	ticket := newTestTicket(t)
	if err := ApplyTransition(ticket, TransitionCmd{Status: StatusApproved}, 1, fixedClock()); err != nil {
		t.Fatalf("first transition: %v", err)
	}
	// Terminal states reject every further transition: repeated callbacks
	// are harmless even without the repository CAS.
	if err := ApplyTransition(ticket, TransitionCmd{Status: StatusRejected}, 2, fixedClock()); !errors.Is(err, ErrTicketHandled) {
		t.Errorf("repeat transition error = %v, want ErrTicketHandled", err)
	}
}

func TestApplyTransitionInvalidTarget(t *testing.T) {
	ticket := newTestTicket(t)
	err := ApplyTransition(ticket, TransitionCmd{Status: StatusPending}, 1, fixedClock())
	if !errors.Is(err, ErrTicketHandled) {
		t.Errorf("PENDING->PENDING error = %v, want ErrTicketHandled", err)
	}
}

func TestClassifyCASFailure(t *testing.T) {
	now := fixedClock()
	expired := newTestTicket(t)
	expired.ExpireAt = now.Add(-time.Minute)
	staleVersion := newTestTicket(t)
	staleVersion.Version = 3
	decided := newTestTicket(t)
	if err := ApplyTransition(decided, TransitionCmd{Status: StatusApproved}, 1, now); err != nil {
		t.Fatalf("setup: %v", err)
	}
	decided.ExpireAt = now.Add(-time.Minute) // decided first, expired later

	cases := []struct {
		name    string
		ticket  *ApprovalTicket
		wantErr error
	}{
		{"nil ticket", nil, ErrTicketNotFound},
		{"pending but expired", expired, ErrTicketExpired},
		{"still valid pending", newTestTicket(t), ErrTicketHandled},
		{"stale version", staleVersion, ErrTicketHandled},
		{"decided even if expired later", decided, ErrTicketHandled},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ClassifyCASFailure(c.ticket, 1, now)
			if !errors.Is(err, c.wantErr) {
				t.Errorf("ClassifyCASFailure = %v, want %v", err, c.wantErr)
			}
		})
	}
}

func TestSignAndVerifyCallback(t *testing.T) {
	secret := "deadbeef" + strings.Repeat("0", 56)
	ticketID := "3fa85f64-5717-4562-b3fc-2c963f66afa6"
	expire := fixedClock().Add(DefaultExpiry)

	// Round trip: a signature the server itself produced must verify.
	for _, decision := range []string{DecisionApprove, DecisionReject} {
		sig := SignCallback(secret, ticketID, decision, expire)
		if len(sig) != 64 {
			t.Errorf("signature length = %d, want 64", len(sig))
		}
		if !VerifyCallbackSignature(secret, ticketID, decision, sig, expire) {
			t.Errorf("valid signature rejected for decision %q", decision)
		}
	}
	// Deterministic: same inputs, same signature.
	if SignCallback(secret, ticketID, DecisionApprove, expire) != SignCallback(secret, ticketID, DecisionApprove, expire) {
		t.Error("signature is not deterministic")
	}
}

func TestVerifyCallbackRejectsTampering(t *testing.T) {
	secret := "s3cr3t"
	ticketID := "3fa85f64-5717-4562-b3fc-2c963f66afa6"
	expire := fixedClock().Add(DefaultExpiry)
	sig := SignCallback(secret, ticketID, DecisionApprove, expire)

	cases := []struct {
		name     string
		secret   string
		id       string
		decision string
		expire   time.Time
		sig      string
	}{
		{"tampered ticket id", secret, "3fa85f64-5717-4562-b3fc-2c963f66afa7", DecisionApprove, expire, sig},
		{"tampered decision", secret, ticketID, DecisionReject, expire, sig},
		{"tampered expire", secret, ticketID, DecisionApprove, expire.Add(60 * time.Second), sig},
		{"tampered signature", secret, ticketID, DecisionApprove, expire, sig[:63] + "0"},
		{"wrong secret", "wrong-secret", ticketID, DecisionApprove, expire, sig},
		{"empty signature", secret, ticketID, DecisionApprove, expire, ""},
		{"non hex signature", secret, ticketID, DecisionApprove, expire, "zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if VerifyCallbackSignature(c.secret, c.id, c.decision, c.sig, c.expire) {
				t.Error("tampered callback verified, want rejection")
			}
		})
	}

	// A signature over an unknown decision is rejected even when the HMAC
	// matches, because decisions are an enum (design/33 3.4.1).
	bogus := SignCallback(secret, ticketID, "sneaky", expire)
	if VerifyCallbackSignature(secret, ticketID, "sneaky", bogus, expire) {
		t.Error("signature over unknown decision verified, want rejection")
	}
}

func TestCallbackWindowValid(t *testing.T) {
	expire := fixedClock().Add(DefaultExpiry)
	cases := []struct {
		name string
		now  time.Time
		want bool
	}{
		{"before expiry", fixedClock(), true},
		{"exactly at expiry", expire, false},
		{"after expiry", expire.Add(time.Second), false},
		{"long after expiry", expire.Add(time.Hour), false},
		{"zero now is treated as in the past", time.Time{}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := CallbackWindowValid(expire, c.now); got != c.want {
				t.Errorf("CallbackWindowValid(%v) = %v, want %v", c.now, got, c.want)
			}
		})
	}

	// A zero expire means the window never opened (Unix 0 is far in the
	// past), so the check must refuse even a zero timestamp input.
	if CallbackWindowValid(time.Time{}, time.Time{}) {
		t.Error("zero expire accepted, want rejection")
	}
}

func TestSecretHelpers(t *testing.T) {
	s1, err := GenerateSecret()
	if err != nil {
		t.Fatalf("GenerateSecret: %v", err)
	}
	s2, err := GenerateSecret()
	if err != nil {
		t.Fatalf("GenerateSecret: %v", err)
	}
	if len(s1) != 64 || len(s2) != 64 {
		t.Fatalf("secret lengths = %d/%d, want 64/64", len(s1), len(s2))
	}
	if s1 == s2 {
		t.Error("GenerateSecret produced the same key twice")
	}
	if HashSecret(s1) == HashSecret(s2) {
		t.Error("distinct secrets hash identically")
	}
	if HashSecret(s1) != HashSecret(s1) {
		t.Error("HashSecret is not deterministic")
	}
	if len(HashSecret(s1)) != 64 {
		t.Errorf("hash length = %d, want 64", len(HashSecret(s1)))
	}
}

func TestNewTicketIDFormat(t *testing.T) {
	id, err := NewTicketID()
	if err != nil {
		t.Fatalf("NewTicketID: %v", err)
	}
	// UUID v4 shape: 8-4-4-4-12 with version nibble 4 and variant nibble 8.
	if len(id) != 36 {
		t.Fatalf("id length = %d, want 36", len(id))
	}
	if id[14] != '4' {
		t.Errorf("version nibble = %q, want '4'", id[14])
	}
	if id[19] != '8' && id[19] != '9' && id[19] != 'a' && id[19] != 'b' {
		t.Errorf("variant nibble = %q, want RFC 4122", id[19])
	}
	other, _ := NewTicketID()
	if id == other {
		t.Error("NewTicketID returned the same id twice")
	}
}

func TestCanonicalArgsHash(t *testing.T) {
	device, tool := "device-1", "power_off"
	base := json.RawMessage(`{"force":true,"timeout":30}`)
	reordered := json.RawMessage(`{"timeout":30,"force":true}`)
	padded := json.RawMessage(`  {"force":true,"timeout":30}  `)

	if CanonicalArgsHash(device, tool, base) != CanonicalArgsHash(device, tool, reordered) {
		t.Error("key reordering changed the hash")
	}
	if CanonicalArgsHash(device, tool, base) != CanonicalArgsHash(device, tool, padded) {
		t.Error("whitespace padding changed the hash")
	}
	if CanonicalArgsHash(device, tool, json.RawMessage(`{"force":true}`)) == CanonicalArgsHash(device, tool, base) {
		t.Error("different arguments hash identically")
	}
	if CanonicalArgsHash("device-2", tool, base) == CanonicalArgsHash(device, tool, base) {
		t.Error("different device hashes identically")
	}
	if CanonicalArgsHash(device, "read_status", base) == CanonicalArgsHash(device, tool, base) {
		t.Error("different tool hashes identically")
	}
	// nil and empty arguments are canonically identical (scanner path).
	if CanonicalArgsHash(device, tool, nil) != CanonicalArgsHash(device, tool, json.RawMessage("")) {
		t.Error("nil and empty arguments hash differently")
	}
}
