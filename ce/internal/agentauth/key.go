// Package agentauth implements Agent API key authentication (SEC-02):
// tokens of the form adc_<keyID>_<secret> are validated against hashed
// credentials stored in PostgreSQL. The tenant context comes exclusively
// from the key record, never from request headers (SEC-02 / GAP-13).
package agentauth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Token layout (design/33 1.2): "adc_<keyID>_<secret>", e.g.
// "adc_3c4d5e6f_xH7kP2mQ9vL4nB8tR6wY". keyID is an 8-char hex id matching
// adc_agent_api_keys.key_prefix; secret is a random high-entropy value
// returned to the caller exactly once at issuance (FR-009). Only hashes
// are persisted, never the plaintext (NFR-004).
const tokenPrefix = "adc_"

var (
	// keyIDRe enforces the documented key id charset/length (SEC-20 input
	// validation). Uppercase hex is rejected so lookups are unambiguous.
	keyIDRe = regexp.MustCompile(`^[a-f0-9]{8}$`)
	// secretRe enforces the secret charset; '_' is forbidden so parsing
	// "adc_<keyID>_<secret>" at the first underscore is unambiguous.
	secretRe = regexp.MustCompile(`^[A-Za-z0-9]{16,128}$`)
)

// ScopeMode mirrors adc_agent_api_keys.scope_mode (design/32 3.7).
type ScopeMode string

const (
	ScopeModeAllowList ScopeMode = "ALLOW_LIST"
	ScopeModeDenyList  ScopeMode = "DENY_LIST"
)

// AgentKey is the credential record of one Agent API key as stored in
// adc_agent_api_keys. Only hashes are kept: the key id prefix hash and the
// secret hash; the plaintext secret exists only at issuance time.
type AgentKey struct {
	KeyID      string    // key id part of the token, e.g. "3c4d5e6f"
	TenantID   string    // tenant bound to the key; authoritative tenant context (SEC-02)
	AgentID    string    // agent application id, real subject for audit (SEC-21)
	ScopeMode  ScopeMode // ALLOW_LIST or DENY_LIST
	Scopes     []string  // tool ids allowed (or denied, per ScopeMode)
	ExpiresAt  time.Time // zero value means never expires
	Enabled    bool      // false when revoked or the tenant is suspended
	SecretHash []byte    // raw SHA-256 of the secret (32 bytes)
}

// KeyStore loads agent keys by key id. The PG implementation resolves the
// key id to its hash and hits the unique index (design/32 4), never a
// full-table scan. Implementations must never return plaintext secrets.
type KeyStore interface {
	GetByKeyID(ctx context.Context, keyID string) (*AgentKey, error)
}

// HashKeyID returns the hex SHA-256 of the key id prefix ("adc_<keyID>").
// This is the canonical key id hash stored in adc_agent_api_keys.key_hash;
// the Admin API must use the same function when issuing keys.
func HashKeyID(keyID string) string {
	sum := sha256.Sum256([]byte(tokenPrefix + keyID))
	return hex.EncodeToString(sum[:])
}

// HashSecret returns the hex SHA-256 of the secret part. SHA-256 is
// sufficient because secrets are high-entropy random values (rainbow-table
// resistant by construction); it keeps the hot auth path cheap (NFR-003).
// The plaintext secret is returned only once at issuance.
func HashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// parseToken splits "adc_<keyID>_<secret>" into its parts, enforcing the
// documented charset and lengths (SEC-20). Malformed tokens return an
// error without echoing their content.
func parseToken(token string) (keyID, secret string, err error) {
	if len(token) <= len(tokenPrefix) || !strings.HasPrefix(token, tokenPrefix) {
		return "", "", errors.New("missing adc_ prefix")
	}
	rest := token[len(tokenPrefix):]
	i := strings.IndexByte(rest, '_')
	if i < 0 {
		return "", "", errors.New("missing key/secret separator")
	}
	keyID, secret = rest[:i], rest[i+1:]
	if !keyIDRe.MatchString(keyID) {
		return "", "", errors.New("malformed key id")
	}
	if !secretRe.MatchString(secret) {
		return "", "", errors.New("malformed secret")
	}
	return keyID, secret, nil
}

