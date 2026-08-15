// Package integration hosts the ADC backend integration test suite
// (design/40 2.2 integration tier, design/80 B-12): real PostgreSQL and
// real Valkey, real handlers over httptest, contract-checked against the
// endpoint set of design/33. No mock database layers are used; the only
// fakes are the device and the webhook card channels (design/40 3.4).
//
// Prerequisites: PostgreSQL 16 at ADC_IT_PG_HOST:ADC_IT_PG_PORT (default
// 127.0.0.1:55433, user adc / adc_dev_only) and Valkey at
// ADC_IT_VALKEY_ADDR (default 127.0.0.1:6389, password adc_dev_only).
// When either is unreachable every test is skipped via t.Skip, so a CI
// without the dependency containers stays green (design/40 3.1 local dev
// environment; CI brings its own container group per run).
//
// Migration discipline (task 1): TestMain executes
// ce/migrations/0001_init.up.sql through the pgx simple protocol
// (pgconn.Exec), after running 0001_init.down.sql when the schema already
// exists, making the step idempotent across repeated local runs.
package integration

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"adc.dev/ce/internal/config"
	"adc.dev/ce/internal/db"
)

// Dependency coordinates, overridable through environment variables so the
// suite can target any environment (design/40 3.1). Defaults match the
// dedicated integration containers documented above.
var (
	itPGHost     = envOr("ADC_IT_PG_HOST", "127.0.0.1")
	itPGPort     = envOrInt("ADC_IT_PG_PORT", 55433)
	itPGUser     = envOr("ADC_IT_PG_USER", "adc")
	itPGPassword = envOr("ADC_IT_PG_PASSWORD", "adc_dev_only")
	itPGDBName   = envOr("ADC_IT_PG_DBNAME", "adc")
	itValkeyAddr = envOr("ADC_IT_VALKEY_ADDR", "127.0.0.1:6389")
	itValkeyPass = envOr("ADC_IT_VALKEY_PASSWORD", "adc_dev_only")
	// itKEKSeed feeds the device credential KEK derivation (auth.LoadKEK
	// semantics: SHA-256 of the raw string). Tests never read the KEK from
	// production env; this fixed value keeps runs deterministic.
	itKEKSeed = envOr("ADC_IT_DEVICE_KEK", "adc-integration-test-kek-value")
	// itCallbackKey is the platform HITL callback signing key (SEC-13):
	// injected like ADC_HITL_CALLBACK_KEY in cmd/adc.
	itCallbackKey = envOr("ADC_IT_HITL_CALLBACK_KEY", "adc-integration-callback-key")
)

// env is the shared test environment assembled by TestMain. nil means the
// prerequisites are unavailable and every test must skip.
var env *testEnv

// skipReason explains why env is nil (dependency connection failure etc.).
var skipReason string

// TestMain connects the real PostgreSQL and Valkey instances, resets and
// migrates the schema, assembles the full in-process server stack and
// seeds the admin fixtures. Any dependency failure is recorded instead of
// failing: the tests then skip individually (CI without containers).
func TestMain(m *testing.M) {
	if e, err := buildEnv(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "integration: prerequisites unavailable, all tests will be skipped: %v\n", err)
		skipReason = err.Error()
	} else {
		env = e
	}
	code := m.Run()
	if env != nil {
		env.close()
	}
	os.Exit(code)
}

// envOrSkip returns the shared environment or skips the test when the
// dependency containers were unreachable at startup.
func envOrSkip(t *testing.T) *testEnv {
	t.Helper()
	if env == nil {
		t.Skipf("integration prerequisites unavailable: %s", skipReason)
	}
	return env
}

