# ADC 接口设计说明书（API 规范）

| 文档属性 | 内容 |
|---------|------|
| 文档编号 | design/33 |
| 文档版本 | V1.0 |
| 适用范围 | V1.0 接口全集：Admin API、Agent API、设备隧道（WSS + JSON-RPC 2.0）、HITL 回调、健康与指标 |
| 关联基线 | design/30（模块归属与职责划分，本文所有端点按 design/30 标注所属模块）；design/32（数据模型与字段命名，本文 JSON 示例字段名与其保持一致）；doc/03（PRD，FR-xxx）；doc/05（架构评审，SEC-xxx、ADR-xxx）；doc/Agentic Device Cloud.md（技术 PoC 基线，下称"原方案"） |
| 术语约定 | 全文中"原方案 X 行"指 doc/Agentic Device Cloud.md 的行号，用于标注改造点 |

---

## 1. 设计总则

### 1.1 API 风格

ADC 按服务面划分三种接口风格，各服务面职责与模块归属以 design/30 为准：

| 服务面 | 风格 | 传输 | 归属模块（design/30） | 开源属性 |
|--------|------|------|----------------------|----------|
| Admin API | REST，资源化路径，JSON 请求响应 | HTTPS | 管理控制台 Admin API | 企业版 EE |
| Agent API | MCP 风格（V1.0 为兼容形态，V1.5 升级标准 MCP Streamable HTTP，ADR-03） | HTTPS | Agent API | 社区版 CE |
| 设备隧道 | JSON-RPC 2.0 over WSS（Legacy Bridge，服务 B 类设备） | WSS（强制 TLS） | 设备 MCP 接入与注册治理（Legacy Bridge 子模块） | 社区版 CE |
| HITL 回调 | 签名回调（POST JSON）+ 落地页（GET HTML） | HTTPS | Approval Service | 社区版 CE 单级审批，企业版 EE 扩展 |

通用约定：

- 所有 API 统一 `/v1` 前缀；JSON 编码 UTF-8，请求 `Content-Type: application/json`。
- 时间字段统一 RFC3339 UTC 格式（结尾 Z，如 `2026-08-14T08:30:00Z`）；控制台展示按租户时区换算（默认 Asia/Shanghai，doc/03 FR-014 异常场景约定）。
- ID 字段：租户 `tenant_id`、设备 `device_id`、工单 `ticket_id`、审计 `log_id`、调用 `request_id` 均为字符串，其中 `device_id`、`request_id`、`ticket_id`、`log_id` 为 UUID 格式；设备对外标识 `device_code` 为业务唯一码（白名单字符集：字母、数字、中划线、下划线，长度 1-128，SEC-20 格式校验）。
- 字段命名统一 snake_case，与 design/32 表字段一一对应；请求体未知字段忽略，响应体不允许出现未定义字段（新增字段须先入设计基线）。

### 1.2 认证方式总表

| 端点面 | 鉴权方式 | 凭证载体 | 租户上下文来源 | 对应安全改造 |
|--------|---------|---------|---------------|-------------|
| Admin API | 会话认证（Cookie `adc_session`，OIDC/SSO 预留）+ RBAC | Cookie | 从会话解析；平台管理员可经路径或查询参数跨租户操作，租户管理员锁定会话租户，拒绝任何试图切换租户的请求 | SEC-02 原则延伸至控制面 |
| Agent API | API Key | 请求头 `X-ADC-Key: adc_<id>_<secret>` 或 `Authorization: Bearer adc_<id>_<secret>` | 从 API Key 绑定的租户解析；不接受 `X-Tenant-ID` 头作为租户依据 | SEC-02（原方案 7.1 节 1229-1252 行仅校验 X-Tenant-ID 非空的改造） |
| 设备隧道 | HMAC-SHA256 + nonce + timestamp | WSS 升级请求头 `X-Device-ID`、`X-ADC-Timestamp`、`X-ADC-Nonce`、`X-ADC-Signature` | 从设备台账解析（设备码绑定租户） | SEC-03（原方案 4.2 节 218-252 行静态凭证与固定 HMAC 的改造） |
| HITL 回调 | HMAC-SHA256 签名（工单级一次性密钥） | 请求头 `X-ADC-Timestamp`、`X-ADC-Nonce`、`X-ADC-Signature` | 从工单解析（ticket_id 绑定租户） | SEC-01（原方案 7.1 节 1319-1343 行无校验回调的改造）、SEC-13（回调 URL 签名） |
| 运维端点 | 无鉴权 | 不适用 | 不适用 | 仅限内网或负载均衡健康检查网段暴露（NFR-004） |

租户上下文硬性规则（SEC-02）：任何业务端点不接受以 `X-Tenant-ID` 请求头为唯一依据的租户上下文；租户一律从认证凭证（会话、API Key、设备凭证、工单密钥）解析，凭证与租户强绑定，解析失败直接 401。服务端按以下顺序解析租户：会话角色为租户管理员时取会话租户；平台管理员经路径参数 `tenant_id` 或查询参数显式指定目标租户（缺失时按 10001 拒绝）；Agent 面取 Key 绑定租户；设备面取设备台账租户；HITL 面取工单租户。任何路径中出现与凭证租户不一致的显式租户参数，一律返回 403 code 13007。

Admin API 角色权限矩阵（FR-008/FR-014，菜单级裁剪由控制台执行、接口级校验由服务端强制）：

| 角色 | 端点范围 | 备注 |
|------|---------|------|
| platform_admin | 全部 Admin API | 唯一可创建/停用租户与跨租户查询的角色 |
| tenant_admin | 本租户设备、工具、API Key、组织成员、审批工单、审计、用量 | 不得触碰其他租户任何资源（13007） |
| approver | GET 本租户设备列表（只读）、审批工单查询 | 审批动作经 HITL 回调面完成，不经 Admin API |
| auditor | 审计日志查询与导出、用量只读 | 全接口只读，任何写操作 10003 |

### 1.3 设备隧道 HMAC 签名规范（SEC-03）

B 类设备（Legacy Bridge）建立 WSS 连接时携带以下请求头：

```
X-Device-ID: <device_id>
X-ADC-Timestamp: 1755117000          # Unix 秒
X-ADC-Nonce: <随机串，8-32 字节，十六进制>
X-ADC-Signature: <hex(HMAC-SHA256(device_secret, 签名串))>
签名串 = device_id + "\n" + timestamp + "\n" + nonce
```

校验规则：

1. `|now - timestamp| <= 300` 秒，超出返回 401（漂移窗口 5 分钟，doc/03 FR-002 异常场景）。
2. nonce 服务端缓存 10 分钟，命中即判定重放，返回 401 code 11012（SEC-03）。
3. 签名不匹配返回 401 code 11011；设备被吊销或冻结返回 401 code 11004 或 403 code 11003。
4. 凭证为签发时一次性明文返回、库中仅存哈希（NFR-004）；凭证支持重置与轮转（FR-010）。

### 1.4 HITL 回调签名规范（SEC-01/SEC-13）

回调链路两级签名：

1. 落地页签名（URL）：`GET /v1/hitl/action?ticket_id=<t>&decision=<d>&expire=<e>&sig=<s>`
   `sig = hex(HMAC-SHA256(callback_secret, ticket_id + "." + decision + "." + expire))`，`expire` 为工单过期时间 Unix 秒。`callback_secret` 为工单创建时生成的工单级一次性密钥，仅经审批卡片 URL 参数传给审批人，落库仅存哈希（SEC-13 原方案 6.3 节 1056-1103 行按钮 URL 无签名的改造）。
2. 回调请求签名（头）：`POST /v1/hitl/callback` 携带 `X-ADC-Timestamp`、`X-ADC-Nonce`、`X-ADC-Signature`，签名串 = `timestamp + "\n" + nonce + "\n" + 请求体原文`，密钥同上。nonce 一次性消费（缓存至工单终态），校验失败返回 401 code 12004 并记录异常事件（doc/03 FR-005 验收）。

