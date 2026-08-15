// Package auth implements the SEC-03 device handshake authentication:
// HMAC-SHA256 signature over (deviceCode, timestamp, nonce), a +/- 300 second
// clock-skew window, and one-time nonce consumption in Valkey.
// Design baseline: LLD 3.1.2 / 3.1.3 (interface I1 DeviceAuthenticator).
package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
)

const (
	// secretLenBytes is the device secret size: 32 bytes = 256 bits of
	// entropy (FR-010, NFR-004). 256-bit random secrets are not
	// brute-forceable, which matters for the hash choice below.
	secretLenBytes = 32
	// saltLenBytes is the random salt size for one-way hashes.
	saltLenBytes = 16
	// hashAlgPrefix marks the one-way storage form "sha256$<salt>$<digest>".
	hashAlgPrefix = "sha256$"
	// encPrefix marks the KEK-encrypted storage form "encv1$<base64>".
	encPrefix = "encv1$"
)

// GenerateSecret returns a new 32-byte random secret, hex-encoded. The
// plaintext is handed to the caller exactly once (NFR-004: secrets are never
// stored or logged in clear text). Callers must pass the plaintext to the
// device provisioning flow immediately.
func GenerateSecret() (string, error) {
	b := make([]byte, secretLenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("auth: generate secret: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// HashSecret returns the one-way storage form of a secret:
// "sha256$<salt-hex>$<digest-hex>", digest = SHA-256(salt || secret).
//
// Choice of algorithm: bcrypt (and Argon2id) were considered and rejected for
// two reasons. First, this package is constrained to the standard library plus
// pgx/v5 and go-redis/v9; bcrypt lives in golang.org/x/crypto and would widen
// the direct dependency surface. Second, and more fundamentally, bcrypt's
// adaptive cost factor protects low-entropy human passwords against offline
// guessing; a 32-byte random device secret has 256 bits of entropy and cannot
// be brute-forced regardless of hash speed, so SHA-256 + per-secret random
// salt is sufficient and matches the adc_agent_api_keys precedent (design/32
// 3.7: SHA-256 only, no salt column there). Argon2id remains the target for
// any future human-supplied credential path.
func HashSecret(secret string) (string, error) {
	salt := make([]byte, saltLenBytes)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("auth: hash secret: %w", err)
	}
	sum := sha256.Sum256(append(salt, []byte(secret)...))
	return hashAlgPrefix + hex.EncodeToString(salt) + "$" + hex.EncodeToString(sum[:]), nil
}

// VerifySecret checks a plaintext secret against a HashSecret storage form
// in constant time.
func VerifySecret(secret, stored string) (bool, error) {
	saltHex, digestHex, err := parseHashForm(stored)
	if err != nil {
		return false, err
	}
	salt, err := hex.DecodeString(saltHex)
	if err != nil {
		return false, fmt.Errorf("auth: verify secret: bad salt: %w", err)
	}
	want, err := hex.DecodeString(digestHex)
	if err != nil {
		return false, fmt.Errorf("auth: verify secret: bad digest: %w", err)
	}
	sum := sha256.Sum256(append(salt, []byte(secret)...))
	return subtle.ConstantTimeCompare(want, sum[:]) == 1, nil
}

// RotateSecret generates a fresh secret together with its stored form for a
// token-type device. Rotation support: the caller keeps the previous
// credential valid during the transition window by storing both under
// credential_version (design/32 3.13, FR-010 reset semantics) and bumps the
// version column; old hashes become unverifiable only after the transition
// window expires. For hmac devices the stored form is produced with
// EncryptSecret instead.
func RotateSecret() (secret, stored string, err error) {
	secret, err = GenerateSecret()
	if err != nil {
		return "", "", err
	}
	stored, err = HashSecret(secret)
	if err != nil {
		return "", "", err
	}
	return secret, stored, nil
}

func parseHashForm(stored string) (saltHex, digestHex string, err error) {
	if !strings.HasPrefix(stored, hashAlgPrefix) {
		return "", "", fmt.Errorf("auth: stored form %q is not %q", stored, hashAlgPrefix)
	}
	rest := strings.TrimPrefix(stored, hashAlgPrefix)
	saltHex, digestHex, ok := strings.Cut(rest, "$")
	if !ok || saltHex == "" || digestHex == "" {
		return "", "", fmt.Errorf("auth: malformed stored form %q", stored)
	}
	return saltHex, digestHex, nil
}

// LoadKEK reads the KEK from the ADC_DEVICE_KEK environment variable
// (SEC-13: secrets come from the environment, never from code or arguments;
// LLD 3.1.3). The raw string is derived to a 32-byte AES key with SHA-256 so
// any reasonable passphrase length works. Production should prefer a KMS-held
// key; ADC_DEVICE_KEK is the V1.0 mechanism.
func LoadKEK() ([]byte, error) {
	kek := os.Getenv("ADC_DEVICE_KEK")
	if kek == "" {
		return nil, errors.New("auth: ADC_DEVICE_KEK is required when hmac devices are served")
	}
	if len(kek) < 16 {
		return nil, errors.New("auth: ADC_DEVICE_KEK must be at least 16 characters")
	}
	sum := sha256.Sum256([]byte(kek))
	return sum[:], nil
}

// EncryptSecret seals a plaintext signing key with AES-256-GCM under the KEK
// and returns the storage form "encv1$<base64(nonce||ciphertext)>". This is
// the hmac-device storage per LLD 3.1.3: the server must be able to recover
// the key at verification time, so a one-way hash is not usable for hmac;
// the KEK keeps the blob at rest encrypted.
func EncryptSecret(secret string, kek []byte) (string, error) {
	if len(kek) != 32 {
		return "", fmt.Errorf("auth: encrypt secret: KEK must be a 32-byte key, got %d bytes", len(kek))
	}
	block, err := aes.NewCipher(kek)
	if err != nil {
		return "", fmt.Errorf("auth: encrypt secret: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("auth: encrypt secret: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("auth: encrypt secret: %w", err)
	}
	sealed := gcm.Seal(nonce, nonce, []byte(secret), nil)
	return encPrefix + base64.StdEncoding.EncodeToString(sealed), nil
}

// DecryptSecret opens an EncryptSecret blob. It fails closed: a blob that
// cannot be authenticated is rejected (GCM guarantees integrity), so a
// tampered or KEK-mismatched credential never reaches the verifier.
func DecryptSecret(stored string, kek []byte) (string, error) {
	if kek == nil {
		return "", errors.New("auth: decrypt secret: KEK not configured (fail closed)")
	}
	if len(kek) != 32 {
		return "", fmt.Errorf("auth: decrypt secret: KEK must be a 32-byte key, got %d bytes", len(kek))
	}
	if !strings.HasPrefix(stored, encPrefix) {
		return "", fmt.Errorf("auth: decrypt secret: stored form %q is not %q", stored, encPrefix)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(stored, encPrefix))
	if err != nil {
		return "", fmt.Errorf("auth: decrypt secret: %w", err)
	}
	block, err := aes.NewCipher(kek)
	if err != nil {
		return "", fmt.Errorf("auth: decrypt secret: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("auth: decrypt secret: %w", err)
	}
	if len(raw) < gcm.NonceSize() {
		return "", errors.New("auth: decrypt secret: blob too short")
	}
	nonce, ciphertext := raw[:gcm.NonceSize()], raw[gcm.NonceSize():]
	plain, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", fmt.Errorf("auth: decrypt secret: %w", err)
	}
	return string(plain), nil
}
