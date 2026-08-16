package alerts

// Alert aggregation dedup (FR-017 异常场景: 告警风暴需聚合去重). A
// Deduper claims a key for a window; the claim succeeds only once per
// window, so the same rule re-alerts at most every DedupWindow (5 minutes
// in the B2 slice). Valkey-backed claims coordinate across processes;
// MemoryDeduper is the single-process fallback and the test seam.

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// DefaultDedupWindow is the FR-017 aggregation window: the same rule emits
// at most one alert per 5 minutes.
const DefaultDedupWindow = 5 * time.Minute

// dedupKeyPrefix is the Valkey key namespace.
const dedupKeyPrefix = "adc:alerts:dedup:"

// DedupKey renders the Valkey key for one rule.
func DedupKey(ruleID string) string { return dedupKeyPrefix + ruleID }

// Deduper claims dedup windows. Claim returns (true, nil) when the caller
// won the claim and should fire; (false, nil) when the key was claimed
// within the window; (false, err) when the backing store is unreachable —
// the evaluator treats that as fail-open (alert delivery outranks storm
// suppression) while its local last-fired guard still caps the rate.
type Deduper interface {
	Claim(ctx context.Context, key string, ttl time.Duration) (bool, error)
}

// ValkeyDeduper claims via SET NX EX, so claims expire automatically and
// every process sees the same window (design/31 3.2.7 store-the-state-
// in-Valkey pattern).
type ValkeyDeduper struct {
	client *redis.Client
}

// NewValkeyDeduper wraps a go-redis client (shared with the other
// modules' Valkey usage; keys are namespaced per concern).
func NewValkeyDeduper(client *redis.Client) *ValkeyDeduper {
	return &ValkeyDeduper{client: client}
}

// Claim implements Deduper.
func (d *ValkeyDeduper) Claim(ctx context.Context, key string, ttl time.Duration) (bool, error) {
	ok, err := d.client.SetNX(ctx, key, "1", ttl).Result()
	if err != nil {
		return false, fmt.Errorf("alerts: dedup setnx: %w", err)
	}
	return ok, nil
}

// MemoryDeduper keeps claims in-process (single node, tests).
type MemoryDeduper struct {
	mu    sync.Mutex
	now   func() time.Time
	until map[string]time.Time
}

// NewMemoryDeduper builds an empty in-process deduper.
func NewMemoryDeduper() *MemoryDeduper {
	return &MemoryDeduper{now: time.Now, until: map[string]time.Time{}}
}

// Claim implements Deduper.
func (m *MemoryDeduper) Claim(_ context.Context, key string, ttl time.Duration) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	if exp, ok := m.until[key]; ok && now.Before(exp) {
		return false, nil
	}
	m.until[key] = now.Add(ttl)
	return true, nil
}