审批人身份从企业微信/钉钉 OAuth 授权回调获取真实工号，禁止硬编码（SEC-01 原方案 1329 行 "DutyEngineer" 的改造）。

### 1.5 错误码规范

统一错误结构（所有非 2xx JSON 响应）：

```json
{
  "code": 11001,
  "message": "设备不存在",
  "message_key": "errors.device.not_found",
  "trace_id": "4f8a2c1e9b3d4f5a8c6d7e8f9a0b1c2d",
  "details": [
    { "field": "device_id", "reason": "no row matches" }
  ]
}
```

| 字段 | 说明 |
|------|------|
| code | 业务错误码（int），见下方分段；HTTP 状态码与之配套，二者冲突以 HTTP 状态码控制传输语义、code 控制业务语义 |
| message | V1.0 中文文案；V1.5 起按 i18n key 返回租户语言（FR-023） |
| message_key | i18n 词条键，V1.0 即预留并随语言包发布（GAP-16） |
| trace_id | 全链路追踪 ID，贯通设备接入、工具调用、HITL 工单与审计日志（doc/03 埋点规范） |
| details | 可选，字段级错误明细，用于表单提示与批量导入行级原因 |

业务错误码分段：

| 分段 | 码段 | 示例 |
|------|------|------|
| 10xxx 通用 | 10001 参数校验失败（400）；10002 未认证或凭证无效（401）；10003 权限不足（403）；10004 资源不存在（404）；10005 资源冲突（409）；10006 请求过于频繁（429）；10007 服务内部错误（500）；10008 请求体过大（413）；10009 幂等键冲突（409）；10010 请求体非合法 JSON（400）；10011 不支持的媒体类型（415） | |
| 11xxx 设备 | 11001 设备不存在（404）；11002 设备离线（503）；11003 设备已冻结（403）；11004 设备凭证无效或已吊销（401）；11005 工具不存在（404）；11006 工具已禁用（403）；11007 工具调用超时（504）；11008 设备码已存在（409）；11009 批量导入存在错误行（422）；11010 设备配额超限（403）；11011 设备签名校验失败（401）；11012 设备 nonce 重放（401） | |
| 12xxx 审批 | 12001 工单不存在（404）；12002 工单已处理（409）；12003 工单已过期（403，实现为准）；12004 回调签名校验失败（401，签名缺失同 401）；12005 审批人无权处理（403）；12006 操作已被 HITL 审批拦截（403，即 BLOCKED_BY_HITL）；12007 审批流未配置（503）；12008 审批渠道推送失败（503） | |
| 13xxx 租户 | 13001 租户不存在（404）；13002 租户已停用（403）；13003 租户配额超限（403）；13004 租户标识重复（409）；13005 角色不存在（404）；13006 组织成员不存在（404）；13007 越权访问其他租户资源（403） | |
| 14xxx 审计 | 14001 审计查询条件非法（400）；14002 导出数量超限（422）；14003 导出任务不存在（404）；14004 审计日志不可修改（403） | |

JSON-RPC 错误码（设备隧道内，标准码加自定义段）：

| 码 | 含义 |
|----|------|
| -32600 / -32601 / -32602 / -32603 | 无效请求 / 方法不存在 / 参数无效 / 内部错误（JSON-RPC 2.0 标准） |
| -32001 | 设备未注册或已冻结 |
| -32002 | 工具不存在 |
| -32003 | 工具已禁用（平台配置 is_enabled=false） |
| -32004 | 工具执行失败（设备侧回传 isError=true） |
| -32005 | 调用超时（数据面 15 秒默认，长任务走 ADR-10 异步模型，阶段 2 交付） |

### 1.6 分页约定

| 模式 | 适用场景 | 参数 | 响应 |
|------|---------|------|------|
| offset 分页 | 列表页：设备列表、租户列表、API Key 列表、组织成员、审批工单（小数据量、需要跳页与总数） | `page`（默认 1）、`page_size`（默认 20，上限 200） | `items` + `total` |
| cursor 分页 | 审计日志（append-only、大数据量、导出场景，FR-013） | `limit`（默认 50，上限 500）、`cursor`（上一页返回的游标，首查省略） | `items` + `next_cursor`（null 表示末页） |

`total` 由数据库 count 提供；审计日志按 `created_at DESC, id DESC` 排序，cursor 为该组合键的 base64 编码，客户端不得自行构造。

### 1.7 幂等约定

- 写操作支持 `Idempotency-Key` 请求头（UUID 格式）：`POST /v1/tenants`、`POST /v1/devices`、`POST /v1/agent-keys`、`POST /v1/devices/import`。
- 同一租户 + 同一 Key 在 24 小时内重复请求返回首次响应（含首次响应体）；Key 相同但请求体不同返回 409 code 10009。
- 幂等记录存 Valkey（24 小时 TTL），不计入审计；HITL 回调的幂等由工单状态机原子 CAS 保证（SEC-11），不使用 Idempotency-Key。

### 1.8 版本约定

- 路径前缀 `/v1` 为 API 主版本；次版本以 `X-ADC-Version` 响应头声明（如 `2026-08`），供客户端兼容判断。
- 演进规则：只增不改不删。新增端点或字段为兼容变更；字段重命名、类型变更、语义变更、端点删除为破坏性变更，一律升主版本 `/v2` 并保留旧版本至少两个主版本周期；废弃字段标注 `"deprecated": true` 并在 release notes 说明移除计划。

### 1.9 限流头与通用请求头

所有业务端点返回限流头（SEC-12）：

```
X-RateLimit-Limit: 1000        # 窗口内配额上限
X-RateLimit-Remaining: 998     # 剩余配额
X-RateLimit-Reset: 1755117300  # 窗口重置时间（Unix 秒）
```

超限返回 429 + code 10006 + `Retry-After` 头。限流维度：Admin API 按用户；Agent API 按 API Key（默认 1000 次/分钟）；设备隧道按设备（连接建立 5 次/分钟）；HITL 回调按工单（每个工单仅接受一次有效回调）。单 Key 高频异常（1 分钟 100 次失败）触发熔断告警（doc/03 FR-009 异常场景）。

通用请求头：`X-ADC-Request-ID`（可选，UUID，未携带由服务端生成，响应同头回传）；`Accept-Language`（V1.5 起生效，V1.0 忽略）。响应统一携带 `X-ADC-Request-ID`、`X-ADC-Version`、`X-RateLimit-*`。

### 1.10 设备状态机与工具可见性

设备状态流转（design/32 `adc_devices.status`，FR-010）：

```text
offline --注册--> offline --隧道连接+心跳--> online
online --心跳超时 60 秒/断连--> offline
online --执行失败率超阈值或告警--> error --恢复心跳--> online
任意 --管理员冻结--> frozen --管理员解冻--> offline
```

约束：`frozen` 态拒绝新连接并踢下线（FR-002 验收）；`error` 态不摘除工具（设备仍在线），仅告警；工具聚合只收录 `online` 设备的已启用工具，设备上线到工具对 Agent 可见不超过 30 秒（FR-003 验收、NFR-003）；离线设备工具自动摘除；设备离线期间发起的调用返回 11002。状态变更产生审计事件（FR-010 验收）。

### 1.11 数据脱敏与凭证处理

- 凭证（设备 secret、API Key、工单回调密钥）仅在签发响应中出现一次，库中仅存哈希，日志与审计中一律掩码（如 `adc_3c4d****`）；网关任何响应不输出明文凭证（NFR-004）。
- 审计日志中 `request_params` 与 `response_payload` 原样留存（合规留证需要），但导出接口对标记为敏感的工具参数执行掩码（按 design/32 工具元数据 `sensitive_params` 声明）；控制台列表页默认截断 200 字符并标注 `"truncated": true`（FR-013 异常场景）。
- trace_id 贯通接入、调用、工单与审计（doc/03 埋点规范）；响应头 `X-ADC-Request-ID` 与错误体 `trace_id` 一致，供排障关联。

