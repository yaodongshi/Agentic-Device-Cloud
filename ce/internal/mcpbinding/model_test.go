package mcpbinding

import (
	"encoding/hex"
	"testing"
)

func TestValidateTransition(t *testing.T) {
	cases := []struct {
		from, to Status
		want     bool
	}{
		{StatusRegistered, StatusBinding, true},
		{StatusRegistered, StatusRevoked, true},
		{StatusBinding, StatusBound, true},
		{StatusBinding, StatusRevoked, true},
		{StatusBound, StatusRevoked, true},
		{StatusRevoked, StatusBinding, true},
		// illegal: skips, loops, terminal exits
		{StatusRegistered, StatusBound, false},
		{StatusBinding, StatusRegistered, false},
		{StatusBound, StatusBinding, false},
		{StatusRevoked, StatusRevoked, false},
		{StatusRegistered, StatusRegistered, false},
		{StatusBound, StatusBound, false},
		{StatusRevoked, StatusBound, false},
		{StatusRevoked, StatusRegistered, false},
	}
	for i, c := range cases {
		if got := ValidateTransition(c.from, c.to); got != c.want {
			t.Errorf("case %d: ValidateTransition(%s, %s) = %v, want %v", i, c.from, c.to, got, c.want)
		}
	}
}

func TestGenerateBindingToken(t *testing.T) {
	a, err := GenerateBindingToken()
	if err != nil {
		t.Fatalf("GenerateBindingToken: %v", err)
	}
	b, err := GenerateBindingToken()
	if err != nil {
		t.Fatalf("GenerateBindingToken: %v", err)
	}
	if len(a) != 64 || len(b) != 64 {
		t.Fatalf("token length = %d/%d, want 64 hex chars (256 bits)", len(a), len(b))
	}
	if _, err := hex.DecodeString(a); err != nil {
		t.Fatalf("token is not hex: %v", err)
	}
	if a == b {
		t.Fatal("two generated tokens must differ")
	}
}

func TestHashBindingTokenDeterministic(t *testing.T) {
	const token = "7f3a9c2e1b8d4f6a0c5e2d9b3a7f1c4e8d2a5b9c6e1f3a7d0b4c8e2f5a1d6b"
	h1 := HashBindingToken(token)
	h2 := HashBindingToken(token)
	if h1 != h2 {
		t.Fatal("hash must be deterministic")
	}
	if h1 == token {
		t.Fatal("hash must not equal the plaintext token")
	}
	if len(h1) != 64 {
		t.Fatalf("hash length = %d, want 64", len(h1))
	}
	if HashBindingToken("other") == h1 {
		t.Fatal("different tokens must hash differently")
	}
}
