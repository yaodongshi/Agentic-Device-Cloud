// Package mcpbinding implements the class-A native MCP device binding
// lifecycle (FR-021, design/82 B5, doc/07 chapter 2): MCP endpoint
// registration, the OAuth 2.1 client credentials binding against the
// device-side authorization server, and the post-binding tool catalog
// synchronization into adc_device_tools.
//
// The binding state machine is persisted on the adc_devices row of a
// class-A device (device_class='A') and mirrors the doc/07 seven-step
// pairing flow:
//
//	REGISTERED -> BINDING   (mcp_endpoint registered, pairing token issued)
//	BINDING    -> BOUND     (pairing token consumed once, OAuth token held)
//	BOUND      -> REVOKED   (unbind)
//	REGISTERED / BINDING -> REVOKED (admin cancel before/while pairing)
//	REVOKED    -> BINDING   (re-binding starts a fresh pairing)
//
// REVOKED is not terminal for the device row itself: an admin may re-run
// the pairing flow. The pairing token is the anti-MITM device (doc/07
// step 4): only its SHA-256 hash is stored, the plaintext exists in the
// initiate response exactly once (NFR-004). The OAuth access token is
// never persisted; it lives in the in-memory cache of the OAuth binder.
package mcpbinding

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// Status is the binding state machine state. Values must match the
// adc_devices.binding_status CHECK constraint (migration 0003).
type Status string

const (
	StatusRegistered Status = "REGISTERED"
	StatusBinding    Status = "BINDING"
	StatusBound      Status = "BOUND"
	StatusRevoked    Status = "REVOKED"
)

// DefaultBindingTokenTTL is the pairing token lifetime when the caller
// does not override it (design/32 3.5: binding_expire_at default 24h).
const DefaultBindingTokenTTL = 24 * time.Hour

// transitionTable is the state machine definition. REVOKED has one
// outgoing edge: RegisterEndpoint re-opens a fresh pairing.
var transitionTable = map[Status][]Status{
	StatusRegistered: {StatusBinding, StatusRevoked},
	StatusBinding:    {StatusBound, StatusRevoked},
	StatusBound:      {StatusRevoked},
	StatusRevoked:    {StatusBinding},
}

// ValidateTransition reports whether a state machine step is legal.
func ValidateTransition(from, to Status) bool {
	for _, next := range transitionTable[from] {
		if next == to {
			return true
		}
	}
	return false
}

// Sentinel errors shared by the repository, the service and the admin API
// error mapper.
var (
	// ErrBindingNotFound is returned when no active class-A device row
	// matches the device id.
	ErrBindingNotFound = errors.New("mcpbinding: binding not found")
	// ErrNotClassA is returned when the target device is not a class-A
	// native MCP device (doc/07 2.1: only A supports MCP binding).
	ErrNotClassA = errors.New("mcpbinding: device is not class A")
	// ErrStateConflict is returned when the requested transition is not
	// legal for the current binding_status (e.g. completing a binding
	// that is not BINDING).
	ErrStateConflict = errors.New("mcpbinding: invalid binding state transition")
	// ErrTokenInvalid marks a pairing token that does not match the
	// stored hash or was already consumed (anti-replay, doc/07 step 4).
	ErrTokenInvalid = errors.New("mcpbinding: binding token invalid or already consumed")
	// ErrTokenExpired marks a pairing token that matched but arrived
	// after binding_expire_at.
	ErrTokenExpired = errors.New("mcpbinding: binding token expired")
	// ErrEndpointInvalid marks a malformed mcp_endpoint (scheme/host
	// validation, SEC-20 spirit).
	ErrEndpointInvalid = errors.New("mcpbinding: mcp endpoint invalid")
	// ErrOAuthFailed wraps discovery/token-endpoint failures during the
	// OAuth 2.1 client credentials binding (doc/07 step 3).
	ErrOAuthFailed = errors.New("mcpbinding: oauth binding failed")
)

// Binding is the binding aggregate mapped onto the class-A adc_devices
// row (design/32 3.5 plus migration 0003 columns).
type Binding struct {
	DeviceID    string
	TenantID    string
	DeviceClass string // 'A' only; B is refused with ErrNotClassA
	AuthType    string

	McpEndpoint string
	Status      Status

	// BindingTokenHash is the SHA-256 of the pairing token; cleared on
	// consumption (BOUND) and revoke. The plaintext is never stored.
	BindingTokenHash string
	BindingExpireAt  *time.Time

	// OAuthClientSecretEnc is the KEK-encrypted platform client secret
	// (auth.EncryptSecret, NFR-004). It is decrypted only in memory for
	// token requests and never returned by any handler.
	OAuthClientID        string
	OAuthClientSecretEnc string

	// TokenEndpoint and ResourceIdentifier are the discovered AS token
	// endpoint (RFC 8414) and the RFC8707 resource parameter persisted
	// at BOUND time (doc/07 step 2/3).
	TokenEndpoint      string
	ResourceIdentifier string

	BoundAt   *time.Time
	RevokedAt *time.Time
	UpdatedAt time.Time
}

// GenerateBindingToken returns a fresh 256-bit pairing token, hex
// encoded. The plaintext is returned exactly once (NFR-004); only
// HashBindingToken(output) may be persisted.
func GenerateBindingToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("mcpbinding: generate binding token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// HashBindingToken returns the persisted form of a pairing token. A
// one-way SHA-256 is sufficient because the token carries 256 bits of
// entropy (same reasoning as approval.HashSecret, SEC-13 spirit).
func HashBindingToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
