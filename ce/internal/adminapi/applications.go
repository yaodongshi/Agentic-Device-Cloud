package adminapi

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
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
	"adc.dev/ce/internal/httpx"
)

const (
	ApplicationScopeTasksRead  = "a2a.tasks:read"
	ApplicationScopeTasksWrite = "a2a.tasks:write"
	applicationStatusActive    = "ACTIVE"
	applicationStatusDisabled  = "DISABLED"
)

var (
	ErrApplicationNotFound = errors.New("adminapi: developer application not found")
	ErrApplicationConflict = errors.New("adminapi: developer application name exists")
	ErrCredentialInvalid   = errors.New("adminapi: invalid application credential")
	ErrScopeForbidden      = errors.New("adminapi: application scope forbidden")
	applicationNameRe      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ._-]{0,127}$`)
)

type DeveloperApplication struct {
	ID, TenantID, Name, Purpose, Status string
	Scopes                              []string
	CredentialID, CredentialPrefix      string
	CredentialRevokedAt                 *time.Time
	CreatedAt, UpdatedAt                time.Time
}

type NewDeveloperApplication struct {
	TenantID, Name, Purpose, CreatedBy string
	Scopes                             []string
	CredentialID, SecretHash, Prefix   string
}

type NewApplicationCredential struct {
	ID, SecretHash, Prefix, CreatedBy string
}

type ApplicationPrincipal struct {
	ApplicationID, TenantID string
	Scopes                  []string
}

type DeveloperApplicationRepo interface {
	Create(context.Context, *NewDeveloperApplication) (*DeveloperApplication, error)
	Get(context.Context, string) (*DeveloperApplication, error)
	List(context.Context, string, Page) ([]DeveloperApplication, int, error)
	Disable(context.Context, string) (*DeveloperApplication, error)
	Delete(context.Context, string) error
	Rotate(context.Context, string, *NewApplicationCredential) (*DeveloperApplication, error)
	Revoke(context.Context, string) (*DeveloperApplication, error)
	Authenticate(context.Context, string, string) (*ApplicationPrincipal, error)
}

func hashApplicationSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

func generateApplicationCredential() (id, plaintext, hash, prefix string, err error) {
	id = newEventID()
	b := make([]byte, 32)
	if _, err = rand.Read(b); err != nil {
		return "", "", "", "", fmt.Errorf("adminapi: generate application secret: %w", err)
	}
	secret := hex.EncodeToString(b)
	prefix = "adc_app_" + strings.ReplaceAll(id[:8], "-", "")
	plaintext = prefix + "." + id + "." + secret
	return id, plaintext, hashApplicationSecret(secret), prefix, nil
}

func parseApplicationCredential(raw string) (string, string, bool) {
	parts := strings.Split(strings.TrimSpace(raw), ".")
	if len(parts) != 3 || !strings.HasPrefix(parts[0], "adc_app_") || !isValidUUID(parts[1]) || len(parts[2]) != 64 {
		return "", "", false
	}
	return parts[1], parts[2], true
}

func normalizeApplicationScopes(in []string) ([]string, bool) {
	if len(in) == 0 {
		return nil, false
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, scope := range in {
		if scope != ApplicationScopeTasksRead && scope != ApplicationScopeTasksWrite {
			return nil, false
		}
		if !seen[scope] {
			seen[scope] = true
			out = append(out, scope)
		}
	}
	return out, true
}

type applicationRequest struct {
	Name    string   `json:"name"`
	Purpose string   `json:"purpose"`
	Scopes  []string `json:"scopes"`
}

type applicationResponse struct {
	ID               string     `json:"application_id"`
	Name             string     `json:"name"`
	Purpose          string     `json:"purpose"`
	Status           string     `json:"status"`
	Scopes           []string   `json:"scopes"`
	CredentialID     string     `json:"credential_id,omitempty"`
	CredentialPrefix string     `json:"credential_prefix,omitempty"`
	CredentialStatus string     `json:"credential_status"`
	Secret           string     `json:"secret,omitempty"`
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
	RevokedAt        *time.Time `json:"revoked_at,omitempty"`
}

func newApplicationResponse(a *DeveloperApplication, secret string) applicationResponse {
	credentialStatus := "active"
	if a.CredentialID == "" || a.CredentialRevokedAt != nil {
		credentialStatus = "revoked"
	}
	return applicationResponse{ID: a.ID, Name: a.Name, Purpose: a.Purpose,
		Status: strings.ToLower(a.Status), Scopes: a.Scopes, CredentialID: a.CredentialID,
		CredentialPrefix: a.CredentialPrefix, CredentialStatus: credentialStatus, Secret: secret,
		CreatedAt: a.CreatedAt.UTC(), UpdatedAt: a.UpdatedAt.UTC(), RevokedAt: a.CredentialRevokedAt}
}

func (s *Server) applicationRepo(w http.ResponseWriter, r *http.Request) (DeveloperApplicationRepo, bool) {
	if s.Applications == nil {
		writeError(w, r, http.StatusInternalServerError, codeInternal, "developer applications unavailable")
		return nil, false
	}
	return s.Applications, true
}

func (s *Server) handleCreateApplication(w http.ResponseWriter, r *http.Request) {
	p, _ := adminauth.FromContext(r.Context())
	tenantID, ok := s.resolveTenant(w, r, p)
	if !ok {
		return
	}
	repo, ok := s.applicationRepo(w, r)
	if !ok {
		return
	}
	var req applicationRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	req.Purpose = strings.TrimSpace(req.Purpose)
	scopes, valid := normalizeApplicationScopes(req.Scopes)
	if !applicationNameRe.MatchString(req.Name) || len(req.Purpose) > 1000 || !valid {
		writeError(w, r, http.StatusBadRequest, codeBadRequest, "invalid name, purpose, or scopes")
		return
	}
	credentialID, secret, secretHash, prefix, err := generateApplicationCredential()
	if err != nil {
		writeError(w, r, 500, codeInternal, "credential generation failed")
		return
	}
	app, err := repo.Create(r.Context(), &NewDeveloperApplication{TenantID: tenantID, Name: req.Name,
		Purpose: req.Purpose, Scopes: scopes, CreatedBy: p.UserID, CredentialID: credentialID,
		SecretHash: secretHash, Prefix: prefix})
	if errors.Is(err, ErrApplicationConflict) {
		writeError(w, r, 409, codeConflict, "application name already exists")
		return
	}
	if err != nil {
		writeError(w, r, 500, codeInternal, "create application failed")
		return
	}
	s.recordApplicationAudit(r, p, app, "developer_application.create")
	httpx.WriteJSON(w, http.StatusCreated, newApplicationResponse(app, secret))
}

func (s *Server) handleListApplications(w http.ResponseWriter, r *http.Request) {
	p, _ := adminauth.FromContext(r.Context())
	tenantID, ok := s.resolveTenant(w, r, p)
	if !ok {
		return
	}
	repo, ok := s.applicationRepo(w, r)
	if !ok {
		return
	}
	page, err := parsePage(r.URL.Query())
	if err != nil {
		writeError(w, r, 400, codeBadRequest, err.Error())
		return
	}
	apps, total, err := repo.List(r.Context(), tenantID, page)
	if err != nil {
		writeError(w, r, 500, codeInternal, "list applications failed")
		return
	}
	items := make([]applicationResponse, 0, len(apps))
	for i := range apps {
		items = append(items, newApplicationResponse(&apps[i], ""))
	}
	httpx.WriteJSON(w, 200, map[string]any{"items": items, "total": total, "page": page.Number, "page_size": page.Size})
}

func (s *Server) loadOwnedApplication(w http.ResponseWriter, r *http.Request, p *adminauth.Principal) (*DeveloperApplication, DeveloperApplicationRepo, bool) {
	id := r.PathValue("applicationID")
	if !requireUUID(w, r, "application_id", id) {
		return nil, nil, false
	}
	repo, ok := s.applicationRepo(w, r)
	if !ok {
		return nil, nil, false
	}
	app, err := repo.Get(r.Context(), id)
	if errors.Is(err, ErrApplicationNotFound) {
		writeError(w, r, 404, codeNotFound, "application not found")
		return nil, nil, false
	}
	if err != nil {
		writeError(w, r, 500, codeInternal, "load application failed")
		return nil, nil, false
	}
	if !s.checkOwnership(w, r, p, app.TenantID) {
		return nil, nil, false
	}
	return app, repo, true
}

func (s *Server) handleGetApplication(w http.ResponseWriter, r *http.Request) {
	p, _ := adminauth.FromContext(r.Context())
	app, _, ok := s.loadOwnedApplication(w, r, p)
	if ok {
		httpx.WriteJSON(w, 200, newApplicationResponse(app, ""))
	}
}

func (s *Server) mutateApplication(w http.ResponseWriter, r *http.Request, action string) {
	p, _ := adminauth.FromContext(r.Context())
	app, repo, ok := s.loadOwnedApplication(w, r, p)
	if !ok {
		return
	}
	var err error
	switch action {
	case "disable":
		app, err = repo.Disable(r.Context(), app.ID)
	case "delete":
		err = repo.Delete(r.Context(), app.ID)
	case "revoke":
		app, err = repo.Revoke(r.Context(), app.ID)
	}
	if err != nil {
		writeError(w, r, 500, codeInternal, action+" application failed")
		return
	}
	s.recordApplicationAudit(r, p, app, "developer_application."+action)
	if action == "delete" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	httpx.WriteJSON(w, 200, newApplicationResponse(app, ""))
}

func (s *Server) handleRotateApplication(w http.ResponseWriter, r *http.Request) {
	p, _ := adminauth.FromContext(r.Context())
	app, repo, ok := s.loadOwnedApplication(w, r, p)
	if !ok {
		return
	}
	credentialID, secret, hash, prefix, err := generateApplicationCredential()
	if err != nil {
		writeError(w, r, 500, codeInternal, "credential generation failed")
		return
	}
	app, err = repo.Rotate(r.Context(), app.ID, &NewApplicationCredential{ID: credentialID, SecretHash: hash, Prefix: prefix, CreatedBy: p.UserID})
	if err != nil {
		writeError(w, r, 500, codeInternal, "rotate credential failed")
		return
	}
	s.recordApplicationAudit(r, p, app, "developer_application.rotate")
	httpx.WriteJSON(w, http.StatusCreated, newApplicationResponse(app, secret))
}

func (s *Server) recordApplicationAudit(r *http.Request, p *adminauth.Principal, app *DeveloperApplication, action string) {
	s.recordAudit(r.Context(), AdminOp{EventID: newEventID(), TenantID: app.TenantID, ActorID: p.UserID,
		Action: action, Target: app.ID, Details: map[string]any{"scopes": app.Scopes}, TraceID: httpx.TraceIDFrom(r)})
}

func extractApplicationCredential(r *http.Request) string {
	if v := strings.TrimSpace(r.Header.Get("X-ADC-Application-Credential")); v != "" {
		return v
	}
	const bearer = "Bearer "
	v := strings.TrimSpace(r.Header.Get("Authorization"))
	if len(v) > len(bearer) && strings.EqualFold(v[:len(bearer)], bearer) {
		return strings.TrimSpace(v[len(bearer):])
	}
	return ""
}

type applicationPrincipalKey struct{}

type applicationIntrospectionRequest struct {
	RequiredScope string `json:"required_scope"`
}

func ApplicationPrincipalFromContext(ctx context.Context) (*ApplicationPrincipal, bool) {
	p, ok := ctx.Value(applicationPrincipalKey{}).(*ApplicationPrincipal)
	return p, ok
}

// RequireApplicationScope is a testable credential-auth seam. The repo is
// consulted on every request, so disable, rotate, and revoke take effect immediately.
func RequireApplicationScope(repo DeveloperApplicationRepo, scope string, audit AuditSink) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if repo == nil {
				httpx.WriteError(w, 500, codeInternal, "credential service unavailable", httpx.TraceIDFrom(r))
				return
			}
			p, err := repo.Authenticate(r.Context(), extractApplicationCredential(r), scope)
			if err != nil {
				status, code := http.StatusUnauthorized, codeUnauthorized
				if errors.Is(err, ErrScopeForbidden) {
					status, code = http.StatusForbidden, codeForbidden
				}
				if audit != nil && p != nil {
					_ = audit.Record(r.Context(), AdminOp{EventID: newEventID(), TenantID: p.TenantID, ActorID: p.ApplicationID,
						Action: "developer_application.scope_denied", Target: p.ApplicationID,
						Details: map[string]any{"required_scope": scope}, TraceID: httpx.TraceIDFrom(r), CreatedAt: time.Now()})
				}
				httpx.WriteError(w, status, code, http.StatusText(status), httpx.TraceIDFrom(r))
				return
			}
			ctx := context.WithValue(r.Context(), applicationPrincipalKey{}, p)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func (s *Server) authenticateApplication(w http.ResponseWriter, r *http.Request, scope string) (*ApplicationPrincipal, bool) {
	if s.Applications == nil {
		httpx.WriteError(w, 500, codeInternal, "credential service unavailable", httpx.TraceIDFrom(r))
		return nil, false
	}
	p, err := s.Applications.Authenticate(r.Context(), extractApplicationCredential(r), scope)
	if err == nil {
		return p, true
	}
	status, code := http.StatusUnauthorized, codeUnauthorized
	if errors.Is(err, ErrScopeForbidden) {
		status, code = http.StatusForbidden, codeForbidden
		if s.Audit != nil && p != nil {
			_ = s.Audit.Record(r.Context(), AdminOp{EventID: newEventID(), TenantID: p.TenantID, ActorID: p.ApplicationID,
				Action: "developer_application.scope_denied", Target: p.ApplicationID,
				Details: map[string]any{"required_scope": scope}, TraceID: httpx.TraceIDFrom(r), CreatedAt: time.Now()})
		}
	}
	httpx.WriteError(w, status, code, http.StatusText(status), httpx.TraceIDFrom(r))
	return nil, false
}

func (s *Server) handleApplicationIntrospection(w http.ResponseWriter, r *http.Request) {
	var req applicationIntrospectionRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.RequiredScope != ApplicationScopeTasksRead && req.RequiredScope != ApplicationScopeTasksWrite {
		writeError(w, r, http.StatusBadRequest, codeBadRequest, "invalid required_scope")
		return
	}
	p, ok := s.authenticateApplication(w, r, req.RequiredScope)
	if !ok {
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"active": true, "application_id": p.ApplicationID, "tenant_id": p.TenantID,
		"required_scope": req.RequiredScope,
	})
}

func (s *Server) handleApplicationConnectivity(w http.ResponseWriter, r *http.Request) {
	p, _ := ApplicationPrincipalFromContext(r.Context())
	httpx.WriteJSON(w, 200, map[string]any{"ok": true, "application_id": p.ApplicationID,
		"tenant_id": p.TenantID, "scope": ApplicationScopeTasksRead,
		"mode": "credential_introspection", "a2a_protected": true})
}

const applicationCols = `a.id::text, a.tenant_id::text, a.name, a.purpose, a.status,
	a.scopes, COALESCE(c.id::text,''), COALESCE(c.secret_prefix,''), c.revoked_at,
	a.created_at, a.updated_at`

type pgDeveloperApplicationRepo struct{ pool pgxPooler }

func NewPGDeveloperApplicationRepo(pool pgxPooler) *pgDeveloperApplicationRepo {
	return &pgDeveloperApplicationRepo{pool: pool}
}

func scanDeveloperApplication(row pgx.Row) (*DeveloperApplication, error) {
	var a DeveloperApplication
	var scopes []byte
	if err := row.Scan(&a.ID, &a.TenantID, &a.Name, &a.Purpose, &a.Status, &scopes,
		&a.CredentialID, &a.CredentialPrefix, &a.CredentialRevokedAt, &a.CreatedAt, &a.UpdatedAt); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(scopes, &a.Scopes); err != nil {
		return nil, err
	}
	if a.Scopes == nil {
		a.Scopes = []string{}
	}
	return &a, nil
}

func applicationSelect(where string) string {
	return `SELECT ` + applicationCols + ` FROM adc_developer_applications a
	LEFT JOIN LATERAL (SELECT id, secret_prefix, revoked_at FROM adc_developer_application_credentials
	 WHERE application_id=a.id ORDER BY created_at DESC LIMIT 1) c ON true ` + where
}

func (r *pgDeveloperApplicationRepo) Create(ctx context.Context, n *NewDeveloperApplication) (*DeveloperApplication, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var active bool
	if err = tx.QueryRow(ctx, `SELECT status='ACTIVE' FROM adc_tenants WHERE id=$1::uuid AND deleted_at IS NULL`, n.TenantID).Scan(&active); err != nil || !active {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrTenantNotFound
		}
		if err == nil {
			return nil, ErrTenantSuspended
		}
		return nil, err
	}
	scopes, _ := json.Marshal(n.Scopes)
	var id string
	err = tx.QueryRow(ctx, `INSERT INTO adc_developer_applications (tenant_id,name,purpose,scopes,created_by)
	 VALUES ($1::uuid,$2,$3,$4::jsonb,$5::uuid) RETURNING id::text`, n.TenantID, n.Name, n.Purpose, string(scopes), n.CreatedBy).Scan(&id)
	if isUniqueViolation(err) {
		return nil, ErrApplicationConflict
	}
	if err != nil {
		return nil, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO adc_developer_application_credentials (id,application_id,secret_hash,secret_prefix,created_by)
	 VALUES ($1::uuid,$2::uuid,$3,$4,$5::uuid)`, n.CredentialID, id, n.SecretHash, n.Prefix, n.CreatedBy)
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return r.Get(ctx, id)
}

