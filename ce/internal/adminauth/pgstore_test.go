package adminauth

import (
	"context"
	"errors"
	"regexp"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v4"
)

// TestPGUserStoreGetByUsername covers the PG user loader: successful load
// with role normalization, unknown username mapping, and role rows.
func TestPGUserStoreGetByUsername(t *testing.T) {
	ctx := context.Background()

	t.Run("ok with role normalization", func(t *testing.T) {
		m, err := pgxmock.NewPool()
		if err != nil {
			t.Fatalf("pgxmock.NewPool: %v", err)
		}
		defer m.Close()
		m.ExpectQuery(regexp.QuoteMeta(`
		SELECT id::text, username, COALESCE(password_hash, ''), tenant_id::text, status
		  FROM adc_users
		 WHERE username = $1 AND deleted_at IS NULL`)).
			WithArgs("admin").
			WillReturnRows(pgxmock.NewRows([]string{"id", "username", "password_hash", "tenant_id", "status"}).
				AddRow("11111111-2222-3333-4444-555555555555", "admin", "$2a$10$hash", "t1", "ACTIVE"))
		m.ExpectQuery(regexp.QuoteMeta(`
		SELECT r.role_code
		  FROM adc_user_roles ur
		  JOIN adc_roles r ON r.id = ur.role_id
		 WHERE ur.user_id = $1::uuid`)).
			WithArgs("11111111-2222-3333-4444-555555555555").
			WillReturnRows(pgxmock.NewRows([]string{"role_code"}).
				AddRow("PLATFORM_ADMIN").
				AddRow("AUDITOR"))
		store := NewPGUserStore(m)
		u, err := store.GetByUsername(ctx, "admin")
		if err != nil {
			t.Fatalf("GetByUsername: %v", err)
		}
		if u.ID != "11111111-2222-3333-4444-555555555555" || u.TenantID != "t1" || u.Status != "ACTIVE" {
			t.Fatalf("unexpected user: %+v", u)
		}
		if len(u.Roles) != 2 || u.Roles[0] != "platform_admin" || u.Roles[1] != "auditor" {
			t.Fatalf("roles must be normalized to lowercase: %v", u.Roles)
		}
		if err := m.ExpectationsWereMet(); err != nil {
			t.Fatalf("unmet expectations: %v", err)
		}
	})

	t.Run("unknown username", func(t *testing.T) {
		m, err := pgxmock.NewPool()
		if err != nil {
			t.Fatalf("pgxmock.NewPool: %v", err)
		}
		defer m.Close()
		m.ExpectQuery(regexp.QuoteMeta(`
		SELECT id::text, username, COALESCE(password_hash, ''), tenant_id::text, status
		  FROM adc_users
		 WHERE username = $1 AND deleted_at IS NULL`)).
			WithArgs("ghost").
			WillReturnRows(pgxmock.NewRows([]string{"id", "username", "password_hash", "tenant_id", "status"}))
		store := NewPGUserStore(m)
		if _, err := store.GetByUsername(ctx, "ghost"); !errors.Is(err, ErrUserNotFound) {
			t.Fatalf("want ErrUserNotFound, got %v", err)
		}
	})

	t.Run("role query failure propagates", func(t *testing.T) {
		m, err := pgxmock.NewPool()
		if err != nil {
			t.Fatalf("pgxmock.NewPool: %v", err)
		}
		defer m.Close()
		m.ExpectQuery(regexp.QuoteMeta(`
		SELECT id::text, username, COALESCE(password_hash, ''), tenant_id::text, status
		  FROM adc_users
		 WHERE username = $1 AND deleted_at IS NULL`)).
			WithArgs("admin").
			WillReturnRows(pgxmock.NewRows([]string{"id", "username", "password_hash", "tenant_id", "status"}).
				AddRow("id-1", "admin", "h", "t1", "ACTIVE"))
		m.ExpectQuery(regexp.QuoteMeta(`
		SELECT r.role_code
		  FROM adc_user_roles ur
		  JOIN adc_roles r ON r.id = ur.role_id
		 WHERE ur.user_id = $1::uuid`)).
			WithArgs("id-1").
			WillReturnError(errors.New("conn down"))
		store := NewPGUserStore(m)
		if _, err := store.GetByUsername(ctx, "admin"); err == nil {
			t.Fatal("want role query error")
		}
	})
}

