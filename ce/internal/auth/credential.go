// Package auth implements the SEC-03 device handshake authentication:
// HMAC-SHA256 signature over (deviceCode, timestamp, nonce), a +/- 300 second
// clock-skew window, and one-time nonce consumption in Valkey.
// Design baseline: LLD 3.1.2 / 3.1.3 (interface I1 DeviceAuthenticator).
package auth

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// AuthType values mirror the adc_devices.auth_type CHECK constraint
// (design/32 3.5): token / hmac / mtls / oauth2_client_credentials.
const (
	AuthTypeToken   = "token"
	AuthTypeHMAC    = "hmac"
	AuthTypeMTLS    = "mtls"
	AuthTypeOAuthCC = "oauth2_client_credentials"
)

// DeviceCredential is the credential record of one device, loaded from
// adc_devices by device_code. The tenant binding is taken from the record,
// never from the request (SEC-02).
//
// SecretHash holds the verification material: for hmac devices it is the
// decrypted signing key (in-memory only, never logged, never re-persisted);
// for token/mtls devices it is the one-way hash from credential_hash.
type DeviceCredential struct {
	DeviceID   string // UUID, adc_devices.id
	DeviceCode string // global unique business code (FR-010/FR-011)
	TenantID   string // UUID, adc_devices.tenant_id
	SecretHash string // see type doc above
	AuthType   string // adc_devices.auth_type
	Enabled    bool   // false when status is FROZEN or RETIRED
}

// CredentialRepo loads a device credential by device code (SEC-03).
// Implementations must map "no row" to ErrCredentialNotFound so the
// verifier never leaks which device codes exist.
type CredentialRepo interface {
	Load(ctx context.Context, deviceCode string) (*DeviceCredential, error)
}

// pgCredentialRepo reads adc_devices with pgx parameterized queries.
// hmac signing keys are stored KEK-encrypted (LLD 3.1.3) and decrypted in
// memory at load time; a missing or wrong KEK fails closed.
type pgCredentialRepo struct {
	pool *pgxpool.Pool
	kek  []byte // ADC_DEVICE_KEK, derived to a 32-byte AES key by LoadKEK
}

// NewCredentialRepo builds the PostgreSQL-backed CredentialRepo.
// kek may be nil only for deployments that never serve hmac devices; any
// hmac load then fails closed with a decrypt error.
func NewCredentialRepo(pool *pgxpool.Pool, kek []byte) CredentialRepo {
	return &pgCredentialRepo{pool: pool, kek: kek}
}

func (r *pgCredentialRepo) Load(ctx context.Context, deviceCode string) (*DeviceCredential, error) {
	const q = `SELECT id, tenant_id, auth_type, credential_hash, status
	           FROM adc_devices
	           WHERE device_code = $1 AND deleted_at IS NULL`

	var (
		id, tenantID, authType, credentialHash, status string
	)
	err := r.pool.QueryRow(ctx, q, deviceCode).Scan(&id, &tenantID, &authType, &credentialHash, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: device_code %q", ErrCredentialNotFound, deviceCode)
	}
	if err != nil {
		return nil, fmt.Errorf("auth: load credential for device_code %q: %w", deviceCode, err)
	}

	cred := &DeviceCredential{
		DeviceID:   id,
		DeviceCode: deviceCode,
		TenantID:   tenantID,
		AuthType:   authType,
		Enabled:    status != "FROZEN" && status != "RETIRED",
	}
	if authType == AuthTypeHMAC {
		secret, err := DecryptSecret(credentialHash, r.kek)
		if err != nil {
			// Fail closed: an undecryptable hmac key must never pass.
			return nil, fmt.Errorf("auth: decrypt hmac key for device_code %q: %w", deviceCode, err)
		}
		cred.SecretHash = secret
	} else {
		cred.SecretHash = credentialHash
	}
	return cred, nil
}
