## ADDED Requirements

### Requirement: 租户可管理开发者应用
系统 SHALL 允许授权用户创建、查看、停用和删除本租户开发者应用，并禁止跨租户访问。

#### Scenario: 创建租户应用
- **WHEN** 租户管理员提交合法名称、用途和允许 scope
- **THEN** 系统创建归属当前租户的应用并写入审计记录

### Requirement: 应用凭证必须一次性展示和可吊销
系统 MUST 只存储凭证哈希，完整 secret 仅在创建或轮换响应中展示一次；旧凭证在轮换或吊销后立即失效。

#### Scenario: 轮换应用凭证
- **WHEN** 授权用户轮换应用凭证
- **THEN** 响应仅一次返回新 secret，旧 secret 随即无法认证

### Requirement: 应用调用必须受 scope 限制
系统 MUST 默认拒绝未授权能力；A2A 任务列表和读取要求 `a2a.tasks:read`，创建和审批决策要求 `a2a.tasks:write`。Python A2A 服务 MUST 对每次请求调用 Go 应用凭证 introspection，认证依赖不可用或响应无效时 fail closed。

#### Scenario: 超范围调用
- **WHEN** 只拥有只读 scope 的应用提交写任务
- **THEN** 系统返回 403 并记录应用、租户和拒绝原因

#### Scenario: A2A 匿名调用
- **WHEN** 调用方未携带有效应用凭证访问任一 A2A 任务路由
- **THEN** 系统返回 401，且 Agent Card 仍可匿名读取

### Requirement: A2A 任务必须按认证租户隔离
系统 MUST 由认证主体注入任务的 `tenant_id` 和 `application_id`，不得信任客户端提交的租户字段；列表、读取和审批决策 MUST 严格限制在当前认证租户。

#### Scenario: 跨租户读取或决策
- **WHEN** 一个租户的应用读取或审批另一租户的任务
- **THEN** 系统返回 404 且不泄漏任务存在性，列表不包含其他租户任务

### Requirement: 门户必须提供最小 A2A 接入信息
控制台 SHALL 展示 Agent Card URL、A2A endpoint、鉴权方式和中英最小请求示例，并提供只读连通性测试。

#### Scenario: 开发者完成连通性测试
- **WHEN** 开发者使用新凭证执行门户测试
- **THEN** 页面显示可判定的成功或诊断失败结果，不暴露完整凭证
