// Package approval implements the HITL approval core of the Approval Service
// (design/31 LLD 3.3, design/32 3.8/6.1, design/33 1.4/3.4). It ships the
// ticket aggregate with its four-state machine, the PostgreSQL-backed CAS
// repository (SEC-11), callback signature helpers (SEC-13/01), the unified
// cross-node wake channel (SEC-10) and the timeout scanner. PostgreSQL is the
// source of truth (ADR-05); Valkey is only the wake transport (design/32 1.1).
package approval

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Status is the approval ticket state machine state. Values must match the
// adc_approval_tickets.status CHECK constraint (design/32 3.8).
type Status string

const (
	StatusPending  Status = "PENDING"
	StatusApproved Status = "APPROVED"
	StatusRejected Status = "REJECTED"
	StatusExpired  Status = "EXPIRED"
)

// Decision values accepted by the callback API (design/33 3.4.1).
const (
	DecisionApprove = "approve"
	DecisionReject  = "reject"
)

// Risk levels (design/32 2.4): 0 read-only, 1 low, 2 high (HITL), 3 critical.
const (
	RiskRead     = 0
	RiskLow      = 1
	RiskHigh     = 2
	RiskCritical = 3
)

// DefaultExpiry is the ticket TTL used when Create receives no explicit
// ExpireAt (FR-005 default 5 minutes).
const DefaultExpiry = 5 * time.Minute

var (
	ErrInvalidRiskLevel = errors.New("approval: risk level must be 0-3")
	ErrInvalidArguments = errors.New("approval: arguments must be valid JSON")
)

// ApprovalTicket is the HITL aggregate root mapped to adc_approval_tickets
// (design/32 3.8). Column name mapping: TicketID -> id, Approver ->
// approver_name (snapshot semantics, design/32 3.13), ExpireAt -> expires_at.
//
// SecretKey is the ticket-level one-shot callback key (SEC-13). It is a
// 256-bit random value that exists in memory only: Create generates it, the
// caller embeds the signature built with it into the approval card URL, and
// the repository persists only SecretHash (SHA-256). It is never logged and
// never written to PostgreSQL (design/33 1.4: "落库仅存哈希").
type ApprovalTicket struct {
	TicketID  string
	TenantID  string
	AgentID   string
	DeviceID  string
	ToolName  string
	Arguments json.RawMessage // pending call arguments (JSONB in PG)
	RiskLevel int
	Status    Status
	Approver  string // approver_name snapshot at decision time
	Comment   string
	CreatedAt time.Time
	ExpireAt  time.Time
	Version   int // optimistic lock (design/32 1.4)
	DecidedAt *time.Time

	SecretKey  string // one-shot callback key, memory only (SEC-13)
	SecretHash string // persisted SHA-256 of SecretKey
	ParamsHash string // dedup key, see CanonicalArgsHash (design/32 6.3)
}

// NewPending builds a PENDING ticket with all derived fields computed
// (version, hashes, default status). ExpireAt zero means DefaultExpiry from
// now. Errors: ErrInvalidRiskLevel, ErrInvalidArguments.
func NewPending(tenantID, agentID, deviceID, toolName string, arguments json.RawMessage, riskLevel int, expireAt time.Time) (*ApprovalTicket, error) {
	if riskLevel < RiskRead || riskLevel > RiskCritical {
		return nil, fmt.Errorf("%w: %d", ErrInvalidRiskLevel, riskLevel)
	}
	if len(arguments) > 0 && !json.Valid(arguments) {
		return nil, fmt.Errorf("%w", ErrInvalidArguments)
	}
	now := time.Now().UTC()
	t := &ApprovalTicket{
		TenantID:  tenantID,
		AgentID:   agentID,
		DeviceID:  deviceID,
		ToolName:  toolName,
		Arguments: arguments,
		RiskLevel: riskLevel,
		Status:    StatusPending,
		CreatedAt: now,
		Version:   1,
	}
	if expireAt.IsZero() {
		t.ExpireAt = now.Add(DefaultExpiry)
	} else {
		t.ExpireAt = expireAt.UTC()
	}
	secret, err := GenerateSecret()
	if err != nil {
		return nil, err
	}
	t.SecretKey = secret
	t.SecretHash = HashSecret(secret)
	t.ParamsHash = CanonicalArgsHash(deviceID, toolName, arguments)
	return t, nil
}

// ValidateTransition reports whether a state machine step is legal
// (design/31 LLD 2.3.2): only PENDING may leave its state, and only once.
// Terminal states (APPROVED/REJECTED/EXPIRED) reject every further
// transition, which is what makes repeated callbacks harmless on top of the
// CAS in the repository.
func ValidateTransition(from, to Status) bool {
	if from != StatusPending {
		return false
	}
	switch to {
	case StatusApproved, StatusRejected, StatusExpired:
		return true
	}
	return false
}

// DecisionStatus maps a callback decision string to the target status
// (design/33 3.4.1: "approve"/"reject"). The second return is false for any
// other value (error code 10001 per design/33).
func DecisionStatus(decision string) (Status, bool) {
	switch decision {
	case DecisionApprove:
		return StatusApproved, true
	case DecisionReject:
		return StatusRejected, true
	}
	return "", false
}

// GenerateSecret returns a 32-byte random key, hex encoded (256 bits of
// entropy, NFR-004). This is the ticket-level one-shot callback key (SEC-13).
func GenerateSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("approval: generate secret: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// HashSecret returns the persisted form of a ticket secret. A one-way hash is
// sufficient because the key carries 256 bits of entropy (same reasoning as
// auth.HashSecret); the callback secret is never recoverable from the
// database, matching design/33 1.4.
func HashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// NewTicketID returns a random UUID-format id for adc_approval_tickets.id
// (UUID PK per design/32 1.3). crypto/rand keeps the approval package free of
// non-stdlib UUID dependencies. Non-finite values are treated as an error.
func NewTicketID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("approval: generate ticket id: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// canonicalArgs returns a deterministic byte form of the arguments so that
// semantically equal payloads hash identically: JSON objects are
// unmarshaled and remarshaled (encoding/json sorts object keys), anything
// else keeps its raw bytes.
func canonicalArgs(args json.RawMessage) []byte {
	trimmed := []byte(strings.TrimSpace(string(args)))
	if len(trimmed) == 0 {
		return nil
	}
	var v any
	if err := json.Unmarshal(trimmed, &v); err == nil {
		if b, err := json.Marshal(v); err == nil {
			return b
		}
	}
	return trimmed
}

// CanonicalArgsHash computes params_hash = SHA-256(device_id || tool_name ||
// canonical arguments), the key of the partial unique index
// uq_tickets_pending_dedup (design/32 6.3): one in-flight ticket per device,
// tool and parameter set, so an Agent retry cannot flood the approval queue.
func CanonicalArgsHash(deviceID, toolName string, args json.RawMessage) string {
	h := sha256.New()
	_, _ = h.Write([]byte(deviceID))
	_, _ = h.Write([]byte{0x1f})
	_, _ = h.Write([]byte(toolName))
	_, _ = h.Write([]byte{0x1f})
	_, _ = h.Write(canonicalArgs(args))
	return hex.EncodeToString(h.Sum(nil))
}
