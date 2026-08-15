-- ============================================================================
-- ADC 初始库结构迁移（0001_init）
-- 依据：design/32-数据库设计说明书.md 第 3 章（13 张表完整 DDL）
-- 目标版本：PostgreSQL 16+
-- 约定说明：
--   1. UUID 主键默认值使用 gen_random_uuid()：PG 13+ 核心内置函数，
--      不依赖 pgcrypto 或 uuid-ossp 扩展（design/32 1.3）
--   2. 审计表按月 RANGE 分区 + DEFAULT 兜底分区（design/32 3.10、5.1）；
--      初始分区为"执行当月 + 未来两个月"（边界 +08 月首零点），
--      后续按月预建由 pg_cron 或 Go 后台任务续接（design/32 5.1）
--   3. 与现有代码契约的对齐修正（不改变 design/32 语义，仅补齐/纠错）：
--      - adc_agent_api_keys.secret_hash：ce/internal/agentauth/key.go 查询列
--        （design/32 3.7 未列出，按代码契约补充，与 key_hash 同为 SHA-256）
--      - idx_tickets_pending_expire 索引列由 expire_at 修正为 expires_at
--        （design/32 3.8 笔误；3.9/6.1 及 ce/internal/approval/repo.go
--         全部使用 expires_at）
-- 本迁移只建表与索引；数据库角色/应用账号（adc_app/adc_admin）与平台
-- 预置角色种子数据不在本文件（design/32 5.3、7.3）。
-- ============================================================================

-- ----------------------------------------------------------------------------
-- 3.1 adc_tenants：租户与配额台账（FR-008）
-- ----------------------------------------------------------------------------
CREATE TABLE adc_tenants (
    id                   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    code                 VARCHAR(64)  NOT NULL,             -- 租户编码，全局唯一
    name                 VARCHAR(255) NOT NULL,
    status               VARCHAR(16)  NOT NULL DEFAULT 'ACTIVE'
                         CHECK (status IN ('ACTIVE','SUSPENDED','DELETING','DELETED')),
    quota_devices        INTEGER NOT NULL DEFAULT 100,      -- 设备数配额（NFR-002 单租户上限 1 万）
    quota_calls_monthly  BIGINT  NOT NULL DEFAULT 100000,   -- 月调用量配额
    quota_concurrent     INTEGER NOT NULL DEFAULT 10,       -- 并发连接配额
    used_devices         INTEGER NOT NULL DEFAULT 0,        -- 已用设备数（冗余计数，见 6.2）
    used_calls_month     BIGINT  NOT NULL DEFAULT 0,        -- 当月已用调用量（冗余计数）
    billing_period_start DATE,                              -- 计费周期起点（V1.5 计费用）
    timezone             VARCHAR(64) NOT NULL DEFAULT 'Asia/Shanghai',
    locale               VARCHAR(16) NOT NULL DEFAULT 'zh-CN',
    metadata             JSONB NOT NULL DEFAULT '{}',
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at           TIMESTAMPTZ,
    CHECK (used_devices <= quota_devices),
    CHECK (used_calls_month <= quota_calls_monthly)
);
CREATE UNIQUE INDEX uq_tenants_code ON adc_tenants (code) WHERE deleted_at IS NULL;
CREATE INDEX idx_tenants_status ON adc_tenants (status) WHERE deleted_at IS NULL;
COMMENT ON TABLE adc_tenants IS '租户与配额台账（FR-008）。配额在线校验走 Valkey 计数，本表为权威账本与对账基准';
COMMENT ON COLUMN adc_tenants.status IS 'ACTIVE 正常 / SUSPENDED 停用（踢下线、工单失效）/ DELETING 冷静期 / DELETED 已删除';

