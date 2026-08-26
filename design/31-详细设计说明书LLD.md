# ADC 详细设计说明书（31）

> 版本：V1.0　状态：评审稿　适用版本：ADC V1.0 MVP
> 上游依据：`design/30`（高层设计，模块边界）、`design/32`（数据库设计说明书，表结构与状态枚举）、`design/33`（接口契约，HTTP 端点与错误码）
> 输入基线：`doc/Agentic Device Cloud.md`（PoC 代码）、`doc/05-架构产品化评审与演进路线.md`（SEC-01~14、ADR-01~18、GAP-01~16）、`openspec/changes/phase0-security-hardening/`
> 一致性锚点：本说明书与 30/32/33 号设计文档共享同一定义；与 32 号文档的表名、状态枚举、幂等机制严格对齐，任何一处变更须三文同步修订并走 OpenSpec 流程。
> 文档约定：全中文、无 emoji；Go 代码片段注释可用英文；引用的 SEC/ADR/FR 编号含义见 doc/05 与 doc/03，本说明不重复展开。

---

## 1. 引言

### 1.1 目的与范围

本说明书（LLD）将 design/30 定义的 V1.0 MVP 后端四服务拆解为可直接编码的详细设计：包结构、关键类型与接口签名、并发模型、错误与重试策略、事务边界、单测与集成测试清单；并按方案 A 增补统一 API 网关与 Python Agent 面的详细设计（3.5/3.6 节）。读者对象为 Go 与 Python 后端实现者及代码评审者；阅读前提是已理解 design/30 的模块边界、design/32 的表结构与 design/33 的接口契约，本文不重复其内容。

设计范围（V1.0 MVP）：

1. Device Connector：B 类遗留设备 Legacy Bridge WSS 隧道、HMAC+nonce 设备鉴权、工具同步（ADR-15 中 Legacy Bridge 路径）。
2. Agent API：MCP 风格 tools/list 与 tools/call、API Key 认证与租户强绑定、工具聚合、调用路由、限流。
3. Approval Service：HITL 工单状态机（PG 主存）、Valkey 唤醒、企微/钉钉通知（重试与降级）、超时扫描。
4. Admin API：租户、设备台账、工具风险等级、API Key、配额、审计查询与导出。

非目标（V1.0 不做，仅预留扩展点）：原生 MCP Server 设备接入与 OAuth 2.1 绑定（阶段 2，ADR-15）、NATS JetStream（阶段 2，ADR-02）、OPA 策略中心（阶段 2，ADR-07）、A2A 网关（阶段 3，ADR-16/17；V1.5 仅由 Python Agent 面发布 A2A Agent Card，见 3.6.1）、设备 mTLS（阶段 3，ADR-04 终态）、LLM 网关计费计量（阶段 3/4，EE 特性；V1.5 起编排接入由 Python Agent 面承载，见 3.6.1）、多区域 cell（ADR-11）、边缘 SDK（阶段 4）。

### 1.2 与上层设计的对应关系

| design/30 模块（HLD） | 本 LLD 章节 | 进程入口 | 仓库 | 对外通道 |
|---|---|---|---|---|
| Device Connector | 3.1 | ce/cmd/device-connector | ce/ | WSS（设备面）、内部 HTTP |
| Agent API | 3.2 | ce/cmd/agent-api | ce/ | MCP 风格 HTTP（design/33） |
| Approval Service | 3.3 | ce/cmd/approval-service | ce/ | 回调 HTTP、Valkey 唤醒 |
| Admin API | 3.4 | ce/cmd/admin-api | ce/（EE 增强经 seam 装配） | 管理 HTTP（design/33） |

注（方案 A 新增）：统一 API 网关（3.5，ce/cmd/gateway）为前端与外部客户端唯一入口，上表四服务的对外通道一律经网关转发；Python Agent 面（3.6，ce/py-agent）经 /v2/agents 前缀对外。两模块为方案 A 新增边界，其与 design/30 模块划分、design/60 部署清单的同步修订随 OpenSpec 变更进行，本表口径暂按 design/30 现状表述。

三仓与依赖方向（ADR-12/14）：core-sdk/（Apache-2.0，wire 协议与 SDK，OEM 可静态链接）→ ce/（LGPL-3，上述四服务）→ ee/（源码可见商业许可，seam 实现）。ee/ 只能实现 ce/ 中冻结的 seam 接口并通过插件注册装配，ce/ 不 import ee/。V1.0 即冻结的 seam 接口：RiskPolicy、TaskStore、AdminAuthenticator、QuotaEngine、TicketNotifier（通知渠道），语义化版本管理。

服务间通信约定：Agent API 与 Approval Service 之间为同步 HTTP（创建工单）+ Valkey 唤醒订阅（等待决策）；跨节点设备调用经 MessageBus 抽象（V1.0 实现为 Valkey Pub/Sub，阶段 2 切换 NATS JetStream，ADR-02）。设计/33 的端点契约与错误码体系为对外接口，本文只引用不重复。

### 1.3 Go 工程规范总则

#### 1.3.1 包结构分层

ce/ 内统一布局：

```
ce/
├── cmd/<service>/main.go        # 仅装配：读环境变量、构造依赖、注册路由、优雅退出
├── internal/<service>/
│   ├── server/                  # 传输层：HTTP/WSS 处理器、中间件、请求解析与响应编码
│   ├── app/                     # 用例编排层：业务流程、事务边界、审计埋点
│   └── store/                   # 仓储实现：PG(pgx)、Valkey(go-redis) 访问
└── pkg/                         # 跨服务共享：pkg/audit、pkg/clusterbus、pkg/valkey、pkg/ratelimit、pkg/slogx
```

原则：internal 包不可被其他服务 import（Go 编译期强制）；跨服务共享代码只放 ce/pkg/；领域类型（聚合根、状态枚举、事件结构）放各服务 internal/<service>/domain 包；wire 协议类型只在 core-sdk/。每个服务内保持 server（薄）、app（业务）、store（IO）三层，禁止 server 直接调用 store。

#### 1.3.2 依赖方向

core-sdk ← ce ← ee；服务之间禁止横向 import 对方的 internal 包；app 层只依赖领域类型与仓储接口（接口定义在 app 或 domain 包，实现在 store 包）；外部库只允许出现在 store 与传输层（gorilla/websocket、pgx、go-redis 均不穿透到 app 层）。新增依赖须通过 doc/05 7.1 许可证兼容审计（govulncheck 与 licensecheck 并列门禁）。

#### 1.3.3 context 规范

context 作为第一参数贯穿所有阻塞 IO（网络、锁、channel 接收）；禁止把 context 存进结构体字段；后台 goroutine 使用独立 context 并显式设置超时；禁止在处理链中间出现 context.Background()（仅允许 main 与测试中使用）；所有 select 等待必须有 ctx.Done 分支。长连接组件的退出统一走 doneChan（关闭广播）加 context 取消双通道。

#### 1.3.4 错误处理

哨兵错误 + fmt.Errorf 加 %w 包装；错误分类实现 Retryable 标记接口（`interface{ Retryable() bool }`），网络瞬断、Valkey 抖动、PG 死锁可重试，鉴权失败、参数非法、状态冲突不可重试；错误一次处理一处记录：底层只包装上抛，中间层不重复打日志，HTTP 边界统一映射为 design/33 错误码（含 request_id 可检索）。

#### 1.3.5 日志规范

slog 结构化日志；上下文字段统一：trace_id（请求链 ID，取 X-Trace-ID 或生成 UUID）、request_id（业务幂等键）、tenant_id、device_code、agent_id、ticket_id、node_id；敏感脱敏：密钥、凭证、回调签名、调用参数中的口令类键名一律打码；级别约定：设备断连 Info、鉴权失败 Warn（限流降级采样防刷屏）、状态机冲突 Info、通知失败告警级（并触发告警审计事件）。

#### 1.3.6 配置规范

全部配置经环境变量注入（SEC-13），启动时一次性解析并校验失败即退出；密钥类配置（Valkey 密码、回调 HMAC secret、webhook 地址、设备凭证 KEK）禁止出现在命令行参数与日志中；配置项统一 ADC_ 前缀，缺省值可运行（本地开发零配置起步），生产部署清单见 design/60。

#### 1.3.7 横切约定

Valkey 键空间规范（全部带 adc: 前缀，禁用动态 key 拼接未校验输入，SEC-20）：

| 键 | 类型 | 生命周期 | 用途 |
|---|---|---|---|
| adc:loc:{tenant}:{deviceCode} | String（节点 ID） | TTL 90s，心跳续期 | 设备路由索引 |
| adc:tools:v1:{tenant}:{deviceCode} | String（JSON） | TTL 90s，随 loc 续期 | 工具缓存 |
| adc:online:{tenant} | Set | 与连接同生命周期 | 租户在线设备集合 |
| adc:tools:gen:{tenant} | String（递增） | 长期 | 工具视图代际 |
| adc:nonce:{sha256(nonce)} | String | TTL 600s | 防重放一次性消费 |
| adc:req:{nodeID} / adc:resp:{nodeID} | Pub/Sub | 瞬态 | 跨节点调用 |
| adc:hitl:resolve | Pub/Sub | 瞬态 | 审批决策统一唤醒 |
| adc:audit:events / adc:audit:dead | List | 消费即出队，死信保留 7 天 | 审计异步队列 |
| adc:lock:scanner | String（SET NX EX） | 50s | 扫描器互斥 |
| adc:quota:call:{tenant}:{month} | Counter | 月度 | 调用量热计数（design/32 6.2） |

优雅停机顺序（四服务一致）：先摘除流量（停止接受新连接与新 HTTP 请求）→ 通知等待中的会话与 goroutine 取消（context cancel + doneChan 广播）→ 等 errgroup 全部退出（上限 10 秒）→ 关闭 Valkey/PG 连接池 → 退出。审计入队失败时不阻塞停机（事件已在上游链路的先审计语义中尽力而为）。

可观测性起步埋点（GAP-07 阶段 1 基线）：Prometheus 指标 node_ws_connections（在线连接数）、node_tool_call_total/duration_seconds（按结果状态分桶）、node_hitl_ticket_duration_seconds、node_audit_queue_depth（审计队列深度）、node_ratelimit_rejected_total；slog 日志全文携带 1.3.5 上下文字段；V1.0 不做分布式链路追踪（阶段 2 接 Langfuse）。

