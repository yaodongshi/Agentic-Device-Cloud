package adminapi

import (
	"context"
	"crypto/rand"
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
	"adc.dev/ce/internal/agentauth"
	"adc.dev/ce/internal/httpx"
)

// Agent API key repository sentinels.
var (
	ErrApiKeyNotFound       = errors.New("adminapi: api key not found")
	ErrApiKeyAlreadyRevoked = errors.New("adminapi: api key already revoked")
	// ErrApiKeyConflict signals a key-hash collision; handlers retry with
	// fresh key material before surfacing an error.
	ErrApiKeyConflict = errors.New("adminapi: api key hash collision")
)

// ApiKey statuses as derived from revoked_at (design/32 3.7: revocation is
// a timestamp, not a status column).
const (
	apiKeyStatusActive  = "active"
	apiKeyStatusRevoked = "revoked"
)

// keyScopeRe validates one scopes entry (design/33 3.1.12: "*", tool names
// and "device_code__*" / "*__tool_name" forms share a conservative charset).
var keyScopeRe = regexp.MustCompile(`^[A-Za-z0-9_*-]{1,128}$`)

// keyMaxLifetime caps expires_at at 365 days out (design/33 3.1.12).
const keyMaxLifetime = 365 * 24 * time.Hour

// keyIssueRetries bounds regeneration attempts on key-hash collisions.
const keyIssueRetries = 3

// ApiKey is one adc_agent_api_keys row (design/32 3.7, migration 0001).
// Only hashes are persisted; the plaintext secret exists solely in the
// issuance response (NFR-004). Status is derived: revoked_at == nil means
// active.
type ApiKey struct {
	ID         string
	TenantID   string
	Name       string
	AgentID    string
	KeyPrefix  string
	Scopes     []string
	ScopeMode  string // ALLOW_LIST only in V1
	ExpiresAt  *time.Time
	RevokedAt  *time.Time
	LastUsedAt *time.Time
	CreatedAt  time.Time
}

// NewKey carries the issuance-time key material handed to the repository:
// only hashes travel to storage (agentauth.HashKeyID / HashSecret).
type NewKey struct {
	TenantID   string
	Name       string
	AgentID    string
	KeyPrefix  string
	KeyHash    string
	SecretHash string
	Scopes     []string
	ExpiresAt  *time.Time
	CreatedBy  string
}

// ApiKeyRepo persists agent API keys (design/31 LLD 3.4.2 seam).
type ApiKeyRepo interface {
	Issue(ctx context.Context, k *NewKey) (*ApiKey, error)
	Get(ctx context.Context, keyID string) (*ApiKey, error)
	List(ctx context.Context, tenantID string, p Page) ([]ApiKey, int, error)
	Revoke(ctx context.Context, keyID, reason string) (*ApiKey, error)
	// Rotate issues a new key and revokes the old one in one transaction;
	// the new key is returned and the old one keeps its record with
	// revoked_at set.
	Rotate(ctx context.Context, keyID string, k *NewKey, reason string) (*ApiKey, error)
}

// generateKeyMaterial creates one Agent API key token: "adc_<keyID>_<secret>"
// (design/33 1.2). keyID is 8 lowercase hex chars (4 random bytes) and the
// secret is 32 random bytes hex-encoded (64 chars, inside the agentauth
// secret charset). Only SHA-256 hashes are persisted — agentauth.HashKeyID
// for the prefix, agentauth.HashSecret for the secret (NFR-004).
func generateKeyMaterial() (keyID, prefix, secret, keyHash, secretHash string, err error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", "", "", "", "", fmt.Errorf("adminapi: generate key id: %w", err)
	}
	keyID = hex.EncodeToString(b[:])
	prefix = "adc_" + keyID
	sb := make([]byte, 32)
	if _, err := rand.Read(sb); err != nil {
		return "", "", "", "", "", fmt.Errorf("adminapi: generate key secret: %w", err)
	}
	secret = hex.EncodeToString(sb)
	return keyID, prefix, secret, agentauth.HashKeyID(keyID), agentauth.HashSecret(secret), nil
}

