package adminauth

import (
	"errors"
	"strings"
	"testing"
)

func TestHashPasswordRoundTrip(t *testing.T) {
	const password = "correct horse battery staple"

	hash, err := HashPassword(password)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	// bcrypt storage form: $2a$/$2b$/$2y$ + cost + 53-char salt/hash.
	if !strings.HasPrefix(hash, "$2") || len(hash) != 60 {
		t.Fatalf("hash = %q, want a 60-char bcrypt string", hash)
	}

	if err := VerifyPassword(password, hash); err != nil {
		t.Fatalf("VerifyPassword(correct) = %v, want nil", err)
	}
	if err := VerifyPassword("wrong password", hash); !errors.Is(err, ErrBadPassword) {
		t.Fatalf("VerifyPassword(wrong) = %v, want ErrBadPassword", err)
	}
	if err := VerifyPassword("", hash); !errors.Is(err, ErrBadPassword) {
		t.Fatalf("VerifyPassword(empty) = %v, want ErrBadPassword", err)
	}
}

func TestHashPasswordSaltUniqueness(t *testing.T) {
	h1, err := HashPassword("same-password")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	h2, err := HashPassword("same-password")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if h1 == h2 {
		t.Fatal("two hashes of the same password are identical; bcrypt must embed a fresh salt")
	}
}

func TestVerifyPasswordAgainstMalformedHash(t *testing.T) {
	if err := VerifyPassword("anything", "not-a-bcrypt-hash"); !errors.Is(err, ErrBadPassword) {
		t.Fatalf("VerifyPassword(malformed hash) = %v, want ErrBadPassword", err)
	}
}