审计事件类型清单（对齐 design/32 adc_audit_logs.event_type 枚举）：tool_call、approval_decision、admin_op、auth_event、quota_event、device_event；每个事件必含 request_id（幂等键）、actor_type/actor_id（真实主体，SEC-21）、tenant_id、created_at；来源服务与节点在附加属性中记录。

---

## 2. 领域模型设计

### 2.1 限界上下文图

```
┌──────────────────────────── 设备上下文 ────────────────────────────┐
│ Device（聚合根） DeviceCredential（实体） DeviceTool（实体）         │
│ DeviceSession（运行时实体，内存，非持久化）                          │
└───────┬───────────────────────────▲───────────────────────────────┘
        │ 心跳/在线状态               │ 工具元数据（含 risk 建议值）
┌───────▼───────────┐   ┌───────────┴───────────────────────────────┐
│ 租户与权限上下文   │   │ 审批上下文                                   │
│ Tenant（聚合根）   │   │ ApprovalTicket（聚合根） Decision（值对象）  │
│ ApiKey（实体）     │   │ （调用方创建工单，回调驱动状态机）             │
│ Quota（值对象）    │   └───────────┬───────────────────────────────┘
└───────┬───────────┘               │ 审批决策事件
        │ 配额与风险策略              │
┌───────▼────────────────────────────▼──────────────────────────────┐
│ 审计上下文（append-only）：AuditEvent（聚合根），异步落库，只读投影   │
└─────────────────────────────────────────────────────────────────────┘
```

上下文交互规则：审批上下文对 Device/Tenant 只持标识引用（ticket 冗余 tool_name、approver_name 快照，见 design/32 3.8/3.13），不跨聚合开大事务；配额与风险决策发生在租户与权限上下文的策略决策点（RiskPolicy、QuotaEngine seam），决策结果以事件进入审计上下文；设备在线状态为跨上下文高频只读数据，经 Valkey 传递（design/32 D-1 原则：PG 权威 + Valkey 热数据）。

### 2.2 聚合根与实体清单

聚合即事务边界。实体与 design/32 表结构一一对应：

| 聚合根/实体 | 类型 | 对应 design/32 表 | 归属上下文 | 读写方 |
|---|---|---|---|---|
| Tenant | 聚合根 | adc_tenants（含配额计数列） | 租户与权限 | Admin API 写；Agent API/Connector 读 |
| ApiKey | 实体 | adc_agent_api_keys | 租户与权限 | Admin API 签发/吊销；Agent API 校验 |
| Quota | 值对象 | adc_tenants.quota_* / used_* | 租户与权限 | Admin API 配置；Agent API 执行扣减 |
| Device | 聚合根 | adc_devices | 设备 | Admin API 台账；Connector 状态与心跳 |
| DeviceCredential | 实体 | adc_devices.credential_* 列 | 设备 | Admin API 签发/重置；Connector 校验 |
| DeviceTool | 实体 | adc_device_tools | 设备 | Connector 同步；Admin API 改风险等级 |
| ApprovalTicket | 聚合根 | adc_approval_tickets | 审批 | Approval Service 独占写；Admin API 查询 |
| AuditEvent | 聚合根 | adc_audit_logs（分区） | 审计 | 四服务生产；审计消费器落库；Admin API 查询 |

补充说明：adc_users/roles/device_groups/notifications/usage_events 等表在 V1.0 由 Admin API 与审批通知链路读写，其领域归属以 design/32 实体清单为准；ApprovalTicket 的 params_hash 部分唯一索引（design/32 6.3）承担"同设备同工具同参数仅一个在途工单"的防重语义，创建工单的 INSERT 冲突即返回已有工单。

#### 2.2.1 跨上下文领域事件清单

上下文之间的协作一律经事件或明确的服务调用完成，不跨聚合开事务。事件经 Valkey（V1.0）传递，消费方以事件 ID 幂等处理：

| 事件 | 生产方 | 消费方 | 用途 |
|---|---|---|---|
| device.online / device.offline | Device Connector | Agent API（失效工具缓存）、Admin API（在线状态投影） | 在线状态同步与缓存一致性 |
| tools.synced | Device Connector | Agent API（bump 工具代际） | 工具视图失效 |
| ticket.resolved | Approval Service | Agent API（唤醒挂起调用）、通知链路 | 审批决策传播（adc:hitl:resolve） |
| quota.adjusted | Admin API | Agent API（配额缓存刷新） | 配额变更即时生效 |
| risk.changed | Admin API | Agent API（风险判定缓存刷新） | 风险等级变更生效 |
| audit.* | 各服务 | 审计消费器（落 adc_audit_logs） | 异步审计落库 |

事件可靠性等级：在线状态类允许丢失（PG 有重建路径，design/32 D-1）；ticket.resolved 与 audit 类不允许静默丢失（ticket.resolved 有等待方超时兜底与工单状态可查询两条重建路径；audit 靠先入队语义与死信队列保底）。

### 2.3 关键状态机

#### 2.3.1 设备状态机

状态集合取自 design/32 adc_devices.status 枚举：OFFLINE / ONLINE / ERROR / FROZEN / RETIRED。

| 当前态 | 触发事件 | 目标态 | 守卫条件 | 副作用 |
|---|---|---|---|---|
| OFFLINE | 首次 WSS 连接成功且完成工具同步 | ONLINE | 凭证鉴权通过、非 FROZEN/RETIRED | 注册路由索引、写 last_heartbeat_at、审计 device_event |
| ONLINE | 心跳超时（连续 2 个 pong 窗口）/连接断开 | OFFLINE | — | 清理路由索引、发布设备下线事件、审计 |
| OFFLINE | 重新连接 | ONLINE | 凭证有效 | 踢旧会话（代际机制）、TTL 续期 |
| ONLINE/OFFLINE | 10 分钟内异常断连满 3 次 | ERROR | — | 告警审计事件 |
| ERROR | 重连成功 | ONLINE | 凭证有效 | 清零异常计数 |
| 任意非终态 | 管理员冻结 | FROZEN | — | 立即断开现有连接、拒绝新连接、作废在途工单、审计 admin_op |
| FROZEN | 管理员解冻 | OFFLINE | — | 审计 admin_op |
| 任意 | 管理员退役 | RETIRED | — | 终态、审计 admin_op |

状态权威存储为 PG；ONLINE/OFFLINE 的瞬时变更先落 Valkey 心跳（design/32 1.1 允许丢失），批量落盘 last_heartbeat_at 由 Connector 心跳周期执行（SEC-17）。

#### 2.3.2 审批工单状态机

状态集合与 design/32 adc_approval_tickets.status CHECK 约束一致：PENDING / APPROVED / REJECTED / EXPIRED。

| 当前态 | 触发事件 | 目标态 | 守卫条件（落进 CAS 语句 WHERE） | 副作用 |
|---|---|---|---|---|
| PENDING | 回调 approve 且验签与 OAuth 身份通过 | APPROVED | status='PENDING' AND expires_at>now() AND version 匹配 | 唤醒挂起调用、审计 approval_decision、通知审批结果 |
| PENDING | 回调 reject 且验签通过 | REJECTED | 同上 | 唤醒挂起调用、审计、通知 |
| PENDING | 超时扫描器到期 / 挂起调用方取消 | EXPIRED | 同上 | 唤醒挂起调用、审计 |
| APPROVED/REJECTED/EXPIRED | 任何重复回调 | 保持不变（拒绝迁移） | — | 审计"重复尝试被拒"（一次性消费） |

迁移实现为单语句条件原子更新（与 design/32 6.1 完全一致），受影响行数为 0 即冲突或已过期，返回"工单已处理"（SEC-11）；乐观锁 version 兜底；回调签名验证失败在进入状态机之前即拒绝（SEC-01/13）。

补充语义：EXPIRED 覆盖三种触发源——超时到期（扫描器）、挂起调用方上下文取消、目标设备离线（FR-005 异常场景，工单失去执行前提即失效）；三种触发均走同一 CAS 迁移与同一唤醒路径，Agent 侧按决策状态区分返回文案。工单的 tool_name/arguments/approver_name 为创建与决策时刻的快照（design/32 3.13），状态机迁移不触碰快照字段，保证历史工单语义稳定。

---

## 3. 核心模块详细设计

每个模块给出：内部包结构、关键类型与接口签名、核心流程、并发模型、错误与重试策略。接口统一编号 I1~I23（I21~I23 定义于 3.5/3.6 节），第 4 章汇总索引。

### 3.1 Device Connector

#### 3.1.1 内部包结构

```
ce/internal/connector/
├── server/server.go        # /v1/devices/tunnel 接入：Origin 白名单（SEC-04）、upgrade、会话装配
├── auth/auth.go            # HMAC+nonce 鉴权（SEC-03）
├── session/session.go      # 会话生命周期：读/写泵、心跳、doneChan、幂等 Close
├── session/hub.go          # 本地会话路由表：代际踢旧（SEC-15）
├── registry/registry.go    # Valkey 路由索引：注册/TTL 续期/断连清理（SEC-14）
└── sync/sync.go            # tools/list 同步、risk 权威合并、PG 落库（SEC-09）
```

#### 3.1.2 关键类型与接口签名

```go
// auth.go — device handshake authentication (SEC-03)
type DeviceAuthenticator interface {
    Authenticate(ctx context.Context, h *sdkproto.Handshake) (*DeviceIdentity, error)
}

type DeviceIdentity struct {
    DeviceCode string   // global unique, from credential record
    TenantID   string   // UUID string, from credential record, never from request
}

// handshake wire format (core-sdk): headers X-Device-Code / X-Device-TS / X-Device-Nonce / X-Device-Sig
// sig = HMAC-SHA256(secret, deviceCode + "\n" + ts + "\n" + nonce)
```

```go
// registry.go — Valkey is a routing index only; the local hub is authoritative (GAP-11)
type SessionRegistry interface {
    Register(ctx context.Context, tenantID, deviceCode, nodeID string, ttl time.Duration) error
    Heartbeat(ctx context.Context, tenantID, deviceCode string) error // refresh loc+tools TTL (SEC-14)
    Unregister(ctx context.Context, tenantID, deviceCode string) error // DEL + publish offline event
    OnlineSet(ctx context.Context, tenantID, deviceCode string, member bool) error
}
```

```go
// sync.go — tool sync, DB is authoritative for risk_level / is_enabled (SEC-09)
type ToolCatalog interface {
    UpsertTools(ctx context.Context, tenantID string, deviceID, tools []sdkproto.MCPTool) ([]sdkproto.MCPTool, error)
    // merge rule: DB row exists -> keep DB risk_level/is_enabled, update schema/description;
    // DB row missing -> insert with device-reported risk_level, default 2 if absent (先审后用)
}
```

