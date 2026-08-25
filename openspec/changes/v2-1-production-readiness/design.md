## Context

V2.0 功能已进入 CE、控制台和 Compose，但认证、数据库升级和发布制品之间尚未形成生产闭环。当前 OIDC Provider 仅解码 JWT；PostgreSQL init 脚本只在空卷执行；开发者接入仍依赖管理员手工创建凭证；中英资源缺少结构化校验。M8 涉及认证、数据、前端和交付多个信任边界，必须先统一安全与升级策略。

## Goals / Non-Goals

**Goals:**

- 建立可验证的 OIDC 信任链，认证失败时不创建用户或会话。
- 让空库和既有数据卷通过同一迁移执行器到达目标 schema，失败时应用不启动。
- 用现有租户和 RBAC 体系提供一条最小开发者自助接入路径。
- 将中英资源、版本和 Compose 专项冒烟纳入候选发布门禁。

**Non-Goals:**

- 不实现多 IdP、SAML、SCIM、MFA 编排或完整 OAuth 授权服务器。
- 不承诺在线 DDL、自动向下迁移、零停机或生产数据恢复。
- 不替代真实客户 IdP、原生双架构 Linux 和工业硬件验收。

## Decisions

### 1. 使用成熟 OIDC verifier，不自行实现 JWT 加密验证

采用 `coreos/go-oidc/v3` 完成 discovery、JWKS、签名和标准 claims 验证，并在 ADC 层追加 nonce、允许算法和端点安全策略。相比继续扩展手写 JWT，成熟库降低算法混淆和 key rotation 风险。生产仅允许 HTTPS issuer；显式开发开关可允许 loopback HTTP 测试 IdP。

### 2. 登录事务是服务端单次对象

Valkey 保存 `state`、nonce、PKCE verifier 和浏览器绑定摘要，使用 Lua compare-and-delete 原子消费；浏览器只持有 `Secure`、`HttpOnly`、`SameSite=Lax` 事务 cookie。回调不在 URL 返回会话 token，统一设置 no-store/no-referrer。

### 3. 身份以 issuer + subject 唯一映射

新增 `adc_oidc_identities`，对 `(issuer, subject)` 建唯一约束并关联本地用户。未知身份默认拒绝；M8 不自动授予管理员角色。相比 JSONB subject，专表能提供数据库一致性和安全迁移边界。

### 4. 迁移器作为独立一次性 Compose 服务

新增 Go `adc-migrate` 命令，使用 PostgreSQL advisory lock、事务和 `adc_schema_migrations` 记录版本。Compose 中 `adc-app` 依赖 migrate 成功，既有 init SQL 挂载保留为兼容入口但不再承担升级。数据库版本高于二进制支持版本时拒绝执行。

### 5. 开发者门户复用 API Key 基础设施但独立建模

新增租户级 developer applications 与 hashed credentials 表；secret 只在创建和轮换时返回一次。凭证 scope 最小为 `a2a.tasks:write` 和 `a2a.tasks:read`，停用、吊销和越权均审计。控制台复用当前布局，不新建站点。

### 6. 发布候选使用不可变版本变量

Compose 应用镜像由 `ADC_VERSION` 控制，开发默认 `dev`；本地阶段仍按用户要求执行 `--build`，候选发布则固定版本或 digest。CI 校验迁移、i18n、SDK 矩阵和三应用镜像，正式 tag 留待用户验收后创建。

### 7. Python A2A 每请求实时消费 Go 应用凭证 introspection

Go Admin API 提供只接受 `a2a.tasks:read` 或 `a2a.tasks:write` 的动态 required-scope introspection，凭证无效返回 401、scope 不足返回 403，并返回认证应用和租户标识。Python A2A 从网关透传的凭证调用内部 `ADC_ADMIN_INTERNAL_URL`，不缓存认证结果，外呼失败或响应不完整时 fail closed；任务租户和应用标识仅取认证主体，所有读取和决策按租户过滤。网关继续公开转发 A2A 与 Agent Card，不成为第二权限权威。

## Risks / Trade-offs

- [新增 OIDC 依赖带来许可证与供应链风险] → 仅引入 Apache-2.0 依赖，执行 `govulncheck`、SBOM 和许可证复核。
- [企业内网 IdP 常使用私有 CA 或 HTTP] → 生产默认 HTTPS；私有 CA 通过系统 trust store，HTTP 仅允许显式开发模式与 loopback。
- [历史数据库没有迁移版本表] → 迁移器按对象存在性校验并一次性建立基线，升级测试覆盖 `0001` 到当前版本。
- [旧应用无法读取新增 schema] → 迁移只采用 expand-only；应用回滚不执行 down migration。
- [开发者凭证扩大攻击面] → secret 只存哈希、scope 默认拒绝、租户隔离、限流和审计。
- [本机验证不能代替真实环境] → 验收报告明确区分本地通过项和线下条件项。

## Migration Plan

1. 备份当前数据库并记录关键表计数。
2. 构建包含 `adc-migrate` 的应用镜像，先在临时数据库验证空库安装。
3. 对当前持久卷运行迁移服务，确认版本和幂等性。
4. 重建无状态服务，执行主链路和 M8 专项冒烟。
5. 回滚时切回旧应用镜像并保留向前兼容 schema；仅在数据损坏时从备份恢复。

## Open Questions

- 真实客户 IdP 的 issuer、私有 CA、MFA claim 和角色映射策略需在线下验收前确认。
- OIDC/白标等能力最终 CE/EE 商业装配边界需在正式 `v0.3.0` 发布评审确认；本变更不改变仓库许可证。
