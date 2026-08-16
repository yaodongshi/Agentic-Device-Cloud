package adminapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"adc.dev/ce/internal/adminauth"
	"adc.dev/ce/internal/httpx"
)

// Tenant repository sentinels, shared with the device and api key
// aggregates whose storage gates on tenant existence and status.
var (
	ErrTenantNotFound = errors.New("adminapi: tenant not found")
	// ErrTenantSuspended maps to 403 code 13002 (design/33): suspended
	// tenants refuse new devices and agent keys.
	ErrTenantSuspended = errors.New("adminapi: tenant suspended")
	// ErrTenantCodeConflict maps to 409 code 13004 (design/33 3.1.3).
	ErrTenantCodeConflict = errors.New("adminapi: tenant code already exists")
	// ErrTenantQuotaBelowUsage maps to 403 code 13003 (design/33 3.1.5).
	ErrTenantQuotaBelowUsage = errors.New("adminapi: quota below current usage")
)

// Tenant statuses as the API spells them (design/33 3.1.x). Note design/33
// 3.1.2 calls the filter value "disabled" while adc_tenants.status CHECK
// uses SUSPENDED (design/32 3.1); this package follows the DB enum and
// spells it "suspended" everywhere.
const (
	tenantStatusActive    = "active"
	tenantStatusSuspended = "suspended"
)

