CREATE TABLE adc_oidc_identities (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    issuer     TEXT NOT NULL,
    subject    TEXT NOT NULL,
    user_id    UUID NOT NULL REFERENCES adc_users(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (issuer, subject),
    UNIQUE (user_id, issuer)
);

COMMENT ON TABLE adc_oidc_identities IS '显式预配的 OIDC issuer 与 subject 到本地管理用户映射';

CREATE TABLE adc_oidc_identity_quarantine (
    user_id       UUID PRIMARY KEY REFERENCES adc_users(id) ON DELETE CASCADE,
    issuer        TEXT,
    subject       TEXT,
    reason        TEXT NOT NULL CHECK (reason IN ('MISSING_ISSUER', 'MISSING_SUBJECT', 'DUPLICATE_IDENTITY')),
    quarantined_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

COMMENT ON TABLE adc_oidc_identity_quarantine IS '无法安全自动回填的历史 OIDC 用户；需人工确认 issuer 与 subject 后建立身份映射';

WITH legacy AS (
    SELECT id AS user_id,
           NULLIF(btrim(metadata->>'oidc_issuer'), '') AS issuer,
           NULLIF(btrim(metadata->>'oidc_subject'), '') AS subject
    FROM adc_users
    WHERE auth_source = 'OIDC'
), classified AS (
    SELECT legacy.*,
           count(*) FILTER (WHERE issuer IS NOT NULL AND subject IS NOT NULL)
               OVER (PARTITION BY issuer, subject) AS identity_count
    FROM legacy
)
INSERT INTO adc_oidc_identity_quarantine (user_id, issuer, subject, reason)
SELECT user_id, issuer, subject,
       CASE
           WHEN issuer IS NULL THEN 'MISSING_ISSUER'
           WHEN subject IS NULL THEN 'MISSING_SUBJECT'
           ELSE 'DUPLICATE_IDENTITY'
       END
FROM classified
WHERE issuer IS NULL OR subject IS NULL OR identity_count > 1;

WITH legacy AS (
    SELECT id AS user_id,
           NULLIF(btrim(metadata->>'oidc_issuer'), '') AS issuer,
           NULLIF(btrim(metadata->>'oidc_subject'), '') AS subject
    FROM adc_users
    WHERE auth_source = 'OIDC'
), importable AS (
    SELECT legacy.*,
           count(*) OVER (PARTITION BY issuer, subject) AS identity_count
    FROM legacy
    WHERE issuer IS NOT NULL AND subject IS NOT NULL
)
INSERT INTO adc_oidc_identities (issuer, subject, user_id)
SELECT issuer, subject, user_id
FROM importable
WHERE identity_count = 1;
