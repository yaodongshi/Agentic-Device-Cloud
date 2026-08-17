package mcpbinding

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"
)

const (
	repDeviceID  = "11111111-2222-3333-4444-555555555555"
	repTenantID  = "99999999-8888-7777-6666-555555555555"
	repToken     = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	repTokenHash = "2e7d2c03a9507ae265ecf5b5356885a53393a2029d241394997265a1a25aefc3"
)

var (
	repNow     = time.Date(2026, 8, 17, 8, 0, 0, 0, time.UTC)
	repExpire  = repNow.Add(time.Hour)
	repUpdated = time.Date(2026, 8, 17, 8, 0, 1, 0, time.UTC)
)

func newBindingMockPool(t *testing.T) pgxmock.PgxPoolIface {
	t.Helper()
	m, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	t.Cleanup(func() { m.Close() })
	return m
}

var bindingRowCols = []string{
	"id", "tenant_id", "device_class", "mcp_endpoint", "auth_type",
	"binding_status", "binding_token_hash", "binding_expire_at",
	"oauth_client_id", "oauth_client_secret_enc", "token_endpoint",
	"resource_identifier", "bound_at", "revoked_at", "updated_at",
}

// bindingRowVals builds a scan row in bindingCols column order. status
// is a named Status value so pgxmock's reflection assigner can map it
// onto the Status-typed scan destination.
func bindingRowVals(status Status, endpoint, hash, clientID, tokenEndpoint, resource string, expireAt, boundAt, revokedAt any) []any {
	return []any{
		repDeviceID, repTenantID, "A", endpoint, "oauth2_client_credentials",
		status, hash, expireAt,
		clientID, "encv1$c2VjcmV0", tokenEndpoint, resource,
		boundAt, revokedAt, repUpdated,
	}
}

func epochAny() any { return time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC) }

func wantBindingErr(t *testing.T, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
}