#### 3.1.3 HMAC+nonce 鉴权流程（SEC-03）

```go
func (a *authenticator) Authenticate(ctx context.Context, h *sdkproto.Handshake) (*DeviceIdentity, error) {
    if !deviceCodePattern.MatchString(h.DeviceCode) { return nil, ErrInvalidID }          // SEC-20
    if abs(time.Now().Unix()-h.TS) > authWindowSecs { return nil, ErrClockSkew }          // 300s window
    consumed, err := a.nonces.Consume(ctx, h.Nonce, authWindowSecs*2)                     // SET NX EX, one-shot
    if err != nil || !consumed { return nil, ErrReplay }                                   // replay rejected
    cred, err := a.credStore.Load(ctx, h.DeviceCode)                                      // PG, 30s cache + singleflight
    if err != nil || !cred.Enabled() || cred.Status == FROZEN || cred.Status == RETIRED {
        return nil, ErrRevoked
    }
    want := hmacSHA256(cred.Secret(), h.DeviceCode+"\n"+strconv.FormatInt(h.TS, 10)+"\n"+h.Nonce)
    if subtle.ConstantTimeCompare([]byte(want), []byte(h.Sig)) != 1 { return nil, ErrBadSignature }
    return &DeviceIdentity{DeviceCode: h.DeviceCode, TenantID: cred.TenantID}, nil
}
```

要点：时间窗正负 300 秒；nonce 一次性消费走 Valkey SETNX（TTL 为时间窗两倍），Valkey 不可用时拒绝接入（fail-closed）；凭证从 PG 加载，本地缓存 30 秒加 singleflight，FROZEN/RETIRED 立即生效；hmac 类型凭证服务端密文存储（KEK 加密，密钥经环境变量 ADC_DEVICE_KEK 注入，存储形式以 design/32 credential_hash 字段语义为准）；支持凭证轮转过渡（credential_version 双凭证窗口，SEC-03 验证方法"吊销后旧凭证失效"）。常数时间比较防时序侧信道。

#### 3.1.4 WSS 会话管理与心跳

```go
// session.go — per-connection lifecycle, idempotent close
type Session struct {
    identity  DeviceIdentity
    gen       uint64                    // generation: bumped by hub on kick-old (SEC-15)
    conn      *websocket.Conn
    writeMu   sync.Mutex                // single writer, all frames serialized
    pending   sync.Map                  // requestID -> chan *sdkproto.JSONRPCResponse
    done      chan struct{}
    closeOnce sync.Once
}

func (s *Session) SendRPC(ctx context.Context, method string, params any) (*sdkproto.JSONRPCResponse, error)
func (s *Session) Run(ctx context.Context, onClose func(*Session)) // reader pump; writer goroutine owned inside
func (s *Session) Close()                                         // closeOnce: close(done) + conn.Close
```

并发模型（每连接 2 个 goroutine，生命周期清晰）：读泵 goroutine 循环 ReadMessage 并 dispatch（按 requestID 唤醒 pending 等待者）；写泵由 Run 内建 goroutine 承担，负责心跳 ping（25 秒周期）与所有下行帧的串行写出（writeMu 保证帧不交织）。下行超时 writeWait=10 秒；读侧 pongWait=60 秒，连续两个 pong 窗口未收到任何帧判心跳超时断连。所有 goroutine 退出条件统一为 done 关闭；会话结束时由 onClose 回调触发 hub 与 registry 清理。优雅停机：HTTP server Shutdown 后广播关闭全部会话，errgroup 等待各会话退出。

会话全生命周期时序：升级握手前先完成 3.1.3 鉴权（失败不升级，返回 401 与原因码）→ hub.Register（踢旧，SEC-15）→ registry.Register（写 loc/online 集合）→ 5 秒超时内 tools/list 并同步（3.1.6）→ 进入心跳与心跳续期循环 → 断连时按序执行：hub 代际校验后移除、registry.Unregister 删键、发布 device.offline 事件、写断连审计。连接建立到工具可见的最坏时延 = 鉴权（毫秒级）+ 工具同步（5 秒超时上限），与 tools/list 30 秒内可见的设计目标（design/32 1.2 访问模式表）一致。

背压与内存上限：入站帧限制 512KB（PoC 已设，保留）；单会话 pending 请求表上限 256 条，超限拒绝新调用（防慢设备拖垮节点）；写侧无缓冲队列（直接写 socket，写超时即断连），从根本上避免无限背压；每会话内存预算（接收缓冲 + pending 表 + 工具缓存）合计不超过 2MB，1 万在线设备单节点内存预算约 20GB 会话数据之外的开销可忽略（假设值，阶段 2 压测校准）。

#### 3.1.5 踢旧连接与代际机制（SEC-15）

hub.Register 时若发现同 tenant+deviceCode 已有会话，先将其标记 stale 并调用其 Close，再登记新会话并分配新 gen；旧会话的 onClose 回调携带自身 gen 与 hub 当前 gen 比对，不一致则不执行 Unregister，防止旧连接断开时误删新会话路由。GetDevice 只返回当前 gen 的会话。

#### 3.1.6 工具注册同步与 risk 元数据（SEC-09/22）

连接建立后 Connector 以 5 秒超时向设备发 tools/list，成功后进入 sync.ToolCatalog.UpsertTools：PG upsert（ON CONFLICT device_id+tool_name DO UPDATE，schema_version 递增判定覆盖，design/32 6.3 幂等）；随后刷新 Valkey 工具缓存与租户工具代际。风险等级合并规则见 3.1.2 注释：DB 为权威，设备上报仅作建议值，DB 无记录且设备未报时缺省 2（先审后用）；is_enabled=false 的工具不出现在任何聚合视图。工具同步失败不拆除会话：重试 3 次退避（1/2/4 秒），仍失败则该设备标记 ERROR 状态并告警，会话保留（仅路由不可用）。

#### 3.1.7 TTL 续期与断连清理（SEC-14）

Valkey 键：adc:loc:{tenant}:{deviceCode} 与 adc:tools:v1:{tenant}:{deviceCode}，TTL 90 秒；心跳 goroutine 每 30 秒执行 registry.Heartbeat 刷新 TTL，同时每 60 秒将 last_heartbeat_at 批量写 PG（SEC-17）。断连路径：registry.Unregister 删除键并 Publish 设备下线事件（Agent API 订阅以失效工具缓存）。会话归属以本进程 hub 为权威，Valkey 只是跨节点索引，两者漂移时以本地会话为准（doc/05 第 8 章风险对策）。

#### 3.1.8 重连协商与退避

服务端在鉴权失败或连接被踢时发送 Close 帧，帧文本携带重连提示（retry_after_secs 字段，core-sdk wire 定义，GAP-06 版本协商）；SDK 侧按 1/2/4/8/16/30/60 秒上限指数退避并加随机抖动，收到服务端提示时以提示值为准。V1.0 服务端只做提示下发，退避策略在 SDK 实现。

#### 3.1.9 错误与重试策略

鉴权失败不建立连接（连接前拒绝，带原因码）；对设备下行工具调用不自动重发（设备指令幂等性不可假设，重试决策留给 Agent 侧，V1.0 不做自动重试）；SendRPC 写失败视为断连并触发清理链路；Valkey 注册失败重试 3 次，仍失败则拒绝该连接（宁可设备暂不可路由，不产生幽灵路由）。

### 3.2 Agent API

#### 3.2.1 内部包结构

```
ce/internal/agentapi/
├── server/server.go        # MCP 风格端点（design/33）、中间件链装配
├── mw/auth.go              # API Key 校验（SEC-02）
├── mw/ratelimit.go         # token bucket 限流（SEC-12）
├── aggregate/aggregate.go  # 工具聚合、命名空间解析、缓存一致性
├── route/router.go         # 调用路由：本节点直连 / MessageBus 跨节点（SEC-06）
├── risk/policy.go          # RiskPolicy seam（V1.0 DB 实现）
├── hitl/client.go          # 审批服务客户端：创建工单 + 唤醒订阅等待
└── task/task.go            # 长任务 seam（ADR-10 预留）
```

#### 3.2.2 关键类型与接口签名

```go
// mw/auth.go — tenant comes from the credential record, never from request headers (SEC-02)
type ApiKeyValidator interface {
    Validate(ctx context.Context, token string) (*Principal, error)
}
type Principal struct {
    KeyID, TenantID string
    Scopes   []string   // reserved: tools.list / tools.call
    AgentID  string     // real subject for audit (SEC-21)
}
```

```go
// risk/policy.go — seam frozen for OPA (ADR-07); V1.0 reads DB risk_level (SEC-09)
type RiskPolicy interface {
    Decide(ctx context.Context, tenantID string, ref ToolRef) (Decision, error)
}
type Decision struct {
    Level           int  // 0 read / 1 low / 2 high / 3 critical
    RequireApproval bool
    Reason          string
}
```

```go
// aggregate/aggregate.go — tenant-scoped namespace; cache is an optimization, DB is truth
type ToolAggregator interface {
    List(ctx context.Context, tenantID string) ([]sdkproto.MCPTool, error)
    Resolve(ctx context.Context, tenantID, qualifiedName string) (ToolRef, error)
}
type ToolRef struct {
    DeviceID, ToolName string
    RiskLevel  int
    LongRunning bool
}
```

```go
// route/router.go — local fast path first; cross-node via bus with node signature (SEC-06)
type ToolRouter interface {
    Call(ctx context.Context, tenantID string, ref ToolRef, args map[string]any) (*CallResult, error)
}
type CallResult struct {
    Content  []sdkproto.ToolContent
    IsError  bool
    Accepted bool   // long task: device returned task_id (ADR-10 seam)
    TaskID   string
}
```

```go
// hitl/client.go — Approval Service owns the state machine; this client only creates and waits
type HITLClient interface {
    CreateTicket(ctx context.Context, req *TicketRequest) (*TicketRef, error)
    AwaitDecision(ctx context.Context, ticketID string, timeout time.Duration) (*TicketDecision, error)
}
```

```go
// ce/pkg/clusterbus/bus.go — abstraction so NATS JetStream replaces the V1 impl (ADR-02)
type MessageBus interface {
    Request(ctx context.Context, targetNode, topic string, payload []byte, timeout time.Duration) ([]byte, error)
    Publish(ctx context.Context, topic string, payload []byte) error
    Subscribe(ctx context.Context, topic string, fn func(payload []byte)) error
}
```

