## Why

V2.1 M8 已打通开发者应用与发布链路，但仍存在默认部署可写入演示数据、Python Agent 鉴权边界不完整、用户降权不能即时生效、A2A 任务重启丢失及构建输入可漂移等生产阻断风险。V2.2 M9 需在不扩大中间件范围的前提下闭合运行时韧性与权限闭环，落实 SEC-01/02/08/11/13/21/25。

## What Changes

- 收紧 Compose 默认行为：默认不 seed，仅开发环境可显式执行一次性 seed；生产启动校验默认凭证与 KEK，日志禁止泄密。
- 为 Python Agent 的 LLM、Eval、A2A 非公开路由统一接入实时鉴权、开发者 scope 与服务端租户注入；公开面仅保留 health 和 Agent Card。
- 强制 A2A HITL 决策来自人类管理员会话与真实 approver，机器应用不得自批；批准后任务仅进入 `working`。
- 引入用户 `authz_version` 与逐请求当前状态、有效角色校验，使禁用、降权和过期角色在下一请求生效并 fail closed。
- 将 A2A 任务迁入 PostgreSQL，以幂等键、CAS、租户约束支持多实例与重启恢复。
- 将真实 Compose 参数 guard、不可变镜像与 GitHub Actions 引用纳入候选发布硬门禁。

## Capabilities

### New Capabilities

- `runtime-security-baseline`: 约束 seed、生产默认凭证/KEK、Python Agent 路由鉴权与 HITL 人机授权边界。
- `durable-a2a-tasks`: 定义 PostgreSQL A2A 任务、幂等、CAS、多实例恢复与租户隔离。
- `immediate-session-revocation`: 定义用户授权版本及逐请求状态、角色有效性校验。
- `immutable-build-inputs`: 定义镜像 digest、Actions 完整 SHA、最小权限与自动更新入口。

### Modified Capabilities

- `developer-application-portal`: 收紧 A2A scope，使机器应用不能提交 HITL 人类审批决策，并扩展 LLM/Eval 开发者 scope。
- `release-candidate-gate`: 将真实 Compose guard 四路径、不可变引用检查和零跳过运行时门禁纳入候选发布条件。

## Impact

影响 `ce/` 用户会话与迁移、`ce/py-agent/` 全部路由及 A2A 存储、Compose/seed/guard、Dockerfile、GitHub Actions、Dependabot、运行时测试与发布验收。新增 PostgreSQL schema，不引入 JetStream、outbox 或新运行时中间件。

## 非目标

- 不引入 NATS JetStream、outbox、分布式事件总线或 A2A 异步消息重构。
- 不新增工业协议、设备类型、产品功能或正式发布 tag。
- 不替代现有 OIDC、开发者应用凭证及 PostgreSQL 迁移框架。