---

## 2. 端点清单总表

| # | 方法 | 路径 | 鉴权 | 用途 | 对应需求 | 版本 |
|---|------|------|------|------|---------|------|
| 1 | POST | /v1/auth/login | 无（换取会话） | 管理员登录，签发会话 Cookie | FR-014 | V1.0 |
| 2 | GET | /v1/tenants | 会话 + RBAC（平台管理员） | 租户列表 | FR-008 | V1.0 |
| 3 | POST | /v1/tenants | 会话 + RBAC（平台管理员） | 创建租户（含配额） | FR-008 | V1.0 |
| 4 | GET | /v1/tenants/{tenant_id} | 会话 + RBAC | 租户详情与配额 | FR-008 | V1.0 |
| 5 | PATCH | /v1/tenants/{tenant_id} | 会话 + RBAC | 更新配额、停用/启用租户 | FR-008 | V1.0 |
| 6 | POST | /v1/devices | 会话 + RBAC | 设备注册并签发凭证 | FR-010 | V1.0 |
| 7 | GET | /v1/devices | 会话 + RBAC | 设备列表与筛选 | FR-010 | V1.0 |
| 8 | PATCH | /v1/devices/{device_id} | 会话 + RBAC | 冻结/解冻、吊销、重置凭证 | FR-010 | V1.0 |
| 9 | POST | /v1/devices/import | 会话 + RBAC | 批量 CSV 导入 | FR-011 | V1.5 预留 |
| 10 | GET | /v1/devices/{device_id}/tools | 会话 + RBAC | 设备工具列表与风险等级 | FR-006 | V1.0 |
| 11 | PATCH | /v1/devices/{device_id}/tools | 会话 + RBAC | 工具风险等级与启停配置 | FR-006 | V1.0 |
| 12 | POST | /v1/agent-keys | 会话 + RBAC | 签发 Agent API Key | FR-009 | V1.0 |
| 13 | GET | /v1/agent-keys | 会话 + RBAC | API Key 列表 | FR-009 | V1.0 |
| 14 | DELETE | /v1/agent-keys/{key_id} | 会话 + RBAC | 吊销 API Key | FR-009 | V1.0 |
| 15 | GET | /v1/approval-tickets | 会话 + RBAC | 审批工单列表 | FR-005 | V1.0 |
| 16 | GET | /v1/audit-logs | 会话 + RBAC | 审计日志查询与导出 | FR-013 | V1.0 |
| 17 | GET | /v1/usage | 会话 + RBAC | 配额用量查询 | FR-008/FR-016 | V1.0 |
| 18 | GET | /v1/org/users | 会话 + RBAC | 组织成员与角色 | FR-008 | V1.0 |
| 19 | GET | /v1/agent/mcp/tools | API Key | 聚合工具列表 | FR-003 | V1.0 |
| 20 | POST | /v1/agent/mcp/tools/call | API Key | 工具调用（含 HITL 拦截） | FR-003/FR-005/FR-006 | V1.0 |
| 21 | GET | /v1/agent/mcp/tools/call/{request_id} | API Key | HITL 挂起调用结果查询 | FR-005 | V1.0 |
| 22 | WSS | /v1/devices/tunnel | HMAC + nonce | 设备反向隧道（JSON-RPC 2.0） | FR-001/FR-002 | V1.0 |
| 23 | POST | /v1/hitl/callback | HMAC 签名 | 审批决策回调 | FR-005 | V1.0 |
| 24 | GET | /v1/hitl/action | URL 签名 | 审批落地页 | FR-005 | V1.0 |
| 25 | GET | /healthz | 无 | 存活探针 | FR-017 | V1.0 |
| 26 | GET | /readyz | 无 | 就绪探针（依赖检查） | FR-017 | V1.0 |
| 27 | GET | /metrics | 无（内网） | Prometheus 指标 | FR-017 | V1.0 |

端点总数：27。V1.5 预留端点（不在 V1.0 交付）：A 类设备 MCP 绑定管理 `/v1/bindings` 系列（FR-021）；LLM 网关 `/v1/llm` 系列（FR-012）；计费账单 `/v1/billing` 系列（FR-016）；A2A 端点（FR-022，V2.0，见第 5 章）。

端点分组与版本归属汇总（模块归属以 design/30 为准）：

| 分组 | 端点数 | 归属 | 交付版本 |
|------|--------|------|---------|
| 管理控制台 Admin API（租户/设备/工具/API Key/组织/用量） | 17 | 企业版 EE | V1.0 |
| 审批工单查询（3.1.15） | 1（含于 Admin） | 企业版 EE | V1.0 |
| Agent API（工具聚合与调用） | 3 | 社区版 CE | V1.0 |
| 设备隧道 WSS | 1 | 社区版 CE | V1.0 |
| HITL 回调与落地页 | 2 | 社区版 CE（单级审批）；多级审批企业版 EE（V1.5） | V1.0 |
| 运维探针与指标 | 3 | 社区版 CE | V1.0 |

典型调用时序（免审与审批两条链路）：

```text
免审：Agent --GET /v1/agent/mcp/tools--> 工具聚合 --POST tools/call--> OPA 风险判定(risk_level<2)
      --> 集群路由 --> 设备隧道 JSON-RPC tools/call --> 设备回包 --> 200 结果 + 审计落库

审批：Agent --POST tools/call--> OPA 判定(risk_level>=2) --> 创建 PG 工单 + 202 响应
      --> 卡片推送(企微/钉钉) --> 审批人点按钮 --> GET /v1/hitl/action 验签落地页
      --> POST /v1/hitl/callback 验签消费 --> 状态机原子更新 --> 唤醒挂起调用
      --> 执行或拒绝 --> Agent 经 GET tools/call/{request_id} 取回结果或 BLOCKED_BY_HITL
```

---

## 3. 接口详细定义

以下各端点标注所属模块（design/30）与改造点（原方案行号）。

### 3.1 Admin API

#### 3.1.1 POST /v1/auth/login

- 模块：管理控制台 Admin API（design/30）；用途：管理员登录，签发会话（FR-014）。
- 请求：

```json
{
  "username": "itadmin@example-factory.com",
  "password": "********",
  "mfa_code": "123456"
}
```

- 响应 200：

```json
{
  "user_id": "u_9f8e7d6c",
  "display_name": "工厂 IT 管理员",
  "tenant_id": "t_1a2b3c4d",
  "role": "tenant_admin",
  "expires_at": "2026-08-15T08:30:00Z"
}
```

响应同时经 `Set-Cookie: adc_session=...; HttpOnly; Secure; SameSite=Lax` 下发会话。`mfa_code` 在企业版开启 MFA 时必填（阶段 3 SSO/OIDC + MFA，doc/05）。

- 错误码：10001（用户名或密码格式非法）、10002（用户名或密码错误，连续 5 次失败锁定 15 分钟并审计）、13002（租户停用）、13001（租户不存在）。
- 备注：V1.0 为账号密码 + 会话，OIDC/SSO 作为预留扩展点（登录端点兼容标准 OIDC 授权码流程回调）。

#### 3.1.2 GET /v1/tenants

- 模块：管理控制台 Admin API；用途：租户列表（仅平台管理员可见全量，FR-008）。
- 查询参数：`page`、`page_size`、`status`（active/disabled）、`keyword`（名称模糊）。
- 响应 200：

```json
{
  "items": [
    {
      "tenant_id": "t_1a2b3c4d",
      "name": "华东精密制造有限公司",
      "status": "active",
      "device_count": 132,
      "agent_key_count": 4,
      "created_at": "2026-05-01T02:00:00Z"
    }
  ],
  "total": 1,
  "page": 1,
  "page_size": 20
}
```

- 错误码：10003（非平台管理员）、10001（分页参数非法）。
- 备注：租户管理员调用本端点仅返回自身租户一行。

#### 3.1.3 POST /v1/tenants

