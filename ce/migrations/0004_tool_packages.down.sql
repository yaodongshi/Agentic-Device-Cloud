-- ============================================================================
-- Tool package marketplace rollback (0004_tool_packages.down)
-- 说明：design/32 7.2 迁移纪律——生产环境禁止执行 down 迁移，本文件仅供
-- 开发环境回滚验证。
-- ============================================================================

DROP TABLE IF EXISTS adc_tool_package_installs;
DROP TABLE IF EXISTS adc_tool_packages;
