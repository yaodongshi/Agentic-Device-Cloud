// testEnv assembly (design/80 B-12). cmd/adc lives in package main and
// cannot be imported, so this file replicates its minimal wiring inside
// the integration package. The replication is deliberate and kept close
// to cmd/adc/main.go: same repositories, same middleware order
// (rate limit -> auth -> quota -> handler), same HITL production wiring
// (PG ticket repo + Valkey wake bus + signed callback handler). The only
// deliberate simplifications, commented at each site, are: no cluster
// routing (single node), no card notifiers (no external webhooks in
// integration, design/40 3.4), no observability wrappers.
package integration

import (
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"adc.dev/ce/internal/adminapi"
	"adc.dev/ce/internal/adminauth"
	"adc.dev/ce/internal/agentapi"
	"adc.dev/ce/internal/agentauth"
	"adc.dev/ce/internal/approval"
	"adc.dev/ce/internal/auth"
	"adc.dev/ce/internal/connector"
	"adc.dev/ce/internal/db"
	"adc.dev/ce/pkg/audit"
	"adc.dev/ce/pkg/metering"
	"adc.dev/ce/pkg/ratelimit"
)

// testEnv bundles the live dependencies and the assembled handlers for the
// whole suite. One env is shared per test binary (TestMain); individual
// tests keep themselves independent through unique resource codes.
type testEnv struct {
	pool *db.Pool
	rdb  *redis.Client
	kek  []byte

	srv *httptest.Server // full stack: admin + agent + tunnel + hitl
	hub *connector.DeviceHub

	callbackKey string
	ticketRepo  *approval.PGTicketRepo
	cancel      context.CancelFunc
}

