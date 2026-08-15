package adminapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"adc.dev/ce/internal/adminauth"
	"adc.dev/ce/internal/auth"
	"adc.dev/ce/internal/httpx"
)

// Device repository sentinels, mapped to design/33 11xxx codes.
var (
	ErrDeviceNotFound      = errors.New("adminapi: device not found")
	ErrDeviceCodeExists    = errors.New("adminapi: device code already exists")
	ErrDeviceQuotaExceeded = errors.New("adminapi: device quota exceeded")
	ErrDeviceFrozen        = errors.New("adminapi: device is frozen")
)

// Device statuses in API spelling; adc_devices.status CHECK stores the
// uppercase forms (design/32 3.5).
const (
	deviceStatusOffline = "offline"
	deviceStatusOnline  = "online"
	deviceStatusError   = "error"
	deviceStatusFrozen  = "frozen"
	deviceStatusRetired = "retired"
)

// Device patch operations (design/33 3.1.8 op enum).
const (
	opFreeze           = "freeze"
	opUnfreeze         = "unfreeze"
	opRevokeCredential = "revoke_credential"
	opResetCredential  = "reset_credential"
	opUpdateMeta       = "update_meta"
)

// deviceCodeRe mirrors design/33 1.1 whitelist (SEC-20): letters, digits,
// hyphen, underscore, 1-128 chars.
var deviceCodeRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// mtlsPlaceholder is the V1 credential_hash stored for mTLS devices:
// client certificate provisioning is deferred (design/33 3.1.6 cert_url),
// and a placeholder that never verifies keeps the NOT NULL column honest
// while failing closed in the data plane.
const mtlsPlaceholder = "mtls-pending"

// Device is one adc_devices row (design/32 3.5) in API spelling.
type Device struct {
	ID                string
	TenantID          string
	GroupID           *string
	DeviceCode        string
	Name              string
	DeviceType        string
	DeviceClass       string // A / B (ADR-15)
	AuthType          string // token / hmac / mtls
	Status            string
	SDKVersion        string
	ProtocolVersion   string
	LastHeartbeat     *time.Time
	Metadata          map[string]any
	CredentialVersion int
	CreatedAt         time.Time
	UpdatedAt         time.Time

	// credentialStored mirrors adc_devices.credential_hash and never
	// leaves the repository layer (NFR-004: only hashes/encrypted forms
	// are stored, never plaintext).
	credentialStored string

	// scan scratch space shared by scanDevice and the list scan so a row
	// is consumed by exactly one Scan call (pgx constraint).
	dbStatus string
	metaJSON []byte
}

// scanTargets returns the scan pointers for deviceCols in column order.
func (d *Device) scanTargets() []any {
	return []any{&d.ID, &d.TenantID, &d.GroupID, &d.DeviceCode, &d.Name,
		&d.DeviceType, &d.DeviceClass, &d.AuthType, &d.dbStatus, &d.SDKVersion,
		&d.ProtocolVersion, &d.LastHeartbeat, &d.metaJSON, &d.CredentialVersion,
		&d.CreatedAt, &d.UpdatedAt}
}

// finishScan converts the DB spellings to API spellings after Scan.
func (d *Device) finishScan() error {
	d.Status = dbDeviceStatus(d.dbStatus)
	d.Metadata = map[string]any{}
	if len(d.metaJSON) > 0 {
		if err := json.Unmarshal(d.metaJSON, &d.Metadata); err != nil {
			return err
		}
	}
	return nil
}

// DeviceCredential is the storage form of a freshly issued device
// credential. The plaintext exists only in the issuance response.
type DeviceCredential struct {
	Stored string
}

// DeviceFilter narrows device list queries (design/33 3.1.7).
type DeviceFilter struct {
	Status     string
	DeviceType string
	Keyword    string // matches device_code or name (case-insensitive)
	GroupID    string
}

