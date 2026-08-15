// Package auth implements the SEC-03 device handshake authentication:
// HMAC-SHA256 signature over (deviceCode, timestamp, nonce), a +/- 300 second
// clock-skew window, and one-time nonce consumption in Valkey.
// Design baseline: LLD 3.1.2 / 3.1.3 (interface I1 DeviceAuthenticator).
package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	sdkproto "adc.dev/core-sdk/protocol"
)

// fixedNow pins the clock so window-boundary cases are deterministic instead
// of racing against wall time; WithNow is the injection seam.
var fixedNow = time.Unix(1_750_000_000, 0)

const (
	testDeviceCode = "device-01"
	// testSecret is a hex-encoded 32-byte secret (64 hex chars).
	testSecret = "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"
	// testNonce is a 16-byte nonce, hex encoded (32 chars).
	testNonce = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// fakeNonceStore is an in-memory NonceStore with SETNX semantics.
//
// Choice of interface mock over miniredis: miniredis is a test-only
// third-party dependency outside the allowed set (stdlib + pgx/v5 +
// go-redis/v9), and NonceStore is exactly the seam the LLD's nonce.Consume
// implies. Unit tests must run without a live Valkey; the go-redis-backed
// implementation is exercised in integration tests against a real Valkey.
type fakeNonceStore struct {
	mu   sync.Mutex
	seen map[string]struct{}
	err  error // injectable failure to exercise fail-closed behavior
}

func (f *fakeNonceStore) Consume(_ context.Context, key string, _ time.Duration) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return false, f.err
	}
	if _, ok := f.seen[key]; ok {
		return false, nil
	}
	f.seen[key] = struct{}{}
	return true, nil
}

// fakeCredentialRepo serves a fixed credential; a nil cred means "not found".
type fakeCredentialRepo struct {
	cred *DeviceCredential
}

func (f *fakeCredentialRepo) Load(_ context.Context, deviceCode string) (*DeviceCredential, error) {
	if f.cred == nil || f.cred.DeviceCode != deviceCode {
		return nil, ErrCredentialNotFound
	}
	return f.cred, nil
}

