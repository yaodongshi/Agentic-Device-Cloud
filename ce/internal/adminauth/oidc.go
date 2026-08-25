package adminauth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/redis/go-redis/v9"

	"adc.dev/ce/internal/httpx"
)

const (
	codeNotFound              = "10004"
	OIDCStateTTL              = 10 * time.Minute
	oidcStateKeyPrefix        = "adc:oidc:state:"
	CookieOIDCTransaction     = "adc_oidc_transaction"
	oidcTransactionCookiePath = "/v1/admin/auth/oidc/"
)

type OIDCConfig struct {
	Issuer       string
	ClientID     string
	ClientSecret string
	RedirectURL  string
	Scope        string
}

type OIDCIdentity struct {
	Issuer   string
	Subject  string
	Audience []string
	AZP      string
	Email    string
	Name     string
	Nonce    string
}

type OIDCTransaction struct {
	Nonce          string `json:"nonce"`
	PKCEVerifier   string `json:"pkce_verifier"`
	BrowserBinding string `json:"browser_binding"`
}

type OIDCProvider interface {
	AuthorizationURL(state, nonce, codeChallenge, redirect string) (string, error)
	ExchangeCode(ctx context.Context, code, redirect, codeVerifier string) (*OIDCIdentity, error)
}

type OIDCIdentityStore interface {
	UserStore
	GetByOIDCIdentity(ctx context.Context, issuer, subject string) (*User, error)
}

type OIDCStateStore interface {
	Set(ctx context.Context, state string, tx *OIDCTransaction, ttl time.Duration) error
	Consume(ctx context.Context, state, browserBinding string) (*OIDCTransaction, error)
}

type OIDCAuditEvent struct {
	Result   string
	Reason   string
	Issuer   string
	Subject  string
	UserID   string
	TenantID string
	TraceID  string
}

type OIDCAuditSink interface {
	RecordOIDCLogin(context.Context, OIDCAuditEvent) error
}

type oidcValkeyCmd interface {
	Set(ctx context.Context, key string, value interface{}, expiration time.Duration) *redis.StatusCmd
	Eval(ctx context.Context, script string, keys []string, args ...interface{}) *redis.Cmd
}

type ValkeyOIDCStateStore struct{ rdb oidcValkeyCmd }

func NewValkeyOIDCStateStore(rdb oidcValkeyCmd) *ValkeyOIDCStateStore {
	return &ValkeyOIDCStateStore{rdb: rdb}
}

func oidcStateKey(state string) string { return oidcStateKeyPrefix + state }

func (s *ValkeyOIDCStateStore) Set(ctx context.Context, state string, tx *OIDCTransaction, ttl time.Duration) error {
	if state == "" || tx == nil || tx.Nonce == "" || tx.PKCEVerifier == "" || tx.BrowserBinding == "" {
		return errors.New("adminauth: incomplete oidc transaction")
	}
	raw, err := json.Marshal(tx)
	if err != nil {
		return fmt.Errorf("adminauth: encode oidc transaction: %w", err)
	}
	if err := s.rdb.Set(ctx, oidcStateKey(state), string(raw), ttl).Err(); err != nil {
		return fmt.Errorf("adminauth: store oidc transaction: %w", err)
	}
	return nil
}

const consumeOIDCTransactionScript = `
local value = redis.call('GET', KEYS[1])
if not value then return nil end
local transaction = cjson.decode(value)
if transaction.browser_binding ~= ARGV[1] then return nil end
redis.call('DEL', KEYS[1])
return value
`

func (s *ValkeyOIDCStateStore) Consume(ctx context.Context, state, browserBinding string) (*OIDCTransaction, error) {
	if state == "" || browserBinding == "" {
		return nil, nil
	}
	result, err := s.rdb.Eval(ctx, consumeOIDCTransactionScript, []string{oidcStateKey(state)}, HashToken(browserBinding)).Result()
	if errors.Is(err, redis.Nil) || result == nil {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("adminauth: consume oidc transaction: %w", err)
	}
	raw, ok := result.(string)
	if !ok {
		return nil, errors.New("adminauth: invalid oidc transaction result")
	}
	var tx OIDCTransaction
	if err := json.Unmarshal([]byte(raw), &tx); err != nil {
		return nil, fmt.Errorf("adminauth: decode oidc transaction: %w", err)
	}
	return &tx, nil
}

type OIDCHandler struct {
	Provider            OIDCProvider
	Users               UserStore
	Sessions            SessionStore
	States              OIDCStateStore
	Config              OIDCConfig
	FrontendRedirectURL string
	Audit               OIDCAuditSink
	sessionTTL          time.Duration
	now                 func() time.Time
}

func NewOIDCHandler(users UserStore, sessions SessionStore, provider OIDCProvider, states OIDCStateStore, cfg OIDCConfig) *OIDCHandler {
	return &OIDCHandler{Provider: provider, Users: users, Sessions: sessions, States: states, Config: cfg, sessionTTL: SessionTTL, now: time.Now}
}

func handleOIDCDisabled(w http.ResponseWriter, r *http.Request) {
	httpx.WriteError(w, http.StatusNotFound, codeNotFound, "oidc login is not enabled", httpx.TraceIDFrom(r))
}

func handleOIDCCallbackDisabled(w http.ResponseWriter, r *http.Request) {
	setOIDCCallbackHeaders(w)
	handleOIDCDisabled(w, r)
}

