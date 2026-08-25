package migrations

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestPostgresMigrationLifecycle(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("ADC_MIGRATION_TEST_DATABASE_URL"))
	if dsn == "" {
		t.Skip("ADC_MIGRATION_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Skipf("PostgreSQL unavailable: %v", err)
	}
	defer admin.Close(context.Background())
	schema := fmt.Sprintf("adc_migration_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, `CREATE SCHEMA `+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	defer admin.Exec(context.Background(), `DROP SCHEMA `+pgx.Identifier{schema}.Sanitize()+` CASCADE`)

	open := func() *pgx.Conn {
		cfg, err := pgx.ParseConfig(dsn)
		if err != nil {
			t.Fatal(err)
		}
		cfg.RuntimeParams["search_path"] = schema
		conn, err := pgx.ConnectConfig(ctx, cfg)
		if err != nil {
			t.Fatal(err)
		}
		return conn
	}
	first := open()
	defer first.Close(context.Background())
	if err := Run(ctx, first); err != nil {
		t.Fatalf("empty database migration: %v", err)
	}
	if _, err := first.Exec(ctx, `INSERT INTO adc_tenants (code, name) VALUES ('migration-sentinel', 'keep me')`); err != nil {
		t.Fatal(err)
	}

	second := open()
	defer second.Close(context.Background())
	errs := make(chan error, 2)
	go func() { errs <- Run(ctx, first) }()
	go func() { errs <- Run(ctx, second) }()
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent idempotent migration: %v", err)
		}
	}
	var sentinelCount, versions int
	if err := first.QueryRow(ctx, `SELECT count(*) FROM adc_tenants WHERE code = 'migration-sentinel'`).Scan(&sentinelCount); err != nil {
		t.Fatal(err)
	}
	if err := first.QueryRow(ctx, `SELECT count(*) FROM adc_schema_migrations`).Scan(&versions); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(embedded)
	if err != nil {
		t.Fatal(err)
	}
	if sentinelCount != 1 || versions != len(loaded) {
		t.Fatalf("sentinel=%d versions=%d, want sentinel=1 versions=%d", sentinelCount, versions, len(loaded))
	}
}

func TestPostgresFailedMigrationRollsBack(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("ADC_MIGRATION_TEST_DATABASE_URL"))
	if dsn == "" {
		t.Skip("ADC_MIGRATION_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("adc_migration_fail_%d", time.Now().UnixNano())
	admin, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Skipf("PostgreSQL unavailable: %v", err)
	}
	defer admin.Close(context.Background())
	if _, err := admin.Exec(ctx, `CREATE SCHEMA `+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	defer admin.Exec(context.Background(), `DROP SCHEMA `+pgx.Identifier{schema}.Sanitize()+` CASCADE`)
	cfg.RuntimeParams["search_path"] = schema
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	files := fstest.MapFS{
		"0001_first.up.sql":  {Data: []byte(`CREATE TABLE first_ok (id integer)`)},
		"0002_broken.up.sql": {Data: []byte(`CREATE TABLE rolled_back (id integer); SELECT missing_function()`)},
	}
	if err := RunFS(ctx, conn, files); err == nil {
		t.Fatal("expected failing migration")
	}
	var current int
	if err := conn.QueryRow(ctx, `SELECT COALESCE(MAX(version), 0) FROM adc_schema_migrations`).Scan(&current); err != nil {
		t.Fatal(err)
	}
	var rolledBack bool
	if err := conn.QueryRow(ctx, `SELECT to_regclass('rolled_back') IS NOT NULL`).Scan(&rolledBack); err != nil {
		t.Fatal(err)
	}
	if current != 1 || rolledBack {
		t.Fatalf("current=%d rolledBackTable=%v, want 1/false", current, rolledBack)
	}
}

func TestPostgresCompleteLegacySchemaBaselinesOneThroughFour(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("ADC_MIGRATION_TEST_DATABASE_URL"))
	if dsn == "" {
		t.Skip("ADC_MIGRATION_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("adc_legacy_baseline_%d", time.Now().UnixNano())
	admin, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Skipf("PostgreSQL unavailable: %v", err)
	}
	defer admin.Close(context.Background())
	if _, err := admin.Exec(ctx, `CREATE SCHEMA `+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	defer admin.Exec(context.Background(), `DROP SCHEMA `+pgx.Identifier{schema}.Sanitize()+` CASCADE`)
	cfg.RuntimeParams["search_path"] = schema
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())

	for _, path := range []string{"0001_init.up.sql", "0002_billing.up.sql", "0003_mcp_binding.up.sql", "0004_tool_packages.up.sql"} {
		body, err := embedded.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Exec(ctx, string(body)); err != nil {
			t.Fatalf("prepare %s: %v", path, err)
		}
	}
	if err := Run(ctx, conn); err != nil {
		t.Fatalf("baseline complete legacy schema: %v", err)
	}
	var count, maximum, legacy int
	if err := conn.QueryRow(ctx, `SELECT COUNT(*), MAX(version), COUNT(*) FILTER (WHERE version <= 4 AND name = 'legacy_baseline') FROM adc_schema_migrations`).Scan(&count, &maximum, &legacy); err != nil {
		t.Fatal(err)
	}
	if count != 6 || maximum != 6 || legacy != 4 {
		t.Fatalf("count=%d max=%d legacy=%d, want 6/6/4", count, maximum, legacy)
	}
}

func TestPostgresOIDCLegacyIdentityBackfillFailsClosed(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("ADC_MIGRATION_TEST_DATABASE_URL"))
	if dsn == "" {
		t.Skip("ADC_MIGRATION_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("adc_oidc_backfill_%d", time.Now().UnixNano())
	admin, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Skipf("PostgreSQL unavailable: %v", err)
	}
	defer admin.Close(context.Background())
	if _, err := admin.Exec(ctx, `CREATE SCHEMA `+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	defer admin.Exec(context.Background(), `DROP SCHEMA `+pgx.Identifier{schema}.Sanitize()+` CASCADE`)
	cfg.RuntimeParams["search_path"] = schema
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())

	files := fstest.MapFS{}
	for _, base := range []string{"0001_init", "0002_billing", "0003_mcp_binding", "0004_tool_packages"} {
		path := base + ".up.sql"
		body, err := embedded.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		files[path] = &fstest.MapFile{Data: body}
	}
	if err := RunFS(ctx, conn, files); err != nil {
		t.Fatalf("prepare legacy schema: %v", err)
	}
	var tenantID string
	if err := conn.QueryRow(ctx, `INSERT INTO adc_tenants (code, name) VALUES ('oidc-backfill', 'OIDC Backfill') RETURNING id`).Scan(&tenantID); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `INSERT INTO adc_users (tenant_id, username, auth_source, metadata) VALUES
		($1, 'complete', 'OIDC', '{"oidc_issuer":"https://idp.example","oidc_subject":"subject-1"}'),
		($1, 'missing-issuer', 'OIDC', '{"oidc_subject":"subject-2"}')`, tenantID); err != nil {
		t.Fatal(err)
	}
	if err := Run(ctx, conn); err != nil {
		t.Fatalf("run OIDC backfill: %v", err)
	}
	var mapped, quarantined int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM adc_oidc_identities WHERE issuer = 'https://idp.example' AND subject = 'subject-1'`).Scan(&mapped); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM adc_oidc_identity_quarantine WHERE reason = 'MISSING_ISSUER'`).Scan(&quarantined); err != nil {
		t.Fatal(err)
	}
	if mapped != 1 || quarantined != 1 {
		t.Fatalf("mapped=%d quarantined=%d, want 1/1", mapped, quarantined)
	}
}
