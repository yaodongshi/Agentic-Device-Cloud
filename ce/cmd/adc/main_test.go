package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"

	"adc.dev/ce/internal/adminapi"
	"adc.dev/ce/internal/agentapi"
	"adc.dev/ce/internal/agentauth"
	"adc.dev/ce/internal/approval"
	"adc.dev/ce/internal/config"
	"adc.dev/ce/pkg/audit"
	"adc.dev/ce/pkg/ratelimit"
)

const seedTenantUUID = "11111111-2222-3333-4444-555555555555"
const seedCNCUUID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
const seedAGVUUID = "bbbbbbbb-cccc-dddd-eeee-ffffffffffff"

var seedKEK = []byte("0123456789abcdef0123456789abcdef")

var testSeedSecrets = devSeedSecrets{
	CNCDevice:      strings.Repeat("a", 64),
	AGVDevice:      strings.Repeat("b", 64),
	AdminPassword:  "admin-explicit-password",
	TenantPassword: "tenant-explicit-password",
	ApproverPass:   "approver-explicit-password",
	AgentAPIKey:    "adc_1234abcd_" + strings.Repeat("c", 64),
}

// fakeKeyRepo records the issued NewKey and returns a fixed record.
type fakeKeyRepo struct {
	issued *adminapi.NewKey
}

func (f *fakeKeyRepo) Issue(ctx context.Context, k *adminapi.NewKey) (*adminapi.ApiKey, error) {
	f.issued = k
	return &adminapi.ApiKey{ID: "key-uuid-1", Name: k.Name, TenantID: k.TenantID, KeyPrefix: k.KeyPrefix}, nil
}
func (f *fakeKeyRepo) Get(ctx context.Context, keyID string) (*adminapi.ApiKey, error) {
	return nil, adminapi.ErrApiKeyNotFound
}
func (f *fakeKeyRepo) List(ctx context.Context, tenantID string, p adminapi.Page) ([]adminapi.ApiKey, int, error) {
	return nil, 0, nil
}
func (f *fakeKeyRepo) Revoke(ctx context.Context, keyID, reason string) (*adminapi.ApiKey, error) {
	return nil, adminapi.ErrApiKeyNotFound
}
func (f *fakeKeyRepo) Rotate(ctx context.Context, keyID string, k *adminapi.NewKey, reason string) (*adminapi.ApiKey, error) {
	return nil, adminapi.ErrApiKeyNotFound
}

// fakeTicketRepo returns a fixed ticket, optionally signalling dedup.
type fakeTicketRepo struct {
	dedup bool
	seen  *approval.ApprovalTicket
}

func (f *fakeTicketRepo) Create(ctx context.Context, t *approval.ApprovalTicket) (*approval.ApprovalTicket, error) {
	f.seen = t
	if f.dedup {
		return &approval.ApprovalTicket{TicketID: "ticket-uuid-existing"}, approval.ErrTicketDedup
	}
	return &approval.ApprovalTicket{TicketID: "ticket-uuid-1"}, nil
}
func (f *fakeTicketRepo) Transition(ctx context.Context, ticketID string, wantVersion int, cmd approval.TransitionCmd) (*approval.ApprovalTicket, error) {
	return nil, approval.ErrTicketHandled
}
func (f *fakeTicketRepo) Get(ctx context.Context, ticketID string) (*approval.ApprovalTicket, error) {
	return nil, approval.ErrTicketNotFound
}
func (f *fakeTicketRepo) FindExpired(ctx context.Context, now time.Time, limit int) ([]approval.ApprovalTicket, error) {
	return nil, nil
}

// fakeAuditSink captures enqueued events.
type fakeAuditSink struct {
	events []*audit.AuditEvent
}

func (f *fakeAuditSink) Enqueue(ctx context.Context, ev *audit.AuditEvent) error {
	f.events = append(f.events, ev)
	return nil
}