// DeviceRepo persists the device ledger (design/31 LLD 3.4.2 seam).
type DeviceRepo interface {
	// Register inserts the device and consumes one tenant device quota
	// slot in the same transaction (design/32 6.2).
	Register(ctx context.Context, d *Device, cred DeviceCredential) (*Device, error)
	Get(ctx context.Context, deviceID string) (*Device, error)
	List(ctx context.Context, tenantID string, f DeviceFilter, p Page) ([]Device, int, error)
	SetStatus(ctx context.Context, deviceID, status string) (*Device, error)
	// ResetCredential stores a fresh credential and bumps
	// credential_version; ErrDeviceFrozen when frozen (design/33 11003).
	ResetCredential(ctx context.Context, deviceID string, cred DeviceCredential) (*Device, error)
	// RevokeCredential tombstones the credential so it can never verify
	// again (design/33 revoke semantics; the device must re-register).
	RevokeCredential(ctx context.Context, deviceID string) (*Device, error)
	UpdateMeta(ctx context.Context, deviceID string, name *string, metadata map[string]any) (*Device, error)
	// SoftDelete retires the device: deleted_at + status RETIRED and the
	// tenant quota slot is released (design/32 2.4 RETIRED, 6.5).
	SoftDelete(ctx context.Context, deviceID string) error
}

// issueCredential generates a fresh device credential and its storage form
// for the given auth type. token: salted one-way hash (auth.HashSecret);
// hmac: KEK-encrypted key (auth.EncryptSecret) so the data plane can
// recover it for signature verification (LLD 3.1.3); mtls: V1 placeholder,
// no secret is returned (certificate provisioning deferred).
func issueCredential(authType string, kek []byte) (plaintext, stored string, err error) {
	switch authType {
	case auth.AuthTypeToken:
		return auth.RotateSecret()
	case auth.AuthTypeHMAC:
		secret, err := auth.GenerateSecret()
		if err != nil {
			return "", "", err
		}
		stored, err = auth.EncryptSecret(secret, kek)
		if err != nil {
			return "", "", fmt.Errorf("adminapi: encrypt hmac secret: %w", err)
		}
		return secret, stored, nil
	case auth.AuthTypeMTLS:
		return "", mtlsPlaceholder, nil
	default:
		return "", "", fmt.Errorf("adminapi: unsupported auth type %q", authType)
	}
}

// mapDeviceRepoError translates repository sentinels onto the design/33
// device/tenant error codes.
func mapDeviceRepoError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ErrDeviceCodeExists):
		writeError(w, r, http.StatusConflict, codeDeviceCodeExists, "device code already exists")
	case errors.Is(err, ErrDeviceQuotaExceeded):
		writeError(w, r, http.StatusForbidden, codeDeviceQuota, "device quota exceeded")
	case errors.Is(err, ErrTenantNotFound):
		writeError(w, r, http.StatusNotFound, codeTenantNotFound, "tenant not found")
	case errors.Is(err, ErrTenantSuspended):
		writeError(w, r, http.StatusForbidden, codeTenantSuspended, "tenant suspended")
	case errors.Is(err, ErrDeviceNotFound):
		writeError(w, r, http.StatusNotFound, codeDeviceNotFound, "device not found")
	case errors.Is(err, ErrDeviceFrozen):
		writeError(w, r, http.StatusForbidden, codeDeviceFrozen, "device frozen")
	default:
		writeError(w, r, http.StatusInternalServerError, codeInternal, "internal error")
	}
}

// ---------------------------------------------------------------------------
// handlers
// ---------------------------------------------------------------------------

// credentialResponse mirrors design/33 3.1.6: secret appears exactly once
// at issuance; mtls returns cert_url (empty in V1) instead.
type credentialResponse struct {
	Secret  string  `json:"secret,omitempty"`
	CertURL *string `json:"cert_url,omitempty"`
}

