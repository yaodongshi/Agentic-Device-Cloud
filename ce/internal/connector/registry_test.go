package connector

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// fakeRedis is an in-memory implementation of the minimal redisCommands
// surface used by ValkeyRegistry; no external test dependencies are needed.
type fakeRedis struct {
	mu   sync.Mutex
	kv   map[string]string
	ttls map[string]time.Duration
	sets map[string]map[string]struct{}

	setFails   int // fail the next N Set calls (retry path)
	expireErr  error
	delErr     error
	sremErr    error
	expireSeen int
	lastExpire time.Duration
}

func newFakeRedis() *fakeRedis {
	return &fakeRedis{
		kv:   make(map[string]string),
		ttls: make(map[string]time.Duration),
		sets: make(map[string]map[string]struct{}),
	}
}

func (f *fakeRedis) Set(ctx context.Context, key string, value interface{}, expiration time.Duration) *redis.StatusCmd {
	cmd := redis.NewStatusCmd(ctx)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.setFails > 0 {
		f.setFails--
		cmd.SetErr(errors.New("fake: set failed"))
		return cmd
	}
	f.kv[key] = fmt.Sprint(value)
	f.ttls[key] = expiration
	cmd.SetVal("OK")
	return cmd
}

func (f *fakeRedis) Expire(ctx context.Context, key string, expiration time.Duration) *redis.BoolCmd {
	cmd := redis.NewBoolCmd(ctx)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.expireSeen++
	f.lastExpire = expiration
	if f.expireErr != nil {
		cmd.SetErr(f.expireErr)
		return cmd
	}
	if _, ok := f.kv[key]; !ok {
		cmd.SetVal(false)
		return cmd
	}
	f.ttls[key] = expiration
	cmd.SetVal(true)
	return cmd
}

func (f *fakeRedis) Del(ctx context.Context, keys ...string) *redis.IntCmd {
	cmd := redis.NewIntCmd(ctx)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.delErr != nil {
		cmd.SetErr(f.delErr)
		return cmd
	}
	n := int64(0)
	for _, k := range keys {
		if _, ok := f.kv[k]; ok {
			delete(f.kv, k)
			delete(f.ttls, k)
			n++
		}
	}
	cmd.SetVal(n)
	return cmd
}

func (f *fakeRedis) SAdd(ctx context.Context, key string, members ...interface{}) *redis.IntCmd {
	cmd := redis.NewIntCmd(ctx)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sets[key] == nil {
		f.sets[key] = make(map[string]struct{})
	}
	n := int64(0)
	for _, m := range members {
		s := fmt.Sprint(m)
		if _, ok := f.sets[key][s]; !ok {
			f.sets[key][s] = struct{}{}
			n++
		}
	}
	cmd.SetVal(n)
	return cmd
}

func (f *fakeRedis) SRem(ctx context.Context, key string, members ...interface{}) *redis.IntCmd {
	cmd := redis.NewIntCmd(ctx)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sremErr != nil {
		cmd.SetErr(f.sremErr)
		return cmd
	}
	n := int64(0)
	for _, m := range members {
		s := fmt.Sprint(m)
		if _, ok := f.sets[key][s]; ok {
			delete(f.sets[key], s)
			n++
		}
	}
	if len(f.sets[key]) == 0 {
		delete(f.sets, key)
	}
	cmd.SetVal(n)
	return cmd
}

// TestKeyLayout verifies the Valkey key naming contract (design/31 3.1.7).
func TestKeyLayout(t *testing.T) {
	if got, want := locKey("t1", "dev-1"), "adc:loc:t1:dev-1"; got != want {
		t.Errorf("locKey = %q, want %q", got, want)
	}
	if got, want := onlineSetKey("t1"), "adc:tenant_devices:t1"; got != want {
		t.Errorf("onlineSetKey = %q, want %q", got, want)
	}
}

