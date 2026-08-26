## MODIFIED Requirements

### Requirement: 应用调用必须受 scope 限制
系统 MUST 默认拒绝未授权能力；A2A 任务列表和读取要求 `a2a.tasks:read`，创建任务要求 `a2a.tasks:write`，LLM 调用要求 `llm:invoke`，Eval 读取与执行分别要求 `evals:read` 与 `evals:write`。Python Agent 服务 MUST 对每次非公开请求调用 Go 应用凭证 introspection，认证依赖不可用或响应无效时 fail closed。机器应用 scope 不得授予 HITL 人类审批决策能力。

#### Scenario: 超范围调用
- **WHEN** 只拥有只读 scope 的应用提交写任务
- **THEN** 系统返回 403并记录应用、租户和拒绝原因

#### Scenario: A2A 匿名调用
- **WHEN** 调用方未携带有效应用凭证访问任一 A2A 任务路由
- **THEN** 系统返回 401，且 Agent Card 仍可匿名读取

#### Scenario: LLM 或 Eval 超范围调用
- **WHEN** 应用访问未被其 scope 授权的 LLM 或 Eval 路由
- **THEN** 系统返回 403且不执行模型调用或评测

#### Scenario: 应用尝试 HITL 决策
- **WHEN** 任一应用凭证调用 A2A HITL decision
- **THEN** 系统返回 403且不得因 `a2a.tasks:write` 或其他应用 scope 获得审批能力

### Requirement: A2A 任务必须按认证租户隔离
系统 MUST 由认证主体注入任务的 `tenant_id` 和 `application_id`，不得信任客户端提交的租户字段；列表、读取和创建 MUST 严格限制在当前认证租户。审批决策不属于应用凭证能力，必须转由当前人类管理员会话处理。

#### Scenario: 跨租户读取或创建
- **WHEN** 一个租户的应用读取另一租户的任务或提交伪造租户字段
- **THEN** 系统返回 404或仅按认证租户处理，列表不包含其他租户任务

#### Scenario: 应用跨租户决策
- **WHEN** 一个租户的应用凭证尝试决策任一任务
- **THEN** 系统返回 403且不泄露目标任务是否存在
