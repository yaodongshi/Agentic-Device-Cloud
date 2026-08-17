// Tool package marketplace endpoints (design/83 C3.1/C3.2, design/82 C3).
// The market surface publishes, lists, installs and uninstalls tool
// packages: a named/versioned bundle of standard MCP tools
// (core-sdk/protocol.MCPTool) with a SHA-256 signature over the canonical
// tools JSON for tamper detection (migration 0004_tool_packages).
//
//	POST /v1/admin/tool-packages                     publish
//	GET  /v1/admin/tool-packages                     market list (search/paging)
//	GET  /v1/admin/tool-packages/{id}                detail incl. tools
//	POST /v1/admin/tool-packages/{id}/install        install to tenant (idempotent)
//	POST /v1/admin/tool-packages/{id}/uninstall      uninstall (idempotent)
//
// Scope rules (design/83 C3.1): platform_admin publishes platform-level
// packages visible to every tenant; tenant_admin publishes tenant-scoped
// packages visible to that tenant only. Install state is per tenant
// (adc_tool_package_installs) and duplicate installs are no-ops.
package adminapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"adc.dev/ce/internal/adminauth"
	"adc.dev/ce/internal/httpx"
	"adc.dev/core-sdk/protocol"
)

// Tool package repository sentinels.
var (
	ErrToolPackageNotFound   = errors.New("adminapi: tool package not found")
	ErrToolPackageNameExists = errors.New("adminapi: tool package name/version already published")
	ErrToolPackageTampered   = errors.New("adminapi: tool package signature mismatch")
)

// Tool package statuses (migration 0004; V1 only ever publishes
// PUBLISHED, DEPRECATED is reserved for the C3 rating lifecycle).
const (
	toolPackageStatusPublished  = "PUBLISHED"
	toolPackageStatusDeprecated = "DEPRECATED"
)

// ToolPackage is one adc_tool_packages row (design/83 C3.1). TenantID
// nil means a platform-level package visible to every tenant; InstalledAt
// is requester-scoped: on List/Get it reports the calling tenant's
// install time (adc_tool_package_installs), nil when not installed.
type ToolPackage struct {
	ID          string
	TenantID    *string
	Name        string
	Version     string
	Description string
	Author      string
	Tools       []protocol.MCPTool
	Signature   string
	PublishedAt time.Time
	Status      string
	InstalledAt *time.Time
	ToolCount   int
}

// NewToolPackage carries the publish-time payload handed to the
// repository; the signature digest is computed by the handler over the
// canonical tools JSON and stored verbatim.
type NewToolPackage struct {
	TenantID    *string
	Name        string
	Version     string
	Description string
	Author      string
	Tools       []protocol.MCPTool
	Signature   string
}

// ToolPackageFilter narrows the market list query.
type ToolPackageFilter struct {
	Keyword string // case-insensitive over name/description/author
}

// ToolPackageRepo persists tool packages (design/31 LLD 3.4.2 seam).
// List/Get take the requesting tenant so per-tenant install state can be
// joined and tenant-scoped packages filtered out.
type ToolPackageRepo interface {
	Create(ctx context.Context, p *NewToolPackage) (*ToolPackage, error)
	Get(ctx context.Context, id, tenantScope string) (*ToolPackage, error)
	List(ctx context.Context, tenantScope string, f ToolPackageFilter, p Page) ([]ToolPackage, int, error)
	Install(ctx context.Context, packageID, tenantID, userID string) (*ToolPackage, error)
	Uninstall(ctx context.Context, packageID, tenantID string) (*ToolPackage, error)
}

// Validation bounds (design/83 C3.1: package metadata mirrors the tool
// catalog conventions of design/33 3.1.10-3.1.11).
const (
	maxPackageNameLen        = 128
	maxPackageVersionLen     = 32
	maxPackageDescriptionLen = 2000
	maxPackageAuthorLen      = 128
	maxPackageTools          = 100
	maxToolsJSONBytes        = 256 << 10 // 256 KiB canonical tools payload
	maxPackageKeywordLen     = 256
	maxToolSchemaVersionLen  = 64
)

