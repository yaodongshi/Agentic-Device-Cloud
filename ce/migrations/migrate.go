// Package migrations applies the SQL migrations embedded in the ADC binary.
package migrations

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const advisoryLockID int64 = 0x4144434d494752 // "ADCMIGR"
const advisoryLockTimeout = 30 * time.Second
const advisoryUnlockTimeout = 5 * time.Second

var migrationName = regexp.MustCompile(`^(\d+)_([a-z0-9][a-z0-9_-]*)\.up\.sql$`)

//go:embed *.sql
var embedded embed.FS

// Migration is one ordered, forward-only schema change.
type Migration struct {
	Version int64
	Name    string
	SQL     string
}

type row interface {
	Scan(...any) error
}

type tx interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	Commit(context.Context) error
	Rollback(context.Context) error
}

type conn interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	QueryRow(context.Context, string, ...any) row
	Begin(context.Context) (tx, error)
}

type pgxConn struct{ conn *pgx.Conn }

func (p pgxConn) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return p.conn.Exec(ctx, sql, args...)
}

func (p pgxConn) QueryRow(ctx context.Context, sql string, args ...any) row {
	return p.conn.QueryRow(ctx, sql, args...)
}

func (p pgxConn) Begin(ctx context.Context) (tx, error) {
	return p.conn.Begin(ctx)
}

// Load validates and orders all up migrations in fsys.
func Load(fsys fs.FS) ([]Migration, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("migration: read embedded files: %w", err)
	}
	migrations := make([]Migration, 0, len(entries))
	seen := make(map[int64]string)
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".sql" {
			continue
		}
		match := migrationName.FindStringSubmatch(entry.Name())
		if match == nil {
			continue
		}
		version, err := strconv.ParseInt(match[1], 10, 64)
		if err != nil || version < 1 {
			return nil, fmt.Errorf("migration: invalid version in %q", entry.Name())
		}
		if previous, ok := seen[version]; ok {
			return nil, fmt.Errorf("migration: duplicate version %d in %q and %q", version, previous, entry.Name())
		}
		body, err := fs.ReadFile(fsys, entry.Name())
		if err != nil {
			return nil, fmt.Errorf("migration: read %q: %w", entry.Name(), err)
		}
		seen[version] = entry.Name()
		migrations = append(migrations, Migration{Version: version, Name: match[2], SQL: string(body)})
	}
	sort.Slice(migrations, func(i, j int) bool { return migrations[i].Version < migrations[j].Version })
	for i, migration := range migrations {
		if migration.Version != int64(i+1) {
			return nil, fmt.Errorf("migration: expected version %d, found %d", i+1, migration.Version)
		}
	}
	if len(migrations) == 0 {
		return nil, errors.New("migration: no embedded up migrations")
	}
	return migrations, nil
}

// Run applies all embedded migrations over connection. The caller must use a
// dedicated connection because PostgreSQL advisory locks are session scoped.
func Run(ctx context.Context, connection *pgx.Conn) error {
	return run(ctx, pgxConn{conn: connection}, embedded)
}

// RunFS applies migrations from fsys for integration testing.
func RunFS(ctx context.Context, connection *pgx.Conn, fsys fs.FS) error {
	return run(ctx, pgxConn{conn: connection}, fsys)
}

