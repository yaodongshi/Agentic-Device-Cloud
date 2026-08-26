## ADDED Requirements

### Requirement: 默认 Compose 不得自动写入种子数据
系统 MUST 使默认 Compose 启动路径不创建、不依赖且不执行 seed；seed 只能作为显式 `dev` profile 中的一次性服务运行，并在环境不是开发环境时拒绝执行。

#### Scenario: 默认启动不执行 seed
- **WHEN** 操作员在未启用任何 profile 时执行默认 Compose 启动
- **THEN** 系统完成迁移和服务启动但不运行 seed，数据库中不出现演示租户、用户、角色绑定或凭证

#### Scenario: 非开发环境显式请求 seed
- **WHEN** 操作员在非开发环境显式启动 seed 服务
- **THEN** seed 以非零状态退出且不写入任何数据

#### Scenario: 开发环境一次性 seed
- **WHEN** 操作员显式启用 `dev` profile 并运行 seed 服务
- **THEN** seed 可重复安全执行、完成后正常退出且不会成为常驻服务

### Requirement: 生产启动必须拒绝不安全凭证与 KEK
系统 MUST 在迁移和业务服务启动前校验生产环境所需凭证与 KEK，缺失、默认值、占位值、类型错误、格式错误或低于安全强度要求时 MUST fail closed，并落实 SEC-03/13/25。

#### Scenario: 生产使用默认凭证
- **WHEN** 生产配置包含仓库已知默认管理员、设备或应用凭证
- **THEN** 启动 guard 以非零状态退出且业务服务不进入 ready

#### Scenario: 生产 KEK 不合法
- **WHEN** KEK 缺失、为占位值或不满足规定格式与强度
- **THEN** 启动 guard 以非零状态退出且不执行迁移或 seed

#### Scenario: 生产安全配置合法
- **WHEN** 所有必需凭证和 KEK 均为非默认且满足类型、格式、范围和强度要求
- **THEN** guard 成功并允许迁移及业务服务启动

### Requirement: 配置与 seed 日志不得泄露秘密
系统 MUST 只记录敏感配置的变量名、检查类别和脱敏诊断，不得记录凭证、KEK、seed secret、哈希、可逆编码或足以推导秘密的片段，并落实 SEC-13/25。

#### Scenario: 敏感配置校验失败
- **WHEN** guard 或 seed 因敏感配置不合法而失败
- **THEN** 日志可定位失败变量和规则，但不包含提交值及其可识别片段

### Requirement: Python Agent 非公开路由必须逐请求鉴权
系统 MUST 仅允许匿名访问 health、readiness 和 Agent Card；LLM、Eval 与 A2A 的全部其他路由 MUST 每请求实时 introspection、校验所需开发者 scope，并由认证结果注入 `tenant_id` 和 `application_id`，认证依赖异常时 fail closed，落实 SEC-02。

#### Scenario: 匿名访问公开路由
- **WHEN** 调用方不携带凭证访问 health、readiness 或 Agent Card
- **THEN** 系统返回公开响应且不泄露租户数据

#### Scenario: 匿名访问非公开路由
- **WHEN** 调用方不携带有效凭证访问任一 LLM、Eval 或 A2A 非公开路由
- **THEN** 系统返回 401

#### Scenario: scope 不足
- **WHEN** 有效应用凭证访问其未获授权的 LLM、Eval 或 A2A 能力
- **THEN** 系统返回 403并记录脱敏拒绝原因

#### Scenario: 客户端伪造租户
- **WHEN** 已认证应用提交与认证主体不一致的租户字段或租户头
- **THEN** 系统忽略或拒绝该字段并仅在认证租户内处理请求

#### Scenario: 凭证吊销后再次请求
- **WHEN** 应用凭证被吊销后用于下一次 Python Agent 请求
- **THEN** 实时 introspection 拒绝该请求且 Python Agent 不使用旧认证结果

### Requirement: HITL 决策必须来自当前人类授权主体
系统 MUST 仅接受同租户有效人类管理员会话中 `TENANT_ADMIN`、`APPROVER` 或平台管理员语义的 HITL decision，并将真实用户记录为 approver；机器应用凭证不得自批，落实 SEC-01/21。

#### Scenario: 机器应用尝试审批
- **WHEN** 开发者应用使用 application credential 调用 HITL decision
- **THEN** 系统返回 403且任务状态不变

#### Scenario: 无审批角色的人类尝试审批
- **WHEN** 当前有效人类会话不具有管理员或审批人角色
- **THEN** 系统返回 403且任务状态不变

#### Scenario: 人类审批通过
- **WHEN** 同租户有效管理员或审批人会话批准 `input-required` 任务
- **THEN** 系统记录真实 approver并仅将任务原子迁移到 `working`，不得将批准伪造成 `completed`

#### Scenario: 跨租户人类尝试审批
- **WHEN** 一个租户的管理员或审批人决策另一租户任务
- **THEN** 系统返回 404且不泄露任务存在性
