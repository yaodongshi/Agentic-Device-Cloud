package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// mockLimiter implements both RateLimiter and the optional rateLimiterInfo
// extension; it records every (scope, key) pair it sees. When denyScopes is
// set, only those scopes are denied (everything else is allowed).
type mockLimiter struct {
	mu         sync.Mutex
	allow      bool
	info       RateLimitInfo
	err        error
	denyScopes map[string]bool
	calls      []string
}

func (m *mockLimiter) Allow(_ context.Context, scope, key string) (bool, error) {
	allowed, _, err := m.record(scope, key, m.allow, m.info)
	return allowed, err
}

func (m *mockLimiter) AllowInfo(_ context.Context, scope, key string) (bool, RateLimitInfo, error) {
	return m.record(scope, key, m.allow, m.info)
}

func (m *mockLimiter) record(scope, key string, allowed bool, info RateLimitInfo) (bool, RateLimitInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, scope+":"+key)
	if m.err != nil {
		return false, RateLimitInfo{}, m.err
	}
	if m.denyScopes != nil {
		allowed = !m.denyScopes[scope]
	}
	return allowed, info, nil
}

func (m *mockLimiter) scopes(t *testing.T) []string {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.calls...)
}

func TestTraceIDMiddlewareGeneratesAndEchoes(t *testing.T) {
	h := TraceID("2026-08")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Trace-ID"); got == "" || len(got) != 32 {
			t.Errorf("generated X-Trace-ID = %q, want 32 hex chars", got)
		}
		if got := r.Header.Get("X-ADC-Request-ID"); got == "" || len(got) != 32 {
			t.Errorf("generated X-ADC-Request-ID = %q, want 32 hex chars", got)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/admin", nil)
	h.ServeHTTP(rec, req)

	if got := rec.Header().Get("X-Trace-ID"); len(got) != 32 {
		t.Errorf("response X-Trace-ID = %q, want 32 hex chars", got)
	}
	if got := rec.Header().Get("X-ADC-Request-ID"); len(got) != 32 {
		t.Errorf("response X-ADC-Request-ID = %q, want 32 hex chars", got)
	}
	if got := rec.Header().Get("X-ADC-Version"); got != "2026-08" {
		t.Errorf("response X-ADC-Version = %q, want 2026-08", got)
	}
}

func TestTraceIDMiddlewarePassthroughUnchanged(t *testing.T) {
	h := TraceID("2026-08")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("X-Trace-ID", "trace-from-client")
	req.Header.Set("X-ADC-Request-ID", "req-from-client")
	h.ServeHTTP(rec, req)

	if got := rec.Header().Get("X-Trace-ID"); got != "trace-from-client" {
		t.Errorf("response X-Trace-ID = %q, want pass-through value", got)
	}
	if got := rec.Header().Get("X-ADC-Request-ID"); got != "req-from-client" {
		t.Errorf("response X-ADC-Request-ID = %q, want pass-through value", got)
	}
}

func TestPassAuthSnapshotsCredentialHeadersVerbatim(t *testing.T) {
	h := PassAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		out := make(http.Header)
		applyAuthHeaders(r.Context(), out)
		for name, want := range map[string]string{
			"Authorization":                "Bearer adc_live_abc",
			"X-ADC-Key":                    "adc_xyz_secret",
			"X-ADC-Application-Credential": "adc_app_once",
			"Cookie":                       "adc_session=session-1",
			"X-Device-ID":                  "dev-001",
			"X-Device-Model":               "T-800",
			"X-ADC-Signature":              "deadbeef",
		} {
			if got := out.Get(name); got != want {
				t.Errorf("re-applied header %s = %q, want %q", name, got, want)
			}
		}
	}))
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Authorization", "Bearer adc_live_abc")
	req.Header.Set("X-ADC-Key", "adc_xyz_secret")
	req.Header.Set("X-ADC-Application-Credential", "adc_app_once")
	req.Header.Add("Cookie", "adc_session=session-1")
	req.Header.Set("X-Device-ID", "dev-001")
	req.Header.Set("X-Device-Model", "T-800")
	req.Header.Set("X-ADC-Signature", "deadbeef")
	h.ServeHTTP(httptest.NewRecorder(), req)
}

func TestRateLimitDenyWrites429WithHeaders(t *testing.T) {
	reset := time.Now().Add(2 * time.Second)
	lim := &mockLimiter{allow: false, info: RateLimitInfo{Limit: 100, Remaining: 0, Reset: reset}}
	ran := false
	h := RateLimit(lim, func(string) string { return "" }, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ran = true
	}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	h.ServeHTTP(rec, req)

	if ran {
		t.Fatal("handler ran despite limiter denial")
	}
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	var body struct{ Code, Message string }
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Code != "10006" {
		t.Errorf("code = %q, want 10006", body.Code)
	}
	if rec.Header().Get("X-RateLimit-Limit") != "100" {
		t.Errorf("X-RateLimit-Limit = %q, want 100", rec.Header().Get("X-RateLimit-Limit"))
	}
	if rec.Header().Get("X-RateLimit-Remaining") != "0" {
		t.Errorf("X-RateLimit-Remaining = %q, want 0", rec.Header().Get("X-RateLimit-Remaining"))
	}
	if rec.Header().Get("X-RateLimit-Reset") == "" {
		t.Error("X-RateLimit-Reset missing")
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("Retry-After missing")
	}
}

func TestRateLimitAllowEmitsWindowHeaders(t *testing.T) {
	lim := &mockLimiter{allow: true, info: RateLimitInfo{Limit: 100, Remaining: 57, Reset: time.Now().Add(time.Minute)}}
	h := RateLimit(lim, func(string) string { return "" }, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if got := rec.Header().Get("X-RateLimit-Remaining"); got != "57" {
		t.Errorf("X-RateLimit-Remaining = %q, want 57", got)
	}
}

func TestRateLimiterErrorFailsOpen(t *testing.T) {
	lim := &mockLimiter{err: errors.New("valkey down")}
	ran := false
	h := RateLimit(lim, func(string) string { return "" }, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ran = true
		w.WriteHeader(http.StatusNoContent)
	}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	h.ServeHTTP(rec, req)

	if !ran {
		t.Fatal("handler did not run; limiter infra error must fail open")
	}
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
}

func TestRateLimitNilLimiterSkips(t *testing.T) {
	h := RateLimit(nil, func(string) string { return "" }, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
}