func (r *pgDeveloperApplicationRepo) Get(ctx context.Context, id string) (*DeveloperApplication, error) {
	a, err := scanDeveloperApplication(r.pool.QueryRow(ctx, applicationSelect(`WHERE a.id=$1::uuid`), id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrApplicationNotFound
	}
	return a, err
}

func (r *pgDeveloperApplicationRepo) List(ctx context.Context, tenantID string, p Page) ([]DeveloperApplication, int, error) {
	rows, err := r.pool.Query(ctx, applicationSelect(`WHERE a.tenant_id=$1::uuid ORDER BY a.created_at DESC LIMIT $2 OFFSET $3`), tenantID, p.Size, p.offset())
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []DeveloperApplication
	for rows.Next() {
		a, err := scanDeveloperApplication(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, *a)
	}
	var total int
	if err = r.pool.QueryRow(ctx, `SELECT count(*) FROM adc_developer_applications WHERE tenant_id=$1::uuid`, tenantID).Scan(&total); err != nil {
		return nil, 0, err
	}
	return out, total, rows.Err()
}

func (r *pgDeveloperApplicationRepo) Disable(ctx context.Context, id string) (*DeveloperApplication, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx, `UPDATE adc_developer_applications SET status='DISABLED',updated_at=now() WHERE id=$1::uuid`, id)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, ErrApplicationNotFound
	}
	if _, err = tx.Exec(ctx, `UPDATE adc_developer_application_credentials SET revoked_at=COALESCE(revoked_at,now()) WHERE application_id=$1::uuid`, id); err != nil {
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return r.Get(ctx, id)
}

