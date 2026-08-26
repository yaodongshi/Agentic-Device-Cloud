## Context

V2.1 M8 已实现开发者应用实时 introspection、迁移框架和候选发布链路，但 `design/84-V2.1-M8验收报告.md` 明确保留两项生产风险：Python A2A 任务仍在进程内存，服务重启即丢失；除 uv 外的 Dockerfile 与 Compose 基础镜像尚未固定 digest。现状还包括 Eval 路由无鉴权、机器应用凭证可直接调用 HITL decision、用户会话只信任签发时角色，以及默认 Compose 启动 seed。

本变更跨 Go 控制面、Python Agent 面、PostgreSQL、Compose 和 GitHub Actions。安全边界遵循 SEC-01/02/08/11/13/21/25：租户和主体只能来自可信认证结果，审批必须可归因于真实人类，状态迁移必须持久且原子，密钥不得进入日志。架构、安全、DevOps、测试专家共同约束为默认拒绝、权威状态单一、制品不可变、异常路径自动化且门禁无跳过。

## Goals / Non-Goals

**Goals:**

- 默认 Compose 不产生演示数据，生产配置在应用启动前 fail closed。
- Python Agent 仅公开 health、readiness 和 Agent Card，其余路由逐请求鉴权并按最小 scope 授权。
- 用户禁用、降权和角色过期在下一请求生效；HITL 决策只能由本租户人类管理员或审批人完成。
- A2A 任务以 PostgreSQL 为权威源，支持请求幂等、CAS、多副本和重启恢复。
- Docker/Compose/Actions 输入不可漂移，真实 Compose guard 与运行时测试成为零失败、零跳过门禁。

**Non-Goals:**

- 不引入 NATS JetStream、outbox、任务事件流、后台队列或跨地域容灾。
- 不实现设备执行完成回调、自动重试编排或新的 A2A task state。
- 不新增工业协议、设备类型、正式发布 tag 或生产部署平台。
- 不以 Valkey 缓存用户授权或 A2A 任务权威状态。

## Decisions

### 1. Compose seed 使用显式开发 profile 的一次性服务

默认 `docker compose up` 不声明对 seed 的依赖，也不启动 seed。seed 仅位于 `dev` profile，要求显式开发环境标志并作为可重复执行但正常退出的一次性服务；环境不是 `dev` 时即使显式调用也拒绝。seed 与启动 guard 共用敏感值判定规则，只输出变量名和错误类别，不输出值、哈希或可逆片段。

生产模式在迁移和业务服务启动前检查默认管理员凭证、设备/应用示例凭证和 KEK：缺失、已知默认值、占位值、格式或强度不合法均阻断。选择启动前 guard 而非运行后告警，因为后者会留下可利用窗口；选择 profile 而非第二份 Compose 文件，避免拓扑漂移。

### 2. Python Agent 使用统一认证依赖与路由级 scope

health、readiness、Agent Card 保持公开。LLM、Eval、A2A task 路由统一通过 Go introspection 每请求验证应用状态、凭证和 scope，认证服务不可用、超时或响应非法均 fail closed；租户与 application id 只从 introspection 响应注入，忽略或拒绝请求中的租户字段。

新增 `llm:invoke` 与 `evals:read/write` 开发者 scopes；A2A 保留 `a2a.tasks:read/write` 用于任务读写，但从机器应用能力中移除 decision。选择实时 introspection 而非 Python 本地 JWT 或缓存，以复用 M8 吊销即时语义并避免双份授权源。

### 3. HITL decision 分离为人类会话端点

A2A decision 由 Go 会话认证边界受理，逐请求加载当前用户、租户和有效角色，仅 `TENANT_ADMIN`、`APPROVER` 或平台管理语义允许决策，并将真实 user id/display identity 作为 approver 写入。机器 application credential 无论拥有何种 scope 均返回 403，且不得存在可授予的审批 scope。

Go 端在认证后调用共享 PostgreSQL repository 完成租户限定 CAS；Python 不信任转发头模拟人类身份。批准只将 `input-required` 原子迁移为 `working`，后续必须由真实执行结果另行推进到 `completed`；拒绝迁移为 `rejected`。这避免 SEC-01/21 的主体伪造以及当前“批准即完成”的假成功。

### 4. 用户授权版本与数据库逐请求复核

为 `adc_users` 增加非空单调递增 `authz_version`。登录成功时 session 保存 user id、tenant id 和签发时版本；每个受保护请求均查询用户当前状态、未删除状态、当前版本及 `expires_at IS NULL OR expires_at > now()` 的角色集合。用户非 ACTIVE、租户不可用、版本不匹配、角色为空、查询失败或数据矛盾均拒绝并删除当前 session。

用户禁用、角色授予/撤销/降权等授权变更必须在同一事务中递增版本。角色自然过期即使未递增版本，也因逐请求过滤在下一请求失效。选择不缓存，以严格满足“下一请求生效”；代价是每请求一次轻量数据库读取，后续只有在保持同等撤权语义时才可引入短缓存。

