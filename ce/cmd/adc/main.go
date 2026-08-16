// Command adc runs the ADC development server: all core modules assembled in
// a single process for local integration and smoke testing (design/31 5.x).
// Production deployment splits these modules into separate services behind
// the unified API gateway (design/60).
package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/redis/go-redis/v9"

	"adc.dev/ce/internal/adminapi"
	"adc.dev/ce/internal/adminauth"
	"adc.dev/ce/internal/agentapi"
	"adc.dev/ce/internal/agentapi/route"
	"adc.dev/ce/internal/agentauth"
	"adc.dev/ce/internal/approval"
	"adc.dev/ce/internal/auth"
	"adc.dev/ce/internal/config"
	"adc.dev/ce/internal/connector"
	"adc.dev/ce/internal/db"
	"adc.dev/ce/pkg/audit"
	"adc.dev/ce/pkg/clusterbus"
	"adc.dev/ce/pkg/metering"
	"adc.dev/ce/pkg/observe"
	"adc.dev/ce/pkg/ratelimit"
)

// demoAdminPassword is the default password of the seeded admin account
// (dev only, -seed flag). It is printed at seed time so operators know to
// change it before the database is shared outside a developer machine.
const demoAdminPassword = "admin123!"

func main() {
	var seed bool
	flag.BoolVar(&seed, "seed", false, "seed a demo tenant/device/tool and print the device secret (dev only)")
	flag.Parse()

	cfg, err := config.Load()
	if err != nil {
		fatal(err)
	}
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))

	// --- observability (design/80 B-09, design/60 6.1) ---
	// Standard-library-only Prometheus metrics; exposed on /metrics of the
	// internal port only (the public entry is the gateway, design/60).
	obs := observe.New()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := db.Connect(ctx, cfg.Postgres)
	if err != nil {
		fatal(err)
	}
	defer pool.Close()

	rdb := redis.NewClient(&redis.Options{Addr: cfg.Valkey.Addr, Username: cfg.Valkey.Username, Password: cfg.Valkey.Password, DB: cfg.Valkey.DB})
	defer rdb.Close()

	// --- device auth (SEC-03) ---
	kek, err := auth.LoadKEK()
	if err != nil {
		fatal(fmt.Errorf("ADC_DEVICE_KEK required (64 hex chars): %w", err))
	}
	credRepo := auth.NewCredentialRepo(pool.Pool, kek)
	nonces := auth.NewValkeyNonceStore(&redis.Options{Addr: cfg.Valkey.Addr, Username: cfg.Valkey.Username, Password: cfg.Valkey.Password, DB: cfg.Valkey.DB})
	verifier := auth.NewVerifier(credRepo, nonces)

	if seed {
		demo, serr := seedDemo(ctx, seedDemoInput{
			dbx:     pool,
			kek:     kek,
			keys:    adminapi.NewPGApiKeyRepo(pool.Pool),
			tickets: approval.NewPGTicketRepo(pool.Pool),
			sink:    audit.NewValkeySink(rdb),
		})
		if serr != nil {
			fatal(serr)
		}
		log.Info("seeded demo tenant/devices/tools", "tenant", "tenant-demo",
			"devices", "cnc-demo-01,agv-demo-01",
			"tools", "set_spindle_speed,get_spindle_status,move_to,get_position")
		log.Info("seeded demo device secrets", "cnc-demo-01", demo.DeviceSecret, "agv-demo-01", demo.AGVSecret)
		if demo.APIKey != "" {
			log.Info("seeded demo agent api key (plaintext shown once)", "name", "demo-agent",
				"key", demo.APIKey, "key_id", demo.APIKeyID)
		} else {
			log.Info("demo agent api key already present, keeping the existing key", "name", "demo-agent")
		}
		log.Info("seeded demo approval ticket", "ticket_id", demo.TicketID,
			"status", "PENDING", "tool", "set_spindle_speed")
		log.Info("seeded demo audit events", "event_ids", demo.AuditEventIDs)
		log.Info("seeded demo admin account", "username", "admin", "password", demoAdminPassword,
			"note", "dev default, change it before any shared environment")
		_ = os.WriteFile("/tmp/adc-demo-secret", []byte(demo.DeviceSecret), 0o600)
		_ = os.WriteFile("/tmp/adc-demo-agv-secret", []byte(demo.AGVSecret), 0o600)
	}
	var tenantID string
	if err := pool.QueryRow(ctx, `SELECT id::text FROM adc_tenants WHERE code='tenant-demo'`).Scan(&tenantID); err != nil {
		fatal(fmt.Errorf("load demo tenant: %w", err))
	}

	// --- connector (WSS tunnel, SEC-04/14/15) ---
	hub := connector.NewDeviceHub()
	registry := connector.NewValkeyRegistry(rdb, time.Duration(cfg.DeviceTTLSeconds)*time.Second, log)
	// Observing registry seam (B-09): device online/offline events update
	// adc_device_online_total{tenant} via Register/Unregister; a periodic
	// SCARD reconcile heals drift (restarts, failed unregisters).
	obsRegistry := &observingRegistry{inner: registry, obs: obs}
	tunnel := connector.NewTunnelServer(connector.TunnelServerConfig{
		Auth:      deviceAuthAdapter{v: verifier},
		Hub:       hub,
		Registry:  obsRegistry,
		NodeID:    cfg.NodeID,
		DeviceTTL: time.Duration(cfg.DeviceTTLSeconds) * time.Second,
		Logger:    log,
	})

	// --- agent api (SEC-02/09) ---
	policy := agentapi.DBPolicy{Lookup: riskLookup(pool)}
	agg := agentapi.DBAggregator{Rows: toolRows(pool), Lookup: riskLookup(pool), UUIDLookup: deviceUUIDLookup(pool), OwnerCheck: ownerCheck(pool)}
	localRouter := agentapi.LocalRouter{Session: func(ctx context.Context, tenantID, deviceID string) (agentapi.RPCClient, error) {
		sess, err := hub.GetDevice(tenantID, deviceID)
		if err != nil {
			return nil, err
		}
		return sess, nil
	}}
	// --- cluster routing (design/80 B-08): local fast path + signed
	// cross-node Request over the MessageBus (SEC-06). ADC_NODE_KEY is the
	// shared cluster signing key; outside dev it must be injected (SEC-13).
	nodeKey, err := loadNodeKey(cfg)
	if err != nil {
		fatal(err)
	}
	bus := clusterbus.NewBus(cfg.NodeID, nodeKey, rdb)
	router := &route.ClusterRouter{
		Local:       localRouter,
		Locator:     clusterbus.NewNodeRegistry(rdb),
		Bus:         bus,
		NodeID:      cfg.NodeID,
		CallTimeout: time.Duration(cfg.ToolCallTimeoutSec) * time.Second,
	}
	go func() { _ = router.Serve(ctx) }() // answer remote tool calls
	// --- metering (design/80 B-11): TOOL_CALL events flow through the
	// router decorator into the async PG pipeline (design/32 3.11).
	meter := metering.NewValkeyMeter(rdb)
	routerWithMeter := agentapi.MeteringRouter{Next: router, Meter: meter}
	go metering.NewValkeyWorker(rdb, pool.Pool).Run(ctx)
	// --- hitl (production wiring, design/80 B-07) ---
	// The approval state machine lives in PG (ADR-05); the wake bus is the
	// unified Valkey channel (SEC-10); approvers are reached via the signed
	// WeCom/DingTalk cards (SEC-13/18). The formal /v1/hitl/* endpoints are
	// always registered; /dev/decide exists only in dev (SEC-01).
	if cfg.HITLCallbackKey == "" && cfg.Env != "dev" {
		fatal(fmt.Errorf("ADC_HITL_CALLBACK_KEY required outside dev (SEC-13)"))
	}
	ticketRepo := approval.NewPGTicketRepo(pool.Pool)
	wakeBus := approval.NewValkeyEventBus(rdb)
	var cardChannels []approval.Notifier
	if cfg.WeComWebhookURL != "" {
		cardChannels = append(cardChannels, &observingNotifier{
			inner:   approval.NewWeComNotifier(cfg.WeComWebhookURL),
			channel: "wecom",
			obs:     obs,
		})
	}
	if cfg.DingTalkWebhook != "" {
		cardChannels = append(cardChannels, &observingNotifier{
			inner:   approval.NewDingTalkNotifier(cfg.DingTalkWebhook),
			channel: "dingtalk",
			obs:     obs,
		})
	}
	var cardNotifier approval.Notifier
	if len(cardChannels) > 0 {
		cardNotifier = approval.NewRetryNotifier(approval.NewMultiNotifier(cardChannels...))
	}
	hitl := agentapi.NewApprovalHITLClient(agentapi.ApprovalHITLConfig{
		Repo:        ticketRepo,
		Bus:         wakeBus,
		Notify:      cardNotifier,
		BaseURL:     cfg.PublicURL,
		CallbackKey: cfg.HITLCallbackKey,
		TicketTTL:   time.Duration(cfg.HITLTimeoutSec) * time.Second,
		UUIDLookup:  deviceUUIDLookup(pool),
	})
	defer hitl.Close()
	// Observing seams (B-09): tool call outcomes and durations are recorded
	// by wrapping the router (success/failed + duration) and the HITL client
	// (intercepted/approved/rejected); wrappers no-op when obs is nil. The
	// metering seam (B-11) sits inside the observing wrapper so both
	// cross-cutting concerns see every executed call.
	agentSrv := agentapi.NewServer(authValidator{tenantID: tenantID}, agg,
		&observingRouter{inner: routerWithMeter, obs: obs}, policy,
		&observingHITL{inner: hitl, obs: obs})
	agentSrv.Audit = audit.NewValkeySink(rdb)

	// --- audit pipeline (SEC-07) ---
	auditWorker := audit.NewValkeyWorker(rdb, pool.Pool)
	go auditWorker.Run(ctx)

	callbackHandler := approval.NewCallbackHandler(ticketRepo, wakeBus, cfg.HITLCallbackKey)

	// --- admin api + session auth (design/80 B-01..B-06): production
	// wiring with the PG repos and the Valkey session store. The
	// dev authValidator above is Agent-side only and never guards the
	// Admin surface; admins authenticate through adminauth against
	// adc_users (seeded by -seed).
	sessions := adminauth.NewValkeySessionStore(rdb)
	authHandler := adminauth.NewHandler(adminauth.NewPGUserStore(pool.Pool), sessions)
	adminSrv := adminapi.NewServer(
		adminapi.NewPGTenantRepo(pool.Pool),
		adminapi.NewPGDeviceRepo(pool.Pool),
		adminapi.NewPGApiKeyRepo(pool.Pool),
		sessions)
	adminSrv.KEK = kek
	adminSrv.Tools = adminapi.NewPGToolRepo(pool.Pool)
	adminSrv.Policies = adminapi.NewPGPolicyRepo(pool.Pool)
	adminSrv.AuditQuery = adminapi.NewPGAuditQueryRepo(pool.Pool)
	adminSrv.Tickets = adminapi.NewPGTicketsRepo(pool.Pool)
	adminSrv.Audit = &adminAuditSink{inner: audit.NewValkeySink(rdb)}
	adminHandler := adminSrv.Handler()

	// --- agent api quota + rate limit wiring (design/80 B-10, SEC-12) ---
	// The Agent API routes get a sub-mux so the quota gate and rate
	// limiter wrap only agent endpoints. Order per design/31 3.2.3:
	// 限流 (429) then 认证 (401, inside Routes) then 配额 (403, 13003);
	// tenant/agent identity only exists after auth, so both middlewares
	// wrap the authenticated sub-mux.
	quotaGate := agentapi.PGQuotaGate{Lookup: quotaLookup(pool)}
	limiter, err := ratelimit.NewTokenBucket(ratelimit.NewValkeyStore(rdb), nil)
	if err != nil {
		fatal(err)
	}
	agentMux := http.NewServeMux()
	agentSrv.Routes(agentMux)
	agentChain := ratelimit.Middleware(limiter,
		principalKey(ratelimit.ScopeTenant),
		principalKey(ratelimit.ScopeAgent),
	)(agentapi.QuotaMiddleware(quotaGate)(agentMux))

	mux := http.NewServeMux()
	mux.Handle("/v1/admin/", combinedAdminHandler(authHandler.Routes(), adminHandler))
	mux.Handle("/v1/agent/mcp/", agentChain)
	callbackHandler.Routes(mux)
	mux.Handle("GET /v1/devices/tunnel", tunnel)
	// Metrics on the internal port only (B-09): the adc service exposes no
	// public ports (design/60 3.2); the gateway serves its own instance.
	mux.Handle("GET /metrics", obs.Handler)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"status":"ok"}`)
	})
	// Root index: dev-friendly endpoint listing for browser visits.
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		items := []string{
			`<li><a href="/healthz">/healthz</a> 健康检查</li>`,
			`<li>POST /v1/admin/auth/login（控制台管理员登录，默认账号 admin）</li>`,
			`<li>GET /v1/agent/mcp/tools（需 X-ADC-Key 头）</li>`,
			`<li>POST /v1/agent/mcp/tools/call（需 X-ADC-Key 头）</li>`,
			`<li>GET /v1/devices/tunnel（设备 WSS 隧道）</li>`,
			`<li>POST /v1/hitl/callback（HITL 签名回调）</li>`,
			`<li>GET /v1/hitl/action（审批落地页）</li>`,
			`<li>GET /metrics（Prometheus 指标，仅内网）</li>`,
		}
		if cfg.Env == "dev" {
			items = append(items, `<li>POST /dev/decide（dev 审批决策注入）</li>`)
		}
		fmt.Fprint(w, `<html><head><title>ADC Dev Server</title></head><body>