// registerDeviceRequest mirrors design/33 3.1.6. group_ids is accepted for
// contract compatibility; adc_devices carries a single group_id column
// (design/32 3.5) so only the first entry is stored in V1.
type registerDeviceRequest struct {
	DeviceCode string         `json:"device_code"`
	Name       string         `json:"name"`
	DeviceType string         `json:"device_type"`
	AuthType   string         `json:"auth_type"`
	GroupIDs   []string       `json:"group_ids"`
	Metadata   map[string]any `json:"metadata"`
}

type deviceRegisterResponse struct {
	DeviceID   string              `json:"device_id"`
	DeviceCode string              `json:"device_code"`
	Name       string              `json:"name"`
	DeviceType string              `json:"device_type"`
	AuthType   string              `json:"auth_type"`
	Status     string              `json:"status"`
	Credential *credentialResponse `json:"credential"`
	Metadata   map[string]any      `json:"metadata"`
	CreatedAt  time.Time           `json:"created_at"`
}

// deviceListItem mirrors design/33 3.1.7: never any credential fields.
type deviceListItem struct {
	DeviceID      string         `json:"device_id"`
	DeviceCode    string         `json:"device_code"`
	Name          string         `json:"name"`
	DeviceType    string         `json:"device_type"`
	AuthType      string         `json:"auth_type"`
	Status        string         `json:"status"`
	LastHeartbeat *time.Time     `json:"last_heartbeat"`
	SDKVersion    string         `json:"sdk_version"`
	Metadata      map[string]any `json:"metadata"`
	CreatedAt     time.Time      `json:"created_at"`
}

type deviceListResponse struct {
	Items    []deviceListItem `json:"items"`
	Total    int              `json:"total"`
	Page     int              `json:"page"`
	PageSize int              `json:"page_size"`
}

// patchDeviceRequest mirrors design/33 3.1.8.
type patchDeviceRequest struct {
	Op           string         `json:"op"`
	ChangeReason string         `json:"change_reason"`
	Name         *string        `json:"name"`
	Metadata     map[string]any `json:"metadata"`
}

type devicePatchResponse struct {
	DeviceID   string              `json:"device_id"`
	DeviceCode string              `json:"device_code"`
	Name       string              `json:"name"`
	Status     string              `json:"status"`
	Credential *credentialResponse `json:"credential,omitempty"`
	UpdatedAt  time.Time           `json:"updated_at"`
}

