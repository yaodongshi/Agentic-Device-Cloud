package ratelimit

import (
	"context"
	"sync"
	"testing"
	"time"
)

func newTestLimiter(t *testing.T, limits map[Scope]Limit) *TokenBucketLimiter {
	t.Helper()
	lim, err := NewTokenBucket(NewMemStore(), limits)
	if err != nil {
		t.Fatalf("NewTokenBucket: %v", err)
	}
	return lim
}

// drain consumes n tokens, failing the test on denial or error.
func drain(t *testing.T, ctx context.Context, lim RateLimiter, scope Scope, key string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		ok, err := lim.Allow(ctx, scope, key)
		if err != nil {
			t.Fatalf("drain %d/%d: %v", i+1, n, err)
		}
		if !ok {
			t.Fatalf("drain %d/%d: unexpectedly denied", i+1, n)
		}
	}
}

func TestTokenBucketBurstExhaustion(t *testing.T) {
	lim := newTestLimiter(t, map[Scope]Limit{ScopeTenant: {Rate: 2, Burst: 4}})
	ctx := context.Background()
	drain(t, ctx, lim, ScopeTenant, "t1", 4)
	ok, err := lim.Allow(ctx, ScopeTenant, "t1")
	if err != nil {
		t.Fatalf("Allow: %v", err)
	}
	if ok {
		t.Fatal("fifth request without refill must exhaust the burst")
	}
}

func TestTokenBucketRefill(t *testing.T) {
	lim := newTestLimiter(t, map[Scope]Limit{ScopeTenant: {Rate: 2, Burst: 4}})
	now := time.Now()
	lim.now = func() time.Time { return now }
	ctx := context.Background()
	drain(t, ctx, lim, ScopeTenant, "t1", 4)

	now = now.Add(500 * time.Millisecond) // one token refilled at 2/s
	ok, err := lim.Allow(ctx, ScopeTenant, "t1")
	if err != nil {
		t.Fatalf("Allow: %v", err)
	}
	if !ok {
		t.Fatal("refilled token should be admitted")
	}
	if ok, err = lim.Allow(ctx, ScopeTenant, "t1"); err != nil {
		t.Fatalf("Allow: %v", err)
	} else if ok {
		t.Fatal("only one token was refilled, second request must be denied")
	}

	now = now.Add(2 * time.Second) // 4 tokens refilled, capped at burst
	drain(t, ctx, lim, ScopeTenant, "t1", 4)
}

func TestMultiDimensionIsolation(t *testing.T) {
	lim := newTestLimiter(t, map[Scope]Limit{
		ScopeTenant: {Rate: 2, Burst: 2},
		ScopeAgent:  {Rate: 2, Burst: 2},
	})
	ctx := context.Background()
	drain(t, ctx, lim, ScopeTenant, "tenantA", 2)

	for _, tc := range []struct {
		scope Scope
		key   string
		want  bool
	}{
		{ScopeTenant, "tenantB", true},  // same scope, different key
		{ScopeAgent, "tenantA", true},   // different scope, same key
		{ScopeTenant, "tenantA", false}, // the drained bucket stays drained
	} {
		ok, err := lim.Allow(ctx, tc.scope, tc.key)
		if err != nil {
			t.Fatalf("Allow(%s, %s): %v", tc.scope, tc.key, err)
		}
		if ok != tc.want {
			t.Errorf("Allow(%s, %s) = %v, want %v", tc.scope, tc.key, ok, tc.want)
		}
	}
}

func TestAllowInfo(t *testing.T) {
	lim := newTestLimiter(t, map[Scope]Limit{ScopeTenant: {Rate: 2, Burst: 4}})
	ctx := context.Background()

	allowed, info, err := lim.AllowInfo(ctx, ScopeTenant, "t1")
	if err != nil {
		t.Fatalf("AllowInfo: %v", err)
	}
	if !allowed || info.Limit != 4 || info.Remaining != 3 || info.RetryAfter != 0 {
		t.Fatalf("first call: allowed=%v info=%+v, want allowed limit=4 remaining=3 retry=0", allowed, info)
	}

	drain(t, ctx, lim, ScopeTenant, "t1", 3)
	allowed, info, err = lim.AllowInfo(ctx, ScopeTenant, "t1")
	if err != nil {
		t.Fatalf("AllowInfo: %v", err)
	}
	if allowed {
		t.Fatal("empty bucket must deny")
	}
	if info.Limit != 4 || info.Remaining != 0 {
		t.Errorf("denied info = %+v, want limit=4 remaining=0", info)
	}
	if info.RetryAfter != time.Second { // ceil(1 token / 2 per s) = 1s
		t.Errorf("RetryAfter = %v, want 1s", info.RetryAfter)
	}
	if !info.Reset.After(time.Now()) {
		t.Errorf("Reset = %v must be in the future", info.Reset)
	}
}