- 模块：管理控制台 Admin API；用途：创建租户（FR-008），支持 `Idempotency-Key`。
- 请求：

```json
{
  "name": "华东精密制造有限公司",
  "code": "huadong-precision",
  "quota": {
    "max_devices": 1000,
    "max_agent_keys": 20,
    "monthly_call_limit": 1000000,
    "audit_retention_days": 180
  }
}
```

- 响应 201：

```json
{
  "tenant_id": "t_1a2b3c4d",
  "name": "华东精密制造有限公司",
  "code": "huadong-precision",
  "status": "active",
  "quota": {
    "max_devices": 1000,
    "max_agent_keys": 20,
    "monthly_call_limit": 1000000,
    "audit_retention_days": 180
  },
  "created_at": "2026-05-01T02:00:00Z"
}
```

- 错误码：10001（code 不合规）、13004（code 重复）、10003（权限不足）。
- 备注：创建即生成独立命名空间（PG schema 级隔离），设备、工具、Agent、审计数据全部隔离（FR-008 验收）；配额模型与 design/32 `adc_tenants` 一致。

#### 3.1.4 GET /v1/tenants/{tenant_id}

- 模块：管理控制台 Admin API；用途：租户详情（FR-008）。
- 响应 200：

```json
{
  "tenant_id": "t_1a2b3c4d",
  "name": "华东精密制造有限公司",
  "code": "huadong-precision",
  "status": "active",
  "quota": {
    "max_devices": 1000,
    "max_agent_keys": 20,
    "monthly_call_limit": 1000000,
    "audit_retention_days": 180
  },
  "created_at": "2026-05-01T02:00:00Z",
  "updated_at": "2026-07-12T09:30:00Z"
}
```

- 错误码：13001（租户不存在）、13007（越权访问其他租户，返回 403，不泄露存在性）。
- 备注：租户管理员访问时必须与会话租户一致，否则 13007（SEC-02 原则）。

#### 3.1.5 PATCH /v1/tenants/{tenant_id}

- 模块：管理控制台 Admin API；用途：更新配额、停用/启用租户（FR-008）。
- 请求（部分字段更新，语义明确）：

```json
{
  "quota": { "max_devices": 2000 },
  "status": "active",
  "change_reason": "客户扩容，设备配额由 1000 提升至 2000"
}
```

- 响应 200：返回更新后的租户详情（结构同 3.1.4）。
- 错误码：13001、13007、10001（status 非法值）、13003（新配额低于当前用量时拒绝）。
- 备注：`change_reason` 必填并写入审计；`status` 置 `disabled` 时服务端踢下线该租户全部设备连接、失效全部在途工单（FR-008 异常场景）；删除租户不提供 DELETE（7 天冷静期，经内部工单流程）。

#### 3.1.6 POST /v1/devices

- 模块：管理控制台 Admin API；用途：设备注册并签发凭证（FR-010），支持 `Idempotency-Key`。
- 请求：

```json
{
  "device_code": "cnc-lathe-01",
  "name": "一号车床",
  "device_type": "cnc",
  "auth_type": "hmac",
  "group_ids": [],
  "metadata": { "workshop": "A1", "vendor": "Siemens" }
}
```

- 响应 201（凭证仅本次返回，库中仅存哈希，NFR-004）：

```json
{
  "device_id": "d_7a6b5c4d",
  "device_code": "cnc-lathe-01",
  "name": "一号车床",
  "device_type": "cnc",
  "auth_type": "hmac",
  "status": "offline",
  "credential": { "secret": "adc_dev_sec_xxxxxxxxxxxxxxxx" },
  "metadata": { "workshop": "A1", "vendor": "Siemens" },
  "created_at": "2026-08-14T08:00:00Z"
}
```

- 错误码：11008（device_code 已存在）、10001（device_code 含非法字符或超长，SEC-20）、11010（设备配额超限，13003 亦可能）、10003。
- 备注：`auth_type` 枚举 token/hmac/mtls；mTLS 类型时 `credential.secret` 为客户端证书下载地址（`credential.cert_url`）。注册成功后设备码全局唯一（FR-010 异常场景）。

#### 3.1.7 GET /v1/devices

- 模块：管理控制台 Admin API；用途：设备列表与筛选（FR-010/FR-014）。
- 查询参数：`page`、`page_size`、`status`（offline/online/error/frozen）、`device_type`、`keyword`（名称或设备码模糊）、`group_id`。
- 响应 200：

```json
{
  "items": [
    {
      "device_id": "d_7a6b5c4d",
      "device_code": "cnc-lathe-01",
      "name": "一号车床",
      "device_type": "cnc",
      "auth_type": "hmac",
      "status": "online",
      "last_heartbeat": "2026-08-14T08:29:55Z",
      "sdk_version": "1.2.0",
      "metadata": { "workshop": "A1", "vendor": "Siemens" },
      "created_at": "2026-08-14T08:00:00Z"
    }
  ],
  "total": 132,
  "page": 1,
  "page_size": 20
}
```

- 错误码：10001（筛选参数非法）。
- 备注：在线状态由数据面心跳事件实时计算（SEC-14/SEC-17），响应允许 10 秒内最终一致；不返回任何凭证字段。

#### 3.1.8 PATCH /v1/devices/{device_id}

- 模块：管理控制台 Admin API；用途：冻结/解冻、吊销凭证、重置凭证、改名与元数据（FR-010）。
- 请求：

```json
{
  "op": "reset_credential",
  "change_reason": "工程变更，旧凭证随外包交接作废",
  "name": "一号车床（A1 车间）"
}
```

- 响应 200：

```json
{
  "device_id": "d_7a6b5c4d",
  "device_code": "cnc-lathe-01",
  "name": "一号车床（A1 车间）",
  "status": "offline",
  "credential": { "secret": "adc_dev_sec_yyyyyyyyyyyyyyyy" },
  "updated_at": "2026-08-14T09:00:00Z"
}
```

- `op` 枚举：`freeze`（冻结，踢下线现有连接，新连接拒绝）、`unfreeze`（解冻）、`revoke_credential`（吊销凭证，设备须重新注册获发新凭证）、`reset_credential`（签发新凭证，旧凭证 24 小时过渡期后失效，FR-009 轮换思路复用）、`update_meta`（更新名称与 metadata）。`op=reset_credential` 时响应含 `credential`，其余 op 不含。
- 错误码：11001（设备不存在）、10001（op 非法或缺少 change_reason）、11003（冻结态设备不可再 reset）。
- 备注：`change_reason` 必填并写入审计（FR-010 异常场景）；吊销/冻结生效即断开现有连接（FR-002 验收）。

#### 3.1.9 POST /v1/devices/import（V1.5 预留，FR-011）

- 模块：管理控制台 Admin API；用途：CSV 批量导入注册，异步任务。
- 请求：`multipart/form-data`，字段 `file`（CSV，上限 1000 行）、`dry_run`（true/false）。CSV 列：`device_code,name,device_type,auth_type,group_ids`。
- 响应 202：

```json
{
  "task_id": "task_9d8c7b6a",
  "status": "queued",
  "status_url": "/v1/devices/import/{task_id}"
}
```

- 备注：失败行给出逐行原因（details 数组复用 1.5 节错误结构）；重复设备码整行回滚；V1.0 不交付本端点，客户端不得依赖。

#### 3.1.10 GET /v1/devices/{device_id}/tools

- 模块：管理控制台 Admin API；用途：设备工具列表与风险等级（FR-006）。
- 响应 200：

```json
{
  "items": [
    {
      "name": "get_spindle_status",
      "description": "Read CNC spindle RPM and temperature",
      "input_schema": { "type": "object" },
      "risk_level": 0,
      "is_enabled": true,
      "schema_version": 1,
      "updated_at": "2026-08-14T07:40:00Z"
    },
    {
      "name": "set_spindle_speed",
      "description": "Set CNC spindle RPM",
      "input_schema": { "type": "object", "properties": { "rpm": { "type": "number" } }, "required": ["rpm"] },
      "risk_level": 2,
      "is_enabled": true,
      "schema_version": 1,
      "updated_at": "2026-08-14T07:40:00Z"
    }
  ],
  "total": 2,
  "page": 1,
  "page_size": 50
}
```