// handleRegisterDevice serves POST /v1/admin/devices: registration plus
// one-shot credential issuance (FR-010, NFR-004). The plaintext exists only
// in this response; the repository stores the hashed/encrypted form.
func (s *Server) handleRegisterDevice(w http.ResponseWriter, r *http.Request) {
	p, ok := adminauth.FromContext(r.Context())
	if !ok {
		writeError(w, r, http.StatusUnauthorized, codeUnauthorized, "unauthenticated")
		return
	}
	tenantID, ok := s.resolveTenant(w, r, p)
	if !ok {
		return
	}
	var req registerDeviceRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if !deviceCodeRe.MatchString(req.DeviceCode) {
		writeError(w, r, http.StatusBadRequest, codeBadRequest, "device_code must be 1-128 chars of letters, digits, dash or underscore")
		return
	}
	if req.Name == "" || len(req.Name) > 255 {
		writeError(w, r, http.StatusBadRequest, codeBadRequest, "name is required (max 255 chars)")
		return
	}
	if req.DeviceType == "" || len(req.DeviceType) > 64 {
		writeError(w, r, http.StatusBadRequest, codeBadRequest, "device_type is required (max 64 chars)")
		return
	}
	switch req.AuthType {
	case auth.AuthTypeToken, auth.AuthTypeHMAC, auth.AuthTypeMTLS:
	default:
		// oauth2_client_credentials devices arrive through the V1.5
		// binding flow, not through ledger registration (ADR-15).
		writeError(w, r, http.StatusBadRequest, codeBadRequest, "auth_type must be one of token, hmac, mtls")
		return
	}
	var groupID *string
	if len(req.GroupIDs) > 0 {
		if !isValidUUID(req.GroupIDs[0]) {
			writeError(w, r, http.StatusBadRequest, codeBadRequest, "group_ids[0] must be a valid uuid")
			return
		}
		gid := req.GroupIDs[0]
		groupID = &gid
	}
	if req.Metadata == nil {
		req.Metadata = map[string]any{}
	}
	plaintext, stored, err := issueCredential(req.AuthType, s.KEK)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, codeInternal, "credential issuance failed")
		return
	}
	now := s.now().UTC()
	created, err := s.Devices.Register(r.Context(), &Device{
		TenantID:   tenantID,
		GroupID:    groupID,
		DeviceCode: req.DeviceCode,
		Name:       req.Name,
		DeviceType: req.DeviceType,
		AuthType:   req.AuthType,
		Status:     deviceStatusOffline,
		Metadata:   req.Metadata,
		CreatedAt:  now,
		UpdatedAt:  now,
	}, DeviceCredential{Stored: stored})
	if err != nil {
		mapDeviceRepoError(w, r, err)
		return
	}
	s.recordAudit(r.Context(), AdminOp{
		EventID:  newEventID(),
		TenantID: tenantID,
		ActorID:  p.UserID,
		Action:   "device.register",
		Target:   created.ID,
		TraceID:  httpx.TraceIDFrom(r),
	})
	cred := &credentialResponse{}
	if req.AuthType == auth.AuthTypeMTLS {
		url := ""
		cred.CertURL = &url
	} else {
		cred.Secret = plaintext
	}
	httpx.WriteJSON(w, http.StatusCreated, deviceRegisterResponse{
		DeviceID:   created.ID,
		DeviceCode: created.DeviceCode,
		Name:       created.Name,
		DeviceType: created.DeviceType,
		AuthType:   created.AuthType,
		Status:     created.Status,
		Credential: cred,
		Metadata:   created.Metadata,
		CreatedAt:  created.CreatedAt.UTC(),
	})
}

// handleListDevices serves GET /v1/admin/devices with filter and offset
// pagination (design/33 3.1.7). Online status is eventually consistent
// (heartbeat batch flush, design/33 3.1.7 note).
func (s *Server) handleListDevices(w http.ResponseWriter, r *http.Request) {
	p, ok := adminauth.FromContext(r.Context())
	if !ok {
		writeError(w, r, http.StatusUnauthorized, codeUnauthorized, "unauthenticated")
		return
	}
	tenantID, ok := s.resolveTenant(w, r, p)
	if !ok {
		return
	}
	page, err := parsePage(r.URL.Query())
	if err != nil {
		writeError(w, r, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}
	q := r.URL.Query()
	f := DeviceFilter{
		Status:     strings.TrimSpace(q.Get("status")),
		DeviceType: strings.TrimSpace(q.Get("device_type")),
		Keyword:    strings.TrimSpace(q.Get("keyword")),
		GroupID:    strings.TrimSpace(q.Get("group_id")),
	}
	if f.Status != "" && f.Status != deviceStatusOffline && f.Status != deviceStatusOnline &&
		f.Status != deviceStatusError && f.Status != deviceStatusFrozen {
		writeError(w, r, http.StatusBadRequest, codeBadRequest, "status must be one of offline, online, error, frozen")
		return
	}
	if f.GroupID != "" && !isValidUUID(f.GroupID) {
		writeError(w, r, http.StatusBadRequest, codeBadRequest, "group_id must be a valid uuid")
		return
	}
	list, total, err := s.Devices.List(r.Context(), tenantID, f, page)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, codeInternal, "list devices failed")
		return
	}
	items := make([]deviceListItem, 0, len(list))
	for i := range list {
		items = append(items, deviceListItem{
			DeviceID:      list[i].ID,
			DeviceCode:    list[i].DeviceCode,
			Name:          list[i].Name,
			DeviceType:    list[i].DeviceType,
			AuthType:      list[i].AuthType,
			Status:        list[i].Status,
			LastHeartbeat: list[i].LastHeartbeat,
			SDKVersion:    list[i].SDKVersion,
			Metadata:      list[i].Metadata,
			CreatedAt:     list[i].CreatedAt.UTC(),
		})
	}
	httpx.WriteJSON(w, http.StatusOK, deviceListResponse{Items: items, Total: total, Page: page.Number, PageSize: page.Size})
}

