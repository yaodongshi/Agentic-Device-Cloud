// Package adminauth implements Admin API session authentication (design/80
// B-01): bcrypt password verification for human admin credentials, opaque
// session tokens stored only as SHA-256 hashes in Valkey, and the RBAC
// middleware enforcing the role-by-path matrix of design/33 1.2.
package adminauth

import (
	"errors"
	"fmt"

	"golang.org/x/crypto/bcrypt"
)

// bcrypt is the storage hash for admin passwords because they are
// human-chosen and low-entropy: the adaptive cost factor protects them
// against offline guessing. This differs deliberately from device/API-key
// secrets, which are high-entropy random values hashed with SHA-256
// elsewhere (see internal/auth/secrets.go rationale).

// ErrBadPassword reports a bcrypt mismatch. The login handler must never
// reveal whether the username exists, is disabled or had a wrong password:
// every one of those branches answers 401 code 10002 (design/33 3.1.1).
var ErrBadPassword = errors.New("adminauth: password mismatch")

// HashPassword returns the bcrypt storage form of a plaintext password.
// Only the hash is persisted, never the plaintext (NFR-004). bcrypt embeds
// the salt in the returned string, so every call yields a distinct hash.
func HashPassword(password string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", fmt.Errorf("adminauth: hash password: %w", err)
	}
	return string(b), nil
}

// VerifyPassword checks a plaintext password against a bcrypt storage form.
// bcrypt performs the comparison in constant time internally; the caller
// must not distinguish ErrBadPassword from other login failures.
func VerifyPassword(password, hash string) error {
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)); err != nil {
		return fmt.Errorf("%w: bcrypt compare failed", ErrBadPassword)
	}
	return nil
}
