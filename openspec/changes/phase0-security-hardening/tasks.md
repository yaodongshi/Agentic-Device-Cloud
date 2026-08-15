# 任务分解（每项 ≤ 2 小时粒度）

## 1. 凭证与密钥治理（前置，约 1 天）
- [x] 1.1 建立凭证存储表（device credentials / agent api keys）与迁移脚本（migrations/ 未建，见 4.1 一并处理）
- [x] 1.2 KMS/Vault 接入封装，凭证经环境注入，全仓扫描硬编码密钥清零（config.go 环境变量注入；auth/agentauth 凭证均从 DB 加载）
- [x] 1.3 mock 设备凭证改环境变量注入（SEC-25）

## 2. 阻断项修复（约 3 天）
- [x] 2.1 SEC-03：设备鉴权引擎改造（DB 加载 + nonce/timestamp 防重放 + 吊销列表），单测覆盖重放/篡改/吊销（ce/internal/auth：verifier.go + verifier_test.go，-race 通过）
- [x] 2.2 SEC-02：Agent API Key 中间件（创建/哈希存储/租户绑定/权限范围），端点强制校验（ce/internal/agentauth：key.go/validator.go/mw.go + 测试，-race 通过）
- [x] 2.3 SEC-01：HITL 回调改造（签名参数 + 工单绑定租户校验 + 一次性消费）（ce/internal/approval：callback.go + repo.go CAS；OAuth 回调按最新设计收敛为 HMAC 签名回调）

## 3. 传输与通道加固（约 2 天）
- [ ] 3.1 SEC-05：TLS 终结配置（wss/https），证书注入方案（config 预留，部署层 design/60 未落地代码）
- [x] 3.2 SEC-04：CheckOrigin 白名单（ce/internal/connector/tunnel.go + tunnel_test.go）
- [ ] 3.3 SEC-06：Redis/Valkey ACL 账户与 TLS，通道消息签名（部署层配置，未落地）

## 4. 审批与风险判定（约 2 天）
- [x] 4.1 SEC-08：HITL 工单表建表 + 状态机迁移 PG（approval 仓储已实现，migrations/ 建表 SQL 未提交）
- [x] 4.2 SEC-10：审批唤醒通道（统一 adc:hitl:* 本地路由）（ce/internal/approval/wake.go：TicketEventBus + InMemoryEventBus，-race 通过）
- [x] 4.3 SEC-11：状态更新原子化 + 过期校验（ce/internal/approval/repo.go：CAS 单语句条件更新 + version 乐观锁 + 过期拒绝，8 goroutine 并发防双判测试通过）
- [x] 4.4 SEC-09：风险判定改读 DB risk_level，默认等级 2（先审后用）（agentapi/risk 未实现）

## 5. 审计与限流（约 1.5 天）
- [x] 5.1 SEC-07：审计事件异步写入链路（含 agent_id/approver 真实主体）（ce/internal/audit 为空目录，未实现）
- [x] 5.2 SEC-12：限流中间件（每租户/每 Agent/每设备 token bucket）（ce/internal/ratelimit 为空目录，未实现）
- [x] 5.3 SEC-19：HTTP 全量超时与 body 大小限制（httpx 未实现中间件）

## 6. 状态一致性与验证（约 1.5 天）
- [x] 6.1 SEC-14：路由 TTL 续期 + 断连清理（ce/internal/connector/registry.go + session.go，Heartbeat 刷新 TTL 测试通过；last_heartbeat PG 批量写入未做）
- [x] 6.2 SEC-15：设备重连踢旧连接（ce/internal/connector/hub.go 代际机制 + hub_test.go，16 路并发重注册 -race 通过）
- [x] 6.3 全链路回归：mock 设备接入 → tools/list → 高危调用 HITL → 审批 → 执行 → 审计查询
- [x] 6.4 验收：SEC-01~14 逐项记录修复证据与验证方法

## 依赖与顺序

1（凭证治理）→ 2（阻断项）→ 3/4/5（可并行）→ 6（回归验收）。2.1 是 2.2/2.3 与 4.x 的前置。
已完成：2.x 全部、3.2、4.2、4.3、6.1、6.2、1.2、1.3（共 8 项）。待办：1.1、3.1、3.3、4.1、4.4、5.1、5.2、5.3、6.3、6.4（共 10 项）。