- 错误码：11001（设备不存在）。
- 备注：工具元数据来自设备 tools/list 上报落库（原方案 4.3 节内存缓存改造为 PG 主存），`risk_level` 为平台权威配置（SEC-09：拦截读库而非关键字，原方案 1345-1353 行 isHighRiskOperation 删除）。

#### 3.1.11 PATCH /v1/devices/{device_id}/tools

- 模块：管理控制台 Admin API；用途：工具风险等级与启停配置（FR-006）。
- 请求（按工具名批量提交，部分更新）：

```json
{
  "changes": [
    { "name": "set_spindle_speed", "risk_level": 2, "is_enabled": true },
    { "name": "set_rpm_override", "risk_level": 0, "is_enabled": true }
  ],
  "change_reason": "产线安全评审决议：override 类工具降级"
}
```

- 响应 200：

```json
{
  "changed": 2,
  "failed": []
}
```

- 错误码：11001、11005（工具名不存在）、10001（risk_level 超出 0-3、或缺少 change_reason）。
- 备注：等级下调（2 及以上降至 1 及以下）强制二次确认（请求头 `X-ADC-Confirm: true`），并写入审计（FR-006 异常场景）；配置变更 30 秒内对数据面拦截生效（FR-006 验收）；工具名不含设备码（本端点已由 device_id 定位）。

#### 3.1.12 POST /v1/agent-keys

- 模块：管理控制台 Admin API；用途：签发 Agent API Key（FR-009），支持 `Idempotency-Key`。
- 请求：

```json
{
  "name": "排产调度 Agent",
  "scopes": { "allowed_tools": ["*"] },
  "expires_at": "2026-11-14T08:00:00Z"
}
```

- 响应 201（完整 Key 仅显示一次，库中仅存哈希，FR-009 验收）：

```json
{
  "key_id": "k_3c4d5e6f",
  "name": "排产调度 Agent",
  "key": "adc_3c4d5e6f_xH7kP2mQ9vL4nB8tR6wY",
  "key_prefix": "adc_3c4d5e6f",
  "scopes": { "allowed_tools": ["*"] },
  "status": "active",
  "expires_at": "2026-11-14T08:00:00Z",
  "created_at": "2026-08-14T09:10:00Z"
}
```

- `scopes.allowed_tools` 为工具名匹配列表（`*` 全部，支持 `device_code__*` 前缀、`*__tool_name` 后缀）；为空数组表示无工具权限。
- 错误码：10001（expires_at 已过期或超过 365 天）、13003（Key 数量超配额）。
- 备注：Key 与租户强绑定（SEC-02）；撤销、过期即 401（FR-009 验收）。

#### 3.1.13 GET /v1/agent-keys

- 模块：管理控制台 Admin API；用途：API Key 列表（FR-009）。
- 响应 200：

```json
{
  "items": [
    {
      "key_id": "k_3c4d5e6f",
      "name": "排产调度 Agent",
      "key_prefix": "adc_3c4d5e6f",
      "scopes": { "allowed_tools": ["*"] },
      "status": "active",
      "last_used_at": "2026-08-14T09:20:00Z",
      "expires_at": "2026-11-14T08:00:00Z",
      "created_at": "2026-08-14T09:10:00Z"
    }
  ],
  "total": 4,
  "page": 1,
  "page_size": 20
}
```

- 备注：仅返回 key_prefix 掩码，不返回完整 Key。

#### 3.1.14 DELETE /v1/agent-keys/{key_id}

- 模块：管理控制台 Admin API；用途：吊销 API Key（FR-009）。
- 响应 204。错误码：10004（Key 不存在）、10001。
- 备注：吊销即时生效（新请求 401，FR-009 验收）；轮换场景支持新旧 Key 24 小时并行过渡（FR-009 异常场景，通过创建新 Key 后延迟吊销旧 Key 实现）。

#### 3.1.15 GET /v1/approval-tickets

- 模块：Approval Service（design/30）；用途：审批工单列表与状态查询（FR-005，工单中心 FR-014）。
- 查询参数：`page`、`page_size`、`status`（pending/approved/rejected/expired）、`device_code`、`tool_name`、`time_from`、`time_to`。
- 响应 200：

```json
{
  "items": [
    {
      "ticket_id": "hitl_8b7c6d5e",
      "device_id": "d_7a6b5c4d",
      "device_code": "cnc-lathe-01",
      "tool_name": "set_spindle_speed",
      "arguments": { "rpm": 4200 },
      "risk_level": 2,
      "status": "approved",
      "approver": "emp_zhangwei",
      "comment": "已核实工艺单，放行",
      "created_at": "2026-08-14T08:15:00Z",
      "expire_at": "2026-08-14T08:20:00Z",
      "resolved_at": "2026-08-14T08:17:30Z"
    }
  ],
  "total": 1,
  "page": 1,
  "page_size": 20
}
```

- 错误码：10001（时间范围非法）。
- 备注：工单主存 PostgreSQL、Valkey 仅作唤醒通道（ADR-05，原方案 6.2 节 915-918 行仅存 Redis 的改造）；单级审批入 CE，多级审批链字段 `approval_chain` 为企业版扩展（V1.5，FR-007）。

#### 3.1.16 GET /v1/audit-logs

- 模块：审计中心（design/30，企业版 EE）；用途：审计日志查询与导出（FR-013）。
- 查询参数：`limit`、`cursor`、`time_from`、`time_to`、`device_code`、`agent_id`、`tool_name`、`status`、`format`（json 默认 / csv 导出）、`event_type`（tool_call/hitl/admin/api_key）。
- 响应 200：

```json
{
  "items": [
    {
      "log_id": "l_5e6f7a8b",
      "event_type": "tool_call",
      "trace_id": "4f8a2c1e9b3d4f5a8c6d7e8f9a0b1c2d",
      "agent_id": "agent-prod-worker",
      "key_id": "k_3c4d5e6f",
      "device_id": "d_7a6b5c4d",
      "device_code": "cnc-lathe-01",
      "tool_name": "set_spindle_speed",
      "request_params": { "rpm": 4200 },
      "response_payload": { "content": [{ "type": "text", "text": "Spindle speed set to 4200 RPM" }] },
      "status": "success",
      "hitl_approver": "emp_zhangwei",
      "hitl_comment": "已核实工艺单，放行",
      "exempt_reason": null,
      "execution_duration_ms": 328,
      "created_at": "2026-08-14T08:16:00Z"
    }
  ],
  "next_cursor": "eyJjcmVhdGVkX2F0IjoiMjAyNi0wOC0xNFQwODoxNjowMFoiLCJpZCI6ImxfNWU2ZjdhOGIifQ"
}
```

- 错误码：14001（条件非法，如 time_from 晚于 time_to）、14002（csv 导出超过 10 万条，须走异步导出任务）。
- 备注：append-only，任何修改/删除尝试返回 14004（FR-013 验收）；审计写入走 JetStream 异步链路（ADR-06，原方案 adc_mcp_call_logs 表定义了但零写入，SEC-07）；日志留存 180 天（NFR-006）；超大响应负载截断并标注 `"truncated": true`（FR-013 异常场景）。

#### 3.1.17 GET /v1/usage

- 模块：计量计费（design/30，企业版 EE）；用途：租户配额用量（FR-008 配额、FR-016 计量埋点基础）。
- 响应 200：

```json
{
  "tenant_id": "t_1a2b3c4d",
  "period": "2026-08",
  "usage": {
    "device_peak_daily": 118,
    "agent_calls": 87234,
    "hllm_tokens": null
  },
  "quota": {
    "max_devices": 1000,
    "monthly_call_limit": 1000000
  }
}
```