// handlePatchDevice serves PATCH /v1/admin/devices/{deviceID}: freeze,
// unfreeze, credential revoke/reset and metadata updates (design/33 3.1.8).
// change_reason is mandatory on every op and written to the audit trail
// (FR-010 exception path). Freeze flips the ledger status; dropping live
// tunnel connections is enforced by the data plane reading the same status
// (design/31 3.4.3, wired with the connector follow-up).
func (s *Server) handlePatchDevice(w http.ResponseWriter, r *http.Request) {
	p, ok := adminauth.FromContext(r.Context())
	if !ok {
		writeError(w, r, http.StatusUnauthorized, codeUnauthorized, "unauthenticated")
		return
	}
	deviceID := r.PathValue("deviceID")
	if !requireUUID(w, r, "device_id", deviceID) {
		return
	}
	var req patchDeviceRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	switch req.Op {
	case opFreeze, opUnfreeze, opRevokeCredential, opResetCredential, opUpdateMeta:
	default:
		writeError(w, r, http.StatusBadRequest, codeBadRequest, "op must be one of freeze, unfreeze, revoke_credential, reset_credential, update_meta")
		return
	}
	if req.ChangeReason == "" {
		writeError(w, r, http.StatusBadRequest, codeBadRequest, "change_reason is required")
		return
	}
	if req.Op == opUpdateMeta && req.Name == nil && req.Metadata == nil {
		writeError(w, r, http.StatusBadRequest, codeBadRequest, "name or metadata is required for update_meta")
		return
	}
	if req.Name != nil && (*req.Name == "" || len(*req.Name) > 255) {
		writeError(w, r, http.StatusBadRequest, codeBadRequest, "name must be 1-255 chars")
		return
	}
	dev, err := s.Devices.Get(r.Context(), deviceID)
	if err != nil {
		if errors.Is(err, ErrDeviceNotFound) {
			writeError(w, r, http.StatusNotFound, codeDeviceNotFound, "device not found")
			return
		}
		writeError(w, r, http.StatusInternalServerError, codeInternal, "load device failed")
		return
	}
	if !s.checkOwnership(w, r, p, dev.TenantID) {
		return
	}

	var (
		updated   *Device
		newSecret string
	)
	switch req.Op {
	case opFreeze:
		updated, err = s.Devices.SetStatus(r.Context(), deviceID, deviceStatusFrozen)
	case opUnfreeze:
		if dev.Status != deviceStatusFrozen {
			writeError(w, r, http.StatusBadRequest, codeBadRequest, "device is not frozen")
			return
		}
		updated, err = s.Devices.SetStatus(r.Context(), deviceID, deviceStatusOffline)
	case opRevokeCredential:
		updated, err = s.Devices.RevokeCredential(r.Context(), deviceID)
	case opResetCredential:
		if dev.Status == deviceStatusFrozen {
			writeError(w, r, http.StatusForbidden, codeDeviceFrozen, "frozen devices cannot reset credentials")
			return
		}
		if dev.AuthType == auth.AuthTypeMTLS {
			writeError(w, r, http.StatusBadRequest, codeBadRequest, "mtls credential reset is not supported in V1.0")
			return
		}
		var ierr error
		var stored string
		newSecret, stored, ierr = issueCredential(dev.AuthType, s.KEK)
		if ierr != nil {
			writeError(w, r, http.StatusInternalServerError, codeInternal, "credential issuance failed")
			return
		}
		updated, err = s.Devices.ResetCredential(r.Context(), deviceID, DeviceCredential{Stored: stored})
	case opUpdateMeta:
		updated, err = s.Devices.UpdateMeta(r.Context(), deviceID, req.Name, req.Metadata)
	}
	if err != nil {
		mapDeviceRepoError(w, r, err)
		return
	}
	s.recordAudit(r.Context(), AdminOp{
		EventID:  newEventID(),
		TenantID: updated.TenantID,
		ActorID:  p.UserID,
		Action:   "device." + req.Op,
		Target:   deviceID,
		Reason:   req.ChangeReason,
		TraceID:  httpx.TraceIDFrom(r),
	})
	resp := devicePatchResponse{
		DeviceID:   updated.ID,
		DeviceCode: updated.DeviceCode,
		Name:       updated.Name,
		Status:     updated.Status,
		UpdatedAt:  updated.UpdatedAt.UTC(),
	}
	if req.Op == opResetCredential {
		resp.Credential = &credentialResponse{Secret: newSecret}
	}
	httpx.WriteJSON(w, http.StatusOK, resp)
}