// expectSeedCommon declares the ordered SQL expectations of seedDemo.
func expectSeedCommon(m pgxmock.PgxPoolIface, keyExists bool, auditExists bool) {
	m.ExpectExec(regexp.QuoteMeta(`INSERT INTO adc_tenants (id, code, name, status)
		SELECT gen_random_uuid(), 'tenant-demo', 'Demo', 'ACTIVE'
		WHERE NOT EXISTS (SELECT 1 FROM adc_tenants WHERE code='tenant-demo')`)).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	m.ExpectQuery(regexp.QuoteMeta(`SELECT id::text FROM adc_tenants WHERE code='tenant-demo'`)).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(seedTenantUUID))
	m.ExpectExec(regexp.QuoteMeta(`INSERT INTO adc_devices (id, tenant_id, device_code, name, device_type, device_class, auth_type, credential_hash, status)
		SELECT gen_random_uuid(), id, 'cnc-demo-01', 'Demo CNC', 'cnc', 'B', 'hmac', $1, 'OFFLINE'
		FROM adc_tenants WHERE code='tenant-demo'
		AND NOT EXISTS (SELECT 1 FROM adc_devices WHERE device_code='cnc-demo-01')`)).
		WithArgs(pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", map[bool]int64{true: 0, false: 1}[keyExists]))
	m.ExpectExec(regexp.QuoteMeta(`INSERT INTO adc_device_tools (id, tenant_id, device_id, tool_name, description, input_schema, annotations, risk_level, is_enabled)
		SELECT gen_random_uuid(), d.tenant_id, d.id, 'set_spindle_speed', 'Set CNC spindle RPM (high risk)', '{"type":"object","properties":{"rpm":{"type":"number"}},"required":["rpm"]}', '{"adc_param_rules":{"rpm":{"min":0,"max":12000}}}', 2, true
		FROM adc_devices d WHERE d.device_code='cnc-demo-01'
		ON CONFLICT (device_id, tool_name) DO UPDATE
		SET input_schema=EXCLUDED.input_schema,
			annotations=jsonb_set(COALESCE(adc_device_tools.annotations, '{}'), '{adc_param_rules}',
			COALESCE(adc_device_tools.annotations->'adc_param_rules', '{}') || '{"rpm":{"min":0,"max":12000}}', true)`)).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	m.ExpectExec(regexp.QuoteMeta(`INSERT INTO adc_device_tools (id, tenant_id, device_id, tool_name, description, input_schema, risk_level, is_enabled)
		SELECT gen_random_uuid(), d.tenant_id, d.id, 'get_spindle_status', 'Read CNC spindle RPM (read-only)', '{"type":"object"}', 0, true
		FROM adc_devices d WHERE d.device_code='cnc-demo-01'
		AND NOT EXISTS (SELECT 1 FROM adc_device_tools t WHERE t.device_id=d.id AND t.tool_name='get_spindle_status')`)).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	m.ExpectExec(regexp.QuoteMeta(`INSERT INTO adc_devices (id, tenant_id, device_code, name, device_type, device_class, auth_type, credential_hash, status)
		SELECT gen_random_uuid(), id, 'agv-demo-01', 'Demo AGV', 'agv', 'B', 'hmac', $1, 'OFFLINE'
		FROM adc_tenants WHERE code='tenant-demo'
		AND NOT EXISTS (SELECT 1 FROM adc_devices WHERE device_code='agv-demo-01')`)).
		WithArgs(pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", map[bool]int64{true: 0, false: 1}[keyExists]))
	m.ExpectExec(regexp.QuoteMeta(`INSERT INTO adc_device_tools (id, tenant_id, device_id, tool_name, description, input_schema, risk_level, is_enabled)
		SELECT gen_random_uuid(), d.tenant_id, d.id, 'move_to', 'Move AGV to a warehouse location (high risk)', '{"type":"object","properties":{"location":{"type":"string"}},"required":["location"]}', 2, true
		FROM adc_devices d WHERE d.device_code='agv-demo-01'
		AND NOT EXISTS (SELECT 1 FROM adc_device_tools t WHERE t.device_id=d.id AND t.tool_name='move_to')`)).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	m.ExpectExec(regexp.QuoteMeta(`INSERT INTO adc_device_tools (id, tenant_id, device_id, tool_name, description, input_schema, risk_level, is_enabled)
		SELECT gen_random_uuid(), d.tenant_id, d.id, 'get_position', 'Read AGV position (read-only)', '{"type":"object"}', 0, true
		FROM adc_devices d WHERE d.device_code='agv-demo-01'
		AND NOT EXISTS (SELECT 1 FROM adc_device_tools t WHERE t.device_id=d.id AND t.tool_name='get_position')`)).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	for _, q := range []string{
		`INSERT INTO adc_roles (tenant_id, role_code, scope, description, permissions, is_system)
		 VALUES (NULL, 'PLATFORM_ADMIN', 'PLATFORM', 'Platform administrator (system role)', '[]', TRUE)
		 ON CONFLICT DO NOTHING`,
		`INSERT INTO adc_roles (tenant_id, role_code, scope, description, permissions, is_system)
		 SELECT id, 'TENANT_ADMIN', 'TENANT', 'Tenant administrator (system role)', '[]', TRUE
		 FROM adc_tenants WHERE code='tenant-demo' ON CONFLICT DO NOTHING`,
		`INSERT INTO adc_roles (tenant_id, role_code, scope, description, permissions, is_system)
		 SELECT id, 'APPROVER', 'TENANT', 'Approval approver (system role)', '[]', TRUE
		 FROM adc_tenants WHERE code='tenant-demo' ON CONFLICT DO NOTHING`,
		`INSERT INTO adc_roles (tenant_id, role_code, scope, description, permissions, is_system)
		 SELECT id, 'AUDITOR', 'TENANT', 'Audit auditor (system role)', '[]', TRUE
		 FROM adc_tenants WHERE code='tenant-demo' ON CONFLICT DO NOTHING`,
	} {
		m.ExpectExec(regexp.QuoteMeta(q)).
			WillReturnResult(pgxmock.NewResult("INSERT", 1))
	}
	m.ExpectExec(regexp.QuoteMeta(`INSERT INTO adc_users (tenant_id, username, display_name, password_hash, auth_source, status)
		SELECT id, 'admin', 'Platform Admin', $1, 'LOCAL', 'ACTIVE'
		FROM adc_tenants WHERE code='tenant-demo'
		ON CONFLICT DO NOTHING`)).
		WithArgs(pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	m.ExpectExec(regexp.QuoteMeta(`INSERT INTO adc_user_roles (user_id, role_id, tenant_id)
		SELECT u.id, r.id, u.tenant_id FROM adc_users u
		JOIN adc_roles r ON r.role_code='PLATFORM_ADMIN' AND r.scope='PLATFORM'
		WHERE u.username='admin'
		ON CONFLICT DO NOTHING`)).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	// Tenant-level demo accounts (A1.1): tenant-admin and approver.
	for range 2 {
		m.ExpectExec(regexp.QuoteMeta(`INSERT INTO adc_users (tenant_id, username, display_name, password_hash, auth_source, status)
		SELECT id, $1, $2, $3, 'LOCAL', 'ACTIVE'
		FROM adc_tenants WHERE code='tenant-demo'
		ON CONFLICT DO NOTHING`)).
			WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
			WillReturnResult(pgxmock.NewResult("INSERT", 1))
		m.ExpectExec(regexp.QuoteMeta(`INSERT INTO adc_user_roles (user_id, role_id, tenant_id)
		SELECT u.id, r.id, u.tenant_id FROM adc_users u
		JOIN adc_roles r ON r.role_code=$1 AND r.tenant_id = u.tenant_id
		WHERE u.username=$2
		ON CONFLICT DO NOTHING`)).
			WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg()).
			WillReturnResult(pgxmock.NewResult("INSERT", 1))
	}
	m.ExpectQuery(regexp.QuoteMeta(`SELECT EXISTS(SELECT 1 FROM adc_agent_api_keys
		WHERE tenant_id=$1::uuid AND name='demo-agent' AND revoked_at IS NULL)`)).
		WithArgs(seedTenantUUID).
		WillReturnRows(pgxmock.NewRows([]string{"exists"}).AddRow(keyExists))
	if !keyExists {
		m.ExpectQuery(regexp.QuoteMeta(`SELECT id::text FROM adc_users
			WHERE username='admin' AND deleted_at IS NULL`)).
			WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("admin-uuid-1"))
	}
	m.ExpectQuery(regexp.QuoteMeta(`SELECT id::text FROM adc_devices WHERE device_code='cnc-demo-01'`)).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(seedCNCUUID))
	m.ExpectQuery(regexp.QuoteMeta(`SELECT id::text FROM adc_devices WHERE device_code='agv-demo-01'`)).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(seedAGVUUID))
	for _, evID := range []string{"d3e00000-0000-4000-8000-000000000001", "d3e00000-0000-4000-8000-000000000002", "d3e00000-0000-4000-8000-000000000003"} {
		m.ExpectQuery(regexp.QuoteMeta(`SELECT EXISTS(SELECT 1 FROM adc_audit_logs WHERE request_id=$1::uuid)`)).
			WithArgs(evID).
			WillReturnRows(pgxmock.NewRows([]string{"exists"}).AddRow(auditExists))
	}
}

