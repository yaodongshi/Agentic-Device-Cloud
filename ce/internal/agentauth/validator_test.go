package agentauth

import (
	"context"
	"crypto/sha256"
	"errors"
	"reflect"
	"testing"
	"time"
)

const (
	testKeyID   = "3c4d5e6f"
	testSecret  = "xH7kP2mQ9vL4nB8tR6wY"
	testTenant  = "f1a2b3c4-1111-4222-8333-000000000001"
	testAgentID = "ag-7f9a"
)

var errStoreDown = errors.New("agentauth test: store down")

// testToken builds "adc_<keyID>_<secret>".
func testToken(keyID, secret string) string {
	return tokenPrefix + keyID + "_" + secret
}

// mockKeyStore is the repo-layer mock (KeyStore interface).
type mockKeyStore struct {
	keys map[string]*AgentKey
	err  error
}

func (m *mockKeyStore) GetByKeyID(_ context.Context, keyID string) (*AgentKey, error) {
	if m.err != nil {
		return nil, m.err
	}
	key, ok := m.keys[keyID]
	if !ok {
		return nil, ErrKeyNotFound
	}
	return key, nil
}

// validKey returns a key valid for testSecret with scopes populated.
func validKey() *AgentKey {
	sum := sha256.Sum256([]byte(testSecret))
	return &AgentKey{
		KeyID:      testKeyID,
		TenantID:   testTenant,
		AgentID:    testAgentID,
		ScopeMode:  ScopeModeAllowList,
		Scopes:     []string{"tools.list", "cnc-lathe-01__get_spindle_status"},
		ExpiresAt:  time.Now().Add(24 * time.Hour),
		Enabled:    true,
		SecretHash: sum[:],
	}
}

func keyWith(k *AgentKey, mutate func(*AgentKey)) *AgentKey {
	cp := *k
	cp.Scopes = append([]string(nil), k.Scopes...)
	mutate(&cp)
	return &cp
}

func storeWith(k *AgentKey) *mockKeyStore {
	return &mockKeyStore{keys: map[string]*AgentKey{k.KeyID: k}}
}

func TestValidate(t *testing.T) {
	expired := time.Now().Add(-time.Hour)
	valid := validKey()
	expected := &Principal{
		KeyID:    testKeyID,
		TenantID: testTenant,
		AgentID:  testAgentID,
		Scopes:   []string{"tools.list", "cnc-lathe-01__get_spindle_status"},
	}

	tests := []struct {
		name    string
		token   string
		store   KeyStore
		wantErr error
		want    *Principal
	}{
		{
			name:  "valid key",
			token: testToken(testKeyID, testSecret),
			store: storeWith(valid),
			want:  expected,
		},
		{
			name:    "wrong secret",
			token:   testToken(testKeyID, "Yz9kQ2mR7vL4nB8tR6wYxH7"),
			store:   storeWith(valid),
			wantErr: ErrBadSecret,
		},
		{
			name:    "expired key",
			token:   testToken(testKeyID, testSecret),
			store:   storeWith(keyWith(valid, func(k *AgentKey) { k.ExpiresAt = expired })),
			wantErr: ErrKeyExpired,
		},
		{
			name:    "disabled key",
			token:   testToken(testKeyID, testSecret),
			store:   storeWith(keyWith(valid, func(k *AgentKey) { k.Enabled = false })),
			wantErr: ErrKeyDisabled,
		},
		{
			name:    "unknown key id",
			token:   testToken("deadbeef", testSecret),
			store:   storeWith(valid),
			wantErr: ErrKeyNotFound,
		},
		{
			name:    "malformed token: missing prefix",
			token:   "key_3c4d5e6f_" + testSecret,
			store:   storeWith(valid),
			wantErr: ErrKeyNotFound,
		},
		{
			name:    "malformed token: bad key id charset",
			token:   testToken("3C4D5E6F", testSecret),
			store:   storeWith(valid),
			wantErr: ErrKeyNotFound,
		},
		{
			name:    "malformed token: short secret",
			token:   testToken(testKeyID, "short"),
			store:   storeWith(valid),
			wantErr: ErrKeyNotFound,
		},
		{
			name:    "malformed token: underscore in secret",
			token:   testToken(testKeyID, "xH7kP2m_Q9vL4nB8tR6wY"),
			store:   storeWith(valid),
			wantErr: ErrKeyNotFound,
		},
		{
			name:    "malformed token: no separator",
			token:   "adc_3c4d5e6f",
			store:   storeWith(valid),
			wantErr: ErrKeyNotFound,
		},
		{
			name:  "never-expiring key",
			token: testToken(testKeyID, testSecret),
			store: storeWith(keyWith(valid, func(k *AgentKey) { k.ExpiresAt = time.Time{} })),
			want:  expected,
		},
		{
			name:    "store transport error",
			token:   testToken(testKeyID, testSecret),
			store:   &mockKeyStore{err: errStoreDown},
			wantErr: errStoreDown,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := NewValidator(tt.store)
			got, err := v.Validate(context.Background(), tt.token)
			if tt.wantErr != nil {
				if err == nil {
					t.Fatalf("Validate() error = nil, want errors.Is(err, %v)", tt.wantErr)
				}
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("Validate() error = %v, want errors.Is(err, %v)", err, tt.wantErr)
				}
				if got != nil {
					t.Fatalf("Validate() principal = %+v, want nil on error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Validate() unexpected error: %v", err)
			}
			if got == nil {
				t.Fatal("Validate() principal = nil, want non-nil")
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Validate() principal = %+v, want %+v", *got, *tt.want)
			}
		})
	}
}