func TestVerify(t *testing.T) {
	ctx := context.Background()
	validTS := fixedNow.Unix()

	baseCred := &DeviceCredential{
		DeviceID:   "d-1",
		DeviceCode: testDeviceCode,
		TenantID:   "tenant-1",
		SecretHash: testSecret,
		AuthType:   AuthTypeHMAC,
		Enabled:    true,
	}
	disabledCred := *baseCred
	disabledCred.Enabled = false
	tokenCred := *baseCred
	tokenCred.AuthType = AuthTypeToken

	cases := []struct {
		name       string
		cred       *DeviceCredential
		deviceCode string
		ts         int64
		nonce      string
		sig        func(ts int64, nonce string) string
		storeErr   error
		replay     bool
		wantErr    error
	}{
		{
			name:       "valid signature passes",
			cred:       baseCred,
			deviceCode: testDeviceCode,
			ts:         validTS,
			nonce:      testNonce,
			sig:        func(ts int64, nonce string) string { return hexEncodeHMAC(testSecret, testDeviceCode, ts, nonce) },
		},
		{
			name:       "replay of same nonce rejected",
			cred:       baseCred,
			deviceCode: testDeviceCode,
			ts:         validTS,
			nonce:      testNonce,
			sig:        func(ts int64, nonce string) string { return hexEncodeHMAC(testSecret, testDeviceCode, ts, nonce) },
			replay:     true,
			wantErr:    ErrNonceReused,
		},
		{
			name:       "timestamp too old rejected",
			cred:       baseCred,
			deviceCode: testDeviceCode,
			ts:         validTS - sdkproto.AuthTimeWindowSec - 1,
			nonce:      strings.Repeat("c", 32),
			sig:        func(ts int64, nonce string) string { return hexEncodeHMAC(testSecret, testDeviceCode, ts, nonce) },
			wantErr:    ErrInvalidTimestamp,
		},
		{
			name:       "timestamp in future rejected",
			cred:       baseCred,
			deviceCode: testDeviceCode,
			ts:         validTS + sdkproto.AuthTimeWindowSec + 1,
			nonce:      strings.Repeat("d", 32),
			sig:        func(ts int64, nonce string) string { return hexEncodeHMAC(testSecret, testDeviceCode, ts, nonce) },
			wantErr:    ErrInvalidTimestamp,
		},
		{
			name:       "timestamp at window edge accepted",
			cred:       baseCred,
			deviceCode: testDeviceCode,
			ts:         validTS + sdkproto.AuthTimeWindowSec,
			nonce:      strings.Repeat("e", 32),
			sig:        func(ts int64, nonce string) string { return hexEncodeHMAC(testSecret, testDeviceCode, ts, nonce) },
		},
		{
			name:       "tampered signature rejected",
			cred:       baseCred,
			deviceCode: testDeviceCode,
			ts:         validTS,
			nonce:      strings.Repeat("f", 32),
			sig: func(ts int64, nonce string) string {
				b := []byte(hexEncodeHMAC(testSecret, testDeviceCode, ts, nonce))
				b[0] ^= 0x01 // flip one bit of the first hex char
				return string(b)
			},
			wantErr: ErrBadSignature,
		},
		{
			name:       "non-hex signature rejected",
			cred:       baseCred,
			deviceCode: testDeviceCode,
			ts:         validTS,
			nonce:      strings.Repeat("a1", 16),
			sig:        func(ts int64, nonce string) string { return strings.Repeat("z", 64) },
			wantErr:    ErrBadSignature,
		},
		{
			name:       "disabled device rejected",
			cred:       &disabledCred,
			deviceCode: testDeviceCode,
			ts:         validTS,
			nonce:      strings.Repeat("c0", 16),
			sig:        func(ts int64, nonce string) string { return hexEncodeHMAC(testSecret, testDeviceCode, ts, nonce) },
			wantErr:    ErrDeviceDisabled,
		},
		{
			name:       "unknown device rejected",
			cred:       nil,
			deviceCode: testDeviceCode,
			ts:         validTS,
			nonce:      strings.Repeat("d0", 16),
			sig:        func(ts int64, nonce string) string { return hexEncodeHMAC(testSecret, testDeviceCode, ts, nonce) },
			wantErr:    ErrCredentialNotFound,
		},
		{
			name:       "non-hmac auth type rejected",
			cred:       &tokenCred,
			deviceCode: testDeviceCode,
			ts:         validTS,
			nonce:      strings.Repeat("e0", 16),
			sig:        func(ts int64, nonce string) string { return hexEncodeHMAC(testSecret, testDeviceCode, ts, nonce) },
			wantErr:    ErrUnsupportedAuthType,
		},
		{
			name:       "malformed device code rejected",
			cred:       baseCred,
			deviceCode: "bad code!",
			ts:         validTS,
			nonce:      strings.Repeat("f0", 16),
			sig:        func(ts int64, nonce string) string { return hexEncodeHMAC(testSecret, testDeviceCode, ts, nonce) },
			wantErr:    ErrInvalidID,
		},
		{
			name:       "malformed nonce rejected",
			cred:       baseCred,
			deviceCode: testDeviceCode,
			ts:         validTS,
			nonce:      "abc",
			sig:        func(ts int64, nonce string) string { return hexEncodeHMAC(testSecret, testDeviceCode, ts, nonce) },
			wantErr:    ErrInvalidID,
		},
		{
			name:       "nonce store failure fails closed",
			cred:       baseCred,
			deviceCode: testDeviceCode,
			ts:         validTS,
			nonce:      strings.Repeat("a2", 16),
			sig:        func(ts int64, nonce string) string { return hexEncodeHMAC(testSecret, testDeviceCode, ts, nonce) },
			storeErr:   errors.New("valkey unreachable"),
			wantErr:    ErrNonceStore,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store := &fakeNonceStore{seen: make(map[string]struct{}), err: c.storeErr}
			repo := &fakeCredentialRepo{cred: c.cred}
			v := NewVerifier(repo, store, WithNow(func() time.Time { return fixedNow }))

			sig := c.sig(c.ts, c.nonce)
			got, err := v.Verify(ctx, c.deviceCode, c.ts, c.nonce, sig)
			if c.replay {
				// First attempt with a fresh nonce must pass; the second
				// attempt reuses the same nonce and must be rejected.
				if err != nil {
					t.Fatalf("first attempt should pass: %v", err)
				}
				got, err = v.Verify(ctx, c.deviceCode, c.ts, c.nonce, sig)
			}
			if !errors.Is(err, c.wantErr) {
				t.Fatalf("Verify() error = %v, want errors.Is(err, %v)", err, c.wantErr)
			}
			if c.wantErr == nil {
				if got == nil || got.DeviceCode != c.deviceCode || got.TenantID != baseCred.TenantID {
					t.Fatalf("Verify() returned unexpected credential: %+v", got)
				}
			}
		})
	}
}