- 备注：`llm_tokens` 自 V1.5（FR-012）起生效，V1.0 恒为 null；计量与审计日志对账一致（FR-016 验收）。

#### 3.1.18 GET /v1/org/users

- 模块：管理控制台 Admin API；用途：组织成员与角色（FR-008 RBAC）。
- 查询参数：`page`、`page_size`、`keyword`。
- 响应 200：

```json
{
  "items": [
    {
      "user_id": "u_9f8e7d6c",
      "display_name": "工厂 IT 管理员",
      "account": "itadmin",
      "role": "tenant_admin",
      "status": "active",
      "created_at": "2026-05-02T03:00:00Z"
    },
    {
      "user_id": "u_1234abcd",
      "display_name": "张伟",
      "account": "zhangwei",
      "role": "approver",
      "status": "active",
      "created_at": "2026-05-02T03:10:00Z"
    }
  ],
  "total": 2,
  "page": 1,
  "page_size": 20
}
```

- 角色枚举：`platform_admin`（平台管理员）、`tenant_admin`（租户管理员）、`approver`（审批人）、`auditor`（只读审计员）。菜单级权限裁剪由控制台按角色实现（FR-014）。
- 备注：V1.0 用户由租户管理员创建，SSO/OIDC 对接在阶段 3（doc/05）；审批人绑定企微/钉钉工号字段 `im_identity` 为审批渠道必需（SEC-01 真实身份回传）。

### 3.2 Agent API

#### 3.2.1 GET /v1/agent/mcp/tools

- 模块：Agent API（design/30，社区版 CE）；用途：租户级聚合工具列表（FR-003）。
- 鉴权：API Key（`X-ADC-Key` 或 `Authorization: Bearer`），租户自 Key 解析（SEC-02）。
- 响应 200：

```json
{
  "tools": [
    {
      "name": "cnc-lathe-01__get_spindle_status",
      "description": "[cnc-lathe-01] Read CNC spindle RPM and temperature",
      "input_schema": { "type": "object" },
      "device_code": "cnc-lathe-01",
      "device_name": "一号车床",
      "device_status": "online",
      "risk_level": 0,
      "schema_version": 1
    },
    {
      "name": "cnc-lathe-01__set_spindle_speed",
      "description": "[cnc-lathe-01] Set CNC spindle RPM",
      "input_schema": { "type": "object", "properties": { "rpm": { "type": "number" } }, "required": ["rpm"] },
      "device_code": "cnc-lathe-01",
      "device_name": "一号车床",
      "device_status": "online",
      "risk_level": 2,
      "schema_version": 1
    }
  ]
}
```

- 错误码：10002（Key 无效或过期）、13002（租户停用）、10006（限流）。
- 备注：改造点（原方案 7.1 节 1229-1244 行）：删除 X-Tenant-ID 头依赖（SEC-02）；`name` 保留"设备码__工具名"聚合命名（FR-003 验收），`description` 加设备前缀，另提供结构化 `device_code` 字段供调用（SEC-24 的兼容解法：列表层保持 MCP 生态命名习惯，调用层结构化拆解）；离线设备工具不出现（FR-003 验收）；未配置风险等级的工具按租户默认策略（默认 2，先审后用）返回（FR-006 验收）；P95 延迟 200ms（NFR-003）。

#### 3.2.2 POST /v1/agent/mcp/tools/call

- 模块：Agent API + Approval Service（design/30）；用途：工具调用，含风险判定与 HITL 拦截（FR-003/FR-005/FR-006）。
- 鉴权：API Key。请求（两种工具定位方式，推荐结构化字段，SEC-24）：

```json
{
  "device_code": "cnc-lathe-01",
  "tool_name": "set_spindle_speed",
  "arguments": { "rpm": 4200 },
  "timeout_seconds": 20
}
```

兼容形式（无 device_code 时解析 name 的前缀段，解析失败返回 10001）：

```json
{
  "name": "cnc-lathe-01__set_spindle_speed",
  "arguments": { "rpm": 4200 }
}
```

- 响应三种情况：
  1. 免审执行成功 200：

```json
{
  "request_id": "rq_6a7b8c9d",
  "content": [
    { "type": "text", "text": "{\"status\":\"RUNNING\",\"rpm\":4200,\"temperature_celsius\":38.2}" }
  ],
  "is_error": false
}
```

  2. 风险等级不低于 2，进入 HITL 审批，202：

```json
{
  "request_id": "rq_6a7b8c9d",
  "ticket_id": "hitl_8b7c6d5e",
  "polling": "/v1/agent/mcp/tools/call/rq_6a7b8c9d"
}
```

  3. 免审但设备执行错误，200：

```json
{
  "request_id": "rq_6a7b8c9d",
  "content": [ { "type": "text", "text": "drive fault: servo overload" } ],
  "is_error": true
}
```

- 实现为准（align-api-contract-drift）：免审 200 与审批完成查询 200 的响应信封为 `request_id` + 结果字段（`content`、`is_error`，长任务预留 `task_id`/`accepted`），`request_id` 与 202 信封同源、贯穿审计（design/32 6.3 幂等键）；`status`、`execution_duration_ms` 字段为 V1.5 MCP 对齐预留，V1.0 未返回。跨租户设备调用（设备不属于 Key 租户）返回 403 code 13007（SEC-02），不再以 500 隐藏原因。

- 错误码：10002、10003（Key 权限范围不含该工具，FR-009 验收）、11001、11002（设备离线）、11003（设备冻结）、11005、11006、11007（调用超时）、12007（高危工具但审批流未配置，fail-safe 直接拒绝）、13003（租户调用量配额超限）、13007（跨租户设备调用，SEC-02）、10006。
- BLOCKED_BY_HITL 错误结构（审批拒绝或超时，经 3.2.3 查询端点返回）：

```json
{
  "code": 12006,
  "message": "操作已被 HITL 审批拦截",
  "message_key": "errors.hitl.blocked",
  "trace_id": "4f8a2c1e9b3d4f5a8c6d7e8f9a0b1c2d",
  "details": [
    { "ticket_id": "hitl_8b7c6d5e", "decision": "rejected", "approver": "emp_zhangwei", "comment": "参数与工艺单不符" }
  ]
}
```

- 备注：改造点（原方案 7.1 节 1247-1316 行）：删除 X-Tenant-ID 头（SEC-02）；删除关键字硬编码风险判定（1345-1353 行），改读平台 risk_level 配置并经 OPA 决策（SEC-09/ADR-07）；删除 5 分钟同步阻塞等待（原方案 1270-1290 行 InterceptAndWait），改 202 挂起 + 查询模型，避免长连接占用与网关协程悬挂；工具调用全量落审计（SEC-07）；`timeout_seconds` 上限 60，默认 15（NFR-003）；长任务（超出 60 秒）在阶段 2 走 ADR-10 异步任务模型，V1.0 返回 11007。

#### 3.2.3 GET /v1/agent/mcp/tools/call/{request_id}

- 模块：Agent API + Approval Service；用途：查询挂起调用（HITL 审批中）的结果（FR-005）。
- 鉴权：API Key（request_id 归属 Key 的租户与 Key，跨 Key 查询返回 10004）。
- 审批通过且执行完成，200（实现为准：信封与免审 200 一致，无 `status`/`execution_duration_ms`，见 3.2.2）：

```json
{
  "request_id": "rq_6a7b8c9d",
  "content": [ { "type": "text", "text": "Spindle speed set to 4200 RPM successfully" } ],
  "is_error": false
}
```

- 审批中，202（实现为准）：

```json
{
  "request_id": "rq_6a7b8c9d",
  "state": "PENDING"
}
```

- 审批拒绝或超时，403 + BLOCKED_BY_HITL 错误结构（同 3.2.2 展示结构，`decision` 为 rejected 或 expired）。
- 备注：挂起记录在工单终态后保留 24 小时供查询；超时默认 5 分钟（FR-005，可经 FR-007 策略配置调整）；挂起期间设备离线则工单自动失效、查询返回 403 + 11002 details（FR-005 异常场景）。

