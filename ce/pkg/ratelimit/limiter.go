// Package ratelimit implements per-dimension token bucket rate limiting
// (SEC-12, design/31 3.2.7): tenant, agent and device dimensions each get
// an independent bucket, so a noisy key in one dimension never starves the
// others. The production limiter is backed by Valkey through one atomic
// Lua script (tokenbucket.go); an in-memory store provides the same
// semantics for tests and single-process deployments.
package ratelimit

import (
	"context"
	"fmt"
	"time"
)

// Scope identifies one rate limit dimension (design/31 3.2.7).
type Scope string

const (
	ScopeTenant Scope = "tenant"
	ScopeAgent  Scope = "agent"
	ScopeDevice Scope = "device"
)

// Limit defines the token bucket parameters for one scope: Rate tokens
// are refilled per second and Burst is the bucket capacity in tokens.
type Limit struct {
	Rate  float64 // tokens refilled per second
	Burst int64   // bucket capacity
}

// DefaultLimits mirrors the design/31 3.2.7 defaults (tenant 200 req/s
// with a 2x bucket, agent 50 req/s, device 10 req/s). Deployments
// override them from configuration (ADC_RATELIMIT_* env keys) when
// wiring the limiter.
func DefaultLimits() map[Scope]Limit {
	return map[Scope]Limit{
		ScopeTenant: {Rate: 200, Burst: 400},
		ScopeAgent:  {Rate: 50, Burst: 100},
		ScopeDevice: {Rate: 10, Burst: 20},
	}
}

// Validate rejects non-positive parameters, which would break the token
// bucket arithmetic (division by zero in the refill TTL).
func (l Limit) Validate() error {
	if l.Rate <= 0 {
		return fmt.Errorf("ratelimit: rate must be positive, got %v", l.Rate)
	}
	if l.Burst <= 0 {
		return fmt.Errorf("ratelimit: burst must be positive, got %d", l.Burst)
	}
	return nil
}

// RateLimiter reports whether one request is admitted for a scope key.
// Implementations must be safe for concurrent use.
type RateLimiter interface {
	Allow(ctx context.Context, scope Scope, key string) (bool, error)
}

// QuotaInfo carries bucket state after an admission decision so callers
// can emit X-RateLimit-* response headers (design/33 1.9).
type QuotaInfo struct {
	Limit      int64
	Remaining  int64
	Reset      time.Time
	RetryAfter time.Duration // zero when the request was admitted
}

// QuotaInformer is the optional richer interface implemented by
// TokenBucketLimiter. The middleware prefers it so that 429 responses
// carry accurate quota headers, and falls back to plain Allow.
type QuotaInformer interface {
	AllowInfo(ctx context.Context, scope Scope, key string) (bool, QuotaInfo, error)
}
