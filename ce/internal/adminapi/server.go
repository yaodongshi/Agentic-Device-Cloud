// Package adminapi implements the Admin API control plane (design/31 LLD
// 3.4, design/33 3.1, design/80 B-02/B-03/B-05): tenant CRUD with quota,
// the device ledger with one-shot credential issuance, and Agent API key
// lifecycle management. Requests are authenticated by session
// (adminauth.Authorize) and gated by the admin RBAC matrix
// (adminauth.RequireRole); the tenant context always derives from the
// session record, never from request headers (SEC-02 / GAP-13).
package adminapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"adc.dev/ce/internal/adminauth"
	"adc.dev/ce/internal/httpx"
)

// Unified business error codes (design/33 1.5). Like adminauth and
// agentauth, codes travel as strings on the wire; the HTTP status controls
// transport semantics and the code carries the business semantics.
const (
	codeBadRequest       = "10001"
	codeUnauthorized     = "10002"
	codeForbidden        = "10003"
	codeNotFound         = "10004"
	codeConflict         = "10005"
	codeInternal         = "10007"
	codeDeviceNotFound   = "11001"
	codeDeviceFrozen     = "11003"
	codeDeviceCodeExists = "11008"
	codeDeviceQuota      = "11010"
	codeTenantNotFound   = "13001"
	codeTenantSuspended  = "13002"
	codeTenantQuota      = "13003"
	codeTenantCodeExists = "13004"
	codeCrossTenant      = "13007"
)

// rolePlatformAdmin is the only role allowed to create/suspend tenants and
// to operate across tenants (design/33 1.2).
const rolePlatformAdmin = "platform_admin"

// maxBodyBytes caps request bodies decoded by handlers (defense in depth
// behind the gateway-level limit, SEC-19).
const maxBodyBytes = 1 << 20

// Page is the offset pagination window (design/33 1.6): 1-based page,
// page_size defaults to 20 and is capped at 200.
type Page struct {
	Number int
	Size   int
}

const (
	defaultPageSize = 20
	maxPageSize     = 200
)

// offset converts the 1-based page to a SQL OFFSET.
func (p Page) offset() int { return (p.Number - 1) * p.Size }

// parsePage validates the page/page_size query parameters (design/33 1.6).
func parsePage(q url.Values) (Page, error) {
	p := Page{Number: 1, Size: defaultPageSize}
	if v := q.Get("page"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return Page{}, errors.New("page must be a positive integer")
		}
		p.Number = n
	}
	if v := q.Get("page_size"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxPageSize {
			return Page{}, errors.New("page_size must be between 1 and 200")
		}
		p.Size = n
	}
	return p, nil
}

// AuditSink is the optional audit seam for admin operations (SEC-07). V1
// wiring note: pkg/audit emits tool-call-shaped events only (its worker
// fills event_type=tool_call at insert time), so producing admin_op rows
// through pkg/audit is a follow-up task and this package declares its own
// minimal contract instead. Implementations map AdminOp onto
// adc_audit_logs with event_type='admin_op' (design/32 2.4).
type AuditSink interface {
	Record(ctx context.Context, op AdminOp) error
}

// AdminOp is one administrative action worth an adc_audit_logs admin_op row.
type AdminOp struct {
	EventID   string // request idempotency key (design/32 6.3)
	TenantID  string
	ActorID   string // adc_users.id of the acting admin (SEC-21 real subject)
	Action    string // e.g. "tenant.create", "device.credential_reset"
	Target    string // id of the affected resource
	Reason    string // change_reason, mandatory on mutating ops
	TraceID   string
	CreatedAt time.Time
}

// Server wires the Admin API endpoints over the repository seams defined
// per aggregate (design/31 3.4.2).
type Server struct {
	Tenants  TenantRepo
	Devices  DeviceRepo
	APIKeys  ApiKeyRepo
	Sessions adminauth.SessionStore
	// Audit receives admin_op events; nil disables emission (tests,
	// assemblies without a wired audit pipeline).
	Audit AuditSink
	// KEK is the device credential encryption key (auth.LoadKEK). It is
	// required only when hmac devices are registered; registration fails
	// closed without it.
	KEK []byte
	// Now overrides the clock (tests); nil means time.Now.
	Now func() time.Time
}

// NewServer builds the Admin API server over the given repository seams.
func NewServer(tenants TenantRepo, devices DeviceRepo, keys ApiKeyRepo, sessions adminauth.SessionStore) *Server {
	return &Server{
		Tenants:  tenants,
		Devices:  devices,
		APIKeys:  keys,
		Sessions: sessions,
	}
}

