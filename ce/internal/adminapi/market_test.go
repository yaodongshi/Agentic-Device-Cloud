package adminapi

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"adc.dev/core-sdk/protocol"
)

// ---------------------------------------------------------------------------
// In-memory ToolPackageRepo. Mirrors the PostgreSQL semantics (partial
// unique indexes per scope, per-tenant install idempotency, visibility
// filtering) so handler tests run without a database.
// ---------------------------------------------------------------------------

type memToolPackageRepo struct {
	mu       sync.Mutex
	pkgs     map[string]*ToolPackage
	installs map[string]map[string]time.Time // packageID -> tenantID -> installedAt
	nextID   int
	now      func() time.Time
}

func newMemToolPackageRepo() *memToolPackageRepo {
	return &memToolPackageRepo{
		pkgs:     map[string]*ToolPackage{},
		installs: map[string]map[string]time.Time{},
		now:      func() time.Time { return fixedNow },
	}
}

func cloneToolPackage(p *ToolPackage) *ToolPackage {
	if p == nil {
		return nil
	}
	cp := *p
	if p.TenantID != nil {
		tid := *p.TenantID
		cp.TenantID = &tid
	}
	if p.Tools != nil {
		cp.Tools = make([]protocol.MCPTool, len(p.Tools))
		copy(cp.Tools, p.Tools)
		for i := range cp.Tools {
			cp.Tools[i].InputSchema = append(json.RawMessage(nil), p.Tools[i].InputSchema...)
		}
	}
	if p.InstalledAt != nil {
		t := *p.InstalledAt
		cp.InstalledAt = &t
	}
	return &cp
}

func (r *memToolPackageRepo) nameConflict(tenantID *string, name, version, selfID string) bool {
	for id, cur := range r.pkgs {
		if id == selfID {
			continue
		}
		sameScope := (tenantID == nil && cur.TenantID == nil) ||
			(tenantID != nil && cur.TenantID != nil && *tenantID == *cur.TenantID)
		if sameScope && cur.Name == name && cur.Version == version {
			return true
		}
	}
	return false
}

func (r *memToolPackageRepo) Create(ctx context.Context, p *NewToolPackage) (*ToolPackage, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.nameConflict(p.TenantID, p.Name, p.Version, "") {
		return nil, ErrToolPackageNameExists
	}
	r.nextID++
	now := r.now().UTC()
	pkg := &ToolPackage{
		ID:          uuidOf(400000 + r.nextID),
		TenantID:    p.TenantID,
		Name:        p.Name,
		Version:     p.Version,
		Description: p.Description,
		Author:      p.Author,
		Tools:       p.Tools,
		Signature:   p.Signature,
		PublishedAt: now,
		Status:      toolPackageStatusPublished,
		ToolCount:   len(p.Tools),
	}
	r.pkgs[pkg.ID] = cloneToolPackage(pkg)
	return cloneToolPackage(pkg), nil
}

func (r *memToolPackageRepo) Get(ctx context.Context, id, tenantScope string) (*ToolPackage, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	pkg, ok := r.pkgs[id]
	if !ok {
		return nil, ErrToolPackageNotFound
	}
	return r.withInstall(pkg, tenantScope), nil
}

// withInstall attaches the tenant scope's install time (caller holds mu).
// The package-level watermark is cleared when the scope has no install
// record: InstalledAt is strictly per-tenant.
func (r *memToolPackageRepo) withInstall(pkg *ToolPackage, tenantScope string) *ToolPackage {
	cp := cloneToolPackage(pkg)
	cp.InstalledAt = nil
	if byTenant, ok := r.installs[pkg.ID]; ok {
		if at, ok := byTenant[tenantScope]; ok {
			cp.InstalledAt = &at
		}
	}
	return cp
}

