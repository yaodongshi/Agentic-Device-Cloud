package adminauth

// OIDC login (design/83 C4.1, design/80 B-01 seam): the authorization
// code flow adds a second, password-free path next to username+password
// login. Two endpoints are served:
//
//	GET /v1/admin/auth/oidc/start     - state nonce + redirect to the IdP
//	GET /v1/admin/auth/oidc/callback  - CSRF check, code exchange, user
//	                                     mapping, session creation
//
// The flow reuses the password login session machinery (Session,
// SessionStore, CookieSession) untouched, so RBAC middleware and logout
// behave identically for OIDC sessions. When OIDC is not configured the
// endpoints answer 404 code 10004 (design/33: resource not found).

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/redis/go-redis/v9"

	"adc.dev/ce/internal/httpx"
)

// codeNotFound maps the "OIDC not enabled" answer to design/33 10004
// (resource not found, 404).
const codeNotFound = "10004"

// OIDCStateTTL bounds how long a /oidc/start nonce stays consumable.
const OIDCStateTTL = 10 * time.Minute

// oidcStateKeyPrefix follows the Valkey key layout convention used by
// sessions: adc:oidc:state:{state}.
const oidcStateKeyPrefix = "adc:oidc:state:"

// OIDCConfig holds the static IdP client registration (design/83 C4.1).
// RedirectURL is the absolute callback URL of this deployment; Scope is
// sent verbatim to the authorization endpoint (default "openid email
// profile" when empty).
type OIDCConfig struct {
	Issuer       string
	ClientID     string
	ClientSecret string
	RedirectURL  string
	Scope        string
}

// OIDCIdentity is the normalized IdP claim set after code exchange. The
// Subject is the stable OIDC sub claim and the only field used for user
// mapping; Email and Name are carried for provisioning.
type OIDCIdentity struct {
	Subject string
	Email   string
	Name    string
}

// OIDCProvider abstracts the IdP interaction (dependency seam; a
// production implementation completes the authorization code flow
// against a real IdP).
type OIDCProvider interface {
	// AuthorizationURL builds the IdP authorization URL carrying the
	// state nonce and the callback redirect.
	AuthorizationURL(state, redirect string) string
	// ExchangeCode trades the authorization code for the identity.
	ExchangeCode(ctx context.Context, code, redirect string) (*OIDCIdentity, error)
}

// SubjectUserStore extends UserStore with subject-based lookup for OIDC
// mappings. It is a separate interface (rather than a UserStore method)
// so pre-existing UserStore implementations outside this package keep
// compiling; PGUserStore satisfies it.
type SubjectUserStore interface {
	UserStore
	// GetBySubject loads the user whose OIDC subject matches; unknown
	// subjects return ErrUserNotFound.
	GetBySubject(ctx context.Context, subject string) (*User, error)
}

// OIDCUserProvisioner auto-maps an unknown OIDC identity onto a new
// adc_users row. The implementation must persist auth_source='OIDC' and
// metadata.oidc_subject so GetBySubject resolves it on the next login.
type OIDCUserProvisioner func(ctx context.Context, id *OIDCIdentity, defaultTenantID string, defaultRoles []string) (*User, error)

// OIDCStateStore stores single-use CSRF nonces for the OIDC dance.
type OIDCStateStore interface {
	// Set stores a state nonce with a TTL.
	Set(ctx context.Context, state string, ttl time.Duration) error
	// Consume verifies and removes a state nonce. It returns true only
	// when the nonce existed and was deleted (single use).
	Consume(ctx context.Context, state string) (bool, error)
}

// ValkeyOIDCStateStore implements OIDCStateStore on Valkey via the same
// minimal client surface as ValkeySessionStore.
type ValkeyOIDCStateStore struct {
	rdb valkeyCmd
}

// NewValkeyOIDCStateStore builds a state store over the given client.
func NewValkeyOIDCStateStore(rdb valkeyCmd) *ValkeyOIDCStateStore {
	return &ValkeyOIDCStateStore{rdb: rdb}
}

