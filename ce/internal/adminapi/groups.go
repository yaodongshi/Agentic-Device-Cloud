package adminapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"adc.dev/ce/internal/adminauth"
	"adc.dev/ce/internal/httpx"
)

// Device groups (FR-011, design/82 B1.3): tenant-scoped tree buckets
// (design/32 3.4 adc_device_groups) usable for device queries and later
// for approval-policy scoping. Devices join through the single
// adc_devices.group_id column; PATCH /v1/admin/devices/{id} with
// group_id attaches/detaches (see devices.go). Deleting a group soft
// deletes it and detaches its devices (design/32: "删组时设备自动脱组，
// 不误删设备").

// Group repository sentinels.
var (
	ErrGroupNotFound   = errors.New("adminapi: device group not found")
	ErrGroupNameExists = errors.New("adminapi: device group name already exists")
)

// DeviceGroup is one adc_device_groups row (design/32 3.4) in API
// spelling. Metadata is not exposed in V1 (the column exists for future
// use, like approval-policy scoping).
type DeviceGroup struct {
	ID          string
	TenantID    string
	ParentID    *string
	Name        string
	Description string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// GroupRepo persists device groups (design/31 store seam).
type GroupRepo interface {
	Create(ctx context.Context, g *DeviceGroup) (*DeviceGroup, error)
	Get(ctx context.Context, groupID string) (*DeviceGroup, error)
	List(ctx context.Context, tenantID string, p Page) ([]DeviceGroup, int, error)
	Update(ctx context.Context, groupID string, name, description *string) (*DeviceGroup, error)
	// Delete soft deletes the group and detaches its devices in one
	// transaction (design/32 3.4: devices survive, group_id -> NULL).
	Delete(ctx context.Context, groupID string) error
}

func mapGroupRepoError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ErrGroupNotFound):
		writeError(w, r, http.StatusNotFound, codeGroupNotFound, "device group not found")
	case errors.Is(err, ErrGroupNameExists):
		writeError(w, r, http.StatusConflict, codeGroupNameExists, "device group name already exists")
	case errors.Is(err, ErrTenantNotFound):
		writeError(w, r, http.StatusNotFound, codeTenantNotFound, "tenant not found")
	case errors.Is(err, ErrTenantSuspended):
		writeError(w, r, http.StatusForbidden, codeTenantSuspended, "tenant suspended")
	default:
		writeError(w, r, http.StatusInternalServerError, codeInternal, "internal error")
	}
}

// ---------------------------------------------------------------------------
// handlers
// ---------------------------------------------------------------------------

type createGroupRequest struct {
	Name        string  `json:"name"`
	Description string  `json:"description"`
	ParentID    *string `json:"parent_id"`
}

