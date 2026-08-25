## Why

V2.0 M5-M7 已形成适配器、工具市场、OIDC、白标和预算能力，但现有 OIDC 未验证令牌签名与关键声明，既有 PostgreSQL 数据卷也不会自动执行新增迁移，当前状态不满足生产试点和 `v0.3.0` 发布门禁。需要以 V2.1 M8 收敛安全、升级、开发者自助和中英发布链路，再进入验收。

## What Changes

- 完成 OIDC 生产安全闭环：JWKS 验签，`iss`/`aud`/`exp`/`nonce` 校验，PKCE S256，浏览器绑定且原子消费的登录事务，安全默认开户策略和回调防泄漏。
- 引入内置数据库迁移执行器和 Compose 单实例迁移服务，支持空库安装、既有卷升级、并发锁、版本检查和失败阻断。
- 增加最小开发者门户：租户应用注册、一次性凭证、轮换/吊销、A2A 接入信息和只读连通性测试。
- 增加中英语言包 key、占位符和术语一致性门禁，并补充翻译贡献流程。
- 完善 `v0.3.0` 发布候选门禁、迁移与 V2.0 专项冒烟、不可变镜像版本和升级说明。
- 修订 V2.0 计划的完成状态和 V2.1 M8 验收范围，不改变三仓许可证战略基线。

## Capabilities

### New Capabilities

- `secure-oidc-login`: 管理端安全 OIDC Authorization Code + PKCE 登录和本地身份映射。
- `database-migration-lifecycle`: 数据库版本化迁移、并发互斥、启动阻断和升级验证。
- `developer-application-portal`: 租户开发者应用、凭证生命周期和 A2A 自助接入。
- `translation-quality-gate`: 中英资源与文档翻译的一致性和发布门禁。
- `release-candidate-gate`: `v0.3.0` 候选制品、Compose 升级和专项冒烟准入。

### Modified Capabilities

无。当前 `openspec/specs/` 尚无已沉淀能力规范。

## Impact

- 后端：`ce/internal/adminauth/`、`ce/internal/adminapi/`、`ce/cmd/`、数据库迁移。
- 前端：登录状态恢复、开发者门户、i18n 资源和路由。
- 交付：`deploy/compose.yaml`、Dockerfile、迁移和冒烟脚本、GitHub Actions、CHANGELOG 与部署文档。
- 安全：关闭当前未验签 OIDC 路径，修复认证绕过、登录 CSRF、SSRF 和默认高权限风险，关联 SEC-03、SEC-06、SEC-09、SEC-12。

## 非目标

- 不做真实客户 IdP、FANUC/Siemens/华中数控或 ESP32 硬件联调。
- 不做 SAML、SCIM、多 IdP、完整 OAuth 授权服务器、开发者结算或社区系统。
- 不承诺正式等保测评、生产 SLA、零停机升级和双架构原生机验收。
- 不创建正式 Git tag 或 GitHub Release；本阶段交付 `v0.3.0` 候选能力，待用户验收后发布。