var _ = pgx.ErrNoRows

func TestPGUserStoreGetByOIDCIdentity(t *testing.T) {
	ctx := context.Background()

	t.Run("ok with roles", func(t *testing.T) {
		m, err := pgxmock.NewPool()
		if err != nil {
			t.Fatalf("pgxmock.NewPool: %v", err)
		}
		defer m.Close()
		m.ExpectQuery(regexp.QuoteMeta(`
		SELECT u.id::text, u.username, COALESCE(u.password_hash, ''), u.tenant_id::text, u.status
		  FROM adc_oidc_identities i
		  JOIN adc_users u ON u.id = i.user_id
		 WHERE i.issuer = $1 AND i.subject = $2
		   AND u.auth_source = 'OIDC' AND u.deleted_at IS NULL`)).
			WithArgs("https://idp.example", "sub-oidc-1").
			WillReturnRows(pgxmock.NewRows([]string{"id", "username", "password_hash", "tenant_id", "status"}).
				AddRow("oidc-user-id", "sso@corp.com", "", "t1", "ACTIVE"))
		m.ExpectQuery(regexp.QuoteMeta(`
		SELECT r.role_code
		  FROM adc_user_roles ur
		  JOIN adc_roles r ON r.id = ur.role_id
		 WHERE ur.user_id = $1::uuid`)).
			WithArgs("oidc-user-id").
			WillReturnRows(pgxmock.NewRows([]string{"role_code"}).
				AddRow("TENANT_ADMIN"))
		store := NewPGUserStore(m)
		u, err := store.GetByOIDCIdentity(ctx, "https://idp.example", "sub-oidc-1")
		if err != nil {
			t.Fatalf("GetByOIDCIdentity: %v", err)
		}
		if u.ID != "oidc-user-id" || u.TenantID != "t1" || u.Status != "ACTIVE" {
			t.Fatalf("unexpected user: %+v", u)
		}
		if u.PasswordHash != "" {
			t.Fatalf("OIDC users must load an empty password hash, got %q", u.PasswordHash)
		}
		if len(u.Roles) != 1 || u.Roles[0] != "tenant_admin" {
			t.Fatalf("roles must be normalized to lowercase: %v", u.Roles)
		}
		if err := m.ExpectationsWereMet(); err != nil {
			t.Fatalf("unmet expectations: %v", err)
		}
	})

	t.Run("unknown subject", func(t *testing.T) {
		m, err := pgxmock.NewPool()
		if err != nil {
			t.Fatalf("pgxmock.NewPool: %v", err)
		}
		defer m.Close()
		m.ExpectQuery(regexp.QuoteMeta(`
		SELECT u.id::text, u.username, COALESCE(u.password_hash, ''), u.tenant_id::text, u.status
		  FROM adc_oidc_identities i
		  JOIN adc_users u ON u.id = i.user_id
		 WHERE i.issuer = $1 AND i.subject = $2
		   AND u.auth_source = 'OIDC' AND u.deleted_at IS NULL`)).
			WithArgs("https://idp.example", "sub-ghost").
			WillReturnRows(pgxmock.NewRows([]string{"id", "username", "password_hash", "tenant_id", "status"}))
		store := NewPGUserStore(m)
		if _, err := store.GetByOIDCIdentity(ctx, "https://idp.example", "sub-ghost"); !errors.Is(err, ErrUserNotFound) {
			t.Fatalf("want ErrUserNotFound, got %v", err)
		}
	})
}
