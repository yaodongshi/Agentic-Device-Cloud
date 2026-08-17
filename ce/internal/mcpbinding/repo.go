// Repository seam for the class-A MCP binding aggregate (design/31 LLD
// B5.1, doc/07 chapter 2). The binding lives on the adc_devices row of a
// class-A device; PGBindingRepo performs every transition as a
// single-statement conditional UPDATE so the state machine predicates and
// the one-shot token consumption (anti-replay, doc/07 step 4) are atomic
// under concurrency. Zero-row updates are re-read and classified into the
// package sentinels, mirroring the approval.PGTicketRepo CAS technique.

package mcpbinding

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// pooler is the minimal pgx pool surface the repositories use.
// *pgxpool.Pool satisfies it in production; unit tests inject pgxmock.
type pooler interface {
	Begin(ctx context.Context) (pgx.Tx, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// BindingRepo persists the binding aggregate with CAS atomicity. All
// transitions are conditional on the current binding_status; a zero-row
// update is classified into the precise sentinel.
type BindingRepo interface {
	// RegisterEndpoint writes the MCP endpoint, the OAuth client
	// credentials (secret in KEK-encrypted form) and a fresh pairing
	// token hash, flipping REGISTERED (or REVOKED) to BINDING.
	RegisterEndpoint(ctx context.Context, deviceID, mcpEndpoint, clientID, clientSecretEnc, tokenHash string, expireAt time.Time) (*Binding, error)
	// Get loads the current binding state of a class-A device.
	Get(ctx context.Context, deviceID string) (*Binding, error)
	// ConsumeTokenAndBind consumes the pairing token (hash cleared) and
	// flips BINDING to BOUND in one conditional UPDATE. The token is
	// verified by hash match and expiry inside the predicate, so a token
	// can ever be consumed once (doc/07 step 4 anti-replay).
	ConsumeTokenAndBind(ctx context.Context, deviceID, tokenHash, tokenEndpoint, resource string, now time.Time) (*Binding, error)
	// Revoke flips REGISTERED/BINDING/BOUND to REVOKED and clears the
	// pairing token in one conditional UPDATE.
	Revoke(ctx context.Context, deviceID string, revokedAt time.Time) (*Binding, error)
}

// epochSentinel is the NULL-coalesced scan value for nullable timestamp
// columns (integration B-12 P1 fix pattern; real rows are far from 1970).
var epochSentinel = time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC)

// nonNilTime maps the NULL-coalesced epoch sentinel back to nil.
func nonNilTime(t time.Time) *time.Time {
	if t.Equal(epochSentinel) {
		return nil
	}
	return &t
}

// bindingCols rebuilds a Binding. UUID columns are cast to text (pgx
// v5.10 scan plan); nullable columns are coalesced so plain-value scan
// targets never see NULL.
const bindingCols = `id::text, tenant_id::text, device_class, COALESCE(mcp_endpoint,''), auth_type,
	binding_status, COALESCE(binding_token_hash,''), COALESCE(binding_expire_at,'1970-01-01'::timestamptz),
	COALESCE(oauth_client_id,''), COALESCE(oauth_client_secret_enc,''), COALESCE(token_endpoint,''),
	COALESCE(resource_identifier,''), COALESCE(bound_at,'1970-01-01'::timestamptz), COALESCE(revoked_at,'1970-01-01'::timestamptz),
	updated_at`

// SQL statements are package-level so the unit tests can assert the exact
// predicate text with regexp.QuoteMeta.
const (
	sqlRegisterEndpoint = `UPDATE adc_devices
		SET mcp_endpoint = $2, auth_type = 'oauth2_client_credentials',
		    binding_token_hash = $5, binding_expire_at = $6,
		    oauth_client_id = $3, oauth_client_secret_enc = $4,
		    token_endpoint = NULL, resource_identifier = NULL,
		    bound_at = NULL, revoked_at = NULL,
		    binding_status = 'BINDING', updated_at = now()
		WHERE id = $1::uuid AND device_class = 'A' AND deleted_at IS NULL
		  AND binding_status IN ('REGISTERED','REVOKED')
		RETURNING ` + bindingCols

	sqlGetBinding = `SELECT ` + bindingCols + ` FROM adc_devices
		WHERE id = $1::uuid AND device_class = 'A' AND deleted_at IS NULL`

	sqlConsumeTokenAndBind = `UPDATE adc_devices
		SET binding_status = 'BOUND', binding_token_hash = NULL, binding_expire_at = NULL,
		    token_endpoint = $4, resource_identifier = $5, bound_at = $3,
		    revoked_at = NULL, updated_at = now()
		WHERE id = $1::uuid AND binding_status = 'BINDING'
		  AND binding_token_hash = $2 AND binding_expire_at > $3
		RETURNING ` + bindingCols

	sqlRevokeBinding = `UPDATE adc_devices
		SET binding_status = 'REVOKED', binding_token_hash = NULL, binding_expire_at = NULL,
		    revoked_at = $2, updated_at = now()
		WHERE id = $1::uuid AND device_class = 'A' AND deleted_at IS NULL
		  AND binding_status IN ('REGISTERED','BINDING','BOUND')
		RETURNING ` + bindingCols

	sqlClassifyRow = `SELECT device_class, binding_status,
		COALESCE(binding_token_hash,''), COALESCE(binding_expire_at,'1970-01-01'::timestamptz)
		FROM adc_devices WHERE id = $1::uuid AND deleted_at IS NULL`
)

// PGBindingRepo is the PostgreSQL BindingRepo implementation.
type PGBindingRepo struct {
	pool pooler
}

// NewPGBindingRepo builds a binding repository over an existing pgx pool.
func NewPGBindingRepo(pool pooler) *PGBindingRepo {
	return &PGBindingRepo{pool: pool}
}

// scanBinding rebuilds a Binding from a bindingCols projection row.
func scanBinding(row pgx.Row) (*Binding, error) {
	var b Binding
	var expireAt, boundAt, revokedAt time.Time
	err := row.Scan(
		&b.DeviceID, &b.TenantID, &b.DeviceClass, &b.McpEndpoint, &b.AuthType,
		&b.Status, &b.BindingTokenHash, &expireAt,
		&b.OAuthClientID, &b.OAuthClientSecretEnc, &b.TokenEndpoint, &b.ResourceIdentifier,
		&boundAt, &revokedAt, &b.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	b.BindingExpireAt = nonNilTime(expireAt)
	b.BoundAt = nonNilTime(boundAt)
	b.RevokedAt = nonNilTime(revokedAt)
	return &b, nil
}

// classifyRow re-reads the minimal classification columns of the device
// row after a zero-row conditional UPDATE and explains why it failed.
func (r *PGBindingRepo) classifyRow(ctx context.Context, deviceID, wantHash string, now time.Time, registerPath bool) error {
	var class, status, hash string
	var expireAt time.Time
	err := r.pool.QueryRow(ctx, sqlClassifyRow, deviceID).Scan(&class, &status, &hash, &expireAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrBindingNotFound
	}
	if err != nil {
		return err
	}
	if class != "A" {
		return ErrNotClassA
	}
	if registerPath {
		// RegisterEndpoint only accepts REGISTERED/REVOKED, so a
		// zero-row update here means a concurrent transition (BINDING
		// in progress or already BOUND); any surviving row of either
		// shape is a state conflict.
		return ErrStateConflict
	}
	switch status {
	case string(StatusBinding):
		// The token predicate failed: either the hash did not match
		// (wrong or already-cleared token) or the pairing expired.
		if hash == "" || hash != wantHash {
			return ErrTokenInvalid
		}
		if !now.Before(expireAt) {
			return ErrTokenExpired
		}
		return ErrStateConflict
	case string(StatusBound):
		// BOUND means the pairing token was consumed by the winner;
		// a second attempt with the same token is a replay (doc/07 4).
		return ErrTokenInvalid
	default:
		return ErrStateConflict
	}
}

// RegisterEndpoint implements BindingRepo. Zero-row updates are explained
// by classifyRow (register path).
func (r *PGBindingRepo) RegisterEndpoint(ctx context.Context, deviceID, mcpEndpoint, clientID, clientSecretEnc, tokenHash string, expireAt time.Time) (*Binding, error) {
	row := r.pool.QueryRow(ctx, sqlRegisterEndpoint,
		deviceID, mcpEndpoint, clientID, clientSecretEnc, tokenHash, expireAt)
	b, err := scanBinding(row)
	if err == nil {
		return b, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	return nil, r.classifyRow(ctx, deviceID, "", time.Time{}, true)
}

// Get implements BindingRepo.
func (r *PGBindingRepo) Get(ctx context.Context, deviceID string) (*Binding, error) {
	row := r.pool.QueryRow(ctx, sqlGetBinding, deviceID)
	b, err := scanBinding(row)
	if errors.Is(err, pgx.ErrNoRows) {
		// Distinguish "row missing" from "row exists but class B":
		// only the class-A lookup is expected to miss here, but a B
		// device answering the same id must map to ErrNotClassA.
		return nil, r.classifyRow(ctx, deviceID, "", time.Time{}, true)
	}
	if err != nil {
		return nil, err
	}
	return b, nil
}

// ConsumeTokenAndBind implements BindingRepo. The pairing token is
// verified and cleared inside the UPDATE predicate, which is what makes
// double consumption impossible (SEC-11 CAS technique).
func (r *PGBindingRepo) ConsumeTokenAndBind(ctx context.Context, deviceID, tokenHash, tokenEndpoint, resource string, now time.Time) (*Binding, error) {
	row := r.pool.QueryRow(ctx, sqlConsumeTokenAndBind,
		deviceID, tokenHash, now, tokenEndpoint, resource)
	b, err := scanBinding(row)
	if err == nil {
		return b, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	return nil, r.classifyRow(ctx, deviceID, tokenHash, now, false)
}

// Revoke implements BindingRepo.
func (r *PGBindingRepo) Revoke(ctx context.Context, deviceID string, revokedAt time.Time) (*Binding, error) {
	row := r.pool.QueryRow(ctx, sqlRevokeBinding, deviceID, revokedAt)
	b, err := scanBinding(row)
	if err == nil {
		return b, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	return nil, r.classifyRow(ctx, deviceID, "", time.Time{}, false)
}