-- ----------------------------------------------------------------------------
-- 3.2 adc_users：租户内用户（FR-008/FR-014）
-- ----------------------------------------------------------------------------
CREATE TABLE adc_users (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     UUID NOT NULL REFERENCES adc_tenants(id),
    username      VARCHAR(128) NOT NULL,
    display_name  VARCHAR(255),
    email         VARCHAR(255),
    phone         VARCHAR(32),
    password_hash TEXT,                                      -- Argon2id；SSO 用户为空
    auth_source   VARCHAR(16) NOT NULL DEFAULT 'LOCAL'
                  CHECK (auth_source IN ('LOCAL','OIDC')),
    status        VARCHAR(16) NOT NULL DEFAULT 'ACTIVE'
                  CHECK (status IN ('ACTIVE','DISABLED','LOCKED')),
    last_login_at TIMESTAMPTZ,
    metadata      JSONB NOT NULL DEFAULT '{}',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at    TIMESTAMPTZ
);
CREATE UNIQUE INDEX uq_users_tenant_username ON adc_users (tenant_id, username) WHERE deleted_at IS NULL;
CREATE INDEX idx_users_tenant ON adc_users (tenant_id) WHERE deleted_at IS NULL;
COMMENT ON TABLE adc_users IS '租户内用户（FR-008 组织成员：平台管理员/租户管理员/审批人/只读审计员/Agent 开发者）';