func TestSeedDemoFullRun(t *testing.T) {
	m, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer m.Close()
	expectSeedCommon(m, false, false)

	keys := &fakeKeyRepo{}
	tickets := &fakeTicketRepo{}
	sink := &fakeAuditSink{}
	demo, err := seedDemo(context.Background(), seedDemoInput{
		dbx: m, kek: seedKEK, keys: keys, tickets: tickets, sink: sink, secrets: testSeedSecrets,
	})
	if err != nil {
		t.Fatalf("seedDemo: %v", err)
	}
	if len(demo.DeviceSecret) != 64 || len(demo.AGVSecret) != 64 {
		t.Fatalf("device secrets must be 64 hex chars: cnc=%q agv=%q", demo.DeviceSecret, demo.AGVSecret)
	}
	if !strings.HasPrefix(demo.APIKey, "adc_") || demo.APIKeyID != "key-uuid-1" {
		t.Fatalf("unexpected api key: %+v", demo)
	}
	if keys.issued == nil || keys.issued.Name != "demo-agent" || keys.issued.AgentID != "demo-agent" ||
		keys.issued.TenantID != seedTenantUUID || keys.issued.SecretHash == "" || keys.issued.KeyHash == "" {
		t.Fatalf("issued key not recorded properly: %+v", keys.issued)
	}
	if keys.issued.CreatedBy != "admin-uuid-1" {
		t.Fatalf("created_by must be the demo admin user id, got %q", keys.issued.CreatedBy)
	}
	if demo.TicketID != "ticket-uuid-1" {
		t.Fatalf("unexpected ticket id: %q", demo.TicketID)
	}
	if tickets.seen == nil || tickets.seen.ToolName != "set_spindle_speed" || tickets.seen.DeviceID != seedCNCUUID {
		t.Fatalf("ticket not created through the repo with the demo device: %+v", tickets.seen)
	}
	if len(demo.AuditEventIDs) != 3 || len(sink.events) != 3 {
		t.Fatalf("want 3 audit events, got %d (%d enqueued)", len(demo.AuditEventIDs), len(sink.events))
	}
	wantStatus := map[string]string{
		"d3e00000-0000-4000-8000-000000000001": audit.StatusSuccess,
		"d3e00000-0000-4000-8000-000000000002": audit.StatusBlockedByHITL,
		"d3e00000-0000-4000-8000-000000000003": audit.StatusFailed,
	}
	for i, ev := range sink.events {
		if wantStatus[ev.EventID] != ev.Status {
			t.Fatalf("event %d wrong status: %+v", i, ev)
		}
		if ev.TenantID != seedTenantUUID {
			t.Fatalf("event %d wrong tenant: %+v", i, ev)
		}
	}
	if err := m.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestSeedDemoIdempotentRerun(t *testing.T) {
	m, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer m.Close()
	// key and audit events already exist from a previous run
	expectSeedCommon(m, true, true)

	keys := &fakeKeyRepo{}
	tickets := &fakeTicketRepo{dedup: true}
	sink := &fakeAuditSink{}
	demo, err := seedDemo(context.Background(), seedDemoInput{
		dbx: m, kek: seedKEK, keys: keys, tickets: tickets, sink: sink, secrets: testSeedSecrets,
	})
	if err != nil {
		t.Fatalf("seedDemo: %v", err)
	}
	if demo.APIKey != "" {
		t.Fatalf("re-run must not reissue the api key, got %q", demo.APIKey)
	}
	if keys.issued != nil {
		t.Fatalf("re-run must not call Issue, got %+v", keys.issued)
	}
	if demo.TicketID != "ticket-uuid-existing" {
		t.Fatalf("re-run must reuse the deduped ticket, got %q", demo.TicketID)
	}
	if len(demo.AuditEventIDs) != 0 || len(sink.events) != 0 {
		t.Fatalf("re-run must not re-enqueue audit events: %v %v", demo.AuditEventIDs, sink.events)
	}
	if err := m.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestSeedDemoNilSinkSkipsAudit(t *testing.T) {
	m, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer m.Close()
	expectSeedCommon(m, true, false)

	demo, err := seedDemo(context.Background(), seedDemoInput{
		dbx: m, kek: seedKEK, keys: &fakeKeyRepo{}, tickets: &fakeTicketRepo{}, sink: nil, secrets: testSeedSecrets,
	})
	if err != nil {
		t.Fatalf("seedDemo: %v", err)
	}
	if len(demo.AuditEventIDs) != 0 {
		t.Fatalf("nil sink must skip audit events, got %v", demo.AuditEventIDs)
	}
}

func TestParseDemoKeyMaterial(t *testing.T) {
	keyID, prefix, secret, keyHash, secretHash, err := parseDemoKeyMaterial(testSeedSecrets.AgentAPIKey)
	if err != nil {
		t.Fatalf("parseDemoKeyMaterial: %v", err)
	}
	if len(keyID) != 8 || prefix != "adc_"+keyID || len(secret) != 64 {
		t.Fatalf("unexpected material: %s %s %s", keyID, prefix, secret)
	}
	if keyHash != agentauth.HashKeyID(keyID) || secretHash != agentauth.HashSecret(secret) {
		t.Fatal("hashes must match agentauth's")
	}
}

func TestLoadNodeKey(t *testing.T) {
	cfg := &configDemo{Env: "dev", NodeKey: ""}
	key, err := loadNodeKey(&config.Config{Env: "dev", NodeKey: ""})
	if err != nil || len(key) != 32 {
		t.Fatalf("dev fallback must derive a 32-byte key: %v", err)
	}
	if _, err := loadNodeKey(&config.Config{Env: "prod", NodeKey: ""}); err == nil {
		t.Fatal("prod without ADC_NODE_KEY must fail (SEC-06)")
	}
	if _, err := loadNodeKey(&config.Config{Env: "prod", NodeKey: "zz"}); err == nil {
		t.Fatal("non-hex key must fail")
	}
	hexKey := strings.Repeat("ab", 16) // 16 bytes
	key, err = loadNodeKey(&config.Config{Env: "prod", NodeKey: hexKey})
	if err != nil || len(key) != 16 {
		t.Fatalf("valid 16-byte hex key must load: %v", err)
	}
	if _, err := loadNodeKey(&config.Config{Env: "prod", NodeKey: "aabbccddeeff"}); err == nil {
		t.Fatal("short key must fail")
	}
	_ = cfg
}

type configDemo struct {
	Env     string
	NodeKey string
}

func TestCombinedAdminHandler(t *testing.T) {
	var (
		authCalled, adminCalled bool
	)
	authH := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { authCalled = true })
	adminH := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { adminCalled = true })
	h := combinedAdminHandler(authH, adminH)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/admin/auth/login", nil))
	if !authCalled || adminCalled {
		t.Fatal("auth subtree must go to the auth handler")
	}
	authCalled, adminCalled = false, false
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/admin/devices", nil))
	if authCalled || !adminCalled {
		t.Fatal("admin routes must go to the admin handler")
	}
}