<h1>ADC Dev Server (Go 数据面)</h1>
<p>此端口为内部服务端口；业务统一入口是网关端口 18080。</p>
<ul>
`+strings.Join(items, "\n")+`
</ul></body></html>`)
	})
	// Dev-only decision injection: bypasses the card signature and drives
	// the same repo CAS + wake bus as the production callback, so dev smoke
	// tests exercise the production decision path (SEC-01: not registered
	// outside ADC_ENV=dev).
	if cfg.Env == "dev" {
		mux.HandleFunc("POST /dev/decide", func(w http.ResponseWriter, r *http.Request) {
			var req struct {
				TicketID string `json:"ticket_id"`
				Decision string `json:"decision"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, "bad json", http.StatusBadRequest)
				return
			}
			status, ok := approval.DecisionStatus(req.Decision)
			if !ok {
				http.Error(w, "decision must be approve or reject", http.StatusBadRequest)
				return
			}
			t, err := ticketRepo.Get(r.Context(), req.TicketID)
			if err != nil {
				http.Error(w, "ticket not found", http.StatusNotFound)
				return
			}
			updated, err := ticketRepo.Transition(r.Context(), req.TicketID, t.Version, approval.TransitionCmd{Status: status, Approver: "dev-script"})
			if err != nil {
				http.Error(w, err.Error(), http.StatusConflict)
				return
			}
			_ = wakeBus.PublishResolved(r.Context(), updated.TicketID, updated.Status)
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, `{"ok":true}`)
		})
	}

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           observe.Middleware(obs.HTTPRequests, obs.HTTPRequestDuration, nil)(mux),
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Background collectors (B-09): PG pool watermark for the P2 alert and
	// device-online reconciliation from the Valkey online set (layout per
	// connector/registry.go, design/31 3.1.7).
	go collectPGPool(ctx, pool, obs)
	go collectDeviceOnline(ctx, rdb, tenantID, obs)

	go func() {
		log.Info("adc dev server listening", "addr", cfg.HTTPAddr, "tls", cfg.TLSEnable)
		var err error
		if cfg.TLSEnable {
			// SEC-05: TLS termination with injected certificate/key (config).
			err = srv.ListenAndServeTLS(cfg.TLSCertFile, cfg.TLSKeyFile)
		} else {
			err = srv.ListenAndServe()
		}
		if err != nil && err != http.ErrServerClosed {
			fatal(err)
		}
	}()

	<-ctx.Done()
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	hub.CloseAll()
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "fatal:", err)
	os.Exit(1)
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