func oidcStateKey(state string) string {
	return oidcStateKeyPrefix + state
}

// Set stores the nonce marker under adc:oidc:state:{state}.
func (s *ValkeyOIDCStateStore) Set(ctx context.Context, state string, ttl time.Duration) error {
	if state == "" {
		return errors.New("adminauth: cannot store an empty oidc state")
	}
	if err := s.rdb.Set(ctx, oidcStateKey(state), "1", ttl).Err(); err != nil {
		return err
	}
	return nil
}

// Consume reads and deletes the nonce in one flow. Deleting is part of
// the verification: when the DEL fails the nonce is treated as unusable
// (fail closed), so a storage outage never silently disables CSRF checks.
func (s *ValkeyOIDCStateStore) Consume(ctx context.Context, state string) (bool, error) {
	err := s.rdb.Get(ctx, oidcStateKey(state)).Err()
	if errors.Is(err, redis.Nil) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := s.rdb.Del(ctx, oidcStateKey(state)).Err(); err != nil {
		return false, err
	}
	return true, nil
}

// OIDCHandler serves the OIDC authorization code endpoints.
type OIDCHandler struct {
	Provider OIDCProvider
	// Users must also implement SubjectUserStore (enforced per request).
	Users       UserStore
	Sessions    SessionStore
	States      OIDCStateStore
	Provisioner OIDCUserProvisioner // nil disables auto-provisioning

	Config OIDCConfig

	// DefaultTenantID is the tenant new OIDC users are auto-provisioned
	// into. DefaultRoles are the platform default roles granted to them;
	// the platform default is "tenant_admin" scoped to the default
	// tenant - never platform_admin - so an external IdP cannot mint a
	// platform superuser by itself (design/33 3.1.18 role model).
	DefaultTenantID string
	DefaultRoles    []string

	// FrontendRedirectURL is the post-login browser target (the session
	// arrives via the HttpOnly adc_session cookie, never in the URL).
	FrontendRedirectURL string

	sessionTTL time.Duration
	now        func() time.Time
}

// NewOIDCHandler builds the OIDC handler with the same defaults as
// NewHandler (24h sessions).
func NewOIDCHandler(users UserStore, sessions SessionStore, provider OIDCProvider, states OIDCStateStore, cfg OIDCConfig) *OIDCHandler {
	return &OIDCHandler{
		Provider:   provider,
		Users:      users,
		Sessions:   sessions,
		States:     states,
		Config:     cfg,
		sessionTTL: SessionTTL,
		now:        time.Now,
	}
}

// handleOIDCDisabled is the uniform answer when OIDC is not configured:
// 404 code 10004 (design/33: the resource does not exist).
func handleOIDCDisabled(w http.ResponseWriter, r *http.Request) {
	httpx.WriteError(w, http.StatusNotFound, codeNotFound, "oidc login is not enabled", httpx.TraceIDFrom(r))
}

// handleStart serves GET /v1/admin/auth/oidc/start: generate the CSRF
// nonce, store it in Valkey, then 302 to the IdP authorization URL.
func (h *OIDCHandler) handleStart(w http.ResponseWriter, r *http.Request) {
	if h.Provider == nil {
		handleOIDCDisabled(w, r)
		return
	}
	state, err := NewToken() // 256-bit random nonce, same generator as sessions
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, codeInternal, msgInternal, httpx.TraceIDFrom(r))
		return
	}
	if h.States == nil {
		httpx.WriteError(w, http.StatusInternalServerError, codeInternal, msgInternal, httpx.TraceIDFrom(r))
		return
	}
	if err := h.States.Set(r.Context(), state, OIDCStateTTL); err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, codeInternal, msgInternal, httpx.TraceIDFrom(r))
		return
	}
	http.Redirect(w, r, h.Provider.AuthorizationURL(state, h.Config.RedirectURL), http.StatusFound)
}