func TestPrincipalKey(t *testing.T) {
	tenantKey := principalKey(ratelimit.ScopeTenant)
	agentKey := principalKey(ratelimit.ScopeAgent)
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	if scope, key := tenantKey(req); scope != ratelimit.ScopeTenant || key != "" {
		t.Fatalf("no principal must yield empty key: %v %q", scope, key)
	}
	ctx := agentauth.WithPrincipal(req.Context(), &agentauth.Principal{TenantID: "t1", AgentID: "a1"})
	req = req.WithContext(ctx)
	if scope, key := tenantKey(req); scope != ratelimit.ScopeTenant || key != "t1" {
		t.Fatalf("tenant scope must key on tenant: %v %q", scope, key)
	}
	if scope, key := agentKey(req); scope != ratelimit.ScopeAgent || key != "a1" {
		t.Fatalf("agent scope must key on agent: %v %q", scope, key)
	}
}

func TestAuthValidator(t *testing.T) {
	v := authValidator{tenantID: "t1"}
	p, err := v.Validate(context.Background(), "dev-agent-key")
	if err != nil || p.TenantID != "t1" || p.AgentID != "dev-agent" {
		t.Fatalf("valid key must authenticate: %+v %v", p, err)
	}
	if _, err := v.Validate(context.Background(), "wrong"); !errors.Is(err, agentauth.ErrKeyNotFound) {
		t.Fatalf("unknown key must fail: %v", err)
	}
}

