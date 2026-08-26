package adminauth

// PGUserStore implements the UserStore seam on PostgreSQL (design/80
// B-01). It is the production twin of the inline store the integration
// package used before this file existed: it loads the bcrypt hash and
// the role codes from adc_roles / adc_user_roles.
//
// Role codes are normalized to lowercase on load: adc_roles.role_code
// may carry the design/32 uppercase spelling (PLATFORM_ADMIN,
// TENANT_ADMIN, ...) while the RBAC matrix in rbac.go matches the
// design/33 3.1.18 lowercase names. Normalizing here keeps both
// spellings working and the wire form (login response role, session
// roles) consistently lowercase.

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// poolQueryer is the minimal pgx pool surface PGUserStore uses.
// *pgxpool.Pool satisfies it in production; unit tests inject pgxmock.
type poolQueryer interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// PGUserStore is the PostgreSQL-backed adminauth.UserStore.
type PGUserStore struct {
	pool poolQueryer
}

// NewPGUserStore builds a PG user store over an existing pgx pool.
func NewPGUserStore(pool poolQueryer) *PGUserStore {
	return &PGUserStore{pool: pool}
}

// GetByUsername loads one user row plus its role codes. password_hash is
// COALESCE'd so SSO-style users (NULL hash) fall onto the uniform
// invalid-credential path (bcrypt compare against the empty hash fails)
// instead of surfacing a scan error as a 500.
func (s *PGUserStore) GetByUsername(ctx context.Context, username string) (*User, error) {
	var u User
	err := s.pool.QueryRow(ctx, `
		SELECT u.id::text, u.username, COALESCE(u.password_hash, ''), u.tenant_id::text,
		       u.status, t.status, u.authz_version,
		       COALESCE(array_agg(lower(r.role_code) ORDER BY r.role_code)
		           FILTER (WHERE r.id IS NOT NULL), ARRAY[]::text[])
		  FROM adc_users u
		  JOIN adc_tenants t ON t.id = u.tenant_id
		  LEFT JOIN adc_user_roles ur ON ur.user_id = u.id
		       AND (ur.expires_at IS NULL OR ur.expires_at > now())
		  LEFT JOIN adc_roles r ON r.id = ur.role_id
		 WHERE u.username = $1 AND u.deleted_at IS NULL AND t.deleted_at IS NULL
		 GROUP BY u.id, t.status
		HAVING bool_and(r.id IS NULL OR (ur.tenant_id = u.tenant_id AND
		       ((r.scope = 'PLATFORM' AND r.tenant_id IS NULL) OR
		        (r.scope = 'TENANT' AND r.tenant_id = u.tenant_id))))`, username).
		Scan(&u.ID, &u.Username, &u.PasswordHash, &u.TenantID, &u.Status, &u.TenantStatus, &u.AuthzVersion, &u.Roles)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrUserNotFound
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// GetByOIDCIdentity resolves only explicitly provisioned issuer + subject
// mappings. Unknown identities are denied by the handler and never create users.
func (s *PGUserStore) GetByOIDCIdentity(ctx context.Context, issuer, subject string) (*User, error) {
	var u User
	err := s.pool.QueryRow(ctx, `
		SELECT u.id::text, u.username, COALESCE(u.password_hash, ''), u.tenant_id::text,
		       u.status, t.status, u.authz_version,
		       COALESCE(array_agg(lower(r.role_code) ORDER BY r.role_code)
		           FILTER (WHERE r.id IS NOT NULL), ARRAY[]::text[])
		  FROM adc_oidc_identities i
		  JOIN adc_users u ON u.id = i.user_id
		  JOIN adc_tenants t ON t.id = u.tenant_id
		  LEFT JOIN adc_user_roles ur ON ur.user_id = u.id
		       AND (ur.expires_at IS NULL OR ur.expires_at > now())
		  LEFT JOIN adc_roles r ON r.id = ur.role_id
		 WHERE i.issuer = $1 AND i.subject = $2
		   AND u.auth_source = 'OIDC' AND u.deleted_at IS NULL AND t.deleted_at IS NULL
		 GROUP BY u.id, t.status
		HAVING bool_and(r.id IS NULL OR (ur.tenant_id = u.tenant_id AND
		       ((r.scope = 'PLATFORM' AND r.tenant_id IS NULL) OR
		        (r.scope = 'TENANT' AND r.tenant_id = u.tenant_id))))`, issuer, subject).
		Scan(&u.ID, &u.Username, &u.PasswordHash, &u.TenantID, &u.Status, &u.TenantStatus, &u.AuthzVersion, &u.Roles)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrUserNotFound
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// CurrentAuthorization loads the complete current authorization snapshot in
// one statement, using PostgreSQL now() as the role-expiry authority.
func (s *PGUserStore) CurrentAuthorization(ctx context.Context, userID, tenantID string) (*AuthorizationSnapshot, error) {
	var snapshot AuthorizationSnapshot
	err := s.pool.QueryRow(ctx, `
		SELECT u.status, u.deleted_at IS NOT NULL, t.status, t.deleted_at IS NOT NULL,
		       u.authz_version,
		       COALESCE(array_agg(lower(r.role_code) ORDER BY r.role_code)
		           FILTER (WHERE r.id IS NOT NULL), ARRAY[]::text[])
		  FROM adc_users u
		  JOIN adc_tenants t ON t.id = u.tenant_id
		  LEFT JOIN adc_user_roles ur ON ur.user_id = u.id
		       AND (ur.expires_at IS NULL OR ur.expires_at > now())
		  LEFT JOIN adc_roles r ON r.id = ur.role_id
		 WHERE u.id = $1::uuid AND u.tenant_id = $2::uuid
		 GROUP BY u.id, t.id
		HAVING bool_and(r.id IS NULL OR (ur.tenant_id = u.tenant_id AND
		       ((r.scope = 'PLATFORM' AND r.tenant_id IS NULL) OR
		        (r.scope = 'TENANT' AND r.tenant_id = u.tenant_id))))`, userID, tenantID).
		Scan(&snapshot.UserStatus, &snapshot.UserDeleted, &snapshot.TenantStatus,
			&snapshot.TenantDeleted, &snapshot.AuthzVersion, &snapshot.Roles)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrAuthorizationInvalid
	}
	if err != nil {
		return nil, err
	}
	return &snapshot, nil
}