// handleDeleteDevice serves DELETE /v1/admin/devices/{deviceID}: soft
// retirement (deleted_at + RETIRED, design/32 2.4). The device code stays
// burned forever (uq_devices_code has no deleted_at predicate, FR-011).
func (s *Server) handleDeleteDevice(w http.ResponseWriter, r *http.Request) {
	p, ok := adminauth.FromContext(r.Context())
	if !ok {
		writeError(w, r, http.StatusUnauthorized, codeUnauthorized, "unauthenticated")
		return
	}
	deviceID := r.PathValue("deviceID")
	if !requireUUID(w, r, "device_id", deviceID) {
		return
	}
	dev, err := s.Devices.Get(r.Context(), deviceID)
	if err != nil {
		if errors.Is(err, ErrDeviceNotFound) {
			writeError(w, r, http.StatusNotFound, codeDeviceNotFound, "device not found")
			return
		}
		writeError(w, r, http.StatusInternalServerError, codeInternal, "load device failed")
		return
	}
	if !s.checkOwnership(w, r, p, dev.TenantID) {
		return
	}
	if err := s.Devices.SoftDelete(r.Context(), deviceID); err != nil {
		if errors.Is(err, ErrDeviceNotFound) {
			writeError(w, r, http.StatusNotFound, codeDeviceNotFound, "device not found")
			return
		}
		writeError(w, r, http.StatusInternalServerError, codeInternal, "delete device failed")
		return
	}
	s.recordAudit(r.Context(), AdminOp{
		EventID:  newEventID(),
		TenantID: dev.TenantID,
		ActorID:  p.UserID,
		Action:   "device.delete",
		Target:   deviceID,
		TraceID:  httpx.TraceIDFrom(r),
	})
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// PostgreSQL repository
// ---------------------------------------------------------------------------

// deviceCols rebuilds a Device; uuid columns are cast to text (pgx v5.10
// scan plan, see approval.PGTicketRepo).
const deviceCols = `id::text, tenant_id::text, group_id::text, device_code, name,
	device_type, device_class, auth_type, status, sdk_version, protocol_version,
	last_heartbeat_at, metadata, credential_version, created_at, updated_at`

// pgDeviceRepo is the PostgreSQL DeviceRepo (design/32 3.5).
type pgDeviceRepo struct {
	pool *pgxpool.Pool
}

// NewPGDeviceRepo builds a device repository over an existing pgx pool.
func NewPGDeviceRepo(pool *pgxpool.Pool) *pgDeviceRepo {
	return &pgDeviceRepo{pool: pool}
}

func apiDeviceStatus(s string) string { return strings.ToUpper(s) }
func dbDeviceStatus(s string) string  { return strings.ToLower(s) }

func scanDevice(row pgx.Row) (*Device, error) {
	d := &Device{}
	if err := row.Scan(d.scanTargets()...); err != nil {
		return nil, err
	}
	if err := d.finishScan(); err != nil {
		return nil, err
	}
	return d, nil
}

// Register inserts the device and consumes a tenant device quota slot in
// one transaction (design/32 6.2): the tenant gate, the conditional quota
// UPDATE and the INSERT commit or roll back together.
func (r *pgDeviceRepo) Register(ctx context.Context, d *Device, cred DeviceCredential) (*Device, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	var tenantStatus string
	err = tx.QueryRow(ctx, `SELECT status FROM adc_tenants
		WHERE id = $1::uuid AND deleted_at IS NULL`, d.TenantID).Scan(&tenantStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrTenantNotFound
	}
	if err != nil {
		return nil, err
	}
	if tenantStatus != "ACTIVE" {
		return nil, ErrTenantSuspended
	}
	tag, err := tx.Exec(ctx, `UPDATE adc_tenants
		SET used_devices = used_devices + 1, updated_at = now()
		WHERE id = $1::uuid AND status = 'ACTIVE' AND used_devices < quota_devices`, d.TenantID)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, ErrDeviceQuotaExceeded
	}
	meta, err := json.Marshal(d.Metadata)
	if err != nil {
		return nil, err
	}
	row := tx.QueryRow(ctx, `INSERT INTO adc_devices
		(tenant_id, group_id, device_code, name, device_type, auth_type,
		 credential_hash, credential_version, status, metadata)
		VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6, $7, 1, 'OFFLINE', $8)
		RETURNING `+deviceCols,
		d.TenantID, d.GroupID, d.DeviceCode, d.Name, d.DeviceType, d.AuthType, cred.Stored, string(meta))
	created, err := scanDevice(row)
	if isUniqueViolation(err) {
		return nil, ErrDeviceCodeExists
	}
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return created, nil
}