// newTestEnv assembles the in-process stack. Wiring mirrors
// cmd/adc/main.go (see the package comment for the deltas).
func newTestEnv(ctx context.Context, pool *db.Pool, rdb *redis.Client) (*testEnv, error) {
	ctx, cancel := context.WithCancel(ctx)
	log := slog.New(slog.NewTextHandler(io.Discard, nil)) // keep test output clean

	// --- device auth (SEC-03), same as cmd/adc ---
	kekSum := sha256.Sum256([]byte(itKEKSeed))
	kek := kekSum[:]
	credRepo := auth.NewCredentialRepo(pool.Pool, kek)
	nonces := auth.NewValkeyNonceStore(&redis.Options{Addr: itValkeyAddr, Password: itValkeyPass, DB: 0})
	verifier := auth.NewVerifier(credRepo, nonces)

	// --- connector (WSS tunnel, SEC-04/14/15), same as cmd/adc ---
	hub := connector.NewDeviceHub()
	registry := connector.NewValkeyRegistry(rdb, 90*time.Second, log)
	tunnel := connector.NewTunnelServer(connector.TunnelServerConfig{
		Auth:      deviceAuthAdapter{v: verifier},
		Hub:       hub,
		Registry:  registry,
		NodeID:    "integration-node",
		DeviceTTL: 90 * time.Second,
		Logger:    log,
	})

	// --- agent api (SEC-02/09), same as cmd/adc; cluster routing omitted
	// (single node in integration, LocalRouter is the fast path) ---
	policy := agentapi.DBPolicy{Lookup: riskLookup(pool)}
	agg := agentapi.DBAggregator{Rows: toolRows(pool), Lookup: riskLookup(pool), UUIDLookup: deviceUUIDLookup(pool), OwnerCheck: ownerCheck(pool)}
	localRouter := agentapi.LocalRouter{Session: func(ctx context.Context, tenantID, deviceID string) (agentapi.RPCClient, error) {
		sess, err := hub.GetDevice(tenantID, deviceID)
		if err != nil {
			return nil, err
		}
		return sess, nil
	}}
	meter := metering.NewValkeyMeter(rdb)
	routerWithMeter := agentapi.MeteringRouter{Next: localRouter, Meter: meter}
	go metering.NewValkeyWorker(rdb, pool.Pool).Run(ctx)

	// --- hitl (production wiring, design/80 B-07); Notify nil because
	// webhook channels are out of scope for backend integration
	// (design/40 3.4 mock-webhook belongs to the E2E tier) ---
	ticketRepo := approval.NewPGTicketRepo(pool.Pool)
	wakeBus := approval.NewValkeyEventBus(rdb)
	hitl := agentapi.NewApprovalHITLClient(agentapi.ApprovalHITLConfig{
		Repo:        ticketRepo,
		Bus:         wakeBus,
		Notify:      nil,
		BaseURL:     "http://integration.local",
		CallbackKey: itCallbackKey,
		TicketTTL:   approval.DefaultExpiry,
		UUIDLookup:  deviceUUIDLookup(pool),
	})

	agentSrv := agentapi.NewServer(
		agentauth.NewValidator(agentauth.NewPGKeyStore(pool.Pool)),
		agg, routerWithMeter, policy, hitl)
	agentSrv.Audit = audit.NewValkeySink(rdb)

	// --- audit pipeline (SEC-07), same as cmd/adc ---
	go audit.NewValkeyWorker(rdb, pool.Pool).Run(ctx)

	callbackHandler := approval.NewCallbackHandler(ticketRepo, wakeBus, itCallbackKey)

	// --- agent api quota + rate limit chain (design/80 B-10, SEC-12);
	// order: rate limit (429) -> auth (401, inside Routes) -> quota
	// (403 13003), per design/31 3.2.3 ---
	quotaGate := agentapi.PGQuotaGate{Lookup: quotaLookup(pool)}
	limiter, err := ratelimit.NewTokenBucket(ratelimit.NewValkeyStore(rdb), nil)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("rate limiter: %w", err)
	}
	agentMux := http.NewServeMux()
	agentSrv.Routes(agentMux)
	agentChain := ratelimit.Middleware(limiter,
		principalKey(ratelimit.ScopeTenant),
		principalKey(ratelimit.ScopeAgent),
	)(agentapi.QuotaMiddleware(quotaGate)(agentMux))

	// --- admin api + session auth (design/80 B-01/B-02/B-03/B-05/B-06) ---
	sessions := adminauth.NewValkeySessionStore(rdb)
	users := adminauth.NewPGUserStore(pool.Pool)
	authHandler := adminauth.NewHandler(users, sessions)
	adminSrv := adminapi.NewServer(
		adminapi.NewPGTenantRepo(pool.Pool),
		adminapi.NewPGDeviceRepo(pool.Pool),
		adminapi.NewPGApiKeyRepo(pool.Pool),
		sessions)
	adminSrv.Authorization = users
	adminSrv.KEK = kek
	adminSrv.AuditQuery = newPGAuditQueryRepo(pool.Pool)
	adminSrv.Tools = &pgToolRepo{pool: pool.Pool}
	adminHandler := adminSrv.Handler()

	mux := http.NewServeMux()
	mux.Handle("/v1/admin/", combinedAdminHandler(authHandler.Routes(), adminHandler))
	mux.Handle("/v1/agent/mcp/", agentChain)
	callbackHandler.Routes(mux)
	mux.Handle("GET /v1/devices/tunnel", tunnel)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"status":"ok"}`)
	})

	srv := httptest.NewServer(mux)

	e := &testEnv{
		pool:        pool,
		rdb:         rdb,
		kek:         kek,
		srv:         srv,
		hub:         hub,
		callbackKey: itCallbackKey,
		ticketRepo:  ticketRepo,
		cancel:      cancel,
	}
	return e, nil
}

// close tears down the assembled stack and the background workers.
func (e *testEnv) close() {
	e.srv.Close()
	e.hub.CloseAll()
	e.cancel()
	e.rdb.Close()
	e.pool.Close()
}

// wsURL converts the httptest HTTP origin to its websocket counterpart
// (same trick as connector/tunnel_test.go).
func (e *testEnv) wsURL() string {
	return "ws" + strings.TrimPrefix(e.srv.URL, "http") + "/v1/devices/tunnel"
}

// combinedAdminHandler dispatches the /v1/admin/ subtree between the auth
// endpoints (POST /v1/admin/auth/login|logout) and the Admin API. Both
// child muxes register absolute-path patterns and receive the unmodified
// request path, so plain prefix dispatch is sufficient.
func combinedAdminHandler(authHandler, adminHandler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1/admin/auth/") {
			authHandler.ServeHTTP(w, r)
			return
		}
		adminHandler.ServeHTTP(w, r)
	})
}

// deviceAuthAdapter adapts the auth.Verifier (SEC-03) to the connector
// DeviceAuth seam (identical to cmd/adc).
type deviceAuthAdapter struct{ v *auth.Verifier }

func (a deviceAuthAdapter) Authenticate(ctx context.Context, hs *connector.Handshake) (*connector.DeviceIdentity, error) {
	cred, err := a.v.Verify(ctx, hs.DeviceID, hs.Timestamp, hs.Nonce, hs.Signature)
	if err != nil {
		return nil, err
	}
	return &connector.DeviceIdentity{TenantID: cred.TenantID, DeviceCode: cred.DeviceCode}, nil
}

// The lookup closures below are verbatim copies of cmd/adc/main.go (they
// live in package main and are not importable).

func quotaLookup(pool *db.Pool) agentapi.QuotaLookup {
	return func(ctx context.Context, tenantID string) (int64, int64, error) {
		var used, quota int64
		err := pool.QueryRow(ctx,
			`SELECT used_calls_month, quota_calls_monthly
			   FROM adc_tenants WHERE id = $1::uuid AND deleted_at IS NULL`,
			tenantID).Scan(&used, &quota)
		return used, quota, err
	}
}

func principalKey(scope ratelimit.Scope) ratelimit.KeyFunc {
	return func(r *http.Request) (ratelimit.Scope, string) {
		p, ok := agentauth.FromContext(r.Context())
		if !ok {
			return scope, ""
		}
		if scope == ratelimit.ScopeAgent {
			return scope, p.AgentID
		}
		return scope, p.TenantID
	}
}

func deviceUUIDLookup(pool *db.Pool) agentapi.DeviceUUIDLookup {
	return func(ctx context.Context, tenantID, deviceCode string) (string, error) {
		var uuid string
		err := pool.QueryRow(ctx,
			`SELECT id::text FROM adc_devices WHERE tenant_id=$1::uuid AND device_code=$2`,
			tenantID, deviceCode).Scan(&uuid)
		return uuid, err
	}
}

// ownerCheck is the tenant ownership gate of the tool call path
// (design/33 13007, SEC-02): a device that does not exist in the caller's
// tenant ledger returns agentapi.ErrDeviceNotOwned (403), never leaking
// existence through a 500.
func ownerCheck(pool *db.Pool) agentapi.DeviceOwnerCheck {
	return func(ctx context.Context, tenantID, deviceCode string) error {
		var exists bool
		err := pool.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM adc_devices WHERE tenant_id=$1::uuid AND device_code=$2)`,
			tenantID, deviceCode).Scan(&exists)
		if err != nil {
			return err
		}
		if !exists {
			return agentapi.ErrDeviceNotOwned
		}
		return nil
	}
}

