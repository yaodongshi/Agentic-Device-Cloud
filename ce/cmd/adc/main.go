// Command adc runs the ADC development server: all core modules assembled in
// a single process for local integration and smoke testing (design/31 5.x).
// Production deployment splits these modules into separate services behind
// the unified API gateway (design/60).
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"

	"adc.dev/ce/internal/agentapi"
	"adc.dev/ce/internal/agentauth"
	"adc.dev/ce/internal/auth"
	"adc.dev/ce/internal/config"
	"adc.dev/ce/internal/connector"
	"adc.dev/ce/internal/db"
	"adc.dev/ce/pkg/audit"
)

func main() {
	var seed bool
	flag.BoolVar(&seed, "seed", false, "seed a demo tenant/device/tool and print the device secret (dev only)")
	flag.Parse()

	cfg, err := config.Load()
	if err != nil {
		fatal(err)
	}
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := db.Connect(ctx, cfg.Postgres)
	if err != nil {
		fatal(err)
	}
	defer pool.Close()

	rdb := redis.NewClient(&redis.Options{Addr: cfg.Valkey.Addr, Password: cfg.Valkey.Password, DB: cfg.Valkey.DB})
	defer rdb.Close()

	// --- device auth (SEC-03) ---
	kek, err := auth.LoadKEK()
	if err != nil {
		fatal(fmt.Errorf("ADC_DEVICE_KEK required (64 hex chars): %w", err))
	}
	credRepo := auth.NewCredentialRepo(pool.Pool, kek)
	nonces := auth.NewValkeyNonceStore(&redis.Options{Addr: cfg.Valkey.Addr, Password: cfg.Valkey.Password, DB: cfg.Valkey.DB})
	verifier := auth.NewVerifier(credRepo, nonces)

	if seed {
		secret, serr := seedDemo(ctx, pool, kek)
		if serr != nil {
			fatal(serr)
		}
		log.Info("seeded demo tenant/device/tool", "device_code", "cnc-demo-01", "secret", secret)
		_ = os.WriteFile("/tmp/adc-demo-secret", []byte(secret), 0o600)
	}
	var tenantID string
	if err := pool.QueryRow(ctx, `SELECT id::text FROM adc_tenants WHERE code='tenant-demo'`).Scan(&tenantID); err != nil {
		fatal(fmt.Errorf("load demo tenant: %w", err))
	}

	// --- connector (WSS tunnel, SEC-04/14/15) ---
	hub := connector.NewDeviceHub()
	registry := connector.NewValkeyRegistry(rdb, time.Duration(cfg.DeviceTTLSeconds)*time.Second, log)
	tunnel := connector.NewTunnelServer(connector.TunnelServerConfig{
		Auth:      deviceAuthAdapter{v: verifier},
		Hub:       hub,
		Registry:  registry,
		NodeID:    cfg.NodeID,
		DeviceTTL: time.Duration(cfg.DeviceTTLSeconds) * time.Second,
		Logger:    log,
	})

	// --- agent api (SEC-02/09) ---
	policy := agentapi.DBPolicy{Lookup: riskLookup(pool)}
	agg := agentapi.DBAggregator{Rows: toolRows(pool), Lookup: riskLookup(pool), UUIDLookup: deviceUUIDLookup(pool)}
	router := agentapi.LocalRouter{Session: func(ctx context.Context, tenantID, deviceID string) (agentapi.RPCClient, error) {
		sess, err := hub.GetDevice(tenantID, deviceID)
		if err != nil {
			return nil, err
		}
		return sess, nil
	}}
	hitl := agentapi.NewInMemoryHITLClient()
	agentSrv := agentapi.NewServer(authValidator{tenantID: tenantID}, agg, router, policy, hitl)
	agentSrv.Audit = audit.NewValkeySink(rdb)

	// --- audit pipeline (SEC-07) ---
	auditWorker := audit.NewValkeyWorker(rdb, pool.Pool)
	go auditWorker.Run(ctx)

	mux := http.NewServeMux()
	agentSrv.Routes(mux)
	mux.Handle("GET /v1/devices/tunnel", tunnel)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"status":"ok"}`)
	})
	// Root index: dev-friendly endpoint listing for browser visits.
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<html><head><title>ADC Dev Server</title></head><body>
<h1>ADC Dev Server (Go 数据面)</h1>
<p>此端口为内部服务端口；业务统一入口是网关端口 18080。</p>
<ul>
<li><a href="/healthz">/healthz</a> 健康检查</li>
<li>GET /v1/agent/mcp/tools（需 X-ADC-Key 头）</li>
<li>POST /v1/agent/mcp/tools/call（需 X-ADC-Key 头）</li>
<li>GET /v1/devices/tunnel（设备 WSS 隧道）</li>
<li>POST /dev/decide（dev 审批决策注入）</li>
</ul></body></html>`)
	})
	// Dev-only decision injection: production replaces this with the signed
	// HITL callback endpoint of the approval service (SEC-01).
	mux.HandleFunc("POST /dev/decide", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			TicketID string `json:"ticket_id"`
			Decision string `json:"decision"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		status := agentapi.DecisionRejected
		if req.Decision == "approve" {
			status = agentapi.DecisionApproved
		}
		hitl.DecideTicket(req.TicketID, agentapi.TicketDecision{Status: status, By: "dev-script"})
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"ok":true}`)
	})

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Info("adc dev server listening", "addr", cfg.HTTPAddr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
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

// seedDemo inserts a demo tenant, device (hmac, KEK-encrypted secret) and a
// high-risk tool for the smoke flow. The plaintext secret is returned once
// for the mock device.
func seedDemo(ctx context.Context, pool *db.Pool, kek []byte) (string, error) {
	if _, err := pool.Exec(ctx, `INSERT INTO adc_tenants (id, code, name, status)
		SELECT gen_random_uuid(), 'tenant-demo', 'Demo', 'ACTIVE'
		WHERE NOT EXISTS (SELECT 1 FROM adc_tenants WHERE code='tenant-demo')`); err != nil {
		return "", err
	}
	secret, err := auth.GenerateSecret()
	if err != nil {
		return "", err
	}
	stored, err := auth.EncryptSecret(secret, kek)
	if err != nil {
		return "", err
	}
	// Dev idempotency: update the existing demo device's credential with the
	// freshly generated secret (the plaintext is only known to this run),
	// then insert only when absent.
	if _, err := pool.Exec(ctx, `UPDATE adc_devices SET credential_hash=$1, auth_type='hmac', device_class='B'
		WHERE device_code='cnc-demo-01'`, stored); err != nil {
		return "", err
	}
	if _, err := pool.Exec(ctx, `INSERT INTO adc_devices (id, tenant_id, device_code, name, device_type, device_class, auth_type, credential_hash, status)
		SELECT gen_random_uuid(), id, 'cnc-demo-01', 'Demo CNC', 'cnc', 'B', 'hmac', $1, 'OFFLINE'
		FROM adc_tenants WHERE code='tenant-demo'
		AND NOT EXISTS (SELECT 1 FROM adc_devices WHERE device_code='cnc-demo-01')`, stored); err != nil {
		return "", err
	}
	if _, err := pool.Exec(ctx, `INSERT INTO adc_device_tools (id, tenant_id, device_id, tool_name, description, input_schema, risk_level, is_enabled)
		SELECT gen_random_uuid(), d.tenant_id, d.id, 'set_spindle_speed', 'Set CNC spindle RPM (high risk)', '{"type":"object","properties":{"rpm":{"type":"number"}}}', 2, true
		FROM adc_devices d WHERE d.device_code='cnc-demo-01'
		AND NOT EXISTS (SELECT 1 FROM adc_device_tools t WHERE t.device_id=d.id AND t.tool_name='set_spindle_speed')`); err != nil {
		return "", err
	}
	return secret, nil
}
