-- ============================================================================
-- Tool package marketplace (0004_tool_packages)
-- Refs: design/83 C3.1/C3.2, design/82 C3 工具市场与开发者平台
--
-- adc_tool_packages stores published tool bundles: a named/versioned set
-- of MCP tools (standard MCPTool JSON, core-sdk/protocol) plus a SHA-256
-- signature over the canonical tools JSON for tamper detection
-- (ce/internal/adminapi/market.go verifies it on every read).
--
-- Visibility rules (design/83 C3.1):
--   tenant_id NULL      平台级工具包：全平台租户可见（platform_admin 发布）
--   tenant_id NOT NULL  租户私有工具包：仅发布租户可见（tenant_admin 发布）
-- 唯一性：平台级 (name, version) 全局唯一；租户级按 (tenant_id, name,
-- version) 唯一，租户包可与平台包同名（租户影子包）。
--
-- Per-tenant install state lives in adc_tool_package_installs; the
-- installed_at column on the package row is a denormalized "most recent
-- install anywhere" watermark for operations views (installs table is
-- authoritative, design/83 C3.1 去重幂等安装）。
-- ============================================================================

CREATE TABLE adc_tool_packages (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id    UUID REFERENCES adc_tenants(id),            -- NULL = 平台级全局可见
    name         VARCHAR(128) NOT NULL,
    version      VARCHAR(32)  NOT NULL,
    description  TEXT NOT NULL DEFAULT '',
    author       VARCHAR(128) NOT NULL,
    tools        JSONB NOT NULL,                             -- protocol.MCPTool 数组（规范 JSON）
    signature    TEXT NOT NULL,                              -- SHA-256(tools 规范 JSON) hex
    published_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    installed_at TIMESTAMPTZ,                                -- 冗余最近安装水印，权威见 installs 表
    status       VARCHAR(16) NOT NULL DEFAULT 'PUBLISHED'
                 CHECK (status IN ('PUBLISHED','DEPRECATED'))
);

CREATE UNIQUE INDEX uq_tool_packages_name_version
    ON adc_tool_packages (name, version) WHERE tenant_id IS NULL;
CREATE UNIQUE INDEX uq_tool_packages_tenant_name_version
    ON adc_tool_packages (tenant_id, name, version) WHERE tenant_id IS NOT NULL;
CREATE INDEX idx_tool_packages_tenant ON adc_tool_packages (tenant_id);
CREATE INDEX idx_tool_packages_published ON adc_tool_packages (published_at DESC);

CREATE TABLE adc_tool_package_installs (
    package_id   UUID NOT NULL REFERENCES adc_tool_packages(id) ON DELETE CASCADE,
    tenant_id    UUID NOT NULL REFERENCES adc_tenants(id),
    installed_by UUID REFERENCES adc_users(id),
    installed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (package_id, tenant_id)
);
CREATE INDEX idx_tool_package_installs_tenant
    ON adc_tool_package_installs (tenant_id);

COMMENT ON TABLE adc_tool_packages IS
  '工具市场工具包（design/83 C3.1）。tenant_id 为空表示平台级全局包，非空表示租户私有包；tools 为标准 MCPTool 数组，signature 为防篡改摘要';
COMMENT ON COLUMN adc_tool_packages.tenant_id IS
  '发布租户；NULL 表示平台级全局可见（platform_admin 发布）';
COMMENT ON COLUMN adc_tool_packages.tools IS
  '标准 MCP 工具数组（core-sdk/protocol.MCPTool：name/description/inputSchema/riskLevel/schemaVersion），发布时经规范序列化后入库';
COMMENT ON COLUMN adc_tool_packages.signature IS
  'SHA-256(tools 规范 JSON) 十六进制摘要；读取时重算比对，不一致拒绝服务（防篡改 seam）';
COMMENT ON COLUMN adc_tool_packages.installed_at IS
  '冗余：最近一次被任意租户安装的时间水印（运维视图）；逐租户安装状态以 adc_tool_package_installs 为准';
COMMENT ON TABLE adc_tool_package_installs IS
  '逐租户工具包安装记录（design/83 C3.1 去重幂等安装：主键 (package_id, tenant_id)，重复安装为 no-op）';