func riskLookup(pool *db.Pool) agentapi.RiskLookup {
	return func(ctx context.Context, tenantID, deviceCode, toolName string) (int, error) {
		var level int
		err := pool.QueryRow(ctx,
			`SELECT t.risk_level FROM adc_device_tools t
			 JOIN adc_devices d ON d.id = t.device_id
			 WHERE d.tenant_id=$1::uuid AND d.device_code=$2 AND t.tool_name=$3`,
			tenantID, deviceCode, toolName).Scan(&level)
		if err != nil {
			return 0, agentapi.RiskNotFound
		}
		return level, nil
	}
}

func toolRows(pool *db.Pool) agentapi.ToolRows {
	return func(ctx context.Context, tenantID string) ([]agentapi.ToolRow, error) {
		rows, err := pool.Query(ctx,
			`SELECT d.device_code, t.tool_name, COALESCE(t.description,''),
			        t.input_schema::text, t.risk_level
			 FROM adc_device_tools t
			 JOIN adc_devices d ON d.id = t.device_id
			 WHERE d.tenant_id=$1::uuid AND t.is_enabled=true`, tenantID)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []agentapi.ToolRow
		for rows.Next() {
			var r agentapi.ToolRow
			var schema string
			if err := rows.Scan(&r.DeviceCode, &r.ToolName, &r.Description, &schema, &r.RiskLevel); err != nil {
				return nil, err
			}
			r.InputSchema = json.RawMessage(schema)
			out = append(out, r)
		}
		return out, rows.Err()
	}
}

// ---------------------------------------------------------------------------
// pgUserStore: minimal PostgreSQL UserStore for adminauth (design/80 B-01).
// No PG implementation exists in internal/adminauth yet, so the integration
// package supplies it; it loads the bcrypt hash and the role names from
// adc_roles / adc_user_roles.
// ---------------------------------------------------------------------------

type pgUserStore struct {
	pool *pgxpool.Pool
}

