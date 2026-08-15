package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	sdkproto "adc.dev/core-sdk/protocol"
)

// Sentinel errors of the device handshake (SEC-03). All are matched with
// errors.Is; the HTTP layer maps them to design/33 codes:
//
//	ErrInvalidTimestamp    -> 401 (clock skew, 11001 family)
//	ErrNonceReused         -> 401 code 11012
//	ErrBadSignature        -> 401 code 11011
//	ErrDeviceDisabled      -> 403 code 11003 / 401 code 11004
//	ErrCredentialNotFound  -> 404 code 11001
var (
	ErrInvalidID          = errors.New("auth: invalid device id or nonce format")
	ErrInvalidTimestamp   = errors.New("auth: timestamp outside allowed window")
	ErrNonceReused        = errors.New("auth: nonce already consumed (replay)")
	ErrNonceStore         = errors.New("auth: nonce store unavailable (fail closed)")
	ErrCredentialNotFound = errors.New("auth: device credential not found")
	ErrDeviceDisabled     = errors.New("auth: device disabled (frozen or retired)")
	ErrUnsupportedAuthType = errors.New("auth: auth_type does not support hmac signing")
	ErrBadSignature       = errors.New("auth: signature mismatch")
)

// Retryable marks transient errors that may succeed on retry (LLD 1.3.4).
type Retryable interface{ Retryable() bool }

// nonceStoreError reports a Valkey failure. It satisfies Retryable so the
// caller may retry, and Is(ErrNonceStore) so errors.Is works on the sentinel.
type nonceStoreError struct{ err error }

func (e *nonceStoreError) Error() string { return ErrNonceStore.Error() + ": " + e.err.Error() }
func (e *nonceStoreError) Unwrap() error { return e.err }
func (e *nonceStoreError) Is(target error) bool {
	return target == ErrNonceStore || errors.Is(e.err, target)
}
func (e *nonceStoreError) Retryable() bool { return true }

// NonceStore consumes a nonce exactly once (one-time semantics, SEC-03).
// Consume returns true only if the nonce was not seen before and is now
// held until ttl elapses.
type NonceStore interface {
	Consume(ctx context.Context, nonceKey string, ttl time.Duration) (bool, error)
}

// redisNonceStore implements NonceStore with SETNX on a go-redis client
// (Valkey is wire-compatible with redis.Options).
type redisNonceStore struct {
	client *redis.Client
}

func (s *redisNonceStore) Consume(ctx context.Context, nonceKey string, ttl time.Duration) (bool, error) {
	ok, err := s.client.SetNX(ctx, nonceKey, "1", ttl).Result()
	if err != nil {
		return false, err
	}
	return ok, nil
}

// NewValkeyNonceStore builds a NonceStore from redis.Options
// (Valkey-compatible, ADR-13).
func NewValkeyNonceStore(opt *redis.Options) NonceStore {
	return &redisNonceStore{client: redis.NewClient(opt)}
}

const (
	// nonceKeyPrefix follows the LLD Valkey key space: adc:nonce:{sha256(nonce)}.
	// The nonce is hashed before key building because SEC-20 forbids
	// unvalidated input inside dynamic keys.
	nonceKeyPrefix = "adc:nonce:"
	// nonceTTL keeps a consumed nonce for 10 minutes (600s, LLD table),
	// twice the authentication window.
	nonceTTL = 10 * time.Minute
)

// nonceKey returns "adc:nonce:" + hex(sha256(nonce)).
func nonceKey(nonce string) string {
	sum := sha256.Sum256([]byte(nonce))
	return nonceKeyPrefix + hex.EncodeToString(sum[:])
}

// deviceCodePattern matches the design/33 1.1 whitelist: letters, digits,
// hyphen, underscore, length 1-128 (SEC-20 format validation).
var deviceCodePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// noncePattern matches design/33 1.3: random string of 8-32 bytes, hex
// encoded (16-64 hex characters).
var noncePattern = regexp.MustCompile(`^[0-9a-fA-F]{16,64}$`)

// Verifier authenticates device handshakes (I1 DeviceAuthenticator, SEC-03).
type Verifier struct {
	repo   CredentialRepo
	nonces NonceStore
	now    func() time.Time
}

