package agentapi

import (
	"context"
	"fmt"
	"net/http"

	"adc.dev/ce/internal/agentauth"
	"adc.dev/ce/internal/httpx"
)

// CodeQuotaExceeded is the unified business code for quota denials:
// design/33 assigns 租户配额超限 to 13003 with HTTP 403 (the admin side
// uses the same code in adminapi.codeTenantQuota).
const CodeQuotaExceeded = "13003"

// QuotaGate enforces the tenant call quota ledger (design/32 6.2, B-10).
// The hot per-call counter lives in Valkey (adc:quota:call:{tenant},
// design/31 1.3.7) and is aggregated into adc_tenants.used_calls_month by
// a background task; the PG ledger is the authoritative backstop.
type QuotaGate interface {
	// Allow reports whether the tenant may make one more call. A denied
	// admission (quota exhausted) is (false, nil): denial is a normal
	// outcome, errors are reserved for lookup failures, which the
	// middleware treats as fail-open.
	Allow(ctx context.Context, tenantID string) (bool, error)
}

// QuotaLookup reads the authoritative ledger row for a tenant:
// used_calls_month vs quota_calls_monthly (design/32 3.1). It is injected
// by cmd so the gate stays DB-free in tests:
//
//	func quotaLookup(pool *db.Pool) agentapi.QuotaLookup {
//		return func(ctx context.Context, tenantID string) (int64, int64, error) {
//			var used, quota int64
//			err := pool.QueryRow(ctx,
//				`SELECT used_calls_month, quota_calls_monthly
//				   FROM adc_tenants WHERE id = $1::uuid AND deleted_at IS NULL`,
//				tenantID).Scan(&used, &quota)
//			return used, quota, err
//		}
//	}
type QuotaLookup func(ctx context.Context, tenantID string) (used int64, quota int64, err error)

// PGQuotaGate is the V1.0 QuotaGate reading the authoritative ledger.
type PGQuotaGate struct {
	Lookup QuotaLookup
}

// Allow admits a call while used < quota. A denied admission is
// (false, nil); lookup failures surface as errors and the middleware
// fails open on them (the rate limiter stays the online guard, and the
// conditional-update ledger prevents oversell).
func (g PGQuotaGate) Allow(ctx context.Context, tenantID string) (bool, error) {
	used, quota, err := g.Lookup(ctx, tenantID)
	if err != nil {
		return false, fmt.Errorf("agentapi: quota lookup: %w", err)
	}
	return used < quota, nil
}

// QuotaMiddleware rejects tool calls once the monthly call quota is
// exhausted with 403 and the unified 13xxx code. The tenant comes from
// the authenticated principal (SEC-02: never from request headers), so
// the middleware must sit AFTER agentauth.Authorize in the chain; the
// rate limiter (SEC-12) sits before it and answers 429 independently.
//
// cmd wiring shape (B-10):
//
//	gate := agentapi.PGQuotaGate{Lookup: quotaLookup(pool)}
//	agentMux := http.NewServeMux()
//	agentSrv.Routes(agentMux)
//	mux.Handle("/v1/agent/mcp/", ratelimit.Middleware(limiter, tenantKeyFn)(
//		agentapi.QuotaMiddleware(gate)(agentMux)))
//
// Gate errors fail open: a PG outage must not take the whole call path
// down; the ledger's conditional updates remain the hard backstop
// (design/32 6.2). Requests without a principal pass through — auth
// rejects them downstream.
func QuotaMiddleware(gate QuotaGate) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, ok := agentauth.FromContext(r.Context())
			if !ok || gate == nil {
				next.ServeHTTP(w, r)
				return
			}
			allowed, err := gate.Allow(r.Context(), p.TenantID)
			if err != nil {
				next.ServeHTTP(w, r) // fail open
				return
			}
			if !allowed {
				httpx.WriteError(w, http.StatusForbidden, CodeQuotaExceeded,
					"monthly call quota exceeded", httpx.TraceIDFrom(r))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
