package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// tokenBucketScript atomically refills and consumes a token bucket.
// Running the whole read-modify-write inside one Lua script makes the
// operation atomic on the Valkey node, so any number of application
// nodes can share one bucket without racing each other (a GET-then-SET
// pair would lose updates between concurrent callers). The current time
// is passed by the caller in ARGV[3], which requires the application
// nodes to have NTP-synchronized clocks.
//
// An INCR+EXPIRE fixed window (INCR on first hit, EXPIRE the key, deny
// when the counter exceeds the quota) was considered as a degraded
// fallback for environments without scripting and rejected: it is not a
// token bucket — it refills in discrete windows instead of smoothly at
// Rate, and it admits up to twice the quota across a window boundary
// (burst at the end of one window and again at the start of the next).
// If scripting were unavailable, server-side limits are the mitigation,
// not a fixed window.
//
// KEYS[1]  bucket key holding the token balance; its timestamp lives at
//
//	KEYS[1]..":ts"
//
// ARGV[1]  refill rate, tokens per second
// ARGV[2]  burst capacity
// ARGV[3]  current time, unix seconds (float, caller clock)
// ARGV[4]  requested tokens (always 1)
// Returns  {allowed, tokens_remaining, retry_after_seconds}
const tokenBucketScript = `
local rate = tonumber(ARGV[1])
local burst = tonumber(ARGV[2])
local now = tonumber(ARGV[3])
local requested = tonumber(ARGV[4])
local tokens = tonumber(redis.call('GET', KEYS[1]))
local last = tonumber(redis.call('GET', KEYS[1] .. ':ts'))
if tokens == nil then tokens = burst end
if last == nil then last = now end
local delta = now - last
if delta < 0 then delta = 0 end
tokens = math.min(burst, tokens + delta * rate)
local allowed = 0
local retry = 0
if tokens >= requested then
  tokens = tokens - requested
  allowed = 1
else
  retry = math.ceil((requested - tokens) / rate)
end
local ttl = math.ceil(burst / rate) + 1
redis.call('SET', KEYS[1], tostring(tokens), 'EX', ttl)
redis.call('SET', KEYS[1] .. ':ts', tostring(now), 'EX', ttl)
return {allowed, tokens, retry}
`

// Store abstracts the Valkey client behind an Eval seam. miniredis does
// not support Lua scripting, so tests substitute MemStore, which
// implements the same algorithm natively.
type Store interface {
	Eval(ctx context.Context, script string, keys []string, args ...interface{}) (interface{}, error)
}

// ValkeyStore adapts go-redis/v9 to Store. Client.Eval returns *redis.Cmd;
// Result blocks until the reply arrives and decodes the Lua return table
// into a []interface{} of int64/float64 elements.
type ValkeyStore struct {
	client *redis.Client
}

// NewValkeyStore wraps a go-redis client.
func NewValkeyStore(client *redis.Client) *ValkeyStore {
	return &ValkeyStore{client: client}
}

// Eval runs script on the Valkey node and returns the decoded reply.
func (s *ValkeyStore) Eval(ctx context.Context, script string, keys []string, args ...interface{}) (interface{}, error) {
	return s.client.Eval(ctx, script, keys, args...).Result()
}

const keyPrefix = "adc:ratelimit:"

// TokenBucketLimiter enforces per-scope token buckets whose state lives
// in a Store. Because the state lives in Valkey, limits hold across every
// application node (the V1.0 design decision in design/31 3.2.7).
type TokenBucketLimiter struct {
	store     Store
	limits    map[Scope]Limit
	keyPrefix string
	now       func() time.Time // clock hook, overridable in tests
}

// NewTokenBucket builds a limiter over store. A nil limits map selects
// DefaultLimits. Every configured limit is validated before use.
func NewTokenBucket(store Store, limits map[Scope]Limit) (*TokenBucketLimiter, error) {
	if store == nil {
		return nil, errors.New("ratelimit: nil store")
	}
	if limits == nil {
		limits = DefaultLimits()
	}
	for scope, limit := range limits {
		if err := limit.Validate(); err != nil {
			return nil, fmt.Errorf("ratelimit: scope %q: %w", scope, err)
		}
	}
	return &TokenBucketLimiter{
		store:     store,
		limits:    limits,
		keyPrefix: keyPrefix,
		now:       time.Now,
	}, nil
}

// Allow consumes one token for scope/key and reports admission.
func (l *TokenBucketLimiter) Allow(ctx context.Context, scope Scope, key string) (bool, error) {
	allowed, _, err := l.AllowInfo(ctx, scope, key)
	return allowed, err
}

// AllowInfo is Allow plus the post-decision bucket state for headers.
func (l *TokenBucketLimiter) AllowInfo(ctx context.Context, scope Scope, key string) (bool, QuotaInfo, error) {
	limit, ok := l.limits[scope]
	if !ok {
		return false, QuotaInfo{}, fmt.Errorf("ratelimit: no limit configured for scope %q", scope)
	}
	if key == "" {
		return false, QuotaInfo{}, errors.New("ratelimit: refusing to limit an empty key (would share one bucket across all callers)")
	}
	now := l.now()
	raw, err := l.store.Eval(ctx, tokenBucketScript,
		[]string{l.keyPrefix + string(scope) + ":" + key},
		limit.Rate, limit.Burst, float64(now.UnixNano())/1e9, int64(1))
	if err != nil {
		return false, QuotaInfo{}, fmt.Errorf("ratelimit: eval: %w", err)
	}
	allowed, remaining, retry, err := parseEvalResult(raw)
	if err != nil {
		return false, QuotaInfo{}, err
	}
	info := QuotaInfo{
		Limit:     limit.Burst,
		Remaining: int64(math.Floor(remaining)),
	}
	if allowed {
		// Time until the bucket is full again; zero for a full bucket.
		info.Reset = now.Add(time.Duration(math.Ceil((float64(limit.Burst)-remaining)/limit.Rate)) * time.Second)
		return true, info, nil
	}
	if retry < 1 {
		retry = 1
	}
	info.Reset = now.Add(time.Duration(math.Ceil(retry)) * time.Second)
	info.RetryAfter = time.Duration(retry * float64(time.Second))
	return false, info, nil
}

// parseEvalResult decodes the Lua return table. go-redis converts Lua
// numbers to int64 when integral and float64 otherwise, so both types
// (and numeric strings, defensively) are accepted.
func parseEvalResult(raw interface{}) (allowed bool, remaining, retryAfter float64, err error) {
	arr, ok := raw.([]interface{})
	if !ok || len(arr) != 3 {
		return false, 0, 0, fmt.Errorf("ratelimit: unexpected eval result %T, want 3-element table", raw)
	}
	names := [...]string{"allowed", "remaining", "retry_after"}
	vals := make([]float64, 3)
	for i, name := range names {
		if vals[i], err = asFloat(arr[i], name); err != nil {
			return false, 0, 0, err
		}
	}
	return vals[0] != 0, vals[1], vals[2], nil
}

// asFloat coerces a Lua reply element (int64, float64 or numeric string)
// to float64.
func asFloat(v interface{}, name string) (float64, error) {
	switch n := v.(type) {
	case int64:
		return float64(n), nil
	case float64:
		return n, nil
	case string:
		f, err := strconv.ParseFloat(n, 64)
		if err != nil {
			return 0, fmt.Errorf("ratelimit: %s %q is not numeric", name, n)
		}
		return f, nil
	default:
		return 0, fmt.Errorf("ratelimit: %s has unexpected type %T", name, v)
	}
}