type groupResponse struct {
	GroupID     string    `json:"group_id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	ParentID    *string   `json:"parent_id"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

func newGroupResponse(g *DeviceGroup) groupResponse {
	return groupResponse{
		GroupID:     g.ID,
		Name:        g.Name,
		Description: g.Description,
		ParentID:    g.ParentID,
		CreatedAt:   g.CreatedAt.UTC(),
		UpdatedAt:   g.UpdatedAt.UTC(),
	}
}

type groupListResponse struct {
	Items    []groupResponse `json:"items"`
	Total    int             `json:"total"`
	Page     int             `json:"page"`
	PageSize int             `json:"page_size"`
}

type patchGroupRequest struct {
	Name        *string `json:"name"`
	Description *string `json:"description"`
}

// requireGroups fails the group endpoints closed when the seam is not
// wired (same discipline as Tools/Policies, see server.go).
func (s *Server) requireGroups(w http.ResponseWriter, r *http.Request) bool {
	if s.Groups == nil {
		writeError(w, r, http.StatusInternalServerError, codeInternal, "device groups not configured")
		return false
	}
	return true
}

// loadOwnedGroup loads a group by id and enforces tenant ownership
// (design/33 1.2, SEC-02); it writes the response on failure.
func (s *Server) loadOwnedGroup(w http.ResponseWriter, r *http.Request, p *adminauth.Principal, groupID string) (*DeviceGroup, bool) {
	if !requireUUID(w, r, "group_id", groupID) {
		return nil, false
	}
	g, err := s.Groups.Get(r.Context(), groupID)
	if err != nil {
		if errors.Is(err, ErrGroupNotFound) {
			writeError(w, r, http.StatusNotFound, codeGroupNotFound, "device group not found")
			return nil, false
		}
		writeError(w, r, http.StatusInternalServerError, codeInternal, "load device group failed")
		return nil, false
	}
	if !s.checkOwnership(w, r, p, g.TenantID) {
		return nil, false
	}
	return g, true
}

// handleCreateGroup serves POST /v1/admin/device-groups (FR-011/B1.3):
// name is required and unique per tenant (uq_groups_tenant_name); the
// optional parent must exist in the same tenant.
func (s *Server) handleCreateGroup(w http.ResponseWriter, r *http.Request) {
	p, ok := adminauth.FromContext(r.Context())
	if !ok {
		writeError(w, r, http.StatusUnauthorized, codeUnauthorized, "unauthenticated")
		return
	}
	tenantID, ok := s.resolveTenant(w, r, p)
	if !ok {
		return
	}
	if !s.requireGroups(w, r) {
		return
	}
	var req createGroupRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Name == "" || len(req.Name) > 255 {
		writeError(w, r, http.StatusBadRequest, codeBadRequest, "name is required (max 255 chars)")
		return
	}
	var parentID *string
	if req.ParentID != nil {
		parent, ok := s.loadOwnedGroup(w, r, p, *req.ParentID)
		if !ok {
			return
		}
		parentID = &parent.ID
	}
	created, err := s.Groups.Create(r.Context(), &DeviceGroup{
		TenantID:    tenantID,
		ParentID:    parentID,
		Name:        req.Name,
		Description: req.Description,
		CreatedAt:   s.now().UTC(),
		UpdatedAt:   s.now().UTC(),
	})
	if err != nil {
		mapGroupRepoError(w, r, err)
		return
	}
	s.recordAudit(r.Context(), AdminOp{
		EventID:  newEventID(),
		TenantID: tenantID,
		ActorID:  p.UserID,
		Action:   "group.create",
		Target:   created.ID,
		TraceID:  httpx.TraceIDFrom(r),
	})
	httpx.WriteJSON(w, http.StatusCreated, newGroupResponse(created))
}

// handleListGroups serves GET /v1/admin/device-groups with offset
// pagination (design/33 1.6).
func (s *Server) handleListGroups(w http.ResponseWriter, r *http.Request) {
	p, ok := adminauth.FromContext(r.Context())
	if !ok {
		writeError(w, r, http.StatusUnauthorized, codeUnauthorized, "unauthenticated")
		return
	}
	tenantID, ok := s.resolveTenant(w, r, p)
	if !ok {
		return
	}
	if !s.requireGroups(w, r) {
		return
	}
	page, err := parsePage(r.URL.Query())
	if err != nil {
		writeError(w, r, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}
	list, total, err := s.Groups.List(r.Context(), tenantID, page)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, codeInternal, "list device groups failed")
		return
	}
	items := make([]groupResponse, 0, len(list))
	for i := range list {
		items = append(items, newGroupResponse(&list[i]))
	}
	httpx.WriteJSON(w, http.StatusOK, groupListResponse{Items: items, Total: total, Page: page.Number, PageSize: page.Size})
}

// handlePatchGroup serves PATCH /v1/admin/device-groups/{groupID}: rename
// and/or re-describe; at least one field is required.
func (s *Server) handlePatchGroup(w http.ResponseWriter, r *http.Request) {
	p, ok := adminauth.FromContext(r.Context())
	if !ok {
		writeError(w, r, http.StatusUnauthorized, codeUnauthorized, "unauthenticated")
		return
	}
	groupID := r.PathValue("groupID")
	if !s.requireGroups(w, r) {
		return
	}
	if _, ok := s.loadOwnedGroup(w, r, p, groupID); !ok {
		return
	}
	var req patchGroupRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Name == nil && req.Description == nil {
		writeError(w, r, http.StatusBadRequest, codeBadRequest, "name or description is required")
		return
	}
	if req.Name != nil && (*req.Name == "" || len(*req.Name) > 255) {
		writeError(w, r, http.StatusBadRequest, codeBadRequest, "name must be 1-255 chars")
		return
	}
	updated, err := s.Groups.Update(r.Context(), groupID, req.Name, req.Description)
	if err != nil {
		mapGroupRepoError(w, r, err)
		return
	}
	s.recordAudit(r.Context(), AdminOp{
		EventID:  newEventID(),
		TenantID: updated.TenantID,
		ActorID:  p.UserID,
		Action:   "group.update",
		Target:   groupID,
		TraceID:  httpx.TraceIDFrom(r),
	})
	httpx.WriteJSON(w, http.StatusOK, newGroupResponse(updated))
}