func (s *pgUserStore) GetByUsername(ctx context.Context, username string) (*adminauth.User, error) {
	var u adminauth.User
	err := s.pool.QueryRow(ctx, `
		SELECT id::text, username, password_hash, tenant_id::text, status
		  FROM adc_users
		 WHERE username = $1 AND deleted_at IS NULL`, username).
		Scan(&u.ID, &u.Username, &u.PasswordHash, &u.TenantID, &u.Status)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, adminauth.ErrUserNotFound
	}
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `
		SELECT r.role_code
		  FROM adc_user_roles ur
		  JOIN adc_roles r ON r.id = ur.role_id
		 WHERE ur.user_id = $1::uuid`, u.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var role string
		if err := rows.Scan(&role); err != nil {
			return nil, err
		}
		u.Roles = append(u.Roles, role)
	}
	return &u, rows.Err()
}

// ---------------------------------------------------------------------------
// pgAuditQueryRepo: minimal PostgreSQL AuditQueryRepo for adminapi
// (design/80 B-06, FR-013). No PG implementation exists in internal/
// adminapi yet; the integration package supplies one riding the partition
// index (idx_audit_tenant_time) and the (created_at, id) keyset cursor of
// design/33 1.6.
// ---------------------------------------------------------------------------

type pgAuditQueryRepo struct {
	pool *pgxpool.Pool
}

func newPGAuditQueryRepo(pool *pgxpool.Pool) *pgAuditQueryRepo {
	return &pgAuditQueryRepo{pool: pool}
}

// auditSelect is the shared column projection (mirrors adminapi.AuditLog
// field order; device_code comes from the ledger join). Nullable text
// columns are COALESCE'd because AuditLog models them as plain strings
// (pgx cannot scan NULL into *string).
const auditSelect = `
SELECT a.id::text, a.tenant_id::text, a.event_type, a.actor_type,
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
  LEFT JOIN adc_devices d ON d.id = a.device_id`

// auditWhere builds the WHERE clause shared by Query/Count/Export. All
// values travel as parameters (SEC-20: no string concatenation); add
// appends the values first and then renders integer placeholder indices
// so multi-placeholder conditions index correctly.
func auditWhere(f adminapi.AuditFilter, args *[]any) string {
	var conds []string
	add := func(sql string, vals ...any) {
		start := len(*args) + 1
		*args = append(*args, vals...)
		nums := make([]any, 0, len(vals))
		for i := range vals {
			nums = append(nums, start+i)
		}
		conds = append(conds, fmt.Sprintf(sql, nums...))
	}
	add(`a.tenant_id = $%d::uuid`, f.TenantID)
	if f.TimeFrom != nil {
		add(`a.created_at >= $%d`, *f.TimeFrom)
	}
	if f.TimeTo != nil {
		add(`a.created_at <= $%d`, *f.TimeTo)
	}
	if f.DeviceID != "" {
		add(`a.device_id = $%d::uuid`, f.DeviceID)
	}
	if f.DeviceCode != "" {
		add(`d.device_code = $%d`, f.DeviceCode)
	}
	if f.ToolName != "" {
		add(`a.tool_name = $%d`, f.ToolName)
	}
	if f.Status != "" {
		add(`a.status = $%d`, f.Status)
	}
	if f.EventType != "" {
		add(`a.event_type = $%d`, f.EventType)
	}
	if f.AgentID != "" {
		add(`a.actor_id = $%d`, f.AgentID)
	}
	if f.Keyword != "" {
		pattern := "%" + f.Keyword + "%"
		add(`(a.tool_name ILIKE $%d OR a.actor_id ILIKE $%d OR a.hitl_approver ILIKE $%d OR a.hitl_comment ILIKE $%d)`,
			pattern, pattern, pattern, pattern)
	}
	return " WHERE " + strings.Join(conds, " AND ")
}

func scanAuditLog(rows pgx.Rows) (*adminapi.AuditLog, error) {
	var l adminapi.AuditLog
	var params, payload []byte
	err := rows.Scan(
		&l.ID, &l.TenantID, &l.EventType, &l.ActorType, &l.ActorID,
		&l.ApiKeyID, &l.DeviceID, &l.DeviceCode, &l.ToolName, &l.RiskLevel,
		&params, &payload, &l.ResponseTruncated, &l.ExecutionDurationMS,
		&l.Status, &l.HitlTicketID, &l.HitlApprover, &l.HitlComment,
		&l.ExemptReason, &l.RequestID, &l.CreatedAt)
	if err != nil {
		return nil, err
	}
	if len(params) > 0 {
		if err := json.Unmarshal(params, &l.RequestParams); err != nil {
			return nil, err
		}
	}
	if len(payload) > 0 {
		if err := json.Unmarshal(payload, &l.ResponsePayload); err != nil {
			return nil, err
		}
	}
	return &l, nil
}