// validateKeyExpiry enforces design/33 3.1.12: expires_at, when set, must
// be in the future and at most 365 days out. nil means never expires
// (allowed in V1, forced later per design/32 3.7 note).
func validateKeyExpiry(expires *time.Time, now time.Time) error {
	if expires == nil {
		return nil
	}
	if !expires.After(now) {
		return errors.New("expires_at must be in the future")
	}
	if expires.After(now.Add(keyMaxLifetime)) {
		return errors.New("expires_at must be within 365 days")
	}
	return nil
}

func mapApiKeyRepoError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ErrTenantNotFound):
		writeError(w, r, http.StatusNotFound, codeTenantNotFound, "tenant not found")
	case errors.Is(err, ErrTenantSuspended):
		writeError(w, r, http.StatusForbidden, codeTenantSuspended, "tenant suspended")
	case errors.Is(err, ErrApiKeyNotFound):
		writeError(w, r, http.StatusNotFound, codeNotFound, "api key not found")
	case errors.Is(err, ErrApiKeyAlreadyRevoked):
		writeError(w, r, http.StatusConflict, codeConflict, "api key already revoked")
	default:
		writeError(w, r, http.StatusInternalServerError, codeInternal, "internal error")
	}
}

// ---------------------------------------------------------------------------
// handlers
// ---------------------------------------------------------------------------

type keyScopesRequest struct {
	AllowedTools []string `json:"allowed_tools"`
}

type keyScopesResponse struct {
	AllowedTools []string `json:"allowed_tools"`
}

// issueKeyRequest mirrors design/33 3.1.12.
type issueKeyRequest struct {
	Name      string           `json:"name"`
	AgentID   string           `json:"agent_id"`
	Scopes    keyScopesRequest `json:"scopes"`
	ExpiresAt *time.Time       `json:"expires_at"`
}

// apiKeyResponse mirrors design/33 3.1.12 (issuance, Key present exactly
// once) and 3.1.13 (listing, Key omitted).
type apiKeyResponse struct {
	KeyID      string            `json:"key_id"`
	Name       string            `json:"name"`
	Key        string            `json:"key,omitempty"`
	KeyPrefix  string            `json:"key_prefix"`
	Scopes     keyScopesResponse `json:"scopes"`
	Status     string            `json:"status"`
	LastUsedAt *time.Time        `json:"last_used_at,omitempty"`
	ExpiresAt  *time.Time        `json:"expires_at"`
	CreatedAt  time.Time         `json:"created_at"`
}

type apiKeyListResponse struct {
	Items    []apiKeyResponse `json:"items"`
	Total    int              `json:"total"`
	Page     int              `json:"page"`
	PageSize int              `json:"page_size"`
}

// revokeKeyRequest carries the mandatory revocation reason (FR-009:
// revocation is audited with a reason).
type revokeKeyRequest struct {
	Reason string `json:"reason"`
}

type apiKeyRevokeResponse struct {
	KeyID     string     `json:"key_id"`
	Name      string     `json:"name"`
	KeyPrefix string     `json:"key_prefix"`
	Status    string     `json:"status"`
	RevokedAt *time.Time `json:"revoked_at"`
}

// apiKeyRotateResponse extends the issuance shape with the replaced key
// (design/80 F-08 rotate; design/33 3.1.14 documents only plain revoke).
type apiKeyRotateResponse struct {
	apiKeyResponse
	PreviousKeyID string `json:"previous_key_id"`
}

func newApiKeyResponse(k *ApiKey, plaintext string) apiKeyResponse {
	return apiKeyResponse{
		KeyID:      k.ID,
		Name:       k.Name,
		Key:        plaintext,
		KeyPrefix:  k.KeyPrefix,
		Scopes:     keyScopesResponse{AllowedTools: k.Scopes},
		Status:     apiKeyStatus(k),
		LastUsedAt: k.LastUsedAt,
		ExpiresAt:  k.ExpiresAt,
		CreatedAt:  k.CreatedAt.UTC(),
	}
}

func apiKeyStatus(k *ApiKey) string {
	if k.RevokedAt == nil {
		return apiKeyStatusActive
	}
	return apiKeyStatusRevoked
}