// PgxPool is the minimal pgx surface needed by PGKeyStore.
type PgxPool interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// PGKeyStore loads AgentKey records from PostgreSQL.
//
// Expected schema (migration task 1.1 of phase0-security-hardening):
//
//	CREATE TABLE adc_agent_api_keys (
//	    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
//	    tenant_id    UUID NOT NULL REFERENCES adc_tenants(id),
//	    name         VARCHAR(255) NOT NULL,
//	    agent_id     VARCHAR(64),
//	    key_prefix   VARCHAR(12) NOT NULL,   -- display prefix, e.g. adc_3c4d5e6f
//	    key_hash     TEXT NOT NULL,          -- SHA-256 hex of key_prefix (HashKeyID)
//	    secret_hash  TEXT NOT NULL,          -- SHA-256 hex of secret (HashSecret)
//	    scope_mode   VARCHAR(16) NOT NULL DEFAULT 'ALLOW_LIST'
//	                 CHECK (scope_mode IN ('ALLOW_LIST','DENY_LIST')),
//	    scopes       JSONB NOT NULL DEFAULT '[]',
//	    expires_at   TIMESTAMPTZ,            -- NULL = never expires
//	    revoked_at   TIMESTAMPTZ,            -- NULL = active
//	    ...
//	);
//	CREATE UNIQUE INDEX uq_api_keys_hash ON adc_agent_api_keys (key_hash);
//
// Enabled maps to (revoked_at IS NULL AND tenant.status = 'ACTIVE'):
// suspension of the tenant disables all of its keys (design/31 3.2.3).
type PGKeyStore struct {
	pool PgxPool
}

// NewPGKeyStore returns a key store backed by the given pool. Both
// *db.Pool and *pgxpool.Pool satisfy the PgxPool interface.
func NewPGKeyStore(pool PgxPool) *PGKeyStore {
	return &PGKeyStore{pool: pool}
}

const getByKeyIDSQL = `
SELECT k.id::text, k.tenant_id::text, k.agent_id, k.secret_hash, k.scope_mode,
       k.scopes, k.expires_at, k.revoked_at, t.status AS tenant_status
  FROM adc_agent_api_keys k
  JOIN adc_tenants t ON t.id = k.tenant_id
 WHERE k.key_hash = $1`

// GetByKeyID loads the key whose key id prefix hashes to keyID.
func (s *PGKeyStore) GetByKeyID(ctx context.Context, keyID string) (*AgentKey, error) {
	var (
		k           AgentKey
		secretHex   string
		scopeMode   string
		scopesJSON  []byte
		expiresAt   *time.Time
		revokedAt   *time.Time
		tenantState string
	)
	err := s.pool.QueryRow(ctx, getByKeyIDSQL, HashKeyID(keyID)).Scan(
		new(string), &k.TenantID, &k.AgentID, &secretHex, &scopeMode,
		&scopesJSON, &expiresAt, &revokedAt, &tenantState,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: key id %s", ErrKeyNotFound, keyID)
	}
	if err != nil {
		return nil, fmt.Errorf("agentauth: load key %s: %w", keyID, err)
	}

	k.KeyID = keyID
	k.SecretHash, err = hex.DecodeString(secretHex)
	if err != nil {
		return nil, fmt.Errorf("agentauth: decode secret hash for key %s: %w", keyID, err)
	}
	k.ScopeMode = ScopeMode(scopeMode)
	if err := json.Unmarshal(scopesJSON, &k.Scopes); err != nil {
		return nil, fmt.Errorf("agentauth: parse scopes for key %s: %w", keyID, err)
	}
	if k.Scopes == nil {
		k.Scopes = []string{}
	}
	if expiresAt != nil {
		k.ExpiresAt = *expiresAt
	}
	k.Enabled = revokedAt == nil && tenantState == "ACTIVE"
	return &k, nil
}