// Get loads one active (non-deleted) device by id.
func (r *pgDeviceRepo) Get(ctx context.Context, deviceID string) (*Device, error) {
	row := r.pool.QueryRow(ctx, `SELECT `+deviceCols+` FROM adc_devices
		WHERE id = $1::uuid AND deleted_at IS NULL`, deviceID)
	d, err := scanDevice(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrDeviceNotFound
	}
	if err != nil {
		return nil, err
	}
	return d, nil
}

// List pages the tenant device ledger with filter sentinels and the window
// total (design/33 3.1.7).
func (r *pgDeviceRepo) List(ctx context.Context, tenantID string, f DeviceFilter, p Page) ([]Device, int, error) {
	keyword := f.Keyword
	pattern := ""
	if keyword != "" {
		pattern = "%" + keyword + "%"
	}
	var groupParam any // nil = no group filter
	if f.GroupID != "" {
		groupParam = f.GroupID
	}
	rows, err := r.pool.Query(ctx, `SELECT `+deviceCols+`, count(*) OVER () AS total
		FROM adc_devices
		WHERE tenant_id = $1::uuid AND deleted_at IS NULL
		  AND ($2::text = '' OR status = $2)
		  AND ($3::text = '' OR device_type = $3)
		  AND ($4::text = '' OR (device_code ILIKE $5 OR name ILIKE $5))
		  AND ($6::uuid IS NULL OR group_id = $6)
		ORDER BY created_at DESC, id
		LIMIT $7 OFFSET $8`,
		tenantID, apiDeviceStatus(f.Status), f.DeviceType, keyword, pattern, groupParam, p.Size, p.offset())
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var (
		out   []Device
		total int
	)
	for rows.Next() {
		var d Device
		targets := append(d.scanTargets(), &total)
		if err := rows.Scan(targets...); err != nil {
			return nil, 0, err
		}
		if err := d.finishScan(); err != nil {
			return nil, 0, err
		}
		out = append(out, d)
	}
	return out, total, rows.Err()
}

