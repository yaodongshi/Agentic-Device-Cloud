package connector

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

// SessionRegistry is the cross-node routing index (design/31 3.1.2).
// Valkey is only an index: the local hub is authoritative (GAP-11).
type SessionRegistry interface {
	// Register writes the loc key (nodeID) with a TTL and adds the device to
	// the tenant's online set.
	Register(ctx context.Context, tenantID, deviceCode, nodeID string, ttl time.Duration) error
	// Heartbeat refreshes the loc key TTL (SEC-14).
	Heartbeat(ctx context.Context, tenantID, deviceCode string) error
	// Unregister deletes the loc key and removes the device from the online
	// set. Offline-event publish is the Agent API's concern (subscriber).
	Unregister(ctx context.Context, tenantID, deviceCode string) error
	// OnlineSet adds (member=true) or removes the device from the online set.
	OnlineSet(ctx context.Context, tenantID, deviceCode string, member bool) error
}

// Valkey key layout (inherited from the PoC, design/31 3.1.7):
//
//	adc:loc:{tenant}:{deviceCode}        String, nodeID, TTL renewed by heartbeat
//	adc:tenant_devices:{tenant}          Set of online deviceCodes
const (
	locKeyPrefix            = "adc:loc:"
	onlineSetPrefix         = "adc:tenant_devices:"
	registryRegisterRetries = 3
	registryRetryBaseDelay  = 50 * time.Millisecond
)

// redisCommands is the minimal command surface used by ValkeyRegistry; it is
// satisfied by *redis.Client and by the in-memory fake used in tests.
type redisCommands interface {
	Set(ctx context.Context, key string, value interface{}, expiration time.Duration) *redis.StatusCmd
	Expire(ctx context.Context, key string, expiration time.Duration) *redis.BoolCmd
	Del(ctx context.Context, keys ...string) *redis.IntCmd
	SAdd(ctx context.Context, key string, members ...interface{}) *redis.IntCmd
	SRem(ctx context.Context, key string, members ...interface{}) *redis.IntCmd
}

// ValkeyRegistry is the SessionRegistry backed by Valkey (go-redis/v9).
type ValkeyRegistry struct {
	rdb redisCommands
	ttl time.Duration // heartbeat renewal TTL (config DeviceTTLSeconds, 90s)
	log *slog.Logger
}

// NewValkeyRegistry builds the registry; ttl is the TTL used by Heartbeat.
func NewValkeyRegistry(rdb redisCommands, ttl time.Duration, log *slog.Logger) *ValkeyRegistry {
	if log == nil {
		log = slog.Default()
	}
	return &ValkeyRegistry{rdb: rdb, ttl: ttl, log: log}
}

func locKey(tenantID, deviceCode string) string {
	return locKeyPrefix + tenantID + ":" + deviceCode
}

func onlineSetKey(tenantID string) string {
	return onlineSetPrefix + tenantID
}

// Register writes the loc key and online-set membership with up to 3 attempts
// and short backoff. If Valkey is unavailable after all attempts the caller
// must refuse the connection (fail-closed, design/31 3.1.9): better an
// unroutable device than a ghost route.
func (r *ValkeyRegistry) Register(ctx context.Context, tenantID, deviceCode, nodeID string, ttl time.Duration) error {
	var lastErr error
	for attempt := 1; attempt <= registryRegisterRetries; attempt++ {
		if err := r.registerOnce(ctx, tenantID, deviceCode, nodeID, ttl); err == nil {
			return nil
		} else {
			lastErr = err
			r.log.Warn("registry register attempt failed",
				"tenant_id", tenantID, "device_id", deviceCode, "attempt", attempt, "err", err)
		}
		delay := registryRetryBaseDelay << (attempt - 1)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return fmt.Errorf("connector: registry register failed after %d attempts: %w",
		registryRegisterRetries, lastErr)
}

func (r *ValkeyRegistry) registerOnce(ctx context.Context, tenantID, deviceCode, nodeID string, ttl time.Duration) error {
	if err := r.rdb.Set(ctx, locKey(tenantID, deviceCode), nodeID, ttl).Err(); err != nil {
		return err
	}
	return r.rdb.SAdd(ctx, onlineSetKey(tenantID), deviceCode).Err()
}

// Heartbeat renews the loc key TTL every 30s from the write pump (SEC-14).
func (r *ValkeyRegistry) Heartbeat(ctx context.Context, tenantID, deviceCode string) error {
	return r.rdb.Expire(ctx, locKey(tenantID, deviceCode), r.ttl).Err()
}

// Unregister removes the loc key and the online-set membership on disconnect.
func (r *ValkeyRegistry) Unregister(ctx context.Context, tenantID, deviceCode string) error {
	if err := r.rdb.Del(ctx, locKey(tenantID, deviceCode)).Err(); err != nil {
		return err
	}
	return r.rdb.SRem(ctx, onlineSetKey(tenantID), deviceCode).Err()
}

// OnlineSet toggles the device membership in the tenant online set.
func (r *ValkeyRegistry) OnlineSet(ctx context.Context, tenantID, deviceCode string, member bool) error {
	if member {
		return r.rdb.SAdd(ctx, onlineSetKey(tenantID), deviceCode).Err()
	}
	return r.rdb.SRem(ctx, onlineSetKey(tenantID), deviceCode).Err()
}
