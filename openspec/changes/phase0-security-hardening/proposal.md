# 变更：阶段 0 安全加固

## Why

《doc/05-架构产品化评审与演进路线》评审发现原方案 25 项安全问题：3 项阻断级（SEC-01/02/03）使 HITL 审批熔断与租户隔离形同虚设，11 项严重级（SEC-04~14）使平台不可对外上线。按 05 文档结论，阶段 0 必须在任何对外联调与开源发布之前完成全部阻断级与严重级修复（约 2 周）。

## What Changes

- SEC-01：/v1/hitl/action 审批回调改为 OAuth 身份 + HMAC 签名校验，approver 取真实身份
- SEC-02：Agent 端点引入 API Key 认证与租户强绑定，租户上下文取自凭证而非请求头
- SEC-03：设备凭证改从 DB/KMS 加载，认证改 HMAC(secret, deviceID + nonce + timestamp) 防重放，支持轮转吊销
- SEC-04：WebSocket Upgrader CheckOrigin 白名单校验
- SEC-05：全链路 TLS（设备面 wss://、Agent 面 HTTPS）
- SEC-06：Redis/Valkey 通道 ACL + TLS + 节点签名（ADR-13）
- SEC-07：工具调用与审批决策异步写入审计日志（adc_mcp_call_logs）
- SEC-08：HITL 工单主存迁移 PostgreSQL，Redis 仅作唤醒通道（ADR-05）
- SEC-09：风险判定改为读取 DB risk_level 元数据（过渡方案，OPA 在阶段 2）
- SEC-10：修复跨节点 HITL 唤醒缺口（adc:hitl:resolve 无订阅者）
- SEC-11：审批状态更新原子化（CAS/唯一约束），过期工单不可批准
- SEC-12：Agent/设备/审批三维度限流中间件
- SEC-13：webhook key 与凭证改环境变量/KMS 注入，清理硬编码
- SEC-14：设备路由 TTL 续期 + 断连主动清理，对齐 last_heartbeat

## Capabilities

- **New Capabilities**: `security-hardening`（网关安全基线：设备认证、Agent 认证、HITL 回调验签、风险判定、审计、传输加密、限流）
- **Modified Capabilities**: 无（openspec/specs/ 暂无既有能力）

## Impact

- 代码：auth/、gateway/handler.go、hitl/、cluster/ 全量改造（PoC 代码在 doc/Agentic Device Cloud.md 内嵌，需先抽出独立工程）
- 依赖：PostgreSQL（工单/审计主存）、Valkey（替换 Redis）、KMS/Vault 接入
- 非目标：不做 OPA 策略中心（阶段 2）、不做 NATS JetStream 迁移（阶段 2）、不做控制面与控制台（阶段 1）、不做原生 MCP 设备接入与 A2A（阶段 2/3）