// handleCallback serves GET /v1/admin/auth/oidc/callback: consume the
// state nonce (CSRF), exchange the code, map the subject onto a user
// (auto-provision on first sight), create the session and redirect the
// browser back to the console frontend.
func (h *OIDCHandler) handleCallback(w http.ResponseWriter, r *http.Request) {
	if h.Provider == nil {
		handleOIDCDisabled(w, r)
		return
	}
	state := r.URL.Query().Get("state")
	code := r.URL.Query().Get("code")
	if state == "" {
		writeOIDCLoginFailed(w, r)
		return
	}
	if h.States == nil {
		httpx.WriteError(w, http.StatusInternalServerError, codeInternal, msgInternal, httpx.TraceIDFrom(r))
		return
	}
	ok, err := h.States.Consume(r.Context(), state)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, codeInternal, msgInternal, httpx.TraceIDFrom(r))
		return
	}
	if !ok {
		// Unknown or already-consumed nonce: CSRF or replay.
		writeOIDCLoginFailed(w, r)
		return
	}
	if code == "" {
		httpx.WriteError(w, http.StatusBadRequest, codeBadRequest, "authorization code is missing", httpx.TraceIDFrom(r))
		return
	}

	id, err := h.Provider.ExchangeCode(r.Context(), code, h.Config.RedirectURL)
	if err != nil || id == nil || id.Subject == "" {
		writeOIDCLoginFailed(w, r)
		return
	}

	subjects, ok2 := h.Users.(SubjectUserStore)
	if !ok2 {
		httpx.WriteError(w, http.StatusInternalServerError, codeInternal, msgInternal, httpx.TraceIDFrom(r))
		return
	}
	user, err := subjects.GetBySubject(r.Context(), id.Subject)
	if errors.Is(err, ErrUserNotFound) {
		if h.Provisioner == nil {
			writeOIDCLoginFailed(w, r)
			return
		}
		// Auto-provision: map the unknown subject onto a new adc_users
		// row in the default tenant with the platform default roles.
		// The provisioner must persist auth_source='OIDC' and
		// metadata.oidc_subject so the next login resolves via
		// GetBySubject instead of creating a second user.
		user, err = h.Provisioner(r.Context(), id, h.DefaultTenantID, h.DefaultRoles)
		if err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, codeInternal, msgInternal, httpx.TraceIDFrom(r))
			return
		}
	} else if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, codeInternal, msgInternal, httpx.TraceIDFrom(r))
		return
	}
	if user == nil || user.Status != userStatusActive {
		writeOIDCLoginFailed(w, r)
		return
	}

	token, err := NewToken()
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, codeInternal, msgInternal, httpx.TraceIDFrom(r))
		return
	}
	expiresAt := h.now().Add(h.sessionTTL)
	sess := &Session{
		TokenHash: HashToken(token),
		UserID:    user.ID,
		TenantID:  user.TenantID,
		Roles:     append([]string(nil), user.Roles...),
		ExpiresAt: expiresAt,
	}
	if err := h.Sessions.Create(r.Context(), sess, h.sessionTTL); err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, codeInternal, msgInternal, httpx.TraceIDFrom(r))
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     CookieSession,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
		Expires:  expiresAt,
	})
	target := h.FrontendRedirectURL
	if target == "" {
		target = "/"
	}
	http.Redirect(w, r, target, http.StatusFound)
}

// writeOIDCLoginFailed is the unified OIDC authentication failure: 401
// code 10002. Unlike handleLogin there is no username enumeration risk
// here (the IdP already authenticated the human), so stages share one
// opaque message.
func writeOIDCLoginFailed(w http.ResponseWriter, r *http.Request) {
	httpx.WriteError(w, http.StatusUnauthorized, codeUnauthorized, "oidc authentication failed", httpx.TraceIDFrom(r))
}
