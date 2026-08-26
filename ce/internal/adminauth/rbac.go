package adminauth

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"adc.dev/ce/internal/httpx"
)

// Business error codes from design/33 1.5 (same string scheme as the
// agentauth middleware).
const (
	codeBadRequest   = "10001" // malformed request
	codeUnauthorized = "10002" // unauthenticated or invalid credential
	codeForbidden    = "10003" // insufficient permission
	codeInternal     = "10007" // service internal error
)

const tenantStatusActive = "ACTIVE"

var ErrAuthorizationInvalid = errors.New("adminauth: authorization snapshot invalid")

type AuthorizationSnapshot struct {
	UserStatus    string
	UserDeleted   bool
	TenantStatus  string
	TenantDeleted bool
	AuthzVersion  int64
	Roles         []string
}

type AuthorizationStore interface {
	CurrentAuthorization(ctx context.Context, userID, tenantID string) (*AuthorizationSnapshot, error)
}

const (
	msgBadRequest   = "invalid request"
	msgUnauthorized = "invalid or expired session"
	msgForbidden    = "forbidden"
	msgInternal     = "internal error"
)

// CookieSession is the admin session cookie name (design/33 1.2):
// Set-Cookie: adc_session=...; HttpOnly; Secure; SameSite=Lax.
const CookieSession = "adc_session"

// Role is one RBAC tier of the three-tier matrix (design/80 B-01). The
// design/33 3.1.18 role names map into these tiers via roleGroups.
type Role string

const (
	// RoleAdmin covers design/33 platform_admin and tenant_admin: full
	// Admin API access (tenant_admin is additionally constrained to its
	// own tenant by resource handlers, design/33 1.2).
	RoleAdmin Role = "admin"
	// RoleApprover: read-only device list and approval ticket queries.
	// Approval decisions flow through the HITL callback surface, never
	// through Admin API (design/33 1.2).
	RoleApprover Role = "approver"
	// RoleAuditor: read-only audit log query/export and usage views; any
	// write attempt must be rejected with 10003 (design/33 1.2).
	RoleAuditor Role = "auditor"
)

// roleGroups maps each tier to the design/33 3.1.18 role names belonging to
// it, plus the tier name itself so sessions may carry either spelling.
var roleGroups = map[Role][]string{
	RoleAdmin:    {"platform_admin", "tenant_admin", "admin"},
	RoleApprover: {"approver"},
	RoleAuditor:  {"auditor"},
}

// accessRule is one allow entry of the role-by-path-prefix matrix
// (design/33 1.2 Admin API permission matrix). Menu-level trimming in the
// console is cosmetic; this matrix is the server-side enforcement point.
type accessRule struct {
	path    string          // request path prefix
	methods map[string]bool // allowed methods; nil means any method
}

// readOnlyMethods are the HTTP methods approver/auditor may use on the
// paths their matrix covers.
var readOnlyMethods = map[string]bool{
	http.MethodGet:  true,
	http.MethodHead: true,
}

// roleMatrix is the role x resource-path-prefix allow table. Paths follow
// the Admin API mount: the gateway routes /v1/admin/* to this service
// (design/31 3.5.1).
var roleMatrix = map[Role][]accessRule{
	RoleAdmin: {
		{path: "/"}, // every Admin API path, every method
	},
	RoleApprover: {
		{path: "/v1/admin/devices", methods: readOnlyMethods},
		{path: "/v1/admin/approval-tickets", methods: readOnlyMethods},
	},
	RoleAuditor: {
		{path: "/v1/admin/audit-logs", methods: readOnlyMethods},
		{path: "/v1/admin/usage", methods: readOnlyMethods},
	},
}