func (r *memToolPackageRepo) List(ctx context.Context, tenantScope string, f ToolPackageFilter, p Page) ([]ToolPackage, int, error) {
	r.mu.Lock()
	var candidates []*ToolPackage
	for _, pkg := range r.pkgs {
		if pkg.TenantID != nil && *pkg.TenantID != tenantScope {
			continue
		}
		if f.Keyword != "" {
			kw := strings.ToLower(f.Keyword)
			if !strings.Contains(strings.ToLower(pkg.Name), kw) &&
				!strings.Contains(strings.ToLower(pkg.Description), kw) &&
				!strings.Contains(strings.ToLower(pkg.Author), kw) {
				continue
			}
		}
		candidates = append(candidates, r.withInstall(pkg, tenantScope))
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].PublishedAt.Equal(candidates[j].PublishedAt) {
			return candidates[i].ID < candidates[j].ID
		}
		return candidates[i].PublishedAt.After(candidates[j].PublishedAt)
	})
	r.mu.Unlock()

	total := len(candidates)
	start := p.offset()
	if start > len(candidates) {
		start = len(candidates)
	}
	end := start + p.Size
	if end > len(candidates) {
		end = len(candidates)
	}
	out := make([]ToolPackage, 0, end-start)
	for _, pkg := range candidates[start:end] {
		out = append(out, *pkg)
	}
	return out, total, nil
}

func (r *memToolPackageRepo) Install(ctx context.Context, packageID, tenantID, userID string) (*ToolPackage, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	pkg, ok := r.pkgs[packageID]
	if !ok {
		return nil, ErrToolPackageNotFound
	}
	if _, exists := r.installs[packageID]; !exists {
		r.installs[packageID] = map[string]time.Time{}
	}
	now := r.now().UTC()
	if _, installed := r.installs[packageID][tenantID]; !installed {
		r.installs[packageID][tenantID] = now
		r.pkgs[packageID].InstalledAt = &now
	}
	return r.withInstall(pkg, tenantID), nil
}

func (r *memToolPackageRepo) Uninstall(ctx context.Context, packageID, tenantID string) (*ToolPackage, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	pkg, ok := r.pkgs[packageID]
	if !ok {
		return nil, ErrToolPackageNotFound
	}
	if byTenant, exists := r.installs[packageID]; exists {
		delete(byTenant, tenantID)
	}
	return r.withInstall(pkg, tenantID), nil
}

// tamper mutates the stored tools JSON without touching the signature,
// simulating an at-rest integrity violation (tamper detection seam).
func (r *memToolPackageRepo) tamper(packageID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if pkg, ok := r.pkgs[packageID]; ok && len(pkg.Tools) > 0 {
		pkg.Tools[0].Description = "tampered"
	}
}

// ---------------------------------------------------------------------------
// fixtures and tests
// ---------------------------------------------------------------------------

// newMarketEnv builds the shared test harness with the market repo wired.
func newMarketEnv(t *testing.T) *testEnv {
	t.Helper()
	env := newTestEnv(t)
	env.srv.Market = newMemToolPackageRepo()
	return env
}

const (
	tenantA = "00000000-0000-4000-8000-000000000011"
	tenantB = "00000000-0000-4000-8000-000000000022"
)

func publishReq(name, version string, tools []protocol.MCPTool) publishToolPackageRequest {
	return publishToolPackageRequest{
		Name:        name,
		Version:     version,
		Description: "description of " + name,
		Author:      "qa",
		Tools:       tools,
	}
}

