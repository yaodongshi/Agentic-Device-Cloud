package adminauth

import (
	"context"
	"errors"
	"testing"

	"github.com/pashagolub/pgxmock/v4"
)

func TestPGUserStoreLoginSnapshots(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name string
		oidc bool
	}{
		{name: "local"},
		{name: "oidc", oidc: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, err := pgxmock.NewPool()
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close()
			pattern := `(?s)SELECT u\.id::text.*LEFT JOIN adc_user_roles ur.*ur\.expires_at IS NULL OR ur\.expires_at > now\(\).*GROUP BY u\.id, t\.status`
			expect := m.ExpectQuery(pattern)
			if tt.oidc {
				expect.WithArgs("https://idp.example", "subject")
			} else {
				expect.WithArgs("admin")
			}
			expect.WillReturnRows(pgxmock.NewRows([]string{"id", "username", "password_hash", "tenant_id", "user_status", "tenant_status", "authz_version", "roles"}).
				AddRow("11111111-2222-3333-4444-555555555555", "admin", "hash", "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", "ACTIVE", "ACTIVE", int64(7), []string{"auditor", "tenant_admin"}))

			store := NewPGUserStore(m)
			var user *User
			if tt.oidc {
				user, err = store.GetByOIDCIdentity(ctx, "https://idp.example", "subject")
			} else {
				user, err = store.GetByUsername(ctx, "admin")
			}
			if err != nil {
				t.Fatal(err)
			}
			if user.AuthzVersion != 7 || user.TenantStatus != "ACTIVE" || len(user.Roles) != 2 || user.Roles[1] != "tenant_admin" {
				t.Fatalf("snapshot = %+v", user)
			}
			if err := m.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPGUserStoreUnknownAndFailure(t *testing.T) {
	m, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	m.ExpectQuery(`(?s)SELECT u\.id::text.*u\.username = \$1`).WithArgs("ghost").
		WillReturnRows(pgxmock.NewRows([]string{"id", "username", "password_hash", "tenant_id", "user_status", "tenant_status", "authz_version", "roles"}))
	if _, err := NewPGUserStore(m).GetByUsername(context.Background(), "ghost"); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("error = %v, want ErrUserNotFound", err)
	}
}

func TestPGAuthorizationStoreCurrentAuthorization(t *testing.T) {
	ctx := context.Background()
	t.Run("current roles use database expiry", func(t *testing.T) {
		m, err := pgxmock.NewPool()
		if err != nil {
			t.Fatal(err)
		}
		defer m.Close()
		m.ExpectQuery(`(?s)SELECT u\.status.*LEFT JOIN adc_user_roles ur.*ur\.expires_at IS NULL OR ur\.expires_at > now\(\).*u\.id = \$1::uuid AND u\.tenant_id = \$2::uuid`).
			WithArgs("11111111-2222-3333-4444-555555555555", "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee").
			WillReturnRows(pgxmock.NewRows([]string{"user_status", "user_deleted", "tenant_status", "tenant_deleted", "authz_version", "roles"}).
				AddRow("ACTIVE", false, "ACTIVE", false, int64(9), []string{"approver"}))
		got, err := NewPGUserStore(m).CurrentAuthorization(ctx, "11111111-2222-3333-4444-555555555555", "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")
		if err != nil || got.AuthzVersion != 9 || len(got.Roles) != 1 || got.Roles[0] != "approver" {
			t.Fatalf("snapshot=%+v err=%v", got, err)
		}
	})

	t.Run("database error propagates", func(t *testing.T) {
		m, err := pgxmock.NewPool()
		if err != nil {
			t.Fatal(err)
		}
		defer m.Close()
		m.ExpectQuery(`(?s)SELECT u\.status.*FROM adc_users`).WillReturnError(errors.New("postgres unavailable"))
		if _, err := NewPGUserStore(m).CurrentAuthorization(ctx, "u", "t"); err == nil {
			t.Fatal("expected database error")
		}
	})
}
