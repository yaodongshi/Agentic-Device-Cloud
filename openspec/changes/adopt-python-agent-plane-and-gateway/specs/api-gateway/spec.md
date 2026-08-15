# 能力：统一 API 网关（api-gateway）

## ADDED Requirements

### Requirement: 前端单一入口与路径路由
平台 MUST 通过统一 API 网关向客户端提供服务，客户端只配置一个网关地址；网关按路径前缀路由到后端服务，后端语言与实现变更对客户端无感。

#### Scenario: 前端只连网关
- **WHEN** 前端控制台发起 /v1/admin/*、/v1/agent/*、/v1/hitl/* 任一请求
- **THEN** 请求经网关路由到对应后端服务，客户端无感知后端语言

#### Scenario: 未匹配路由返回标准错误
- **WHEN** 请求路径不匹配任何已注册前缀
- **THEN** 返回 404 与统一错误码 10001，不泄露内部服务地址

#### Scenario: 后端实现替换无感
- **WHEN** 某后端服务实现替换（如数据面 Go 换 Python）仅变更网关路由配置
- **THEN** 前端契约与行为保持不变

### Requirement: 网关鉴权透传与限流
网关 MUST 透传客户端鉴权头到后端，不解析业务凭证；MUST 在网关层实施全局限流并输出限流响应头。

#### Scenario: 鉴权头透传
- **WHEN** 客户端携带 Authorization/X-ADC-Key 头经网关访问后端
- **THEN** 网关原样透传该头，后端完成业务鉴权

#### Scenario: 限流生效
- **WHEN** 客户端超过网关配置的调用速率
- **THEN** 返回 429，并携带 X-RateLimit-Limit/Remaining/Reset 响应头

### Requirement: trace_id 贯通
网关 MUST 为无 trace_id 的入站请求生成 trace_id，并在转发时注入，贯通网关与全部后端服务。

#### Scenario: trace_id 贯通调用链
- **WHEN** 客户端请求经网关路由到后端并产生审计日志
- **THEN** 审计日志可凭同一 trace_id 关联网关与后端调用

### Requirement: 内部服务不暴露公网
后端服务 MUST 只接受网关与内部网络访问，不得暴露公网端口。

#### Scenario: 后端端口隔离
- **WHEN** 外部客户端尝试直接访问后端服务端口
- **THEN** 连接被拒绝或不可达
