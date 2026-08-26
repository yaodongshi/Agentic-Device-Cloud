package migrations

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5"

	"adc.dev/ce/internal/adminapi"

	"adc.dev/ce/internal/adminauth"
)

type integrationSessions struct {
	mu       sync.Mutex
	sessions map[string]*adminauth.Session
}

func (s *integrationSessions) Create(_ context.Context, session *adminauth.Session, _ time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	copy := *session
	copy.Roles = append([]string(nil), session.Roles...)
	s.sessions[session.TokenHash] = &copy
	return nil
}

func (s *integrationSessions) Get(_ context.Context, tokenHash string) (*adminauth.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	session := s.sessions[tokenHash]
	if session == nil {
		return nil, adminauth.ErrSessionNotFound
	}
	copy := *session
	copy.Roles = append([]string(nil), session.Roles...)
	return &copy, nil
}

func (s *integrationSessions) Delete(_ context.Context, tokenHash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, tokenHash)
	return nil
}

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

func TestPostgresA2AHumanDecisionCAS(t *testing.T) {
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
	schema := fmt.Sprintf("adc_a2a_decision_%d", time.Now().UnixNano())
	admin, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
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
	if err := Run(ctx, conn); err != nil {
		t.Fatal(err)
	}

	var tenantID, userID, applicationID, taskID string
	if err := conn.QueryRow(ctx, `INSERT INTO adc_tenants(code,name) VALUES('a2a-decision','A2A Decision') RETURNING id`).Scan(&tenantID); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow(ctx, `INSERT INTO adc_users(tenant_id,username,display_name) VALUES($1,'approver','Alice Approver') RETURNING id`, tenantID).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow(ctx, `INSERT INTO adc_developer_applications(tenant_id,name,purpose) VALUES($1,'decision-app','test') RETURNING id`, tenantID).Scan(&applicationID); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow(ctx, `INSERT INTO adc_a2a_tasks(tenant_id,application_id,idempotency_key,request_hash,state,task_type,goal)
		VALUES($1,$2,'decision-key',repeat('a',64),'input-required','maintain','replace spindle') RETURNING task_id`, tenantID, applicationID).Scan(&taskID); err != nil {
		t.Fatal(err)
	}
	// pgx.Conn is intentionally not concurrency-safe. Use separate database
	// sessions to exercise the same production CAS path as two app instances.
	decisionConns := make([]*pgx.Conn, 2)
	for i := range decisionConns {
		decisionConns[i], err = pgx.ConnectConfig(ctx, cfg)
		if err != nil {
			t.Fatal(err)
		}
		defer decisionConns[i].Close(context.Background())
	}
	type outcome struct {
		result *adminapi.A2ATaskDecision
		err    error
	}
	outcomes := make(chan outcome, 2)
	for i, reason := range []string{"first", "second"} {
		go func(repo adminapi.A2ATaskDecisionRepo) {
			result, err := repo.Decide(ctx, tenantID, taskID, 1, "approve", userID, reason)
			outcomes <- outcome{result: result, err: err}
		}(adminapi.NewPGA2ATaskDecisionRepo(decisionConns[i]))
	}
	successes, conflicts := 0, 0
	for range 2 {
		outcome := <-outcomes
		switch {
		case outcome.err == nil:
			successes++
			if outcome.result.State != "working" || outcome.result.Version != 2 || outcome.result.ApproverUserID != userID || outcome.result.ApproverIdentity != "Alice Approver" {
				t.Fatalf("decision=%+v", outcome.result)
			}
		case errors.Is(outcome.err, adminapi.ErrA2ATaskConflict):
			conflicts++
		default:
			t.Fatalf("unexpected decision error: %v", outcome.err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("successes=%d conflicts=%d, want 1/1", successes, conflicts)
	}

	var otherTenantID string
	if err := conn.QueryRow(ctx, `INSERT INTO adc_tenants(code,name) VALUES('a2a-other','A2A Other') RETURNING id`).Scan(&otherTenantID); err != nil {
		t.Fatal(err)
	}
	if _, err := adminapi.NewPGA2ATaskDecisionRepo(conn).Decide(ctx, otherTenantID, taskID, 2, "reject", userID, "cross tenant"); !errors.Is(err, adminapi.ErrA2ATaskNotFound) {
		t.Fatalf("cross-tenant decision error=%v, want task not found", err)
	}
	var state string
	var version int64
	if err := conn.QueryRow(ctx, `SELECT state, version FROM adc_a2a_tasks WHERE tenant_id=$1 AND task_id=$2`, tenantID, taskID).Scan(&state, &version); err != nil {
		t.Fatal(err)
	}
	if state != "working" || version != 2 {
		t.Fatalf("task after cross-tenant decision state=%s version=%d, want working/2", state, version)
	}
	if _, err := conn.Exec(ctx, `DELETE FROM adc_developer_applications WHERE id=$1::uuid`, applicationID); err != nil {
		t.Fatalf("delete application with historical task: %v", err)
	}
	var retained bool
	if err := conn.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM adc_a2a_tasks WHERE task_id=$1::uuid)`, taskID).Scan(&retained); err != nil || !retained {
		t.Fatalf("historical task retained=%v err=%v", retained, err)
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
	loaded, err := Load(embedded)
	if err != nil {
		t.Fatal(err)
	}
	want := len(loaded)
	if count != want || maximum != want || legacy != 4 {
		t.Fatalf("count=%d max=%d legacy=%d, want %d/%d/4", count, maximum, legacy, want, want)
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

func TestPostgresAuthzVersionTriggersAreTransactional(t *testing.T) {
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
	schema := fmt.Sprintf("adc_authz_trigger_%d", time.Now().UnixNano())
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
	if err := Run(ctx, conn); err != nil {
		t.Fatal(err)
	}

	var tenantID, userID, adminRoleID, auditorRoleID string
	if err := conn.QueryRow(ctx, `INSERT INTO adc_tenants (code, name) VALUES ('authz-trigger', 'Authz Trigger') RETURNING id`).Scan(&tenantID); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow(ctx, `INSERT INTO adc_users (tenant_id, username) VALUES ($1, 'authz-user') RETURNING id`, tenantID).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow(ctx, `INSERT INTO adc_roles (tenant_id, role_code, scope) VALUES ($1, 'TENANT_ADMIN', 'TENANT') RETURNING id`, tenantID).Scan(&adminRoleID); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow(ctx, `INSERT INTO adc_roles (tenant_id, role_code, scope) VALUES ($1, 'AUDITOR', 'TENANT') RETURNING id`, tenantID).Scan(&auditorRoleID); err != nil {
		t.Fatal(err)
	}
	assertVersion := func(want int64) {
		t.Helper()
		var got int64
		if err := conn.QueryRow(ctx, `SELECT authz_version FROM adc_users WHERE id = $1`, userID).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("authz_version=%d want=%d", got, want)
		}
	}
	assertVersion(1)
	var bindingID string
	if err := conn.QueryRow(ctx, `INSERT INTO adc_user_roles (user_id, role_id, tenant_id) VALUES ($1, $2, $3) RETURNING id`, userID, adminRoleID, tenantID).Scan(&bindingID); err != nil {
		t.Fatal(err)
	}
	assertVersion(2)
	if _, err := conn.Exec(ctx, `UPDATE adc_user_roles SET expires_at = now() + interval '1 hour' WHERE id = $1`, bindingID); err != nil {
		t.Fatal(err)
	}
	assertVersion(3)
	if _, err := conn.Exec(ctx, `DELETE FROM adc_user_roles WHERE id = $1`, bindingID); err != nil {
		t.Fatal(err)
	}
	assertVersion(4)
	if _, err := conn.Exec(ctx, `UPDATE adc_users SET status = 'DISABLED' WHERE id = $1`, userID); err != nil {
		t.Fatal(err)
	}
	assertVersion(5)
	if _, err := conn.Exec(ctx, `UPDATE adc_users SET status = 'DISABLED' WHERE id = $1`, userID); err != nil {
		t.Fatal(err)
	}
	assertVersion(5)

	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO adc_user_roles (user_id, role_id, tenant_id) VALUES ($1, $2, $3)`, userID, auditorRoleID, tenantID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	assertVersion(5)

	if _, err := conn.Exec(ctx, `UPDATE adc_users SET status = 'ACTIVE' WHERE id = $1`, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `INSERT INTO adc_user_roles (user_id, role_id, tenant_id, expires_at) VALUES
		($1, $2, $3, now() - interval '1 second'), ($1, $4, $3, now() + interval '1 hour')`, userID, adminRoleID, tenantID, auditorRoleID); err != nil {
		t.Fatal(err)
	}
	var roles []string
	if err := conn.QueryRow(ctx, `SELECT array_agg(r.role_code ORDER BY r.role_code)
		FROM adc_user_roles ur JOIN adc_roles r ON r.id = ur.role_id
		WHERE ur.user_id = $1 AND (ur.expires_at IS NULL OR ur.expires_at > now())`, userID).Scan(&roles); err != nil {
		t.Fatal(err)
	}
	if len(roles) != 1 || roles[0] != "AUDITOR" {
		t.Fatalf("effective roles=%v want [AUDITOR]", roles)
	}
}

func TestPostgresImmediateSessionRevocation(t *testing.T) {
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
	schema := fmt.Sprintf("adc_authz_request_%d", time.Now().UnixNano())
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
	if err := Run(ctx, conn); err != nil {
		t.Fatal(err)
	}

	var tenantID, userID, roleID string
	if err := conn.QueryRow(ctx, `INSERT INTO adc_tenants (code, name) VALUES ('authz-request', 'Authz Request') RETURNING id`).Scan(&tenantID); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow(ctx, `INSERT INTO adc_users (tenant_id, username) VALUES ($1, 'request-user') RETURNING id`, tenantID).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow(ctx, `INSERT INTO adc_roles (tenant_id, role_code, scope) VALUES ($1, 'TENANT_ADMIN', 'TENANT') RETURNING id`, tenantID).Scan(&roleID); err != nil {
		t.Fatal(err)
	}
	var bindingID string
	if err := conn.QueryRow(ctx, `INSERT INTO adc_user_roles (user_id, role_id, tenant_id) VALUES ($1, $2, $3) RETURNING id`, userID, roleID, tenantID).Scan(&bindingID); err != nil {
		t.Fatal(err)
	}
	store := adminauth.NewPGUserStore(conn)

	issue := func(token string) (*integrationSessions, http.Handler) {
		t.Helper()
		var version int64
		if err := conn.QueryRow(ctx, `SELECT authz_version FROM adc_users WHERE id = $1`, userID).Scan(&version); err != nil {
			t.Fatal(err)
		}
		sessions := &integrationSessions{sessions: map[string]*adminauth.Session{}}
		hash := adminauth.HashToken(token)
		sessions.sessions[hash] = &adminauth.Session{TokenHash: hash, UserID: userID, TenantID: tenantID, AuthzVersion: version, Roles: []string{"tenant_admin"}, ExpiresAt: time.Now().Add(time.Hour)}
		handler := adminauth.Authorize(sessions, store)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
		return sessions, handler
	}
	request := func(token string, handler http.Handler) int {
		recorder := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v1/admin/tenants", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		handler.ServeHTTP(recorder, req)
		return recorder.Code
	}

	_, handler := issue("valid")
	if got := request("valid", handler); got != http.StatusOK {
		t.Fatalf("valid request status=%d", got)
	}

	sessions, handler := issue("disabled")
	if _, err := conn.Exec(ctx, `UPDATE adc_users SET status = 'DISABLED' WHERE id = $1`, userID); err != nil {
		t.Fatal(err)
	}
	if got := request("disabled", handler); got != http.StatusUnauthorized {
		t.Fatalf("disabled request status=%d", got)
	}
	if _, err := sessions.Get(ctx, adminauth.HashToken("disabled")); err != adminauth.ErrSessionNotFound {
		t.Fatalf("disabled session was not deleted: %v", err)
	}
	if _, err := conn.Exec(ctx, `UPDATE adc_users SET status = 'ACTIVE' WHERE id = $1`, userID); err != nil {
		t.Fatal(err)
	}

	_, handler = issue("revoked")
	if _, err := conn.Exec(ctx, `DELETE FROM adc_user_roles WHERE id = $1`, bindingID); err != nil {
		t.Fatal(err)
	}
	if got := request("revoked", handler); got != http.StatusUnauthorized {
		t.Fatalf("revoked role request status=%d", got)
	}

	if err := conn.QueryRow(ctx, `INSERT INTO adc_user_roles (user_id, role_id, tenant_id, expires_at)
		VALUES ($1, $2, $3, now() + interval '200 milliseconds') RETURNING id`, userID, roleID, tenantID).Scan(&bindingID); err != nil {
		t.Fatal(err)
	}
	_, handler = issue("expired")
	time.Sleep(300 * time.Millisecond)
	if got := request("expired", handler); got != http.StatusUnauthorized {
		t.Fatalf("naturally expired role request status=%d", got)
	}

	if _, err := conn.Exec(ctx, `UPDATE adc_user_roles SET expires_at = NULL WHERE id = $1`, bindingID); err != nil {
		t.Fatal(err)
	}
	_, handler = issue("suspended")
	if _, err := conn.Exec(ctx, `UPDATE adc_tenants SET status = 'SUSPENDED' WHERE id = $1`, tenantID); err != nil {
		t.Fatal(err)
	}
	if got := request("suspended", handler); got != http.StatusUnauthorized {
		t.Fatalf("suspended tenant request status=%d", got)
	}
}