func TestNonceStoreErrorIsRetryable(t *testing.T) {
	store := &fakeNonceStore{seen: make(map[string]struct{}), err: errors.New("valkey unreachable")}
	v := NewVerifier(&fakeCredentialRepo{cred: &DeviceCredential{
		DeviceID: "d-1", DeviceCode: testDeviceCode, TenantID: "tenant-1",
		SecretHash: testSecret, AuthType: AuthTypeHMAC, Enabled: true,
	}}, store, WithNow(func() time.Time { return fixedNow }))

	_, err := v.Verify(context.Background(), testDeviceCode, fixedNow.Unix(), testNonce,
		hexEncodeHMAC(testSecret, testDeviceCode, fixedNow.Unix(), testNonce))
	if !errors.Is(err, ErrNonceStore) {
		t.Fatalf("want ErrNonceStore, got %v", err)
	}
	var r interface{ Retryable() bool }
	if !errors.As(err, &r) || !r.Retryable() {
		t.Fatalf("nonce store error must be marked Retryable, got %v", err)
	}
}

func TestNonceKeyHashesNonce(t *testing.T) {
	// LLD key space: adc:nonce:{sha256(nonce)} — never raw input in keys (SEC-20).
	sum := sha256.Sum256([]byte(testNonce))
	want := "adc:nonce:" + hex.EncodeToString(sum[:])
	if got := nonceKey(testNonce); got != want {
		t.Fatalf("nonceKey() = %q, want %q", got, want)
	}
}

func TestHashSecretRoundTrip(t *testing.T) {
	secret, err := GenerateSecret()
	if err != nil {
		t.Fatalf("GenerateSecret: %v", err)
	}
	stored, err := HashSecret(secret)
	if err != nil {
		t.Fatalf("HashSecret: %v", err)
	}
	if strings.Contains(stored, secret) {
		t.Fatal("stored form must not contain the plaintext secret")
	}
	ok, err := VerifySecret(secret, stored)
	if err != nil || !ok {
		t.Fatalf("VerifySecret(correct) = %v, %v; want true, nil", ok, err)
	}
	ok, err = VerifySecret(secret+"ff", stored)
	if err != nil || ok {
		t.Fatalf("VerifySecret(wrong) = %v, %v; want false, nil", ok, err)
	}
	if _, err := VerifySecret(secret, "not-a-hash"); err == nil {
		t.Fatal("VerifySecret(malformed stored form) must error")
	}
}

func TestEncryptDecryptSecretRoundTrip(t *testing.T) {
	kek := []byte(strings.Repeat("k", 32))
	secret := testSecret

	blob, err := EncryptSecret(secret, kek)
	if err != nil {
		t.Fatalf("EncryptSecret: %v", err)
	}
	got, err := DecryptSecret(blob, kek)
	if err != nil || got != secret {
		t.Fatalf("DecryptSecret = %q, %v; want %q, nil", got, err, secret)
	}

	// Wrong KEK must fail closed (GCM auth failure).
	if _, err := DecryptSecret(blob, []byte(strings.Repeat("x", 32))); err == nil {
		t.Fatal("DecryptSecret with wrong KEK must fail")
	}
	// Missing KEK must fail closed.
	if _, err := DecryptSecret(blob, nil); err == nil {
		t.Fatal("DecryptSecret with nil KEK must fail")
	}
	// Tampered blob must fail closed.
	tampered := blob[:len(blob)-1]
	if tampered == blob {
		tampered = blob[:len(blob)-2]
	}
	if _, err := DecryptSecret(tampered, kek); err == nil {
		t.Fatal("DecryptSecret with tampered blob must fail")
	}
	// Non-encv1 form must fail.
	if _, err := DecryptSecret("sha256$aa$bb", kek); err == nil {
		t.Fatal("DecryptSecret with non-encrypted form must fail")
	}
}

func TestRotateSecret(t *testing.T) {
	s1, h1, err := RotateSecret()
	if err != nil {
		t.Fatalf("RotateSecret: %v", err)
	}
	s2, h2, err := RotateSecret()
	if err != nil {
		t.Fatalf("RotateSecret: %v", err)
	}
	if s1 == s2 || h1 == h2 {
		t.Fatal("rotation must produce fresh secret and stored form")
	}
	if ok, _ := VerifySecret(s1, h1); !ok {
		t.Fatal("rotated secret must verify against its own stored form")
	}
	if ok, _ := VerifySecret(s1, h2); ok {
		t.Fatal("old secret must not verify against the new stored form")
	}
}