var (
	packageNameRe    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ._-]{0,127}$`)
	packageVersionRe = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z._-]{0,31}$`)
	toolNameRe       = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_./-]{0,127}$`)
)

// canonicalToolsJSON re-encodes the decoded tool list through
// protocol.MCPTool so the stored JSONB and the signature digest share one
// canonical byte form. JSONB would otherwise normalize whitespace and key
// order and drift from the digest of the raw client payload.
func canonicalToolsJSON(tools []protocol.MCPTool) ([]byte, error) {
	return json.Marshal(tools)
}

// toolsDigest returns the hex SHA-256 over the canonical tools JSON.
func toolsDigest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// verifyPackageSignature recomputes the digest over the canonical tools
// JSON and compares it with the stored signature (tamper detection seam).
func verifyPackageSignature(p *ToolPackage) error {
	canonical, err := canonicalToolsJSON(p.Tools)
	if err != nil {
		return err
	}
	if toolsDigest(canonical) != p.Signature {
		return ErrToolPackageTampered
	}
	return nil
}

// validatePackageTools enforces the standard MCP tool shape for every
// entry (design/83 C3.1: 包内工具用标准 MCP 工具定义) and returns the
// canonical encoding for storage/signature.
func validatePackageTools(tools []protocol.MCPTool) ([]byte, error) {
	if len(tools) == 0 {
		return nil, errors.New("tools must not be empty")
	}
	if len(tools) > maxPackageTools {
		return nil, fmt.Errorf("tools must be at most %d entries", maxPackageTools)
	}
	for i := range tools {
		t := &tools[i]
		if !toolNameRe.MatchString(t.Name) {
			return nil, fmt.Errorf("tool %d: name is invalid", i)
		}
		if strings.TrimSpace(t.Description) == "" {
			return nil, fmt.Errorf("tool %s: description is required", t.Name)
		}
		trimmed := bytes.TrimSpace(t.InputSchema)
		if len(trimmed) == 0 || trimmed[0] != '{' {
			return nil, fmt.Errorf("tool %s: inputSchema must be a JSON object", t.Name)
		}
		var probe map[string]any
		if err := json.Unmarshal(trimmed, &probe); err != nil {
			return nil, fmt.Errorf("tool %s: inputSchema is not valid JSON: %v", t.Name, err)
		}
		if t.RiskLevel != nil && (*t.RiskLevel < 0 || *t.RiskLevel > 3) {
			return nil, fmt.Errorf("tool %s: riskLevel must be 0..3", t.Name)
		}
		if len(t.SchemaVersion) > maxToolSchemaVersionLen {
			return nil, fmt.Errorf("tool %s: schemaVersion must be at most %d chars", t.Name, maxToolSchemaVersionLen)
		}
	}
	canonical, err := canonicalToolsJSON(tools)
	if err != nil {
		return nil, err
	}
	if len(canonical) > maxToolsJSONBytes {
		return nil, fmt.Errorf("tools payload exceeds %d bytes", maxToolsJSONBytes)
	}
	return canonical, nil
}

func mapToolPackageRepoError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ErrToolPackageNotFound):
		writeError(w, r, http.StatusNotFound, codeNotFound, "tool package not found")
	case errors.Is(err, ErrToolPackageNameExists):
		writeError(w, r, http.StatusConflict, codeConflict, "tool package name/version already published")
	case errors.Is(err, ErrToolPackageTampered):
		writeError(w, r, http.StatusConflict, codeConflict, "tool package integrity check failed")
	default:
		writeError(w, r, http.StatusInternalServerError, codeInternal, "internal error")
	}
}

// ---------------------------------------------------------------------------
// handlers
// ---------------------------------------------------------------------------

// publishToolPackageRequest mirrors the publish form: name/version/
// description plus the raw tools array (standard MCP tool definitions).
// Author is optional and defaults to the acting admin's user id.
type publishToolPackageRequest struct {
	Name        string             `json:"name"`
	Version     string             `json:"version"`
	Description string             `json:"description"`
	Author      string             `json:"author"`
	Tools       []protocol.MCPTool `json:"tools"`
}

// toolPackageResponse mirrors the package row for the console. Tools
// (and the signature) are carried only on publish/detail responses; the
// market list omits them to keep the payload small.
type toolPackageResponse struct {
	ID          string             `json:"id"`
	Name        string             `json:"name"`
	Version     string             `json:"version"`
	Description string             `json:"description"`
	Author      string             `json:"author"`
	Scope       string             `json:"scope"`
	ToolCount   int                `json:"tool_count"`
	Tools       []protocol.MCPTool `json:"tools,omitempty"`
	Status      string             `json:"status"`
	Installed   bool               `json:"installed"`
	InstalledAt *time.Time         `json:"installed_at,omitempty"`
	PublishedAt time.Time          `json:"published_at"`
}

type toolPackageListResponse struct {
	Items    []toolPackageResponse `json:"items"`
	Total    int                   `json:"total"`
	Page     int                   `json:"page"`
	PageSize int                   `json:"page_size"`
}

func newToolPackageResponse(p *ToolPackage, withTools bool) toolPackageResponse {
	resp := toolPackageResponse{
		ID:          p.ID,
		Name:        p.Name,
		Version:     p.Version,
		Description: p.Description,
		Author:      p.Author,
		Scope:       packageScope(p),
		ToolCount:   len(p.Tools),
		Status:      p.Status,
		Installed:   p.InstalledAt != nil,
		InstalledAt: p.InstalledAt,
		PublishedAt: p.PublishedAt.UTC(),
	}
	if withTools {
		resp.Tools = p.Tools
	}
	return resp
}

func packageScope(p *ToolPackage) string {
	if p.TenantID == nil {
		return "platform"
	}
	return "tenant"
}

// handlePublishToolPackage serves POST /v1/admin/tool-packages
// (design/83 C3.1): validates the tool array as a legal MCPTool list,
// computes the SHA-256 signature over the canonical JSON and stores the
// package. Platform admins publish platform-level (global) packages;
// tenant admins publish tenant-scoped packages.
func (s *Server) handlePublishToolPackage(w http.ResponseWriter, r *http.Request) {
	p, ok := adminauth.FromContext(r.Context())
	if !ok {
		writeError(w, r, http.StatusUnauthorized, codeUnauthorized, "unauthenticated")
		return
	}
	if s.Market == nil {
		writeError(w, r, http.StatusInternalServerError, codeInternal, "tool market not configured")
		return
	}
	var req publishToolPackageRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	name := strings.TrimSpace(req.Name)
	if !packageNameRe.MatchString(name) {
		writeError(w, r, http.StatusBadRequest, codeBadRequest, fmt.Sprintf("name is required (1..%d chars, letters/digits/space/dot/underscore/hyphen)", maxPackageNameLen))
		return
	}
	version := strings.TrimSpace(req.Version)
	if !packageVersionRe.MatchString(version) {
		writeError(w, r, http.StatusBadRequest, codeBadRequest, fmt.Sprintf("version is required (1..%d chars, letters/digits/dot/underscore/hyphen)", maxPackageVersionLen))
		return
	}
	if len(req.Description) > maxPackageDescriptionLen {
		writeError(w, r, http.StatusBadRequest, codeBadRequest, fmt.Sprintf("description must be at most %d chars", maxPackageDescriptionLen))
		return
	}
	author := strings.TrimSpace(req.Author)
	if author == "" {
		author = p.UserID
	}
	if len(author) > maxPackageAuthorLen {
		writeError(w, r, http.StatusBadRequest, codeBadRequest, fmt.Sprintf("author must be at most %d chars", maxPackageAuthorLen))
		return
	}
	if req.Tools == nil {
		writeError(w, r, http.StatusBadRequest, codeBadRequest, "tools is required")
		return
	}
	canonical, err := validatePackageTools(req.Tools)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}
	// Scope decision (design/83 C3.1): the publishing role determines the
	// visibility scope — no client-controlled scope parameter.
	var tenantID *string
	if !isPlatformAdmin(p) {
		tid := p.TenantID
		tenantID = &tid
	}
	created, err := s.Market.Create(r.Context(), &NewToolPackage{
		TenantID:    tenantID,
		Name:        name,
		Version:     version,
		Description: strings.TrimSpace(req.Description),
		Author:      author,
		Tools:       req.Tools,
		Signature:   toolsDigest(canonical),
	})
	if err != nil {
		mapToolPackageRepoError(w, r, err)
		return
	}
	s.recordAudit(r.Context(), AdminOp{
		EventID:  newEventID(),
		TenantID: p.TenantID,
		ActorID:  p.UserID,
		Action:   "tool_package.publish",
		Target:   created.ID,
		Details:  map[string]any{"name": created.Name, "version": created.Version, "scope": packageScope(created)},
		TraceID:  httpx.TraceIDFrom(r),
	})
	httpx.WriteJSON(w, http.StatusCreated, newToolPackageResponse(created, true))
}

// handleListToolPackages serves GET /v1/admin/tool-packages: the market
// list with keyword search and paging. Visibility (design/83 C3.1):
// platform-level packages plus the requesting tenant's own packages.
func (s *Server) handleListToolPackages(w http.ResponseWriter, r *http.Request) {
	p, ok := adminauth.FromContext(r.Context())
	if !ok {
		writeError(w, r, http.StatusUnauthorized, codeUnauthorized, "unauthenticated")
		return
	}
	if s.Market == nil {
		writeError(w, r, http.StatusInternalServerError, codeInternal, "tool market not configured")
		return
	}
	tenantScope, ok := s.resolveTenant(w, r, p)
	if !ok {
		return
	}
	page, err := parsePage(r.URL.Query())
	if err != nil {
		writeError(w, r, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}
	keyword := strings.TrimSpace(r.URL.Query().Get("keyword"))
	if len(keyword) > maxPackageKeywordLen {
		writeError(w, r, http.StatusBadRequest, codeBadRequest, fmt.Sprintf("keyword must be at most %d chars", maxPackageKeywordLen))
		return
	}
	list, total, err := s.Market.List(r.Context(), tenantScope, ToolPackageFilter{Keyword: keyword}, page)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, codeInternal, "list tool packages failed")
		return
	}
	items := make([]toolPackageResponse, 0, len(list))
	for i := range list {
		items = append(items, newToolPackageResponse(&list[i], false))
	}
	httpx.WriteJSON(w, http.StatusOK, toolPackageListResponse{Items: items, Total: total, Page: page.Number, PageSize: page.Size})
}

// loadVisiblePackage loads the package and enforces the visibility rule
// (design/83 C3.1): a tenant-scoped package is reachable only from its
// owning tenant's scope; platform-level packages are reachable from
// every scope. Out-of-scope lookups answer 404 without leaking
// existence. The stored signature is verified before any read or
// mutation.
func (s *Server) loadVisiblePackage(w http.ResponseWriter, r *http.Request, packageID, scopeTenant string) (*ToolPackage, bool) {
	if !requireUUID(w, r, "package_id", packageID) {
		return nil, false
	}
	if s.Market == nil {
		writeError(w, r, http.StatusInternalServerError, codeInternal, "tool market not configured")
		return nil, false
	}
	pkg, err := s.Market.Get(r.Context(), packageID, scopeTenant)
	if err != nil {
		if errors.Is(err, ErrToolPackageNotFound) {
			writeError(w, r, http.StatusNotFound, codeNotFound, "tool package not found")
			return nil, false
		}
		writeError(w, r, http.StatusInternalServerError, codeInternal, "load tool package failed")
		return nil, false
	}
	if pkg.TenantID != nil && *pkg.TenantID != scopeTenant {
		writeError(w, r, http.StatusNotFound, codeNotFound, "tool package not found")
		return nil, false
	}
	if err := verifyPackageSignature(pkg); err != nil {
		mapToolPackageRepoError(w, r, err)
		return nil, false
	}
	return pkg, true
}

// handleGetToolPackage serves GET /v1/admin/tool-packages/{id}: full
// detail including the tool list, signature-verified on read. The access
// scope is the session tenant (platform admins browse with their session
// tenant, tenant admins with their own).
func (s *Server) handleGetToolPackage(w http.ResponseWriter, r *http.Request) {
	p, ok := adminauth.FromContext(r.Context())
	if !ok {
		writeError(w, r, http.StatusUnauthorized, codeUnauthorized, "unauthenticated")
		return
	}
	pkg, ok := s.loadVisiblePackage(w, r, r.PathValue("packageID"), p.TenantID)
	if !ok {
		return
	}
	httpx.WriteJSON(w, http.StatusOK, newToolPackageResponse(pkg, true))
}

// handleInstallToolPackage serves POST /v1/admin/tool-packages/{id}/install:
// installs the package into the resolved tenant scope (design/83 C3.1).
// Duplicate installs are idempotent no-ops answering 200 with the
// original installed_at.
func (s *Server) handleInstallToolPackage(w http.ResponseWriter, r *http.Request) {
	p, ok := adminauth.FromContext(r.Context())
	if !ok {
		writeError(w, r, http.StatusUnauthorized, codeUnauthorized, "unauthenticated")
		return
	}
	tenantID, ok := s.resolveTenant(w, r, p)
	if !ok {
		return
	}
	packageID := r.PathValue("packageID")
	if _, ok := s.loadVisiblePackage(w, r, packageID, tenantID); !ok {
		return
	}
	installed, err := s.Market.Install(r.Context(), packageID, tenantID, p.UserID)
	if err != nil {
		mapToolPackageRepoError(w, r, err)
		return
	}
	s.recordAudit(r.Context(), AdminOp{
		EventID:  newEventID(),
		TenantID: tenantID,
		ActorID:  p.UserID,
		Action:   "tool_package.install",
		Target:   packageID,
		Details:  map[string]any{"name": installed.Name, "version": installed.Version},
		TraceID:  httpx.TraceIDFrom(r),
	})
	httpx.WriteJSON(w, http.StatusOK, newToolPackageResponse(installed, false))
}

// handleUninstallToolPackage serves POST /v1/admin/tool-packages/{id}/uninstall:
// removes the install record for the resolved tenant. Uninstalling a
// package that is not installed is an idempotent no-op.
func (s *Server) handleUninstallToolPackage(w http.ResponseWriter, r *http.Request) {
	p, ok := adminauth.FromContext(r.Context())
	if !ok {
		writeError(w, r, http.StatusUnauthorized, codeUnauthorized, "unauthenticated")
		return
	}
	tenantID, ok := s.resolveTenant(w, r, p)
	if !ok {
		return
	}
	packageID := r.PathValue("packageID")
	if _, ok := s.loadVisiblePackage(w, r, packageID, tenantID); !ok {
		return
	}
	removed, err := s.Market.Uninstall(r.Context(), packageID, tenantID)
	if err != nil {
		mapToolPackageRepoError(w, r, err)
		return
	}
	s.recordAudit(r.Context(), AdminOp{
		EventID:  newEventID(),
		TenantID: tenantID,
		ActorID:  p.UserID,
		Action:   "tool_package.uninstall",
		Target:   packageID,
		Details:  map[string]any{"name": removed.Name, "version": removed.Version},
		TraceID:  httpx.TraceIDFrom(r),
	})
	httpx.WriteJSON(w, http.StatusOK, newToolPackageResponse(removed, false))
}

// ---------------------------------------------------------------------------
// PostgreSQL repository
// ---------------------------------------------------------------------------

// toolPackageCols rebuilds a ToolPackage (uuid cast to text, see
// approval.PGTicketRepo); the trailing installed_at placeholder is filled
// by the per-tenant join parameter.
const toolPackageCols = `p.id::text, p.tenant_id::text, p.name, p.version,
	COALESCE(p.description,''), p.author, p.tools, p.signature,
	p.published_at, p.status, i.installed_at`

// pgToolPackageRepo is the PostgreSQL ToolPackageRepo (design/83 C3.1,
// migration 0004).
type pgToolPackageRepo struct {
	pool pgxPooler
}

// NewPGToolPackageRepo builds a tool package repository over an existing
// pgx pool.
func NewPGToolPackageRepo(pool pgxPooler) *pgToolPackageRepo {
	return &pgToolPackageRepo{pool: pool}
}

func scanToolPackage(row pgx.Row) (*ToolPackage, error) {
	var (
		pkg      ToolPackage
		toolsRaw []byte
	)
	err := row.Scan(&pkg.ID, &pkg.TenantID, &pkg.Name, &pkg.Version,
		&pkg.Description, &pkg.Author, &toolsRaw, &pkg.Signature,
		&pkg.PublishedAt, &pkg.Status, &pkg.InstalledAt)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(toolsRaw, &pkg.Tools); err != nil {
		return nil, err
	}
	if pkg.Tools == nil {
		pkg.Tools = []protocol.MCPTool{}
	}
	pkg.ToolCount = len(pkg.Tools)
	return &pkg, nil
}

// Create inserts a new package; the (name, version) partial unique
// indexes map collisions onto ErrToolPackageNameExists.
func (r *pgToolPackageRepo) Create(ctx context.Context, p *NewToolPackage) (*ToolPackage, error) {
	tools, err := json.Marshal(p.Tools)
	if err != nil {
		return nil, err
	}
	var id string
	err = r.pool.QueryRow(ctx, `INSERT INTO adc_tool_packages
		(tenant_id, name, version, description, author, tools, signature)
		VALUES ($1::uuid, $2, $3, $4, $5, $6::jsonb, $7)
		RETURNING id::text`,
		p.TenantID, p.Name, p.Version, p.Description, p.Author, string(tools), p.Signature).Scan(&id)
	if isUniqueViolation(err) {
		return nil, ErrToolPackageNameExists
	}
	if err != nil {
		return nil, err
	}
	return r.getByID(ctx, id, "")
}

// getByID loads one package with the optional per-tenant install join.
func (r *pgToolPackageRepo) getByID(ctx context.Context, id, tenantScope string) (*ToolPackage, error) {
	row := r.pool.QueryRow(ctx, `SELECT `+toolPackageCols+`
		FROM adc_tool_packages p
		LEFT JOIN adc_tool_package_installs i
			ON i.package_id = p.id AND i.tenant_id = $2::uuid
		WHERE p.id = $1::uuid`, id, tenantScope)
	pkg, err := scanToolPackage(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrToolPackageNotFound
	}
	if err != nil {
		return nil, err
	}
	return pkg, nil
}

// Get loads one package; InstalledAt reports the tenant scope's install
// time (nil when not installed). Visibility filtering happens in the
// handler, not here, so platform admins can still reach tenant packages.
func (r *pgToolPackageRepo) Get(ctx context.Context, id, tenantScope string) (*ToolPackage, error) {
	return r.getByID(ctx, id, tenantScope)
}

// List pages the market visible to the tenant scope (platform packages
// plus the tenant's own), newest first, joining the per-tenant install
// state.
func (r *pgToolPackageRepo) List(ctx context.Context, tenantScope string, f ToolPackageFilter, p Page) ([]ToolPackage, int, error) {
	pattern := "%" + f.Keyword + "%"
	rows, err := r.pool.Query(ctx, `SELECT `+toolPackageCols+`, count(*) OVER () AS total
		FROM adc_tool_packages p
		LEFT JOIN adc_tool_package_installs i
			ON i.package_id = p.id AND i.tenant_id = $1::uuid
		WHERE (p.tenant_id IS NULL OR p.tenant_id = $1::uuid)
		  AND ($2 = '' OR p.name ILIKE $3 OR p.description ILIKE $3 OR p.author ILIKE $3)
		ORDER BY p.published_at DESC, p.id
		LIMIT $4 OFFSET $5`,
		tenantScope, f.Keyword, pattern, p.Size, p.offset())
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var (
		out   []ToolPackage
		total int
	)
	for rows.Next() {
		var (
			pkg      ToolPackage
			toolsRaw []byte
		)
		if err := rows.Scan(&pkg.ID, &pkg.TenantID, &pkg.Name, &pkg.Version,
			&pkg.Description, &pkg.Author, &toolsRaw, &pkg.Signature,
			&pkg.PublishedAt, &pkg.Status, &pkg.InstalledAt, &total); err != nil {
			return nil, 0, err
		}
		if err := json.Unmarshal(toolsRaw, &pkg.Tools); err != nil {
			return nil, 0, err
		}
		if pkg.Tools == nil {
			pkg.Tools = []protocol.MCPTool{}
		}
		pkg.ToolCount = len(pkg.Tools)
		out = append(out, pkg)
	}
	return out, total, rows.Err()
}

// Install records the per-tenant install (ON CONFLICT DO NOTHING keeps
// duplicate installs idempotent) and returns the package with the
// tenant's installed_at set.
func (r *pgToolPackageRepo) Install(ctx context.Context, packageID, tenantID, userID string) (*ToolPackage, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx, `INSERT INTO adc_tool_package_installs
		(package_id, tenant_id, installed_by)
		VALUES ($1::uuid, $2::uuid, $3::uuid)
		ON CONFLICT (package_id, tenant_id) DO NOTHING`,
		packageID, tenantID, userID)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 1 {
		// Watermark only moves on a genuinely new install so idempotent
		// re-installs keep the original installed_at.
		if _, err := tx.Exec(ctx, `UPDATE adc_tool_packages
			SET installed_at = now() WHERE id = $1::uuid`, packageID); err != nil {
			return nil, err
		}
	}
	row := tx.QueryRow(ctx, `SELECT `+toolPackageCols+`
		FROM adc_tool_packages p
		LEFT JOIN adc_tool_package_installs i
			ON i.package_id = p.id AND i.tenant_id = $2::uuid
		WHERE p.id = $1::uuid`, packageID, tenantID)
	pkg, err := scanToolPackage(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrToolPackageNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return pkg, nil
}

// Uninstall removes the tenant's install record (deleting an absent
// record is a no-op, keeping uninstall idempotent) and returns the
// package with InstalledAt nil.
func (r *pgToolPackageRepo) Uninstall(ctx context.Context, packageID, tenantID string) (*ToolPackage, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `DELETE FROM adc_tool_package_installs
		WHERE package_id = $1::uuid AND tenant_id = $2::uuid`,
		packageID, tenantID); err != nil {
		return nil, err
	}
	row := tx.QueryRow(ctx, `SELECT `+toolPackageCols+`
		FROM adc_tool_packages p
		LEFT JOIN adc_tool_package_installs i
			ON i.package_id = p.id AND i.tenant_id = $2::uuid
		WHERE p.id = $1::uuid`, packageID, tenantID)
	pkg, err := scanToolPackage(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrToolPackageNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return pkg, nil
}