func (r *pgDeveloperApplicationRepo) Delete(ctx context.Context, id string) error {
	var deletedID string
	err := r.pool.QueryRow(ctx, `DELETE FROM adc_developer_applications WHERE id=$1::uuid RETURNING id::text`, id).Scan(&deletedID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrApplicationNotFound
	}
	return err
}

func (r *pgDeveloperApplicationRepo) Revoke(ctx context.Context, id string) (*DeveloperApplication, error) {
	var credentialID string
	err := r.pool.QueryRow(ctx, `UPDATE adc_developer_application_credentials SET revoked_at=COALESCE(revoked_at,now())
		WHERE id=(SELECT id FROM adc_developer_application_credentials WHERE application_id=$1::uuid ORDER BY created_at DESC LIMIT 1)
		RETURNING id::text`, id).Scan(&credentialID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrApplicationNotFound
	}
	if err != nil {
		return nil, err
	}
	return r.Get(ctx, id)
}

func (r *pgDeveloperApplicationRepo) Rotate(ctx context.Context, id string, n *NewApplicationCredential) (*DeveloperApplication, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var status string
	if err = tx.QueryRow(ctx, `SELECT status FROM adc_developer_applications WHERE id=$1::uuid FOR UPDATE`, id).Scan(&status); errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrApplicationNotFound
	}
	if err != nil {
		return nil, err
	}
	if status != applicationStatusActive {
		return nil, ErrCredentialInvalid
	}
	if _, err = tx.Exec(ctx, `UPDATE adc_developer_application_credentials SET revoked_at=COALESCE(revoked_at,now()) WHERE application_id=$1::uuid`, id); err != nil {
		return nil, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO adc_developer_application_credentials(id,application_id,secret_hash,secret_prefix,created_by) VALUES($1::uuid,$2::uuid,$3,$4,$5::uuid)`, n.ID, id, n.SecretHash, n.Prefix, n.CreatedBy); err != nil {
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return r.Get(ctx, id)
}

func (r *pgDeveloperApplicationRepo) Authenticate(ctx context.Context, raw, scope string) (*ApplicationPrincipal, error) {
	id, secret, ok := parseApplicationCredential(raw)
	if !ok {
		return nil, ErrCredentialInvalid
	}
	var appID, tenantID, status, stored string
	var scopesRaw []byte
	var revoked *time.Time
	err := r.pool.QueryRow(ctx, `SELECT a.id::text,a.tenant_id::text,a.status,a.scopes,c.secret_hash,c.revoked_at FROM adc_developer_application_credentials c JOIN adc_developer_applications a ON a.id=c.application_id JOIN adc_tenants t ON t.id=a.tenant_id WHERE c.id=$1::uuid AND t.status='ACTIVE' AND t.deleted_at IS NULL`, id).Scan(&appID, &tenantID, &status, &scopesRaw, &stored, &revoked)
	if err != nil || status != applicationStatusActive || revoked != nil {
		return nil, ErrCredentialInvalid
	}
	actual := hashApplicationSecret(secret)
	if subtle.ConstantTimeCompare([]byte(actual), []byte(stored)) != 1 {
		return nil, ErrCredentialInvalid
	}
	var scopes []string
	if json.Unmarshal(scopesRaw, &scopes) != nil {
		return nil, ErrCredentialInvalid
	}
	p := &ApplicationPrincipal{ApplicationID: appID, TenantID: tenantID, Scopes: scopes}
	for _, v := range scopes {
		if v == scope {
			var touched string
			_ = r.pool.QueryRow(ctx, `UPDATE adc_developer_application_credentials SET last_used_at=now() WHERE id=$1::uuid RETURNING id::text`, id).Scan(&touched)
			return p, nil
		}
	}
	return p, ErrScopeForbidden
}