// SetStatus updates the device status (freeze/unfreeze). Dropping live
// connections on freeze is enforced by the data plane reading this ledger.
func (r *pgDeviceRepo) SetStatus(ctx context.Context, deviceID, status string) (*Device, error) {
	row := r.pool.QueryRow(ctx, `UPDATE adc_devices
		SET status = $2, updated_at = now()
		WHERE id = $1::uuid AND deleted_at IS NULL
		RETURNING `+deviceCols, deviceID, apiDeviceStatus(status))
	d, err := scanDevice(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrDeviceNotFound
	}
	if err != nil {
		return nil, err
	}
	return d, nil
}

// ResetCredential stores the new credential and bumps credential_version in
// one conditional UPDATE; frozen devices are refused (design/33 11003). The
// FR-009-style 24h transition window keeping the old credential valid is a
// follow-up on the auth layer (the version column is the hook).
func (r *pgDeviceRepo) ResetCredential(ctx context.Context, deviceID string, cred DeviceCredential) (*Device, error) {
	row := r.pool.QueryRow(ctx, `UPDATE adc_devices
		SET credential_hash = $2, credential_version = credential_version + 1, updated_at = now()
		WHERE id = $1::uuid AND deleted_at IS NULL AND status <> 'FROZEN'
		RETURNING `+deviceCols, deviceID, cred.Stored)
	d, err := scanDevice(row)
	if err == nil {
		return d, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	cur, getErr := r.Get(ctx, deviceID)
	if getErr != nil {
		return nil, getErr
	}
	if cur.Status == "FROZEN" {
		return nil, ErrDeviceFrozen
	}
	return nil, ErrDeviceNotFound
}

// RevokeCredential tombstones the stored credential so it can never verify
// again; the device must be re-registered to obtain a new one (design/33
// 3.1.8 revoke semantics).
func (r *pgDeviceRepo) RevokeCredential(ctx context.Context, deviceID string) (*Device, error) {
	tombstone := "revoked$" + time.Now().UTC().Format(time.RFC3339)
	row := r.pool.QueryRow(ctx, `UPDATE adc_devices
		SET credential_hash = $2, credential_version = credential_version + 1, updated_at = now()
		WHERE id = $1::uuid AND deleted_at IS NULL
		RETURNING `+deviceCols, deviceID, tombstone)
	d, err := scanDevice(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrDeviceNotFound
	}
	if err != nil {
		return nil, err
	}
	return d, nil
}

// UpdateMeta patches name/metadata; nil parts are left untouched.
func (r *pgDeviceRepo) UpdateMeta(ctx context.Context, deviceID string, name *string, metadata map[string]any) (*Device, error) {
	var metaParam any
	if metadata != nil {
		raw, err := json.Marshal(metadata)
		if err != nil {
			return nil, err
		}
		metaParam = string(raw)
	}
	row := r.pool.QueryRow(ctx, `UPDATE adc_devices
		SET name = COALESCE($2, name), metadata = COALESCE($3, metadata), updated_at = now()
		WHERE id = $1::uuid AND deleted_at IS NULL
		RETURNING `+deviceCols, deviceID, name, metaParam)
	d, err := scanDevice(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrDeviceNotFound
	}
	if err != nil {
		return nil, err
	}
	return d, nil
}

// SoftDelete retires the device and releases its tenant quota slot in one
// transaction (design/32 2.4 RETIRED, 6.2 counter consistency).
func (r *pgDeviceRepo) SoftDelete(ctx context.Context, deviceID string) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var tenantID string
	err = tx.QueryRow(ctx, `UPDATE adc_devices
		SET deleted_at = now(), status = 'RETIRED', updated_at = now()
		WHERE id = $1::uuid AND deleted_at IS NULL
		RETURNING tenant_id::text`, deviceID).Scan(&tenantID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrDeviceNotFound
	}
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE adc_tenants
		SET used_devices = GREATEST(used_devices - 1, 0), updated_at = now()
		WHERE id = $1::uuid`, tenantID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
