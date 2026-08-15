package ratelimit

import (
	"context"
	"errors"
	"math"
	"sync"
)

// MemStore is an in-memory Store that implements the token bucket
// algorithm natively. It exists because miniredis does not support Lua
// scripting, so tests exercise the limiter against this reference
// implementation; it is also usable for single-process deployments whose
// limits need not be shared across nodes.
//
// MemStore is not a Lua interpreter: Eval only accepts the exact
// tokenBucketScript constant, which doubles as a regression check that
// the limiter and the reference algorithm agree on the script.
type MemStore struct {
	mu    sync.Mutex
	state map[string]*memBucket
}

type memBucket struct {
	tokens float64
	last   float64 // unix seconds, caller clock
}

// NewMemStore returns an empty in-memory store.
func NewMemStore() *MemStore {
	return &MemStore{state: make(map[string]*memBucket)}
}

// Eval applies the token bucket algorithm for keys[0] using the same
// argument layout as tokenBucketScript (rate, burst, now, requested) and
// returns the same 3-element table shape.
func (s *MemStore) Eval(ctx context.Context, script string, keys []string, args ...interface{}) (interface{}, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if script != tokenBucketScript {
		return nil, errors.New("ratelimit: memstore only evaluates the token bucket script")
	}
	if len(keys) != 1 {
		return nil, errors.New("ratelimit: memstore expects exactly one key")
	}
	if len(args) != 4 {
		return nil, errors.New("ratelimit: memstore expects four args (rate, burst, now, requested)")
	}
	rate, err := asFloat(args[0], "rate")
	if err != nil {
		return nil, err
	}
	burst, err := asFloat(args[1], "burst")
	if err != nil {
		return nil, err
	}
	now, err := asFloat(args[2], "now")
	if err != nil {
		return nil, err
	}
	requested, err := asFloat(args[3], "requested")
	if err != nil {
		return nil, err
	}
	if rate <= 0 || burst <= 0 {
		return nil, errors.New("ratelimit: rate and burst must be positive")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.state[keys[0]]
	if b == nil {
		b = &memBucket{tokens: burst}
		s.state[keys[0]] = b
	}
	delta := now - b.last
	if b.last == 0 {
		delta = 0 // fresh bucket: starts full, no refill on first hit
	}
	if delta < 0 {
		delta = 0 // clock skew guard, mirrors the Lua script
	}
	b.tokens = math.Min(burst, b.tokens+delta*rate)
	b.last = now

	allowed := 0.0
	retry := 0.0
	if b.tokens >= requested {
		b.tokens -= requested
		allowed = 1
	} else {
		retry = math.Ceil((requested - b.tokens) / rate)
	}
	return []interface{}{int64(allowed), b.tokens, retry}, nil
}