func TestAllowEmptyKey(t *testing.T) {
	lim := newTestLimiter(t, nil)
	if _, err := lim.Allow(context.Background(), ScopeTenant, ""); err == nil {
		t.Fatal("empty key must be rejected, one shared bucket would cross-contaminate callers")
	}
}

func TestAllowUnknownScope(t *testing.T) {
	lim := newTestLimiter(t, nil)
	if _, err := lim.Allow(context.Background(), Scope("bogus"), "t1"); err == nil {
		t.Fatal("unconfigured scope must return an error")
	}
}

func TestNewTokenBucketValidation(t *testing.T) {
	if _, err := NewTokenBucket(nil, nil); err == nil {
		t.Fatal("nil store must be rejected")
	}
	if _, err := NewTokenBucket(NewMemStore(), map[Scope]Limit{ScopeTenant: {Rate: 0, Burst: 10}}); err == nil {
		t.Fatal("zero rate must be rejected")
	}
	if _, err := NewTokenBucket(NewMemStore(), map[Scope]Limit{ScopeTenant: {Rate: 10, Burst: 0}}); err == nil {
		t.Fatal("zero burst must be rejected")
	}
	lim, err := NewTokenBucket(NewMemStore(), nil)
	if err != nil {
		t.Fatalf("nil limits must select defaults: %v", err)
	}
	ok, err := lim.Allow(context.Background(), ScopeTenant, "t1")
	if err != nil || !ok {
		t.Fatalf("default limits must admit: ok=%v err=%v", ok, err)
	}
}

func TestMemStoreRejectsForeignScript(t *testing.T) {
	s := NewMemStore()
	if _, err := s.Eval(context.Background(), "return 1", nil); err == nil {
		t.Fatal("foreign script must be rejected (limiter/memstore script identity check)")
	}
}

func TestMemStoreRejectsWrongShape(t *testing.T) {
	s := NewMemStore()
	if _, err := s.Eval(context.Background(), tokenBucketScript, nil); err == nil {
		t.Fatal("missing key must be rejected")
	}
	if _, err := s.Eval(context.Background(), tokenBucketScript, []string{"k"}, int64(1), int64(2)); err == nil {
		t.Fatal("missing args must be rejected")
	}
}

func TestParseEvalResult(t *testing.T) {
	tests := []struct {
		name          string
		raw           interface{}
		wantAllowed   bool
		wantRemaining float64
		wantRetry     float64
		wantErr       bool
	}{
		{"integral values", []interface{}{int64(1), int64(3), int64(0)}, true, 3, 0, false},
		{"fractional values", []interface{}{float64(0), float64(0.5), float64(0.25)}, false, 0.5, 0.25, false},
		{"string values", []interface{}{"0", "2.5", "1"}, false, 2.5, 1, false},
		{"short table", []interface{}{int64(1)}, false, 0, 0, true},
		{"not a table", "nonsense", false, 0, 0, true},
		{"bad element type", []interface{}{boolValue{}, int64(0), int64(0)}, false, 0, 0, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			allowed, remaining, retry, err := parseEvalResult(tc.raw)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
			if err != nil {
				return
			}
			if allowed != tc.wantAllowed || remaining != tc.wantRemaining || retry != tc.wantRetry {
				t.Errorf("got (%v, %v, %v), want (%v, %v, %v)",
					allowed, remaining, retry, tc.wantAllowed, tc.wantRemaining, tc.wantRetry)
			}
		})
	}
}

type boolValue struct{}

func TestMemStoreConcurrent(t *testing.T) {
	// Fixed clock: 150 concurrent requests must admit exactly the burst of
	// 100 — no wall-clock micro-refills may leak in (P1 fix: the previous
	// real-clock version intermittently admitted 101).
	lim, err := NewTokenBucketWithClock(NewMemStore(), map[Scope]Limit{ScopeTenant: {Rate: 100, Burst: 100}}, func() time.Time {
		return time.Unix(1_700_000_000, 0)
	})
	if err != nil {
		t.Fatalf("NewTokenBucketWithClock: %v", err)
	}
	ctx := context.Background()
	var wg sync.WaitGroup
	var mu sync.Mutex
	admitted := 0
	var firstErr error
	for i := 0; i < 150; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := lim.Allow(ctx, ScopeTenant, "t1")
			if err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				mu.Unlock()
				return
			}
			if ok {
				mu.Lock()
				admitted++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if firstErr != nil {
		t.Fatalf("Allow: %v", firstErr)
	}
	if admitted != 100 {
		t.Fatalf("admitted %d requests, want exactly the burst of 100", admitted)
	}
}