func run(ctx context.Context, connection conn, fsys fs.FS) (err error) {
	all, err := Load(fsys)
	if err != nil {
		return err
	}
	lockCtx, cancelLock := context.WithTimeout(ctx, advisoryLockTimeout)
	defer cancelLock()
	if _, err = connection.Exec(lockCtx, `SELECT pg_advisory_lock($1)`, advisoryLockID); err != nil {
		return fmt.Errorf("migration: acquire advisory lock: %w", err)
	}
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.Background(), advisoryUnlockTimeout)
		defer cancel()
		_, unlockErr := connection.Exec(unlockCtx, `SELECT pg_advisory_unlock($1)`, advisoryLockID)
		if err == nil && unlockErr != nil {
			err = fmt.Errorf("migration: release advisory lock: %w", unlockErr)
		}
	}()

	if _, err = connection.Exec(ctx, `CREATE TABLE IF NOT EXISTS adc_schema_migrations (
		version BIGINT PRIMARY KEY,
		name TEXT NOT NULL,
		applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
	)`); err != nil {
		return fmt.Errorf("migration: create version table: %w", err)
	}
	current, applied, err := readLedger(ctx, connection)
	if err != nil {
		return err
	}
	if applied != current {
		return fmt.Errorf("migration: version ledger is not contiguous (current %d, applied %d)", current, applied)
	}
	if applied == 0 {
		if err = baselineLegacySchema(ctx, connection); err != nil {
			return err
		}
		current, applied, err = readLedger(ctx, connection)
		if err != nil {
			return err
		}
		if applied != current {
			return fmt.Errorf("migration: legacy baseline produced a non-contiguous ledger (current %d, applied %d)", current, applied)
		}
	}
	target := all[len(all)-1].Version
	if current > target {
		return fmt.Errorf("migration: database version %d is newer than supported version %d", current, target)
	}
	for _, migration := range all {
		if migration.Version <= current {
			continue
		}
		migrationTx, beginErr := connection.Begin(ctx)
		if beginErr != nil {
			return fmt.Errorf("migration %04d_%s: begin: %w", migration.Version, migration.Name, beginErr)
		}
		if _, execErr := migrationTx.Exec(ctx, `SET LOCAL lock_timeout = '10s'; SET LOCAL statement_timeout = '5min'`); execErr != nil {
			_ = migrationTx.Rollback(ctx)
			return fmt.Errorf("migration %04d_%s: set timeouts: %w", migration.Version, migration.Name, execErr)
		}
		if _, execErr := migrationTx.Exec(ctx, migration.SQL); execErr != nil {
			_ = migrationTx.Rollback(ctx)
			return fmt.Errorf("migration %04d_%s: execute: %w", migration.Version, migration.Name, execErr)
		}
		if _, execErr := migrationTx.Exec(ctx, `INSERT INTO adc_schema_migrations (version, name) VALUES ($1, $2)`, migration.Version, migration.Name); execErr != nil {
			_ = migrationTx.Rollback(ctx)
			return fmt.Errorf("migration %04d_%s: record: %w", migration.Version, migration.Name, execErr)
		}
		if commitErr := migrationTx.Commit(ctx); commitErr != nil {
			return fmt.Errorf("migration %04d_%s: commit: %w", migration.Version, migration.Name, commitErr)
		}
		current = migration.Version
	}
	return nil
}

func readLedger(ctx context.Context, connection conn) (current, applied int64, err error) {
	var minimum int64
	if err = connection.QueryRow(ctx, `SELECT COALESCE(MAX(version), 0), COUNT(*), COALESCE(MIN(version), 0) FROM adc_schema_migrations`).Scan(&current, &applied, &minimum); err != nil {
		return 0, 0, fmt.Errorf("migration: read version ledger: %w", err)
	}
	if applied > 0 && minimum != 1 {
		return 0, 0, fmt.Errorf("migration: version ledger must start at 1 (minimum %d)", minimum)
	}
	return current, applied, nil
}