// adminAuditSink adapts adminapi.AuditSink onto the pkg/audit Valkey
// pipeline (SEC-07). Emission is best-effort by contract: the pkg/audit
// worker currently stamps tool_call/agent at insert time (worker.go
// insertSQL), so admin ops do not yet carry the admin_op shape on
// adc_audit_logs; wiring the sink keeps the seam active until the audit
// package grows an admin-shaped event (see the AuditSink note in
// internal/adminapi/server.go).
type adminAuditSink struct {
	inner *audit.ValkeySink
}

func (a *adminAuditSink) Record(ctx context.Context, op adminapi.AdminOp) error {
	if a == nil || a.inner == nil {
		return nil
	}
	return a.inner.Enqueue(ctx, &audit.AuditEvent{
		EventID:   op.EventID,
		TenantID:  op.TenantID,
		AgentID:   op.ActorID,
		Params:    op.Details,
		Status:    audit.StatusSuccess,
		TraceID:   op.TraceID,
		CreatedAt: op.CreatedAt,
	})
}

// loadNodeKey returns the cluster node signing key (SEC-06). Production
// and staging require ADC_NODE_KEY (hex, SEC-13); dev falls back to a
// fixed key derived from a constant so local smoke runs zero-config.
func loadNodeKey(cfg *config.Config) ([]byte, error) {
	hexKey := cfg.NodeKey
	if hexKey == "" {
		if cfg.Env != "dev" {
			return nil, fmt.Errorf("ADC_NODE_KEY required outside dev (SEC-06 cluster signing key)")
		}
		sum := sha256.Sum256([]byte("adc-dev-node-key"))
		return sum[:], nil
	}
	key, err := hex.DecodeString(strings.TrimSpace(hexKey))
	if err != nil {
		return nil, fmt.Errorf("ADC_NODE_KEY must be hex encoded: %w", err)
	}
	if len(key) < 16 {
		return nil, fmt.Errorf("ADC_NODE_KEY must be at least 16 bytes (SEC-06)")
	}
	return key, nil
}

