# 能力：网关安全基线（security-hardening）

## ADDED Requirements

### Requirement: 设备凭证防重放认证
设备接入 MUST 经平台签发的凭证认证，凭证支持轮转与吊销；认证报文含时间戳与 nonce，重放与篡改一律拒绝。

#### Scenario: 重放旧报文被拒绝
- **WHEN** 攻击者重放设备曾发送过的合法认证报文
- **THEN** 认证失败并记录告警事件

#### Scenario: 凭证吊销后立即失效
- **WHEN** 管理员吊销某设备凭证后该设备用旧凭证再次连接
- **THEN** 认证失败且已建立的旧连接被踢下线

### Requirement: Agent 身份认证与租户绑定
Agent 访问平台端点 MUST 携带 API Key 或 OIDC 凭证，租户上下文取自凭证而非请求头，跨租户访问一律拒绝。

#### Scenario: 跨租户访问被拒
- **WHEN** 租户 A 的 Agent 携带合法凭证请求租户 B 的设备工具
- **THEN** 返回 403 并记录审计事件

### Requirement: HITL 审批回调验签
审批回调 MUST 校验签名与审批人身份，工单一次性消费；无签名、伪造签名或已处理工单一律拒绝。

#### Scenario: 伪造回调被拒
- **WHEN** 回调请求签名校验失败
- **THEN** 工单保持 PENDING，指令不执行，记录异常事件

#### Scenario: 过期工单不可批准
- **WHEN** 审批人提交超过有效期工单的批准回调
- **THEN** 决策被拒，工单置为 EXPIRED

### Requirement: 风险判定读取数据库元数据
高危拦截 MUST 依据持久化的工具风险等级元数据，默认等级为 2（先审后用），不依赖工具名关键字。

#### Scenario: 改名不绕过拦截
- **WHEN** Agent 调用数据库配置风险等级为 2 的工具
- **THEN** 触发 HITL 审批，无论工具名是否含高危关键字

### Requirement: 审计日志不可抵赖
工具调用与审批决策 MUST 异步写入审计日志，记录真实 agent 与审批人身份，日志只追加不可改。

#### Scenario: 审批决策留痕
- **WHEN** 一次高危指令经审批执行后查询审计日志
- **THEN** 可查到调用参数、审批人、审批意见与执行结果

### Requirement: 全链路传输加密
设备隧道 MUST 强制 wss、Agent 与管理 API 强制 HTTPS，内部通道启用 TLS 与最小权限 ACL。

#### Scenario: 明文连接被拒
- **WHEN** 设备尝试以 ws:// 明文建立隧道
- **THEN** 连接被拒绝并记录告警

### Requirement: 分级限流
平台 MUST 按租户、Agent、设备三个维度实施限流，超限请求返回 429 且不影响其他租户服务。

#### Scenario: 超限请求被限流
- **WHEN** 某 Agent 超过配置的调用频率上限后继续发起调用
- **THEN** 返回 429，审计日志记录限流事件
