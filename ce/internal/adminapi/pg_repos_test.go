package adminapi

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pashagolub/pgxmock/v4"
)

// newMockPool builds a pgxmock pool with the default query matcher
// (regexp-based). Every Expect* uses regexp.QuoteMeta'ed SQL below.
func newMockPool(t *testing.T) pgxmock.PgxPoolIface {
	t.Helper()
	m, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	t.Cleanup(func() { m.Close() })
	return m
}

const (
	testUUID   = "11111111-2222-3333-4444-555555555555"
	testUUID2  = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	testUUID3  = "bbbbbbbb-cccc-dddd-eeee-ffffffffffff"
	testTenant = "99999999-8888-7777-6666-555555555555"
)

// ---------------------------------------------------------------------------
// scanDevice / scanTargets / finishScan row fixtures
// ---------------------------------------------------------------------------

var deviceRowCols = []string{
	"id", "tenant_id", "group_id", "device_code", "name", "device_type",
	"device_class", "auth_type", "status", "sdk_version", "protocol_version",
	"last_heartbeat_at", "metadata", "credential_version", "created_at", "updated_at",
}

func deviceRowVals(status string) []any {
	return []any{
		testUUID, testTenant, nil, "dev-1", "Dev One", "cnc", "B", "hmac", status,
		"", "1.0", nil, []byte(`{"site":"s1"}`), int(1),
		time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), time.Date(2026, 1, 2, 3, 4, 6, 0, time.UTC),
	}
}

func expectDeviceRows(m pgxmock.PgxPoolIface, cols []string, vals []any) *pgxmock.Rows {
	rows := pgxmock.NewRows(cols)
	if len(vals) == 0 {
		return rows
	}
	rows = rows.AddRow(vals...)
	return rows
}

// ---------------------------------------------------------------------------
// pgDeviceRepo
// ---------------------------------------------------------------------------