func TestPGBindingRepoRegisterEndpoint(t *testing.T) {
	t.Run("ok", func(t *testing.T) {
		m := newBindingMockPool(t)
		repo := NewPGBindingRepo(m)
		m.ExpectQuery(regexp.QuoteMeta(sqlRegisterEndpoint)).
			WithArgs(repDeviceID, "https://dev.example/mcp", "platform-client", "encv1$c2VjcmV0", repTokenHash, repExpire).
			WillReturnRows(pgxmock.NewRows(bindingRowCols).
				AddRow(bindingRowVals(StatusBinding, "https://dev.example/mcp", repTokenHash, "platform-client", "", "", repExpire, epochAny(), epochAny())...))
		b, err := repo.RegisterEndpoint(context.Background(), repDeviceID, "https://dev.example/mcp", "platform-client", "encv1$c2VjcmV0", repTokenHash, repExpire)
		if err != nil {
			t.Fatalf("RegisterEndpoint: %v", err)
		}
		if b.Status != StatusBinding || b.McpEndpoint != "https://dev.example/mcp" ||
			b.BindingTokenHash != repTokenHash || b.BindingExpireAt == nil || !b.BindingExpireAt.Equal(repExpire) {
			t.Fatalf("unexpected binding: %+v", b)
		}
		if err := m.ExpectationsWereMet(); err != nil {
			t.Fatalf("unmet expectations: %v", err)
		}
	})

	t.Run("state conflict while binding in progress", func(t *testing.T) {
		m := newBindingMockPool(t)
		repo := NewPGBindingRepo(m)
		m.ExpectQuery(regexp.QuoteMeta(sqlRegisterEndpoint)).
			WithArgs(repDeviceID, "https://dev.example/mcp", "platform-client", "encv1$c2VjcmV0", repTokenHash, repExpire).
			WillReturnRows(pgxmock.NewRows(bindingRowCols))
		m.ExpectQuery(regexp.QuoteMeta(sqlClassifyRow)).
			WithArgs(repDeviceID).
			WillReturnRows(pgxmock.NewRows([]string{"device_class", "binding_status", "binding_token_hash", "binding_expire_at"}).
				AddRow("A", "BINDING", "other-hash", repExpire))
		_, err := repo.RegisterEndpoint(context.Background(), repDeviceID, "https://dev.example/mcp", "platform-client", "encv1$c2VjcmV0", repTokenHash, repExpire)
		wantBindingErr(t, err, ErrStateConflict)
		if err := m.ExpectationsWereMet(); err != nil {
			t.Fatalf("unmet expectations: %v", err)
		}
	})

	t.Run("not class A", func(t *testing.T) {
		m := newBindingMockPool(t)
		repo := NewPGBindingRepo(m)
		m.ExpectQuery(regexp.QuoteMeta(sqlRegisterEndpoint)).
			WithArgs(repDeviceID, "https://dev.example/mcp", "platform-client", "encv1$c2VjcmV0", repTokenHash, repExpire).
			WillReturnRows(pgxmock.NewRows(bindingRowCols))
		m.ExpectQuery(regexp.QuoteMeta(sqlClassifyRow)).
			WithArgs(repDeviceID).
			WillReturnRows(pgxmock.NewRows([]string{"device_class", "binding_status", "binding_token_hash", "binding_expire_at"}).
				AddRow("B", "REGISTERED", "", epochAny()))
		_, err := repo.RegisterEndpoint(context.Background(), repDeviceID, "https://dev.example/mcp", "platform-client", "encv1$c2VjcmV0", repTokenHash, repExpire)
		wantBindingErr(t, err, ErrNotClassA)
	})

	t.Run("not found", func(t *testing.T) {
		m := newBindingMockPool(t)
		repo := NewPGBindingRepo(m)
		m.ExpectQuery(regexp.QuoteMeta(sqlRegisterEndpoint)).
			WithArgs(repDeviceID, "https://dev.example/mcp", "platform-client", "encv1$c2VjcmV0", repTokenHash, repExpire).
			WillReturnRows(pgxmock.NewRows(bindingRowCols))
		m.ExpectQuery(regexp.QuoteMeta(sqlClassifyRow)).
			WithArgs(repDeviceID).
			WillReturnRows(pgxmock.NewRows([]string{"device_class", "binding_status", "binding_token_hash", "binding_expire_at"}))
		_, err := repo.RegisterEndpoint(context.Background(), repDeviceID, "https://dev.example/mcp", "platform-client", "encv1$c2VjcmV0", repTokenHash, repExpire)
		wantBindingErr(t, err, ErrBindingNotFound)
	})
}