func TestObservingWrappers(t *testing.T) {
	// observingRouter success + failure
	inner := &stubRouter{err: nil}
	or := &observingRouter{inner: inner}
	if _, err := or.Call(context.Background(), "t1", agentapi.ToolRef{}, nil); err != nil {
		t.Fatalf("Call: %v", err)
	}
	or.inner = &stubRouter{err: errors.New("boom")}
	if _, err := or.Call(context.Background(), "t1", agentapi.ToolRef{}, nil); err == nil {
		t.Fatal("want error passthrough")
	}

	// observingHITL create + await decisions
	oh := &observingHITL{inner: &stubHITL{ref: &agentapi.TicketRef{TicketID: "x"}}}
	if _, err := oh.CreateTicket(context.Background(), &agentapi.TicketRequest{TenantID: "t1"}); err != nil {
		t.Fatalf("CreateTicket: %v", err)
	}
	for _, st := range []string{agentapi.DecisionApproved, agentapi.DecisionRejected, agentapi.DecisionExpired} {
		oh.inner = &stubHITL{decision: &agentapi.TicketDecision{Status: st}}
		if _, err := oh.AwaitDecision(context.Background(), "x", time.Second); err != nil {
			t.Fatalf("AwaitDecision: %v", err)
		}
	}

	// observingNotifier
	on := &observingNotifier{inner: &stubNotifier{err: errors.New("down")}, channel: "wecom"}
	if err := on.SendApprovalCard(context.Background(), &approval.ApprovalTicket{}, "u1", "u2"); err == nil {
		t.Fatal("want notify error passthrough")
	}
	on.inner = &stubNotifier{}
	if err := on.SendApprovalCard(context.Background(), &approval.ApprovalTicket{}, "u1", "u2"); err != nil {
		t.Fatalf("SendApprovalCard: %v", err)
	}

	// observingRegistry (nil obs: pure passthrough, gauge writes skipped)
	reg := &observingRegistry{inner: &stubRegistry{}}
	if err := reg.Register(context.Background(), "t1", "d1", "n1", time.Minute); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := reg.Heartbeat(context.Background(), "t1", "d1"); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if err := reg.Unregister(context.Background(), "t1", "d1"); err != nil {
		t.Fatalf("Unregister: %v", err)
	}
	if err := reg.OnlineSet(context.Background(), "t1", "d1", true); err != nil {
		t.Fatalf("OnlineSet: %v", err)
	}
}

