package adminauth

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	"adc.dev/ce/internal/httpx"
)

// User is one admin user record as loaded from adc_users (design/32 3.2).
// Only the bcrypt hash is loaded: plaintext passwords never leave the
// storage layer (NFR-004).
type User struct {
	ID           string
	Username     string
	PasswordHash string   // bcrypt storage form (HashPassword)
	TenantID     string   // tenant bound to the user (SEC-02)
	Status       string   // adc_users.status: ACTIVE / DISABLED / LOCKED
	Roles        []string // design/33 3.1.18 role names, e.g. tenant_admin
}

// userStatusActive is the only status that may create a session.
const userStatusActive = "ACTIVE"

// ErrUserNotFound is returned by UserStore when no row matches the
// username.
var ErrUserNotFound = errors.New("adminauth: user not found")

// UserStore loads admin users by username (dependency seam for the PG
// implementation; design/80 B-01 handler contract).
type UserStore interface {
	GetByUsername(ctx context.Context, username string) (*User, error)
}

// Handler serves the admin auth endpoints (design/80 B-01, design/33
// 3.1.1): POST /v1/admin/auth/login and POST /v1/admin/auth/logout.
type Handler struct {
	users      UserStore
	sessions   SessionStore
	sessionTTL time.Duration
	now        func() time.Time
}

// NewHandler builds the auth handler; the session TTL defaults to
// SessionTTL (24h).
func NewHandler(users UserStore, sessions SessionStore) *Handler {
	return &Handler{
		users:      users,
		sessions:   sessions,
		sessionTTL: SessionTTL,
		now:        time.Now,
	}
}

// Routes returns the mux serving the auth endpoints.
func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/admin/auth/login", h.handleLogin)
	mux.HandleFunc("POST /v1/admin/auth/logout", h.handleLogout)
	return mux
}

// loginRequest mirrors design/33 3.1.1. mfa_code is reserved for the EE MFA
// path (stage 3) and ignored in V1.0.
type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
	MFACode  string `json:"mfa_code"`
}

// loginResponse mirrors design/33 3.1.1 plus the token itself (the caller
// also receives it via the Set-Cookie header).
type loginResponse struct {
	Token     string    `json:"token"`
	UserID    string    `json:"user_id"`
	TenantID  string    `json:"tenant_id"`
	Role      string    `json:"role"`
	ExpiresAt time.Time `json:"expires_at"`
}

// dummyHash is a fixed bcrypt hash compared against the presented password
// on branches that fail before the real password check (unknown user,
// disabled user), so response timing does not reveal whether the username
// exists (same technique as agentauth.compareDummy). It is computed lazily:
// bcrypt costs ~60ms and must not be paid by every test binary.
var (
	dummyOnce sync.Once
	dummyHash []byte
)

func compareDummy(password string) {
	dummyOnce.Do(func() {
		h, err := bcrypt.GenerateFromPassword([]byte("adc-dummy-password-for-timing-normalization"), bcrypt.DefaultCost)
		if err != nil {
			panic("adminauth: cannot seed dummy bcrypt hash: " + err.Error())
		}
		dummyHash = h
	})
	_ = bcrypt.CompareHashAndPassword(dummyHash, []byte(password))
}

// handleLogin authenticates username+password and issues a session. Every
// credential-related failure answers the same 401 code 10002 with the same
// body: the username existence, user status and password validity are never
// distinguishable (design/33 3.1.1).
func (h *Handler) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&req); err != nil ||
		req.Username == "" || req.Password == "" {
		httpx.WriteError(w, http.StatusBadRequest, codeBadRequest, msgBadRequest, httpx.TraceIDFrom(r))
		return
	}

	user, err := h.users.GetByUsername(r.Context(), req.Username)
	if err != nil {
		if errors.Is(err, ErrUserNotFound) {
			compareDummy(req.Password)
			writeLoginFailed(w, r)
			return
		}
		httpx.WriteError(w, http.StatusInternalServerError, codeInternal, msgInternal, httpx.TraceIDFrom(r))
		return
	}
	if user == nil || user.Status != userStatusActive {
		compareDummy(req.Password)
		writeLoginFailed(w, r)
		return
	}
	if err := VerifyPassword(req.Password, user.PasswordHash); err != nil {
		writeLoginFailed(w, r)
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
	if err := h.sessions.Create(r.Context(), sess, h.sessionTTL); err != nil {
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
	httpx.WriteJSON(w, http.StatusOK, loginResponse{
		Token:     token,
		UserID:    user.ID,
		TenantID:  user.TenantID,
		Role:      primaryRole(user.Roles),
		ExpiresAt: expiresAt,
	})
}

// handleLogout deletes the caller's session (identified by bearer token or
// adc_session cookie) and clears the cookie. Logout is idempotent: an
// unknown or already-deleted session still answers 204.
func (h *Handler) handleLogout(w http.ResponseWriter, r *http.Request) {
	token := ExtractToken(r)
	if token == "" {
		writeUnauthorized(w, r)
		return
	}
	if err := h.sessions.Delete(r.Context(), HashToken(token)); err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, codeInternal, msgInternal, httpx.TraceIDFrom(r))
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     CookieSession,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
	w.WriteHeader(http.StatusNoContent)
}

// primaryRole returns the first role of the user, or "" when the user has
// no roles; it fills the singular "role" field of design/33 3.1.1.
func primaryRole(roles []string) string {
	if len(roles) == 0 {
		return ""
	}
	return roles[0]
}

// writeLoginFailed emits the unified login failure: 401 code 10002 with a
// generic message that leaks no information about the account.
func writeLoginFailed(w http.ResponseWriter, r *http.Request) {
	httpx.WriteError(w, http.StatusUnauthorized, codeUnauthorized, "invalid username or password", httpx.TraceIDFrom(r))
}
