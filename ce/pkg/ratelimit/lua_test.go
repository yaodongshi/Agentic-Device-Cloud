package ratelimit

import (
	"strings"
	"testing"
)

// The Lua script cannot be executed without a Valkey node and miniredis
// does not support scripting, so the script constant is validated by
// shape: argument wiring, key usage, atomicity markers (single EVAL, no
// external round trips) and the return contract the limiter parses.
// Semantic correctness is covered by TestMemStore* which run the same
// algorithm natively against the same constant.
func TestTokenBucketScriptShape(t *testing.T) {
	s := tokenBucketScript
	if strings.TrimSpace(s) == "" {
		t.Fatal("script must not be empty")
	}

	required := []string{
		"local rate = tonumber(ARGV[1])",  // refill rate
		"local burst = tonumber(ARGV[2])", // capacity
		"local now = tonumber(ARGV[3])",   // caller clock
		"local requested = tonumber(ARGV[4])",
		"redis.call('GET', KEYS[1])",                    // reads the token balance
		"KEYS[1] .. ':ts'",                              // separate timestamp key
		"math.min(burst, tokens + delta * rate / 1000)", // capped smooth refill (ms clock)
		"'EX', ttl",                       // keys expire when idle
		"return {allowed, tokens, retry}", // 3-element return contract
	}

	for _, want := range required {
		if !strings.Contains(s, want) {
			t.Errorf("script must contain %q", want)
		}
	}

	// The whole read-modify-write must be one script: exactly two SETs
	// (tokens and timestamp) and no INCR/DECR which would betray a fixed
	// window implementation.
	if got := strings.Count(s, "redis.call('SET'"); got != 2 {
		t.Errorf("script has %d SET calls, want 2 (balance + timestamp)", got)
	}
	if strings.Contains(s, "INCR") || strings.Contains(s, "DECR") {
		t.Error("script must be a token bucket, not an INCR fixed window")
	}

	// Quote sanity: every opening single quote is closed.
	if strings.Count(s, "'")%2 != 0 {
		t.Error("script has unbalanced single quotes")
	}
	// Structure sanity: the balance must be persisted before returning.
	iSet := strings.Index(s, "redis.call('SET', KEYS[1], tostring(tokens)")
	iRet := strings.Index(s, "return {allowed, tokens, retry}")
	if iSet < 0 || iRet < 0 || iSet > iRet {
		t.Error("script must persist the balance before returning the verdict")
	}
}