-- ----------------------------------------------------------------------------
-- 3.3 adc_roles / adc_user_roles：RBAC（FR-014）
-- ----------------------------------------------------------------------------
CREATE TABLE adc_roles (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   UUID REFERENCES adc_tenants(id),             -- 空表示平台级角色
    role_code   VARCHAR(64) NOT NULL,
    scope       VARCHAR(16) NOT NULL CHECK (scope IN ('PLATFORM','TENANT')),
    description TEXT,
    permissions JSONB NOT NULL DEFAULT '[]',                 -- 权限点数组，如 ["device:read","ticket:approve"]
    is_system   BOOLEAN NOT NULL DEFAULT FALSE,              -- 系统预置角色不可删
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX uq_roles_scope_code ON adc_roles (scope, COALESCE(tenant_id, '00000000-0000-0000-0000-000000000000'), role_code);
CREATE INDEX idx_roles_tenant ON adc_roles (tenant_id);
COMMENT ON TABLE adc_roles IS '角色定义（FR-014 菜单级 RBAC）。平台级：PLATFORM_ADMIN；租户级：TENANT_ADMIN/APPROVER/AUDITOR/DEVELOPER';

CREATE TABLE adc_user_roles (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     UUID NOT NULL REFERENCES adc_users(id) ON DELETE CASCADE,
    role_id     UUID NOT NULL REFERENCES adc_roles(id) ON DELETE CASCADE,
    tenant_id   UUID NOT NULL REFERENCES adc_tenants(id),    -- 冗余：约束"用户与角色同租户"
    granted_by  UUID REFERENCES adc_users(id),
    granted_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at  TIMESTAMPTZ,
    UNIQUE (user_id, role_id)
);
CREATE INDEX idx_user_roles_tenant ON adc_user_roles (tenant_id);
COMMENT ON TABLE adc_user_roles IS '用户角色授予记录，含有效期（临时审批授权用）';

-- ----------------------------------------------------------------------------
-- 3.4 adc_device_groups：设备分组（树形，FR-011）
-- ----------------------------------------------------------------------------
CREATE TABLE adc_device_groups (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   UUID NOT NULL REFERENCES adc_tenants(id),
    parent_id   UUID REFERENCES adc_device_groups(id),       -- 树形：产线/车间/厂区
    name        VARCHAR(255) NOT NULL,
    description TEXT,
    metadata    JSONB NOT NULL DEFAULT '{}',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at  TIMESTAMPTZ
);
CREATE UNIQUE INDEX uq_groups_tenant_name ON adc_device_groups (tenant_id, name) WHERE deleted_at IS NULL;
CREATE INDEX idx_groups_parent ON adc_device_groups (parent_id) WHERE deleted_at IS NULL;
COMMENT ON TABLE adc_device_groups IS '设备分组（FR-011），分组可用于工具查询、指令下发与审批策略作用域';

-- ----------------------------------------------------------------------------
-- 3.5 adc_devices：设备纳管与生命周期（FR-010/FR-002/FR-021）
-- ----------------------------------------------------------------------------
CREATE TABLE adc_devices (
    id                     UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id              UUID NOT NULL REFERENCES adc_tenants(id),
    group_id               UUID REFERENCES adc_device_groups(id) ON DELETE SET NULL,
    device_code            VARCHAR(128) NOT NULL,            -- 全局唯一、永不复用（FR-010/FR-011）
    name                   VARCHAR(255) NOT NULL,
    device_type            VARCHAR(64)  NOT NULL,            -- plc/cnc/sensor_hub/robot...
    device_class           VARCHAR(8)   NOT NULL DEFAULT 'B'
                           CHECK (device_class IN ('A','B')),-- A 原生 MCP / B Legacy Bridge（ADR-15）
    auth_type              VARCHAR(32)  NOT NULL
                           CHECK (auth_type IN ('token','hmac','mtls','oauth2_client_credentials')),
    credential_hash        TEXT NOT NULL,                    -- Argon2id 哈希；mTLS 存证书指纹；A 类存公钥指纹
    credential_salt        TEXT,
    credential_version     SMALLINT NOT NULL DEFAULT 1,      -- 重置/轮换计数（FR-010 凭证重置）
    mcp_endpoint           TEXT,                             -- A 类设备 MCP Streamable HTTP 端点（FR-021）
    binding_token_hash     TEXT,                             -- A 类 OAuth 2.1 绑定令牌哈希（FR-021）
    binding_expire_at      TIMESTAMPTZ,                      -- 绑定令牌默认 24h
    status                 VARCHAR(16) NOT NULL DEFAULT 'OFFLINE'
                           CHECK (status IN ('ONLINE','OFFLINE','ERROR','FROZEN','RETIRED')),
    sdk_version            VARCHAR(32),                      -- FR-015 版本兼容矩阵
    protocol_version       VARCHAR(32) NOT NULL DEFAULT '1.0',
    last_heartbeat_at      TIMESTAMPTZ,                      -- 权威值由 Valkey 心跳定期批量落盘
    last_seen_ip           INET,
    metadata               JSONB NOT NULL DEFAULT '{}',
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at             TIMESTAMPTZ
);
CREATE UNIQUE INDEX uq_devices_code ON adc_devices (device_code);   -- 全局唯一，含已删除（不复用）
CREATE INDEX idx_devices_tenant_status ON adc_devices (tenant_id, status) WHERE deleted_at IS NULL;
CREATE INDEX idx_devices_tenant_group ON adc_devices (tenant_id, group_id) WHERE deleted_at IS NULL;
COMMENT ON TABLE adc_devices IS '设备纳管与生命周期（FR-010），扩展 PoC 原表：凭证哈希化、状态机、分组、A/B 设备类';
COMMENT ON COLUMN adc_devices.status IS 'FROZEN 冻结即断开连接并拒绝新连接（FR-010 吊销语义）';

-- ----------------------------------------------------------------------------
-- 3.6 adc_device_tools：工具定义与风险等级（FR-003/FR-006）
-- ----------------------------------------------------------------------------
CREATE TABLE adc_device_tools (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id      UUID NOT NULL REFERENCES adc_tenants(id), -- 冗余：租户聚合查询免跨表 JOIN（见 4.1）
    device_id      UUID NOT NULL REFERENCES adc_devices(id) ON DELETE CASCADE,
    tool_name      VARCHAR(128) NOT NULL,
    display_name   VARCHAR(255),
    description    TEXT,
    input_schema   JSONB NOT NULL,                           -- 设备上报的 MCP inputSchema 原文
    output_schema  JSONB,
    annotations    JSONB NOT NULL DEFAULT '{}',              -- 平台侧扩展注解（多语言描述等，FR-023）
    risk_level     SMALLINT NOT NULL DEFAULT 2               -- 0 只读/1 低危/2 高危 HITL/3 极危物理闭环
                   CHECK (risk_level BETWEEN 0 AND 3),
    risk_changed_by UUID REFERENCES adc_users(id),           -- 等级变更人（FR-006 变更需审计）
    schema_version VARCHAR(32) NOT NULL DEFAULT '1.0',       -- 工具 schema 版本，随设备上报递增
    is_enabled     BOOLEAN NOT NULL DEFAULT TRUE,
    last_seen_at   TIMESTAMPTZ,                              -- 设备离线重连后仍保留定义
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (device_id, tool_name)
);
CREATE INDEX idx_tools_tenant_risk ON adc_device_tools (tenant_id, risk_level) WHERE is_enabled;
COMMENT ON TABLE adc_device_tools IS '设备工具定义与风险等级（FR-003/FR-006）。默认等级 2 先审后用；等级下调需审计（FR-006 异常场景）';
COMMENT ON COLUMN adc_device_tools.risk_level IS '风险判定权威来源，网关拦截读此字段（SEC-09 替代关键字硬编码）';

-- ----------------------------------------------------------------------------
-- 3.7 adc_agent_api_keys：Agent API Key（FR-009）
-- ----------------------------------------------------------------------------
CREATE TABLE adc_agent_api_keys (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id    UUID NOT NULL REFERENCES adc_tenants(id),
    name         VARCHAR(255) NOT NULL,                      -- Key 用途标识
    agent_id     VARCHAR(64),                                -- 关联 Agent 应用标识（FR-009）
    key_prefix   VARCHAR(12) NOT NULL,                       -- 展示前缀，形如 adc_3c4d5e6f（adc_ + 8 位十六进制）
    key_hash     TEXT NOT NULL,                              -- SHA-256("adc_<keyID>")，只存哈希（NFR-004）
    secret_hash  TEXT NOT NULL,                              -- 密钥部分 SHA-256；完整 Key 仅签发时返回一次
    scope_mode   VARCHAR(16) NOT NULL DEFAULT 'ALLOW_LIST'
                 CHECK (scope_mode IN ('ALLOW_LIST','DENY_LIST')),
    scopes       JSONB NOT NULL DEFAULT '[]',                -- 允许/拒绝的工具 ID 列表（FR-009 权限范围）
    expires_at   TIMESTAMPTZ,                                -- 空为永不过期（V1.5 起强制设限）
    revoked_at   TIMESTAMPTZ,
    revoke_reason TEXT,
    created_by   UUID REFERENCES adc_users(id),
    last_used_at TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX uq_api_keys_hash ON adc_agent_api_keys (key_hash);
CREATE INDEX idx_api_keys_tenant ON adc_agent_api_keys (tenant_id);
COMMENT ON TABLE adc_agent_api_keys IS 'Agent API Key（FR-009）。吊销置 revoked_at 保留审计关联；轮换支持新旧并行 24h（FR-009 异常）';
COMMENT ON COLUMN adc_agent_api_keys.secret_hash IS '密钥部分 SHA-256 哈希，列名按 ce/internal/agentauth/key.go 代码契约（design/32 3.7 未列出此列）';

-- ----------------------------------------------------------------------------
-- 3.8 adc_approval_tickets：HITL 审批工单（FR-005/ADR-05，PG 主存）
-- ----------------------------------------------------------------------------
CREATE TABLE adc_approval_tickets (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    ticket_no     BIGSERIAL UNIQUE,                          -- 人类可读单号，用于卡片与口头沟通
    tenant_id     UUID NOT NULL REFERENCES adc_tenants(id),
    agent_id      VARCHAR(64),
    api_key_id    UUID REFERENCES adc_agent_api_keys(id),    -- 审计链：Key -> 工单
    device_id     UUID NOT NULL REFERENCES adc_devices(id),
    tool_id       UUID REFERENCES adc_device_tools(id),
    tool_name     VARCHAR(128) NOT NULL,                     -- 冗余快照：工具改名不影响历史工单
    arguments     JSONB NOT NULL,                            -- 待审批调用参数
    params_hash   CHAR(64) NOT NULL,                         -- SHA-256(device_id||tool_name||arguments)，防重复工单
    risk_level    SMALLINT NOT NULL CHECK (risk_level BETWEEN 0 AND 3), -- 常规仅 2/3 进工单（FR-005）；FR-007 可配置审批流允许低等级工具强制审批
    status        VARCHAR(16) NOT NULL DEFAULT 'PENDING'
                  CHECK (status IN ('PENDING','APPROVED','REJECTED','EXPIRED')),
    approver_id   UUID REFERENCES adc_users(id),
    approver_name VARCHAR(255),                              -- 冗余快照：审批人离职后仍可追溯
    comment       TEXT,                                      -- 审批意见
    decided_at    TIMESTAMPTZ,
    expires_at    TIMESTAMPTZ NOT NULL,                      -- 默认 5 分钟（FR-005）
    callback_signature TEXT,                                 -- 回调签名参数（SEC-13）
    version       INTEGER NOT NULL DEFAULT 1,                -- 乐观锁版本
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX uq_tickets_pending_dedup ON adc_approval_tickets (device_id, tool_name, params_hash)
    WHERE status = 'PENDING';
CREATE INDEX idx_tickets_tenant_status ON adc_approval_tickets (tenant_id, status);
-- design/32 3.8 原文索引列为 expire_at，为笔误；工单列名为 expires_at，
-- 且 ce/internal/approval/repo.go 的超时扫描按 expires_at 排序，此处按契约修正
CREATE INDEX idx_tickets_pending_expire ON adc_approval_tickets (expires_at)
    WHERE status = 'PENDING';
CREATE INDEX idx_tickets_device ON adc_approval_tickets (device_id, created_at DESC);
COMMENT ON TABLE adc_approval_tickets IS 'HITL 审批工单状态机（ADR-05：PG 主存，Valkey 仅唤醒通道）。永久留存不可删除';
COMMENT ON COLUMN adc_approval_tickets.status IS 'PENDING 待审批 / APPROVED 已同意 / REJECTED 已拒绝 / EXPIRED 已过期（含设备离线失效，FR-005 异常）';

-- ----------------------------------------------------------------------------
-- 3.9 adc_approval_steps：多级审批步骤（FR-007，EE）
-- ----------------------------------------------------------------------------
CREATE TABLE adc_approval_steps (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    ticket_id     UUID NOT NULL REFERENCES adc_approval_tickets(id) ON DELETE CASCADE,
    step_no       SMALLINT NOT NULL CHECK (step_no >= 1),    -- 1=一级审批...
    approver_id   UUID REFERENCES adc_users(id),
    approver_name VARCHAR(255),
    status        VARCHAR(16) NOT NULL DEFAULT 'PENDING'
                  CHECK (status IN ('PENDING','APPROVED','REJECTED','EXPIRED','SKIPPED')),
    comment       TEXT,
    decided_at    TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (ticket_id, step_no)
);
CREATE INDEX idx_steps_approver_pending ON adc_approval_steps (approver_id) WHERE status = 'PENDING';
COMMENT ON TABLE adc_approval_steps IS '多级审批步骤（FR-007，EE）。工单级状态机仍为四态（PENDING/APPROVED/REJECTED/EXPIRED），SKIPPED 仅表示本步骤因上级决策而无需执行';

-- ----------------------------------------------------------------------------
-- 3.10 adc_audit_logs：审计日志（FR-013/SEC-07，append-only 按月分区表）
-- ----------------------------------------------------------------------------
CREATE TABLE adc_audit_logs (
    id                   UUID NOT NULL DEFAULT gen_random_uuid(),
    tenant_id            UUID NOT NULL,
    event_type           VARCHAR(32) NOT NULL,               -- tool_call / approval_decision / admin_op / auth_event / quota_event / device_event
    actor_type           VARCHAR(16) NOT NULL,               -- agent / user / device / system
    actor_id             VARCHAR(128),                       -- agent_id 或 user_id 或 device_code
    api_key_id           UUID,
    device_id            UUID,
    tool_name            VARCHAR(128),
    risk_level           SMALLINT,
    request_params       JSONB,
    response_payload     JSONB,
    response_truncated   BOOLEAN NOT NULL DEFAULT FALSE,     -- FR-013：超阈值截断并标注
    execution_duration_ms INTEGER,
    status               VARCHAR(32) NOT NULL,               -- success / failed / blocked_by_hitl（继承 PoC）
    hitl_ticket_id       UUID,
    hitl_approver        VARCHAR(255),
    hitl_comment         TEXT,                               -- 审批意见留痕（FR-013）
    exemption_basis      TEXT,                               -- 免审依据（FR-007 白名单命中）
    request_id           UUID NOT NULL,                      -- 幂等键：唯一索引防重（见 6.3）
    source_ip            INET,
    user_agent           TEXT,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (id, created_at)                             -- 分区表主键须含分区键
) PARTITION BY RANGE (created_at);

-- 兜底分区：未预建月份的数据不丢失（design/32 5.1；DEFAULT 分区行数非零即告警）
CREATE TABLE adc_audit_logs_default PARTITION OF adc_audit_logs DEFAULT;

-- 初始分区：执行当月 + 未来两个月，边界为 +08 月首零点（对齐业务日历，
-- design/32 3.10/5.1）。按月预建的续接由 pg_cron 或 Go 后台任务负责。
DO $$
DECLARE
    m_start timestamptz;
    m_end   timestamptz;
    m_name  text;
    i       integer;
BEGIN
    FOR i IN 0..2 LOOP
        m_start := (date_trunc('month', now() AT TIME ZONE 'Asia/Shanghai') AT TIME ZONE 'Asia/Shanghai')
                   + (i || ' months')::interval;
        m_end   := (date_trunc('month', now() AT TIME ZONE 'Asia/Shanghai') AT TIME ZONE 'Asia/Shanghai')
                   + ((i + 1) || ' months')::interval;
        m_name  := 'adc_audit_logs_' || to_char(m_start AT TIME ZONE 'Asia/Shanghai', 'YYYY_MM');
        EXECUTE format('CREATE TABLE %I PARTITION OF adc_audit_logs FOR VALUES FROM (%L) TO (%L)',
                       m_name, m_start, m_end);
    END LOOP;
END
$$;

CREATE UNIQUE INDEX uq_audit_request_id ON adc_audit_logs (request_id, created_at);
CREATE INDEX idx_audit_tenant_time ON adc_audit_logs (tenant_id, created_at DESC);
CREATE INDEX idx_audit_device_time ON adc_audit_logs (device_id, created_at DESC);
CREATE INDEX idx_audit_status_time ON adc_audit_logs (status, created_at DESC);
CREATE INDEX idx_audit_event_type ON adc_audit_logs (event_type, created_at DESC);
COMMENT ON TABLE adc_audit_logs IS '审计日志中心（FR-013/SEC-07）：append-only、按月分区、保留 180 天。继承并扩展原 adc_mcp_call_logs 全部字段';

-- ----------------------------------------------------------------------------
-- 3.11 adc_usage_events：计量埋点事件（FR-016）
-- ----------------------------------------------------------------------------
CREATE TABLE adc_usage_events (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       UUID NOT NULL REFERENCES adc_tenants(id),
    event_type      VARCHAR(32) NOT NULL
                    CHECK (event_type IN ('DEVICE_DAILY_PEAK','TOOL_CALL','TOKEN_USAGE')),
    source          VARCHAR(32),                             -- mcp_gateway / llm_gateway / a2a_gateway
    metric_key      VARCHAR(128),                            -- 计量对象：device_id / api_key_id / model 名
    metric_value    BIGINT NOT NULL,                         -- 数量（次数或 token 数）
    unit            VARCHAR(16),                             -- count / token
    occurred_at     TIMESTAMPTZ NOT NULL,                    -- 业务发生时间（用于按天峰值归集）
    idempotency_key UUID NOT NULL,                           -- 生产者生成，唯一约束防重复投递
    meta            JSONB NOT NULL DEFAULT '{}',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX uq_usage_idempotency ON adc_usage_events (idempotency_key);
CREATE INDEX idx_usage_tenant_type_time ON adc_usage_events (tenant_id, event_type, occurred_at);
COMMENT ON TABLE adc_usage_events IS 'FR-016 三类计量埋点事件表（V1.0 建表预留，V1.5 计费启用）。与审计日志可按 request_id 对账';

-- ----------------------------------------------------------------------------
-- 3.12 adc_notifications：通知投递记录（FR-005/FR-017，可选表）
-- ----------------------------------------------------------------------------
CREATE TABLE adc_notifications (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   UUID NOT NULL REFERENCES adc_tenants(id),
    ticket_id   UUID REFERENCES adc_approval_tickets(id),
    channel     VARCHAR(16) NOT NULL CHECK (channel IN ('WECOM','DINGTALK','EMAIL','IN_APP')),
    recipient   VARCHAR(255) NOT NULL,
    msg_type    VARCHAR(32) NOT NULL,                        -- approval_request / approval_decision / alert
    status      VARCHAR(16) NOT NULL DEFAULT 'PENDING'
                CHECK (status IN ('PENDING','SENT','FAILED','RETRY_EXHAUSTED')),
    retry_count SMALLINT NOT NULL DEFAULT 0,                 -- FR-005：卡片推送失败重试 3 次
    last_error  TEXT,
    sent_at     TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_notif_ticket ON adc_notifications (ticket_id);
CREATE INDEX idx_notif_status ON adc_notifications (status) WHERE status IN ('PENDING','FAILED');
COMMENT ON TABLE adc_notifications IS '通知投递记录（FR-005 推送重试、FR-017 渠道可用性看板）';
