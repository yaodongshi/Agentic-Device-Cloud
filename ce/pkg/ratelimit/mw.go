package ratelimit

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"
)

// KeyFunc extracts one rate limit dimension from a request. An empty key
// means the dimension does not apply to this request, e.g. a request
// without a device context is not device-limited.
type KeyFunc func(r *http.Request) (Scope, string)

// ContextString builds a KeyFunc that reads a string from the request
// context under ctxKey. It is the injection seam for the tenant / agent /
// device dimensions: the owning packages store their identifiers under
// unexported context keys and wrap them with this helper, or write a
// KeyFunc of their own for structured principals.
func ContextString(scope Scope, ctxKey interface{}) KeyFunc {
	return func(r *http.Request) (Scope, string) {
		v, _ := r.Context().Value(ctxKey).(string)
		return scope, v
	}
}

// Middleware returns an http middleware enforcing every dimension
// produced by keys, in order. Each dimension owns an independent bucket;
// the first one to deny the request aborts with 429, business code 10006
// (design/33 1.9) plus Retry-After and X-RateLimit-* headers.
//
// The limiter is invoked through QuotaInformer when available so the
// headers reflect the real bucket state; a plain RateLimiter still
// enforces 429 but cannot populate quota headers. Limiter errors fail
// open: dropping all traffic because the limiter is unavailable would
// turn a Valkey outage into a full outage, and the quota ledger
// (design/32 6.2) remains the hard quota backstop.
func Middleware(limiter RateLimiter, keys ...KeyFunc) func(http.Handler) http.Handler {
	informer, _ := limiter.(QuotaInformer)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			for _, keyFn := range keys {
				scope, key := keyFn(r)
				if key == "" {
					continue
				}
				if informer != nil {
					allowed, info, err := informer.AllowInfo(r.Context(), scope, key)
					if err != nil {
						continue // fail open
					}
					if !allowed {
						writeRateLimited(w, r, info)
						return
					}
					continue
				}
				allowed, err := limiter.Allow(r.Context(), scope, key)
				if err != nil {
					continue // fail open
				}
				if !allowed {
					writeRateLimited(w, r, QuotaInfo{RetryAfter: time.Second})
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

// codeRateLimited is the design/33 1.9 business error code for 429.
const (
	codeRateLimited = "10006"
	msgRateLimited  = "rate limit exceeded"
)

// errorBody mirrors the unified error shape (design/33) without
// depending on the internal httpx package.
type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	TraceID string `json:"trace_id,omitempty"`
}

// writeRateLimited emits 429 with Retry-After and the quota headers.
// Quota headers are only written when the limiter reported them
// (info.Limit > 0); the plain-Allow fallback can only provide Retry-After.
func writeRateLimited(w http.ResponseWriter, r *http.Request, info QuotaInfo) {
	retry := info.RetryAfter
	if retry < time.Second {
		retry = time.Second
	}
	h := w.Header()
	if info.Limit > 0 {
		h.Set("X-RateLimit-Limit", strconv.FormatInt(info.Limit, 10))
		h.Set("X-RateLimit-Remaining", strconv.FormatInt(info.Remaining, 10))
		h.Set("X-RateLimit-Reset", strconv.FormatInt(info.Reset.Unix(), 10))
	}
	h.Set("Retry-After", strconv.FormatInt(int64(retry/time.Second), 10))
	h.Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusTooManyRequests)
	_ = json.NewEncoder(w).Encode(errorBody{Code: codeRateLimited, Message: msgRateLimited, TraceID: traceID(r)})
}

// traceID mirrors httpx.TraceIDFrom (X-Trace-ID, then X-ADC-Request-ID)
// without importing the internal package.
func traceID(r *http.Request) string {
	if v := r.Header.Get("X-Trace-ID"); v != "" {
		return v
	}
	return r.Header.Get("X-ADC-Request-ID")
}