// V2.0 databases predate the migration ledger. Baseline only a contiguous
// sequence whose schema markers can be proven, then let newer migrations run.
func baselineLegacySchema(ctx context.Context, connection conn) error {
	// Older Compose releases applied SQL through docker-entrypoint-initdb.d.
	// Every historical version needs several independent markers: one surviving
	// object is not proof that its transaction completed.
	var versions int
	var incomplete string
	var nonContiguous bool
	if err := connection.QueryRow(ctx, `WITH markers(version, marker, present) AS (
		VALUES
		(1, 'table adc_tenants', to_regclass('adc_tenants') IS NOT NULL),
		(1, 'table adc_users', to_regclass('adc_users') IS NOT NULL),
		(1, 'table adc_devices', to_regclass('adc_devices') IS NOT NULL),
		(1, 'table adc_approval_tickets', to_regclass('adc_approval_tickets') IS NOT NULL),
		(1, 'table adc_audit_logs', to_regclass('adc_audit_logs') IS NOT NULL),
		(1, 'table adc_usage_events', to_regclass('adc_usage_events') IS NOT NULL),
		(1, 'index uq_tenants_code', to_regclass('uq_tenants_code') IS NOT NULL),
		(1, 'index uq_devices_code', to_regclass('uq_devices_code') IS NOT NULL),
		(1, 'constraint adc_users.tenant_id foreign key', EXISTS (
			SELECT 1 FROM pg_constraint c WHERE c.conrelid = to_regclass('adc_users') AND c.contype = 'f'
			AND pg_get_constraintdef(c.oid) LIKE 'FOREIGN KEY (tenant_id) REFERENCES adc_tenants(id)%')),
		(2, 'table billing_statements', to_regclass('billing_statements') IS NOT NULL),
		(2, 'index idx_billing_tenant_created', to_regclass('idx_billing_tenant_created') IS NOT NULL),
		(2, 'index idx_billing_overdue', to_regclass('idx_billing_overdue') IS NOT NULL),
		(2, 'constraint billing tenant period unique', EXISTS (
			SELECT 1 FROM pg_constraint c WHERE c.conrelid = to_regclass('billing_statements') AND c.contype = 'u'
			AND pg_get_constraintdef(c.oid) = 'UNIQUE (tenant_id, period_year, period_month)')),
		(3, 'column adc_devices.binding_status', EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='adc_devices' AND column_name='binding_status')),
		(3, 'column adc_devices.oauth_client_id', EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='adc_devices' AND column_name='oauth_client_id')),
		(3, 'column adc_devices.oauth_client_secret_enc', EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='adc_devices' AND column_name='oauth_client_secret_enc')),
		(3, 'column adc_devices.token_endpoint', EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='adc_devices' AND column_name='token_endpoint')),
		(3, 'column adc_devices.resource_identifier', EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='adc_devices' AND column_name='resource_identifier')),
		(3, 'column adc_devices.bound_at', EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='adc_devices' AND column_name='bound_at')),
		(3, 'column adc_devices.revoked_at', EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='adc_devices' AND column_name='revoked_at')),
		(3, 'index idx_devices_binding_status', to_regclass('idx_devices_binding_status') IS NOT NULL),
		(3, 'index idx_devices_binding_expire', to_regclass('idx_devices_binding_expire') IS NOT NULL),
		(4, 'table adc_tool_packages', to_regclass('adc_tool_packages') IS NOT NULL),
		(4, 'table adc_tool_package_installs', to_regclass('adc_tool_package_installs') IS NOT NULL),
		(4, 'index uq_tool_packages_name_version', to_regclass('uq_tool_packages_name_version') IS NOT NULL),
		(4, 'index uq_tool_packages_tenant_name_version', to_regclass('uq_tool_packages_tenant_name_version') IS NOT NULL),
		(4, 'index adc_tool_package_installs_pkey', to_regclass('adc_tool_package_installs_pkey') IS NOT NULL)
	), states AS (
		SELECT version, count(*) FILTER (WHERE present) AS found, count(*) AS expected,
			string_agg(marker, ', ' ORDER BY marker) FILTER (WHERE NOT present) AS missing
		FROM markers GROUP BY version
	), summary AS (
		SELECT COALESCE(max(version) FILTER (WHERE found = expected), 0) AS versions,
			COALESCE(string_agg(format('000%s missing [%s]', version, missing), '; ' ORDER BY version)
				FILTER (WHERE found > 0 AND found < expected), '') AS incomplete
		FROM states
	)
	SELECT versions, incomplete, EXISTS (
		SELECT 1 FROM states later JOIN states earlier ON earlier.version < later.version
		WHERE later.found = later.expected AND earlier.found = 0
	) FROM summary`).Scan(&versions, &incomplete, &nonContiguous); err != nil {
		return fmt.Errorf("migration: inspect legacy schema: %w", err)
	}
	if incomplete != "" {
		return fmt.Errorf("migration: legacy schema is partial; refusing baseline: %s", incomplete)
	}
	if nonContiguous {
		return errors.New("migration: legacy schema versions are not contiguous; refusing baseline")
	}
	if versions > 0 {
		if _, err := connection.Exec(ctx, `INSERT INTO adc_schema_migrations (version, name)
			SELECT version, 'legacy_baseline' FROM generate_series(1, $1) AS version`, versions); err != nil {
			return fmt.Errorf("migration: record legacy baseline versions 1-%d: %w", versions, err)
		}
	}
	return nil
}