func TestValidateExpiryBoundary(t *testing.T) {
	fixed := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)

	at := func(exp time.Time) *Validator {
		key := keyWith(validKey(), func(k *AgentKey) { k.ExpiresAt = exp })
		return &Validator{store: storeWith(key), now: func() time.Time { return fixed }}
	}

	if _, err := at(fixed).Validate(context.Background(), testToken(testKeyID, testSecret)); !errors.Is(err, ErrKeyExpired) {
		t.Fatalf("expires exactly at now: error = %v, want ErrKeyExpired", err)
	}
	if _, err := at(fixed.Add(-time.Nanosecond)).Validate(context.Background(), testToken(testKeyID, testSecret)); !errors.Is(err, ErrKeyExpired) {
		t.Fatalf("expired 1ns ago: error = %v, want ErrKeyExpired", err)
	}
	if _, err := at(fixed.Add(time.Second)).Validate(context.Background(), testToken(testKeyID, testSecret)); err != nil {
		t.Fatalf("expires in 1s: unexpected error %v", err)
	}
}

func TestValidateScopesAreCopied(t *testing.T) {
	store := storeWith(validKey())
	v := NewValidator(store)

	p, err := v.Validate(context.Background(), testToken(testKeyID, testSecret))
	if err != nil {
		t.Fatalf("Validate() error: %v", err)
	}
	p.Scopes[0] = "mutated"
	if got := store.keys[testKeyID].Scopes[0]; got == "mutated" {
		t.Fatal("Principal.Scopes aliases the store slice; mutation leaked back")
	}
}

func TestHashFunctions(t *testing.T) {
	if HashKeyID(testKeyID) != HashKeyID(testKeyID) {
		t.Fatal("HashKeyID is not deterministic")
	}
	if HashSecret(testSecret) != HashSecret(testSecret) {
		t.Fatal("HashSecret is not deterministic")
	}
	if len(HashKeyID(testKeyID)) != sha256.Size*2 {
		t.Fatalf("HashKeyID length = %d, want %d", len(HashKeyID(testKeyID)), sha256.Size*2)
	}
	// The key id hash covers the full display prefix including "adc_",
	// matching key_prefix in the database.
	if HashKeyID(testKeyID) != HashSecret(tokenPrefix+testKeyID) {
		t.Fatal("key id hash must equal the hash of the display prefix")
	}
}

func TestParseToken(t *testing.T) {
	keyID, secret, err := parseToken(testToken(testKeyID, testSecret))
	if err != nil {
		t.Fatalf("parseToken() error: %v", err)
	}
	if keyID != testKeyID || secret != testSecret {
		t.Fatalf("parseToken() = (%q, %q), want (%q, %q)", keyID, secret, testKeyID, testSecret)
	}
	if _, _, err := parseToken(""); err == nil {
		t.Fatal("parseToken(\"\") error = nil, want error")
	}
}
