package agentapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"adc.dev/ce/internal/agentauth"
)

func TestPGQuotaGateAllow(t *testing.T) {
	tests := []struct {
		name      string
		used      int64
		quota     int64
		wantAllow bool
	}{
		{"under quota", 90, 100, true},
		{"at quota", 100, 100, false},
		{"over quota", 101, 100, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := PGQuotaGate{Lookup: func(ctx context.Context, tenantID string) (int64, int64, error) {
				return tt.used, tt.quota, nil
			}}
			allowed, err := g.Allow(context.Background(), "t1")
			if err != nil {
				t.Fatal(err)
			}
			if allowed != tt.wantAllow {
				t.Fatalf("allowed=%v, want %v", allowed, tt.wantAllow)
			}
		})
	}
}

func TestPGQuotaGateLookupError(t *testing.T) {
	g := PGQuotaGate{Lookup: func(ctx context.Context, tenantID string) (int64, int64, error) {
		return 0, 0, errors.New("pg down")
	}}
	if _, err := g.Allow(context.Background(), "t1"); err == nil {
		t.Fatal("lookup failures must surface as errors (middleware fails open)")
	}
}

func TestQuotaMiddlewareDeniesOverQuota(t *testing.T) {
	gate := PGQuotaGate{Lookup: func(ctx context.Context, tenantID string) (int64, int64, error) {
		return 100, 100, nil
	}}
	h := QuotaMiddleware(gate)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	r := httptest.NewRequest(http.MethodPost, "/v1/agent/mcp/tools/call", nil)
	r = r.WithContext(agentauth.WithPrincipal(r.Context(),
		&agentauth.Principal{KeyID: "k1", TenantID: "tenant-1", AgentID: "agent-1"}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status %d, want 403", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"code":"`+CodeQuotaExceeded+`"`) {
		t.Fatalf("response must carry unified code %s, got %s", CodeQuotaExceeded, body)
	}
}

func TestQuotaMiddlewareAllowsUnderQuota(t *testing.T) {
	gate := PGQuotaGate{Lookup: func(ctx context.Context, tenantID string) (int64, int64, error) {
		return 1, 100, nil
	}}
	called := false
	h := QuotaMiddleware(gate)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	r := httptest.NewRequest(http.MethodPost, "/v1/agent/mcp/tools/call", nil)
	r = r.WithContext(agentauth.WithPrincipal(r.Context(),
		&agentauth.Principal{KeyID: "k1", TenantID: "tenant-1"}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)

	if rec.Code != http.StatusOK || !called {
		t.Fatalf("under-quota request must pass through, status=%d called=%v", rec.Code, called)
	}
}

func TestQuotaMiddlewareFailsOpenOnGateError(t *testing.T) {
	gate := PGQuotaGate{Lookup: func(ctx context.Context, tenantID string) (int64, int64, error) {
		return 0, 0, errors.New("pg down")
	}}
	called := false
	h := QuotaMiddleware(gate)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	r := httptest.NewRequest(http.MethodPost, "/v1/agent/mcp/tools/call", nil)
	r = r.WithContext(agentauth.WithPrincipal(r.Context(),
		&agentauth.Principal{KeyID: "k1", TenantID: "tenant-1"}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)

	if rec.Code != http.StatusOK || !called {
		t.Fatalf("gate errors must fail open, status=%d called=%v", rec.Code, called)
	}
}

func TestQuotaMiddlewarePassesWithoutPrincipal(t *testing.T) {
	gate := PGQuotaGate{Lookup: func(ctx context.Context, tenantID string) (int64, int64, error) {
		t.Fatal("gate must not run without a principal (auth rejects downstream)")
		return 0, 0, nil
	}}
	called := false
	h := QuotaMiddleware(gate)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	r := httptest.NewRequest(http.MethodPost, "/v1/agent/mcp/tools/call", nil)
	h.ServeHTTP(httptest.NewRecorder(), r)
	if !called {
		t.Fatal("unauthenticated requests pass through to the auth middleware")
	}
}
