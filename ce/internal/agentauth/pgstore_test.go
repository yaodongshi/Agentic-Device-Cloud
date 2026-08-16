package agentauth

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v4"
)

// TestPGKeyStoreGetByKeyID covers the PG agent key loader: enabled key with
// scopes, unknown key id, revoked key, suspended tenant, and a corrupted
// secret hash.
func TestPGKeyStoreGetByKeyID(t *testing.T) {
	ctx := context.Background()
	secretHex := HashSecret("abcdef0123456789abcdef0123456789")

	t.Run("active key", func(t *testing.T) {
		m, err := pgxmock.NewPool()
		if err != nil {
			t.Fatalf("pgxmock.NewPool: %v", err)
		}
		defer m.Close()
		m.ExpectQuery(regexp.QuoteMeta(getByKeyIDSQL)).
			WithArgs(HashKeyID("abc12345")).
			WillReturnRows(pgxmock.NewRows([]string{"id", "tenant_id", "agent_id", "secret_hash", "scope_mode", "scopes", "expires_at", "revoked_at", "tenant_status"}).
				AddRow("k-1", "t1", "demo-agent", secretHex, "ALLOW_LIST", []byte(`["*"]`), nil, nil, "ACTIVE"))
		store := NewPGKeyStore(m)
		k, err := store.GetByKeyID(ctx, "abc12345")
		if err != nil {
			t.Fatalf("GetByKeyID: %v", err)
		}
		if !k.Enabled || k.KeyID != "abc12345" || k.TenantID != "t1" || len(k.Scopes) != 1 {
			t.Fatalf("unexpected key: %+v", k)
		}
		if err := m.ExpectationsWereMet(); err != nil {
			t.Fatalf("unmet expectations: %v", err)
		}
	})

	t.Run("unknown key id", func(t *testing.T) {
		m, err := pgxmock.NewPool()
		if err != nil {
			t.Fatalf("pgxmock.NewPool: %v", err)
		}
		defer m.Close()
		m.ExpectQuery(regexp.QuoteMeta(getByKeyIDSQL)).
			WithArgs(HashKeyID("deadbeef")).
			WillReturnRows(pgxmock.NewRows([]string{"id", "tenant_id", "agent_id", "secret_hash", "scope_mode", "scopes", "expires_at", "revoked_at", "tenant_status"}))
		store := NewPGKeyStore(m)
		if _, err := store.GetByKeyID(ctx, "deadbeef"); !errors.Is(err, ErrKeyNotFound) {
			t.Fatalf("want ErrKeyNotFound, got %v", err)
		}
	})

	t.Run("revoked key disabled", func(t *testing.T) {
		m, err := pgxmock.NewPool()
		if err != nil {
			t.Fatalf("pgxmock.NewPool: %v", err)
		}
		defer m.Close()
		now := "2026-01-01T00:00:00Z"
		_ = now
		m.ExpectQuery(regexp.QuoteMeta(getByKeyIDSQL)).
			WithArgs(HashKeyID("abc12345")).
			WillReturnRows(pgxmock.NewRows([]string{"id", "tenant_id", "agent_id", "secret_hash", "scope_mode", "scopes", "expires_at", "revoked_at", "tenant_status"}).
				AddRow("k-1", "t1", "demo-agent", secretHex, "ALLOW_LIST", []byte(`[]`), nil, timePtr(), "ACTIVE"))
		store := NewPGKeyStore(m)
		k, err := store.GetByKeyID(ctx, "abc12345")
		if err != nil {
			t.Fatalf("GetByKeyID: %v", err)
		}
		if k.Enabled {
			t.Fatal("revoked key must be disabled")
		}
	})

	t.Run("suspended tenant disables key", func(t *testing.T) {
		m, err := pgxmock.NewPool()
		if err != nil {
			t.Fatalf("pgxmock.NewPool: %v", err)
		}
		defer m.Close()
		m.ExpectQuery(regexp.QuoteMeta(getByKeyIDSQL)).
			WithArgs(HashKeyID("abc12345")).
			WillReturnRows(pgxmock.NewRows([]string{"id", "tenant_id", "agent_id", "secret_hash", "scope_mode", "scopes", "expires_at", "revoked_at", "tenant_status"}).
				AddRow("k-1", "t1", "demo-agent", secretHex, "ALLOW_LIST", []byte(`[]`), nil, nil, "SUSPENDED"))
		store := NewPGKeyStore(m)
		k, err := store.GetByKeyID(ctx, "abc12345")
		if err != nil {
			t.Fatalf("GetByKeyID: %v", err)
		}
		if k.Enabled {
			t.Fatal("suspended tenant must disable its keys")
		}
	})

	t.Run("corrupt secret hash fails", func(t *testing.T) {
		m, err := pgxmock.NewPool()
		if err != nil {
			t.Fatalf("pgxmock.NewPool: %v", err)
		}
		defer m.Close()
		m.ExpectQuery(regexp.QuoteMeta(getByKeyIDSQL)).
			WithArgs(HashKeyID("abc12345")).
			WillReturnRows(pgxmock.NewRows([]string{"id", "tenant_id", "agent_id", "secret_hash", "scope_mode", "scopes", "expires_at", "revoked_at", "tenant_status"}).
				AddRow("k-1", "t1", "demo-agent", "not-hex", "ALLOW_LIST", []byte(`[]`), nil, nil, "ACTIVE"))
		store := NewPGKeyStore(m)
		if _, err := store.GetByKeyID(ctx, "abc12345"); err == nil {
			t.Fatal("corrupt secret hash must error")
		}
	})
}

func timePtr() *time.Time {
	t := parseTime("2026-01-01T00:00:00Z")
	return &t
}

func parseTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339, s)
	return t
}

var _ = pgx.ErrNoRows
