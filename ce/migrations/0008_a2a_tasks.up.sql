-- Durable A2A task state and human decision audit (V2.2 M9).

ALTER TABLE adc_developer_applications
    ADD CONSTRAINT uq_developer_applications_id_tenant UNIQUE (id, tenant_id);

CREATE TABLE adc_a2a_tasks (
    task_id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id           UUID NOT NULL REFERENCES adc_tenants(id),
    application_id      UUID NOT NULL,
    idempotency_key     VARCHAR(128) NOT NULL
                        CHECK (idempotency_key ~ '^[A-Za-z0-9._:-]{1,128}$'),
    request_hash        CHAR(64) NOT NULL
                        CHECK (request_hash ~ '^[0-9a-f]{64}$'),
    state               VARCHAR(32) NOT NULL
                        CHECK (state IN ('submitted','working','input-required','completed','failed','rejected')),
    task_type           VARCHAR(64) NOT NULL,
    goal                TEXT NOT NULL,
    devices             JSONB NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(devices) = 'array'),
    steps               JSONB NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(steps) = 'array'),
    message             TEXT NOT NULL DEFAULT '',
    version             BIGINT NOT NULL DEFAULT 1 CHECK (version > 0),
    approver_user_id    UUID REFERENCES adc_users(id) ON DELETE SET NULL,
    approver_identity   TEXT,
    decision_reason     TEXT,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    decided_at          TIMESTAMPTZ,
    CONSTRAINT fk_a2a_tasks_application_tenant
        FOREIGN KEY (application_id, tenant_id)
        REFERENCES adc_developer_applications (id, tenant_id),
    CONSTRAINT uq_a2a_tasks_tenant_application_idempotency
        UNIQUE (tenant_id, application_id, idempotency_key)
);

CREATE INDEX idx_a2a_tasks_tenant_created
    ON adc_a2a_tasks (tenant_id, created_at DESC, task_id DESC);
CREATE INDEX idx_a2a_tasks_tenant_state_updated
    ON adc_a2a_tasks (tenant_id, state, updated_at DESC);