func sampleTools() []protocol.MCPTool {
	return []protocol.MCPTool{
		{
			Name:        "get_status",
			Description: "read machine status",
			InputSchema: json.RawMessage(`{"type":"object","properties":{},"required":[]}`),
		},
		{
			Name:        "set_speed",
			Description: "set spindle speed",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"rpm":{"type":"integer"}},"required":["rpm"]}`),
		},
	}
}

type toolPackageJSON struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Version     string            `json:"version"`
	Scope       string            `json:"scope"`
	ToolCount   int               `json:"tool_count"`
	Tools       []json.RawMessage `json:"tools"`
	Installed   bool              `json:"installed"`
	InstalledAt *string           `json:"installed_at"`
	PublishedAt string            `json:"published_at"`
}

type toolPackageListJSON struct {
	Items []toolPackageJSON `json:"items"`
	Total int               `json:"total"`
}

func TestPublishToolPackage(t *testing.T) {
	env := newMarketEnv(t)
	tok := env.token(t, []string{rolePlatformAdmin}, "")
	rec := env.do(http.MethodPost, "/v1/admin/tool-packages", tok, publishReq("cnc-pack", "1.0.0", sampleTools()))
	if rec.Code != http.StatusCreated {
		t.Fatalf("publish: status = %d; body: %s", rec.Code, rec.Body.String())
	}
	var out toolPackageJSON
	env.decode(t, rec, &out)
	if out.Scope != "platform" {
		t.Fatalf("scope = %q, want platform", out.Scope)
	}
	if out.ToolCount != 2 {
		t.Fatalf("tool_count = %d, want 2", out.ToolCount)
	}
	if len(out.Tools) != 2 {
		t.Fatalf("tools not echoed: %+v", out.Tools)
	}

	// Duplicate (name, version) in the platform scope conflicts.
	rec = env.do(http.MethodPost, "/v1/admin/tool-packages", tok, publishReq("cnc-pack", "1.0.0", sampleTools()))
	env.assertError(t, rec, http.StatusConflict, codeConflict)

	// Tenant publish is tenant-scoped.
	tokA := env.token(t, []string{"tenant_admin"}, tenantA)
	rec = env.do(http.MethodPost, "/v1/admin/tool-packages", tokA, publishReq("line-pack", "0.1.0", sampleTools()))
	if rec.Code != http.StatusCreated {
		t.Fatalf("tenant publish: status = %d; body: %s", rec.Code, rec.Body.String())
	}
	env.decode(t, rec, &out)
	if out.Scope != "tenant" {
		t.Fatalf("tenant publish scope = %q, want tenant", out.Scope)
	}
}

func TestPublishToolPackageValidation(t *testing.T) {
	env := newMarketEnv(t)
	tok := env.token(t, []string{rolePlatformAdmin}, "")

	cases := []struct {
		name string
		req  publishToolPackageRequest
	}{
		{"empty name", publishReq("", "1.0.0", sampleTools())},
		{"bad name charset", publishReq("bad/name!", "1.0.0", sampleTools())},
		{"bad version", publishReq("ok-pack", "1.0 beta", sampleTools())},
		{"nil tools", publishToolPackageRequest{Name: "ok-pack", Version: "1.0.0"}},
		{"empty tools", publishReq("ok-pack", "1.0.0", []protocol.MCPTool{})},
		{"tool missing name", publishReq("ok-pack", "1.0.0", []protocol.MCPTool{{Description: "x", InputSchema: json.RawMessage(`{}`)}})},
		{"tool missing description", publishReq("ok-pack", "1.0.0", []protocol.MCPTool{{Name: "t1", InputSchema: json.RawMessage(`{}`)}})},
		{"tool schema not object", publishReq("ok-pack", "1.0.0", []protocol.MCPTool{{Name: "t1", Description: "x", InputSchema: json.RawMessage(`[1,2]`)}})},
		{"tool risk out of range", publishReq("ok-pack", "1.0.0", func() []protocol.MCPTool {
			rl := 9
			return []protocol.MCPTool{{Name: "t1", Description: "x", InputSchema: json.RawMessage(`{}`), RiskLevel: &rl}}
		}())},
	}
	for _, tc := range cases {
		rec := env.do(http.MethodPost, "/v1/admin/tool-packages", tok, tc.req)
		env.assertError(t, rec, http.StatusBadRequest, codeBadRequest)
	}

	// A tool with an inputSchema that is not valid JSON fails body
	// decoding itself (raw string body, do() would refuse to marshal it).
	rec := env.do(http.MethodPost, "/v1/admin/tool-packages", tok,
		`{"name":"ok-pack","version":"1.0.0","tools":[{"name":"t1","description":"x","inputSchema":{oops}}]}`)
	env.assertError(t, rec, http.StatusBadRequest, codeBadRequest)

	// Publish is admin-only: approver is refused by the route RBAC gate.
	tokAppr := env.token(t, []string{"approver"}, tenantA)
	rec = env.do(http.MethodPost, "/v1/admin/tool-packages", tokAppr, publishReq("x", "1.0.0", sampleTools()))
	env.assertError(t, rec, http.StatusForbidden, codeForbidden)
}

func TestListToolPackagesIsolation(t *testing.T) {
	env := newMarketEnv(t)
	tok := env.token(t, []string{rolePlatformAdmin}, "")
	if rec := env.do(http.MethodPost, "/v1/admin/tool-packages", tok, publishReq("global-pack", "1.0.0", sampleTools())); rec.Code != http.StatusCreated {
		t.Fatalf("publish platform: %d %s", rec.Code, rec.Body.String())
	}
	tokA := env.token(t, []string{"tenant_admin"}, tenantA)
	if rec := env.do(http.MethodPost, "/v1/admin/tool-packages", tokA, publishReq("alpha-pack", "1.0.0", sampleTools())); rec.Code != http.StatusCreated {
		t.Fatalf("publish tenant A: %d %s", rec.Code, rec.Body.String())
	}

	// Tenant A sees the platform package and its own package.
	rec := env.do(http.MethodGet, "/v1/admin/tool-packages", tokA, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list A: %d %s", rec.Code, rec.Body.String())
	}
	var listA toolPackageListJSON
	env.decode(t, rec, &listA)
	if listA.Total != 2 {
		t.Fatalf("tenant A total = %d, want 2: %+v", listA.Total, listA.Items)
	}

	// Tenant B sees only the platform package (cross-tenant isolation).
	tokB := env.token(t, []string{"tenant_admin"}, tenantB)
	rec = env.do(http.MethodGet, "/v1/admin/tool-packages", tokB, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list B: %d %s", rec.Code, rec.Body.String())
	}
	var listB toolPackageListJSON
	env.decode(t, rec, &listB)
	if listB.Total != 1 || listB.Items[0].Name != "global-pack" {
		t.Fatalf("tenant B list = %+v, want only global-pack", listB.Items)
	}

	// Keyword search narrows the market.
	rec = env.do(http.MethodGet, "/v1/admin/tool-packages?keyword=alpha", tokA, nil)
	env.decode(t, rec, &listA)
	if listA.Total != 1 || listA.Items[0].Name != "alpha-pack" {
		t.Fatalf("keyword filter: %+v", listA.Items)
	}

	// Tenant B cannot read tenant A's tenant-scoped package (404, no
	// existence leak).
	rec = env.do(http.MethodGet, "/v1/admin/tool-packages?keyword=alpha&page=1&page_size=200", tokA, nil)
	env.decode(t, rec, &listA)
	alphaID := listA.Items[0].ID
	rec = env.do(http.MethodGet, "/v1/admin/tool-packages/"+alphaID, tokB, nil)
	env.assertError(t, rec, http.StatusNotFound, codeNotFound)
}

func TestGetToolPackageSignatureTamper(t *testing.T) {
	env := newMarketEnv(t)
	tok := env.token(t, []string{rolePlatformAdmin}, "")
	rec := env.do(http.MethodPost, "/v1/admin/tool-packages", tok, publishReq("tamper-pack", "1.0.0", sampleTools()))
	var out toolPackageJSON
	env.decode(t, rec, &out)

	// Detail read passes the signature check and carries the tools.
	rec = env.do(http.MethodGet, "/v1/admin/tool-packages/"+out.ID, tok, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("detail: %d %s", rec.Code, rec.Body.String())
	}
	var detail toolPackageJSON
	env.decode(t, rec, &detail)
	if len(detail.Tools) != 2 {
		t.Fatalf("detail tools = %d, want 2", len(detail.Tools))
	}

	// Tampered tools are refused by the signature verification seam.
	env.srv.Market.(*memToolPackageRepo).tamper(out.ID)
	rec = env.do(http.MethodGet, "/v1/admin/tool-packages/"+out.ID, tok, nil)
	env.assertError(t, rec, http.StatusConflict, codeConflict)
}

func TestInstallUninstallToolPackage(t *testing.T) {
	env := newMarketEnv(t)
	tok := env.token(t, []string{rolePlatformAdmin}, "")
	rec := env.do(http.MethodPost, "/v1/admin/tool-packages", tok, publishReq("install-pack", "1.0.0", sampleTools()))
	var pub toolPackageJSON
	env.decode(t, rec, &pub)

	tokA := env.token(t, []string{"tenant_admin"}, tenantA)
	rec = env.do(http.MethodPost, "/v1/admin/tool-packages/"+pub.ID+"/install", tokA, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("install: %d %s", rec.Code, rec.Body.String())
	}
	var installed toolPackageJSON
	env.decode(t, rec, &installed)
	if !installed.Installed || installed.InstalledAt == nil {
		t.Fatalf("install response: installed=%v at=%v", installed.Installed, installed.InstalledAt)
	}
	firstAt := *installed.InstalledAt

	// Idempotent re-install: 200 and the original installed_at.
	rec = env.do(http.MethodPost, "/v1/admin/tool-packages/"+pub.ID+"/install", tokA, nil)
	env.decode(t, rec, &installed)
	if !installed.Installed || installed.InstalledAt == nil || *installed.InstalledAt != firstAt {
		t.Fatalf("re-install not idempotent: installed=%v at=%v want %s", installed.Installed, installed.InstalledAt, firstAt)
	}

	// The list view reports install state per tenant: A installed, B not.
	tokB := env.token(t, []string{"tenant_admin"}, tenantB)
	rec = env.do(http.MethodGet, "/v1/admin/tool-packages", tokA, nil)
	var listA toolPackageListJSON
	env.decode(t, rec, &listA)
	if !listA.Items[0].Installed {
		t.Fatalf("tenant A list shows not installed: %+v", listA.Items[0])
	}
	rec = env.do(http.MethodGet, "/v1/admin/tool-packages", tokB, nil)
	var listB toolPackageListJSON
	env.decode(t, rec, &listB)
	if listB.Items[0].Installed {
		t.Fatalf("tenant B list shows installed: %+v", listB.Items[0])
	}

	// Uninstall flips the state back; a second uninstall is idempotent.
	rec = env.do(http.MethodPost, "/v1/admin/tool-packages/"+pub.ID+"/uninstall", tokA, nil)
	var removed toolPackageJSON
	env.decode(t, rec, &removed)
	if removed.Installed || removed.InstalledAt != nil {
		t.Fatalf("uninstall: installed=%v at=%v", removed.Installed, removed.InstalledAt)
	}
	rec = env.do(http.MethodPost, "/v1/admin/tool-packages/"+pub.ID+"/uninstall", tokA, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("second uninstall: %d %s", rec.Code, rec.Body.String())
	}
}

func TestInstallCrossTenantIsolation(t *testing.T) {
	env := newMarketEnv(t)
	tokA := env.token(t, []string{"tenant_admin"}, tenantA)
	rec := env.do(http.MethodPost, "/v1/admin/tool-packages", tokA, publishReq("private-pack", "1.0.0", sampleTools()))
	var pub toolPackageJSON
	env.decode(t, rec, &pub)

	// Tenant B cannot install (or see) tenant A's private package.
	tokB := env.token(t, []string{"tenant_admin"}, tenantB)
	rec = env.do(http.MethodPost, "/v1/admin/tool-packages/"+pub.ID+"/install", tokB, nil)
	env.assertError(t, rec, http.StatusNotFound, codeNotFound)

	// A platform admin operating on tenant B's behalf gets the same 404
	// for the tenant-scoped package of A (resolveTenant + visibility).
	tokPlat := env.token(t, []string{rolePlatformAdmin}, "")
	rec = env.do(http.MethodPost, "/v1/admin/tool-packages/"+pub.ID+"/install?tenant_id="+tenantB, tokPlat, nil)
	env.assertError(t, rec, http.StatusNotFound, codeNotFound)

	// ... but operating inside tenant A's scope is allowed.
	rec = env.do(http.MethodPost, "/v1/admin/tool-packages/"+pub.ID+"/install?tenant_id="+tenantA, tokPlat, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("platform install in owner scope: %d %s", rec.Code, rec.Body.String())
	}
}