// normalizeKeyScopes validates the allowed_tools list and guarantees a
// non-nil slice so the stored JSONB is always "[]", never null.
func normalizeKeyScopes(req keyScopesRequest) ([]string, bool) {
	if req.AllowedTools == nil {
		return []string{}, true
	}
	for _, s := range req.AllowedTools {
		if !keyScopeRe.MatchString(s) {
			return nil, false
		}
	}
	return req.AllowedTools, true
}

// handleIssueKey serves POST /v1/admin/agent-keys (design/33 3.1.12). The
// complete key is returned exactly once; storage keeps only SHA-256 hashes.
// Note: the design/33 3.1.12 key-count quota check (13003) is deferred —
// migration 0001 defines no agent-key quota column (design/32 3.1 lists
// only quota_devices/quota_calls_monthly/quota_concurrent).
func (s *Server) handleIssueKey(w http.ResponseWriter, r *http.Request) {
	p, ok := adminauth.FromContext(r.Context())
	if !ok {
		writeError(w, r, http.StatusUnauthorized, codeUnauthorized, "unauthenticated")
		return
	}
	tenantID, ok := s.resolveTenant(w, r, p)
	if !ok {
		return
	}
	var req issueKeyRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Name == "" || len(req.Name) > 255 {
		writeError(w, r, http.StatusBadRequest, codeBadRequest, "name is required (max 255 chars)")
		return
	}
	if len(req.AgentID) > 64 {
		writeError(w, r, http.StatusBadRequest, codeBadRequest, "agent_id must be at most 64 chars")
		return
	}
	scopes, valid := normalizeKeyScopes(req.Scopes)
	if !valid {
		writeError(w, r, http.StatusBadRequest, codeBadRequest, "scopes.allowed_tools contains an invalid tool pattern")
		return
	}
	if err := validateKeyExpiry(req.ExpiresAt, s.now()); err != nil {
		writeError(w, r, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}

	var issued *ApiKey
	for attempt := 0; attempt < keyIssueRetries; attempt++ {
		_, prefix, secret, keyHash, secretHash, err := generateKeyMaterial()
		if err != nil {
			writeError(w, r, http.StatusInternalServerError, codeInternal, "key material generation failed")
			return
		}
		issued, err = s.APIKeys.Issue(r.Context(), &NewKey{
			TenantID:   tenantID,
			Name:       req.Name,
			AgentID:    req.AgentID,
			KeyPrefix:  prefix,
			KeyHash:    keyHash,
			SecretHash: secretHash,
			Scopes:     scopes,
			ExpiresAt:  req.ExpiresAt,
			CreatedBy:  p.UserID,
		})
		if errors.Is(err, ErrApiKeyConflict) {
			continue // hash collision: regenerate and retry
		}
		if err != nil {
			mapApiKeyRepoError(w, r, err)
			return
		}
		s.recordAudit(r.Context(), AdminOp{
			EventID:  newEventID(),
			TenantID: tenantID,
			ActorID:  p.UserID,
			Action:   "apikey.issue",
			Target:   issued.ID,
			TraceID:  httpx.TraceIDFrom(r),
		})
		httpx.WriteJSON(w, http.StatusCreated, newApiKeyResponse(issued, prefix+"_"+secret))
		return
	}
	writeError(w, r, http.StatusInternalServerError, codeInternal, "key issuance retries exhausted")
}

// handleListKeys serves GET /v1/admin/agent-keys (design/33 3.1.13): only
// the key_prefix mask is exposed, never the full key.
func (s *Server) handleListKeys(w http.ResponseWriter, r *http.Request) {
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
	list, total, err := s.APIKeys.List(r.Context(), tenantID, page)
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, codeInternal, "list api keys failed")
		return
	}
	items := make([]apiKeyResponse, 0, len(list))
	for i := range list {
		items = append(items, newApiKeyResponse(&list[i], ""))
	}
	httpx.WriteJSON(w, http.StatusOK, apiKeyListResponse{Items: items, Total: total, Page: page.Number, PageSize: page.Size})
}