// quotaLookup reads the authoritative quota ledger row for the tenant
// (design/32 6.2: used_calls_month vs quota_calls_monthly).
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

// principalKey builds a rate limit KeyFunc from the authenticated
// principal (tenant / agent dimensions, SEC-12). Requests without a
// principal yield an empty key, which the middleware skips; auth rejects
// them downstream.
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

// observingRegistry wraps the connector SessionRegistry seam (B-09): device
// online/offline events bump adc_device_online_total{tenant}. Delegate
// failures never change the gauge (the periodic reconcile heals drift).
type observingRegistry struct {
	inner connector.SessionRegistry
	obs   *observe.Set
}

func (o *observingRegistry) Register(ctx context.Context, tenantID, deviceCode, nodeID string, ttl time.Duration) error {
	if err := o.inner.Register(ctx, tenantID, deviceCode, nodeID, ttl); err != nil {
		return err
	}
	if o.obs != nil {
		o.obs.DeviceOnline.With(tenantID).Add(1)
	}
	return nil
}

func (o *observingRegistry) Heartbeat(ctx context.Context, tenantID, deviceCode string) error {
	return o.inner.Heartbeat(ctx, tenantID, deviceCode)
}

func (o *observingRegistry) Unregister(ctx context.Context, tenantID, deviceCode string) error {
	if err := o.inner.Unregister(ctx, tenantID, deviceCode); err != nil {
		return err
	}
	if o.obs != nil {
		o.obs.DeviceOnline.With(tenantID).Add(-1)
	}
	return nil
}

func (o *observingRegistry) OnlineSet(ctx context.Context, tenantID, deviceCode string, member bool) error {
	return o.inner.OnlineSet(ctx, tenantID, deviceCode, member)
}

// observingRouter wraps the agentapi ToolRouter seam (B-09): every executed
// tool call is counted per tenant/status and observed into the duration
// histogram. A nil obs makes the wrapper a pure pass-through (tests).
type observingRouter struct {
	inner agentapi.ToolRouter
	obs   *observe.Set
}

func (r *observingRouter) Call(ctx context.Context, tenantID string, ref agentapi.ToolRef, args map[string]interface{}) (*agentapi.CallResult, error) {
	start := time.Now()
	res, err := r.inner.Call(ctx, tenantID, ref, args)
	if r.obs != nil {
		r.obs.ToolCallDuration.With().Observe(time.Since(start).Seconds())
		if err != nil {
			r.obs.AgentCalls.With(tenantID, "failed").Inc()
		} else {
			r.obs.AgentCalls.With(tenantID, "success").Inc()
		}
	}
	return res, err
}

// observingHITL wraps the agentapi HITLClient seam (B-09): ticket creation
// counts as intercepted; decisions feed approved/rejected (expiry counts as
// rejected, matching the fail-closed semantics of SEC-11).
type observingHITL struct {
	inner agentapi.HITLClient
	obs   *observe.Set
}

func (o *observingHITL) CreateTicket(ctx context.Context, req *agentapi.TicketRequest) (*agentapi.TicketRef, error) {
	ref, err := o.inner.CreateTicket(ctx, req)
	if err == nil && o.obs != nil {
		o.obs.HITLIntercepted.With().Inc()
		o.obs.AgentCalls.With(req.TenantID, "intercepted").Inc()
	}
	return ref, err
}

