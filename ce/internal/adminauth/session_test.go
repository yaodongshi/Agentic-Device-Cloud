package adminauth

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// fakeValkey is an in-memory implementation of the minimal valkeyCmd
// surface used by ValkeySessionStore; tests never touch a real Valkey.
type fakeValkey struct {
	mu     sync.Mutex
	kv     map[string]string
	ttls   map[string]time.Duration
	getErr error
	setErr error
	delErr error
}

func newFakeValkey() *fakeValkey {
	return &fakeValkey{
		kv:   make(map[string]string),
		ttls: make(map[string]time.Duration),
	}
}

func (f *fakeValkey) Set(ctx context.Context, key string, value interface{}, expiration time.Duration) *redis.StatusCmd {
	cmd := redis.NewStatusCmd(ctx)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.setErr != nil {
		cmd.SetErr(f.setErr)
		return cmd
	}
	f.kv[key] = fmt.Sprint(value)
	f.ttls[key] = expiration
	cmd.SetVal("OK")
	return cmd
}

func (f *fakeValkey) Get(ctx context.Context, key string) *redis.StringCmd {
	cmd := redis.NewStringCmd(ctx)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		cmd.SetErr(f.getErr)
		return cmd
	}
	v, ok := f.kv[key]
	if !ok {
		cmd.SetErr(redis.Nil)
		return cmd
	}
	cmd.SetVal(v)
	return cmd
}