type stubRouter struct{ err error }

func (s *stubRouter) Call(ctx context.Context, tenantID string, ref agentapi.ToolRef, args map[string]interface{}) (*agentapi.CallResult, error) {
	if s.err != nil {
		return nil, s.err
	}
	return &agentapi.CallResult{}, nil
}

type stubHITL struct {
	ref      *agentapi.TicketRef
	decision *agentapi.TicketDecision
	err      error
}

func (s *stubHITL) CreateTicket(ctx context.Context, req *agentapi.TicketRequest) (*agentapi.TicketRef, error) {
	return s.ref, s.err
}
func (s *stubHITL) AwaitDecision(ctx context.Context, ticketID string, timeout time.Duration) (*agentapi.TicketDecision, error) {
	return s.decision, s.err
}

type stubNotifier struct{ err error }

func (s *stubNotifier) SendApprovalCard(ctx context.Context, ticket *approval.ApprovalTicket, approveURL, rejectURL string) error {
	return s.err
}

type stubRegistry struct{}

func (s *stubRegistry) Register(ctx context.Context, tenantID, deviceCode, nodeID string, ttl time.Duration) error {
	return nil
}
func (s *stubRegistry) Heartbeat(ctx context.Context, tenantID, deviceCode string) error {
	return nil
}
func (s *stubRegistry) Unregister(ctx context.Context, tenantID, deviceCode string) error {
	return nil
}
func (s *stubRegistry) OnlineSet(ctx context.Context, tenantID, deviceCode string, member bool) error {
	return nil
}

var _ = json.RawMessage(`{}`)