// TestRegisterWritesLocKeyAndOnlineSet verifies Register writes the loc key
// with the node ID and TTL and adds the device to the tenant online set.
func TestRegisterWritesLocKeyAndOnlineSet(t *testing.T) {
	f := newFakeRedis()
	r := NewValkeyRegistry(f, 90*time.Second, nil)

	if err := r.Register(context.Background(), "t1", "dev-1", "node-1", 90*time.Second); err != nil {
		t.Fatalf("Register: %v", err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if got := f.kv["adc:loc:t1:dev-1"]; got != "node-1" {
		t.Errorf("loc value = %q, want node-1", got)
	}
	if _, ok := f.sets["adc:tenant_devices:t1"]["dev-1"]; !ok {
		t.Errorf("online set missing dev-1")
	}
}

// TestRegisterRetriesThenSucceeds verifies the transient-failure retry path
// (design/31 3.1.9): a failed first attempt is retried and succeeds.
func TestRegisterRetriesThenSucceeds(t *testing.T) {
	f := newFakeRedis()
	f.setFails = 2 // fail attempts 1 and 2, succeed on 3
	r := NewValkeyRegistry(f, 90*time.Second, nil)

	if err := r.Register(context.Background(), "t1", "dev-1", "node-1", 90*time.Second); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if f.setFails != 0 {
		t.Errorf("setFails = %d, want 0", f.setFails)
	}
}

// TestRegisterFailsClosedAfterRetries verifies fail-closed semantics: when
// Valkey stays down, Register gives up after 3 attempts (design/31 3.1.9:
// 宁可设备暂不可路由,不产生幽灵路由).
func TestRegisterFailsClosedAfterRetries(t *testing.T) {
	f := newFakeRedis()
	f.setFails = 99
	r := NewValkeyRegistry(f, 90*time.Second, nil)

	err := r.Register(context.Background(), "t1", "dev-1", "node-1", 90*time.Second)
	if err == nil {
		t.Fatal("Register succeeded, want error")
	}
	if !strings.Contains(err.Error(), "3 attempts") {
		t.Errorf("err = %v, want mention of 3 attempts", err)
	}
}

// TestRegisterContextCancellation verifies Register aborts with ctx.Err() when
// the caller cancels during retries.
func TestRegisterContextCancellation(t *testing.T) {
	f := newFakeRedis()
	f.setFails = 99
	r := NewValkeyRegistry(f, 90*time.Second, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	err := r.Register(ctx, "t1", "dev-1", "node-1", 90*time.Second)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
}

// TestHeartbeatRefreshesTTL verifies the SEC-14 renewal: Heartbeat issues
// EXPIRE on the loc key with the registry TTL.
func TestHeartbeatRefreshesTTL(t *testing.T) {
	f := newFakeRedis()
	r := NewValkeyRegistry(f, 90*time.Second, nil)
	if err := r.Register(context.Background(), "t1", "dev-1", "node-1", 90*time.Second); err != nil {
		t.Fatalf("Register: %v", err)
	}

	f.mu.Lock()
	f.expireSeen = 0
	f.mu.Unlock()

	if err := r.Heartbeat(context.Background(), "t1", "dev-1"); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.expireSeen != 1 {
		t.Errorf("expire calls = %d, want 1", f.expireSeen)
	}
	if f.lastExpire != 90*time.Second {
		t.Errorf("renewed TTL = %v, want 90s", f.lastExpire)
	}
}

// TestHeartbeatPropagatesError verifies Heartbeat surfaces registry errors.
func TestHeartbeatPropagatesError(t *testing.T) {
	f := newFakeRedis()
	f.expireErr = errors.New("valkey down")
	r := NewValkeyRegistry(f, 90*time.Second, nil)

	if err := r.Heartbeat(context.Background(), "t1", "dev-1"); err == nil {
		t.Fatal("Heartbeat succeeded, want error")
	}
}

// TestUnregisterDeletesKeys verifies the disconnect cleanup (SEC-14): the loc
// key is deleted and the device leaves the online set.
func TestUnregisterDeletesKeys(t *testing.T) {
	f := newFakeRedis()
	r := NewValkeyRegistry(f, 90*time.Second, nil)
	if err := r.Register(context.Background(), "t1", "dev-1", "node-1", 90*time.Second); err != nil {
		t.Fatalf("Register: %v", err)
	}

	if err := r.Unregister(context.Background(), "t1", "dev-1"); err != nil {
		t.Fatalf("Unregister: %v", err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.kv["adc:loc:t1:dev-1"]; ok {
		t.Errorf("loc key still present after Unregister")
	}
	if _, ok := f.sets["adc:tenant_devices:t1"]; ok {
		t.Errorf("online set still present after Unregister")
	}
}

// TestUnregisterPropagatesError verifies Unregister surfaces registry errors
// from both the DEL and the SREM step.
func TestUnregisterPropagatesError(t *testing.T) {
	f := newFakeRedis()
	f.delErr = errors.New("valkey down")
	r := NewValkeyRegistry(f, 90*time.Second, nil)
	if err := r.Unregister(context.Background(), "t1", "dev-1"); err == nil {
		t.Error("Unregister succeeded with failing DEL, want error")
	}

	f = newFakeRedis()
	f.sremErr = errors.New("valkey down")
	r = NewValkeyRegistry(f, 90*time.Second, nil)
	if err := r.Unregister(context.Background(), "t1", "dev-1"); err == nil {
		t.Error("Unregister succeeded with failing SREM, want error")
	}
}

// TestOnlineSetToggle verifies the OnlineSet member toggle.
func TestOnlineSetToggle(t *testing.T) {
	f := newFakeRedis()
	r := NewValkeyRegistry(f, 90*time.Second, nil)

	if err := r.OnlineSet(context.Background(), "t1", "dev-1", true); err != nil {
		t.Fatalf("OnlineSet(true): %v", err)
	}
	f.mu.Lock()
	_, ok := f.sets["adc:tenant_devices:t1"]["dev-1"]
	f.mu.Unlock()
	if !ok {
		t.Fatal("dev-1 not added to online set")
	}

	if err := r.OnlineSet(context.Background(), "t1", "dev-1", false); err != nil {
		t.Fatalf("OnlineSet(false): %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.sets["adc:tenant_devices:t1"]; ok {
		t.Fatal("online set not removed after OnlineSet(false)")
	}
}

// TestHeartbeatUnknownKeyIsHarmless verifies Heartbeat on a never-registered
// key does not error (EXPIRE on missing key returns false).
func TestHeartbeatUnknownKeyIsHarmless(t *testing.T) {
	f := newFakeRedis()
	r := NewValkeyRegistry(f, 90*time.Second, nil)

	if err := r.Heartbeat(context.Background(), "t9", "ghost"); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
}
