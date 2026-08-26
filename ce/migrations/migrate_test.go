package migrations

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestLoadOrdersAndValidatesVersions(t *testing.T) {
	t.Parallel()
	migrations, err := Load(fstest.MapFS{
		"0002_second.up.sql": {Data: []byte("SELECT 2")},
		"0001_first.up.sql":  {Data: []byte("SELECT 1")},
		"notes.txt":          {Data: []byte("ignored")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint([]int64{migrations[0].Version, migrations[1].Version}); got != "[1 2]" {
		t.Fatalf("versions = %s", got)
	}
	if _, err := Load(fstest.MapFS{"0002_gap.up.sql": {Data: []byte("SELECT 2")}}); err == nil {
		t.Fatal("expected a version gap to fail")
	}
}

func TestEmbeddedDeveloperApplicationMigrationContract(t *testing.T) {
	t.Parallel()
	body, err := fs.ReadFile(embedded, "0006_developer_applications.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	sql := string(body)
	for _, required := range []string{
		"CREATE TABLE adc_developer_applications",
		"CREATE TABLE adc_developer_application_credentials",
		"secret_hash  CHAR(64) NOT NULL",
		"uq_developer_applications_tenant_name",
		"ON adc_developer_applications (tenant_id, lower(name))",
	} {
		if !strings.Contains(sql, required) {
			t.Errorf("0006 migration missing %q", required)
		}
	}
	if strings.Contains(sql, " secret ") || strings.Contains(sql, "secret TEXT") {
		t.Fatal("0006 migration must not persist plaintext secret")
	}
}

func TestEmbeddedA2ATaskMigrationContract(t *testing.T) {
	t.Parallel()
	body, err := fs.ReadFile(embedded, "0008_a2a_tasks.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	sql := string(body)
	for _, required := range []string{
		"CREATE TABLE adc_a2a_tasks",
		"REFERENCES adc_tenants(id)",
		"FOREIGN KEY (application_id, tenant_id)",
		"REFERENCES adc_developer_applications (id, tenant_id)",
		"uq_a2a_tasks_tenant_application_idempotency",
		"UNIQUE (tenant_id, application_id, idempotency_key)",
		"request_hash        CHAR(64) NOT NULL",
		"version             BIGINT NOT NULL DEFAULT 1",
		"'submitted','working','input-required','completed','failed','rejected'",
		"idx_a2a_tasks_tenant_created",
	} {
		if !strings.Contains(sql, required) {
			t.Errorf("0008 migration missing %q", required)
		}
	}
	down, err := fs.ReadFile(embedded, "0008_a2a_tasks.down.sql")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(down), "DROP TABLE IF EXISTS adc_a2a_tasks") {
		t.Fatal("0008 down migration must drop adc_a2a_tasks")
	}
}

func TestEmbeddedA2AApplicationSnapshotMigrationContract(t *testing.T) {
	t.Parallel()
	body, err := fs.ReadFile(embedded, "0009_a2a_application_snapshot.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	sql := string(body)
	for _, required := range []string{
		"DROP CONSTRAINT IF EXISTS fk_a2a_tasks_application_tenant",
		"DROP CONSTRAINT IF EXISTS uq_developer_applications_id_tenant",
		"CREATE FUNCTION adc_validate_a2a_application_tenant()",
		"CREATE TRIGGER trg_a2a_application_tenant",
	} {
		if !strings.Contains(sql, required) {
			t.Errorf("0009 migration missing %q", required)
		}
	}
}

func TestRunAlreadyCurrentAndVersionTooHigh(t *testing.T) {
	t.Parallel()
	files := testMigrations(2)
	current := newFakeState(2)
	if err := run(context.Background(), &fakeConn{state: current}, files); err != nil {
		t.Fatal(err)
	}
	if current.begins != 0 {
		t.Fatalf("already-current run began %d transactions", current.begins)
	}

	tooHigh := newFakeState(3)
	err := run(context.Background(), &fakeConn{state: tooHigh}, files)
	if err == nil || !strings.Contains(err.Error(), "newer than supported") {
		t.Fatalf("expected newer-version error, got %v", err)
	}
}

func TestRunRejectsVersionLedgerGap(t *testing.T) {
	t.Parallel()
	state := newFakeState(3)
	state.applied = 2
	err := run(context.Background(), &fakeConn{state: state}, testMigrations(3))
	if err == nil || !strings.Contains(err.Error(), "not contiguous") {
		t.Fatalf("expected ledger gap error, got %v", err)
	}
	if state.baselineWrites != 0 || state.baselineQueries != 0 {
		t.Fatalf("ledger gap attempted baseline: queries=%d writes=%d", state.baselineQueries, state.baselineWrites)
	}
}

func TestRunRejectsLedgerThatDoesNotStartAtOne(t *testing.T) {
	t.Parallel()
	state := newFakeState(2)
	state.minimum = 0
	err := run(context.Background(), &fakeConn{state: state}, testMigrations(2))
	if err == nil || !strings.Contains(err.Error(), "must start at 1") {
		t.Fatalf("expected invalid ledger start error, got %v", err)
	}
	if state.baselineQueries != 0 || state.baselineWrites != 0 {
		t.Fatalf("invalid ledger attempted baseline: queries=%d writes=%d", state.baselineQueries, state.baselineWrites)
	}
}

func TestRunRefusesPartialLegacyVersions(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name       string
		incomplete string
	}{
		{name: "partial 0001", incomplete: "0001 missing [table adc_users, index uq_devices_code]"},
		{name: "partial 0004", incomplete: "0004 missing [table adc_tool_package_installs, index adc_tool_package_installs_pkey]"},
	} {
		t.Run(test.name, func(t *testing.T) {
			state := newFakeState(0)
			state.legacyIncomplete = test.incomplete
			err := run(context.Background(), &fakeConn{state: state}, testMigrations(6))
			if err == nil || !strings.Contains(err.Error(), test.incomplete) {
				t.Fatalf("expected diagnostic partial-schema error, got %v", err)
			}
			if state.baselineWrites != 0 || state.begins != 0 {
				t.Fatalf("partial schema was modified: baseline writes=%d begins=%d", state.baselineWrites, state.begins)
			}
		})
	}
}

func TestRunBaselinesCompleteLegacyVersionsOneThroughFour(t *testing.T) {
	t.Parallel()
	state := newFakeState(0)
	state.legacyVersions = 4
	if err := run(context.Background(), &fakeConn{state: state}, testMigrations(6)); err != nil {
		t.Fatal(err)
	}
	if state.baselineWrites != 4 || state.current != 6 || state.applied != 6 || state.commits != 2 {
		t.Fatalf("baseline writes=%d current=%d applied=%d commits=%d, want 4/6/6/2", state.baselineWrites, state.current, state.applied, state.commits)
	}
}

func TestRunFailureDoesNotRecordVersion(t *testing.T) {
	t.Parallel()
	state := newFakeState(0)
	err := run(context.Background(), &fakeConn{state: state}, fstest.MapFS{
		"0001_ok.up.sql":   {Data: []byte("SELECT 1")},
		"0002_fail.up.sql": {Data: []byte("FAIL")},
	})
	if err == nil || !strings.Contains(err.Error(), "0002_fail") {
		t.Fatalf("expected migration failure, got %v", err)
	}
	if state.current != 1 {
		t.Fatalf("current version = %d, want 1", state.current)
	}
}

func TestRunConcurrentUsesAdvisoryLock(t *testing.T) {
	t.Parallel()
	state := newFakeState(0)
	files := testMigrations(3)
	start := make(chan struct{})
	errs := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			errs <- run(context.Background(), &fakeConn{state: state}, files)
		}()
	}
	close(start)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if state.current != 3 || state.commits != 3 {
		t.Fatalf("current=%d commits=%d, want exactly three committed migrations", state.current, state.commits)
	}
}

func testMigrations(count int) fs.FS {
	files := fstest.MapFS{}
	for version := 1; version <= count; version++ {
		files[fmt.Sprintf("%04d_test.up.sql", version)] = &fstest.MapFile{Data: []byte("SELECT 1")}
	}
	return files
}

type fakeState struct {
	lock                sync.Mutex
	current             int64
	applied             int64
	minimum             int64
	begins              int
	commits             int
	baselineQueries     int
	baselineWrites      int
	legacyVersions      int
	legacyIncomplete    string
	legacyNonContiguous bool
}

func newFakeState(current int64) *fakeState {
	minimum := int64(0)
	if current > 0 {
		minimum = 1
	}
	return &fakeState{current: current, applied: current, minimum: minimum}
}

type fakeConn struct{ state *fakeState }

func (c *fakeConn) Exec(_ context.Context, query string, args ...any) (pgconn.CommandTag, error) {
	switch {
	case strings.Contains(query, "pg_advisory_lock("):
		c.state.lock.Lock()
	case strings.Contains(query, "pg_advisory_unlock("):
		c.state.lock.Unlock()
	case strings.Contains(query, "legacy_baseline"):
		versions := args[0].(int)
		c.state.current = int64(versions)
		c.state.applied += int64(versions)
		c.state.minimum = 1
		c.state.baselineWrites += versions
	}
	return pgconn.NewCommandTag("SELECT 1"), nil
}

func (c *fakeConn) QueryRow(_ context.Context, query string, _ ...any) row {
	if strings.Contains(query, "WITH markers") {
		c.state.baselineQueries++
		return fakeRow{values: []any{c.state.legacyVersions, c.state.legacyIncomplete, c.state.legacyNonContiguous}}
	}
	if strings.Contains(query, "SELECT COALESCE(MAX(version), 0), COUNT(*)") {
		return fakeRow{values: []any{c.state.current, c.state.applied, c.state.minimum}}
	}
	return fakeRow{values: []any{c.state.current}}
}

func (c *fakeConn) Begin(context.Context) (tx, error) {
	c.state.begins++
	return &fakeTx{state: c.state}, nil
}

type fakeRow struct {
	values []any
	err    error
}

func (r fakeRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) != len(r.values) {
		return fmt.Errorf("scan targets=%d values=%d", len(dest), len(r.values))
	}
	for i, target := range dest {
		switch typed := target.(type) {
		case *int:
			*typed = r.values[i].(int)
		case *int64:
			*typed = r.values[i].(int64)
		case *string:
			*typed = r.values[i].(string)
		case *bool:
			*typed = r.values[i].(bool)
		default:
			return errors.New("unexpected scan target")
		}
	}
	return nil
}

type fakeTx struct {
	state   *fakeState
	version int64
	failed  bool
}

func (t *fakeTx) Exec(_ context.Context, query string, args ...any) (pgconn.CommandTag, error) {
	if query == "FAIL" {
		t.failed = true
		return pgconn.CommandTag{}, errors.New("intentional failure")
	}
	if strings.Contains(query, "INSERT INTO adc_schema_migrations") {
		t.version = args[0].(int64)
	}
	return pgconn.NewCommandTag("OK"), nil
}

func (t *fakeTx) Commit(context.Context) error {
	if t.failed {
		return errors.New("commit after failure")
	}
	t.state.current = t.version
	t.state.applied++
	if t.state.minimum == 0 {
		t.state.minimum = t.version
	}
	t.state.commits++
	return nil
}

func (t *fakeTx) Rollback(context.Context) error { return nil }
