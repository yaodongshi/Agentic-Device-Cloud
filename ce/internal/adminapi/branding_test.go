package adminapi

import (
	"net/http"
	"testing"
)

func TestGetBrandingDefault(t *testing.T) {
	env := newTestEnv(t)
	rec := env.do(http.MethodGet, "/v1/admin/branding", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body: %s", rec.Code, rec.Body.String())
	}
	var out brandingResponse
	env.decode(t, rec, &out)
	if !out.IsDefault || out.Title != "ADC Console" || out.PrimaryColor != "#2563eb" {
		t.Fatalf("default branding = %+v", out)
	}
}

func TestGetBrandingUnknownTenantFallsBackToDefault(t *testing.T) {
	env := newTestEnv(t)
	rec := env.do(http.MethodGet, "/v1/admin/branding?tenant_id="+uuidOf(1), "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body: %s", rec.Code, rec.Body.String())
	}
	var out brandingResponse
	env.decode(t, rec, &out)
	if !out.IsDefault {
		t.Fatalf("want platform default for unknown tenant, got %+v", out)
	}
}

func TestPutBrandingThenGetScoped(t *testing.T) {
	env := newTestEnv(t)
	tr := env.createTenant(t, "acme", "Acme", nil)
	tok := env.token(t, []string{rolePlatformAdmin}, "")
	rec := env.do(http.MethodPut, "/v1/admin/branding", tok, map[string]any{
		"tenant_id":     tr.TenantID,
		"title":         "Acme 控制台",
		"logo_url":      "https://cdn.acme.test/logo.png",
		"primary_color": "#1d4ed8",
		"change_reason": "white label rollout",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body: %s", rec.Code, rec.Body.String())
	}
	var put brandingResponse
	env.decode(t, rec, &put)
	if put.IsDefault || put.Title != "Acme 控制台" || put.LogoURL != "https://cdn.acme.test/logo.png" || put.PrimaryColor != "#1d4ed8" {
		t.Fatalf("unexpected branding: %+v", put)
	}

	// The branded tenant sees its white label; the platform default
	// stays untouched for everyone else (tenant isolation).
	rec = env.do(http.MethodGet, "/v1/admin/branding?tenant_id="+tr.TenantID, "", nil)
	var got brandingResponse
	env.decode(t, rec, &got)
	if got.IsDefault || got.Title != "Acme 控制台" {
		t.Fatalf("tenant branding = %+v", got)
	}
	rec = env.do(http.MethodGet, "/v1/admin/branding", "", nil)
	var def brandingResponse
	env.decode(t, rec, &def)
	if !def.IsDefault || def.Title != "ADC Console" {
		t.Fatalf("platform branding = %+v", def)
	}
}

func TestBrandingTenantIsolation(t *testing.T) {
	env := newTestEnv(t)
	a := env.createTenant(t, "acme-a", "Acme A", nil)
	b := env.createTenant(t, "acme-b", "Acme B", nil)
	tok := env.token(t, []string{rolePlatformAdmin}, "")
	rec := env.do(http.MethodPut, "/v1/admin/branding", tok, map[string]any{
		"tenant_id":     a.TenantID,
		"title":         "Tenant A Console",
		"change_reason": "brand A",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body: %s", rec.Code, rec.Body.String())
	}
	rec = env.do(http.MethodGet, "/v1/admin/branding?tenant_id="+b.TenantID, "", nil)
	var out brandingResponse
	env.decode(t, rec, &out)
	if !out.IsDefault || out.Title == "Tenant A Console" {
		t.Fatalf("tenant B must not see A's branding: %+v", out)
	}
}

func TestBrandingPartialUpdateMerges(t *testing.T) {
	env := newTestEnv(t)
	tr := env.createTenant(t, "acme", "Acme", nil)
	tok := env.token(t, []string{rolePlatformAdmin}, "")
	rec := env.do(http.MethodPut, "/v1/admin/branding", tok, map[string]any{
		"tenant_id":     tr.TenantID,
		"title":         "Acme 控制台",
		"primary_color": "#1d4ed8",
		"change_reason": "initial brand",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body: %s", rec.Code, rec.Body.String())
	}
	// Second PUT updates only the title: color and logo must survive
	// (JSONB merge, not replace).
	rec = env.do(http.MethodPut, "/v1/admin/branding", tok, map[string]any{
		"tenant_id":     tr.TenantID,
		"title":         "Acme 控制台 v2",
		"logo_url":      "https://cdn.acme.test/logo.png",
		"change_reason": "rename",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body: %s", rec.Code, rec.Body.String())
	}
	rec = env.do(http.MethodGet, "/v1/admin/branding?tenant_id="+tr.TenantID, "", nil)
	var out brandingResponse
	env.decode(t, rec, &out)
	if out.Title != "Acme 控制台 v2" || out.PrimaryColor != "#1d4ed8" || out.LogoURL != "https://cdn.acme.test/logo.png" {
		t.Fatalf("merged branding = %+v", out)
	}
}

func TestPutBrandingValidation(t *testing.T) {
	env := newTestEnv(t)
	tr := env.createTenant(t, "acme", "Acme", nil)
	tok := env.token(t, []string{rolePlatformAdmin}, "")

	cases := []struct {
		name string
		body map[string]any
	}{
		{"missing tenant_id", map[string]any{"title": "x", "change_reason": "r"}},
		{"bad tenant_id", map[string]any{"tenant_id": "nope", "title": "x", "change_reason": "r"}},
		{"missing change_reason", map[string]any{"tenant_id": tr.TenantID, "title": "x"}},
		{"empty title", map[string]any{"tenant_id": tr.TenantID, "title": "  ", "change_reason": "r"}},
		{"bad color", map[string]any{"tenant_id": tr.TenantID, "primary_color": "red", "change_reason": "r"}},
		{"javascript logo", map[string]any{"tenant_id": tr.TenantID, "logo_url": "javascript:alert(1)", "change_reason": "r"}},
	}
	for _, c := range cases {
		rec := env.do(http.MethodPut, "/v1/admin/branding", tok, c.body)
		env.assertError(t, rec, http.StatusBadRequest, codeBadRequest)
	}
	// Unknown tenant -> 404 code 13001.
	rec := env.do(http.MethodPut, "/v1/admin/branding", tok, map[string]any{
		"tenant_id":     uuidOf(999),
		"title":         "x",
		"change_reason": "r",
	})
	env.assertError(t, rec, http.StatusNotFound, codeTenantNotFound)
}

func TestPutBrandingRequiresPlatformAdmin(t *testing.T) {
	env := newTestEnv(t)
	tr := env.createTenant(t, "acme", "Acme", nil)
	tok := env.token(t, []string{"tenant_admin"}, tr.TenantID)
	rec := env.do(http.MethodPut, "/v1/admin/branding", tok, map[string]any{
		"tenant_id":     tr.TenantID,
		"title":         "x",
		"change_reason": "r",
	})
	env.assertError(t, rec, http.StatusForbidden, codeForbidden)
}

func TestPutBrandingIsAudited(t *testing.T) {
	env := newTestEnv(t)
	tr := env.createTenant(t, "acme", "Acme", nil)
	tok := env.token(t, []string{rolePlatformAdmin}, "")
	rec := env.do(http.MethodPut, "/v1/admin/branding", tok, map[string]any{
		"tenant_id":     tr.TenantID,
		"title":         "Acme 控制台",
		"change_reason": "white label rollout",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body: %s", rec.Code, rec.Body.String())
	}
	op, ok := env.audit.last()
	if !ok || op.Action != "branding.update" || op.Target != tr.TenantID || op.Reason != "white label rollout" {
		t.Fatalf("audit = %+v, want branding.update", op)
	}
}

func TestTenantMetaMergeSemantics(t *testing.T) {
	env := newTestEnv(t)
	tr := env.createTenant(t, "acme", "Acme", nil)
	ctx := t.Context()
	meta, err := env.store.tenants.SetMeta(ctx, tr.TenantID, map[string]any{
		"branding":             map[string]any{"title": "A"},
		"budget_monthly_cents": 12345,
	})
	if err != nil {
		t.Fatalf("SetMeta: %v", err)
	}
	if got := metaInt64(meta, "budget_monthly_cents"); got != 12345 {
		t.Fatalf("budget = %d, want 12345", got)
	}
	// Merge keeps sibling keys; nil removes.
	meta, err = env.store.tenants.SetMeta(ctx, tr.TenantID, map[string]any{
		"branding":             map[string]any{"title": "B"},
		"budget_monthly_cents": nil,
	})
	if err != nil {
		t.Fatalf("SetMeta: %v", err)
	}
	if _, ok := meta["budget_monthly_cents"]; ok {
		t.Fatalf("budget key not removed: %+v", meta)
	}
	if b, _ := meta["branding"].(map[string]any); b["title"] != "B" {
		t.Fatalf("branding = %+v", meta["branding"])
	}
	// Unknown tenant.
	if _, err := env.store.tenants.SetMeta(ctx, uuidOf(999), map[string]any{"x": 1}); err != ErrTenantNotFound {
		t.Fatalf("want ErrTenantNotFound, got %v", err)
	}
	if _, err := env.store.tenants.GetMeta(ctx, uuidOf(999)); err != ErrTenantNotFound {
		t.Fatalf("want ErrTenantNotFound, got %v", err)
	}
}