```go
// ce/pkg/audit/sink.go — at-least-once audit path (SEC-07); V1 transport = Valkey list
type AuditSink interface {
    Emit(ctx context.Context, ev *Event) error // RPUSH adc:audit:events; consumer batch-inserts PG
}
type Event struct {
    EventID, EventType, ActorType, ActorID, TenantID, Status string
    RequestID string   // idempotency key, mirrors adc_audit_logs.request_id (design/32 3.10)
    // ... fields mirroring design/32 adc_audit_logs columns
}
```

```go
// mw/ratelimit.go — per-tenant / per-agent / per-device token buckets (SEC-12)
type RateLimiter interface {
    Allow(ctx context.Context, dim Dimension, key string) (ok bool, retryAfter time.Duration)
}
```

#### 3.2.3 API Key 校验中间件（SEC-02）

请求带 Authorization: Bearer adc_live_<随机串>；校验流程：前缀匹配（防全表哈希）→ SHA-256 哈希 → 唯一索引命中 adc_agent_api_keys（design/32 3.7）→ 校验未吊销（revoked_at 空）、未过期、所属租户 ACTIVE → 组装 Principal（tenant 取自记录）。命中结果进 Valkey 短缓存 30 秒（design/32 4 章索引设计），吊销时主动失效。中间件顺序：recover → traceID → 限流 → 认证 → 租户注入。任何请求头中的租户声明一律忽略（GAP-13）。

scope 执行点：ALLOW_LIST 模式仅放行 scopes 内工具，DENY_LIST 模式拦截 scopes 内工具；校验在 aggregate.Resolve 阶段执行（工具维度），与租户维度（数据隔离）双重强制；scopes 为空时 ALLOW_LIST 拒绝全部工具调用、DENY_LIST 放行全部，语义在 design/33 契约中冻结。

#### 3.2.4 工具聚合与缓存一致性

命名空间：qualifiedName 采用结构化解析（deviceCode::toolName，内部立即拆为 ToolRef 结构，禁止拼接字符串路由，修复 SEC-24 的 __ 分隔符缺陷；设备码命名规则禁止包含 ::）。聚合数据源：在线设备集合（Valkey Set）与工具元数据（Valkey 缓存，miss 回源 PG 按租户全量查询）；本地 5 秒短缓存 + 租户工具代际键（adc:tools:gen:{tenant}，任何工具同步、风险等级变更、设备上下线均 bump）做版本校验，代际变化则重建视图。一致性底线：Valkey 缓存全丢仍正确（回源 PG 重建），工具视图最终一致窗口不超过 5 秒。

#### 3.2.5 调用路由（SEC-06）

RouteToolCall 流程：解析 ToolRef → 校验租户归属与 is_enabled → 查 adc:loc:{tenant}:{deviceCode} 得目标节点 → 本节点则走本地执行器（hub 直连会话 SendRPC，快速路径不经过消息层，消息层故障不影响本节点设备，doc/05 第 8 章）；跨节点则经 MessageBus.Request 发送到 adc:req:{node}，消息携带节点签名（HMAC-SHA256 节点密钥，收方验签，SEC-06），等待 adc:resp:{from} 响应。等待超时见 3.2.6。消息层无响应不重发（设备指令不自动重试），返回 504 由 Agent 决定重试。

#### 3.2.6 超时与长任务预留（SEC-16）

调用超时从 PoC 硬编码 15 秒改为配置项 ADC_TOOL_CALL_TIMEOUT（默认 15 秒）；工具元数据 long_running=true 或设备在超时前返回 accepted+task_id 时，调用以 202 返回任务标识（design/33 契约），任务查询走 TaskStore seam（V1.0 内存实现仅支持查询最近任务并审计留痕，完整异步任务模型随 ADR-10 阶段 2 落地）。对未申报长任务但超过超时的调用按失败处理（审计 status=failed 并标注超时）。

#### 3.2.7 限流（SEC-12）

RateLimiter 三维度 token bucket：租户级（默认 200 req/s，桶容量 2 倍）、Agent 级（默认 50 req/s）、设备级（默认 10 req/s）；进程内 bucket 为主（免 Valkey 往返），V1.0 不做跨实例全局精确限流，配额总量由 Admin API 配额账本约束（design/32 6.2：Valkey 计数器 + 每分钟批量聚合 PG）。超限返回 429 并携带 Retry-After，审计 quota_event。限流参数进配置项（ADC_RATELIMIT_*）。

#### 3.2.8 并发模型与错误策略

标准 net/http 并发模型；跨节点等待用 pending map + mutex（key=requestID），超时与 ctx 取消均清理条目防泄漏；聚合本地缓存用 sync.Map + singleflight 防击穿。错误策略：认证失败 401 不重试；路由目标节点无响应 504（可重试，标记 Retryable）；工具不存在/禁用 404；限流 429。所有调用结果（含失败）均经 AuditSink 异步留痕。

内部哨兵错误到 HTTP 的映射（对外错误码细节以 design/33 为准，本表定内部语义）：

| 哨兵错误 | HTTP | 重试语义 | 说明 |
|---|---|---|---|
| ErrInvalidKey / ErrKeyRevoked / ErrKeyExpired | 401 | 不可重试 | SEC-02 |
| ErrRateLimited | 429 | 可重试（按 Retry-After） | SEC-12 |
| ErrToolNotFound / ErrToolDisabled | 404 | 不可重试 | 命名空间或 scope 拦截 |
| ErrDeviceOffline | 404 | 不可重试 | 路由索引未命中（SEC-14 续期保证误判率低） |
| ErrBlockedByHITL | 403 | 不可重试 | 拒绝/过期/取消统一语义 |
| ErrNodeTimeout | 504 | 可重试 | 目标节点无响应，调用方决定是否重发 |
| ErrTicketDedup | 200（返回已有工单） | 幂等 | params_hash 冲突即复用（design/32 6.3） |

### 3.3 Approval Service

#### 3.3.1 内部包结构

```
ce/internal/approval/
├── server/server.go     # /v1/hitl/action 回调（验签 + OAuth）；内部 POST 创建工单（design/33）
├── ticket/ticket.go     # 工单聚合根、状态机迁移
├── ticket/repo.go       # TicketRepo：PG 乐观锁 CAS
├── notify/notifier.go   # TicketNotifier + 重试/降级（SEC-18）
├── wake/wake.go         # TicketEventBus：Valkey 统一唤醒通道（SEC-10）
└── scan/scanner.go      # 超时扫描器（SEC-11 过期兜底）
```

#### 3.3.2 关键类型与接口签名

```go
// ticket/repo.go — PG is the source of truth (ADR-05, SEC-08)
type TicketRepo interface {
    Create(ctx context.Context, t *Ticket) (*Ticket, error) // dedup via params_hash partial unique index
    Transition(ctx context.Context, ticketID string, wantVersion int, cmd TransitionCmd) (*Ticket, error)
    Get(ctx context.Context, ticketID string) (*Ticket, error)
    FindExpired(ctx context.Context, now time.Time, limit int) ([]Ticket, error) // FOR UPDATE SKIP LOCKED
}
type TransitionCmd struct {
    Status   Status
    ApproverID, ApproverName, Comment string
}
```

```go
// notify/notifier.go — channel seam; retry wrapper adds 3-attempt backoff + fallback (SEC-18)
type TicketNotifier interface {
    SendApprovalCard(ctx context.Context, t *Ticket) error
}
// notify/retry.go: attempts 1s/2s/4s, then fallback notifier, then RETRY_EXHAUSTED record
// in adc_notifications (design/32 3.12) + alert audit event. Fail-safe: ticket still expires.
```

```go
// wake/wake.go — unified resolve channel fixes the SEC-10 orphan-channel gap
type TicketEventBus interface {
    PublishResolved(ctx context.Context, ticketID string, status Status) error
    SubscribeResolved(ctx context.Context, fn func(ticketID string, status Status)) error
}
```

```go
// scan/scanner.go — ticker driven; single-instance lock via Valkey SET NX
type TicketScanner interface {
    Run(ctx context.Context) error
    ScanOnce(ctx context.Context) (expired int, err error)
}
```

#### 3.3.3 工单状态机（PG 主存 + 乐观锁）

迁移实现与 design/32 6.1 的三层防线完全一致：单语句条件原子更新（WHERE status='PENDING' AND expires_at>now()）、version 乐观锁、在途工单 params_hash 部分唯一索引防重。受影响行数为 0 时区分"已处理"与"已过期"并返回对应语义（design/33 错误码）。工单创建在 Create 内完成：INSERT 工单 + INSERT 审计事件同事务（"有决策必有留痕"的同事务原则同样适用于创建），回调签名（callback_signature 列）随工单生成。

#### 3.3.4 回调验签与一次性消费（SEC-01/13）

回调 URL：/v1/hitl/action?ticket_no=&decision=&ts=&sig=&approver=；sig = HMAC-SHA256(租户回调 secret, ticket_no + "\n" + decision + "\n" + ts + "\n" + approver)，时间窗 300 秒；approver 身份由企微/钉钉 OAuth 回调解析并与签名内 approver 一致校验，不一致即 403（SEC-01 真实主体）；一次性消费由状态机 CAS 天然保证，重复回调渲染"已处理"页面并审计。回调 secret 经环境变量注入、按租户隔离（SEC-13）。

#### 3.3.5 通知器（重试 3 次 + 降级，SEC-18）

TicketNotifier 为渠道 seam（V1.0 实现企微/钉钉卡片，卡片文案走 i18n 词条，GAP-16；按钮 URL 携带签名参数，SEC-13）；retry 包装器：3 次退避重试（1/2/4 秒），每次投递与最终状态写入 adc_notifications（design/32 3.12 的 retry_count/status 枚举）；全部失败后降级到 fallback 渠道（V1.0 实现为站内告警审计事件 + 管理控制台待办，短信/电话渠道为 ee/ seam 预留），并产生可告警的 RETRY_EXHAUSTED 事件。sendJSON 必须校验 HTTP 状态码（PoC 缺陷）。

通知时序：工单创建事务提交 → 通知投递入有界队列（不阻塞创建接口）→ worker 池发送卡片 → 按 retry_count 退避重试 → 记录 adc_notifications 终态。卡片送达时限目标为工单创建后 30 秒内（design/32 1.2 访问模式表）；通知失败不改变工单状态机走向（决策以回调与状态机为准，通知只是触达手段），工单超时兜底保证 fail-safe 语义。

