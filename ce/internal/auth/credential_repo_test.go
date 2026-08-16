package auth

import (
	"context"
	"errors"
	"regexp"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v4"
)

// TestCredentialRepoLoad exercises the PG credential loader: hmac rows are
// KEK-decrypted, token rows pass the stored hash through, frozen/retired
// devices report disabled, and unknown codes fail closed.
func TestCredentialRepoLoad(t *testing.T) {
	kek := []byte("0123456789abcdef0123456789abcdef") // 32 bytes

	t.Run("hmac device decrypts", func(t *testing.T) {
		m, err := pgxmock.NewPool()
		if err != nil {
			t.Fatalf("pgxmock.NewPool: %v", err)
		}
		defer m.Close()
		enc, err := EncryptSecret("plain-secret", kek)
		if err != nil {
			t.Fatalf("EncryptSecret: %v", err)
		}
		m.ExpectQuery(regexp.QuoteMeta(`SELECT id, tenant_id, auth_type, credential_hash, status
	           FROM adc_devices
	           WHERE device_code = $1 AND deleted_at IS NULL`)).
			WithArgs("dev-1").
			WillReturnRows(pgxmock.NewRows([]string{"id", "tenant_id", "auth_type", "credential_hash", "status"}).
				AddRow("11111111-2222-3333-4444-555555555555", "t1", AuthTypeHMAC, enc, "ONLINE"))
		repo := NewCredentialRepo(m, kek)
		cred, err := repo.Load(context.Background(), "dev-1")
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cred.SecretHash != "plain-secret" || !cred.Enabled || cred.DeviceCode != "dev-1" {
			t.Fatalf("unexpected credential: %+v", cred)
		}
		if err := m.ExpectationsWereMet(); err != nil {
			t.Fatalf("unmet expectations: %v", err)
		}
	})

	t.Run("token device passes hash through", func(t *testing.T) {
		m, err := pgxmock.NewPool()
		if err != nil {
			t.Fatalf("pgxmock.NewPool: %v", err)
		}
		defer m.Close()
		m.ExpectQuery(regexp.QuoteMeta(`SELECT id, tenant_id, auth_type, credential_hash, status
	           FROM adc_devices
	           WHERE device_code = $1 AND deleted_at IS NULL`)).
			WithArgs("dev-2").
			WillReturnRows(pgxmock.NewRows([]string{"id", "tenant_id", "auth_type", "credential_hash", "status"}).
				AddRow("id-2", "t1", AuthTypeToken, "hash-pass", "FROZEN"))
		repo := NewCredentialRepo(m, kek)
		cred, err := repo.Load(context.Background(), "dev-2")
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cred.SecretHash != "hash-pass" {
			t.Fatalf("token hash must pass through: %+v", cred)
		}
		if cred.Enabled {
			t.Fatal("frozen device must be disabled")
		}
	})

	t.Run("unknown device fails closed", func(t *testing.T) {
		m, err := pgxmock.NewPool()
		if err != nil {
			t.Fatalf("pgxmock.NewPool: %v", err)
		}
		defer m.Close()
		m.ExpectQuery(regexp.QuoteMeta(`SELECT id, tenant_id, auth_type, credential_hash, status
	           FROM adc_devices
	           WHERE device_code = $1 AND deleted_at IS NULL`)).
			WithArgs("ghost").
			WillReturnRows(pgxmock.NewRows([]string{"id", "tenant_id", "auth_type", "credential_hash", "status"}))
		repo := NewCredentialRepo(m, kek)
		if _, err := repo.Load(context.Background(), "ghost"); !errors.Is(err, ErrCredentialNotFound) {
			t.Fatalf("want ErrCredentialNotFound, got %v", err)
		}
	})

	t.Run("wrong kek fails closed", func(t *testing.T) {
		m, err := pgxmock.NewPool()
		if err != nil {
			t.Fatalf("pgxmock.NewPool: %v", err)
		}
		defer m.Close()
		m.ExpectQuery(regexp.QuoteMeta(`SELECT id, tenant_id, auth_type, credential_hash, status
	           FROM adc_devices
	           WHERE device_code = $1 AND deleted_at IS NULL`)).
			WithArgs("dev-3").
			WillReturnRows(pgxmock.NewRows([]string{"id", "tenant_id", "auth_type", "credential_hash", "status"}).
				AddRow("id-3", "t1", AuthTypeHMAC, "not-a-ciphertext", "ONLINE"))
		repo := NewCredentialRepo(m, kek)
		if _, err := repo.Load(context.Background(), "dev-3"); err == nil {
			t.Fatal("undecryptable hmac key must fail closed")
		}
	})
}

var _ = pgx.ErrNoRows
