-- ============================================================================
-- Billing statements migration (0002_billing)
-- Refs: design/82 B3 (FR-016 billing go-live), doc/04 pricing (three-tier
-- formula), FR-016 acceptance (monthly bill reconciles with the audit
-- trail).
--
-- billing_statements holds one row per (tenant, month). All monetary
-- amounts are integer fen (1 yuan = 100 fen) so billing arithmetic never
-- touches floating point. Statement generation is idempotent via the
-- (tenant_id, period_year, period_month) unique constraint; the Go
-- service (ce/internal/billing) treats a duplicate as "return the
-- existing statement".
--
-- Status machine (design/82 B3.3):
--   GENERATED -> PAID    (payment ledger integration, out of scope here)
--   GENERATED -> OVERDUE (overdue marker; the gateway quota soft-limit
--   seam reads this state via ce/internal/billing Service.Overdue — the
--   degradation logic itself is deliberately not implemented yet)
-- ============================================================================

CREATE TABLE billing_statements (
    id                   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id            UUID NOT NULL REFERENCES adc_tenants(id),
    period_year          SMALLINT NOT NULL,
    period_month         SMALLINT NOT NULL CHECK (period_month BETWEEN 1 AND 12),
    -- usage aggregates over adc_usage_events (FR-016 three metrics)
    device_peak          INTEGER NOT NULL DEFAULT 0,      -- month's max daily-peak device count (doc/04 7.2)
    tool_calls           BIGINT  NOT NULL DEFAULT 0,      -- summed TOOL_CALL events (metered, not priced)
    tokens               BIGINT  NOT NULL DEFAULT 0,      -- summed TOKEN_USAGE events
    -- price lines in fen
    subscription_fee_fen BIGINT  NOT NULL DEFAULT 0,      -- 39800 yuan/year amortized monthly
    device_fee_fen       BIGINT  NOT NULL DEFAULT 0,      -- ladder 400/300/250 yuan/device/year, amortized monthly
    token_fee_fen        BIGINT  NOT NULL DEFAULT 0,      -- 0.10 yuan per 1000 tokens
    total_fee_fen        BIGINT  NOT NULL DEFAULT 0,
    status               VARCHAR(16) NOT NULL DEFAULT 'GENERATED'
                         CHECK (status IN ('GENERATED','PAID','OVERDUE')),
    paid_at              TIMESTAMPTZ,
    breakdown            JSONB NOT NULL DEFAULT '{}',     -- device ladder tier split (DeviceBreakdown)
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, period_year, period_month)
);

CREATE INDEX idx_billing_tenant_created ON billing_statements (tenant_id, created_at DESC);
CREATE INDEX idx_billing_overdue ON billing_statements (status) WHERE status = 'OVERDUE';

COMMENT ON TABLE billing_statements IS
  'Monthly billing statements (FR-016, design/82 B3). Money stored as integer fen; generation is idempotent per (tenant, month).';
COMMENT ON COLUMN billing_statements.status IS
  'GENERATED initial / PAID settled / OVERDUE unpaid past due — read by the gateway quota soft-limit seam (B3.3; degradation not implemented).';