### 5. PostgreSQL A2A 任务是唯一权威状态

新增 `adc_a2a_tasks`，至少包含 UUID task id、tenant/application id、客户端幂等键、state、task type、goal、devices/steps JSONB、message、version、approver user/identity、decision reason、时间戳。数据库约束状态枚举，唯一约束 `(tenant_id, application_id, idempotency_key)`，索引覆盖租户列表和任务读取；幂等键为创建写请求必填且有长度/字符约束。

创建任务在事务中插入；相同主体与幂等键且规范化请求摘要一致时返回原任务，不一致时返回 409。所有状态更新使用 `WHERE tenant_id=? AND task_id=? AND state=? AND version=?` 条件更新并递增 version；影响行数为零时重读并返回 404 或 409。列表和读取 SQL 必带 tenant 条件，禁止先按 id 读取后在内存过滤。

Python 实例启动不回放任务到内存，请求始终访问 PG，因此多实例与重启天然恢复。选择直接 PG repository 而非 JetStream/outbox，是因为本阶段需要的是持久状态与并发正确性，不承诺异步事件投递；未来事件化可基于 task/version 增量演进。

### 6. Guard 与运行时门禁在真实 Compose 拓扑执行

guard 测试必须使用实际 Compose 配置和构建出的应用镜像，覆盖配置缺失、类型错误、范围越界、合法配置四条独立路径，并验证前三条非零退出、合法路径成功。主 smoke 汇总必须为 `FAIL=0 SKIP=0`；依赖不可用、环境不满足或测试未执行一律计 FAIL，不允许以单测替代运行态 skip。

运行时测试覆盖：默认不 seed、非 dev seed 拒绝、日志脱敏、生产凭证/KEK；Python 路由矩阵与吊销；用户下一请求撤权；机器自批拒绝及批准停留 working；A2A 幂等冲突、CAS 竞争、跨租户、多实例和重启恢复。

### 7. 构建输入全部不可变并保留更新通道

Dockerfile 的 `FROM`、Compose 生产镜像均使用 `name:version@sha256:digest`；版本用于可读性，digest 决定内容。GitHub Actions 的所有 `uses:` 必须固定 40 位完整提交 SHA，workflow/job 显式声明最小 `permissions`，未使用写权限不得授予。静态脚本检查 Dockerfile、Compose 和 workflow，任何 tag-only、分支或短 SHA 均失败。

Dependabot 配置 Docker 与 GitHub Actions 更新入口，更新仍须通过完整门禁后合并。选择 Dependabot 而非人工台账，使不可变不等于永不更新；不在本阶段引入新的供应链平台。

## Risks / Trade-offs

- [逐请求 PostgreSQL 校验增加控制面延迟与数据库负载] → 使用主键和有效角色索引、限定查询列并建立基线；不得以牺牲即时撤权换取缓存命中。
- [Go 人类 decision 与 Python task API 形成跨语言边界] → 共用数据库状态机契约和集成测试，只有 repository CAS 能改变审批状态。
- [JSONB payload 演进可能出现 schema 漂移] → 写入规范化结构并在应用层版本化校验，本阶段不做通用事件存储。
- [digest 和 Actions SHA 更新频繁产生维护成本] → Dependabot 小批量更新，完整测试后合并，不允许浮动引用临时绕过。
- [本地 Docker 环境差异导致 smoke 不稳定] → 固定镜像 digest、显式超时和健康条件；任何未执行测试按 FAIL 处理并保留诊断日志。
- [仅 PG 状态没有异步投递保证] → 本阶段仅承诺请求驱动的持久状态机；需要可靠事件投递时另提 JetStream/outbox 变更。

## Migration Plan

1. 对现有命名卷执行 PostgreSQL 自定义格式备份、SHA-256 校验和 `pg_restore -l` 可读性检查。
2. 增加连续 up/down 迁移：用户授权版本、A2A 任务表及约束索引；先在 V2.1 哨兵卷验证数据保留和迁移幂等。
3. 部署兼容新 schema 的应用镜像；默认 Compose 先运行 guard 和迁移，再启动服务，且不执行 seed。
4. 以同一既有卷执行 Compose rebuild/up，验证用户、开发者应用和既有业务数据保留。
5. 执行四路径 guard、runtime tests、主 smoke 和全部源码/供应链门禁，要求 `FAIL=0 SKIP=0`。
6. 生成 V2.2 M9 验收报告，记录镜像 digest、迁移账本、备份位置、测试命令和结果；提交并推送 `dev`，不创建 tag。

回滚时切回升级前已记录 digest 的应用镜像，不执行 down migration且不删除 PostgreSQL/Valkey 卷。若旧应用不能读取向前兼容 schema或数据损坏，停止写入后使用已校验备份恢复；seed 不作为恢复工具。

## Open Questions

无。实现期间若发现现有管理员会话端点无法在不新增公开接口的情况下承载 decision，应保持本设计的人类会话与 PG CAS 边界，另行评审具体路由，不得退回机器应用审批。
