package adminapi

import (
	"context"
	"net/http"
	"testing"
	"time"

	"adc.dev/ce/internal/billing"
)

func TestBudgetStatusBoundaries(t *testing.T) {
	env := newTestEnv(t)
	tr := env.createTenant(t, "acme", "Acme", nil)
	tok := env.token(t, []string{rolePlatformAdmin}, "")
	// Budget line 10000 fen (100 yuan).
	rec := env.do(http.MethodPut, "/v1/admin/tenants/"+tr.TenantID+"/budget", tok, map[string]any{
		"budget_monthly_cents": 10000,
		"change_reason":        "Q3 budget",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body: %s", rec.Code, rec.Body.String())
	}
	var line budgetLineResponse
	env.decode(t, rec, &line)
	if line.BudgetMonthlyCents != 10000 {
		t.Fatalf("budget line = %+v", line)
	}

	cases := []struct {
		fee     int64
		percent int
		status  string
	}{
		{0, 0, budgetStatusOK},
		{7999, 79, budgetStatusOK},
		{8000, 80, budgetStatusWarn},
		{9999, 99, budgetStatusWarn},
		{10000, 100, budgetStatusExceeded},
		{15000, 150, budgetStatusExceeded},
	}
	for _, c := range cases {
		env.budget.setFee(tr.TenantID, c.fee)
		rec := env.do(http.MethodGet, "/v1/admin/tenants/"+tr.TenantID+"/budget-status", tok, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("fee=%d: status = %d; body: %s", c.fee, rec.Code, rec.Body.String())
		}
		var out budgetStatusResponse
		env.decode(t, rec, &out)
		if out.UsagePercent != c.percent || out.Status != c.status {
			t.Fatalf("fee=%d: got percent=%d status=%s, want percent=%d status=%s",
				c.fee, out.UsagePercent, out.Status, c.percent, c.status)
		}
		if out.BilledFen != c.fee || out.BudgetMonthlyCents != 10000 {
			t.Fatalf("fee=%d: amounts = %+v", c.fee, out)
		}
		if out.Period != "2026-08" {
			t.Fatalf("period = %q, want 2026-08 (fixedNow)", out.Period)
		}
	}
}

func TestBudgetStatusUnset(t *testing.T) {
	env := newTestEnv(t)
	tr := env.createTenant(t, "acme", "Acme", nil)
	tok := env.token(t, []string{rolePlatformAdmin}, "")
	env.budget.setFee(tr.TenantID, 5000)
	rec := env.do(http.MethodGet, "/v1/admin/tenants/"+tr.TenantID+"/budget-status", tok, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body: %s", rec.Code, rec.Body.String())
	}
	var out budgetStatusResponse
	env.decode(t, rec, &out)
	if out.BudgetMonthlyCents != 0 || out.UsagePercent != 0 || out.Status != budgetStatusOK {
		t.Fatalf("unset budget: %+v", out)
	}
}

func TestPutBudgetClearsLine(t *testing.T) {
	env := newTestEnv(t)
	tr := env.createTenant(t, "acme", "Acme", nil)
	tok := env.token(t, []string{rolePlatformAdmin}, "")
	rec := env.do(http.MethodPut, "/v1/admin/tenants/"+tr.TenantID+"/budget", tok, map[string]any{
		"budget_monthly_cents": 10000,
		"change_reason":        "set",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body: %s", rec.Code, rec.Body.String())
	}
	rec = env.do(http.MethodPut, "/v1/admin/tenants/"+tr.TenantID+"/budget", tok, map[string]any{
		"budget_monthly_cents": 0,
		"change_reason":        "remove budget",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body: %s", rec.Code, rec.Body.String())
	}
	var line budgetLineResponse
	env.decode(t, rec, &line)
	if line.BudgetMonthlyCents != 0 {
		t.Fatalf("budget line = %+v", line)
	}
	meta, err := env.store.tenants.GetMeta(t.Context(), tr.TenantID)
	if err != nil {
		t.Fatalf("GetMeta: %v", err)
	}
	if _, ok := meta[budgetMetaKey]; ok {
		t.Fatalf("budget key must be removed, meta = %+v", meta)
	}
	// The audit trail records the change.
	op, ok := env.audit.last()
	if !ok || op.Action != "budget.update" || op.Target != tr.TenantID {
		t.Fatalf("audit = %+v, want budget.update", op)
	}
}

func TestBudgetValidation(t *testing.T) {
	env := newTestEnv(t)
	tr := env.createTenant(t, "acme", "Acme", nil)
	tok := env.token(t, []string{rolePlatformAdmin}, "")
	cases := []struct {
		name string
		body map[string]any
	}{
		{"negative budget", map[string]any{"budget_monthly_cents": -1, "change_reason": "r"}},
		{"missing reason", map[string]any{"budget_monthly_cents": 100}},
	}
	for _, c := range cases {
		rec := env.do(http.MethodPut, "/v1/admin/tenants/"+tr.TenantID+"/budget", tok, c.body)
		env.assertError(t, rec, http.StatusBadRequest, codeBadRequest)
	}
	// Unknown tenant -> 404 code 13001.
	rec := env.do(http.MethodPut, "/v1/admin/tenants/"+uuidOf(999)+"/budget", tok, map[string]any{
		"budget_monthly_cents": 100,
		"change_reason":        "r",
	})
	env.assertError(t, rec, http.StatusNotFound, codeTenantNotFound)
}

func TestBudgetScopingAndRoles(t *testing.T) {
	env := newTestEnv(t)
	a := env.createTenant(t, "acme-a", "Acme A", nil)
	b := env.createTenant(t, "acme-b", "Acme B", nil)
	// tenant_admin may read its own budget status.
	ta := env.token(t, []string{"tenant_admin"}, a.TenantID)
	rec := env.do(http.MethodGet, "/v1/admin/tenants/"+a.TenantID+"/budget-status", ta, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("own tenant read: status = %d; body: %s", rec.Code, rec.Body.String())
	}
	// Cross-tenant read is rejected (design/33 1.2, SEC-02).
	rec = env.do(http.MethodGet, "/v1/admin/tenants/"+b.TenantID+"/budget-status", ta, nil)
	env.assertError(t, rec, http.StatusForbidden, codeCrossTenant)
	// Budget line writes are platform_admin exclusives.
	rec = env.do(http.MethodPut, "/v1/admin/tenants/"+a.TenantID+"/budget", ta, map[string]any{
		"budget_monthly_cents": 100,
		"change_reason":        "r",
	})
	env.assertError(t, rec, http.StatusForbidden, codeForbidden)
}

// stubAggregator feeds the billing engine synthetic usage summaries so
// pgBudgetUsageRepo can be priced without a database.
type stubAggregator struct {
	sum billing.UsageSummary
}

func (a stubAggregator) Aggregate(_ context.Context, _ string, _, _ time.Time) (*billing.UsageSummary, error) {
	return &a.sum, nil
}

func TestPGBudgetUsageRepoPricesCurrentMonth(t *testing.T) {
	repo := &pgBudgetUsageRepo{
		eng: billing.NewEngine(stubAggregator{sum: billing.UsageSummary{
			DevicePeak: 25,
			ToolCalls:  1000,
			Tokens:     1000,
		}}),
		now: func() time.Time { return fixedNow },
	}
	fee, err := repo.CurrentMonthFeeFen(t.Context(), uuidOf(1))
	if err != nil {
		t.Fatalf("CurrentMonthFeeFen: %v", err)
	}
	// subscription 331667 + device tier (5 x 40000 fen annual -> 16667
	// monthly) + tokens (1000 x 10 fen/1k = 10) = 348344 fen.
	if fee != 348344 {
		t.Fatalf("fee = %d, want 348344", fee)
	}
}

func TestBudgetPercentUnit(t *testing.T) {
	cases := []struct {
		billed, budget int64
		percent        int
		status         string
	}{
		{0, 0, 0, budgetStatusOK},
		{5000, 0, 0, budgetStatusOK},
		{80, 100, 80, budgetStatusWarn},
		{100, 100, 100, budgetStatusExceeded},
		{199, 200, 99, budgetStatusWarn},
	}
	for _, c := range cases {
		p := budgetPercent(c.billed, c.budget)
		if p != c.percent || budgetStatus(p) != c.status {
			t.Fatalf("billed=%d budget=%d: percent=%d status=%s, want %d %s",
				c.billed, c.budget, p, budgetStatus(p), c.percent, c.status)
		}
	}
}