#### 3.3.6 唤醒通道（SEC-10）

决策状态机迁移提交后，Publish 统一通道 adc:hitl:resolve（载荷 ticket_id + status）；所有 Agent API 实例常驻 SubscribeResolved，收到后按 ticketID 查本地等待者 map 路由唤醒（本进程等待者无则丢弃）。统一通道订阅取代 PoC 的 adc:hitl:resolve:<ticketID> 无订阅者缺陷，跨节点审批秒级唤醒。

#### 3.3.7 超时扫描器

每 60 秒 ScanOnce：按 idx_tickets_pending_expire 部分索引取过期 PENDING 工单（LIMIT 100，FOR UPDATE SKIP LOCKED），逐个 CAS 置 EXPIRED，唤醒等待方并审计；扫描器用 Valkey SET NX EX 50 锁保证多实例单执行者；扫描失败不吞错（告警 + 下轮重试，EXPIRED 落库可容忍分钟级延迟，等待方超时兜底）。

#### 3.3.8 并发模型与错误策略

HTTP 回调处理并发由状态机 CAS 串行化；通知发送走有界 worker 池（channel 队列，避免回调线程阻塞）；订阅 goroutine 单实例一条。错误策略：Transition 冲突（0 行）返回"已处理"非错误重试；PG 瞬断重试 3 次（Retryable）；通知失败不阻塞状态机（通知是尽力而为，决策以状态机为准）。

### 3.4 Admin API

#### 3.4.1 内部包结构

```
ce/internal/adminapi/
├── server/server.go     # 管理端点（design/33）：路由 + 管理员认证中间件
├── mw/auth.go           # 管理员认证：V1.0 管理员 API Key；seam AdminAuthenticator（ee/ SSO）
├── tenant/ device/ apikey/ quota/ auditq/ toolpolicy/   # 各聚合仓储与用例
└── pkg 复用：pkg/audit、pkg/valkey、pkg/slogx
```

#### 3.4.2 关键类型与接口签名

```go
type TenantRepo interface {
    Create(ctx context.Context, t *Tenant) error
    Get(ctx context.Context, tenantID string) (*Tenant, error)
    Update(ctx context.Context, tenantID string, f func(*Tenant) error) (*Tenant, error) // version-guarded
    List(ctx context.Context, p Page) ([]Tenant, error)
    Suspend(ctx context.Context, tenantID string) error // SUSPENDED: kick sessions + void PENDING tickets
}
type DeviceRepo interface {
    Register(ctx context.Context, d *Device) error      // device_code unique; used_devices quota CAS (design/32 6.2)
    Get(ctx context.Context, deviceCode string) (*Device, error)
    UpdateStatus(ctx context.Context, deviceCode string, status Status, reason string) error // FROZEN/RETIRED
    ResetCredential(ctx context.Context, deviceCode string) (newSecret string, err error)     // credential_version++
    List(ctx context.Context, filter DeviceFilter, p Page) ([]Device, error)
}
type ApiKeyRepo interface {
    Issue(ctx context.Context, spec *KeySpec) (plainKey string, err error) // plaintext returned once, SHA-256 stored
    Revoke(ctx context.Context, keyID string, reason string) error
    List(ctx context.Context, tenantID string, p Page) ([]ApiKeyInfo, error)
}
type QuotaRepo interface {
    Set(ctx context.Context, tenantID string, q Quota) error      // quota_devices/calls_monthly/concurrent
    Get(ctx context.Context, tenantID string) (*Quota, error)
}
type AuditQueryRepo interface {
    Query(ctx context.Context, f AuditFilter, cursor PageCursor, limit int) (*AuditPage, error) // keyset pagination
    Export(ctx context.Context, f AuditFilter, w io.Writer) error // streaming CSV, max 100k rows/run
}
```

#### 3.4.3 CRUD 与配额

租户 CRUD 与 SUSPENDED 语义（踢线、作废在途工单，design/32 3.1 注释与 6.2）；设备台账登记（设备码全局唯一永不复用，FR-011）、状态迁移（FROZEN/RETIRED）、凭证重置（credential_version 递增，旧凭证在过渡窗口失效）；API Key 签发（明文仅返回一次，SHA-256 入库，吊销置 revoked_at 并失效 Valkey 缓存）；配额配置与读取，执行侧扣减逻辑按 design/32 6.2（条件原子 UPDATE + Valkey 热计数器）。

配额执行闭环：Admin API 是配额配置入口（adc_tenants.quota_* 为权威账本），Agent API 是执行点（设备注册走 Admin API 的条件原子 UPDATE；调用量走 Valkey 热计数 + 每分钟批量聚合回 PG used_calls_month，超限由计数判断返回 429 或配额不足错误）；两条路径不一致时以 PG 账本为准，Valkey 计数可重建（月度从 adc_usage_events 聚合对账，差异超阈值告警，design/32 6.2 对账机制）。

#### 3.4.4 工具风险等级变更（二次确认 + 审计）

变更接口要求 confirm=true + 必填 reason；事务内 UPDATE risk_level 并写 risk_changed_by（design/32 3.6）；提交后 bump 租户工具代际（聚合缓存即时失效）；写 admin_op 审计（主体为管理员、含变更前后值与理由）。等级下调与上调同等审计强度（FR-006 异常场景）。

#### 3.4.5 审计查询与导出

查询必带时间范围（分区裁剪），过滤维度：租户、设备、工具、事件类型、状态；keyset 游标分页（(created_at, id) 组合游标，稳定不偏移）；导出为流式 CSV（逐批 SELECT 写入响应流，单次上限 10 万行，FR-013）；查询与导出动作本身写 admin_op 审计。

#### 3.4.6 并发模型

标准 net/http；无后台常驻 goroutine（审计消费器由 pkg/audit 内建 worker 承载，Admin API 为 V1.0 单实例部署形态的默认消费者之一）；写操作全部走短事务（单表条件更新或单聚合事务），不持有长事务。

### 3.5 统一 API 网关（Go，方案 A 新增）

#### 3.5.1 定位与职责边界

统一 API 网关是前端与外部客户端的唯一入口（单地址接入），内部四服务与 Python Agent 面均不对外暴露端口（design/60 互信网络：服务间地址仅内网域名解析）；服务间调用（如 Agent API 到 Approval Service 的同步 HTTP）仍在内网直连，不回环经过网关。职责：

1. 路径前缀路由：`/v1/admin/*` → Admin API、`/v1/agent/*` → Agent API、`/v1/devices/tunnel`（WSS）→ Device Connector、`/v1/hitl/*` → Approval Service、`/v2/agents/*` → Python Agent 面（V1.0 仅评测子集可用，见 3.6.1）。
2. 统一 TLS 终结（SEC-05）：证书与私钥仅在网关挂载，后端在互信网络内经内部地址接收转发流量。
3. 会话校验透传：对入站请求仅做签名与格式校验并原样透传，不解析业务 token 语义——租户与角色判定仍由后端服务按 design/33 1.2 认证总表执行，网关不成为第二个租户判定点（GAP-13 同源原则）。
4. 网关级限流与契约校验：限流复用 ce/pkg/ratelimit（SEC-12 同源实现）；契约校验为 OpenAPI 入站校验（方法与路径、必填参数、请求体 JSON Schema），业务语义校验仍由后端执行。
5. trace_id 透传：X-Trace-ID 与 X-ADC-Request-ID 全链透传与补全（1.3.5 上下文规范、design/33 1.9）。

边界：网关是纯转发与接入层，不承载业务逻辑、不持有业务状态、不产生业务审计事件（业务审计仍由各服务按 SEC-07 产生）；网关取代 design/60 中 nginx 边缘反代的路由与 TLS 终结职责，design/60 的部署编排随 OpenSpec 变更同步修订（不在本说明范围）。原 PoC 的 gateway/handler.go 单体业务网关保持废弃拆分（第 5 章），与本网关无代码承接关系。

#### 3.5.2 内部包结构

```
ce/internal/gateway/
├── server/server.go        # 路由表装配、监听、优雅退出（与四服务同一 run 骨架）
├── router/router.go        # 路径前缀路由注册与匹配（I21 GatewayRouter）
├── proxy/proxy.go          # httputil.ReverseProxy 封装：共享 Transport、WSS 升级、错误映射
├── mw/trace.go             # trace_id 生成与透传
├── mw/auth.go              # 会话校验透传（仅验签不解析业务 token）
├── mw/ratelimit.go         # 网关级限流：复用 ce/pkg/ratelimit（SEC-12）
├── mw/contract.go          # OpenAPI 入站契约校验
└── client/agentplane.go    # I22 AgentPlaneClient：Go 侧调用 Python 面内部接口（3.6.6）
```

#### 3.5.3 关键类型与接口签名

```go
// router.go — path-prefix routing table (I21 GatewayRouter)
type Route struct {
    Prefix   string // /v1/admin、/v1/agent、/v1/devices/tunnel、/v1/hitl、/v2/agents
    Backend  string // internal URL (mutual-trust network only, e.g. http://agent-api:8080)
    WSS      bool   // /v1/devices/tunnel: websocket upgrade must pass through
    Contract string // OpenAPI document path for inbound validation (single contract source)
}

type GatewayRouter interface {
    Register(r Route) error // fail-fast on conflicting prefixes
    Handler() http.Handler  // assembled single entry handler
}
```

```go
// proxy.go — unified httputil.ReverseProxy with a shared transport
func NewProxy(prefix string, backend *url.URL, tr http.RoundTripper) *httputil.ReverseProxy {
    return &httputil.ReverseProxy{
        Rewrite: func(pr *httputil.ProxyRequest) {
            pr.SetURL(backend)
            pr.Out.Header.Set("X-Forwarded-Proto", "https") // TLS terminated at gateway
        },
        Transport:     tr,      // shared http.Transport: conn pool, TLS client cert, dial timeout
        FlushInterval: -1,      // flush immediately: WSS upgrade + streaming pass through
        ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
            // upstream unreachable -> 502, timeout -> 504; echo X-ADC-Request-ID
        },
    }
}
```

```go
// mw/auth.go — pass-through session verification: verify signature only, never parse business tokens
type SessionVerifier interface {
    Verify(ctx context.Context, r *http.Request) error
    // signature + freshness check only; tenant/role decisions stay in backend services
    // (design/33 1.2 auth matrix; the gateway must not become a second tenant authority)
}
```

