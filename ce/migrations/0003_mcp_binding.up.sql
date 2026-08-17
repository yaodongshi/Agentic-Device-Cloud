-- ============================================================================
-- Class-A native MCP binding columns (0003_mcp_binding)
-- Refs: FR-021 (doc/03), design/82 B5.1, doc/07 chapter 2 (seven-step
-- binding flow), design/32 3.5 (adc_devices).
--
-- 0001_init already carried the first half of the A-class columns:
--   device_class ('A'/'B'), mcp_endpoint, binding_token_hash,
--   binding_expire_at, auth_type ('oauth2_client_credentials').
-- This migration adds the binding state machine column and the OAuth
-- client / discovery state the seven-step flow persists:
--
--   binding_status   REGISTERED -> BINDING -> BOUND -> REVOKED
--                    (REVOKED -> BINDING re-pairing; see ce/internal/mcpbinding)
--   oauth_client_id / oauth_client_secret_enc
--                    platform credentials against the device-side AS;
--                    the secret is KEK-encrypted at rest (auth.EncryptSecret,
--                    NFR-004), never plaintext
--   token_endpoint / resource_identifier
--                    discovered at BOUND time: RFC 8414 token endpoint and
--                    the RFC 8707 resource parameter (audience binding)
--   bound_at / revoked_at
--                    lifecycle timestamps for the status endpoint and audit
--
-- The OAuth access token itself is intentionally NOT persisted: it lives
-- in the in-memory cache of ce/internal/mcpbinding (doc/07 2.2, NFR-004).
-- ============================================================================

ALTER TABLE adc_devices
    ADD COLUMN binding_status VARCHAR(16) NOT NULL DEFAULT 'REGISTERED'
        CHECK (binding_status IN ('REGISTERED','BINDING','BOUND','REVOKED')),
    ADD COLUMN oauth_client_id TEXT,
    ADD COLUMN oauth_client_secret_enc TEXT,
    ADD COLUMN token_endpoint TEXT,
    ADD COLUMN resource_identifier TEXT,
    ADD COLUMN bound_at TIMESTAMPTZ,
    ADD COLUMN revoked_at TIMESTAMPTZ;

-- Binding lifecycle lookups are always class-A scoped; the partial
-- indexes keep the B-class majority (Legacy Bridge devices) out of them.
CREATE INDEX idx_devices_binding_status ON adc_devices (binding_status)
    WHERE device_class = 'A';
-- Pairing-token timeout scanner index (24h TTL, doc/07 step 4).
CREATE INDEX idx_devices_binding_expire ON adc_devices (binding_expire_at)
    WHERE binding_status = 'BINDING';

COMMENT ON COLUMN adc_devices.binding_status IS
  'A 类 MCP 绑定状态机（FR-021）：REGISTERED 已登记端点 / BINDING 配对中（令牌待消费）/ BOUND 已绑定 / REVOKED 已吊销。REVOKED 可重新发起配对';
COMMENT ON COLUMN adc_devices.oauth_client_id IS
  '平台作为 OAuth 2.1 机密客户端向设备侧授权服务器注册的 client_id（doc/07 七步流程第 3 步）';
COMMENT ON COLUMN adc_devices.oauth_client_secret_enc IS
  '平台 OAuth client_secret 的 KEK 加密形态（auth.EncryptSecret，NFR-004），任何接口不得回读明文';
COMMENT ON COLUMN adc_devices.token_endpoint IS
  '绑定完成时经 RFC 8414 发现的设备授权服务器 token_endpoint（doc/07 第 2 步）';
COMMENT ON COLUMN adc_devices.resource_identifier IS
  'RFC 8707 resource 参数（受众标识，绑定设备 MCP 端点），每次令牌请求强制携带（doc/07 第 3 步）';
COMMENT ON COLUMN adc_devices.bound_at IS
  '配对令牌一次性消费、OAuth 绑定成功的时间';
COMMENT ON COLUMN adc_devices.revoked_at IS
  '平台侧解绑时间（doc/07 第 7 步）；重新配对时清空';