// loadOwnedKey loads the target key and enforces tenant ownership
// (design/33 1.2, SEC-02); it writes the response on failure.
func (s *Server) loadOwnedKey(w http.ResponseWriter, r *http.Request, p *adminauth.Principal, keyID string) (*ApiKey, bool) {
	if !requireUUID(w, r, "key_id", keyID) {
		return nil, false
	}
	k, err := s.APIKeys.Get(r.Context(), keyID)
	if err != nil {
		if errors.Is(err, ErrApiKeyNotFound) {
			writeError(w, r, http.StatusNotFound, codeNotFound, "api key not found")
			return nil, false
		}
		writeError(w, r, http.StatusInternalServerError, codeInternal, "load api key failed")
		return nil, false
	}
	if !s.checkOwnership(w, r, p, k.TenantID) {
		return nil, false
	}
	return k, true
}

// handleRevokeKey serves POST /v1/admin/agent-keys/{keyID}/revoke. Revoked
// keys answer 401 on the Agent API immediately (FR-009 acceptance; the
// 24h parallel transition for rotation is achieved via rotate).
func (s *Server) handleRevokeKey(w http.ResponseWriter, r *http.Request) {
	p, ok := adminauth.FromContext(r.Context())
	if !ok {
		writeError(w, r, http.StatusUnauthorized, codeUnauthorized, "unauthenticated")
		return
	}
	keyID := r.PathValue("keyID")
	if !requireUUID(w, r, "key_id", keyID) {
		return
	}
	var req revokeKeyRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Reason == "" {
		writeError(w, r, http.StatusBadRequest, codeBadRequest, "reason is required")
		return
	}
	if _, ok := s.loadOwnedKey(w, r, p, keyID); !ok {
		return
	}
	revoked, err := s.APIKeys.Revoke(r.Context(), keyID, req.Reason)
	if err != nil {
		mapApiKeyRepoError(w, r, err)
		return
	}
	s.recordAudit(r.Context(), AdminOp{
		EventID:  newEventID(),
		TenantID: revoked.TenantID,
		ActorID:  p.UserID,
		Action:   "apikey.revoke",
		Target:   keyID,
		Reason:   req.Reason,
		TraceID:  httpx.TraceIDFrom(r),
	})
	httpx.WriteJSON(w, http.StatusOK, apiKeyRevokeResponse{
		KeyID:     revoked.ID,
		Name:      revoked.Name,
		KeyPrefix: revoked.KeyPrefix,
		Status:    apiKeyStatus(revoked),
		RevokedAt: revoked.RevokedAt,
	})
}

// handleRotateKey serves POST /v1/admin/agent-keys/{keyID}/rotate
// (design/80 F-08): a new key inherits the old one's metadata and the old
// key is revoked in the same transaction. FR-009's 24h parallel transition
// window needs auth-layer support for a future revoked_at and is deferred;
// V1 rotates with immediate revocation to stay fail-closed.
func (s *Server) handleRotateKey(w http.ResponseWriter, r *http.Request) {
	p, ok := adminauth.FromContext(r.Context())
	if !ok {
		writeError(w, r, http.StatusUnauthorized, codeUnauthorized, "unauthenticated")
		return
	}
	keyID := r.PathValue("keyID")
	old, ok := s.loadOwnedKey(w, r, p, keyID)
	if !ok {
		return
	}
	var req revokeKeyRequest // optional body; reason defaults below
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes)).Decode(&req)
	reason := strings.TrimSpace(req.Reason)
	if reason == "" {
		reason = "rotated"
	}
	if old.RevokedAt != nil {
		writeError(w, r, http.StatusConflict, codeConflict, "api key already revoked")
		return
	}

	var rotated *ApiKey
	for attempt := 0; attempt < keyIssueRetries; attempt++ {
		_, prefix, secret, keyHash, secretHash, err := generateKeyMaterial()
		if err != nil {
			writeError(w, r, http.StatusInternalServerError, codeInternal, "key material generation failed")
			return
		}
		rotated, err = s.APIKeys.Rotate(r.Context(), keyID, &NewKey{
			TenantID:   old.TenantID,
			Name:       old.Name,
			AgentID:    old.AgentID,
			KeyPrefix:  prefix,
			KeyHash:    keyHash,
			SecretHash: secretHash,
			Scopes:     old.Scopes,
			ExpiresAt:  old.ExpiresAt,
			CreatedBy:  p.UserID,
		}, reason)
		if errors.Is(err, ErrApiKeyConflict) {
			continue
		}
		if err != nil {
			mapApiKeyRepoError(w, r, err)
			return
		}
		s.recordAudit(r.Context(), AdminOp{
			EventID:  newEventID(),
			TenantID: rotated.TenantID,
			ActorID:  p.UserID,
			Action:   "apikey.rotate",
			Target:   keyID,
			Reason:   reason,
			TraceID:  httpx.TraceIDFrom(r),
		})
		resp := apiKeyRotateResponse{
			apiKeyResponse: newApiKeyResponse(rotated, prefix+"_"+secret),
			PreviousKeyID:  keyID,
		}
		httpx.WriteJSON(w, http.StatusCreated, resp)
		return
	}
	writeError(w, r, http.StatusInternalServerError, codeInternal, "key issuance retries exhausted")
}

