// Package adminapi implements the Admin API control plane (design/31 LLD
// 3.4, design/33 3.1, design/80 B-02/B-03/B-04/B-05/B-06): tenant CRUD
// with quota, the device ledger with one-shot credential issuance, Agent
// API key lifecycle management, tool risk level configuration (FR-006),
// the V1 single-level approval policy (FR-007) and the audit log query /
// export surface (FR-013). Requests are authenticated by session
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
	"adc.dev/ce/internal/alerts"
	"adc.dev/ce/internal/billing"
	"adc.dev/ce/internal/httpx"
)

// Unified business error codes (design/33 1.5). Like adminauth and
// agentauth, codes travel as strings on the wire; the HTTP status controls
// transport semantics and the code carries the business semantics.
const (
	codeBadRequest        = "10001"
	codeUnauthorized      = "10002"
	codeForbidden         = "10003"
	codeNotFound          = "10004"
	codeConflict          = "10005"
	codeInternal          = "10007"
	codeBodyTooLarge      = "10008"
	codeDeviceNotFound    = "11001"
	codeDeviceFrozen      = "11003"
	codeToolNotFound      = "11005"
	codeDeviceCodeExists  = "11008"
	codeDeviceQuota       = "11010"
	codeGroupNotFound     = "11013"
	codeGroupNameExists   = "11014"
	codeTenantNotFound    = "13001"
	codeTenantSuspended   = "13002"
	codeTenantQuota       = "13003"
	codeTenantCodeExists  = "13004"
	codeCrossTenant       = "13007"
	codeAuditQueryInvalid = "14001"
	codeAuditExportLimit  = "14002"
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
	EventID  string // request idempotency key (design/32 6.3)
	TenantID string
	ActorID  string // adc_users.id of the acting admin (SEC-21 real subject)
	Action   string // e.g. "tenant.create", "device.credential_reset"
	Target   string // id of the affected resource
	Reason   string // change_reason, mandatory on mutating ops
	// Details carries operation-specific payload for the audit trail,
	// e.g. before/after risk levels of a tool.configure event (design/31
	// 3.4.4: audit must contain the changed values, not just the action).
	Details   map[string]any
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
	// Tools persists adc_device_tools risk/enable configuration (B-04,
	// FR-006); Policies persists the tenant approval policy (B-04,
	// FR-007 V1 single-level subset); AuditQuery serves the audit log
	// query/export surface (B-06, FR-013). The seams are optional at
	// construction: a nil seam fails the corresponding handlers closed
	// with 500 code 10007 until the assembly wires it.
	Tools      ToolRepo
	Policies   PolicyRepo
	AuditQuery AuditQueryRepo
	// Tickets is the read-only approval ticket list for the console
	// (design/33 3.1.15); nil fails the handler closed.
	Tickets TicketsRepo
	// AlertRules persists the FR-017 tenant alert rule set (design/82 B2,
	// internal/alerts); AlertEvents serves the fired-alert history.
	// nil fails the handlers closed.
	AlertRules  alerts.RuleStore
	AlertEvents alerts.EventReader
	// ImportJobs persists batch device import job state (FR-011,
	// design/82 B1.1); Groups persists device groups (FR-011,
	// design/82 B1.3). Both are optional at construction: nil fails the
	// corresponding handlers closed with 500 code 10007.
	ImportJobs ImportJobRepo
	Groups     GroupRepo
	// ExportMaxRows caps the synchronous audit CSV export (FR-013
	// BR-013-03); zero falls back to defaultExportMaxRows.
	ExportMaxRows int
	// Billing serves the FR-016 billing surface (design/82 B3):
	// statement list/generate/detail plus the overdue flag for the
	// gateway quota soft-limit seam (B3.3 — only the state is exposed,
	// the degradation logic is not implemented). nil fails the handlers
	// closed with 500 code 10007 until the assembly wires it.
	Billing *billing.Service
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
	mux.Handle("GET /v1/admin/devices/{deviceID}/tools", authz(admin(http.HandlerFunc(s.handleListDeviceTools))))
	mux.Handle("PATCH /v1/admin/devices/{deviceID}/tools", authz(admin(http.HandlerFunc(s.handlePatchDeviceTools))))

	// FR-011 batch onboarding (design/82 B1.1/B1.3): CSV import is a
	// two-step async flow (POST create + GET poll); device groups are
	// tenant-scoped CRUD. All admin-only. The poll path nests under
	// /import/jobs/ because GET /devices/import/{jobID} would be
	// ambiguous with GET /devices/{deviceID}/tools in ServeMux
	// (literal vs wildcard at the same position).
	mux.Handle("POST /v1/admin/devices/import", authz(admin(http.HandlerFunc(s.handleImportDevices))))
	mux.Handle("GET /v1/admin/devices/import/jobs/{jobID}", authz(admin(http.HandlerFunc(s.handleImportStatus))))
	mux.Handle("POST /v1/admin/device-groups", authz(admin(http.HandlerFunc(s.handleCreateGroup))))
	mux.Handle("GET /v1/admin/device-groups", authz(admin(http.HandlerFunc(s.handleListGroups))))
	mux.Handle("PATCH /v1/admin/device-groups/{groupID}", authz(admin(http.HandlerFunc(s.handlePatchGroup))))
	mux.Handle("DELETE /v1/admin/device-groups/{groupID}", authz(admin(http.HandlerFunc(s.handleDeleteGroup))))

	mux.Handle("GET /v1/admin/tenants/{tenantID}/approval-policy", authz(admin(http.HandlerFunc(s.handleGetApprovalPolicy))))
	mux.Handle("PUT /v1/admin/tenants/{tenantID}/approval-policy", authz(admin(http.HandlerFunc(s.handlePutApprovalPolicy))))

	mux.Handle("POST /v1/admin/agent-keys", authz(admin(http.HandlerFunc(s.handleIssueKey))))
	mux.Handle("GET /v1/admin/agent-keys", authz(admin(http.HandlerFunc(s.handleListKeys))))
	mux.Handle("POST /v1/admin/agent-keys/{keyID}/revoke", authz(admin(http.HandlerFunc(s.handleRevokeKey))))
	mux.Handle("POST /v1/admin/agent-keys/{keyID}/rotate", authz(admin(http.HandlerFunc(s.handleRotateKey))))

	// auditor may read audit logs (design/33 1.2). The matrix in
	// adminauth grants auditors GET/HEAD on /v1/admin/audit-logs only,
	// so the POST export below is admin-only until that matrix is
	// widened (follow-up; see audit.go).
	auditRead := adminauth.RequireRole(adminauth.RoleAdmin, adminauth.RoleAuditor)
	mux.Handle("GET /v1/admin/approval-tickets", authz(admin(http.HandlerFunc(s.handleTicketsList))))
	mux.Handle("GET /v1/admin/audit-logs", authz(auditRead(http.HandlerFunc(s.handleAuditLogs))))
	mux.Handle("POST /v1/admin/audit-logs/export", authz(auditRead(http.HandlerFunc(s.handleAuditExport))))

	// FR-017 alert rules and fired-alert history (design/82 B2). The RBAC
	// matrix (adminauth/rbac.go) covers admin-tier paths only, so both
	// routes are admin-only until the matrix is widened for auditors.
	mux.Handle("GET /v1/admin/alerts/rules", authz(admin(http.HandlerFunc(s.handleGetAlertRules))))
	mux.Handle("PUT /v1/admin/alerts/rules", authz(admin(http.HandlerFunc(s.handlePutAlertRules))))
	mux.Handle("GET /v1/admin/alerts/events", authz(admin(http.HandlerFunc(s.handleGetAlertEvents))))

	// FR-016 billing surface (design/82 B3). List/detail/overdue are
	// tenant-scoped in-handler; generate is a platform_admin exclusive.
	mux.Handle("GET /v1/admin/billing/statements", authz(admin(http.HandlerFunc(s.handleBillingList))))
	mux.Handle("POST /v1/admin/billing/generate", authz(admin(http.HandlerFunc(s.handleBillingGenerate))))
	mux.Handle("GET /v1/admin/billing/statements/{statementID}", authz(admin(http.HandlerFunc(s.handleBillingDetail))))
	mux.Handle("GET /v1/admin/billing/overdue", authz(admin(http.HandlerFunc(s.handleBillingOverdue))))

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