```go
// mw/ratelimit.go — gateway-level limiting reuses ce/pkg/ratelimit (SEC-12):
// entry-level bucket (global, anti-DDoS) + per-prefix bucket; business-dimension
// (tenant/agent/device) limiting remains inside the backend services (3.2.7).
```

```go
// mw/trace.go — trace_id propagation across the whole chain (1.3.5)
func TraceID(next http.Handler) http.Handler {
    // read X-Trace-ID, else generate UUID; inject into ctx and copy header downstream;
    // X-ADC-Request-ID (design/33 1.9) passes through unchanged on both directions
}
```

#### 3.5.4 转发与协议边界

- 常规 HTTP：ReverseProxy 转发；Host 头改写为后端内部域名；下游响应透传 X-ADC-Request-ID 与 X-ADC-Version（design/33 1.9）。
- WSS（/v1/devices/tunnel）：路由表 WSS=true 时启用升级透传（FlushInterval=-1 且共享 Transport 支持 101 升级），TLS 在网关终结；Origin 白名单校验仍在 Connector 侧执行（SEC-04），网关只透传必要头不重复判定。
- 契约校验：按路由表 Contract 声明对入站请求做 OpenAPI 校验（方法、路径、必填参数、请求体 JSON Schema），失败返回 400（code 10001）不进入后端；响应不做校验（响应契约由后端实现保证，design/33 为契约源）。
- 超时：网关对后端连接建立与响应空闲分别设超时（配置项 ADC_GW_UPSTREAM_TIMEOUT，默认 30 秒）；设备调用链路超时仍由 Agent API 按 ADC_TOOL_CALL_TIMEOUT 控制（SEC-16），网关超时不得小于该值。

#### 3.5.5 前端无感切换要点

1. 网关地址唯一：控制台与外部客户端只配置一个基址（https://<edge-domain>/），面与面由路径前缀区分；服务拆分、新增面（如 /v2/agents）只改网关路由表与后端地址，前端与既有调用方零改动。
2. OpenAPI 契约单一来源：design/33 契约文件为唯一契约源；前端请求层类型（TS 类型生成）与网关入站校验消费同一契约产物，新增端点只改契约一处、两端同步生效；契约产物入库版本化，随发布冻结。
3. 会话与凭证透传：Admin 会话 Cookie、Agent API Key 请求头原样透传，网关不存储业务会话状态，无状态水平扩展（与四服务共享 1.3.7 优雅停机骨架）。
4. 路径隔离：/v1/devices/tunnel 仅面向设备 SDK，控制台不访问该路径；/v2/agents 前缀 V1.0 即注册（路由表预留），V1.0 内除评测子集外返回 503（未启用），避免后续前端改动。

#### 3.5.6 并发模型、错误与测试要点

- 并发模型：标准 net/http + 共享 Transport 连接池；网关无业务状态、实例间无协调，水平扩展即插即用（design/60 无状态服务原则同款）。
- 错误策略：未注册前缀 404；契约校验失败 400（10001）；网关级限流 429 带 Retry-After；上游不可达 502、上游超时 504（均携带 X-ADC-Request-ID 可检索）；网关错误一律写接入层日志（1.3.5 字段），不产生业务审计事件。
- 测试要点：路由表冲突注册 fail-fast；WSS 升级透传（httptest + 自定义 dialer）；契约校验命中与放行；trace_id 跨三跳（网关到服务到审计消费）一致；网关限流 429 不拖垮后端；上游故障注入 502/504 映射。

### 3.6 Python Agent 面（ce/py-agent，方案 A 新增）

#### 3.6.1 定位与版本边界

Python Agent 面承载 Agent/AI 生态能力（design/70 2.2 结论：Agent 面 Python 生态占优：官方 MCP Python SDK、LangGraph、A2A 官方 SDK），经统一网关 /v2/agents 前缀对外，是方案 A 中唯一新增的 Python 运行面。职责：评测工具链（V1.0）、LLM 网关编排与内部多 Agent 编排、A2A Agent Card（V1.5）。

版本边界：

| 版本 | 交付内容 | 运行形态 |
|---|---|---|
| V1.0 最小集 | 评测工具链（CLI + 可选 HTTP API）、开发脚本（mock 设备、回归脚本）、core-sdk Python 包（协议类型） | 服务骨架随部署起停；/v2/agents 仅评测子集启用，其余端点 503 |
| V1.5 完整集 | LLM 网关编排（统一模型接入）、内部 Agent 编排（doc/07 四 Agent：Planner/设备选择/执行/HITL 审批）、A2A Agent Card 发布 | 全量启用；Agent Card 仅"发布并接受外部编排委派"，完整 A2A 网关编排链路随阶段 3（ADR-16/17，V2.0） |

边界规则：Python 面不重写数据面逻辑——设备接入、设备鉴权、工具聚合、HITL 工单状态机、审计落库仍在 Go 侧（3.1~3.4）；Python 面经统一网关与 I22 客户端协作，不直连 PG（3.6.5）；MCP 管工具、A2A 管协作的职责划分与 doc/07 一致。

#### 3.6.2 项目结构

```
ce/py-agent/
├── pyproject.toml          # uv 管理：项目内 .venv，声明式依赖
├── uv.lock                 # 全量锁定（含传递依赖），CI 与离线安装同源
├── src/adc_agent/
│   ├── main.py             # FastAPI 应用装配（uvicorn 入口，healthz/readyz 与 Go 服务对齐）
│   ├── api/routes.py       # /v2/agents/* 路由（V1.0 仅评测子集）
│   ├── eval/               # 评测工具链：用例加载、执行器、报告生成
│   ├── mcp/                # 官方 MCP Python SDK 集成（FastMCP，Streamable HTTP 预留）
│   ├── core/models.py      # Pydantic 模型：JSON Schema 契约来源
│   └── orchestration/      # V1.5：Agent 编排（LangGraph seam，V1.0 不实现）
├── scripts/                # 开发脚本：评测 CLI、数据初始化、回归
└── tests/                  # pytest；契约一致性测试（双语言 diff）门禁
```

工程约束：依赖一律经 uv 管理（uv.lock 提交入库、CI 离线缓存 + --offline 安装，对齐 design/70 3 章与 design/60 离线交付）；运行环境 Python 3.14；第三方库选型须过 doc/05 7.1 许可证审计（与 Go 侧同门禁）；官方 MCP Python SDK 复用，不自研 MCP 传输栈。

#### 3.6.3 关键类型与契约来源

```python
# core/models.py — Pydantic v2；模型即 JSON Schema 契约来源
from pydantic import BaseModel, Field

class EvalCaseSpec(BaseModel):
    name: str = Field(min_length=1, max_length=64, description="用例名")
    tool: str = Field(description="目标工具 deviceCode::toolName")
    args: dict[str, object] = Field(default_factory=dict)
    expect_status: str = Field(default="ok", pattern="^(ok|blocked|error)$")

class EvalRunSpec(BaseModel):
    suite: str = Field(min_length=1, max_length=128)
    cases: list[EvalCaseSpec] = Field(min_length=1, max_length=500)
    tenant: str | None = Field(default=None, description="空则用本地 mock 租户")

class EvalReport(BaseModel):
    run_id: str
    suite: str
    started_at: str
    total: int
    passed: int
    failed: int
    results: list[dict[str, object]]   # 逐用例结果（含耗时与失败原因）
```

与 core-sdk Python 包的关系：

1. core-sdk 协议双语言：Go 与 Python 各一份实现，wire 协议类型语义一致（design/70 第 5 章仓库结构修订点）；契约由 OpenAPI/JSON Schema 驱动（单一契约源），两语言产物在 CI 对契约做一致性 diff，防止双语言漂移（与 doc/05 修订纪律同源原则一致）。
2. adc_agent 不重新定义 wire 类型：工具 schema、JSON-RPC 报文、Agent Card 结构一律取自 core-sdk Python 包；py-agent 内仅定义应用层模型（如评测规格）。
3. Pydantic 模型 export 的 JSON Schema 纳入契约产物，与 design/33 OpenAPI 源 diff 校验为 CI 门禁。

#### 3.6.4 评测工具链入口（V1.0 最小集）

评测工具链是 V1.0 最小集的主交付，两个入口共用同一 EvalHarness 实现：

```python
# eval/harness.py — I23 EvalHarness：评测执行与报告
class EvalHarness(Protocol):
    def run(self, spec: EvalRunSpec) -> EvalReport: ...   # 执行用例集
    def load_suite(self, name: str) -> EvalRunSpec: ...   # 从 suites/ 目录加载
    def report(self, run_id: str) -> EvalReport: ...      # 最近一次运行结果
```

- CLI 入口：`uv run eval --suite smoke --tool-filter ... --output report.json`（scripts/eval_cli.py），面向开发与回归，离线可用（不依赖平台实例时走 mock 设备）。
- 可选 API 入口：POST /v2/agents/eval/runs 与 GET /v2/agents/eval/runs/{run_id}（经网关前缀路由，V1.0 已注册），面向外部 Agent 开发者验证自身工具调用契约。
- 评测对象：对 Agent API 的 MCP 端点做契约与行为验证（mock 设备链路 + 真实网关链路可选）；执行器内部预留 V1.5 Agent 编排评测的 runner seam（orchestration/ 装配点，V1.0 不实现）。
- 结果去向：V1.0 输出文件/响应体，不落库（见 3.6.5）。

#### 3.6.5 Python 面数据需求清单

- V1.0 最小集：无需新增表。评测运行与报告数据以本地文件/API 响应为主，不引入领域表；对 Go 数据面现有表的读取一律经 Go 侧接口（网关转发公开端点或 I22）完成，Python 面不直连 PG（权限边界与 design/32 最小权限账号原则一致）。
- V1.5 预留：adc_agent_tasks（Agent 编排任务表）。字段描述（不写 DDL；表结构、索引与状态枚举由 design/32 待追加节经 OpenSpec 变更冻结，并与 design/33 端点契约同步）：任务 ID（UUID）、租户 ID、任务类型（agent_run/planner/device_select/execute/hitl）、输入参数（JSONB）、状态枚举（与 doc/07 A2A TaskState 对齐映射）、父任务 ID（层级编排，可空）、关联审批工单 ID（可空）、执行结果摘要（JSONB）、幂等键、创建/更新/完成时间戳。读写归属：Python 面写入、Go 数据面与控制台经接口查询；字段命名遵循 design/32 snake_case 约定，新增字段先入基线（31/32/33 三文同步原则不变）。