// Handler assembles the Admin API routes (design/33 3.1). Every route is
// wrapped in session authentication followed by the role matrix; handlers
// additionally enforce the platform-admin-only and tenant-ownership rules
// the matrix cannot express (design/33 1.2). Routes are mounted under
// /v1/admin/* per the gateway prefix routing (design/31 3.5.1).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	authz := adminauth.Authorize(s.Sessions)
	admin := adminauth.RequireRole(adminauth.RoleAdmin)
	// approver may read the device list per the design/33 1.2 matrix.
	deviceRead := adminauth.RequireRole(adminauth.RoleAdmin, adminauth.RoleApprover)

	mux.Handle("GET /v1/admin/tenants", authz(admin(http.HandlerFunc(s.handleListTenants))))
	mux.Handle("POST /v1/admin/tenants", authz(admin(http.HandlerFunc(s.handleCreateTenant))))
	mux.Handle("PATCH /v1/admin/tenants/{tenantID}", authz(admin(http.HandlerFunc(s.handlePatchTenant))))

	mux.Handle("POST /v1/admin/devices", authz(admin(http.HandlerFunc(s.handleRegisterDevice))))
	mux.Handle("GET /v1/admin/devices", authz(deviceRead(http.HandlerFunc(s.handleListDevices))))
	mux.Handle("PATCH /v1/admin/devices/{deviceID}", authz(admin(http.HandlerFunc(s.handlePatchDevice))))
	mux.Handle("DELETE /v1/admin/devices/{deviceID}", authz(admin(http.HandlerFunc(s.handleDeleteDevice))))

	mux.Handle("POST /v1/admin/agent-keys", authz(admin(http.HandlerFunc(s.handleIssueKey))))
	mux.Handle("GET /v1/admin/agent-keys", authz(admin(http.HandlerFunc(s.handleListKeys))))
	mux.Handle("POST /v1/admin/agent-keys/{keyID}/revoke", authz(admin(http.HandlerFunc(s.handleRevokeKey))))
	mux.Handle("POST /v1/admin/agent-keys/{keyID}/rotate", authz(admin(http.HandlerFunc(s.handleRotateKey))))

	// Trace id propagation guarantees an X-Trace-ID on every response,
	// including unified error bodies (design/33 1.9).
	return httpx.TraceID(mux)
}

// writeError emits the unified error body (design/33 1.5).
func writeError(w http.ResponseWriter, r *http.Request, status int, code, msg string) {
	httpx.WriteError(w, status, code, msg, httpx.TraceIDFrom(r))
}

// decodeJSON decodes a JSON request body capped at maxBodyBytes and answers
// 400 code 10001 on malformed bodies. Unknown fields are ignored per
// design/33 1.1.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes)).Decode(dst); err != nil {
		writeError(w, r, http.StatusBadRequest, codeBadRequest, "invalid json body")
		return false
	}
	return true
}

// uuidRe is a strict RFC 4122 shape check used for path/query identifiers
// before they reach parameterized queries (SEC-20).
var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func isValidUUID(s string) bool { return uuidRe.MatchString(s) }

// requireUUID validates a path parameter and writes 400 code 10001 when the
// value is not a UUID.
func requireUUID(w http.ResponseWriter, r *http.Request, name, v string) bool {
	if !isValidUUID(v) {
		writeError(w, r, http.StatusBadRequest, codeBadRequest, name+" must be a valid uuid")
		return false
	}
	return true
}

// isPlatformAdmin reports whether the principal carries the platform_admin
// role name (design/33 1.2).
func isPlatformAdmin(p *adminauth.Principal) bool {
	for _, r := range p.Roles {
		if r == rolePlatformAdmin {
			return true
		}
	}
	return false
}

// resolveTenant derives the effective tenant for tenant-scoped resources
// (devices, agent-keys) per design/33 1.2: tenant-scoped roles (tenant_admin,
// approver) are locked to the session tenant and an explicit tenant_id that
// does not match is rejected with 13007 (SEC-02); platform admins must name
// the target tenant via the tenant_id query parameter and are rejected with
// 10001 when they do not.
func (s *Server) resolveTenant(w http.ResponseWriter, r *http.Request, p *adminauth.Principal) (string, bool) {
	explicit := strings.TrimSpace(r.URL.Query().Get("tenant_id"))
	if explicit == "" {
		if isPlatformAdmin(p) {
			writeError(w, r, http.StatusBadRequest, codeBadRequest, "tenant_id query parameter is required for platform admins")
			return "", false
		}
		return p.TenantID, true
	}
	if !isValidUUID(explicit) {
		writeError(w, r, http.StatusBadRequest, codeBadRequest, "tenant_id must be a valid uuid")
		return "", false
	}
	if explicit != p.TenantID && !isPlatformAdmin(p) {
		writeError(w, r, http.StatusForbidden, codeCrossTenant, "cross-tenant access denied")
		return "", false
	}
	return explicit, true
}

// checkOwnership rejects tenant-scoped roles reaching into another tenant's
// resource with 403 code 13007 (design/33 1.2, SEC-02). Platform admins are
// exempt: cross-tenant operation is their job.
func (s *Server) checkOwnership(w http.ResponseWriter, r *http.Request, p *adminauth.Principal, resourceTenantID string) bool {
	if isPlatformAdmin(p) || resourceTenantID == p.TenantID {
		return true
	}
	writeError(w, r, http.StatusForbidden, codeCrossTenant, "cross-tenant access denied")
	return false
}

// recordAudit emits an admin_op event; emission is best-effort and never
// fails the request. A nil sink disables emission.
func (s *Server) recordAudit(ctx context.Context, op AdminOp) {
	if s.Audit == nil {
		return
	}
	if op.CreatedAt.IsZero() {
		op.CreatedAt = s.now()
	}
	_ = s.Audit.Record(ctx, op)
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// newEventID returns a random UUIDv4 string (crypto/rand only, no
// third-party dependency), used as the audit idempotency key.
func newEventID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("evt-%d", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	var dst [36]byte
	hex.Encode(dst[0:8], b[0:4])
	dst[8] = '-'
	hex.Encode(dst[9:13], b[4:6])
	dst[13] = '-'
	hex.Encode(dst[14:18], b[6:8])
	dst[18] = '-'
	hex.Encode(dst[19:23], b[8:10])
	dst[23] = '-'
	hex.Encode(dst[24:36], b[10:16])
	return string(dst[:])
}