// Option configures a Verifier.
type Option func(*Verifier)

// WithNow overrides the clock source (testability).
func WithNow(f func() time.Time) Option {
	return func(v *Verifier) { v.now = f }
}

// NewVerifier wires the credential store and the nonce store into a Verifier.
func NewVerifier(repo CredentialRepo, nonces NonceStore, opts ...Option) *Verifier {
	v := &Verifier{repo: repo, nonces: nonces, now: time.Now}
	for _, o := range opts {
		o(v)
	}
	return v
}

// Verify authenticates a device handshake (SEC-03, LLD 3.1.3).
// Step order is deliberate: format -> timestamp window -> one-time nonce ->
// credential load -> constant-time signature check. The nonce is consumed
// before the credential is loaded so an attacker cannot probe the credential
// store by replaying captured nonces.
//
// timestamp is Unix seconds; the window is AuthTimeWindowSec (300s) in both
// directions. The signature is hex(HMAC-SHA256(secret, deviceCode + "\n" +
// timestamp + "\n" + nonce)) per design/33 1.3 and LLD 3.1.2.
//
// Any infra failure (Valkey down, credential load error) fails closed:
// the handshake is rejected instead of degraded.
func (v *Verifier) Verify(ctx context.Context, deviceCode string, timestamp int64, nonce, signature string) (*DeviceCredential, error) {
	if !deviceCodePattern.MatchString(deviceCode) {
		return nil, fmt.Errorf("%w: device_code %q", ErrInvalidID, deviceCode)
	}
	if !noncePattern.MatchString(nonce) {
		return nil, fmt.Errorf("%w: nonce %q", ErrInvalidID, nonce)
	}
	if timestamp <= 0 {
		return nil, fmt.Errorf("%w: timestamp %d", ErrInvalidTimestamp, timestamp)
	}

	now := v.now().Unix()
	if delta := now - timestamp; delta < -sdkproto.AuthTimeWindowSec || delta > sdkproto.AuthTimeWindowSec {
		return nil, fmt.Errorf("%w: ts=%d now=%d window=%d", ErrInvalidTimestamp, timestamp, now, sdkproto.AuthTimeWindowSec)
	}

	consumed, err := v.nonces.Consume(ctx, nonceKey(nonce), nonceTTL)
	if err != nil {
		// Valkey unavailable: fail closed (LLD 3.1.3).
		return nil, fmt.Errorf("%w", &nonceStoreError{err: err})
	}
	if !consumed {
		return nil, fmt.Errorf("%w: nonce %q for device %q", ErrNonceReused, nonce, deviceCode)
	}

	cred, err := v.repo.Load(ctx, deviceCode)
	if err != nil {
		return nil, fmt.Errorf("auth: load credential for device_code %q: %w", deviceCode, err)
	}
	if !cred.Enabled {
		return nil, fmt.Errorf("%w: device_code %q", ErrDeviceDisabled, deviceCode)
	}
	if cred.AuthType != AuthTypeHMAC {
		return nil, fmt.Errorf("%w: device_code %q auth_type=%s", ErrUnsupportedAuthType, deviceCode, cred.AuthType)
	}

	want := computeHMAC(cred.SecretHash, deviceCode, timestamp, nonce)
	got, err := hex.DecodeString(signature)
	if err != nil || subtle.ConstantTimeCompare(want, got) != 1 {
		// Constant-time comparison defeats timing side channels (LLD 3.1.3).
		return nil, fmt.Errorf("%w: device_code %q", ErrBadSignature, deviceCode)
	}
	return cred, nil
}

// computeHMAC returns HMAC-SHA256(secret, deviceCode + "\n" + ts + "\n" + nonce).
func computeHMAC(secret, deviceCode string, ts int64, nonce string) []byte {
	mac := hmac.New(sha256.New, []byte(secret))
	io.WriteString(mac, deviceCode+"\n"+strconv.FormatInt(ts, 10)+"\n"+nonce)
	return mac.Sum(nil)
}

// hexEncodeHMAC is a helper for callers and tests that need the wire form.
func hexEncodeHMAC(secret, deviceCode string, ts int64, nonce string) string {
	return strings.ToLower(hex.EncodeToString(computeHMAC(secret, deviceCode, ts, nonce)))
}