func handleOIDCStatusDisabled(w http.ResponseWriter, _ *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, map[string]bool{"enabled": false})
}

func (h *OIDCHandler) handleStatus(w http.ResponseWriter, _ *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, map[string]bool{"enabled": h.Provider != nil})
}

func (h *OIDCHandler) handleStart(w http.ResponseWriter, r *http.Request) {
	if h.Provider == nil {
		handleOIDCDisabled(w, r)
		return
	}
	state, err := NewToken()
	if err != nil {
		writeInternal(w, r)
		return
	}
	nonce, err := NewToken()
	if err != nil {
		writeInternal(w, r)
		return
	}
	verifier, err := NewToken()
	if err != nil {
		writeInternal(w, r)
		return
	}
	binding, err := NewToken()
	if err != nil {
		writeInternal(w, r)
		return
	}
	tx := &OIDCTransaction{Nonce: nonce, PKCEVerifier: verifier, BrowserBinding: HashToken(binding)}
	if h.States == nil || h.States.Set(r.Context(), state, tx, OIDCStateTTL) != nil {
		writeInternal(w, r)
		return
	}
	challengeSum := sha256.Sum256([]byte(verifier))
	authorizationURL, err := h.Provider.AuthorizationURL(state, nonce, base64.RawURLEncoding.EncodeToString(challengeSum[:]), h.Config.RedirectURL)
	if err != nil || authorizationURL == "" {
		writeOIDCLoginFailed(w, r)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: CookieOIDCTransaction, Value: binding, Path: oidcTransactionCookiePath, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode, MaxAge: int(OIDCStateTTL.Seconds())})
	http.Redirect(w, r, authorizationURL, http.StatusFound)
}

func (h *OIDCHandler) handleCallback(w http.ResponseWriter, r *http.Request) {
	setOIDCCallbackHeaders(w)
	auditEvent := OIDCAuditEvent{Result: "rejected", Reason: "callback_rejected", TraceID: httpx.TraceIDFrom(r)}
	defer func() {
		if h.Audit != nil {
			_ = h.Audit.RecordOIDCLogin(r.Context(), auditEvent)
		}
	}()
	if h.Provider == nil {
		handleOIDCDisabled(w, r)
		return
	}
	state, code := r.URL.Query().Get("state"), r.URL.Query().Get("code")
	bindingCookie, err := r.Cookie(CookieOIDCTransaction)
	if err != nil || state == "" || h.States == nil {
		writeOIDCLoginFailed(w, r)
		return
	}
	tx, err := h.States.Consume(r.Context(), state, bindingCookie.Value)
	clearOIDCTransactionCookie(w)
	if err != nil {
		writeInternal(w, r)
		return
	}
	if tx == nil {
		writeOIDCLoginFailed(w, r)
		return
	}
	if code == "" {
		httpx.WriteError(w, http.StatusBadRequest, codeBadRequest, "authorization code is missing", httpx.TraceIDFrom(r))
		return
	}
	id, err := h.Provider.ExchangeCode(r.Context(), code, h.Config.RedirectURL, tx.PKCEVerifier)
	if err != nil || id == nil || id.Issuer != h.Config.Issuer || id.Subject == "" || id.Nonce != tx.Nonce {
		writeOIDCLoginFailed(w, r)
		return
	}
	auditEvent.Issuer, auditEvent.Subject = id.Issuer, id.Subject
	identities, ok := h.Users.(OIDCIdentityStore)
	if !ok {
		writeInternal(w, r)
		return
	}
	user, err := identities.GetByOIDCIdentity(r.Context(), id.Issuer, id.Subject)
	if errors.Is(err, ErrUserNotFound) {
		writeOIDCLoginFailed(w, r)
		return
	}
	if err != nil {
		writeInternal(w, r)
		return
	}
	if user == nil || user.Status != userStatusActive {
		writeOIDCLoginFailed(w, r)
		return
	}
	auditEvent.UserID, auditEvent.TenantID = user.ID, user.TenantID
	token, err := NewToken()
	if err != nil {
		writeInternal(w, r)
		return
	}
	expiresAt := h.now().Add(h.sessionTTL)
	sess := &Session{TokenHash: HashToken(token), UserID: user.ID, TenantID: user.TenantID, Roles: append([]string(nil), user.Roles...), ExpiresAt: expiresAt}
	if err := h.Sessions.Create(r.Context(), sess, h.sessionTTL); err != nil {
		writeInternal(w, r)
		return
	}
	auditEvent.Result, auditEvent.Reason = "succeeded", ""
	http.SetCookie(w, &http.Cookie{Name: CookieSession, Value: token, Path: "/", HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode, Expires: expiresAt})
	target := h.FrontendRedirectURL
	if target == "" {
		target = "/login?oidc=success"
	}
	http.Redirect(w, r, target, http.StatusFound)
}

func setOIDCCallbackHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Referrer-Policy", "no-referrer")
}

func clearOIDCTransactionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: CookieOIDCTransaction, Value: "", Path: oidcTransactionCookiePath, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode, MaxAge: -1})
}

func writeInternal(w http.ResponseWriter, r *http.Request) {
	httpx.WriteError(w, http.StatusInternalServerError, codeInternal, msgInternal, httpx.TraceIDFrom(r))
}

func writeOIDCLoginFailed(w http.ResponseWriter, r *http.Request) {
	httpx.WriteError(w, http.StatusUnauthorized, codeUnauthorized, "oidc authentication failed", httpx.TraceIDFrom(r))
}
