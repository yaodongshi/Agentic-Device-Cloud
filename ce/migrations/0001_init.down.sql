-- ============================================================================
-- ADC 初始库结构回滚（0001_init.down）
-- 按外键依赖逆序删除全部 13 张表；子表在前、父表在后。
-- 说明：
--   1. adc_audit_logs 为分区表，删除父表即连带删除全部分区（含 DEFAULT
--      兜底分区与按月分区），无需逐分区 DROP
--   2. design/32 7.2 迁移纪律：生产环境禁止执行 down 迁移，本文件仅供
--      开发环境回滚验证
-- ============================================================================

DROP TABLE IF EXISTS adc_notifications;
DROP TABLE IF EXISTS adc_usage_events;
DROP TABLE IF EXISTS adc_audit_logs;
DROP TABLE IF EXISTS adc_approval_steps;
DROP TABLE IF EXISTS adc_approval_tickets;
DROP TABLE IF EXISTS adc_agent_api_keys;
DROP TABLE IF EXISTS adc_device_tools;
DROP TABLE IF EXISTS adc_devices;
DROP TABLE IF EXISTS adc_device_groups;
DROP TABLE IF EXISTS adc_user_roles;
DROP TABLE IF EXISTS adc_roles;
DROP TABLE IF EXISTS adc_users;
DROP TABLE IF EXISTS adc_tenants;
