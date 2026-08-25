-- Developer applications and one-shot credentials (V2.1 M8).
-- 0005 is reserved for the OIDC identity migration.

CREATE TABLE adc_developer_applications (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   UUID NOT NULL REFERENCES adc_tenants(id),
    name        VARCHAR(128) NOT NULL,
    purpose     TEXT NOT NULL DEFAULT '',
    status      VARCHAR(16) NOT NULL DEFAULT 'ACTIVE'
                CHECK (status IN ('ACTIVE', 'DISABLED')),
    scopes      JSONB NOT NULL DEFAULT '[]',
    created_by  UUID REFERENCES adc_users(id),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX uq_developer_applications_tenant_name
    ON adc_developer_applications (tenant_id, lower(name));
CREATE INDEX idx_developer_applications_tenant
    ON adc_developer_applications (tenant_id, created_at DESC);

CREATE TABLE adc_developer_application_credentials (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    application_id UUID NOT NULL REFERENCES adc_developer_applications(id) ON DELETE CASCADE,
    secret_hash  CHAR(64) NOT NULL,
    secret_prefix VARCHAR(24) NOT NULL,
    revoked_at   TIMESTAMPTZ,
    created_by   UUID REFERENCES adc_users(id),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at TIMESTAMPTZ
);

CREATE UNIQUE INDEX uq_developer_credentials_secret_hash
    ON adc_developer_application_credentials (secret_hash);
CREATE INDEX idx_developer_credentials_application
    ON adc_developer_application_credentials (application_id, created_at DESC);

COMMENT ON COLUMN adc_developer_application_credentials.secret_hash IS
  'SHA-256 application secret; plaintext is returned only by create/rotate responses';