### 3.3 设备隧道：WSS /v1/devices/tunnel

- 模块：设备 MCP 接入与注册治理（Legacy Bridge 子模块，design/30，社区版 CE）。
- 用途：B 类设备反向接入（FR-001/FR-002），V1.5 起定位为 Legacy Bridge（ADR-15）。
- 鉴权：1.3 节 HMAC 签名（SEC-03）。
- 连接建立后 5 秒内网关主动下发 `tools/list`（FR-001 验收），通道内消息为 JSON-RPC 2.0。

网关下发 tools/list 请求：

```json
{ "jsonrpc": "2.0", "id": "g-0001", "method": "tools/list" }
```

设备响应：

```json
{
  "jsonrpc": "2.0",
  "id": "g-0001",
  "result": {
    "tools": [
      {
        "name": "set_spindle_speed",
        "description": "Set CNC spindle RPM",
        "input_schema": { "type": "object", "properties": { "rpm": { "type": "number" } }, "required": ["rpm"] },
        "risk_suggestion": 2,
        "schema_version": 1
      }
    ]
  }
}
```

网关下发 tools/call 请求：

```json
{
  "jsonrpc": "2.0",
  "id": "g-0002",
  "method": "tools/call",
  "params": {
    "name": "set_spindle_speed",
    "arguments": { "rpm": 4200 }
  }
}
```

设备响应：

```json
{
  "jsonrpc": "2.0",
  "id": "g-0002",
  "result": {
    "content": [ { "type": "text", "text": "Spindle speed set to 4200 RPM successfully" } ],
    "isError": false
  }
}
```

设备执行失败（仍为正常 JSON-RPC 响应，isError=true 表示业务失败）：

```json
{
  "jsonrpc": "2.0",
  "id": "g-0002",
  "result": {
    "content": [ { "type": "text", "text": "drive fault: servo overload" } ],
    "isError": true
  }
}
```

JSON-RPC 错误响应（协议层错误）：

```json
{
  "jsonrpc": "2.0",
  "id": "g-0002",
  "error": { "code": -32004, "message": "tool execution failed", "data": { "tool_name": "set_spindle_speed" } }
}
```

应用层心跳（JSON-RPC 通知，无 id，设备主动发起；网关回应 ack；SEC-17 last_heartbeat 由数据面据此写 PG 与 Valkey）：

```json
{ "jsonrpc": "2.0", "method": "heartbeat", "params": { "sdk_version": "1.2.0", "sent_at": "2026-08-14T08:29:55Z" } }
```

```json
{ "jsonrpc": "2.0", "method": "heartbeat_ack", "params": { "server_time": "2026-08-14T08:29:55Z" } }
```

踢下线通知（凭证吊销/冻结/重复连接时，网关主动下发，设备收到后应主动断开，SEC-15 旧连接清理）：

```json
{ "jsonrpc": "2.0", "method": "kick", "params": { "reason": "credential_revoked" } }
```

- 通道约束：单消息上限 512KB，超限网关断开并告警（FR-001 验收）；网关 60 秒无 Pong 判离线（4.3 节心跳改造保留）；设备重连指数退避 1 秒起步、上限 60 秒（FR-001 验收，SDK 实现）；同设备重复连接旧会话被替代且仅保留最新（FR-001 异常场景，原方案 SEC-15 修复）。
- 备注：改造点（原方案 7.1 节 1189-1226 行 ServeDeviceTunnel）：鉴权替换为 1.3 节 HMAC（SEC-03）；CheckOrigin 白名单校验（SEC-04，原方案 1155-1157 行恒 true）；TLS 强制（SEC-05）；工具上报新增 `risk_suggestion`（设备建议值，仅参考）与 `schema_version`（SEC-22），平台风险等级以配置为准（SEC-09）；设备侧错误码表见 1.5 节 JSON-RPC 段。

### 3.4 HITL 回调

#### 3.4.1 POST /v1/hitl/callback

- 模块：Approval Service（design/30）；用途：审批决策回调（企业微信/钉钉回调服务器转发），一次性消费（FR-005）。
- 鉴权：1.4 节 HMAC 签名（工单级一次性密钥）。
- 请求头：`X-ADC-Timestamp`、`X-ADC-Nonce`、`X-ADC-Signature`、`X-ADC-Request-ID`。
- 请求体：

```json
{
  "ticket_id": "hitl_8b7c6d5e",
  "decision": "approve",
  "approver": "emp_zhangwei",
  "comment": "已核实工艺单，放行"
}
```

- 响应 200：

```json
{
  "ticket_id": "hitl_8b7c6d5e",
  "status": "approved"
}
```

- 错误码：12004（签名校验失败，401；签名缺失同 401，实现为准）、12001（工单不存在，404）、12002（工单已处理，重复回调，409）、12003（工单已过期，403，实现为准）、12005（审批人不在该工单审批人名单）、10001（decision 非 approve/reject，400）。
- 备注：改造点（原方案 7.1 节 1319-1343 行 HandleHITLAction）：由 URL 参数 GET 直改 POST 签名回调；approver 取自企微/钉钉 OAuth 真实身份（SEC-01）；状态机原子 CAS（SEC-11，原方案 6.2 节 952-991 行读改写竞态修复）；审批通过后经 JetStream 唤醒挂起调用（SEC-10，原方案 adc:hitl:resolve 通道无订阅者的修复）；回调成功后一次性消费 nonce 与密钥，任何后续回调 12002。

#### 3.4.2 GET /v1/hitl/action

- 模块：Approval Service（design/30）；用途：审批卡片按钮落地页，重定向到回调确认页（FR-005）。
- 参数：`ticket_id`、`decision`、`expire`、`sig`（1.4 节 URL 签名）。
- 行为：验签通过且工单待处理时，返回 302 重定向至控制台审批确认页 `/console/approvals/{ticket_id}?decision=approve`（企业版，由确认页发起 3.4.1 回调）；验签失败返回 401 HTML 错误页；工单已处理返回 409 提示页（文案无 emoji，英文与中文按租户语言，FR-023 V1.5 起）。
- 备注：改造点（原方案 1319-1343 行）：落地页不再直接改工单状态（原方案 GET 直改的绕过风险，SEC-01），审批动作全部收敛至 3.4.1 签名回调；URL 加签名防转发截获（SEC-13）；`expire` 超时点击返回工单已过期提示。

### 3.5 运维端点

| 端点 | 模块 | 响应 | 说明 |
|------|------|------|------|
| GET /healthz | 运维探针（design/30，社区版 CE） | 200 `{"status":"ok"}` | 存活探针，不查依赖；连续 3 次失败由负载均衡摘除实例（FR-017 验收） |
| GET /readyz | 运维探针 | 200 `{"status":"ready","dependencies":{"postgres":"up","valkey":"up","nats":"up","notify_channels":{"wecom":"up","dingtalk":"up"}}}`；任一依赖不可用 503 | 就绪探针，含审批推送渠道可用性（FR-017 验收：推送成功率纳入看板） |
| GET /metrics | 运维探针 | Prometheus 文本格式 | 仅内网暴露；指标含设备在线数、tools/call 计数与延迟直方图、HITL 拦截/审批时效、限流计数（FR-017、doc/03 埋点指标） |

- 备注：探针无鉴权（1.2 节）；指标命名前缀 `adc_`，延迟直方图分桶覆盖 NFR-003 P95/P99。

---

## 4. 与外部系统接口

### 4.1 企业微信/钉钉审批卡片（引用原方案 6.3 节并更新签名）

改造点：原方案 6.3 节 1033-1109 行卡片按钮 URL 无签名（SEC-13）、文案硬编码中文且含 emoji（GAP-16）、推送失败静默（SEC-18）。V1.0 卡片定义如下（企业微信 template_card）：