#### 3.6.6 Go 侧协作接口（I22）

```go
// ce/internal/gateway/client/agentplane.go — Go-side client into the Python plane (I22)
// internal mutual-trust network; payload types come from the core-sdk Go package
// (JSON-Schema-driven, same single contract source as the Python side)
type AgentPlaneClient interface {
    EvalRun(ctx context.Context, spec *EvalRunSpec) (*EvalReport, error) // V1.0: eval harness
    // V1.5 reserved: CreateAgentTask / QueryAgentTask (adc_agent_tasks,
    // frozen via design/32 pending section + design/33 endpoints)
}
```

- 使用方：Admin API 触发评测、控制台后端查询评测状态等 Go 侧调用点；实现位于 ce/internal/gateway/client（复用网关的互信网络配置与超时策略），经依赖注入装配给各调用方，不产生跨服务 import。
- V1.5 扩展：CreateAgentTask/QueryAgentTask 等编排接口在 adc_agent_tasks 契约冻结后追加，接口演进遵循既有 seam 冻结纪律。

---

## 4. 关键实现骨架

### 4.1 事务边界总览

| 编号 | 位置 | 事务范围 | 一致性保障 |
|---|---|---|---|
| T1 | Connector ToolCatalog.UpsertTools | 单表 upsert（ON CONFLICT） | 幂等，冲突更新以 schema_version 判定 |
| T2 | Approval TicketRepo.Create | INSERT 工单 + INSERT 审计（同库同事务） | 有工单必有留痕 |
| T3 | Approval TicketRepo.Transition | 单语句 CAS（design/32 6.1 SQL） | 行锁串行化 + version 乐观锁，一次性消费 |
| T4 | Admin 风险等级变更 | UPDATE 工具（含 risk_changed_by）+ INSERT 审计 | 同事务；代际 bump 在提交后（Valkey，失败仅影响缓存 5 秒内自愈） |
| T5 | Admin API Key 签发 | INSERT key（哈希唯一索引） | 冲突重试；明文只在内存组装 |
| T6 | Agent API 调用链路 | 无跨库事务 | 审计先入队（RPUSH 成功才视为可执行），消费侧按 request_id 幂等（design/32 6.3），at-least-once |

跨组件一致性说明：业务写（PG）与审计入队（Valkey）非原子，采用"关键路径先审计后业务、非关键路径业务后补偿入队"的至少一次语义，消费侧 request_id 唯一索引去重，审计不丢、可重复无害（backend-expert 幂等原则）。

### 4.2 关键链路骨架

```go
// agentapi app: tools/call orchestration
func (s *Service) CallTool(ctx context.Context, p Principal, qualifiedName string, args map[string]any) (*CallResult, error) {
    ref, err := s.agg.Resolve(ctx, p.TenantID, qualifiedName) // tenant scope + is_enabled enforced here
    if err != nil { return nil, err }
    if ok, ra := s.limiter.Allow(ctx, DimDevice, ref.DeviceCode); !ok { return nil, ErrRateLimited(ra) }
    dec, err := s.risk.Decide(ctx, p.TenantID, ref)
    if err != nil { return nil, err }

    var ticketID string
    if dec.RequireApproval {
        tr, err := s.hitl.CreateTicket(ctx, &TicketRequest{ /* tenant/api_key/device/tool/args */ })
        if err != nil { return nil, err }                                   // dedup conflict returns existing ticket
        decision, err := s.hitl.AwaitDecision(ctx, tr.TicketID, tr.ExpiresIn)
        if err != nil || decision.Status != StatusApproved {
            s.audit.Emit(ctx, blockedEvent(p, ref, dec, decision))          // status=blocked_by_hitl
            return nil, ErrBlockedByHITL(decision)
        }
        ticketID = tr.TicketID
    }

    ctx, cancel := context.WithTimeout(ctx, s.callTimeout)                  // ADC_TOOL_CALL_TIMEOUT, default 15s (SEC-16)
    defer cancel()
    res, err := s.router.Call(ctx, p.TenantID, ref, args)                   // local fast path or bus cross-node
    s.audit.Emit(ctx, callEvent(p, ref, res, err, ticketID))                // async, non-blocking, request_id idempotent
    return res, err
}
```

```go
// approval server: callback handler (SEC-01/13)
func (h *Handler) HandleHITLAction(w http.ResponseWriter, r *http.Request) {
    // 1. parse ticket_no/decision/ts/sig/approver; validate decision enum (design/33)
    // 2. HMAC verify over (ticket_no, decision, ts, approver) with 300s window; mismatch -> 403
    // 3. resolve approver identity via WeCom/DingTalk OAuth; mismatch with sig approver -> 403
    // 4. repo.Transition(CAS); 0 rows -> render "already processed" page + audit retry event (one-shot)
    // 5. bus.PublishResolved(ticketID, status); audit approval_decision (same-transaction guarantee T2/T3)
    // 6. render HTML result page (i18n)
}
```

```go
// agentapi hitl client: cross-node wake (SEC-10)
func (c *client) AwaitDecision(ctx context.Context, ticketID string, timeout time.Duration) (*TicketDecision, error) {
    ch := c.waiters.Add(ticketID)          // local map + mutex
    defer c.waiters.Remove(ticketID)
    select {
    case <-ctx.Done():
        return nil, ctx.Err()              // scanner in Approval Service will expire the ticket
    case d := <-ch:                        // routed from unified adc:hitl:resolve subscription
        return d, nil
    case <-time.After(timeout):
        return nil, ErrTicketExpired
    }
}
```

```go
// approval ticket repo: CAS transition (design/32 6.1)
func (r *pgTicketRepo) Transition(ctx context.Context, id string, wantVersion int, cmd TransitionCmd) (*Ticket, error) {
    const q = `UPDATE adc_approval_tickets
               SET status=$1, approver_id=$2, approver_name=$3, comment=$4,
                   decided_at=now(), version=version+1, updated_at=now()
               WHERE id=$5 AND version=$6 AND status='PENDING' AND expires_at > now()
               RETURNING *`            // transaction boundary: single-statement CAS (SEC-11)
    var t Ticket
    if err := r.q.QueryRow(ctx, q, cmd.Status, cmd.ApproverID, cmd.ApproverName, cmd.Comment, id, wantVersion).Scan(...); err != nil {
        if errors.Is(err, pgx.ErrNoRows) { return nil, ErrConflictOrExpired } // one-shot consume
        return nil, err
    }
    return &t, nil
}
```

```go
// ce/pkg/audit consumer worker: batch drain Valkey list into adc_audit_logs
func (w *Consumer) Run(ctx context.Context) error {
    for {
        select {
        case <-ctx.Done(): return nil
        default:
        }
        batch, err := w.kv.BLMove(ctx, "adc:audit:events", "adc:audit:dead", time.Second, "RIGHT", "LEFT") // RPOPLPUSH guard
        if err != nil { continue }                                // transient: retry next loop
        if err := w.pg.InsertBatch(ctx, events(batch)); err != nil {
            continue                                              // item sits in dead list; requeue worker drains it later
        }
        w.kv.LRem(ctx, "adc:audit:dead", 0, batch)                // commit point: remove from dead after PG success
    }
}
```

```go
// shared graceful shutdown sequence (all four services)
func run(ctx context.Context, srv *http.Server, bg *errgroup.Group) error {
    // 1. stop accepting new connections and requests (server.Shutdown with 5s drain)
    // 2. cancel root context -> sessions and waiters see ctx.Done; doneChan broadcasts
    // 3. bg.Wait() with 10s cap (group limit), then close Valkey/PG pools
    // 4. audit queue is best-effort at this point: Emit happened before business commit (T6)
    return nil
}
```

### 4.3 核心接口索引（共 23 个）

| 编号 | 接口 | 所在模块 | 职责 | seam |
|---|---|---|---|---|
| I1 | DeviceAuthenticator | 3.1 | 设备 HMAC+nonce 鉴权 | 否 |
| I2 | SessionRegistry | 3.1 | Valkey 路由索引与 TTL | 否 |
| I3 | ToolCatalog | 3.1 | 工具同步与 risk 权威合并 | 否 |
| I4 | ApiKeyValidator | 3.2 | Agent API Key 校验 | 否 |
| I5 | ToolAggregator | 3.2 | 工具聚合与命名空间 | 否 |
| I6 | ToolRouter | 3.2 | 调用路由 | 否 |
| I7 | RiskPolicy | 3.2 | 风险判定 | 是（阶段 2 OPA） |
| I8 | HITLClient | 3.2 | 工单创建与等待决策 | 否 |
| I9 | RateLimiter | 3.2 | 三维度限流 | 否 |
| I10 | AuditSink | ce/pkg/audit | 审计事件入队 | 否 |
| I11 | TicketRepo | 3.3 | 工单状态机仓储 | 否 |
| I12 | TicketNotifier | 3.3 | 审批通知渠道 | 是（EE 多级/新渠道） |
| I13 | TicketEventBus | 3.3 | 决策唤醒通道 | 否 |
| I14 | TicketScanner | 3.3 | 超时扫描 | 否 |
| I15 | TenantRepo | 3.4 | 租户与配额账本 | 否 |
| I16 | DeviceRepo | 3.4 | 设备台账 | 否 |
| I17 | ApiKeyRepo | 3.4 | API Key 签发吊销 | 否 |
| I18 | QuotaRepo | 3.4 | 配额配置 | 否 |
| I19 | AuditQueryRepo | 3.4 | 审计查询导出 | 否 |
| I20 | MessageBus | ce/pkg/clusterbus | 跨节点消息抽象 | 是（阶段 2 NATS） |
| I21 | GatewayRouter | 3.5 | 网关路由注册与统一转发（前缀路由/透传/契约校验） | 否 |
| I22 | AgentPlaneClient | 3.6 | Go 侧调用 Python 面内部接口（评测，V1.5 任务接口预留） | 否 |
| I23 | EvalHarness | 3.6 | 评测工具链执行与报告（Python） | 否 |

---

## 5. 原 PoC 代码改造点清单

逐包对照（引用 doc/Agentic Device Cloud.md 章节行号；处置分保留、改造、废弃三类）：

