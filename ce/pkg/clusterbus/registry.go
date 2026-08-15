package clusterbus

import (
	"context"
	"errors"
	"fmt"

	"github.com/redis/go-redis/v9"
)

// ErrNotFound marks a device with no live route (loc key absent: the
// device is offline or its index expired, SEC-14).
var ErrNotFound = errors.New("clusterbus: device route not found")

// LocKey builds the Valkey routing-index key for a device. The layout is
// shared with the connector registry (design/31 3.1.7): the key is
// "adc:loc:{tenantID}:{deviceCode}" holding the owning node ID with a TTL
// renewed by heartbeat. clusterbus lives in pkg/ and cannot import
// internal/connector, so the prefix is duplicated here; the two must stay
// in sync (internal/connector/registry.go locKeyPrefix).
func LocKey(tenantID, deviceCode string) string {
	return "adc:loc:" + tenantID + ":" + deviceCode
}

// Getter is the minimal Valkey command surface for the loc lookup. It is
// an interface over (string, error) so tests substitute a map fake
// without constructing redis.StringCmd values.
type Getter interface {
	Get(ctx context.Context, key string) (string, error)
}

// Locator resolves the node currently owning a device session.
type Locator interface {
	Locate(ctx context.Context, tenantID, deviceCode string) (string, error)
}

// NodeRegistry implements Locator over the Valkey routing index
// (design/31 3.1.7). Valkey is only an index: the local hub of the owning
// node stays authoritative (GAP-11).
type NodeRegistry struct {
	get Getter
}

// NewNodeRegistry builds the registry over a go-redis/v9 client.
func NewNodeRegistry(rdb *redis.Client) *NodeRegistry {
	return NewNodeRegistryGetter(redisGetter{rdb: rdb})
}

// NewNodeRegistryGetter builds the registry over an arbitrary Getter
// (tests).
func NewNodeRegistryGetter(g Getter) *NodeRegistry {
	return &NodeRegistry{get: g}
}

// Locate returns the node ID stored in the loc key, or ErrNotFound when
// the key is absent or expired.
func (r *NodeRegistry) Locate(ctx context.Context, tenantID, deviceCode string) (string, error) {
	node, err := r.get.Get(ctx, LocKey(tenantID, deviceCode))
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return "", ErrNotFound
		}
		return "", fmt.Errorf("clusterbus: locate device: %w", err)
	}
	if node == "" {
		return "", ErrNotFound
	}
	return node, nil
}

// redisGetter adapts go-redis/v9 to Getter.
type redisGetter struct {
	rdb *redis.Client
}

func (g redisGetter) Get(ctx context.Context, key string) (string, error) {
	return g.rdb.Get(ctx, key).Result()
}
