package agentauth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"time"
)

// Sentinel validation errors; callers distinguish them with errors.Is.
// They map to HTTP 401 code 10002 at the middleware boundary (design/33
// 1.5); the middleware intentionally does not reveal which one occurred.
var (
	// ErrKeyNotFound: unknown key id or malformed token.
	ErrKeyNotFound = errors.New("agentauth: key not found")
	// ErrKeyDisabled: key revoked or the bound tenant is not ACTIVE.
	ErrKeyDisabled = errors.New("agentauth: key disabled")
	// ErrKeyExpired: key is past expires_at.
	ErrKeyExpired = errors.New("agentauth: key expired")
	// ErrBadSecret: secret hash mismatch.
	ErrBadSecret = errors.New("agentauth: bad secret")
)

// Principal is the authenticated identity attached to the request context
// by the middleware. TenantID comes from the key record, never from request
// headers (SEC-02 / GAP-13).
type Principal struct {
	KeyID    string   // key id part of the token
	TenantID string   // tenant bound to the key
	Scopes   []string // allowed tool ids (ALLOW_LIST) or denied ones (DENY_LIST)
	AgentID  string   // agent application id, real subject for audit (SEC-21)
}

// ApiKeyValidator validates an agent API key token (I4, design/31 3.2.2).
type ApiKeyValidator interface {
	Validate(ctx context.Context, token string) (*Principal, error)
}

// Validator implements ApiKeyValidator against a KeyStore.
type Validator struct {
	store KeyStore
	now   func() time.Time
}

// NewValidator returns a Validator backed by store.
func NewValidator(store KeyStore) *Validator {
	return &Validator{store: store, now: time.Now}
}

// Validate checks the token and returns the bound principal. Validation
// order: parse -> key id lookup -> enabled -> expiry -> constant-time
// secret compare -> scope extraction. The design is fail-closed: any
// lookup or parse problem rejects the request.
func (v *Validator) Validate(ctx context.Context, token string) (*Principal, error) {
	keyID, secret, err := parseToken(token)
	if err != nil {
		return nil, fmt.Errorf("%w: malformed token", ErrKeyNotFound)
	}

	key, err := v.store.GetByKeyID(ctx, keyID)
	if err != nil {
		// A missing key must not be distinguishable by timing from a
		// wrong secret, so normalize the cost of the failed branch.
		if errors.Is(err, ErrKeyNotFound) {
			v.compareDummy(secret)
		}
		return nil, err
	}
	if key == nil {
		v.compareDummy(secret)
		return nil, ErrKeyNotFound
	}
	if !key.Enabled {
		v.compareDummy(secret)
		return nil, ErrKeyDisabled
	}
	if !key.ExpiresAt.IsZero() && !v.now().Before(key.ExpiresAt) {
		v.compareDummy(secret)
		return nil, ErrKeyExpired
	}

	sum := sha256.Sum256([]byte(secret))
	if subtle.ConstantTimeCompare(sum[:], key.SecretHash) != 1 {
		return nil, ErrBadSecret
	}

	return &Principal{
		KeyID:    key.KeyID,
		TenantID: key.TenantID,
		Scopes:   append([]string(nil), key.Scopes...),
		AgentID:  key.AgentID,
	}, nil
}

// dummySecretHash is a fixed hash compared against the presented secret on
// branches that fail before the real secret check, so failure timing does
// not reveal whether the key exists, is disabled or is expired.
var dummySecretHash = sha256.Sum256([]byte("adc-dummy-secret-for-timing-normalization"))

func (v *Validator) compareDummy(secret string) {
	sum := sha256.Sum256([]byte(secret))
	subtle.ConstantTimeCompare(sum[:], dummySecretHash[:])
}