// Query returns one keyset window ordered created_at DESC, id DESC
// (design/33 1.6). One extra row is fetched to decide NextCursor.
func (r *pgAuditQueryRepo) Query(ctx context.Context, f adminapi.AuditFilter, after *adminapi.AuditCursor, limit int) (*adminapi.AuditPage, error) {
	var args []any
	where := auditWhere(f, &args)
	if after != nil {
		args = append(args, after.CreatedAt, after.ID)
		where += fmt.Sprintf(` AND (a.created_at, a.id) < ($%d, $%d::uuid)`, len(args)-1, len(args))
	}
	args = append(args, limit+1)
	q := auditSelect + where + fmt.Sprintf(` ORDER BY a.created_at DESC, a.id DESC LIMIT $%d`, len(args))
	rows, err := r.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	page := &adminapi.AuditPage{}
	for rows.Next() {
		l, err := scanAuditLog(rows)
		if err != nil {
			return nil, err
		}
		if len(page.Items) == limit {
			page.NextCursor = &adminapi.AuditCursor{CreatedAt: page.Items[len(page.Items)-1].CreatedAt, ID: page.Items[len(page.Items)-1].ID}
			break
		}
		page.Items = append(page.Items, *l)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return page, nil
}

// Count reports the rows matching the filter (export cap check).
func (r *pgAuditQueryRepo) Count(ctx context.Context, f adminapi.AuditFilter) (int, error) {
	var args []any
	where := auditWhere(f, &args)
	var n int
	err := r.pool.QueryRow(ctx, `SELECT count(*) FROM adc_audit_logs a
		LEFT JOIN adc_devices d ON d.id = a.device_id`+where, args...).Scan(&n)
	return n, err
}

// Export streams matching rows as CSV capped at maxRows (design/80 B-06
// sync export model); exceeding the cap aborts with the shared sentinel.
func (r *pgAuditQueryRepo) Export(ctx context.Context, f adminapi.AuditFilter, maxRows int, w io.Writer) (int, error) {
	var args []any
	where := auditWhere(f, &args)
	args = append(args, maxRows+1)
	q := auditSelect + where + fmt.Sprintf(` ORDER BY a.created_at DESC, a.id DESC LIMIT $%d`, len(args))
	rows, err := r.pool.Query(ctx, q, args...)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	cw := csv.NewWriter(w)
	if err := cw.Write(auditCSVHeader()); err != nil {
		return 0, err
	}
	written := 0
	for rows.Next() {
		l, err := scanAuditLog(rows)
		if err != nil {
			return 0, err
		}
		if written >= maxRows {
			return written, adminapi.ErrAuditExportLimitExceeded
		}
		if err := cw.Write(auditCSVRow(*l)); err != nil {
			return written, err
		}
		written++
	}
	cw.Flush()
	return written, cw.Error()
}

// auditCSVHeader mirrors adminapi's export contract (design/33 3.1.16).
func auditCSVHeader() []string {
	return []string{
		"log_id", "tenant_id", "event_type", "actor_type", "actor_id",
		"api_key_id", "device_id", "device_code", "tool_name", "risk_level",
		"status", "hitl_ticket_id", "hitl_approver", "hitl_comment",
		"exempt_reason", "execution_duration_ms", "request_id",
		"response_truncated", "request_params", "response_payload",
		"created_at",
	}
}

func auditCSVRow(l adminapi.AuditLog) []string {
	var params, payload []byte
	if l.RequestParams != nil {
		params, _ = json.Marshal(l.RequestParams)
	}
	if l.ResponsePayload != nil {
		payload, _ = json.Marshal(l.ResponsePayload)
	}
	str := func(v *int) string {
		if v == nil {
			return ""
		}
		return fmt.Sprintf("%d", *v)
	}
	return []string{
		l.ID, l.TenantID, l.EventType, l.ActorType, l.ActorID,
		l.ApiKeyID, l.DeviceID, l.DeviceCode, l.ToolName, str(l.RiskLevel),
		l.Status, l.HitlTicketID, l.HitlApprover, l.HitlComment,
		l.ExemptReason, str(l.ExecutionDurationMS), l.RequestID,
		fmt.Sprintf("%t", l.ResponseTruncated), string(params), string(payload),
		l.CreatedAt.UTC().Format(time.RFC3339),
	}
}

// ---------------------------------------------------------------------------
// pgToolRepo: minimal PostgreSQL ToolRepo for adminapi (design/80 B-04,
// FR-006). Like the audit repo, no PG implementation exists in internal/
// adminapi yet; this one persists risk_level / is_enabled with
// risk_changed_by (SEC-21 real subject) and partial-failure outcomes.
// ---------------------------------------------------------------------------

type pgToolRepo struct {
	pool *pgxpool.Pool
}

const toolCols = `id::text, tenant_id::text, device_id::text, tool_name,
	COALESCE(display_name,''), COALESCE(description,''), input_schema,
	risk_level, COALESCE(risk_changed_by::text,''), schema_version, is_enabled, updated_at`

func scanTool(row pgx.Row) (*adminapi.DeviceTool, error) {
	var t adminapi.DeviceTool
	var schema []byte
	err := row.Scan(&t.ID, &t.TenantID, &t.DeviceID, &t.ToolName,
		&t.DisplayName, &t.Description, &schema, &t.RiskLevel,
		&t.RiskChangedBy, &t.SchemaVersion, &t.IsEnabled, &t.UpdatedAt)
	if err != nil {
		return nil, err
	}
	if len(schema) > 0 {
		if err := json.Unmarshal(schema, &t.InputSchema); err != nil {
			return nil, err
		}
	}
	return &t, nil
}

// ListTools pages the tool ledger of one device (design/33 3.1.10).
func (r *pgToolRepo) ListTools(ctx context.Context, deviceID string, p adminapi.Page) ([]adminapi.DeviceTool, int, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+toolCols+`, count(*) OVER () AS total
		FROM adc_device_tools WHERE device_id = $1::uuid
		ORDER BY tool_name LIMIT $2 OFFSET $3`,
		deviceID, p.Size, (p.Number-1)*p.Size)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var (
		out   []adminapi.DeviceTool
		total int
	)
	for rows.Next() {
		var t adminapi.DeviceTool
		var schema []byte
		if err := rows.Scan(&t.ID, &t.TenantID, &t.DeviceID, &t.ToolName,
			&t.DisplayName, &t.Description, &schema, &t.RiskLevel,
			&t.RiskChangedBy, &t.SchemaVersion, &t.IsEnabled, &t.UpdatedAt, &total); err != nil {
			return nil, 0, err
		}
		if len(schema) > 0 {
			_ = json.Unmarshal(schema, &t.InputSchema)
		}
		out = append(out, t)
	}
	return out, total, rows.Err()
}