func TestPGBindingRepoGet(t *testing.T) {
	m := newBindingMockPool(t)
	repo := NewPGBindingRepo(m)

	m.ExpectQuery(regexp.QuoteMeta(sqlGetBinding)).
		WithArgs(repDeviceID).
		WillReturnRows(pgxmock.NewRows(bindingRowCols).
			AddRow(bindingRowVals(StatusBound, "https://dev.example/mcp", "", "platform-client", "", "", epochAny(), repNow, epochAny())...))
	b, err := repo.Get(context.Background(), repDeviceID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if b.Status != StatusBound || b.BindingTokenHash != "" || b.BoundAt == nil || !b.BoundAt.Equal(repNow) {
		t.Fatalf("unexpected binding: %+v", b)
	}

	// missing row: classifyRow re-read distinguishes not-found.
	m.ExpectQuery(regexp.QuoteMeta(sqlGetBinding)).
		WithArgs(repDeviceID).
		WillReturnRows(pgxmock.NewRows(bindingRowCols))
	m.ExpectQuery(regexp.QuoteMeta(sqlClassifyRow)).
		WithArgs(repDeviceID).
		WillReturnRows(pgxmock.NewRows([]string{"device_class", "binding_status", "binding_token_hash", "binding_expire_at"}))
	if _, err := repo.Get(context.Background(), repDeviceID); !errors.Is(err, ErrBindingNotFound) {
		t.Fatalf("want ErrBindingNotFound, got %v", err)
	}

	// class-B row: ErrNotClassA.
	m.ExpectQuery(regexp.QuoteMeta(sqlGetBinding)).
		WithArgs(repDeviceID).
		WillReturnRows(pgxmock.NewRows(bindingRowCols))
	m.ExpectQuery(regexp.QuoteMeta(sqlClassifyRow)).
		WithArgs(repDeviceID).
		WillReturnRows(pgxmock.NewRows([]string{"device_class", "binding_status", "binding_token_hash", "binding_expire_at"}).
			AddRow("B", "REGISTERED", "", epochAny()))
	if _, err := repo.Get(context.Background(), repDeviceID); !errors.Is(err, ErrNotClassA) {
		t.Fatalf("want ErrNotClassA, got %v", err)
	}

	if err := m.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestPGBindingRepoConsumeTokenAndBind(t *testing.T) {
	t.Run("ok consumes token exactly once", func(t *testing.T) {
		m := newBindingMockPool(t)
		repo := NewPGBindingRepo(m)
		m.ExpectQuery(regexp.QuoteMeta(sqlConsumeTokenAndBind)).
			WithArgs(repDeviceID, repTokenHash, repNow, "https://as.example/token", "https://dev.example/mcp").
			WillReturnRows(pgxmock.NewRows(bindingRowCols).
				AddRow(bindingRowVals(StatusBound, "https://dev.example/mcp", "", "platform-client",
					"https://as.example/token", "https://dev.example/mcp", epochAny(), repNow, epochAny())...))
		b, err := repo.ConsumeTokenAndBind(context.Background(), repDeviceID, repTokenHash, "https://as.example/token", "https://dev.example/mcp", repNow)
		if err != nil {
			t.Fatalf("ConsumeTokenAndBind: %v", err)
		}
		if b.Status != StatusBound || b.BindingTokenHash != "" || b.BindingExpireAt != nil {
			t.Fatalf("token not consumed: %+v", b)
		}
		if b.TokenEndpoint != "https://as.example/token" || b.ResourceIdentifier != "https://dev.example/mcp" {
			t.Fatalf("discovery state not persisted: %+v", b)
		}
		if err := m.ExpectationsWereMet(); err != nil {
			t.Fatalf("unmet expectations: %v", err)
		}
	})

	t.Run("wrong token", func(t *testing.T) {
		m := newBindingMockPool(t)
		repo := NewPGBindingRepo(m)
		m.ExpectQuery(regexp.QuoteMeta(sqlConsumeTokenAndBind)).
			WithArgs(repDeviceID, repTokenHash, repNow, "https://as.example/token", "https://dev.example/mcp").
			WillReturnRows(pgxmock.NewRows(bindingRowCols))
		m.ExpectQuery(regexp.QuoteMeta(sqlClassifyRow)).
			WithArgs(repDeviceID).
			WillReturnRows(pgxmock.NewRows([]string{"device_class", "binding_status", "binding_token_hash", "binding_expire_at"}).
				AddRow("A", "BINDING", "another-hash", repExpire))
		_, err := repo.ConsumeTokenAndBind(context.Background(), repDeviceID, repTokenHash, "https://as.example/token", "https://dev.example/mcp", repNow)
		wantBindingErr(t, err, ErrTokenInvalid)
	})

	t.Run("expired token", func(t *testing.T) {
		m := newBindingMockPool(t)
		repo := NewPGBindingRepo(m)
		m.ExpectQuery(regexp.QuoteMeta(sqlConsumeTokenAndBind)).
			WithArgs(repDeviceID, repTokenHash, repNow, "https://as.example/token", "https://dev.example/mcp").
			WillReturnRows(pgxmock.NewRows(bindingRowCols))
		m.ExpectQuery(regexp.QuoteMeta(sqlClassifyRow)).
			WithArgs(repDeviceID).
			WillReturnRows(pgxmock.NewRows([]string{"device_class", "binding_status", "binding_token_hash", "binding_expire_at"}).
				AddRow("A", "BINDING", repTokenHash, repNow.Add(-time.Minute)))
		_, err := repo.ConsumeTokenAndBind(context.Background(), repDeviceID, repTokenHash, "https://as.example/token", "https://dev.example/mcp", repNow)
		wantBindingErr(t, err, ErrTokenExpired)
	})

	t.Run("replay after consumption", func(t *testing.T) {
		m := newBindingMockPool(t)
		repo := NewPGBindingRepo(m)
		m.ExpectQuery(regexp.QuoteMeta(sqlConsumeTokenAndBind)).
			WithArgs(repDeviceID, repTokenHash, repNow, "https://as.example/token", "https://dev.example/mcp").
			WillReturnRows(pgxmock.NewRows(bindingRowCols))
		m.ExpectQuery(regexp.QuoteMeta(sqlClassifyRow)).
			WithArgs(repDeviceID).
			WillReturnRows(pgxmock.NewRows([]string{"device_class", "binding_status", "binding_token_hash", "binding_expire_at"}).
				AddRow("A", "BOUND", "", epochAny()))
		_, err := repo.ConsumeTokenAndBind(context.Background(), repDeviceID, repTokenHash, "https://as.example/token", "https://dev.example/mcp", repNow)
		wantBindingErr(t, err, ErrTokenInvalid)
	})
}

func TestPGBindingRepoRevoke(t *testing.T) {
	t.Run("ok", func(t *testing.T) {
		m := newBindingMockPool(t)
		repo := NewPGBindingRepo(m)
		m.ExpectQuery(regexp.QuoteMeta(sqlRevokeBinding)).
			WithArgs(repDeviceID, repNow).
			WillReturnRows(pgxmock.NewRows(bindingRowCols).
				AddRow(bindingRowVals(StatusRevoked, "https://dev.example/mcp", "", "platform-client", "", "", epochAny(), epochAny(), repNow)...))
		b, err := repo.Revoke(context.Background(), repDeviceID, repNow)
		if err != nil {
			t.Fatalf("Revoke: %v", err)
		}
		if b.Status != StatusRevoked || b.RevokedAt == nil || !b.RevokedAt.Equal(repNow) || b.BindingTokenHash != "" {
			t.Fatalf("unexpected binding: %+v", b)
		}
		if err := m.ExpectationsWereMet(); err != nil {
			t.Fatalf("unmet expectations: %v", err)
		}
	})

	t.Run("already revoked is a state conflict", func(t *testing.T) {
		m := newBindingMockPool(t)
		repo := NewPGBindingRepo(m)
		m.ExpectQuery(regexp.QuoteMeta(sqlRevokeBinding)).
			WithArgs(repDeviceID, repNow).
			WillReturnRows(pgxmock.NewRows(bindingRowCols))
		m.ExpectQuery(regexp.QuoteMeta(sqlClassifyRow)).
			WithArgs(repDeviceID).
			WillReturnRows(pgxmock.NewRows([]string{"device_class", "binding_status", "binding_token_hash", "binding_expire_at"}).
				AddRow("A", "REVOKED", "", epochAny()))
		_, err := repo.Revoke(context.Background(), repDeviceID, repNow)
		wantBindingErr(t, err, ErrStateConflict)
	})

	t.Run("not found", func(t *testing.T) {
		m := newBindingMockPool(t)
		repo := NewPGBindingRepo(m)
		m.ExpectQuery(regexp.QuoteMeta(sqlRevokeBinding)).
			WithArgs(repDeviceID, repNow).
			WillReturnRows(pgxmock.NewRows(bindingRowCols))
		m.ExpectQuery(regexp.QuoteMeta(sqlClassifyRow)).
			WithArgs(repDeviceID).
			WillReturnRows(pgxmock.NewRows([]string{"device_class", "binding_status", "binding_token_hash", "binding_expire_at"}))
		_, err := repo.Revoke(context.Background(), repDeviceID, repNow)
		wantBindingErr(t, err, ErrBindingNotFound)
	})
}