// tenantCodeRe enforces the tenant code charset (SEC-20): lowercase
// letters, digits and inner dashes, 1-64 chars; the handler additionally
// requires a 2-char minimum (no single-character tenant codes).
var tenantCodeRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?$`)

// Quota is the tenant quota model of design/33 3.1.3. max_devices and
// monthly_call_limit map to the adc_tenants columns quota_devices and
// quota_calls_monthly; max_agent_keys and audit_retention_days have no
// dedicated column in migration 0001 (design/32 3.1 defines only the two
// quota columns plus quota_concurrent), so the PG repository carries them
// inside the metadata JSONB until the schema gains columns.
type Quota struct {
	MaxDevices         int
	MaxAgentKeys       int
	MonthlyCallLimit   int64
	AuditRetentionDays int
}

// defaultQuota is applied when POST /v1/admin/tenants omits quota fields:
// max_devices and monthly_call_limit follow the adc_tenants column defaults
// (design/32 3.1), max_agent_keys and audit_retention_days follow the
// design/33 3.1.3 example and the NFR-006 retention default.
var defaultQuota = Quota{
	MaxDevices:         100,
	MaxAgentKeys:       20,
	MonthlyCallLimit:   100000,
	AuditRetentionDays: 180,
}

// maxDevicesHardCap is the platform-wide per-tenant device ceiling
// (NFR-002: 10k devices per tenant).
const maxDevicesHardCap = 10000

// validate rejects nonsensical quota values.
func (q Quota) validate() error {
	if q.MaxDevices < 0 || q.MaxDevices > maxDevicesHardCap {
		return errors.New("max_devices must be between 0 and 10000")
	}
	if q.MaxAgentKeys < 0 {
		return errors.New("max_agent_keys must not be negative")
	}
	if q.MonthlyCallLimit < 0 {
		return errors.New("monthly_call_limit must not be negative")
	}
	if q.AuditRetentionDays < 1 {
		return errors.New("audit_retention_days must be at least 1")
	}
	return nil
}

// Tenant is one adc_tenants row (design/32 3.1) in API spelling (lowercase
// status). UsedDevices/UsedCallsMonth feed the quota-below-usage guard;
// DeviceCount/AgentKeyCount are populated on list queries only.
type Tenant struct {
	ID             string
	Code           string
	Name           string
	Status         string
	Quota          Quota
	UsedDevices    int
	UsedCallsMonth int64
	DeviceCount    int
	AgentKeyCount  int
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// TenantFilter narrows tenant list queries (design/33 3.1.2).
type TenantFilter struct {
	Status   string // "", active or suspended
	Keyword  string // case-insensitive name substring
	TenantID string // constrain to one tenant (tenant_admin's own row)
}

// TenantRepo persists tenants (design/31 LLD 3.4.2 seam). Update replaces
// the mutable fields (quota + status) and is guarded by the usage counts
// inside the repository so quota can never drop below usage.
type TenantRepo interface {
	Create(ctx context.Context, t *Tenant) (*Tenant, error)
	Get(ctx context.Context, tenantID string) (*Tenant, error)
	Update(ctx context.Context, tenantID string, t *Tenant) (*Tenant, error)
	List(ctx context.Context, f TenantFilter, p Page) ([]Tenant, int, error)
}

// ---------------------------------------------------------------------------
// handlers
// ---------------------------------------------------------------------------

// quotaRequest is the partial quota body of POST/PATCH tenants; pointer
// fields distinguish absent from zero (design/33 3.1.3/3.1.5).
type quotaRequest struct {
	MaxDevices         *int   `json:"max_devices"`
	MaxAgentKeys       *int   `json:"max_agent_keys"`
	MonthlyCallLimit   *int64 `json:"monthly_call_limit"`
	AuditRetentionDays *int   `json:"audit_retention_days"`
}

// applyQuota returns base overlaid with the non-nil fields of req.
func (req *quotaRequest) applyQuota(base Quota) Quota {
	if req == nil {
		return base
	}
	if req.MaxDevices != nil {
		base.MaxDevices = *req.MaxDevices
	}
	if req.MaxAgentKeys != nil {
		base.MaxAgentKeys = *req.MaxAgentKeys
	}
	if req.MonthlyCallLimit != nil {
		base.MonthlyCallLimit = *req.MonthlyCallLimit
	}
	if req.AuditRetentionDays != nil {
		base.AuditRetentionDays = *req.AuditRetentionDays
	}
	return base
}

type quotaResponse struct {
	MaxDevices         int   `json:"max_devices"`
	MaxAgentKeys       int   `json:"max_agent_keys"`
	MonthlyCallLimit   int64 `json:"monthly_call_limit"`
	AuditRetentionDays int   `json:"audit_retention_days"`
}

func newQuotaResponse(q Quota) quotaResponse {
	return quotaResponse{
		MaxDevices:         q.MaxDevices,
		MaxAgentKeys:       q.MaxAgentKeys,
		MonthlyCallLimit:   q.MonthlyCallLimit,
		AuditRetentionDays: q.AuditRetentionDays,
	}
}

// tenantResponse mirrors design/33 3.1.3/3.1.4.
type tenantResponse struct {
	TenantID  string        `json:"tenant_id"`
	Name      string        `json:"name"`
	Code      string        `json:"code"`
	Status    string        `json:"status"`
	Quota     quotaResponse `json:"quota"`
	CreatedAt time.Time     `json:"created_at"`
	UpdatedAt time.Time     `json:"updated_at"`
}

func newTenantResponse(t *Tenant) tenantResponse {
	return tenantResponse{
		TenantID:  t.ID,
		Name:      t.Name,
		Code:      t.Code,
		Status:    t.Status,
		Quota:     newQuotaResponse(t.Quota),
		CreatedAt: t.CreatedAt.UTC(),
		UpdatedAt: t.UpdatedAt.UTC(),
	}
}

// tenantListItem mirrors design/33 3.1.2.
type tenantListItem struct {
	TenantID      string    `json:"tenant_id"`
	Name          string    `json:"name"`
	Status        string    `json:"status"`
	DeviceCount   int       `json:"device_count"`
	AgentKeyCount int       `json:"agent_key_count"`
	CreatedAt     time.Time `json:"created_at"`
}

type tenantListResponse struct {
	Items    []tenantListItem `json:"items"`
	Total    int              `json:"total"`
	Page     int              `json:"page"`
	PageSize int              `json:"page_size"`
}

// createTenantRequest mirrors design/33 3.1.3.
type createTenantRequest struct {
	Name  string        `json:"name"`
	Code  string        `json:"code"`
	Quota *quotaRequest `json:"quota"`
}

// patchTenantRequest mirrors design/33 3.1.5.
type patchTenantRequest struct {
	Quota        *quotaRequest `json:"quota"`
	Status       *string       `json:"status"`
	ChangeReason string        `json:"change_reason"`
}

// handleListTenants serves GET /v1/admin/tenants. Platform admins see the
// full list; every other role is locked to its own tenant row (design/33
// 3.1.2 note).
func (s *Server) handleListTenants(w http.ResponseWriter, r *http.Request) {
	p, ok := adminauth.FromContext(r.Context())
	if !ok {
		writeError(w, r, http.StatusUnauthorized, codeUnauthorized, "unauthenticated")
		return
	}
	page, err := parsePage(r.URL.Query())
	if err != nil {
		writeError(w, r, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}
	f := TenantFilter{
		Status:  strings.TrimSpace(r.URL.Query().Get("status")),
		Keyword: strings.TrimSpace(r.URL.Query().Get("keyword")),
	}
	if f.Status != "" && f.Status != tenantStatusActive && f.Status != tenantStatusSuspended {
		writeError(w, r, http.StatusBadRequest, codeBadRequest, "status must be active or suspended")
		return
	}
	if !isPlatformAdmin(p) {
		f.TenantID = p.TenantID
	}
	list, total, err := s.Tenants.List(r.Context(), f, page)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, codeInternal, "list tenants failed")
		return
	}
	items := make([]tenantListItem, 0, len(list))
	for i := range list {
		items = append(items, tenantListItem{
			TenantID:      list[i].ID,
			Name:          list[i].Name,
			Status:        list[i].Status,
			DeviceCount:   list[i].DeviceCount,
			AgentKeyCount: list[i].AgentKeyCount,
			CreatedAt:     list[i].CreatedAt.UTC(),
		})
	}
	httpx.WriteJSON(w, http.StatusOK, tenantListResponse{Items: items, Total: total, Page: page.Number, PageSize: page.Size})
}

// handleCreateTenant serves POST /v1/admin/tenants. Tenant creation is a
// platform_admin exclusive (design/33 1.2, 3.1.3).
func (s *Server) handleCreateTenant(w http.ResponseWriter, r *http.Request) {
	p, ok := adminauth.FromContext(r.Context())
	if !ok {
		writeError(w, r, http.StatusUnauthorized, codeUnauthorized, "unauthenticated")
		return
	}
	if !isPlatformAdmin(p) {
		writeError(w, r, http.StatusForbidden, codeForbidden, "tenant creation requires the platform_admin role")
		return
	}
	var req createTenantRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Name == "" || len(req.Name) > 255 {
		writeError(w, r, http.StatusBadRequest, codeBadRequest, "name is required (max 255 chars)")
		return
	}
	if len(req.Code) < 2 || len(req.Code) > 64 || !tenantCodeRe.MatchString(req.Code) {
		writeError(w, r, http.StatusBadRequest, codeBadRequest, "code must be 2-64 chars of lowercase letters, digits and inner dashes")
		return
	}
	quota := req.Quota.applyQuota(defaultQuota)
	if err := quota.validate(); err != nil {
		writeError(w, r, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}
	now := s.now().UTC()
	created, err := s.Tenants.Create(r.Context(), &Tenant{
		Code:      req.Code,
		Name:      req.Name,
		Status:    tenantStatusActive,
		Quota:     quota,
		CreatedAt: now,
		UpdatedAt: now,
	})
	if err != nil {
		if errors.Is(err, ErrTenantCodeConflict) {
			writeError(w, r, http.StatusConflict, codeTenantCodeExists, "tenant code already exists")
			return
		}
		writeError(w, r, http.StatusInternalServerError, codeInternal, "create tenant failed")
		return
	}
	s.recordAudit(r.Context(), AdminOp{
		EventID:  newEventID(),
		TenantID: created.ID,
		ActorID:  p.UserID,
		Action:   "tenant.create",
		Target:   created.ID,
		TraceID:  httpx.TraceIDFrom(r),
	})
	httpx.WriteJSON(w, http.StatusCreated, newTenantResponse(created))
}

// handlePatchTenant serves PATCH /v1/admin/tenants/{tenantID}: quota
// updates and suspension, both platform_admin exclusives. V1 suspension
// semantics: setting status to "suspended" only flips the ledger marker.
// Kicking the tenant's device connections offline and voiding in-flight
// approval tickets (FR-008 exception path, design/31 3.4.3) is wired with
// the data plane in a follow-up change; the status marker is the contract
// the data plane and Agent API already consume (agentauth disables keys of
// non-ACTIVE tenants).
func (s *Server) handlePatchTenant(w http.ResponseWriter, r *http.Request) {
	p, ok := adminauth.FromContext(r.Context())
	if !ok {
		writeError(w, r, http.StatusUnauthorized, codeUnauthorized, "unauthenticated")
		return
	}
	if !isPlatformAdmin(p) {
		writeError(w, r, http.StatusForbidden, codeForbidden, "tenant updates require the platform_admin role")
		return
	}
	tenantID := r.PathValue("tenantID")
	if !requireUUID(w, r, "tenant_id", tenantID) {
		return
	}
	var req patchTenantRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.ChangeReason == "" {
		writeError(w, r, http.StatusBadRequest, codeBadRequest, "change_reason is required")
		return
	}
	if req.Status != nil && *req.Status != tenantStatusActive && *req.Status != tenantStatusSuspended {
		writeError(w, r, http.StatusBadRequest, codeBadRequest, "status must be active or suspended")
		return
	}
	current, err := s.Tenants.Get(r.Context(), tenantID)
	if err != nil {
		if errors.Is(err, ErrTenantNotFound) {
			writeError(w, r, http.StatusNotFound, codeTenantNotFound, "tenant not found")
			return
		}
		writeError(w, r, http.StatusInternalServerError, codeInternal, "load tenant failed")
		return
	}
	next := *current
	next.Quota = req.Quota.applyQuota(current.Quota)
	if err := next.Quota.validate(); err != nil {
		writeError(w, r, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}
	if req.Status != nil {
		next.Status = *req.Status
	}
	if next.Quota.MaxDevices < current.UsedDevices || next.Quota.MonthlyCallLimit < current.UsedCallsMonth {
		writeError(w, r, http.StatusForbidden, codeTenantQuota, "quota below current usage")
		return
	}
	updated, err := s.Tenants.Update(r.Context(), tenantID, &next)
	if err != nil {
		switch {
		case errors.Is(err, ErrTenantNotFound):
			writeError(w, r, http.StatusNotFound, codeTenantNotFound, "tenant not found")
		case errors.Is(err, ErrTenantQuotaBelowUsage):
			writeError(w, r, http.StatusForbidden, codeTenantQuota, "quota below current usage")
		default:
			writeError(w, r, http.StatusInternalServerError, codeInternal, "update tenant failed")
		}
		return
	}
	s.recordAudit(r.Context(), AdminOp{
		EventID:  newEventID(),
		TenantID: tenantID,
		ActorID:  p.UserID,
		Action:   "tenant.update",
		Target:   tenantID,
		Reason:   req.ChangeReason,
		TraceID:  httpx.TraceIDFrom(r),
	})
	httpx.WriteJSON(w, http.StatusOK, newTenantResponse(updated))
}

// ---------------------------------------------------------------------------
// PostgreSQL repository
// ---------------------------------------------------------------------------

// tenantCols rebuilds a Tenant; UUID columns are cast to text because pgx
// v5.10 has no binary uuid-to-string scan plan (same approach as
// approval.PGTicketRepo).
const tenantCols = `id::text, code, name, status, quota_devices, quota_calls_monthly,
	used_devices, used_calls_month, metadata, created_at, updated_at`

// tenantMeta carries the quota fields without a dedicated column inside
// adc_tenants.metadata (see Quota).
type tenantMeta struct {
	MaxAgentKeys       int `json:"max_agent_keys"`
	AuditRetentionDays int `json:"audit_retention_days"`
}

// pgTenantRepo is the PostgreSQL TenantRepo (design/32 3.1).
type pgTenantRepo struct {
	pool pgxPooler
}

// NewPGTenantRepo builds a tenant repository over an existing pgx pool.
func NewPGTenantRepo(pool pgxPooler) *pgTenantRepo {
	return &pgTenantRepo{pool: pool}
}

// apiTenantStatus maps the API spelling to the adc_tenants.status CHECK
// values (design/32 3.1 stores uppercase).
func apiTenantStatus(s string) string { return strings.ToUpper(s) }

// dbTenantStatus maps the DB spelling back to the API lowercase form.
func dbTenantStatus(s string) string { return strings.ToLower(s) }

func (r *pgTenantRepo) scanTenant(row pgx.Row) (*Tenant, error) {
	var (
		t        Tenant
		status   string
		metaJSON []byte
	)
	err := row.Scan(&t.ID, &t.Code, &t.Name, &status, &t.Quota.MaxDevices, &t.Quota.MonthlyCallLimit,
		&t.UsedDevices, &t.UsedCallsMonth, &metaJSON, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		return nil, err
	}
	t.Status = dbTenantStatus(status)
	var meta tenantMeta
	if len(metaJSON) > 0 {
		if err := json.Unmarshal(metaJSON, &meta); err != nil {
			return nil, err
		}
	}
	t.Quota.MaxAgentKeys = meta.MaxAgentKeys
	t.Quota.AuditRetentionDays = meta.AuditRetentionDays
	return &t, nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// Create inserts a tenant; the partial unique index uq_tenants_code maps
// conflicts onto ErrTenantCodeConflict (design/32 3.1).
func (r *pgTenantRepo) Create(ctx context.Context, t *Tenant) (*Tenant, error) {
	meta, err := json.Marshal(tenantMeta{
		MaxAgentKeys:       t.Quota.MaxAgentKeys,
		AuditRetentionDays: t.Quota.AuditRetentionDays,
	})
	if err != nil {
		return nil, err
	}
	row := r.pool.QueryRow(ctx, `INSERT INTO adc_tenants
		(code, name, status, quota_devices, quota_calls_monthly, metadata)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING `+tenantCols,
		t.Code, t.Name, apiTenantStatus(t.Status), t.Quota.MaxDevices, t.Quota.MonthlyCallLimit, string(meta))
	created, err := r.scanTenant(row)
	if isUniqueViolation(err) {
		return nil, ErrTenantCodeConflict
	}
	if err != nil {
		return nil, err
	}
	return created, nil
}

// Get loads one tenant by id.
func (r *pgTenantRepo) Get(ctx context.Context, tenantID string) (*Tenant, error) {
	row := r.pool.QueryRow(ctx, `SELECT `+tenantCols+` FROM adc_tenants
		WHERE id = $1::uuid AND deleted_at IS NULL`, tenantID)
	t, err := r.scanTenant(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrTenantNotFound
	}
	if err != nil {
		return nil, err
	}
	return t, nil
}

// Update replaces quota and status in one conditional UPDATE. The
// used_devices/used_calls_month guards keep quota above usage atomically
// (design/32 6.2 ledger semantics); a zero-row update is re-read and
// classified.
func (r *pgTenantRepo) Update(ctx context.Context, tenantID string, t *Tenant) (*Tenant, error) {
	meta, err := json.Marshal(tenantMeta{
		MaxAgentKeys:       t.Quota.MaxAgentKeys,
		AuditRetentionDays: t.Quota.AuditRetentionDays,
	})
	if err != nil {
		return nil, err
	}
	row := r.pool.QueryRow(ctx, `UPDATE adc_tenants
		SET quota_devices = $2, quota_calls_monthly = $3, status = $4,
		    metadata = $5, updated_at = now()
		WHERE id = $1::uuid AND deleted_at IS NULL
		  AND used_devices <= $2 AND used_calls_month <= $3
		RETURNING `+tenantCols,
		tenantID, t.Quota.MaxDevices, t.Quota.MonthlyCallLimit, apiTenantStatus(t.Status), string(meta))
	updated, err := r.scanTenant(row)
	if err == nil {
		return updated, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	cur, getErr := r.Get(ctx, tenantID)
	if getErr != nil {
		return nil, getErr
	}
	if cur.UsedDevices > t.Quota.MaxDevices || cur.UsedCallsMonth > t.Quota.MonthlyCallLimit {
		return nil, ErrTenantQuotaBelowUsage
	}
	return nil, ErrTenantNotFound
}

// List pages tenants with per-row device/key counts and the window total
// (design/33 3.1.2). Filter presence is encoded with sentinel parameters so
// the query stays fully parameterized.
func (r *pgTenantRepo) List(ctx context.Context, f TenantFilter, p Page) ([]Tenant, int, error) {
	status := apiTenantStatus(f.Status)
	keyword := f.Keyword
	pattern := ""
	if keyword != "" {
		pattern = "%" + keyword + "%"
	}
	var tenantParam any // nil = all tenants
	if f.TenantID != "" {
		tenantParam = f.TenantID
	}
	rows, err := r.pool.Query(ctx, `
		SELECT t.id::text, t.name, t.status, t.created_at,
		       (SELECT count(*) FROM adc_devices d
		         WHERE d.tenant_id = t.id AND d.deleted_at IS NULL) AS device_count,
		       (SELECT count(*) FROM adc_agent_api_keys k
		         WHERE k.tenant_id = t.id AND k.revoked_at IS NULL) AS key_count,
		       count(*) OVER () AS total
		  FROM adc_tenants t
		 WHERE t.deleted_at IS NULL
		   AND ($1::text = '' OR t.status = $1)
		   AND ($2::text = '' OR t.name ILIKE $3)
		   AND ($4::uuid IS NULL OR t.id = $4)
		 ORDER BY t.created_at DESC, t.id
		 LIMIT $5 OFFSET $6`,
		status, keyword, pattern, tenantParam, p.Size, p.offset())
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var (
		out   []Tenant
		total int
	)
	for rows.Next() {
		var t Tenant
		var dbStatus string
		if err := rows.Scan(&t.ID, &t.Name, &dbStatus, &t.CreatedAt,
			&t.DeviceCount, &t.AgentKeyCount, &total); err != nil {
			return nil, 0, err
		}
		t.Status = dbTenantStatus(dbStatus)
		out = append(out, t)
	}
	return out, total, rows.Err()
}