func (f *fakeValkey) Del(ctx context.Context, keys ...string) *redis.IntCmd {
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

func (f *fakeValkey) Eval(ctx context.Context, _ string, keys []string, args ...interface{}) *redis.Cmd {
	cmd := redis.NewCmd(ctx)
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(keys) != 1 || len(args) != 1 {
		cmd.SetErr(errors.New("invalid eval arguments"))
		return cmd
	}
	raw, ok := f.kv[keys[0]]
	if !ok {
		cmd.SetErr(redis.Nil)
		return cmd
	}
	var tx OIDCTransaction
	if err := json.Unmarshal([]byte(raw), &tx); err != nil {
		cmd.SetErr(err)
		return cmd
	}
	if tx.BrowserBinding != fmt.Sprint(args[0]) {
		cmd.SetErr(redis.Nil)
		return cmd
	}
	delete(f.kv, keys[0])
	delete(f.ttls, keys[0])
	cmd.SetVal(raw)
	return cmd
}

func TestNewToken(t *testing.T) {
	// 32 random bytes base64url-encoded without padding: 43 chars.
	token, err := NewToken()
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}
	if len(token) != 43 {
		t.Fatalf("token length = %d, want 43", len(token))
	}
	if !regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`).MatchString(token) {
		t.Fatalf("token %q is not base64url", token)
	}

	other, err := NewToken()
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}
	if token == other {
		t.Fatal("two generated tokens are identical; crypto/rand must yield unique values")
	}
}

func TestHashToken(t *testing.T) {
	h1 := HashToken("token-one")
	if _, err := hex.DecodeString(h1); err != nil || len(h1) != 64 {
		t.Fatalf("HashToken = %q, want 64 hex chars", h1)
	}
	if h1 != HashToken("token-one") {
		t.Fatal("HashToken is not deterministic")
	}
	if h1 == HashToken("token-two") {
		t.Fatal("HashToken collided across different tokens")
	}
}

func TestValkeySessionStoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	store := NewValkeySessionStore(newFakeValkey())

	expires := time.Now().Add(time.Hour).Truncate(time.Second)
	sess := &Session{
		TokenHash:    HashToken("secret-token"),
		UserID:       "u_9f8e7d6c",
		TenantID:     "t_1a2b3c4d",
		AuthzVersion: 7,
		Roles:        []string{"tenant_admin", "auditor"},
		ExpiresAt:    expires,
	}
	if err := store.Create(ctx, sess, SessionTTL); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := store.Get(ctx, sess.TokenHash)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.UserID != sess.UserID || got.TenantID != sess.TenantID || got.AuthzVersion != sess.AuthzVersion ||
		got.TokenHash != sess.TokenHash || !got.ExpiresAt.Equal(expires) {
		t.Fatalf("Get = %+v, want %+v", got, sess)
	}
	if len(got.Roles) != 2 || got.Roles[0] != "tenant_admin" || got.Roles[1] != "auditor" {
		t.Fatalf("Get roles = %v, want [tenant_admin auditor]", got.Roles)
	}
}

func TestValkeySessionStoreKeyLayout(t *testing.T) {
	ctx := context.Background()
	rdb := newFakeValkey()
	store := NewValkeySessionStore(rdb)

	hash := HashToken("layout-token")
	sess := &Session{TokenHash: hash, UserID: "u1", ExpiresAt: time.Now().Add(time.Hour)}
	if err := store.Create(ctx, sess, 3*time.Hour); err != nil {
		t.Fatalf("Create: %v", err)
	}

	rdb.mu.Lock()
	defer rdb.mu.Unlock()
	key := "adc:session:" + hash
	if _, ok := rdb.kv[key]; !ok {
		t.Fatalf("session not stored under %q", key)
	}
	if rdb.ttls[key] != 3*time.Hour {
		t.Fatalf("TTL = %v, want 3h", rdb.ttls[key])
	}
	if rdb.kv[key] == "" {
		t.Fatal("stored value is empty")
	}
}

func TestValkeySessionStoreGetMissing(t *testing.T) {
	store := NewValkeySessionStore(newFakeValkey())
	_, err := store.Get(context.Background(), HashToken("never-created"))
	if !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("Get(missing) = %v, want ErrSessionNotFound", err)
	}
}

func TestValkeySessionStoreGetExpired(t *testing.T) {
	ctx := context.Background()
	store := NewValkeySessionStore(newFakeValkey())

	sess := &Session{
		TokenHash: HashToken("expired-token"),
		UserID:    "u1",
		ExpiresAt: time.Now().Add(-time.Minute),
	}
	if err := store.Create(ctx, sess, SessionTTL); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := store.Get(ctx, sess.TokenHash); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("Get(expired) = %v, want ErrSessionNotFound", err)
	}
}

func TestValkeySessionStoreDeleteIdempotent(t *testing.T) {
	ctx := context.Background()
	store := NewValkeySessionStore(newFakeValkey())

	hash := HashToken("delete-token")
	sess := &Session{TokenHash: hash, UserID: "u1", ExpiresAt: time.Now().Add(time.Hour)}
	if err := store.Create(ctx, sess, SessionTTL); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.Delete(ctx, hash); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := store.Get(ctx, hash); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("Get after Delete = %v, want ErrSessionNotFound", err)
	}
	if err := store.Delete(ctx, hash); err != nil {
		t.Fatalf("second Delete = %v, want nil (idempotent)", err)
	}
}

func TestValkeySessionStoreCreateRejectsEmptyHash(t *testing.T) {
	store := NewValkeySessionStore(newFakeValkey())
	if err := store.Create(context.Background(), &Session{UserID: "u1"}, SessionTTL); err == nil {
		t.Fatal("Create with empty TokenHash succeeded, want error")
	}
}

func TestValkeySessionStoreTransportErrors(t *testing.T) {
	ctx := context.Background()

	store := NewValkeySessionStore(newFakeValkey())
	if err := store.Create(ctx, &Session{TokenHash: HashToken("t")}, SessionTTL); err != nil {
		t.Fatalf("Create: %v", err)
	}

	rdb := newFakeValkey()
	rdb.getErr = errors.New("valkey down")
	store = NewValkeySessionStore(rdb)
	if _, err := store.Get(ctx, HashToken("t")); err == nil || errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("Get with transport error = %v, want wrapped non-sentinel error", err)
	}

	rdb = newFakeValkey()
	rdb.setErr = errors.New("valkey down")
	store = NewValkeySessionStore(rdb)
	if err := store.Create(ctx, &Session{TokenHash: HashToken("t")}, SessionTTL); err == nil {
		t.Fatal("Create with transport error succeeded, want error")
	}

	rdb = newFakeValkey()
	rdb.delErr = errors.New("valkey down")
	store = NewValkeySessionStore(rdb)
	if err := store.Delete(ctx, HashToken("t")); err == nil {
		t.Fatal("Delete with transport error succeeded, want error")
	}
}
