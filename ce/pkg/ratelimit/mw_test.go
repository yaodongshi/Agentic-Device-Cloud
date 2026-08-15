package ratelimit

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"
)

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
}

func headerKey(scope Scope) KeyFunc {
	return func(r *http.Request) (Scope, string) {
		return scope, r.Header.Get("X-Test-" + string(scope))
	}
}

// fakeLimiter records calls and answers from a hook; it implements only
// the plain RateLimiter interface (no QuotaInformer).
type fakeLimiter struct {
	mu    sync.Mutex
	calls []fakeCall
	allow func(ctx context.Context, scope Scope, key string) (bool, error)
}

type fakeCall struct {
	scope Scope
	key   string
}

func (f *fakeLimiter) Allow(ctx context.Context, scope Scope, key string) (bool, error) {
	f.mu.Lock()
	f.calls = append(f.calls, fakeCall{scope, key})
	f.mu.Unlock()
	return f.allow(ctx, scope, key)
}

func (f *fakeLimiter) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func newRequest(t *testing.T, method, path string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(method, path, nil)
	return r
}

func TestMiddlewarePassesUnderLimit(t *testing.T) {
	lim := newTestLimiter(t, map[Scope]Limit{ScopeTenant: {Rate: 1, Burst: 2}})
	h := Middleware(lim, headerKey(ScopeTenant))(okHandler())

	for i := 0; i < 2; i++ {
		rr := httptest.NewRecorder()
		req := newRequest(t, http.MethodGet, "/")
		req.Header.Set("X-Test-tenant", "t1")
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("request %d: got %d, want 200", i+1, rr.Code)
		}
	}
}

func TestMiddleware429AndHeaders(t *testing.T) {
	lim := newTestLimiter(t, map[Scope]Limit{ScopeTenant: {Rate: 1, Burst: 2}})
	h := Middleware(lim, headerKey(ScopeTenant))(okHandler())

	var rr *httptest.ResponseRecorder
	for i := 0; i < 3; i++ {
		rr = httptest.NewRecorder()
		req := newRequest(t, http.MethodGet, "/")
		req.Header.Set("X-Test-tenant", "t1")
		h.ServeHTTP(rr, req)
	}
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("third request: got %d, want 429", rr.Code)
	}
	hdrs := rr.Header()
	if got := hdrs.Get("Retry-After"); got != "1" {
		t.Errorf("Retry-After = %q, want 1 (1 token/s refill)", got)
	}
	if got := hdrs.Get("X-RateLimit-Limit"); got != "2" {
		t.Errorf("X-RateLimit-Limit = %q, want 2", got)
	}
	if got := hdrs.Get("X-RateLimit-Remaining"); got != "0" {
		t.Errorf("X-RateLimit-Remaining = %q, want 0", got)
	}
	reset, err := strconv.ParseInt(hdrs.Get("X-RateLimit-Reset"), 10, 64)
	if err != nil {
		t.Fatalf("X-RateLimit-Reset = %q, want unix seconds", hdrs.Get("X-RateLimit-Reset"))
	}
	if reset < time.Now().Unix() {
		t.Errorf("X-RateLimit-Reset = %d must not be in the past", reset)
	}
	if ct := hdrs.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var body errorBody
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Code != "10006" || body.Message == "" {
		t.Errorf("body = %+v, want code 10006 with message", body)
	}
}

func TestMiddlewareMultiDimension(t *testing.T) {
	lim := newTestLimiter(t, map[Scope]Limit{
		ScopeTenant: {Rate: 10, Burst: 10},
		ScopeAgent:  {Rate: 1, Burst: 1},
	})
	h := Middleware(lim, headerKey(ScopeTenant), headerKey(ScopeAgent))(okHandler())

	// First request passes every dimension.
	rr := httptest.NewRecorder()
	req := newRequest(t, http.MethodGet, "/")
	req.Header.Set("X-Test-tenant", "t1")
	req.Header.Set("X-Test-agent", "a1")
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("first request: got %d, want 200", rr.Code)
	}

	// Agent a1 is exhausted: denied even though the tenant bucket has room.
	rr = httptest.NewRecorder()
	req = newRequest(t, http.MethodGet, "/")
	req.Header.Set("X-Test-tenant", "t1")
	req.Header.Set("X-Test-agent", "a1")
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("exhausted agent: got %d, want 429", rr.Code)
	}

	// A different agent under the same tenant is not affected.
	rr = httptest.NewRecorder()
	req = newRequest(t, http.MethodGet, "/")
	req.Header.Set("X-Test-tenant", "t1")
	req.Header.Set("X-Test-agent", "a2")
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("other agent: got %d, want 200 (dimension isolation)", rr.Code)
	}
}

