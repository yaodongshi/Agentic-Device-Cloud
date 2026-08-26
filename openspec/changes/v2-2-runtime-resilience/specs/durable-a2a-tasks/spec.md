## ADDED Requirements

### Requirement: A2A 任务必须持久化到 PostgreSQL
系统 MUST 将 A2A 任务及其状态、租户、应用、请求数据、步骤、版本和审批归因持久化到 PostgreSQL，并以数据库为唯一权威源，落实 SEC-08。

#### Scenario: 服务重启恢复任务
- **WHEN** Python Agent 在任务创建或进入 `input-required` 后重启
- **THEN** 重启后的任一实例可读取相同任务、状态和审批上下文

#### Scenario: 多实例读取任务
- **WHEN** 任务由一个 Python Agent 实例创建并由另一实例读取
- **THEN** 两个实例观察到同一数据库权威状态且无需内存复制

### Requirement: A2A 创建请求必须幂等
系统 MUST 要求创建请求携带受格式和长度约束的幂等键，并以 `(tenant_id, application_id, idempotency_key)` 唯一约束保证重试不产生重复任务。

#### Scenario: 相同请求重试
- **WHEN** 同一租户应用以相同幂等键和相同规范化请求重复创建任务
- **THEN** 系统返回首次创建的任务且数据库中只有一条任务记录

#### Scenario: 幂等键复用于不同请求
- **WHEN** 同一租户应用以已有幂等键提交不同规范化请求
- **THEN** 系统返回 409且不覆盖原任务

#### Scenario: 不同应用使用相同幂等键
- **WHEN** 同一租户的两个应用使用相同幂等键创建任务
- **THEN** 系统分别创建归属各应用的任务

### Requirement: A2A 状态迁移必须使用数据库 CAS
系统 MUST 以期望旧状态和版本执行条件更新并单调递增版本，禁止依赖进程锁保证状态一致性；并发或过期写入只能有一个成功，落实 SEC-11。

#### Scenario: 并发审批同一任务
- **WHEN** 两个合法人类会话并发决策同一 `input-required` 任务
- **THEN** 仅一个 CAS 成功，另一个返回 409且不得覆盖首个 approver 或决策

#### Scenario: 使用过期版本更新
- **WHEN** 调用方基于已被其他实例更新的 task version 提交状态迁移
- **THEN** 系统返回 409并保留当前数据库状态

#### Scenario: 批准任务状态迁移
- **WHEN** 合法人类 approver 批准 `input-required` 任务且 CAS 成功
- **THEN** 任务状态变为 `working`且 version 递增，不得直接变为 `completed`

### Requirement: A2A 数据访问必须在 SQL 层按租户隔离
系统 MUST 使任务创建、列表、读取和状态迁移的数据库查询携带认证租户约束，`tenant_id` 和 `application_id` 只能来自认证主体，不得先读取跨租户数据再在内存过滤。

#### Scenario: 跨租户按 ID 读取
- **WHEN** 一个租户主体使用另一租户的 task id 读取任务
- **THEN** SQL 查询不返回记录且 API 返回 404

#### Scenario: 跨租户状态更新
- **WHEN** 一个租户主体尝试更新另一租户任务
- **THEN** 条件更新影响零行、API 返回 404且任务保持不变

#### Scenario: 租户任务列表
- **WHEN** 已认证主体列出 A2A 任务
- **THEN** 返回结果仅包含认证租户的数据
