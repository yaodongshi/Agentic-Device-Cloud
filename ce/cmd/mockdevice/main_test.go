package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"testing"
)

func TestHmacSign(t *testing.T) {
	got := hmacSign("secret", "dev-1", "1700000000", "0123456789abcdef")
	mac := hmac.New(sha256.New, []byte("secret"))
	io.WriteString(mac, "dev-1\n1700000000\n0123456789abcdef")
	want := hex.EncodeToString(mac.Sum(nil))
	if got != want {
		t.Fatalf("hmacSign = %q, want %q", got, want)
	}
	// deterministic: same inputs produce the same signature
	if again := hmacSign("secret", "dev-1", "1700000000", "0123456789abcdef"); again != got {
		t.Fatal("hmacSign must be deterministic")
	}
	// different inputs change the signature
	if other := hmacSign("secret2", "dev-1", "1700000000", "0123456789abcdef"); other == got {
		t.Fatal("different secret must change the signature")
	}
}

func TestMustBytes(t *testing.T) {
	b := mustBytes(8)
	if len(b) != 8 {
		t.Fatalf("mustBytes(8) len = %d", len(b))
	}
	if b2 := mustBytes(16); len(b2) != 16 {
		t.Fatalf("mustBytes(16) len = %d", len(b2))
	}
}