// ---------------------------------------------------------------------------
// PostgreSQL repository
// ---------------------------------------------------------------------------

// keyCols rebuilds an ApiKey (uuid cast to text, see approval.PGTicketRepo).
const keyCols = `id::text, tenant_id::text, name, agent_id, key_prefix, scopes,
	expires_at, revoked_at, last_used_at, created_at`

// pgApiKeyRepo is the PostgreSQL ApiKeyRepo (design/32 3.7, migration 0001).
type pgApiKeyRepo struct {
	pool pgxPooler
}

// NewPGApiKeyRepo builds an api key repository over an existing pgx pool.
func NewPGApiKeyRepo(pool pgxPooler) *pgApiKeyRepo {
	return &pgApiKeyRepo{pool: pool}
}

func scanApiKey(row pgx.Row) (*ApiKey, error) {
	var (
		k         ApiKey
		scopesRaw []byte
	)
	err := row.Scan(&k.ID, &k.TenantID, &k.Name, &k.AgentID, &k.KeyPrefix,
		&scopesRaw, &k.ExpiresAt, &k.RevokedAt, &k.LastUsedAt, &k.CreatedAt)
	if err != nil {
		return nil, err
	}
	k.ScopeMode = "ALLOW_LIST"
	k.Scopes = []string{}
	if len(scopesRaw) > 0 {
		if err := json.Unmarshal(scopesRaw, &k.Scopes); err != nil {
			return nil, err
		}
	}
	if k.Scopes == nil {
		k.Scopes = []string{}
	}
	return &k, nil
}

