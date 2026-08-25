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
	"strings"

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
		SELECT id::text, username, COALESCE(password_hash, ''), tenant_id::text, status
		  FROM adc_users
		 WHERE username = $1 AND deleted_at IS NULL`, username).
		Scan(&u.ID, &u.Username, &u.PasswordHash, &u.TenantID, &u.Status)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrUserNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := s.loadRoles(ctx, &u); err != nil {
		return nil, err
	}
	return &u, nil
}

// GetByOIDCIdentity resolves only explicitly provisioned issuer + subject
// mappings. Unknown identities are denied by the handler and never create users.
func (s *PGUserStore) GetByOIDCIdentity(ctx context.Context, issuer, subject string) (*User, error) {
	var u User
	err := s.pool.QueryRow(ctx, `
		SELECT u.id::text, u.username, COALESCE(u.password_hash, ''), u.tenant_id::text, u.status
		  FROM adc_oidc_identities i
		  JOIN adc_users u ON u.id = i.user_id
		 WHERE i.issuer = $1 AND i.subject = $2
		   AND u.auth_source = 'OIDC' AND u.deleted_at IS NULL`, issuer, subject).
		Scan(&u.ID, &u.Username, &u.PasswordHash, &u.TenantID, &u.Status)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrUserNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := s.loadRoles(ctx, &u); err != nil {
		return nil, err
	}
	return &u, nil
}

// loadRoles fills u.Roles from adc_user_roles/adc_roles, normalizing
// role codes to lowercase (see the package comment).
func (s *PGUserStore) loadRoles(ctx context.Context, u *User) error {
	rows, err := s.pool.Query(ctx, `
		SELECT r.role_code
		  FROM adc_user_roles ur
		  JOIN adc_roles r ON r.id = ur.role_id
		 WHERE ur.user_id = $1::uuid`, u.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var role string
		if err := rows.Scan(&role); err != nil {
			return err
		}
		u.Roles = append(u.Roles, strings.ToLower(role))
	}
	return rows.Err()
}
