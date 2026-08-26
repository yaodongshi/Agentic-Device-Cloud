## 1. OIDC 安全闭环

- [x] 1.1 引入 Apache-2.0 OIDC verifier，完成 JWKS 签名和 issuer/audience/expiry/算法校验；以伪造、过期和错误受众令牌测试验证（SEC-03、SEC-09，≤2h）
- [x] 1.2 将 OIDC state 存储升级为含 nonce、PKCE verifier 和浏览器绑定的事务，并用原子消费并发测试验证（SEC-03、SEC-06，≤2h）
- [x] 1.3 实现 PKCE S256、nonce 传递、回调 no-store/no-referrer 和 cookie 会话恢复测试（SEC-03、SEC-09，≤2h）
- [x] 1.4 校验 issuer/discovery/token/JWKS 端点与重定向策略，以 HTTP、loopback 和跨源恶意端点测试验证 fail closed（SEC-09、SEC-12，≤2h）
- [x] 1.5 新增 issuer+subject 唯一身份迁移和 PG 映射实现，验证重复身份拒绝与未知身份默认拒绝（SEC-03、≤2h）
- [x] 1.6 完成登录页 OIDC 可用性探测、SSO 跳转和 cookie 会话恢复链路测试（≤2h）

## 2. 数据库迁移生命周期

- [x] 2.1 实现内嵌 SQL 迁移器、schema_migrations 和 PostgreSQL advisory lock 单元测试（≤2h）
- [x] 2.2 新增 `adc-migrate` 命令，验证空库、已最新、版本过高和失败回滚（≤2h）
- [x] 2.3 将 Compose 改为 migrate 成功后启动应用，并以既有 ADC 卷验证 `0001` 至目标版本升级（≤2h）
- [x] 2.4 增加迁移幂等、并发和哨兵数据保留集成测试（≤2h）

## 3. 开发者应用门户

- [x] 3.1 新增开发者应用与凭证迁移，确保 secret 仅存哈希并具备租户级唯一约束（SEC-03、SEC-09，≤2h）
- [x] 3.2 实现应用列表/创建/停用/删除和凭证轮换/吊销 API，覆盖 RBAC、租户隔离与审计测试（SEC-03、≤2h）
- [x] 3.3 实现应用 scope 认证中间件，验证越权调用返回 403 且吊销立即生效（SEC-03、SEC-12，≤2h）
- [x] 3.4 实现中英开发者门户页面、一次性 secret 展示、接入信息和只读连通性测试（≤2h）
- [x] 3.5 修复 Python A2A 全路由应用凭证认证、read/write scope 与任务租户隔离，并验证匿名拒绝、403、吊销即时和跨租户不可见（SEC-03、SEC-12，≤2h）

## 4. 翻译与发布门禁

- [x] 4.1 新增中英语言 key、值类型、占位符和空值校验脚本及失败样例测试（≤2h）
- [x] 4.2 新增中英核心术语表和贡献流程，将高风险文案术语检查接入 CI（≤2h）
- [x] 4.3 修订 CHANGELOG、版本一致性检查和 `v0.3.0` 候选发布说明（≤2h）
- [x] 4.4 更新 release workflow：三镜像、SBOM、主门禁、SDK/i18n/迁移检查和不可变 `ADC_VERSION`（≤2h）
- [x] 4.5 扩展 Compose smoke 覆盖迁移、OIDC 配置状态、工具市场、预算和参数校验（≤2h）

## 5. 阶段验证与交付

- [x] 5.1 更新 design/83 的 M5-M8 完成状态、验收矩阵和线下条件项，全文扫描战略基线矛盾（≤2h）
- [x] 5.2 执行 Go race/覆盖率、Python、Console、SDK、i18n、迁移和安全门禁并修复失败（≤2h）
- [x] 5.3 备份开发数据库，重新构建并发布 Compose，验证健康状态、迁移版本、幂等重建和 smoke 零失败（≤2h）
- [x] 5.4 记录 M8 验收证据、残余风险和正式 `v0.3.0` 发布前线下条件（≤2h）
- [x] 5.5 提交全部变更并推送 `dev`，确认工作树干净且远端 commit 一致（≤2h）
