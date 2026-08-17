-- ============================================================================
-- Class-A native MCP binding columns rollback (0003_mcp_binding.down)
-- 说明：design/32 7.2 迁移纪律——生产环境禁止执行 down 迁移，本文件仅供
-- 开发环境回滚验证。
-- ============================================================================

DROP INDEX IF EXISTS idx_devices_binding_status;
DROP INDEX IF EXISTS idx_devices_binding_expire;

ALTER TABLE adc_devices
    DROP COLUMN IF EXISTS binding_status,
    DROP COLUMN IF EXISTS oauth_client_id,
    DROP COLUMN IF EXISTS oauth_client_secret_enc,
    DROP COLUMN IF EXISTS token_endpoint,
    DROP COLUMN IF EXISTS resource_identifier,
    DROP COLUMN IF EXISTS bound_at,
    DROP COLUMN IF EXISTS revoked_at;