// GetTool loads one tool by (device, name).
func (r *pgToolRepo) GetTool(ctx context.Context, deviceID, toolName string) (*adminapi.DeviceTool, error) {
	row := r.pool.QueryRow(ctx, `SELECT `+toolCols+` FROM adc_device_tools
		WHERE device_id = $1::uuid AND tool_name = $2`, deviceID, toolName)
	t, err := scanTool(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, adminapi.ErrToolNotFound
	}
	return t, err
}

// UpdateTools applies changes one by one; a missing tool becomes a failed
// outcome carrying adminapi.ErrToolNotFound while the rest still apply
// (design/33 3.1.11 partial-update semantics).
func (r *pgToolRepo) UpdateTools(ctx context.Context, deviceID string, changes []adminapi.ToolChange, changedBy string) ([]adminapi.ToolUpdateOutcome, error) {
	out := make([]adminapi.ToolUpdateOutcome, 0, len(changes))
	for _, c := range changes {
		var (
			risk   any // nil = keep current risk_level
			enab   any // nil = keep current is_enabled
			byUser any // nil = not a risk change
		)
		if c.RiskLevel != nil {
			risk = *c.RiskLevel
			byUser = changedBy
		}
		if c.IsEnabled != nil {
			enab = *c.IsEnabled
		}
		row := r.pool.QueryRow(ctx, `UPDATE adc_device_tools
			SET risk_level = COALESCE($2, risk_level),
			    is_enabled = COALESCE($3, is_enabled),
			    risk_changed_by = COALESCE($4::uuid, risk_changed_by),
			    updated_at = now()
			WHERE device_id = $1::uuid AND tool_name = $5
			RETURNING `+toolCols, deviceID, risk, enab, byUser, c.ToolName)
		updated, err := scanTool(row)
		if errors.Is(err, pgx.ErrNoRows) {
			out = append(out, adminapi.ToolUpdateOutcome{ToolName: c.ToolName, Err: adminapi.ErrToolNotFound})
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, adminapi.ToolUpdateOutcome{ToolName: c.ToolName, Tool: updated})
	}
	return out, nil
}