// ruleMatch reports whether rule covers method on path: method must be
// allowed (nil = any) and path must equal the prefix or sit under it.
func ruleMatch(rule accessRule, method, path string) bool {
	if rule.methods != nil && !rule.methods[method] {
		return false
	}
	if rule.path == "/" {
		return strings.HasPrefix(path, "/")
	}
	return path == rule.path || strings.HasPrefix(path, rule.path+"/")
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// Permission reports whether any role in roles is allowed to issue method
// on path under the admin RBAC matrix. Roles that map to no tier (unknown
// role names) are denied: the matrix fails closed.
func Permission(roles []string, method, path string) bool {
	for _, role := range roles {
		for tier, names := range roleGroups {
			if !containsString(names, role) {
				continue
			}
			for _, rule := range roleMatrix[tier] {
				if ruleMatch(rule, method, path) {
					return true
				}
			}
		}
	}
	return false
}

// hasAnyTier reports whether roles contains at least one member of any of
// the given tiers.
func hasAnyTier(roles []string, tiers []Role) bool {
	for _, tier := range tiers {
		for _, name := range roleGroups[tier] {
			if containsString(roles, name) {
				return true
			}
		}
	}
	return false
}

// Principal is the authenticated admin identity attached to the request
// context by Authorize. TenantID comes from the session record, never from
// request headers (SEC-02 / GAP-13).
type Principal struct {
	UserID   string
	TenantID string
	Roles    []string
}

type principalCtxKey struct{}

// WithPrincipal attaches the authenticated principal to ctx.
func WithPrincipal(ctx context.Context, p *Principal) context.Context {
	return context.WithValue(ctx, principalCtxKey{}, p)
}

// FromContext returns the principal injected by Authorize, if any.
func FromContext(ctx context.Context) (*Principal, bool) {
	p, ok := ctx.Value(principalCtxKey{}).(*Principal)
	return p, ok
}

// ExtractToken returns the bearer session token from Authorization or the
// adc_session cookie (design/33 1.2). Authorization wins when both are
// present; other Authorization schemes are ignored.
func ExtractToken(r *http.Request) string {
	const scheme = "Bearer "
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if len(auth) > len(scheme) && strings.EqualFold(auth[:len(scheme)], scheme) {
		return strings.TrimSpace(auth[len(scheme):])
	}
	if c, err := r.Cookie(CookieSession); err == nil && c.Value != "" {
		return c.Value
	}
	return ""
}

// Authorize returns middleware that requires a valid admin session: token
// extraction, SHA-256 hash, SessionStore.Get. On success the Principal is
// injected into the request context for downstream handlers (same style as
// agentauth.Authorize). Missing, unknown and expired sessions all answer
// 401 code 10002; storage failures answer 500 code 10007.
func Authorize(store SessionStore, authorization ...AuthorizationStore) func(http.Handler) http.Handler {
	var authz AuthorizationStore
	if len(authorization) > 0 {
		authz = authorization[0]
	} else {
		authz, _ = store.(AuthorizationStore)
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token := ExtractToken(r)
			if token == "" {
				writeUnauthorized(w, r)
				return
			}
			sess, err := store.Get(r.Context(), HashToken(token))
			if err != nil {
				if errors.Is(err, ErrSessionNotFound) {
					writeUnauthorized(w, r)
				} else {
					httpx.WriteError(w, http.StatusInternalServerError, codeInternal, msgInternal, httpx.TraceIDFrom(r))
				}
				return
			}
			tokenHash := HashToken(token)
			if authz == nil {
				_ = store.Delete(r.Context(), tokenHash)
				writeUnauthorized(w, r)
				return
			}
			snapshot, err := authz.CurrentAuthorization(r.Context(), sess.UserID, sess.TenantID)
			if err != nil || snapshot == nil || snapshot.UserDeleted || snapshot.TenantDeleted ||
				snapshot.UserStatus != userStatusActive || snapshot.TenantStatus != tenantStatusActive ||
				snapshot.AuthzVersion <= 0 || snapshot.AuthzVersion != sess.AuthzVersion || len(snapshot.Roles) == 0 {
				_ = store.Delete(r.Context(), tokenHash)
				writeUnauthorized(w, r)
				return
			}
			p := &Principal{
				UserID:   sess.UserID,
				TenantID: sess.TenantID,
				Roles:    append([]string(nil), snapshot.Roles...),
			}
			next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), p)))
		})
	}
}

// RequireRole returns middleware enforcing the RBAC matrix. It needs a
// Principal from Authorize (401 code 10002 otherwise) and answers 403 code
// 10003 when the principal holds none of the required tiers or the matrix
// does not cover the request path/method.
func RequireRole(roles ...Role) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, ok := FromContext(r.Context())
			if !ok {
				writeUnauthorized(w, r)
				return
			}
			if !hasAnyTier(p.Roles, roles) || !Permission(p.Roles, r.Method, r.URL.Path) {
				httpx.WriteError(w, http.StatusForbidden, codeForbidden, msgForbidden, httpx.TraceIDFrom(r))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// writeUnauthorized emits a generic 401 so the failure reason is not
// revealed to callers (no session enumeration).
func writeUnauthorized(w http.ResponseWriter, r *http.Request) {
	httpx.WriteError(w, http.StatusUnauthorized, codeUnauthorized, msgUnauthorized, httpx.TraceIDFrom(r))
}