| 原包/文件 | 原文档位置 | 处置 | 去向与改造要点 | 关联编号 |
|---|---|---|---|---|
| protocol/mcp.go | 4.1 节 130-184 行 | 保留并改造 | 迁 core-sdk/；JSONRPC 报文保留；MCPTool 增加 risk_level/schema_version/long_running 字段；新增 Handshake 与重连提示报文 | SEC-09/16/22、GAP-04/06 |
| protocol/cluster.go | 5.1 节 536-558 行 | 保留并改造 | 迁 core-sdk/；ClusterCallRequest 增加 NodeSignature 与协议版本字段；ID 格式校验 | SEC-06/20 |
| auth/authenticator.go | 4.2 节 188-257 行 | 废弃重写 | 内存 map 与固定 HMAC 废弃；改为 PG 加载 + HMAC(deviceCode+nonce+ts) + nonce 一次性 + 轮转吊销；静态 token 路径删除 | SEC-03/25 |
| hub/session.go | 4.3 节 261-434 行 | 保留骨架并改造 | 保留 doneChan/closeOnce/写锁/pending 机制；增加代际字段、心跳续期挂钩、slog 与优雅退出、背压处理 | SEC-14/15、GAP-11 |
| hub/device_hub.go | 4.4 节 438-500 行 | 保留并改造 | Register 踢旧连接、Unregister 代际校验；key 改为 tenant+deviceCode | SEC-15/20 |
| cluster/manager.go | 5.2 节 562-777 行 | 拆分改造 | RegisterDevice/Unregister 迁 Connector registry（TTL 续期）；RouteToolCall 迁 Agent API router（MessageBus 抽象 + 节点签名）；GetTenantAggregatedTools 迁 aggregate（结构化命名空间、gen 缓存）；本地执行器直连保留为快速路径 | SEC-06/14/20/24、GAP-11 |
| hitl/models.go | 6.1 节 817-857 行 | 改造 | ApprovalTicket 迁 PG 聚合根（对齐 design/32 adc_approval_tickets，增加 version/decided_at 等）；RiskLevel 常量保留 | SEC-08/11 |
| hitl/manager.go | 6.2 节 861-1009 行 | 废弃重写 | 历史 Redis 主存方案禁止采用（SEC-08）；读改写竞态废弃（CAS）；等待逻辑迁 Agent API hitl client；Valkey 唤醒通道统一订阅（SEC-10）；过期校验入 CAS 守卫（SEC-11） | SEC-08/10/11 |
| hitl/notifiers.go | 6.3 节 1013-1125 行 | 改造 | 卡片结构保留；按钮 URL 加签名（SEC-13）；sendJSON 校验状态码 + 3 次退避重试 + 降级 + adc_notifications 记录（SEC-18）；文案 i18n、清除 emoji | SEC-13/18、GAP-16 |
| gateway/handler.go | 7.1 节 1131-1355 行 | 废弃拆分 | 四端点分属四服务；CheckOrigin 白名单（SEC-04）；isHighRiskOperation 废弃改 DB risk_level（SEC-09）；硬编码 agent_id/approver 删除（SEC-21）；15 秒硬超时可配置（SEC-16）。注：方案 A 新增的统一 API 网关（3.5）为全新边界组件，与 PoC 单体网关无代码承接关系，不恢复本包业务逻辑 | SEC-01/02/04/09/16/21 |
| main.go | 7.2 节 1359-1446 行 | 废弃 | 拆四 cmd 入口；flag 改环境变量（SEC-13）；http.Server 全量超时 + MaxBytesReader（SEC-19）；Valkey ACL/TLS 连接配置（SEC-06）；TLS 终结（SEC-05） | SEC-05/06/13/19 |
| mock_device/device.go | 8 节 1450-1555 行 | 保留迁测试 | 迁 ce/test/mockdevice；凭证环境变量注入（SEC-25）；握手升级（nonce+HMAC）；wss 支持 | SEC-25/05 |
| 第 3 章 PG 三表 | 第 3 章 78-124 行 | 保留扩展 | 已演进为 design/32 权威表结构；adc_mcp_call_logs 吸收进 adc_audit_logs（design/32 2.2） | SEC-07/08/17 |
| 控制台前端请求层（Vue 3，design/20 第 4 章各页面共用请求封装） | design/20 | 改造 | 前端适配：全部请求 baseURL 统一指向 API 网关单一地址（前端唯一入口），不直接访问各服务；按路径前缀区分面（/v1/admin 等）；网关地址经构建期环境变量注入；新增 /v2/agents 面时前端无感（3.5.5） | 方案 A（3.5） |

---

## 6. 单测与集成测试边界

### 6.1 测试策略总则

单测用表驱动 + 接口替身（Valkey 用 miniredis、PG 用 testcontainers 或事务回滚）；所有涉及并发与共享状态的包必须跑 go test -race 且作为 CI 门禁；长连接测试注入阻塞点（自定义 dialer 与读循环挂起）覆盖交叉执行路径；鉴权与协议解析为对抗性用例重点（每模块列出）；集成测试用 docker compose（PG+Valkey）跑全链路，故障注入覆盖 Valkey/PG 断连降级行为。测试数据不得含真实密钥（SEC-25 同级要求）。

质量门禁（对齐 design/40 测试计划，本 LLD 只列工程门禁）：CI 依次执行 go vet、go test -race ./...（core-sdk/ 与 ce/ 独立构建，ADR-12）、govulncheck、licensecheck 与 SBOM 生成（doc/05 7.2）；覆盖率要求：domain 状态机与鉴权包 90% 以上行覆盖，app 编排层 70% 以上，store 层以集成用例为主不设强制线；竞态用例必须包含交叉执行路径（用 channel 注入阻塞点迫使交错），不得只测单一路径（go-expert 原则）。

### 6.2 Device Connector 测试清单

- 鉴权对抗：同一 nonce 重放第二次被拒；篡改 timestamp 越窗被拒；篡改 deviceCode 被拒；伪造签名被拒；已冻结/退役/吊销凭证被拒；nonce 过期后复用被拒；时间窗边界值（正负 300 秒）用例。
- 会话：同设备双连接踢旧（旧会话 done 关闭、旧 onClose 不误删新路由，SEC-15）；心跳超时回收；下行写超时断连；优雅关闭无 goroutine 泄漏（go.uber.org/goleak 或 goroutine 计数断言）。
- 工具同步：首次上报缺省 risk 2；DB 权威值覆盖设备上报值；is_enabled=false 剔除；非法 schema 拒绝；同步失败重试 3 次后设备标 ERROR。
- 路由索引：TTL 续期后键不过期；断连立即清理；Valkey 不可用时新连接被拒（fail-closed）。
- 全部用例跑 -race。

### 6.3 Agent API 测试清单

- 认证对抗：无 key/伪造 key/已吊销 key/已过期 key 返回 401；A 租户 key 声明 B 租户头仍按 A 租户执行（GAP-13）。
- 聚合：命名空间解析（deviceCode 含历史 __ 字符串仍正确）；代际 bump 后视图 5 秒内更新；Valkey 缓存清空后回源 PG 重建（缓存全丢仍正确）。
- 路由：本节点直连快速路径；双实例跨节点 Pub/Sub 往返（集成）；伪造节点签名被拒（SEC-06）；目标节点无响应超时返回 504。
- 限流：三维度独立超限 429 且带 Retry-After；限流不误伤其他维度。
- HITL 编排：risk 2 被拦截；审批通过后执行；拒绝/过期返回 BLOCKED；重复工单返回已有工单（params_hash）。
- 审计：调用与拦截均产生事件且字段完整（agent_id 为真实主体，SEC-21）。

### 6.4 Approval Service 测试清单

- 状态机：两个并发 approve 回调仅一个生效（CAS 行数断言）；过期工单不可批；二次回调幂等返回"已处理"并审计；非法迁移拒绝。
- 回调验签：篡改 decision/ticket_no 签名失败；过期 ts 拒绝；无签名 401；approver 与 OAuth 身份不一致 403。
- 通知：失败重试 3 次退避节奏；全部失败写 RETRY_EXHAUSTED 与告警事件；降级渠道被触发；adc_notifications 记录完整。
- 扫描器：到期工单置 EXPIRED 并唤醒等待方；双实例扫描器锁互斥仅一个执行；扫描失败告警。
- 跨节点唤醒：实例 A 挂起、实例 B 接收回调后 A 秒级唤醒（双实例集成，SEC-10）。
- 全部用例跑 -race。

### 6.5 Admin API 测试清单

- CRUD：设备码重复注册整行回滚（FR-011）；ID 字符集校验；软删除后唯一索引可复用（design/32 1.4）。
- 风险等级变更：缺 confirm 拒绝；变更后工具聚合视图立即反映（代际失效）；审计含变更前后值与管理员主体。
- API Key：明文仅返回一次；库内只存哈希；吊销后 30 秒内失效（Valkey 缓存主动失效断言）。
- 配额：超设备数配额注册被拒（条件 UPDATE 0 行）；SUSPENDED 租户被踢线与在途工单作废。
- 审计查询：keyset 分页游标稳定；时间范围过滤走分区裁剪；导出流式且超 10 万行截断。

### 6.6 集成测试链路

1. 全链路（phase0 变更 6.3 回归）：mock 设备接入 → HMAC 鉴权 → tools/list 同步 → 高危调用被拦截 → 工单创建 → 通知卡片（mock webhook）→ 验签回调 → 状态机 APPROVED → 跨节点唤醒 → 指令下发 → 执行结果 → 审计查询可见（事件完整、request_id 幂等）。
2. 双实例场景：设备连在实例 A、Agent 请求打到实例 B 的跨节点路由与跨节点审批唤醒。
3. 故障注入：Valkey 断连（新设备拒接、本节点设备调用仍可用、审计队列积压后恢复）；PG 断连（工单创建失败返回 5xx、设备心跳不受影响）；通知 webhook 超时（重试与降级路径）。
4. 性能门槛：限流压测（超限 429 不拖垮服务，SEC-12）；容量假设（1 万在线设备）在阶段 2 压测关卡兑现（doc/05 第 6 章），V1.0 不做全量容量验证。

---

## 附录 A：与平行设计文档的引用索引

- design/30：服务模块边界、进程部署形态、SLA 目标——本 LLD 的 1.2 节对应表与 3.x 各模块职责为其细化。
- design/32：表结构、状态枚举、索引、幂等机制——本 LLD 第 2 章实体清单、3.3.3 状态机 SQL、6.4 用例均与之对齐。
- design/33：HTTP 端点、请求响应、错误码、分页约定——本 LLD 只引用端点路径不重复定义。
- 修订纪律：任何一张表、一个枚举、一条端点的变更，须同步修订 31/32/33 三文并走 OpenSpec 变更流程（config.yaml 一致性要求）。