// handleDeleteGroup serves DELETE /v1/admin/device-groups/{groupID}:
// soft delete + device detach in one transaction (design/32 3.4).
func (s *Server) handleDeleteGroup(w http.ResponseWriter, r *http.Request) {
	p, ok := adminauth.FromContext(r.Context())
	if !ok {
		writeError(w, r, http.StatusUnauthorized, codeUnauthorized, "unauthenticated")
		return
	}
	groupID := r.PathValue("groupID")
	if !s.requireGroups(w, r) {
		return
	}
	g, ok := s.loadOwnedGroup(w, r, p, groupID)
	if !ok {
		return
	}
	if err := s.Groups.Delete(r.Context(), groupID); err != nil {
		mapGroupRepoError(w, r, err)
		return
	}
	s.recordAudit(r.Context(), AdminOp{
		EventID:  newEventID(),
		TenantID: g.TenantID,
		ActorID:  p.UserID,
		Action:   "group.delete",
		Target:   groupID,
		TraceID:  httpx.TraceIDFrom(r),
	})
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// PostgreSQL repository
// ---------------------------------------------------------------------------

// groupCols rebuilds a DeviceGroup (uuid cast to text per the pgx v5.10
// scan plan, see approval.PGTicketRepo).
const groupCols = `id::text, tenant_id::text, COALESCE(parent_id::text,''), name,
	COALESCE(description,''), created_at, updated_at`

// pgGroupRepo is the PostgreSQL GroupRepo (design/32 3.4).
type pgGroupRepo struct {
	pool pgxPooler
}

// NewPGGroupRepo builds a device group repository over an existing pgx pool.
func NewPGGroupRepo(pool pgxPooler) *pgGroupRepo {
	return &pgGroupRepo{pool: pool}
}

func scanGroup(row pgx.Row) (*DeviceGroup, error) {
	g := &DeviceGroup{}
	if err := row.Scan(&g.ID, &g.TenantID, &g.ParentID, &g.Name,
		&g.Description, &g.CreatedAt, &g.UpdatedAt); err != nil {
		return nil, err
	}
	return g, nil
}

// groupTenantGate classifies missing/suspended tenants (shared sentinels,
// same gate as Register/Issue).
func (r *pgGroupRepo) groupTenantGate(ctx context.Context, q interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}, tenantID string) error {
	var status string
	err := q.QueryRow(ctx, `SELECT status FROM adc_tenants
		WHERE id = $1::uuid AND deleted_at IS NULL`, tenantID).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrTenantNotFound
	}
	if err != nil {
		return err
	}
	if status != "ACTIVE" {
		return ErrTenantSuspended
	}
	return nil
}

// Create inserts one group; the partial unique index
// uq_groups_tenant_name maps name collisions onto ErrGroupNameExists.
func (r *pgGroupRepo) Create(ctx context.Context, g *DeviceGroup) (*DeviceGroup, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if err := r.groupTenantGate(ctx, tx, g.TenantID); err != nil {
		return nil, err
	}
	row := tx.QueryRow(ctx, `INSERT INTO adc_device_groups
		(tenant_id, parent_id, name, description)
		VALUES ($1::uuid, $2::uuid, $3, $4)
		RETURNING `+groupCols, g.TenantID, g.ParentID, g.Name, g.Description)
	created, err := scanGroup(row)
	if isUniqueViolation(err) {
		return nil, ErrGroupNameExists
	}
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return created, nil
}

// Get loads one active (non-deleted) group by id.
func (r *pgGroupRepo) Get(ctx context.Context, groupID string) (*DeviceGroup, error) {
	row := r.pool.QueryRow(ctx, `SELECT `+groupCols+` FROM adc_device_groups
		WHERE id = $1::uuid AND deleted_at IS NULL`, groupID)
	g, err := scanGroup(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrGroupNotFound
	}
	if err != nil {
		return nil, err
	}
	return g, nil
}

// List pages the tenant's groups, newest first, with the window total.
func (r *pgGroupRepo) List(ctx context.Context, tenantID string, p Page) ([]DeviceGroup, int, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+groupCols+`, count(*) OVER () AS total
		FROM adc_device_groups
		WHERE tenant_id = $1::uuid AND deleted_at IS NULL
		ORDER BY created_at DESC, id
		LIMIT $2 OFFSET $3`, tenantID, p.Size, p.offset())
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var (
		out   []DeviceGroup
		total int
	)
	for rows.Next() {
		var g DeviceGroup
		if err := rows.Scan(&g.ID, &g.TenantID, &g.ParentID, &g.Name,
			&g.Description, &g.CreatedAt, &g.UpdatedAt, &total); err != nil {
			return nil, 0, err
		}
		out = append(out, g)
	}
	return out, total, rows.Err()
}

// Update patches name/description; nil parts are left untouched. A name
// collision with another active group maps onto ErrGroupNameExists.
func (r *pgGroupRepo) Update(ctx context.Context, groupID string, name, description *string) (*DeviceGroup, error) {
	row := r.pool.QueryRow(ctx, `UPDATE adc_device_groups
		SET name = COALESCE($2, name),
		    description = COALESCE($3, description),
		    updated_at = now()
		WHERE id = $1::uuid AND deleted_at IS NULL
		RETURNING `+groupCols, groupID, name, description)
	updated, err := scanGroup(row)
	if isUniqueViolation(err) {
		return nil, ErrGroupNameExists
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrGroupNotFound
	}
	if err != nil {
		return nil, err
	}
	return updated, nil
}

// Delete soft deletes the group and detaches its devices in one
// transaction (design/32 3.4: "删组时设备自动脱组，不误删设备"). The
// soft-deleted name stays blocked by the partial unique index until the
// row is purged (consistent with the device-code burn policy).
func (r *pgGroupRepo) Delete(ctx context.Context, groupID string) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx, `UPDATE adc_device_groups
		SET deleted_at = now(), updated_at = now()
		WHERE id = $1::uuid AND deleted_at IS NULL`, groupID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrGroupNotFound
	}
	if _, err := tx.Exec(ctx, `UPDATE adc_devices
		SET group_id = NULL, updated_at = now()
		WHERE group_id = $1::uuid AND deleted_at IS NULL`, groupID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