func (o *observingHITL) AwaitDecision(ctx context.Context, ticketID string, timeout time.Duration) (*agentapi.TicketDecision, error) {
	d, err := o.inner.AwaitDecision(ctx, ticketID, timeout)
	if d != nil && o.obs != nil {
		switch d.Status {
		case agentapi.DecisionApproved:
			o.obs.HITLApproved.With().Inc()
		case agentapi.DecisionRejected, agentapi.DecisionExpired:
			o.obs.HITLRejected.With().Inc()
		}
	}
	return d, err
}

// observingNotifier wraps an approval card channel (B-09): delivery failures
// feed adc_hitl_notify_failures_total{channel}, the input of the P1 push
// failure alert (deploy/prometheus/alert-rules.yml).
type observingNotifier struct {
	inner   approval.Notifier
	channel string
	obs     *observe.Set
}

func (n *observingNotifier) SendApprovalCard(ctx context.Context, ticket *approval.ApprovalTicket, approveURL, rejectURL string) error {
	err := n.inner.SendApprovalCard(ctx, ticket, approveURL, rejectURL)
	if err != nil && n.obs != nil {
		n.obs.NotifyFailures.With(n.channel).Inc()
	}
	return err
}

// collectPGPool feeds the P2 pool-watermark gauge (design/60 6.3) from the
// pgx pool stats every 5s.
func collectPGPool(ctx context.Context, pool *db.Pool, obs *observe.Set) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			st := pool.Stat()
			obs.PGPoolConnections.With("acquired").Set(float64(st.AcquiredConns()))
			obs.PGPoolConnections.With("max").Set(float64(st.MaxConns()))
		}
	}
}

// collectDeviceOnline reconciles adc_device_online_total{tenant} from the
// Valkey online set every 30s (key layout per connector/registry.go,
// design/31 3.1.7). Event-driven inc/dec via observingRegistry keeps the
// gauge live between ticks; this pass heals drift after restarts or failed
// unregisters.
func collectDeviceOnline(ctx context.Context, rdb *redis.Client, tenantID string, obs *observe.Set) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n, err := rdb.SCard(ctx, "adc:tenant_devices:"+tenantID).Result()
			if err != nil {
				continue
			}
			obs.DeviceOnline.With(tenantID).Set(float64(n))
		}
	}
}

// deviceAuthAdapter adapts the auth.Verifier (SEC-03) to the connector
// DeviceAuth seam. The tenant is taken from the credential record.
type deviceAuthAdapter struct{ v *auth.Verifier }

func (a deviceAuthAdapter) Authenticate(ctx context.Context, hs *connector.Handshake) (*connector.DeviceIdentity, error) {
	cred, err := a.v.Verify(ctx, hs.DeviceID, hs.Timestamp, hs.Nonce, hs.Signature)
	if err != nil {
		return nil, err
	}
	return &connector.DeviceIdentity{TenantID: cred.TenantID, DeviceCode: cred.DeviceCode}, nil
}

// authValidator is the dev ApiKeyValidator: single fixed key for local smoke.
type authValidator struct{ tenantID string }

func (a authValidator) Validate(ctx context.Context, token string) (*agentauth.Principal, error) {
	if token == "dev-agent-key" {
		return &agentauth.Principal{KeyID: "dev-k1", TenantID: a.tenantID, AgentID: "dev-agent", Scopes: []string{"tools.list", "tools.call"}}, nil
	}
	return nil, agentauth.ErrKeyNotFound
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
// (design/33 13007, SEC-02): a device not present in the caller's tenant
// ledger yields agentapi.ErrDeviceNotOwned, which the Agent API maps to an
// explicit 403 instead of a generic 500.
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
	return func(ctx context.Context, tenantID, deviceID, toolName string) (int, error) {
		var level int
		err := pool.QueryRow(ctx,
			`SELECT t.risk_level FROM adc_device_tools t
			 JOIN adc_devices d ON d.id = t.device_id
			 WHERE d.tenant_id=$1::uuid AND d.device_code=$2 AND t.tool_name=$3`,
			tenantID, deviceID, toolName).Scan(&level)
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
			slog.Error("toolRows query", "err", err)
			return nil, err
		}
		defer rows.Close()
		var out []agentapi.ToolRow
		for rows.Next() {
			var r agentapi.ToolRow
			var schema string
			if err := rows.Scan(&r.DeviceCode, &r.ToolName, &r.Description, &schema, &r.RiskLevel); err != nil {
				slog.Error("toolRows scan", "err", err)
				return nil, err
			}
			r.InputSchema = json.RawMessage(schema)
			out = append(out, r)
		}
		if err := rows.Err(); err != nil {
			slog.Error("toolRows rows", "err", err)
			return nil, err
		}
		return out, nil
	}
}

// demoDB is the minimal PostgreSQL surface seedDemo needs. *db.Pool
// (pgxpool) satisfies it; unit tests inject pgxmock-backed fakes.
type demoDB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// demoSeed carries the one-time plaintexts and identifiers produced by
// seedDemo for the startup summary log. The API key plaintext exists only
// in this struct and the log line (NFR-004: only hashes are persisted).
type demoSeed struct {
	DeviceSecret  string
	AGVSecret     string
	APIKey        string // plaintext, empty when the key already existed
	APIKeyID      string
	TicketID      string
	AuditEventIDs []string
}

// seedDemoInput bundles the repository seams seedDemo writes through. The
// demo API key is issued through the production adminapi.ApiKeyRepo, the
// demo approval ticket through the production approval.TicketRepo, and the
// demo audit events flow through the production audit.AuditSink (Valkey
// queue -> worker -> adc_audit_logs), so seeding exercises exactly the
// paths production data takes. Every step is idempotent; repeated -seed
// runs keep the demo dataset stable (fixed event ids and ticket args are
// the dedup keys).
type seedDemoInput struct {
	dbx     demoDB
	kek     []byte
	keys    adminapi.ApiKeyRepo
	tickets approval.TicketRepo
	sink    audit.AuditSink // nil in unit tests: audit events are skipped
}