func TestMiddlewareEmptyKeySkipsDimension(t *testing.T) {
	calls := 0
	lim := &fakeLimiter{
		allow: func(ctx context.Context, scope Scope, key string) (bool, error) {
			calls++
			return false, nil // deny everything, but only when consulted
		},
	}
	h := Middleware(lim, headerKey(ScopeDevice))(okHandler())

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, newRequest(t, http.MethodGet, "/")) // no X-Test-device header
	if rr.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 (dimension not applicable)", rr.Code)
	}
	if calls != 0 {
		t.Fatalf("limiter called %d times, want 0 (empty key must skip)", calls)
	}
}

func TestMiddlewareKeyIsolation(t *testing.T) {
	lim := newTestLimiter(t, map[Scope]Limit{ScopeTenant: {Rate: 1, Burst: 1}})
	h := Middleware(lim, headerKey(ScopeTenant))(okHandler())

	rr := httptest.NewRecorder()
	req := newRequest(t, http.MethodGet, "/")
	req.Header.Set("X-Test-tenant", "t1")
	h.ServeHTTP(rr, req)

	rr = httptest.NewRecorder()
	req = newRequest(t, http.MethodGet, "/")
	req.Header.Set("X-Test-tenant", "t1")
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("tenant t1 second request: got %d, want 429", rr.Code)
	}

	rr = httptest.NewRecorder()
	req = newRequest(t, http.MethodGet, "/")
	req.Header.Set("X-Test-tenant", "t2")
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("tenant t2: got %d, want 200 (per-key isolation)", rr.Code)
	}
}

func TestMiddlewareFallbackWithoutQuotaInformer(t *testing.T) {
	lim := &fakeLimiter{
		allow: func(ctx context.Context, scope Scope, key string) (bool, error) {
			return false, nil
		},
	}
	h := Middleware(lim, headerKey(ScopeTenant))(okHandler())

	rr := httptest.NewRecorder()
	req := newRequest(t, http.MethodGet, "/")
	req.Header.Set("X-Test-tenant", "t1")
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("got %d, want 429", rr.Code)
	}
	if got := rr.Header().Get("Retry-After"); got != "1" {
		t.Errorf("Retry-After = %q, want fallback of 1", got)
	}
	if got := rr.Header().Get("X-RateLimit-Limit"); got != "" {
		t.Errorf("X-RateLimit-Limit = %q, want absent without QuotaInformer", got)
	}
	var body errorBody
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Code != "10006" {
		t.Errorf("body code = %q, want 10006", body.Code)
	}
}

func TestMiddlewareLimiterErrorFailsOpen(t *testing.T) {
	lim := &fakeLimiter{
		allow: func(ctx context.Context, scope Scope, key string) (bool, error) {
			return false, context.DeadlineExceeded
		},
	}
	h := Middleware(lim, headerKey(ScopeTenant))(okHandler())

	rr := httptest.NewRecorder()
	req := newRequest(t, http.MethodGet, "/")
	req.Header.Set("X-Test-tenant", "t1")
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 (fail open on limiter errors)", rr.Code)
	}
}

func TestMiddlewareContextString(t *testing.T) {
	lim := newTestLimiter(t, map[Scope]Limit{ScopeAgent: {Rate: 1, Burst: 1}})
	type agentKey struct{}
	h := Middleware(lim, ContextString(ScopeAgent, agentKey{}))(okHandler())

	rr := httptest.NewRecorder()
	req := newRequest(t, http.MethodGet, "/").WithContext(
		context.WithValue(context.Background(), agentKey{}, "a1"))
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("first request: got %d, want 200", rr.Code)
	}

	rr = httptest.NewRecorder()
	req = newRequest(t, http.MethodGet, "/").WithContext(
		context.WithValue(context.Background(), agentKey{}, "a1"))
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("second request: got %d, want 429 (context key dimension)", rr.Code)
	}
}