```json
{
  "msgtype": "template_card",
  "template_card": {
    "card_type": "button_interaction",
    "main_title": {
      "title": "ADC 高危设备操作审批",
      "desc": "Agent 正在申请调度设备 cnc-lathe-01"
    },
    "horizontal_content_list": [
      { "keyname": "目标设备", "value": "cnc-lathe-01" },
      { "keyname": "触发工具", "value": "set_spindle_speed" },
      { "keyname": "调用参数", "value": "{\"rpm\":4200}" },
      { "keyname": "风险等级", "value": "2（高危，须人工审批）" },
      { "keyname": "申请来源", "value": "排产调度 Agent" }
    ],
    "task_id": "hitl_8b7c6d5e",
    "button_list": [
      {
        "text": "核实并执行",
        "style": 1,
        "key": "approve_hitl_8b7c6d5e",
        "url": "https://adc.example.com/v1/hitl/action?ticket_id=hitl_8b7c6d5e&decision=approve&expire=1755145200&sig=<签名>"
      },
      {
        "text": "拦截终止",
        "style": 3,
        "key": "reject_hitl_8b7c6d5e",
        "url": "https://adc.example.com/v1/hitl/action?ticket_id=hitl_8b7c6d5e&decision=reject&expire=1755145200&sig=<签名>"
      }
    ]
  }
}
```

钉钉 actionCard 结构保持原方案 1090-1106 行字段（`msgtype/actionCard/title/text/btnOrientation/btns`），`actionURL` 替换为上述签名 URL，`text` 中增加风险等级与申请来源两行。

约定：

- 卡片文案走 i18n 词条（V1.0 中文值，键预留，FR-023）；卡片不含 emoji。
- 推送失败指数退避重试 3 次并告警，失败可降级短信提醒（SEC-18）；卡片送达不超过 30 秒（NFR-003）。
- webhook key 经环境变量或密钥管理系统注入，禁止进程参数传递（SEC-13，原方案 main.go 1393 行）。

### 4.2 LLM 网关调用（V1.0 旁路说明）

- V1.0 平台不提供 LLM 网关 API（FR-012 属 V1.5）：Agent 自行直连模型供应商，平台不代理模型调用、不计 Token 费。
- 旁路要求：模型调用内容可能含工具描述、调用参数与响应，客户使用境外模型须自行满足数据出境合规（doc/03 风险 8）；平台 V1.5 提供的 `/v1/llm` 系列端点将内置国产模型默认路由与敏感字段过滤（NFR-005）。
- V1.0 仅采集 Agent 面 tools/call 用量（3.1.17），`llm_tokens` 字段预留为 null。

---

## 5. 兼容与演进

### 5.1 V1.0 端点冻结与演进规则

- V1.0 端点集即第 2 章 27 项；任何破坏性变更升主版本（1.8 节），本版承诺：新增字段一律可选；`name` 聚合命名（`device_code__tool_name`）永久兼容，结构化 `device_code/tool_name` 字段为推荐新用法（SEC-24）。
- Agent API V1.5 升级为标准 MCP Streamable HTTP（ADR-03/SEC-23）：增加 `initialize` 握手与能力协商，路径保持 `/v1/agent/mcp/*` 不变，响应结构按 MCP 规范对齐（`tools/list`、`tools/call` 已与 MCP 契约同构，迁移成本受控）；届时 `/v1/agent/mcp/tools/call/{request_id}` 查询端点由 MCP 通知机制渐进替代，保留至 V2.0。
- 设备隧道 V1.5 起降级为 Legacy Bridge（ADR-15），仅服务 B 类设备；通道契约冻结（JSON-RPC 2.0 + 1.3 节 HMAC），SDK wire 协议随 core-sdk 版本化发布，发布即长期承诺（doc/05 阶段 4 风险）。

### 5.2 V1.5 预留端点

| 端点 | 用途 | 需求 | 说明 |
|------|------|------|------|
| POST /v1/bindings、GET /v1/bindings/{binding_id}、DELETE /v1/bindings/{binding_id} | A 类原生 MCP 设备 OAuth 2.1 客户端凭证鉴权绑定、绑定令牌签发与吊销 | FR-021 | 绑定令牌默认 24 小时有效期、轮换失败自动吊销；绑定成功前平台拒绝接入（ADR-15） |
| GET /v1/llm/models、POST /v1/llm/chat/completions、GET /v1/llm/usage | LLM 网关统一代理、计量 | FR-012 | 主备故障转移 5 秒内切换（FR-012 验收）；token 计量归集租户账单 |
| GET /v1/billing/statements | 账单查询与对账 | FR-016 | 三类计量：设备数、调用量、Token 量 |
| GET /v1/device-groups、POST /v1/device-groups | 设备分组（产线/车间） | FR-011 | 分组用于工具查询、审批策略作用域 |

### 5.3 V1.5 预留（A2A，FR-022，方案 A 决策由 V2.0 前移）

| 端点 | 用途 | 说明 |
|------|------|------|
| GET /.well-known/agent-card.json | 平台 A2A Agent Card 发布 | 双语（中英）能力描述、任务类型、鉴权方式；Card 变更版本化并通知订阅方（FR-022 异常场景） |
| POST /v1/a2a/tasks（A2A 规范任务委派路径） | 外部编排 Agent 委派诊断/排产/巡检任务 | 任务级权限与租户绑定；HITL 审批语义随任务状态机回传（FR-022 验收）；恶意高频委派触发任务级限流 |

### 5.4 协议版本协商（GAP-06）

- Agent 面：V1.5 起经 MCP initialize 握手协商 capabilities 与协议版本；V1.0 客户端以 `X-ADC-Version` 响应头判断兼容。
- 设备面：工具元数据含 `schema_version`（3.2.1/3.3 节），聚合与路由按版本兼容矩阵匹配，新旧设备并存可正确路由（SEC-22）。

### 5.5 版本演进矩阵（V1.0 至 V2.0）

| 端点面 | V1.0（本版） | V1.5 | V2.0 |
|--------|-------------|------|------|
| Agent API | 兼容形态（无 initialize 握手） | 标准 MCP Streamable HTTP + 握手协商；工具描述多语言 | 维持，增加流式通知（ADR-10） |
| 设备接入 | Legacy Bridge WSS（B 类设备） | 新增 A 类原生 MCP 设备 OAuth 2.1 绑定（/v1/bindings）；WSS 降级兼容路径 | 双通道并存，mTLS 逐步推行（ADR-04 终态） |
| HITL | 单级审批 + 签名回调 | 多级审批链、免审白名单策略（FR-007）、卡片多语言 | 审批语义经 A2A 回传委派方 |
| Admin API | 账号密码会话 + RBAC | SSO/OIDC + MFA、批量导入、设备分组 | 维持，增加编排任务管理 |
| 对外协作 | 无 | 无 | A2A Agent Card + 任务委派（FR-022） |

冻结承诺：V1.0 已交付端点与字段在 V1.5 内保持兼容（只增不改）；`name` 聚合命名、HMAC 签名格式、错误码分段为跨版本稳定契约，客户端可安全依赖。

---

## 6. 核心决策清单

1. 租户上下文只来自认证凭证（会话/API Key/设备凭证/工单密钥），全面删除 X-Tenant-ID 头依赖（SEC-02 治理）。
2. 高危工具调用由"同步阻塞 5 分钟"改为"202 挂起 + 查询端点 + BLOCKED_BY_HITL 统一错误结构"，消除长连接悬挂并保持 Agent 侧语义清晰。
3. HITL 审批从"GET URL 直改状态"收敛为"落地页验签 + POST HMAC 签名回调 + 状态机原子 CAS"，回调密钥工单级一次性消费（SEC-01/11/13）。
4. 设备隧道 HMAC 签名含 timestamp 与 nonce，双窗口校验防重放；凭证签发一次性明文返回、库中仅存哈希（SEC-03）。
5. Agent API 工具调用改结构化 device_code/tool_name 定位，`name` 聚合命名仅保留在列表层做生态兼容（SEC-24 修复）。