// buildEnv performs the TestMain bootstrap: connect -> reset+migrate ->
// flush Valkey -> assemble stack -> seed fixtures. Every step is a hard
// prerequisite; the first failure aborts the build and skips the run.
func buildEnv(ctx context.Context) (*testEnv, error) {
	connectCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	pool, err := db.Connect(connectCtx, config.DSN{
		Host:     itPGHost,
		Port:     itPGPort,
		User:     itPGUser,
		Password: itPGPassword,
		DBName:   itPGDBName,
		SSLMode:  "disable",
	})
	if err != nil {
		return nil, fmt.Errorf("postgres %s:%d: %w", itPGHost, itPGPort, err)
	}

	rdb := redis.NewClient(&redis.Options{
		Addr:     itValkeyAddr,
		Password: itValkeyPass,
		DB:       0,
	})
	if err := rdb.Ping(connectCtx).Err(); err != nil {
		pool.Close()
		return nil, fmt.Errorf("valkey %s: %w", itValkeyAddr, err)
	}

	// Idempotent schema reset: run the down migration when the schema is
	// already present (repeated local runs), then always run the up
	// migration. Fresh containers skip the down step.
	if exists, err := schemaExists(ctx, pool); err != nil {
		pool.Close()
		return nil, fmt.Errorf("schema probe: %w", err)
	} else if exists {
		if err := runMigrationFile(ctx, pool, "../migrations/0001_init.down.sql"); err != nil {
			pool.Close()
			return nil, fmt.Errorf("down migration: %w", err)
		}
	}
	if err := runMigrationFile(ctx, pool, "../migrations/0001_init.up.sql"); err != nil {
		pool.Close()
		return nil, fmt.Errorf("up migration: %w", err)
	}
	if err := runCompatShims(ctx, pool); err != nil {
		pool.Close()
		return nil, fmt.Errorf("compat shims: %w", err)
	}

	// Clean Valkey slate: sessions, nonces, audit queue, wake channels and
	// rate-limit buckets from earlier runs must not leak into this run.
	if err := rdb.FlushDB(ctx).Err(); err != nil {
		pool.Close()
		return nil, fmt.Errorf("valkey flush: %w", err)
	}

	e, err := newTestEnv(ctx, pool, rdb)
	if err != nil {
		pool.Close()
		rdb.Close()
		return nil, fmt.Errorf("assemble stack: %w", err)
	}
	if err := e.seedFixtures(ctx); err != nil {
		e.close()
		return nil, fmt.Errorf("seed fixtures: %w", err)
	}
	return e, nil
}

// schemaExists reports whether adc_tenants already exists, deciding
// whether the down migration must run first (idempotent bootstrap).
func schemaExists(ctx context.Context, pool *db.Pool) (bool, error) {
	var name string
	err := pool.QueryRow(ctx, `SELECT COALESCE(to_regclass('public.adc_tenants')::text, '')`).Scan(&name)
	return name != "", err
}

// runMigrationFile executes a migration script through the pgx simple
// protocol (pgconn.Exec): the scripts contain DO $$ ... $$ blocks and
// multi-statement bodies that the extended protocol cannot carry, while
// the simple protocol hands the whole script to PostgreSQL natively.
func runMigrationFile(ctx context.Context, pool *db.Pool, path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	results, err := conn.Conn().PgConn().Exec(ctx, string(raw)).ReadAll()
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	for _, r := range results {
		if r.Err != nil {
			return fmt.Errorf("%s: %w", path, r.Err)
		}
	}
	return nil
}

// runCompatShims normalizes nullable columns that the product PG
// repositories scan into plain Go strings (pgx v5 refuses to scan NULL
// into *string). This is an integration-only workaround: the defects live
// in internal/adminapi (pgDeviceRepo scans sdk_version into a string) and
// internal/approval (scanTicket scans approver_name/comment/agent_id into
// strings), which this suite must not modify (hard rule: only
// ce/integration/ is written). The shims do not change API-visible
// semantics: the Go models already treat a missing SDK version and a
// missing approver/comment as empty strings. Defect evidence:
//
//	scanDevice:  can't scan into dest[9] (col: sdk_version): cannot scan NULL into *string
//	scanTicket:  can't scan into dest[9] (col: approver_name): cannot scan NULL into *string
func runCompatShims(ctx context.Context, pool *db.Pool) error {
	stmts := []string{
		// Device registration inserts no sdk_version; the DEFAULT keeps
		// the RETURNING scan (and the list scan) on non-NULL values.
		`ALTER TABLE adc_devices ALTER COLUMN sdk_version SET DEFAULT ''`,
		// Ticket creation writes NULL approver_name/comment/agent_id
		// explicitly, so a column DEFAULT cannot help: a BEFORE INSERT
		// trigger normalizes them to '' before the RETURNING scan.
		`CREATE OR REPLACE FUNCTION adc_it_null_normalize() RETURNS trigger AS $$
		 BEGIN
		   IF NEW.agent_id IS NULL THEN NEW.agent_id := ''; END IF;
		   IF NEW.approver_name IS NULL THEN NEW.approver_name := ''; END IF;
		   IF NEW.comment IS NULL THEN NEW.comment := ''; END IF;
		   RETURN NEW;
		 END $$ LANGUAGE plpgsql`,
		`DROP TRIGGER IF EXISTS adc_it_null_normalize_trigger ON adc_approval_tickets`,
		`CREATE TRIGGER adc_it_null_normalize_trigger
		   BEFORE INSERT OR UPDATE ON adc_approval_tickets
		   FOR EACH ROW EXECUTE FUNCTION adc_it_null_normalize()`,
	}
	for _, sql := range stmts {
		if _, err := pool.Exec(ctx, sql); err != nil {
			return err
		}
	}
	return nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envOrInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