func TestPGDeviceRepoGet(t *testing.T) {
	m := newMockPool(t)
	repo := NewPGDeviceRepo(m)

	m.ExpectQuery(regexp.QuoteMeta(`SELECT ` + deviceCols + ` FROM adc_devices
		WHERE id = $1::uuid AND deleted_at IS NULL`)).
		WithArgs(testUUID).
		WillReturnRows(expectDeviceRows(m, deviceRowCols, deviceRowVals("ONLINE")))
	d, err := repo.Get(context.Background(), testUUID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if d.DeviceCode != "dev-1" || d.Status != "online" || d.Metadata["site"] != "s1" || d.CredentialVersion != 1 {
		t.Fatalf("unexpected device: %+v", d)
	}

	m.ExpectQuery(regexp.QuoteMeta(`SELECT ` + deviceCols + ` FROM adc_devices
		WHERE id = $1::uuid AND deleted_at IS NULL`)).
		WithArgs(testUUID2).
		WillReturnRows(expectDeviceRows(m, deviceRowCols, nil))
	if _, err := repo.Get(context.Background(), testUUID2); !errors.Is(err, ErrDeviceNotFound) {
		t.Fatalf("want ErrDeviceNotFound, got %v", err)
	}

	m.ExpectQuery(regexp.QuoteMeta(`SELECT ` + deviceCols + ` FROM adc_devices
		WHERE id = $1::uuid AND deleted_at IS NULL`)).
		WithArgs(testUUID2).
		WillReturnError(errors.New("conn down"))
	if _, err := repo.Get(context.Background(), testUUID2); err == nil || !strings.Contains(err.Error(), "conn down") {
		t.Fatalf("want conn error, got %v", err)
	}

	m.ExpectQuery(regexp.QuoteMeta(`SELECT ` + deviceCols + ` FROM adc_devices
		WHERE id = $1::uuid AND deleted_at IS NULL`)).
		WithArgs(testUUID2).
		WillReturnRows(expectDeviceRows(m, deviceRowCols, []any{
			testUUID2, testTenant, nil, "dev-2", "Dev Two", "cnc", "B", "hmac", "ONLINE",
			"", "1.0", nil, []byte(`not-json`), int(1),
			time.Now(), time.Now(),
		}))
	if _, err := repo.Get(context.Background(), testUUID2); err == nil {
		t.Fatal("want metadata decode error, got nil")
	}

	if err := m.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestPGDeviceRepoList(t *testing.T) {
	m := newMockPool(t)
	repo := NewPGDeviceRepo(m)

	m.ExpectQuery(regexp.QuoteMeta(`SELECT `+deviceCols+`, count(*) OVER () AS total
		FROM adc_devices
		WHERE tenant_id = $1::uuid AND deleted_at IS NULL
		  AND ($2::text = '' OR status = $2)
		  AND ($3::text = '' OR device_type = $3)
		  AND ($4::text = '' OR (device_code ILIKE $5 OR name ILIKE $5))
		  AND ($6::uuid IS NULL OR group_id = $6)
		ORDER BY created_at DESC, id
		LIMIT $7 OFFSET $8`)).
		WithArgs(testTenant, "ONLINE", "cnc", "dev", "%dev%", nil, 20, 0).
		WillReturnRows(pgxmock.NewRows(append(append([]string{}, deviceRowCols...), "total")).
			AddRow(append(deviceRowVals("ONLINE"), 1)...))

	devs, total, err := repo.List(context.Background(), testTenant, DeviceFilter{Status: "online", DeviceType: "cnc", Keyword: "dev"}, Page{Number: 1, Size: 20})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(devs) != 1 || total != 1 || devs[0].Status != "online" {
		t.Fatalf("unexpected list: %+v total=%d", devs, total)
	}

	m.ExpectQuery(regexp.QuoteMeta(`SELECT `+deviceCols+`, count(*) OVER () AS total
		FROM adc_devices
		WHERE tenant_id = $1::uuid AND deleted_at IS NULL
		  AND ($2::text = '' OR status = $2)
		  AND ($3::text = '' OR device_type = $3)
		  AND ($4::text = '' OR (device_code ILIKE $5 OR name ILIKE $5))
		  AND ($6::uuid IS NULL OR group_id = $6)
		ORDER BY created_at DESC, id
		LIMIT $7 OFFSET $8`)).
		WithArgs(testTenant, "", "", "", "", nil, 20, 0).
		WillReturnError(errors.New("list boom"))
	if _, _, err := repo.List(context.Background(), testTenant, DeviceFilter{}, Page{Number: 1, Size: 20}); err == nil {
		t.Fatal("want query error")
	}

	if err := m.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestPGDeviceRepoSetStatus(t *testing.T) {
	m := newMockPool(t)
	repo := NewPGDeviceRepo(m)

	m.ExpectQuery(regexp.QuoteMeta(`UPDATE adc_devices
		SET status = $2, updated_at = now()
		WHERE id = $1::uuid AND deleted_at IS NULL
		RETURNING `+deviceCols)).
		WithArgs(testUUID, "FROZEN").
		WillReturnRows(expectDeviceRows(m, deviceRowCols, deviceRowVals("FROZEN")))
	d, err := repo.SetStatus(context.Background(), testUUID, "frozen")
	if err != nil {
		t.Fatalf("SetStatus: %v", err)
	}
	if d.Status != "frozen" {
		t.Fatalf("want frozen, got %s", d.Status)
	}

	m.ExpectQuery(regexp.QuoteMeta(`UPDATE adc_devices
		SET status = $2, updated_at = now()
		WHERE id = $1::uuid AND deleted_at IS NULL
		RETURNING `+deviceCols)).
		WithArgs(testUUID2, "FROZEN").
		WillReturnRows(expectDeviceRows(m, deviceRowCols, nil))
	if _, err := repo.SetStatus(context.Background(), testUUID2, "frozen"); !errors.Is(err, ErrDeviceNotFound) {
		t.Fatalf("want ErrDeviceNotFound, got %v", err)
	}

	if err := m.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestPGDeviceRepoResetCredential(t *testing.T) {
	m := newMockPool(t)
	repo := NewPGDeviceRepo(m)
	cred := DeviceCredential{Stored: "enc$1"}

	m.ExpectQuery(regexp.QuoteMeta(`UPDATE adc_devices
		SET credential_hash = $2, credential_version = credential_version + 1, updated_at = now()
		WHERE id = $1::uuid AND deleted_at IS NULL AND status <> 'FROZEN'
		RETURNING `+deviceCols)).
		WithArgs(testUUID, cred.Stored).
		WillReturnRows(expectDeviceRows(m, deviceRowCols, deviceRowVals("ONLINE")))
	d, err := repo.ResetCredential(context.Background(), testUUID, cred)
	if err != nil {
		t.Fatalf("ResetCredential: %v", err)
	}
	if d.CredentialVersion != 1 {
		t.Fatalf("unexpected device: %+v", d)
	}

	// zero-row update -> re-read -> FROZEN classification
	m.ExpectQuery(regexp.QuoteMeta(`UPDATE adc_devices
		SET credential_hash = $2, credential_version = credential_version + 1, updated_at = now()
		WHERE id = $1::uuid AND deleted_at IS NULL AND status <> 'FROZEN'
		RETURNING `+deviceCols)).
		WithArgs(testUUID2, cred.Stored).
		WillReturnRows(expectDeviceRows(m, deviceRowCols, nil))
	m.ExpectQuery(regexp.QuoteMeta(`SELECT ` + deviceCols + ` FROM adc_devices
		WHERE id = $1::uuid AND deleted_at IS NULL`)).
		WithArgs(testUUID2).
		WillReturnRows(expectDeviceRows(m, deviceRowCols, deviceRowVals("FROZEN")))
	if _, err := repo.ResetCredential(context.Background(), testUUID2, cred); !errors.Is(err, ErrDeviceFrozen) {
		t.Fatalf("want ErrDeviceFrozen, got %v", err)
	}

	// zero-row update -> re-read -> not found
	m.ExpectQuery(regexp.QuoteMeta(`UPDATE adc_devices
		SET credential_hash = $2, credential_version = credential_version + 1, updated_at = now()
		WHERE id = $1::uuid AND deleted_at IS NULL AND status <> 'FROZEN'
		RETURNING `+deviceCols)).
		WithArgs(testUUID3, cred.Stored).
		WillReturnRows(expectDeviceRows(m, deviceRowCols, nil))
	m.ExpectQuery(regexp.QuoteMeta(`SELECT ` + deviceCols + ` FROM adc_devices
		WHERE id = $1::uuid AND deleted_at IS NULL`)).
		WithArgs(testUUID3).
		WillReturnRows(expectDeviceRows(m, deviceRowCols, nil))
	if _, err := repo.ResetCredential(context.Background(), testUUID3, cred); !errors.Is(err, ErrDeviceNotFound) {
		t.Fatalf("want ErrDeviceNotFound, got %v", err)
	}

	if err := m.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestPGDeviceRepoRevokeCredential(t *testing.T) {
	m := newMockPool(t)
	repo := NewPGDeviceRepo(m)

	m.ExpectQuery(regexp.QuoteMeta(`UPDATE adc_devices
		SET credential_hash = $2, credential_version = credential_version + 1, updated_at = now()
		WHERE id = $1::uuid AND deleted_at IS NULL
		RETURNING `+deviceCols)).
		WithArgs(testUUID, pgxmock.AnyArg()).
		WillReturnRows(expectDeviceRows(m, deviceRowCols, deviceRowVals("ONLINE")))
	if _, err := repo.RevokeCredential(context.Background(), testUUID); err != nil {
		t.Fatalf("RevokeCredential: %v", err)
	}

	m.ExpectQuery(regexp.QuoteMeta(`UPDATE adc_devices
		SET credential_hash = $2, credential_version = credential_version + 1, updated_at = now()
		WHERE id = $1::uuid AND deleted_at IS NULL
		RETURNING `+deviceCols)).
		WithArgs(testUUID2, pgxmock.AnyArg()).
		WillReturnRows(expectDeviceRows(m, deviceRowCols, nil))
	if _, err := repo.RevokeCredential(context.Background(), testUUID2); !errors.Is(err, ErrDeviceNotFound) {
		t.Fatalf("want ErrDeviceNotFound, got %v", err)
	}

	if err := m.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestPGDeviceRepoUpdateMeta(t *testing.T) {
	m := newMockPool(t)
	repo := NewPGDeviceRepo(m)
	name := "Renamed"

	m.ExpectQuery(regexp.QuoteMeta(`UPDATE adc_devices
		SET name = COALESCE($2, name), metadata = COALESCE($3, metadata), updated_at = now()
		WHERE id = $1::uuid AND deleted_at IS NULL
		RETURNING `+deviceCols)).
		WithArgs(testUUID, &name, pgxmock.AnyArg()).
		WillReturnRows(expectDeviceRows(m, deviceRowCols, deviceRowVals("ONLINE")))
	if _, err := repo.UpdateMeta(context.Background(), testUUID, &name, map[string]any{"site": "s2"}); err != nil {
		t.Fatalf("UpdateMeta: %v", err)
	}

	// nil name and nil metadata keep both columns
	m.ExpectQuery(regexp.QuoteMeta(`UPDATE adc_devices
		SET name = COALESCE($2, name), metadata = COALESCE($3, metadata), updated_at = now()
		WHERE id = $1::uuid AND deleted_at IS NULL
		RETURNING `+deviceCols)).
		WithArgs(testUUID2, pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(expectDeviceRows(m, deviceRowCols, nil))
	if _, err := repo.UpdateMeta(context.Background(), testUUID2, nil, nil); !errors.Is(err, ErrDeviceNotFound) {
		t.Fatalf("want ErrDeviceNotFound, got %v", err)
	}

	if err := m.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestPGDeviceRepoSoftDelete(t *testing.T) {
	m := newMockPool(t)
	repo := NewPGDeviceRepo(m)

	m.ExpectBegin()
	m.ExpectQuery(regexp.QuoteMeta(`UPDATE adc_devices
		SET deleted_at = now(), status = 'RETIRED', updated_at = now()
		WHERE id = $1::uuid AND deleted_at IS NULL
		RETURNING tenant_id::text`)).
		WithArgs(testUUID).
		WillReturnRows(pgxmock.NewRows([]string{"tenant_id"}).AddRow(testTenant))
	m.ExpectExec(regexp.QuoteMeta(`UPDATE adc_tenants
		SET used_devices = GREATEST(used_devices - 1, 0), updated_at = now()
		WHERE id = $1::uuid`)).
		WithArgs(testTenant).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	m.ExpectCommit()
	if err := repo.SoftDelete(context.Background(), testUUID); err != nil {
		t.Fatalf("SoftDelete: %v", err)
	}

	m.ExpectBegin()
	m.ExpectQuery(regexp.QuoteMeta(`UPDATE adc_devices
		SET deleted_at = now(), status = 'RETIRED', updated_at = now()
		WHERE id = $1::uuid AND deleted_at IS NULL
		RETURNING tenant_id::text`)).
		WithArgs(testUUID2).
		WillReturnRows(pgxmock.NewRows([]string{"tenant_id"}))
	m.ExpectRollback()
	if err := repo.SoftDelete(context.Background(), testUUID2); !errors.Is(err, ErrDeviceNotFound) {
		t.Fatalf("want ErrDeviceNotFound, got %v", err)
	}

	if err := m.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestPGDeviceRepoRegister(t *testing.T) {
	newRepo := func(m pgxmock.PgxPoolIface) *pgDeviceRepo { return NewPGDeviceRepo(m) }
	baseDevice := func() *Device {
		return &Device{
			TenantID:   testTenant,
			GroupID:    nil,
			DeviceCode: "dev-new",
			Name:       "New Device",
			DeviceType: "cnc",
			AuthType:   "hmac",
			Metadata:   map[string]any{"site": "s1"},
		}
	}

	t.Run("ok", func(t *testing.T) {
		m := newMockPool(t)
		repo := newRepo(m)
		m.ExpectBegin()
		m.ExpectQuery(regexp.QuoteMeta(`SELECT status FROM adc_tenants WHERE id = $1::uuid AND deleted_at IS NULL`)).
			WithArgs(testTenant).
			WillReturnRows(pgxmock.NewRows([]string{"status"}).AddRow("ACTIVE"))
		m.ExpectExec(regexp.QuoteMeta(`UPDATE adc_tenants SET used_devices = used_devices + 1, updated_at = now() WHERE id = $1::uuid AND status = 'ACTIVE' AND used_devices < quota_devices`)).
			WithArgs(testTenant).
			WillReturnResult(pgxmock.NewResult("UPDATE", 1))
		m.ExpectQuery(regexp.QuoteMeta(`INSERT INTO adc_devices
		(tenant_id, group_id, device_code, name, device_type, auth_type,
		 credential_hash, credential_version, status, metadata)
		VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6, $7, 1, 'OFFLINE', $8)
		RETURNING `+deviceCols)).
			WithArgs(testTenant, pgxmock.AnyArg(), "dev-new", "New Device", "cnc", "hmac", "enc$1", `{"site":"s1"}`).
			WillReturnRows(expectDeviceRows(m, deviceRowCols, deviceRowVals("OFFLINE")))
		m.ExpectCommit()
		d, err := repo.Register(context.Background(), baseDevice(), DeviceCredential{Stored: "enc$1"})
		if err != nil {
			t.Fatalf("Register: %v", err)
		}
		if d.DeviceCode != "dev-1" {
			t.Fatalf("unexpected device: %+v", d)
		}
		if err := m.ExpectationsWereMet(); err != nil {
			t.Fatalf("unmet expectations: %v", err)
		}
	})

	t.Run("tenant missing", func(t *testing.T) {
		m := newMockPool(t)
		repo := newRepo(m)
		m.ExpectBegin()
		m.ExpectQuery(regexp.QuoteMeta(`SELECT status FROM adc_tenants WHERE id = $1::uuid AND deleted_at IS NULL`)).
			WithArgs(testTenant).
			WillReturnRows(pgxmock.NewRows([]string{"status"}))
		m.ExpectRollback()
		if _, err := repo.Register(context.Background(), baseDevice(), DeviceCredential{}); !errors.Is(err, ErrTenantNotFound) {
			t.Fatalf("want ErrTenantNotFound, got %v", err)
		}
	})

	t.Run("tenant suspended", func(t *testing.T) {
		m := newMockPool(t)
		repo := newRepo(m)
		m.ExpectBegin()
		m.ExpectQuery(regexp.QuoteMeta(`SELECT status FROM adc_tenants WHERE id = $1::uuid AND deleted_at IS NULL`)).
			WithArgs(testTenant).
			WillReturnRows(pgxmock.NewRows([]string{"status"}).AddRow("SUSPENDED"))
		m.ExpectRollback()
		if _, err := repo.Register(context.Background(), baseDevice(), DeviceCredential{}); !errors.Is(err, ErrTenantSuspended) {
			t.Fatalf("want ErrTenantSuspended, got %v", err)
		}
	})

	t.Run("quota exceeded", func(t *testing.T) {
		m := newMockPool(t)
		repo := newRepo(m)
		m.ExpectBegin()
		m.ExpectQuery(regexp.QuoteMeta(`SELECT status FROM adc_tenants WHERE id = $1::uuid AND deleted_at IS NULL`)).
			WithArgs(testTenant).
			WillReturnRows(pgxmock.NewRows([]string{"status"}).AddRow("ACTIVE"))
		m.ExpectExec(regexp.QuoteMeta(`UPDATE adc_tenants SET used_devices = used_devices + 1, updated_at = now() WHERE id = $1::uuid AND status = 'ACTIVE' AND used_devices < quota_devices`)).
			WithArgs(testTenant).
			WillReturnResult(pgxmock.NewResult("UPDATE", 0))
		m.ExpectRollback()
		if _, err := repo.Register(context.Background(), baseDevice(), DeviceCredential{}); !errors.Is(err, ErrDeviceQuotaExceeded) {
			t.Fatalf("want ErrDeviceQuotaExceeded, got %v", err)
		}
	})

	t.Run("code conflict", func(t *testing.T) {
		m := newMockPool(t)
		repo := newRepo(m)
		m.ExpectBegin()
		m.ExpectQuery(regexp.QuoteMeta(`SELECT status FROM adc_tenants WHERE id = $1::uuid AND deleted_at IS NULL`)).
			WithArgs(testTenant).
			WillReturnRows(pgxmock.NewRows([]string{"status"}).AddRow("ACTIVE"))
		m.ExpectExec(regexp.QuoteMeta(`UPDATE adc_tenants SET used_devices = used_devices + 1, updated_at = now() WHERE id = $1::uuid AND status = 'ACTIVE' AND used_devices < quota_devices`)).
			WithArgs(testTenant).
			WillReturnResult(pgxmock.NewResult("UPDATE", 1))
		m.ExpectQuery(regexp.QuoteMeta(`INSERT INTO adc_devices
		(tenant_id, group_id, device_code, name, device_type, auth_type,
		 credential_hash, credential_version, status, metadata)
		VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6, $7, 1, 'OFFLINE', $8)
		RETURNING `+deviceCols)).
			WithArgs(testTenant, pgxmock.AnyArg(), "dev-new", "New Device", "cnc", "hmac", pgxmock.AnyArg(), `{"site":"s1"}`).
			WillReturnError(&pgconn.PgError{Code: "23505"})
		m.ExpectRollback()
		if _, err := repo.Register(context.Background(), baseDevice(), DeviceCredential{}); !errors.Is(err, ErrDeviceCodeExists) {
			t.Fatalf("want ErrDeviceCodeExists, got %v", err)
		}
	})
}

// ---------------------------------------------------------------------------
// pgTenantRepo
// ---------------------------------------------------------------------------

var tenantRowCols = []string{
	"id", "code", "name", "status", "quota_devices", "quota_calls_monthly",
	"used_devices", "used_calls_month", "metadata", "created_at", "updated_at",
}

func tenantRowVals(status string) []any {
	return []any{
		testTenant, "tenant-demo", "Demo", status, int(100), int64(100000),
		int(1), int64(10), []byte(`{"max_agent_keys":5,"audit_retention_days":180}`),
		time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), time.Date(2026, 1, 2, 3, 4, 6, 0, time.UTC),
	}
}

func TestPGTenantRepoGet(t *testing.T) {
	m := newMockPool(t)
	repo := NewPGTenantRepo(m)

	m.ExpectQuery(regexp.QuoteMeta(`SELECT ` + tenantCols + ` FROM adc_tenants WHERE id = $1::uuid AND deleted_at IS NULL`)).
		WithArgs(testTenant).
		WillReturnRows(pgxmock.NewRows(tenantRowCols).AddRow(tenantRowVals("ACTIVE")...))
	t2, err := repo.Get(context.Background(), testTenant)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if t2.Code != "tenant-demo" || t2.Status != "active" || t2.Quota.MaxAgentKeys != 5 || t2.Quota.AuditRetentionDays != 180 {
		t.Fatalf("unexpected tenant: %+v", t2)
	}

	m.ExpectQuery(regexp.QuoteMeta(`SELECT ` + tenantCols + ` FROM adc_tenants WHERE id = $1::uuid AND deleted_at IS NULL`)).
		WithArgs(testUUID2).
		WillReturnRows(pgxmock.NewRows(tenantRowCols))
	if _, err := repo.Get(context.Background(), testUUID2); !errors.Is(err, ErrTenantNotFound) {
		t.Fatalf("want ErrTenantNotFound, got %v", err)
	}

	if err := m.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestPGTenantRepoCreate(t *testing.T) {
	m := newMockPool(t)
	repo := NewPGTenantRepo(m)

	in := &Tenant{Code: "t-new", Name: "New", Status: "active",
		Quota: Quota{MaxDevices: 10, MonthlyCallLimit: 1000, MaxAgentKeys: 3, AuditRetentionDays: 30}}

	m.ExpectQuery(regexp.QuoteMeta(`INSERT INTO adc_tenants
		(code, name, status, quota_devices, quota_calls_monthly, metadata)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING `+tenantCols)).
		WithArgs("t-new", "New", "ACTIVE", 10, int64(1000), `{"max_agent_keys":3,"audit_retention_days":30}`).
		WillReturnRows(pgxmock.NewRows(tenantRowCols).AddRow(tenantRowVals("ACTIVE")...))
	if _, err := repo.Create(context.Background(), in); err != nil {
		t.Fatalf("Create: %v", err)
	}

	m.ExpectQuery(regexp.QuoteMeta(`INSERT INTO adc_tenants
		(code, name, status, quota_devices, quota_calls_monthly, metadata)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING `+tenantCols)).
		WithArgs("t-new", "New", "ACTIVE", 10, int64(1000), `{"max_agent_keys":3,"audit_retention_days":30}`).
		WillReturnError(&pgconn.PgError{Code: "23505"})
	if _, err := repo.Create(context.Background(), in); !errors.Is(err, ErrTenantCodeConflict) {
		t.Fatalf("want ErrTenantCodeConflict, got %v", err)
	}

	if err := m.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestPGTenantRepoUpdate(t *testing.T) {
	m := newMockPool(t)
	repo := NewPGTenantRepo(m)
	in := &Tenant{Status: "active", Quota: Quota{MaxDevices: 50, MonthlyCallLimit: 5000}}

	m.ExpectQuery(regexp.QuoteMeta(`UPDATE adc_tenants
		SET quota_devices = $2, quota_calls_monthly = $3, status = $4,
		    metadata = COALESCE(metadata, '{}'::jsonb) || $5::jsonb, updated_at = now()
		WHERE id = $1::uuid AND deleted_at IS NULL
		  AND used_devices <= $2 AND used_calls_month <= $3
		RETURNING `+tenantCols)).
		WithArgs(testTenant, 50, int64(5000), "ACTIVE", `{"max_agent_keys":0,"audit_retention_days":0}`).
		WillReturnRows(pgxmock.NewRows(tenantRowCols).AddRow(tenantRowVals("ACTIVE")...))
	if _, err := repo.Update(context.Background(), testTenant, in); err != nil {
		t.Fatalf("Update: %v", err)
	}

	// zero-row update -> re-read shows usage above new quota
	m.ExpectQuery(regexp.QuoteMeta(`UPDATE adc_tenants
		SET quota_devices = $2, quota_calls_monthly = $3, status = $4,
		    metadata = COALESCE(metadata, '{}'::jsonb) || $5::jsonb, updated_at = now()
		WHERE id = $1::uuid AND deleted_at IS NULL
		  AND used_devices <= $2 AND used_calls_month <= $3
		RETURNING `+tenantCols)).
		WithArgs(testTenant, 50, int64(5000), "ACTIVE", `{"max_agent_keys":0,"audit_retention_days":0}`).
		WillReturnRows(pgxmock.NewRows(tenantRowCols))
	m.ExpectQuery(regexp.QuoteMeta(`SELECT ` + tenantCols + ` FROM adc_tenants WHERE id = $1::uuid AND deleted_at IS NULL`)).
		WithArgs(testTenant).
		WillReturnRows(pgxmock.NewRows(tenantRowCols).AddRow(append(append(tenantRowVals("ACTIVE")[:6], int(100), int64(10)), tenantRowVals("ACTIVE")[8:]...)...))
	if _, err := repo.Update(context.Background(), testTenant, in); !errors.Is(err, ErrTenantQuotaBelowUsage) {
		t.Fatalf("want ErrTenantQuotaBelowUsage, got %v", err)
	}

	// zero-row update -> re-read -> tenant gone
	m.ExpectQuery(regexp.QuoteMeta(`UPDATE adc_tenants
		SET quota_devices = $2, quota_calls_monthly = $3, status = $4,
		    metadata = COALESCE(metadata, '{}'::jsonb) || $5::jsonb, updated_at = now()
		WHERE id = $1::uuid AND deleted_at IS NULL
		  AND used_devices <= $2 AND used_calls_month <= $3
		RETURNING `+tenantCols)).
		WithArgs(testTenant, 50, int64(5000), "ACTIVE", `{"max_agent_keys":0,"audit_retention_days":0}`).
		WillReturnRows(pgxmock.NewRows(tenantRowCols))
	m.ExpectQuery(regexp.QuoteMeta(`SELECT ` + tenantCols + ` FROM adc_tenants WHERE id = $1::uuid AND deleted_at IS NULL`)).
		WithArgs(testTenant).
		WillReturnRows(pgxmock.NewRows(tenantRowCols))
	if _, err := repo.Update(context.Background(), testTenant, in); !errors.Is(err, ErrTenantNotFound) {
		t.Fatalf("want ErrTenantNotFound, got %v", err)
	}

	if err := m.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestPGTenantRepoMeta(t *testing.T) {
	m := newMockPool(t)
	repo := NewPGTenantRepo(m)

	// GetMeta decodes the stored document; an empty column decodes to
	// an empty map.
	m.ExpectQuery(regexp.QuoteMeta(`SELECT COALESCE(metadata, '{}'::jsonb)
		FROM adc_tenants WHERE id = $1::uuid AND deleted_at IS NULL`)).
		WithArgs(testTenant).
		WillReturnRows(pgxmock.NewRows([]string{"metadata"}).AddRow([]byte(`{"branding":{"title":"Acme"},"budget_monthly_cents":10000}`)))
	meta, err := repo.GetMeta(context.Background(), testTenant)
	if err != nil {
		t.Fatalf("GetMeta: %v", err)
	}
	if got := metaInt64(meta, "budget_monthly_cents"); got != 10000 {
		t.Fatalf("budget = %d, want 10000", got)
	}

	// SetMeta merges set keys and removes nil-valued keys in one
	// UPDATE; unknown tenants answer ErrTenantNotFound.
	m.ExpectQuery(regexp.QuoteMeta(`UPDATE adc_tenants
		SET metadata = COALESCE(metadata, '{}'::jsonb) || $2::jsonb - $3,
		    updated_at = now()
		WHERE id = $1::uuid AND deleted_at IS NULL
		RETURNING metadata`)).
		WithArgs(testTenant, `{"branding":{"title":"Acme 2"}}`, []string{"budget_monthly_cents"}).
		WillReturnRows(pgxmock.NewRows([]string{"metadata"}).AddRow([]byte(`{"branding":{"title":"Acme 2"}}`)))
	meta, err = repo.SetMeta(context.Background(), testTenant, map[string]any{
		"branding":             map[string]any{"title": "Acme 2"},
		"budget_monthly_cents": nil,
	})
	if err != nil {
		t.Fatalf("SetMeta: %v", err)
	}
	if _, ok := meta["budget_monthly_cents"]; ok {
		t.Fatalf("budget key not removed: %+v", meta)
	}

	m.ExpectQuery(regexp.QuoteMeta(`UPDATE adc_tenants
		SET metadata = COALESCE(metadata, '{}'::jsonb) || $2::jsonb - $3,
		    updated_at = now()
		WHERE id = $1::uuid AND deleted_at IS NULL
		RETURNING metadata`)).
		WithArgs(testUUID2, `{}`, []string{}).
		WillReturnRows(pgxmock.NewRows([]string{"metadata"}))
	if _, err := repo.SetMeta(context.Background(), testUUID2, map[string]any{}); !errors.Is(err, ErrTenantNotFound) {
		t.Fatalf("want ErrTenantNotFound, got %v", err)
	}

	if err := m.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestPGTenantRepoList(t *testing.T) {
	m := newMockPool(t)
	repo := NewPGTenantRepo(m)

	m.ExpectQuery(regexp.QuoteMeta(`
		SELECT t.id::text, t.name, t.status, t.created_at,
		       (SELECT count(*) FROM adc_devices d
		         WHERE d.tenant_id = t.id AND d.deleted_at IS NULL) AS device_count,
		       (SELECT count(*) FROM adc_agent_api_keys k
		         WHERE k.tenant_id = t.id AND k.revoked_at IS NULL) AS key_count,
		       count(*) OVER () AS total
		  FROM adc_tenants t
		 WHERE t.deleted_at IS NULL
		   AND ($1::text = '' OR t.status = $1)
		   AND ($2::text = '' OR t.name ILIKE $3)
		   AND ($4::uuid IS NULL OR t.id = $4)
		 ORDER BY t.created_at DESC, t.id
		 LIMIT $5 OFFSET $6`)).
		WithArgs("ACTIVE", "demo", "%demo%", nil, 20, 0).
		WillReturnRows(pgxmock.NewRows([]string{"id", "name", "status", "created_at", "device_count", "key_count", "total"}).
			AddRow(testTenant, "Demo", "ACTIVE", time.Now(), 1, 2, 1))
	list, total, err := repo.List(context.Background(), TenantFilter{Status: "active", Keyword: "demo"}, Page{Number: 1, Size: 20})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 || total != 1 || list[0].Name != "Demo" {
		t.Fatalf("unexpected list: %+v total=%d", list, total)
	}
	if err := m.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// ---------------------------------------------------------------------------
// pgApiKeyRepo
// ---------------------------------------------------------------------------

var apiKeyRowCols = []string{
	"id", "tenant_id", "name", "agent_id", "key_prefix", "scopes",
	"expires_at", "revoked_at", "last_used_at", "created_at",
}

func apiKeyRowVals() []any {
	return []any{
		testUUID, testTenant, "demo-agent", "demo-agent", "adc_abc12345",
		[]byte(`["*"]`), nil, nil, nil, time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}
}

func TestPGApiKeyRepoIssue(t *testing.T) {
	m := newMockPool(t)
	repo := NewPGApiKeyRepo(m)
	key := &NewKey{
		TenantID: testTenant, Name: "demo-agent", AgentID: "demo-agent",
		KeyPrefix: "adc_abc12345", KeyHash: "h1", SecretHash: "h2", Scopes: []string{"*"},
	}

	m.ExpectBegin()
	m.ExpectQuery(regexp.QuoteMeta(`SELECT status FROM adc_tenants WHERE id = $1::uuid AND deleted_at IS NULL`)).
		WithArgs(testTenant).
		WillReturnRows(pgxmock.NewRows([]string{"status"}).AddRow("ACTIVE"))
	m.ExpectQuery(regexp.QuoteMeta(`INSERT INTO adc_agent_api_keys
		(tenant_id, name, agent_id, key_prefix, key_hash, secret_hash,
		 scope_mode, scopes, expires_at, created_by)
		VALUES ($1::uuid, $2, $3, $4, $5, $6, 'ALLOW_LIST', $7, $8, $9::uuid)
		RETURNING `+keyCols)).
		WithArgs(testTenant, "demo-agent", "demo-agent", "adc_abc12345", "h1", "h2", `["*"]`, pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows(apiKeyRowCols).AddRow(apiKeyRowVals()...))
	m.ExpectCommit()
	k, err := repo.Issue(context.Background(), key)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if k.Name != "demo-agent" || k.ScopeMode != "ALLOW_LIST" || len(k.Scopes) != 1 {
		t.Fatalf("unexpected key: %+v", k)
	}

	// tenantGate: missing tenant
	m.ExpectBegin()
	m.ExpectQuery(regexp.QuoteMeta(`SELECT status FROM adc_tenants WHERE id = $1::uuid AND deleted_at IS NULL`)).
		WithArgs(testTenant).
		WillReturnRows(pgxmock.NewRows([]string{"status"}))
	m.ExpectRollback()
	if _, err := repo.Issue(context.Background(), key); !errors.Is(err, ErrTenantNotFound) {
		t.Fatalf("want ErrTenantNotFound, got %v", err)
	}

	// tenantGate: suspended tenant
	m.ExpectBegin()
	m.ExpectQuery(regexp.QuoteMeta(`SELECT status FROM adc_tenants WHERE id = $1::uuid AND deleted_at IS NULL`)).
		WithArgs(testTenant).
		WillReturnRows(pgxmock.NewRows([]string{"status"}).AddRow("SUSPENDED"))
	m.ExpectRollback()
	if _, err := repo.Issue(context.Background(), key); !errors.Is(err, ErrTenantSuspended) {
		t.Fatalf("want ErrTenantSuspended, got %v", err)
	}

	// hash collision -> retry sentinel
	m.ExpectBegin()
	m.ExpectQuery(regexp.QuoteMeta(`SELECT status FROM adc_tenants WHERE id = $1::uuid AND deleted_at IS NULL`)).
		WithArgs(testTenant).
		WillReturnRows(pgxmock.NewRows([]string{"status"}).AddRow("ACTIVE"))
	m.ExpectQuery(regexp.QuoteMeta(`INSERT INTO adc_agent_api_keys
		(tenant_id, name, agent_id, key_prefix, key_hash, secret_hash,
		 scope_mode, scopes, expires_at, created_by)
		VALUES ($1::uuid, $2, $3, $4, $5, $6, 'ALLOW_LIST', $7, $8, $9::uuid)
		RETURNING `+keyCols)).
		WithArgs(testTenant, "demo-agent", "demo-agent", "adc_abc12345", "h1", "h2", `["*"]`, pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(&pgconn.PgError{Code: "23505"})
	m.ExpectRollback()
	if _, err := repo.Issue(context.Background(), key); !errors.Is(err, ErrApiKeyConflict) {
		t.Fatalf("want ErrApiKeyConflict, got %v", err)
	}

	if err := m.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestPGApiKeyRepoGetListRevoke(t *testing.T) {
	m := newMockPool(t)
	repo := NewPGApiKeyRepo(m)

	// Get ok
	m.ExpectQuery(regexp.QuoteMeta(`SELECT ` + keyCols + ` FROM adc_agent_api_keys WHERE id = $1::uuid`)).
		WithArgs(testUUID).
		WillReturnRows(pgxmock.NewRows(apiKeyRowCols).AddRow(apiKeyRowVals()...))
	if _, err := repo.Get(context.Background(), testUUID); err != nil {
		t.Fatalf("Get: %v", err)
	}
	// Get not found
	m.ExpectQuery(regexp.QuoteMeta(`SELECT ` + keyCols + ` FROM adc_agent_api_keys WHERE id = $1::uuid`)).
		WithArgs(testUUID2).
		WillReturnRows(pgxmock.NewRows(apiKeyRowCols))
	if _, err := repo.Get(context.Background(), testUUID2); !errors.Is(err, ErrApiKeyNotFound) {
		t.Fatalf("want ErrApiKeyNotFound, got %v", err)
	}

	// List
	m.ExpectQuery(regexp.QuoteMeta(`SELECT `+keyCols+`, count(*) OVER () AS total
		FROM adc_agent_api_keys
		WHERE tenant_id = $1::uuid
		ORDER BY created_at DESC, id
		LIMIT $2 OFFSET $3`)).
		WithArgs(testTenant, 20, 0).
		WillReturnRows(pgxmock.NewRows(append(append([]string{}, apiKeyRowCols...), "total")).
			AddRow(append(apiKeyRowVals(), 1)...))
	keys, total, err := repo.List(context.Background(), testTenant, Page{Number: 1, Size: 20})
	if err != nil || len(keys) != 1 || total != 1 {
		t.Fatalf("List: %v err=%v total=%d", keys, err, total)
	}

	// Revoke ok
	m.ExpectQuery(regexp.QuoteMeta(`UPDATE adc_agent_api_keys
		SET revoked_at = now(), revoke_reason = $2, updated_at = now()
		WHERE id = $1::uuid AND revoked_at IS NULL
		RETURNING `+keyCols)).
		WithArgs(testUUID, "rotated").
		WillReturnRows(pgxmock.NewRows(apiKeyRowCols).AddRow(apiKeyRowVals()...))
	if _, err := repo.Revoke(context.Background(), testUUID, "rotated"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	// Revoke on an already revoked key
	now := time.Now()
	m.ExpectQuery(regexp.QuoteMeta(`UPDATE adc_agent_api_keys
		SET revoked_at = now(), revoke_reason = $2, updated_at = now()
		WHERE id = $1::uuid AND revoked_at IS NULL
		RETURNING `+keyCols)).
		WithArgs(testUUID2, "again").
		WillReturnRows(pgxmock.NewRows(apiKeyRowCols))
	m.ExpectQuery(regexp.QuoteMeta(`SELECT ` + keyCols + ` FROM adc_agent_api_keys WHERE id = $1::uuid`)).
		WithArgs(testUUID2).
		WillReturnRows(pgxmock.NewRows(apiKeyRowCols).AddRow(append(append(apiKeyRowVals()[:7], &now), apiKeyRowVals()[8:]...)...))
	if _, err := repo.Revoke(context.Background(), testUUID2, "again"); !errors.Is(err, ErrApiKeyAlreadyRevoked) {
		t.Fatalf("want ErrApiKeyAlreadyRevoked, got %v", err)
	}
	// Revoke on a missing key
	m.ExpectQuery(regexp.QuoteMeta(`UPDATE adc_agent_api_keys
		SET revoked_at = now(), revoke_reason = $2, updated_at = now()
		WHERE id = $1::uuid AND revoked_at IS NULL
		RETURNING `+keyCols)).
		WithArgs(testUUID2, "x").
		WillReturnRows(pgxmock.NewRows(apiKeyRowCols))
	m.ExpectQuery(regexp.QuoteMeta(`SELECT ` + keyCols + ` FROM adc_agent_api_keys WHERE id = $1::uuid`)).
		WithArgs(testUUID2).
		WillReturnRows(pgxmock.NewRows(apiKeyRowCols))
	if _, err := repo.Revoke(context.Background(), testUUID2, "x"); !errors.Is(err, ErrApiKeyNotFound) {
		t.Fatalf("want ErrApiKeyNotFound, got %v", err)
	}

	if err := m.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestPGApiKeyRepoRotate(t *testing.T) {
	m := newMockPool(t)
	repo := NewPGApiKeyRepo(m)
	key := &NewKey{TenantID: testTenant, Name: "demo-agent", AgentID: "demo-agent",
		KeyPrefix: "adc_abc12345", KeyHash: "h1", SecretHash: "h2", Scopes: []string{"*"}}

	m.ExpectBegin()
	m.ExpectExec(regexp.QuoteMeta(`UPDATE adc_agent_api_keys
		SET revoked_at = now(), revoke_reason = $2, updated_at = now()
		WHERE id = $1::uuid AND revoked_at IS NULL`)).
		WithArgs(testUUID, "rotate").
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	m.ExpectQuery(regexp.QuoteMeta(`INSERT INTO adc_agent_api_keys
		(tenant_id, name, agent_id, key_prefix, key_hash, secret_hash,
		 scope_mode, scopes, expires_at, created_by)
		VALUES ($1::uuid, $2, $3, $4, $5, $6, 'ALLOW_LIST', $7, $8, $9::uuid)
		RETURNING `+keyCols)).
		WithArgs(testTenant, "demo-agent", "demo-agent", "adc_abc12345", "h1", "h2", `["*"]`, pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows(apiKeyRowCols).AddRow(apiKeyRowVals()...))
	m.ExpectCommit()
	if _, err := repo.Rotate(context.Background(), testUUID, key, "rotate"); err != nil {
		t.Fatalf("Rotate: %v", err)
	}

	// already revoked
	now := time.Now()
	m.ExpectBegin()
	m.ExpectExec(regexp.QuoteMeta(`UPDATE adc_agent_api_keys
		SET revoked_at = now(), revoke_reason = $2, updated_at = now()
		WHERE id = $1::uuid AND revoked_at IS NULL`)).
		WithArgs(testUUID2, "rotate").
		WillReturnResult(pgxmock.NewResult("UPDATE", 0))
	m.ExpectQuery(regexp.QuoteMeta(`SELECT revoked_at FROM adc_agent_api_keys WHERE id = $1::uuid`)).
		WithArgs(testUUID2).
		WillReturnRows(pgxmock.NewRows([]string{"revoked_at"}).AddRow(&now))
	m.ExpectRollback()
	if _, err := repo.Rotate(context.Background(), testUUID2, key, "rotate"); !errors.Is(err, ErrApiKeyAlreadyRevoked) {
		t.Fatalf("want ErrApiKeyAlreadyRevoked, got %v", err)
	}

	// missing key
	m.ExpectBegin()
	m.ExpectExec(regexp.QuoteMeta(`UPDATE adc_agent_api_keys
		SET revoked_at = now(), revoke_reason = $2, updated_at = now()
		WHERE id = $1::uuid AND revoked_at IS NULL`)).
		WithArgs(testUUID2, "rotate").
		WillReturnResult(pgxmock.NewResult("UPDATE", 0))
	m.ExpectQuery(regexp.QuoteMeta(`SELECT revoked_at FROM adc_agent_api_keys WHERE id = $1::uuid`)).
		WithArgs(testUUID2).
		WillReturnRows(pgxmock.NewRows([]string{"revoked_at"}))
	m.ExpectRollback()
	if _, err := repo.Rotate(context.Background(), testUUID2, key, "rotate"); !errors.Is(err, ErrApiKeyNotFound) {
		t.Fatalf("want ErrApiKeyNotFound, got %v", err)
	}

	if err := m.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// ---------------------------------------------------------------------------
// pgToolRepo / pgPolicyRepo / pgAuditQueryRepo
// ---------------------------------------------------------------------------

var toolRowCols = []string{
	"id", "tenant_id", "device_id", "tool_name", "display_name", "description",
	"input_schema", "risk_level", "risk_changed_by", "schema_version", "is_enabled", "updated_at",
}

func toolRowVals() []any {
	return []any{
		testUUID, testTenant, testUUID2, "move_to", "", "Move", []byte(`{"type":"object"}`),
		2, "", "1.0", true, time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}
}

func TestPGToolRepoListGetUpdate(t *testing.T) {
	m := newMockPool(t)
	repo := NewPGToolRepo(m)

	// ListTools
	m.ExpectQuery(regexp.QuoteMeta(`SELECT `+toolCols+`, count(*) OVER () AS total
		FROM adc_device_tools WHERE device_id = $1::uuid
		ORDER BY tool_name LIMIT $2 OFFSET $3`)).
		WithArgs(testUUID2, 20, 0).
		WillReturnRows(pgxmock.NewRows(append(append([]string{}, toolRowCols...), "total")).
			AddRow(append(toolRowVals(), 1)...))
	tools, total, err := repo.ListTools(context.Background(), testUUID2, Page{Number: 1, Size: 20})
	if err != nil || len(tools) != 1 || total != 1 {
		t.Fatalf("ListTools: %v err=%v total=%d", tools, err, total)
	}
	if tools[0].InputSchema == nil {
		t.Fatalf("input schema not decoded: %+v", tools[0])
	}

	// GetTool ok
	m.ExpectQuery(regexp.QuoteMeta(`SELECT `+toolCols+` FROM adc_device_tools
		WHERE device_id = $1::uuid AND tool_name = $2`)).
		WithArgs(testUUID2, "move_to").
		WillReturnRows(pgxmock.NewRows(toolRowCols).AddRow(toolRowVals()...))
	if _, err := repo.GetTool(context.Background(), testUUID2, "move_to"); err != nil {
		t.Fatalf("GetTool: %v", err)
	}
	// GetTool not found
	m.ExpectQuery(regexp.QuoteMeta(`SELECT `+toolCols+` FROM adc_device_tools
		WHERE device_id = $1::uuid AND tool_name = $2`)).
		WithArgs(testUUID2, "nope").
		WillReturnRows(pgxmock.NewRows(toolRowCols))
	if _, err := repo.GetTool(context.Background(), testUUID2, "nope"); !errors.Is(err, ErrToolNotFound) {
		t.Fatalf("want ErrToolNotFound, got %v", err)
	}

	// UpdateTools: one ok + one missing (partial failure)
	risk := 1
	m.ExpectQuery(regexp.QuoteMeta(`UPDATE adc_device_tools
			SET risk_level = COALESCE($2, risk_level),
			    is_enabled = COALESCE($3, is_enabled),
			    risk_changed_by = COALESCE($4::uuid, risk_changed_by),
			    updated_at = now()
			WHERE device_id = $1::uuid AND tool_name = $5
			RETURNING `+toolCols)).
		WithArgs(testUUID2, risk, nil, "user-1", "move_to").
		WillReturnRows(pgxmock.NewRows(toolRowCols).AddRow(toolRowVals()...))
	m.ExpectQuery(regexp.QuoteMeta(`UPDATE adc_device_tools
			SET risk_level = COALESCE($2, risk_level),
			    is_enabled = COALESCE($3, is_enabled),
			    risk_changed_by = COALESCE($4::uuid, risk_changed_by),
			    updated_at = now()
			WHERE device_id = $1::uuid AND tool_name = $5
			RETURNING `+toolCols)).
		WithArgs(testUUID2, nil, nil, nil, "missing").
		WillReturnRows(pgxmock.NewRows(toolRowCols))
	out, err := repo.UpdateTools(context.Background(), testUUID2, []ToolChange{
		{ToolName: "move_to", RiskLevel: &risk},
		{ToolName: "missing"},
	}, "user-1")
	if err != nil {
		t.Fatalf("UpdateTools: %v", err)
	}
	if len(out) != 2 || out[0].Tool == nil || !errors.Is(out[1].Err, ErrToolNotFound) {
		t.Fatalf("unexpected outcomes: %+v", out)
	}

	if err := m.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestPGPolicyRepo(t *testing.T) {
	m := newMockPool(t)
	repo := NewPGPolicyRepo(m)

	// defaults when the stored doc is absent
	m.ExpectQuery(regexp.QuoteMeta(`
		SELECT COALESCE(metadata->'approval_policy', 'null'::jsonb)
		  FROM adc_tenants
		 WHERE id = $1::uuid AND deleted_at IS NULL`)).
		WithArgs(testTenant).
		WillReturnRows(pgxmock.NewRows([]string{"doc"}).AddRow([]byte("null")))
	p, err := repo.GetPolicy(context.Background(), testTenant)
	if err != nil {
		t.Fatalf("GetPolicy: %v", err)
	}
	if p.ApprovalTimeoutSec != defaultApprovalTimeoutSec {
		t.Fatalf("want default timeout %d, got %d", defaultApprovalTimeoutSec, p.ApprovalTimeoutSec)
	}

	// stored doc
	m.ExpectQuery(regexp.QuoteMeta(`
		SELECT COALESCE(metadata->'approval_policy', 'null'::jsonb)
		  FROM adc_tenants
		 WHERE id = $1::uuid AND deleted_at IS NULL`)).
		WithArgs(testTenant).
		WillReturnRows(pgxmock.NewRows([]string{"doc"}).
			AddRow([]byte(`{"approval_timeout_sec":60,"approvers":["u1"]}`)))
	p, err = repo.GetPolicy(context.Background(), testTenant)
	if err != nil || p.ApprovalTimeoutSec != 60 || len(p.Approvers) != 1 {
		t.Fatalf("GetPolicy stored: %+v err=%v", p, err)
	}

	// tenant missing
	m.ExpectQuery(regexp.QuoteMeta(`
		SELECT COALESCE(metadata->'approval_policy', 'null'::jsonb)
		  FROM adc_tenants
		 WHERE id = $1::uuid AND deleted_at IS NULL`)).
		WithArgs(testUUID2).
		WillReturnRows(pgxmock.NewRows([]string{"doc"}))
	if _, err := repo.GetPolicy(context.Background(), testUUID2); !errors.Is(err, ErrTenantNotFound) {
		t.Fatalf("want ErrTenantNotFound, got %v", err)
	}

	// SetPolicy ok
	m.ExpectQuery(regexp.QuoteMeta(`UPDATE adc_tenants
		SET metadata = metadata || $2::jsonb, updated_at = now()
		WHERE id = $1::uuid AND deleted_at IS NULL
		RETURNING updated_at`)).
		WithArgs(testTenant, pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"updated_at"}).AddRow(time.Now()))
	if _, err := repo.SetPolicy(context.Background(), testTenant, &ApprovalPolicy{TenantID: testTenant, ApprovalTimeoutSec: 120, Approvers: []string{"u1"}}); err != nil {
		t.Fatalf("SetPolicy: %v", err)
	}
	// SetPolicy tenant missing
	m.ExpectQuery(regexp.QuoteMeta(`UPDATE adc_tenants
		SET metadata = metadata || $2::jsonb, updated_at = now()
		WHERE id = $1::uuid AND deleted_at IS NULL
		RETURNING updated_at`)).
		WithArgs(testUUID2, pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"updated_at"}))
	if _, err := repo.SetPolicy(context.Background(), testUUID2, &ApprovalPolicy{TenantID: testUUID2, ApprovalTimeoutSec: 120}); !errors.Is(err, ErrTenantNotFound) {
		t.Fatalf("want ErrTenantNotFound, got %v", err)
	}

	if err := m.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

var auditRowCols = []string{
	"id", "tenant_id", "event_type", "actor_type", "actor_id",
	"api_key_id", "device_id", "device_code", "tool_name", "risk_level",
	"request_params", "response_payload", "response_truncated", "execution_duration_ms", "status",
	"hitl_ticket_id", "hitl_approver", "hitl_comment", "exemption_basis",
	"request_id", "created_at",
}

func auditRowVals() []any {
	return []any{
		testUUID, testTenant, "tool_call", "agent", "demo-agent",
		"", testUUID2, "dev-1", "get_position", intPtr(0),
		[]byte(`{"x":1}`), []byte(`{"x":1}`), false, intPtr(12), "success",
		"", "", "", "",
		"req-1", time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}
}

func TestAuditWhere(t *testing.T) {
	var args []any
	where := auditWhere(AuditFilter{TenantID: testTenant}, &args)
	if !strings.Contains(where, "a.tenant_id = $1") || len(args) != 1 {
		t.Fatalf("unexpected where: %s args=%v", where, args)
	}

	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	args = nil
	where = auditWhere(AuditFilter{
		TenantID: testTenant, TimeFrom: &from, TimeTo: &to,
		DeviceID: testUUID2, DeviceCode: "dev-1", ToolName: "t1",
		Status: "success", EventType: "tool_call", AgentID: "a1", Keyword: "spindle",
	}, &args)
	for _, want := range []string{"$2", "$3", "$4", "$5", "$6", "$7", "$8", "$9"} {
		if !strings.Contains(where, want) {
			t.Fatalf("where clause missing %s: %s", want, where)
		}
	}
	if len(args) != 13 {
		t.Fatalf("want 13 args, got %d: %v", len(args), args)
	}
	// keyword pattern args
	if args[9] != "%spindle%" {
		t.Fatalf("unexpected keyword arg: %v", args[9])
	}
}

func TestPGAuditQueryRepoQueryCountExport(t *testing.T) {
	m := newMockPool(t)
	repo := NewPGAuditQueryRepo(m)
	ctx := context.Background()
	f := AuditFilter{TenantID: testTenant}

	// Query: two rows with limit 1 -> one item + NextCursor set
	m.ExpectQuery(regexp.QuoteMeta(`SELECT a.id::text, a.tenant_id::text, a.event_type, a.actor_type,
       COALESCE(a.actor_id, ''),
       COALESCE(a.api_key_id::text, ''), COALESCE(a.device_id::text, ''),
       COALESCE(d.device_code, ''),
       COALESCE(a.tool_name, ''), a.risk_level,
       COALESCE(a.request_params, '{}'::jsonb),
       COALESCE(a.response_payload, '{}'::jsonb),
       a.response_truncated, a.execution_duration_ms, a.status,
       COALESCE(a.hitl_ticket_id::text, ''), COALESCE(a.hitl_approver, ''),
       COALESCE(a.hitl_comment, ''), COALESCE(a.exemption_basis, ''),
       a.request_id::text, a.created_at
  FROM adc_audit_logs a
  LEFT JOIN adc_devices d ON d.id = a.device_id WHERE a.tenant_id = $1::uuid ORDER BY a.created_at DESC, a.id DESC LIMIT $2`)).
		WithArgs(testTenant, 2).
		WillReturnRows(pgxmock.NewRows(auditRowCols).
			AddRow(auditRowVals()...).
			AddRow(auditRowVals()...))
	page, err := repo.Query(ctx, f, nil, 1)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(page.Items) != 1 || page.NextCursor == nil || page.NextCursor.ID != testUUID {
		t.Fatalf("unexpected page: %+v", page)
	}

	// Count
	m.ExpectQuery(regexp.QuoteMeta(`SELECT count(*) FROM adc_audit_logs a
		LEFT JOIN adc_devices d ON d.id = a.device_id WHERE a.tenant_id = $1::uuid`)).
		WithArgs(testTenant).
		WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(7))
	if n, err := repo.Count(ctx, f); err != nil || n != 7 {
		t.Fatalf("Count: %d %v", n, err)
	}

	// Export: one row under the cap
	m.ExpectQuery(regexp.QuoteMeta(`SELECT a.id::text, a.tenant_id::text, a.event_type, a.actor_type,
       COALESCE(a.actor_id, ''),
       COALESCE(a.api_key_id::text, ''), COALESCE(a.device_id::text, ''),
       COALESCE(d.device_code, ''),
       COALESCE(a.tool_name, ''), a.risk_level,
       COALESCE(a.request_params, '{}'::jsonb),
       COALESCE(a.response_payload, '{}'::jsonb),
       a.response_truncated, a.execution_duration_ms, a.status,
       COALESCE(a.hitl_ticket_id::text, ''), COALESCE(a.hitl_approver, ''),
       COALESCE(a.hitl_comment, ''), COALESCE(a.exemption_basis, ''),
       a.request_id::text, a.created_at
  FROM adc_audit_logs a
  LEFT JOIN adc_devices d ON d.id = a.device_id WHERE a.tenant_id = $1::uuid ORDER BY a.created_at DESC, a.id DESC LIMIT $2`)).
		WithArgs(testTenant, 11).
		WillReturnRows(pgxmock.NewRows(auditRowCols).AddRow(auditRowVals()...))
	var buf strings.Builder
	if n, err := repo.Export(ctx, f, 10, &buf); err != nil || n != 1 {
		t.Fatalf("Export: n=%d err=%v", n, err)
	}
	if !strings.Contains(buf.String(), "tool_call") {
		t.Fatalf("csv missing event_type: %s", buf.String())
	}

	// Export: cap exceeded on the second row
	m.ExpectQuery(regexp.QuoteMeta(`SELECT a.id::text, a.tenant_id::text, a.event_type, a.actor_type,
       COALESCE(a.actor_id, ''),
       COALESCE(a.api_key_id::text, ''), COALESCE(a.device_id::text, ''),
       COALESCE(d.device_code, ''),
       COALESCE(a.tool_name, ''), a.risk_level,
       COALESCE(a.request_params, '{}'::jsonb),
       COALESCE(a.response_payload, '{}'::jsonb),
       a.response_truncated, a.execution_duration_ms, a.status,
       COALESCE(a.hitl_ticket_id::text, ''), COALESCE(a.hitl_approver, ''),
       COALESCE(a.hitl_comment, ''), COALESCE(a.exemption_basis, ''),
       a.request_id::text, a.created_at
  FROM adc_audit_logs a
  LEFT JOIN adc_devices d ON d.id = a.device_id WHERE a.tenant_id = $1::uuid ORDER BY a.created_at DESC, a.id DESC LIMIT $2`)).
		WithArgs(testTenant, 2).
		WillReturnRows(pgxmock.NewRows(auditRowCols).
			AddRow(auditRowVals()...).
			AddRow(auditRowVals()...))
	buf.Reset()
	if n, err := repo.Export(ctx, f, 1, &buf); !errors.Is(err, ErrAuditExportLimitExceeded) || n != 1 {
		t.Fatalf("Export cap: n=%d err=%v", n, err)
	}

	// Export: bad params json in row -> scan error
	m.ExpectQuery(regexp.QuoteMeta(`SELECT a.id::text, a.tenant_id::text, a.event_type, a.actor_type,
       COALESCE(a.actor_id, ''),
       COALESCE(a.api_key_id::text, ''), COALESCE(a.device_id::text, ''),
       COALESCE(d.device_code, ''),
       COALESCE(a.tool_name, ''), a.risk_level,
       COALESCE(a.request_params, '{}'::jsonb),
       COALESCE(a.response_payload, '{}'::jsonb),
       a.response_truncated, a.execution_duration_ms, a.status,
       COALESCE(a.hitl_ticket_id::text, ''), COALESCE(a.hitl_approver, ''),
       COALESCE(a.hitl_comment, ''), COALESCE(a.exemption_basis, ''),
       a.request_id::text, a.created_at
  FROM adc_audit_logs a
  LEFT JOIN adc_devices d ON d.id = a.device_id WHERE a.tenant_id = $1::uuid ORDER BY a.created_at DESC, a.id DESC LIMIT $2`)).
		WithArgs(testTenant, 11).
		WillReturnRows(pgxmock.NewRows(auditRowCols).
			AddRow(append(append(auditRowVals()[:10], []byte(`{bad`)), auditRowVals()[11:]...)...))
	buf.Reset()
	if _, err := repo.Export(ctx, f, 10, &buf); err == nil {
		t.Fatal("want params decode error")
	}

	// Query error path
	m.ExpectQuery(regexp.QuoteMeta(`SELECT a.id::text, a.tenant_id::text, a.event_type, a.actor_type,
       COALESCE(a.actor_id, ''),
       COALESCE(a.api_key_id::text, ''), COALESCE(a.device_id::text, ''),
       COALESCE(d.device_code, ''),
       COALESCE(a.tool_name, ''), a.risk_level,
       COALESCE(a.request_params, '{}'::jsonb),
       COALESCE(a.response_payload, '{}'::jsonb),
       a.response_truncated, a.execution_duration_ms, a.status,
       COALESCE(a.hitl_ticket_id::text, ''), COALESCE(a.hitl_approver, ''),
       COALESCE(a.hitl_comment, ''), COALESCE(a.exemption_basis, ''),
       a.request_id::text, a.created_at
  FROM adc_audit_logs a
  LEFT JOIN adc_devices d ON d.id = a.device_id WHERE a.tenant_id = $1::uuid ORDER BY a.created_at DESC, a.id DESC LIMIT $2`)).
		WithArgs(testTenant, 2).
		WillReturnError(errors.New("query boom"))
	if _, err := repo.Query(ctx, f, nil, 1); err == nil {
		t.Fatal("want query error")
	}

	if err := m.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}