// tenantGate returns the tenant status and maps missing/suspended tenants
// onto the shared sentinels.
func (r *pgApiKeyRepo) tenantGate(ctx context.Context, q interface {
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

// Issue inserts a new key inside a transaction that also gates on the
// tenant being active. The key-hash unique index maps collisions onto
// ErrApiKeyConflict for the handler's regeneration retry.
func (r *pgApiKeyRepo) Issue(ctx context.Context, k *NewKey) (*ApiKey, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if err := r.tenantGate(ctx, tx, k.TenantID); err != nil {
		return nil, err
	}
	scopes, err := json.Marshal(k.Scopes)
	if err != nil {
		return nil, err
	}
	row := tx.QueryRow(ctx, `INSERT INTO adc_agent_api_keys
		(tenant_id, name, agent_id, key_prefix, key_hash, secret_hash,
		 scope_mode, scopes, expires_at, created_by)
		VALUES ($1::uuid, $2, $3, $4, $5, $6, 'ALLOW_LIST', $7, $8, $9::uuid)
		RETURNING `+keyCols,
		k.TenantID, k.Name, k.AgentID, k.KeyPrefix, k.KeyHash, k.SecretHash, string(scopes), k.ExpiresAt, k.CreatedBy)
	created, err := scanApiKey(row)
	if isUniqueViolation(err) {
		return nil, ErrApiKeyConflict
	}
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return created, nil
}

// Get loads one key by id; revocation keeps the row queryable (design/32
// 3.7: revoked rows are retained for audit linkage).
func (r *pgApiKeyRepo) Get(ctx context.Context, keyID string) (*ApiKey, error) {
	row := r.pool.QueryRow(ctx, `SELECT `+keyCols+` FROM adc_agent_api_keys
		WHERE id = $1::uuid`, keyID)
	k, err := scanApiKey(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrApiKeyNotFound
	}
	if err != nil {
		return nil, err
	}
	return k, nil
}

// List pages the tenant's keys, newest first, with the window total.
func (r *pgApiKeyRepo) List(ctx context.Context, tenantID string, p Page) ([]ApiKey, int, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+keyCols+`, count(*) OVER () AS total
		FROM adc_agent_api_keys
		WHERE tenant_id = $1::uuid
		ORDER BY created_at DESC, id
		LIMIT $2 OFFSET $3`, tenantID, p.Size, p.offset())
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var (
		out   []ApiKey
		total int
	)
	for rows.Next() {
		var k ApiKey
		var scopesRaw []byte
		if err := rows.Scan(&k.ID, &k.TenantID, &k.Name, &k.AgentID, &k.KeyPrefix,
			&scopesRaw, &k.ExpiresAt, &k.RevokedAt, &k.LastUsedAt, &k.CreatedAt, &total); err != nil {
			return nil, 0, err
		}
		k.ScopeMode = "ALLOW_LIST"
		k.Scopes = []string{}
		if len(scopesRaw) > 0 {
			if err := json.Unmarshal(scopesRaw, &k.Scopes); err != nil {
				return nil, 0, err
			}
		}
		out = append(out, k)
	}
	return out, total, rows.Err()
}

// Revoke stamps revoked_at in one conditional UPDATE (only while still
// NULL); zero rows are re-read and classified.
func (r *pgApiKeyRepo) Revoke(ctx context.Context, keyID, reason string) (*ApiKey, error) {
	row := r.pool.QueryRow(ctx, `UPDATE adc_agent_api_keys
		SET revoked_at = now(), revoke_reason = $2, updated_at = now()
		WHERE id = $1::uuid AND revoked_at IS NULL
		RETURNING `+keyCols, keyID, reason)
	revoked, err := scanApiKey(row)
	if err == nil {
		return revoked, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	cur, getErr := r.Get(ctx, keyID)
	if getErr != nil {
		return nil, getErr
	}
	if cur.RevokedAt != nil {
		return nil, ErrApiKeyAlreadyRevoked
	}
	return nil, ErrApiKeyNotFound
}

// Rotate revokes the old key and inserts the replacement in one
// transaction; the new key record is returned.
func (r *pgApiKeyRepo) Rotate(ctx context.Context, keyID string, k *NewKey, reason string) (*ApiKey, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx, `UPDATE adc_agent_api_keys
		SET revoked_at = now(), revoke_reason = $2, updated_at = now()
		WHERE id = $1::uuid AND revoked_at IS NULL`, keyID, reason)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		var oldRevoked *time.Time
		err := tx.QueryRow(ctx, `SELECT revoked_at FROM adc_agent_api_keys
			WHERE id = $1::uuid`, keyID).Scan(&oldRevoked)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrApiKeyNotFound
		}
		if err != nil {
			return nil, err
		}
		if oldRevoked != nil {
			return nil, ErrApiKeyAlreadyRevoked
		}
		return nil, ErrApiKeyNotFound
	}
	scopes, err := json.Marshal(k.Scopes)
	if err != nil {
		return nil, err
	}
	row := tx.QueryRow(ctx, `INSERT INTO adc_agent_api_keys
		(tenant_id, name, agent_id, key_prefix, key_hash, secret_hash,
		 scope_mode, scopes, expires_at, created_by)
		VALUES ($1::uuid, $2, $3, $4, $5, $6, 'ALLOW_LIST', $7, $8, $9::uuid)
		RETURNING `+keyCols,
		k.TenantID, k.Name, k.AgentID, k.KeyPrefix, k.KeyHash, k.SecretHash, string(scopes), k.ExpiresAt, k.CreatedBy)
	created, err := scanApiKey(row)
	if isUniqueViolation(err) {
		return nil, ErrApiKeyConflict
	}
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return created, nil
}
