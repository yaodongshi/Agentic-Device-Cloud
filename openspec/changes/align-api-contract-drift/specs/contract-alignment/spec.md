# 能力：API 契约对齐（contract-alignment）

## ADDED Requirements

### Requirement: 免审调用响应信封
免审（低风险）tools/call 的响应 MUST 携带 request_id 信封字段，与高危 202 响应结构一致，便于客户端统一追踪。

#### Scenario: 免审响应含 request_id
- **WHEN** Agent 调用低风险工具成功执行
- **THEN** 200 响应体包含 request_id 与结果内容

### Requirement: 回调错误码分级
HITL 回调请求 MUST 按失败原因返回区分错误码：无签名 401/12004、签名过期 403/12003、未知工单 404/12001、已处理工单 409/12002。

#### Scenario: 无签名回调返回 401
- **WHEN** 回调请求缺少或伪造签名
- **THEN** 返回 401 与错误码 12004，工单状态不变

### Requirement: 跨租户调用显式拒绝
Agent 跨租户调用设备工具 MUST 返回显式拒绝错误（403），不得以 500 隐藏原因。

#### Scenario: 跨租户调用 403
- **WHEN** 租户 A 的 Agent 请求租户 B 的设备工具
- **THEN** 返回 403 与租户隔离错误码，审计记录异常事件
