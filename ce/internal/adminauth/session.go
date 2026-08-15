package adminauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// Session is one authenticated admin session. The plaintext token exists
// only in the caller's cookie/bearer header; server-side storage keeps just
// the SHA-256 token hash as the Valkey key (design/33 1.2, NFR-004).
type Session struct {
	TokenHash string    // hex SHA-256 of the bearer token, the Valkey key suffix
	UserID    string    // adc_users.id
	TenantID  string    // tenant bound to the user; authoritative tenant context (SEC-02)
	Roles     []string  // design/33 3.1.18 role names
	ExpiresAt time.Time // absolute expiry; enforced by Valkey TTL and re-checked on Get
}

// ErrSessionNotFound is returned by SessionStore.Get for an unknown or
// expired token hash.
var ErrSessionNotFound = errors.New("adminauth: session not found")

// SessionTTL is the default admin session lifetime (design/80 B-01: 24h).
const SessionTTL = 24 * time.Hour

// SessionStore persists admin sessions. Implementations must store only the
// token hash, never the plaintext token (NFR-004).
type SessionStore interface {
	// Create writes the session under adc:session:{TokenHash} with the
	// given TTL; ExpiresAt should equal now+ttl on the caller side.
	Create(ctx context.Context, s *Session, ttl time.Duration) error
	// Get loads the session for a token hash; unknown or expired hashes
	// return ErrSessionNotFound.
	Get(ctx context.Context, tokenHash string) (*Session, error)
	// Delete removes the session. Deleting a missing session is not an
	// error: logout is idempotent.
	Delete(ctx context.Context, tokenHash string) error
}

// NewToken returns a random session token: 32 bytes from crypto/rand,
// base64url-encoded (43 chars, 256 bits of entropy, URL/cookie-safe).
func NewToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("adminauth: generate session token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// HashToken returns the hex SHA-256 of a plaintext token. It is the only
// form persisted and the only form used for session lookups.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// valkeyCmd is the minimal go-redis surface used by ValkeySessionStore; it
// is satisfied by *redis.Client and by the in-memory fake used in tests.
type valkeyCmd interface {
	Set(ctx context.Context, key string, value interface{}, expiration time.Duration) *redis.StatusCmd
	Get(ctx context.Context, key string) *redis.StringCmd
	Del(ctx context.Context, keys ...string) *redis.IntCmd
}

// sessionKeyPrefix follows the design/31 1.3.7 Valkey key layout:
// adc:session:{sha256(token)}.
const sessionKeyPrefix = "adc:session:"

func sessionKey(tokenHash string) string {
	return sessionKeyPrefix + tokenHash
}

// sessionValue is the JSON wire form stored in Valkey. TokenHash is not
// stored; it is reconstructed from the lookup key on Get.
type sessionValue struct {
	UserID    string    `json:"user_id"`
	TenantID  string    `json:"tenant_id"`
	Roles     []string  `json:"roles"`
	ExpiresAt time.Time `json:"expires_at"`
}

// ValkeySessionStore implements SessionStore on Valkey via go-redis/v9.
type ValkeySessionStore struct {
	rdb valkeyCmd
}

// NewValkeySessionStore builds a session store backed by the given client.
func NewValkeySessionStore(rdb valkeyCmd) *ValkeySessionStore {
	return &ValkeySessionStore{rdb: rdb}
}

// Create writes the session JSON under adc:session:{hash} with the TTL.
func (s *ValkeySessionStore) Create(ctx context.Context, sess *Session, ttl time.Duration) error {
	if sess == nil || sess.TokenHash == "" {
		return errors.New("adminauth: cannot create session without a token hash")
	}
	raw, err := json.Marshal(sessionValue{
		UserID:    sess.UserID,
		TenantID:  sess.TenantID,
		Roles:     sess.Roles,
		ExpiresAt: sess.ExpiresAt,
	})
	if err != nil {
		return fmt.Errorf("adminauth: encode session: %w", err)
	}
	if err := s.rdb.Set(ctx, sessionKey(sess.TokenHash), string(raw), ttl).Err(); err != nil {
		return fmt.Errorf("adminauth: store session: %w", err)
	}
	return nil
}

// Get loads the session JSON and maps redis.Nil to ErrSessionNotFound. The
// ExpiresAt field is re-checked as a second line of defense behind the
// Valkey TTL (fail closed).
func (s *ValkeySessionStore) Get(ctx context.Context, tokenHash string) (*Session, error) {
	raw, err := s.rdb.Get(ctx, sessionKey(tokenHash)).Result()
	if errors.Is(err, redis.Nil) {
		return nil, ErrSessionNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("adminauth: load session: %w", err)
	}
	var v sessionValue
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return nil, fmt.Errorf("adminauth: decode session: %w", err)
	}
	if !v.ExpiresAt.IsZero() && !time.Now().Before(v.ExpiresAt) {
		return nil, ErrSessionNotFound
	}
	return &Session{
		TokenHash: tokenHash,
		UserID:    v.UserID,
		TenantID:  v.TenantID,
		Roles:     v.Roles,
		ExpiresAt: v.ExpiresAt,
	}, nil
}

// Delete removes the session key. Deleting a missing key is not an error.
func (s *ValkeySessionStore) Delete(ctx context.Context, tokenHash string) error {
	if err := s.rdb.Del(ctx, sessionKey(tokenHash)).Err(); err != nil {
		return fmt.Errorf("adminauth: delete session: %w", err)
	}
	return nil
}