// seedDemo inserts the demo tenant, the two demo devices (cnc-demo-01 with
// set_spindle_speed risk 2 and read-only get_spindle_status risk 0;
// agv-demo-01 with move_to risk 2 and read-only get_position risk 0), the
// admin bootstrap (system roles + default admin), one demo Agent API key
// (name demo-agent), one PENDING approval ticket and three sample audit
// events (success / blocked_by_hitl / failed).
func seedDemo(ctx context.Context, in seedDemoInput) (*demoSeed, error) {
	dbx, kek := in.dbx, in.kek
	if _, err := dbx.Exec(ctx, `INSERT INTO adc_tenants (id, code, name, status)
		SELECT gen_random_uuid(), 'tenant-demo', 'Demo', 'ACTIVE'
		WHERE NOT EXISTS (SELECT 1 FROM adc_tenants WHERE code='tenant-demo')`); err != nil {
		return nil, err
	}
	var tenantID string
	if err := dbx.QueryRow(ctx, `SELECT id::text FROM adc_tenants WHERE code='tenant-demo'`).Scan(&tenantID); err != nil {
		return nil, fmt.Errorf("seed: load demo tenant: %w", err)
	}
	out := &demoSeed{}

	// --- cnc-demo-01 (existing demo device) ---
	secret, err := auth.GenerateSecret()
	if err != nil {
		return nil, err
	}
	stored, err := auth.EncryptSecret(secret, kek)
	if err != nil {
		return nil, err
	}
	// Dev idempotency: update the existing demo device's credential with the
	// freshly generated secret (the plaintext is only known to this run),
	// then insert only when absent.
	if _, err := dbx.Exec(ctx, `UPDATE adc_devices SET credential_hash=$1, auth_type='hmac', device_class='B'
		WHERE device_code='cnc-demo-01'`, stored); err != nil {
		return nil, err
	}
	if _, err := dbx.Exec(ctx, `INSERT INTO adc_devices (id, tenant_id, device_code, name, device_type, device_class, auth_type, credential_hash, status)
		SELECT gen_random_uuid(), id, 'cnc-demo-01', 'Demo CNC', 'cnc', 'B', 'hmac', $1, 'OFFLINE'
		FROM adc_tenants WHERE code='tenant-demo'
		AND NOT EXISTS (SELECT 1 FROM adc_devices WHERE device_code='cnc-demo-01')`, stored); err != nil {
		return nil, err
	}
	out.DeviceSecret = secret
	if _, err := dbx.Exec(ctx, `INSERT INTO adc_device_tools (id, tenant_id, device_id, tool_name, description, input_schema, risk_level, is_enabled)
		SELECT gen_random_uuid(), d.tenant_id, d.id, 'set_spindle_speed', 'Set CNC spindle RPM (high risk)', '{"type":"object","properties":{"rpm":{"type":"number"}}}', 2, true
		FROM adc_devices d WHERE d.device_code='cnc-demo-01'
		AND NOT EXISTS (SELECT 1 FROM adc_device_tools t WHERE t.device_id=d.id AND t.tool_name='set_spindle_speed')`); err != nil {
		return nil, err
	}
	if _, err := dbx.Exec(ctx, `INSERT INTO adc_device_tools (id, tenant_id, device_id, tool_name, description, input_schema, risk_level, is_enabled)
		SELECT gen_random_uuid(), d.tenant_id, d.id, 'get_spindle_status', 'Read CNC spindle RPM (read-only)', '{"type":"object"}', 0, true
		FROM adc_devices d WHERE d.device_code='cnc-demo-01'
		AND NOT EXISTS (SELECT 1 FROM adc_device_tools t WHERE t.device_id=d.id AND t.tool_name='get_spindle_status')`); err != nil {
		return nil, err
	}

	// --- agv-demo-01 (new demo device: AGV with one HITL move tool) ---
	agvSecret, err := auth.GenerateSecret()
	if err != nil {
		return nil, err
	}
	agvStored, err := auth.EncryptSecret(agvSecret, kek)
	if err != nil {
		return nil, err
	}
	if _, err := dbx.Exec(ctx, `UPDATE adc_devices SET credential_hash=$1, auth_type='hmac', device_class='B'
		WHERE device_code='agv-demo-01'`, agvStored); err != nil {
		return nil, err
	}
	if _, err := dbx.Exec(ctx, `INSERT INTO adc_devices (id, tenant_id, device_code, name, device_type, device_class, auth_type, credential_hash, status)
		SELECT gen_random_uuid(), id, 'agv-demo-01', 'Demo AGV', 'agv', 'B', 'hmac', $1, 'OFFLINE'
		FROM adc_tenants WHERE code='tenant-demo'
		AND NOT EXISTS (SELECT 1 FROM adc_devices WHERE device_code='agv-demo-01')`, agvStored); err != nil {
		return nil, err
	}
	out.AGVSecret = agvSecret
	if _, err := dbx.Exec(ctx, `INSERT INTO adc_device_tools (id, tenant_id, device_id, tool_name, description, input_schema, risk_level, is_enabled)
		SELECT gen_random_uuid(), d.tenant_id, d.id, 'move_to', 'Move AGV to a warehouse location (high risk)', '{"type":"object","properties":{"location":{"type":"string"}},"required":["location"]}', 2, true
		FROM adc_devices d WHERE d.device_code='agv-demo-01'
		AND NOT EXISTS (SELECT 1 FROM adc_device_tools t WHERE t.device_id=d.id AND t.tool_name='move_to')`); err != nil {
		return nil, err
	}
	if _, err := dbx.Exec(ctx, `INSERT INTO adc_device_tools (id, tenant_id, device_id, tool_name, description, input_schema, risk_level, is_enabled)
		SELECT gen_random_uuid(), d.tenant_id, d.id, 'get_position', 'Read AGV position (read-only)', '{"type":"object"}', 0, true
		FROM adc_devices d WHERE d.device_code='agv-demo-01'
		AND NOT EXISTS (SELECT 1 FROM adc_device_tools t WHERE t.device_id=d.id AND t.tool_name='get_position')`); err != nil {
		return nil, err
	}

	// --- admin bootstrap (design/80 B-01): system roles + default admin ---
	// Role codes keep the design/32 uppercase spelling; PGUserStore
	// lowercases them on load because the RBAC matrix matches the
	// design/33 3.1.18 lowercase names. All steps are idempotent.
	roleSteps := []string{
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
	}
	for _, q := range roleSteps {
		if _, err := dbx.Exec(ctx, q); err != nil {
			return nil, err
		}
	}
	passHash, err := adminauth.HashPassword(demoAdminPassword)
	if err != nil {
		return nil, err
	}
	// Insert the admin user when absent, then force the documented dev
	// password on every seed run so the console login always works out
	// of the box.
	if _, err := dbx.Exec(ctx, `INSERT INTO adc_users (tenant_id, username, display_name, password_hash, auth_source, status)
		SELECT id, 'admin', 'Platform Admin', $1, 'LOCAL', 'ACTIVE'
		FROM adc_tenants WHERE code='tenant-demo'
		ON CONFLICT DO NOTHING`, passHash); err != nil {
		return nil, err
	}
	if _, err := dbx.Exec(ctx, `UPDATE adc_users
		SET password_hash=$1, status='ACTIVE', updated_at=now()
		WHERE username='admin' AND deleted_at IS NULL`, passHash); err != nil {
		return nil, err
	}
	if _, err := dbx.Exec(ctx, `INSERT INTO adc_user_roles (user_id, role_id, tenant_id)
		SELECT u.id, r.id, u.tenant_id FROM adc_users u
		JOIN adc_roles r ON r.role_code='PLATFORM_ADMIN' AND r.scope='PLATFORM'
		WHERE u.username='admin'
		ON CONFLICT DO NOTHING`); err != nil {
		return nil, err
	}

	// --- tenant-level demo accounts (A1.1): tenant-admin and approver for
	// the demo tenant, so the console can be exercised with tenant-scoped
	// roles out of the box. Same idempotent pattern as the admin account.
	demoUsers := []struct {
		username, display, password, role string
	}{
		{"tenant-admin", "Tenant Admin", "tenant123!", "TENANT_ADMIN"},
		{"approver", "Demo Approver", "approver123!", "APPROVER"},
	}
	for _, du := range demoUsers {
		hash, err := adminauth.HashPassword(du.password)
		if err != nil {
			return nil, err
		}
		if _, err := dbx.Exec(ctx, `INSERT INTO adc_users (tenant_id, username, display_name, password_hash, auth_source, status)
			SELECT id, $1, $2, $3, 'LOCAL', 'ACTIVE'
			FROM adc_tenants WHERE code='tenant-demo'
			ON CONFLICT DO NOTHING`, du.username, du.display, hash); err != nil {
			return nil, err
		}
		if _, err := dbx.Exec(ctx, `UPDATE adc_users
			SET password_hash=$1, status='ACTIVE', updated_at=now()
			WHERE username=$2 AND deleted_at IS NULL`, hash, du.username); err != nil {
			return nil, err
		}
		if _, err := dbx.Exec(ctx, `INSERT INTO adc_user_roles (user_id, role_id, tenant_id)
			SELECT u.id, r.id, u.tenant_id FROM adc_users u
			JOIN adc_roles r ON r.role_code=$1 AND r.tenant_id = u.tenant_id
			WHERE u.username=$2
			ON CONFLICT DO NOTHING`, du.role, du.username); err != nil {
			return nil, err
		}
		slog.Info("seeded demo tenant account", "username", du.username, "password", du.password, "role", du.role)
	}

	// --- demo Agent API key (name demo-agent). Issued through the
	// production adminapi repo so the stored hashes match agentauth's
	// verification exactly; the plaintext is logged once below. The
	// existence check keeps repeated seeds idempotent (the plaintext of a
	// previously issued key is unknowable by design). created_by carries
	// the demo admin's user id (adc_agent_api_keys.created_by is a uuid).
	var keyExists bool
	if err := dbx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM adc_agent_api_keys
		WHERE tenant_id=$1::uuid AND name='demo-agent' AND revoked_at IS NULL)`, tenantID).Scan(&keyExists); err != nil {
		return nil, fmt.Errorf("seed: probe demo agent key: %w", err)
	}
	if !keyExists {
		var adminID string
		if err := dbx.QueryRow(ctx, `SELECT id::text FROM adc_users
			WHERE username='admin' AND deleted_at IS NULL`).Scan(&adminID); err != nil {
			return nil, fmt.Errorf("seed: load demo admin user: %w", err)
		}
		keyID, prefix, keySecret, keyHash, secretHash, err := generateDemoKeyMaterial()
		if err != nil {
			return nil, err
		}
		created, err := in.keys.Issue(ctx, &adminapi.NewKey{
			TenantID:   tenantID,
			Name:       "demo-agent",
			AgentID:    "demo-agent",
			KeyPrefix:  prefix,
			KeyHash:    keyHash,
			SecretHash: secretHash,
			Scopes:     []string{"*"},
			CreatedBy:  adminID,
		})
		if err != nil {
			return nil, fmt.Errorf("seed: issue demo agent key: %w", err)
		}
		out.APIKey = "adc_" + keyID + "_" + keySecret
		out.APIKeyID = created.ID
	}

	// --- demo approval ticket (PENDING), created through the production
	// approval repo. The fixed tool+arguments pair is the dedup key
	// (uq_tickets_pending_dedup): a repeat seed reuses the existing
	// in-flight ticket instead of flooding the queue.
	var cncUUID, agvUUID string
	if err := dbx.QueryRow(ctx, `SELECT id::text FROM adc_devices WHERE device_code='cnc-demo-01'`).Scan(&cncUUID); err != nil {
		return nil, fmt.Errorf("seed: load demo cnc device: %w", err)
	}
	if err := dbx.QueryRow(ctx, `SELECT id::text FROM adc_devices WHERE device_code='agv-demo-01'`).Scan(&agvUUID); err != nil {
		return nil, fmt.Errorf("seed: load demo agv device: %w", err)
	}
	demoTicket, err := approval.NewPending(tenantID, "demo-agent", cncUUID, "set_spindle_speed",
		json.RawMessage(`{"rpm": 8000}`), approval.RiskHigh, time.Time{})
	if err != nil {
		return nil, fmt.Errorf("seed: build demo ticket: %w", err)
	}
	createdTicket, err := in.tickets.Create(ctx, demoTicket)
	if errors.Is(err, approval.ErrTicketDedup) && createdTicket != nil {
		out.TicketID = createdTicket.TicketID
	} else if err != nil {
		return nil, fmt.Errorf("seed: create demo ticket: %w", err)
	} else {
		out.TicketID = createdTicket.TicketID
	}

	// --- demo audit events, flowing through the production pipeline
	// (Valkey queue -> worker -> adc_audit_logs). Fixed UUID-shaped event
	// ids (adc_audit_logs.request_id is a uuid column) plus an existence
	// probe per event keep repeated seeds idempotent; enqueue failures are
	// warnings by contract (design/31 1.3.7 best-effort) and never abort
	// startup.
	demoEvents := []*audit.AuditEvent{
		{
			EventID:   "d3e00000-0000-4000-8000-000000000001",
			TenantID:  tenantID,
			AgentID:   "demo-agent",
			DeviceID:  agvUUID,
			ToolName:  "get_position",
			Params:    map[string]any{"x": float64(1), "y": float64(2)},
			Result:    map[string]any{"x": float64(1), "y": float64(2)},
			Status:    audit.StatusSuccess,
			RiskLevel: approval.RiskRead,
		},
		{
			EventID:   "d3e00000-0000-4000-8000-000000000002",
			TenantID:  tenantID,
			AgentID:   "demo-agent",
			DeviceID:  cncUUID,
			ToolName:  "set_spindle_speed",
			Params:    map[string]any{"rpm": float64(9000)},
			Status:    audit.StatusBlockedByHITL,
			RiskLevel: approval.RiskHigh,
		},
		{
			EventID:   "d3e00000-0000-4000-8000-000000000003",
			TenantID:  tenantID,
			AgentID:   "demo-agent",
			DeviceID:  cncUUID,
			ToolName:  "set_spindle_speed",
			Params:    map[string]any{"rpm": float64(12000)},
			Result:    map[string]any{"error": "spindle overload"},
			Status:    audit.StatusFailed,
			RiskLevel: approval.RiskHigh,
		},
	}
	for _, ev := range demoEvents {
		var exists bool
		if err := dbx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM adc_audit_logs WHERE request_id=$1::uuid)`, ev.EventID).Scan(&exists); err != nil {
			slog.Warn("seed: demo audit probe failed, skipping event", "event_id", ev.EventID, "err", err)
			continue
		}
		if exists {
			continue
		}
		if in.sink == nil {
			continue
		}
		if err := in.sink.Enqueue(ctx, ev); err != nil {
			slog.Warn("seed: demo audit enqueue failed", "event_id", ev.EventID, "err", err)
			continue
		}
		out.AuditEventIDs = append(out.AuditEventIDs, ev.EventID)
	}
	return out, nil
}

// generateDemoKeyMaterial creates one Agent API key token in the same
// layout as adminapi key issuance: "adc_<keyID>_<secret>" with
// SHA-256 hashes for storage (NFR-004). Only the hashes are persisted by
// the ApiKeyRepo; the plaintext returns here for the one-time log.
func generateDemoKeyMaterial() (keyID, prefix, secret, keyHash, secretHash string, err error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", "", "", "", "", fmt.Errorf("seed: generate demo key id: %w", err)
	}
	keyID = hex.EncodeToString(b[:])
	prefix = "adc_" + keyID
	sb := make([]byte, 32)
	if _, err := rand.Read(sb); err != nil {
		return "", "", "", "", "", fmt.Errorf("seed: generate demo key secret: %w", err)
	}
	secret = hex.EncodeToString(sb)
	return keyID, prefix, secret, agentauth.HashKeyID(keyID), agentauth.HashSecret(secret), nil
}
