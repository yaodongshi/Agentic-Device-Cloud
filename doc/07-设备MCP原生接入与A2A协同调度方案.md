# 设备 MCP 原生接入与 A2A 协同调度方案

> 视角：AI 算法专家 + 架构专家 + 后端专家
> 基线对齐：《Agentic Device Cloud》技术方案（doc/Agentic Device Cloud.md，下称原方案）与《ADC 架构产品化评审与演进路线》（doc/05-架构产品化评审与演进路线.md，下称评审文档）
> 产品方向基线：原生 MCP 设备为一等公民；遗留设备经边缘 SDK + WSS 反向隧道桥接（Legacy Bridge）；A2A 多 Agent 协同调度；全栈 i18n（中英首发）
> 协议事实均来自公开规范调研，来源标注见文内 [S1]-[S6] 与附录

---

## 0. 结论摘要

1. **设备接入方向**：ADC 面向未来"原生带 MCP Server 运行时的设备"（A 类）作为一级接入路径，平台自身充当 MCP 客户端与注册治理中心，通过 MCP 标准鉴权（OAuth 2.1 客户端凭证授权）完成对设备 MCP Server 的鉴权绑定；原 WSS 反向隧道方案保留但降级为 Legacy Bridge，专用于 SDK 桥接设备（B 类）。
2. **A2A 集成方式**：多 Agent 协同调度设备属 A2A 范畴，平台对外发布 A2A Agent Card，以"设备编排 Agent"身份加入 A2A 网络，接受外部编排 Agent 的任务委派；平台内部采用多 Agent 架构（编排 Planner / 设备选择器 / 执行器 / HITL 审批）。
3. **关键技术选型**：设备侧标准 MCP（Streamable HTTP 传输 + 2025-06-18 版协议）、A2A v1.0（JSON-RPC binding）、HITL 语义嵌入 A2A 任务状态机（input-required 状态），i18n 全栈双语覆盖工具描述、审批卡片、Agent Card 与错误码。

---

## 1. 背景与问题

### 1.1 为什么面向"MCP Server 原生设备"

**设备工具化是不可逆趋势**。传统 IoT 平台把硬件当作被动数据源，而 ADC 的立论是"设备即 MCP 节点"。评审文档 GAP-04 已指出边缘 SDK 无接口契约、生态阶段无法开工；原方案阶段三承诺的嵌入式 MCP SDK 本质上是"平台替设备说话"——设备能力由边缘盒子代理暴露。这一形态的下一步演进方向清晰：设备厂商在固件中直接内嵌 MCP Server 运行时，设备出厂即暴露标准 MCP 端点，能力声明、鉴权、调用全部走标准协议，平台从"协议翻译者"变为"注册治理中心"。

**MCP 正在成为设备互联的事实标准**。MCP 由 Anthropic 于 2024 年 11 月发布并已移交 Linux 基金会托管，当前协议版本 2025-06-18，工具（tools）、资源（resources）、提示词（prompts）三类能力中 tools 是设备场景的核心契约 [S4][S5][S6]。MCP 规范明确允许自定义传输（transport-agnostic），只要保持 JSON-RPC 消息格式与生命周期要求 [S5]，这为"设备上跑 MCP、传输按现场条件选择"留足了空间。工业设备厂商跟进 MCP 的成本远低于适配各家平台私有协议，标准化红利是双向的。

**A2A 补齐了设备协同的上半层**。A2A 协议由 Google 于 2025 年 4 月 9 日发布，已捐赠 Linux 基金会，由 AWS、Cisco、Google、IBM Research、Microsoft、Salesforce、SAP、ServiceNow 八家技术指导委员会成员共同维护，当前版本 v1.0.0（此前 0.3.0/0.2.6/0.1.0），Apache-2.0 许可 [S1][S2][S3]。官方对 MCP 与 A2A 的定位分工明确：**MCP 标准化 Agent 与工具/上下文的连接（agent-to-tool），A2A 标准化 Agent 之间的协作（agent-to-agent）**，二者互补而非竞争 [S1][S2]。ADC 的"外部编排 Agent 委派设备协同任务"场景正好落在 A2A 覆盖范围：能力发现（Agent Card）、任务管理（Task 生命周期）、异步长任务、内置 human-in-the-loop 支持 [S2]。

### 1.2 现有 WSS 私有隧道方案的局限

原方案的反向隧道已验证了 NAT 穿透与工具调用的技术闭环，但作为设备接入的"一级路径"存在三个结构性缺陷：

1. **非标准**。设备侧协议只有 tools/list 与 tools/call，没有 MCP 的 initialize 握手与 capabilities 协商（评审文档 SEC-23 已确认"严格意义上非标准 MCP 端点，标准 MCP 客户端无法直连"）。任何标准 MCP 客户端、任何第三方 Agent 生态都无法消费这些设备。
2. **设备被动**。设备上电后被动等待云端经隧道下发 tools/list，工具目录由云端"拉"取且每次重连全量重建；设备无法主动声明能力变更（MCP 标准有 tools/list_changed 通知机制 [S6]），无法主动推事件，也无法被设备侧第三方工具（本地 Agent、巡检机器人）复用。
3. **生态不兼容**。WSS 隧道是 ADC 私有信道，设备厂商为接入 ADC 需要专门适配，而接入标准 MCP 则可以一次开发、多平台复用。私有协议上的每一分投入都随着 MCP 生态成熟而加速贬值。

结论：WSS 隧道不淘汰，但必须从"唯一通道"降级为"Legacy Bridge 兼容通道"，面向无公网 IP、无法跑完整 MCP Server 的 B 类设备。

---

## 2. 设备 MCP 原生接入设计

### 2.1 设备形态分类

| 类别 | 定义 | 接入方式 | 鉴权 | 定位 |
|---|---|---|---|---|
| A 类：原生 MCP 设备 | 设备固件内嵌 MCP Server 运行时，暴露标准 MCP 端点（Streamable HTTP），支持 MCP 鉴权规范 | 平台 MCP Client 直连 | OAuth 2.1 客户端凭证授权（或 mTLS，见 2.3） | 一级路径，新设备首选 |
| B 类：SDK 桥接设备 | 设备跑边缘 SDK（C 内核 + Rust 绑定，ADR-08），经 WSS 反向隧道桥接 | Legacy Bridge（原方案保留） | HMAC 挑战（ADR-04 方案 B） | 兼容路径 |
| C 类：遗留哑设备 | 仅 Modbus/CAN/EtherCAT 等现场总线，无 MCP 能力 | 不直接支持；经边缘盒子聚合为 B 类接入 | 边缘盒子侧 | 明确排除在直连范围外 |

C 类设备不直接支持是本方案的一条硬边界：平台不为 C 类维护私有驱动协议栈，现场总线适配责任落在边缘 SDK 与盒子厂商，避免平台退化为又一个传统 IoT 网关。

### 2.2 鉴权绑定完整流程

MCP 鉴权规范（2025-06-18 版）规定：授权对 MCP 实现可选，但使用 HTTP 传输的实现 SHOULD 遵循该规范；受保护 MCP Server 充当 OAuth 2.1 资源服务器（Resource Server），MCP 客户端充当 OAuth 2.1 客户端；客户端经 401 + WWW-Authenticate 头发现 `/.well-known/oauth-protected-resource` 元数据，再经 RFC8414 发现授权服务器元数据；访问令牌必须绑定受众（RFC8707 resource 参数）且每次请求以 `Authorization: Bearer` 携带 [S4]。设备绑定场景中平台是机密客户端（confidential client），适用于 OAuth 2.1 的客户端凭证授权（client credentials grant）——A2A 规范同样将 ClientCredentialsOAuthFlow 列为标准安全方案之一，佐证了该模式在 Agent 生态中的通用性 [S2][S4]。

**绑定七步流程**（`Reg`=注册中心，`Dev`=设备 MCP Server，`AS`=设备侧授权服务器，`Plat`=平台 MCP Client）：

```text
 Plat          Reg              Dev(RS)          AS(设备侧)
  |  1.预置身份  |                |                |
  |------------->|  设备出厂预置设备证书/密钥对(产线烧录或首次开机引导)
  |              |  注册:device_id + 公钥/指纹 + 型号 + 默认端点
  |              |                |                |
  |  2.发现端点  |                |                |
  |<-------------|  返回设备 MCP URL (登记或动态发现)
  |--------------GET /.well-known/oauth-protected-resource---->|
  |<-------------RS 元数据(含 authorization_servers)-----------|
  |--------------GET /.well-known/oauth-authorization-server-->|
  |<-------------AS 元数据(RFC8414)-----------------------------|
  |                                                              |
  |  3.客户端凭证授权绑定                                        |
  |--------------POST /token (client_credentials, audience=dev)-->|
  |<-------------access_token(+过期时间, 绑定设备为 audience)-----|
  |                                                              |
  |  4.配对防中间人                                              |
  |  平台用设备预置公钥加密/验证配对随机数 nonce; 令牌绑定设备指纹 |
  |                                                              |
  |  5.长期受信会话                                              |
  |--------------POST /mcp  initialize (Bearer token)----------->|
  |<-------------InitializeResult + Mcp-Session-Id---------------|
  |--------------POST tools/list (分页拉取 + listChanged 订阅)-->|
  |<-------------工具目录(含风险注解) + 版本信息------------------|
  |  平台入库: schema_version/风险等级/多语言描述                 |
  |                                                              |
  |  6.心跳健康: 平台定期探测 + 设备 listChanged 主动通知          |
  |                                                              |
  |  7.吊销/解绑: 令牌撤销(AS) + 平台侧解绑 + 设备证书吊销列表     |
```

要点：

- **步骤 1 设备身份注册**：设备出厂预置非对称密钥对或 X.509 证书，平台仅登记公钥/指纹，私钥永不出设备（消除原方案 SEC-03 硬编码凭证问题）。
- **步骤 3 授权绑定**：平台（机密客户端）持自身 client_id/secret 向设备授权服务器换发绑定设备受众的短时 access_token；规范要求 AS 对机密与公开客户端均实施 OAuth 2.1 安全措施，令牌必须验证 audience，禁止 token passthrough [S4]。
- **步骤 4 防中间人**：绑定令牌经设备公钥加密的配对 nonce 校验后生效，防止初始发现阶段被中间人替换端点；设备同时校验平台身份（平台证书/mTLS），双向信任才建立长期会话。
- **步骤 5 会话**：遵循 Streamable HTTP 的 Mcp-Session-Id 会话管理与 MCP-Protocol-Version 头（如 `2025-06-18`），断连用 Last-Event-ID 恢复 [S5]。
- **步骤 6/7**：心跳与会话健康由平台主动探测（设备侧可能无出网），吊销路径覆盖"平台解绑"与"设备侧令牌撤销"双向。

### 2.3 设备身份方案：X.509/mTLS 与 HMAC 挑战的适用场景

| 方案 | 适用场景 | 优势 | 代价 | 与既有决策关系 |
|---|---|---|---|---|
| X.509/mTLS | A 类高安全设备（CNC 主控、动力设备）；设备可管理证书链 | 双向身份、防中间人、可与 MCP OAuth 叠加 | 证书生命周期管理、嵌入式证书库成本 | 评审文档 ADR-04 终态；A 类首推 |
| HMAC 挑战（nonce + timestamp） | B 类经 WSS 桥接设备；资源受限 MCU | 改动小、计算开销低 | 依赖共享密钥分发、需防重放缓存 | ADR-04 方案 B，Legacy Bridge 沿用 |
| OAuth 2.1 客户端凭证 | A 类设备作为 RS、平台作客户端（2.2 主流程） | 与 MCP 规范一致、令牌可短时轮换 | 设备需内嵌极简 AS 或托管 AS | 本方案新增，A 类默认 |

**密钥轮转**：设备私钥/证书按型号配置最长生命周期（建议 1 年），平台控制面发起轮转工单；HMAC 共享密钥轮转走 Legacy Bridge 的带版本密钥槽（新旧双槽并行 24 小时，新签名逐步切换），杜绝原方案"固定值无限重放"缺陷。轮转期间旧凭据一律可即时吊销（评审文档 SEC-03 验收项）。

### 2.4 工具目录：schema 版本与风险等级元数据

MCP 规范的工具定义含 `annotations` 字段（title、readOnlyHint、destructiveHint、idempotentHint、openWorldHint 五个提示属性），且规范明确警告：**annotations 只是 hints，来自不可信服务端的工具注解不得作为客户端决策依据** [S6]。因此 ADC 采用"设备上报建议值 + 平台权威覆盖"双层模型：

1. **标准层（设备声明）**：设备在 tools/list 中填标准 annotations（如高危写操作 `destructiveHint: true`），保证对任何标准 MCP 客户端都有意义。
2. **扩展层（治理声明）**：平台在聚合工具目录中注入自定义命名空间字段，并对第三方 MCP 客户端透明（自定义字段不破坏互操作）：

```json
{
  "name": "set_spindle_speed",
  "annotations": { "readOnlyHint": false, "destructiveHint": true },
  "x-adc": {
    "schemaVersion": 1,
    "riskLevel": 2,
    "riskPolicyRef": "cnc-write-override",
    "hitlRequired": true,
    "hitlLevel": "single",
    "i18n": { "description": { "zh-CN": "设定主轴转速（高危，需人工审批）", "en": "Set spindle RPM (high risk, HITL required)" } }
  }
}
```

- **schema 版本**：工具接口变更递增 `x-adc.schemaVersion`，平台维护兼容矩阵（评审文档 SEC-22 落地），聚合端点按 Agent 声明的版本路由。
- **风险等级**：对接 adc_device_tools.risk_level（0 只读 / 1 低危 / 2 高危 HITL / 3 极危），风险判定以平台 OPA 策略为准（ADR-07），设备上报仅作初始建议值——彻底替代原方案按工具名关键字判定的绕过漏洞（SEC-09）。
- **i18n**：多语言描述以 `x-adc.i18n` 扩展承载，缺失语言回退标准 `description` 字段（见第 6 章）。
- **同步机制**：工具目录变更经 MCP 标准 `notifications/tools/list_changed` 推送 [S6]，平台增量拉取并更新缓存与台账，替代原方案"重连全量重建"。

### 2.5 数据模型与控制面改动（backend 视角）

原方案三张 PG 表（第 3 章）在评审中确认"定义了却零接入"（GAP-01）。原生接入落地的持久化改动：

```sql
-- 1. adc_devices 扩展：设备类别与 MCP 端点（A/B 类并存）
ALTER TABLE adc_devices
  ADD COLUMN device_class CHAR(1) NOT NULL DEFAULT 'B',   -- 'A' 原生 MCP / 'B' SDK 桥接
  ADD COLUMN mcp_endpoint TEXT,                            -- A 类: https://.../mcp
  ADD COLUMN auth_endpoint TEXT,                           -- A 类: /.well-known 元数据地址
  ADD COLUMN public_key_fp TEXT,                           -- 设备公钥指纹（绑定防中间人）
  ADD COLUMN binding_status VARCHAR(16) DEFAULT 'unbound'; -- unbound/pairing/bound/revoked

-- 2. 新增绑定台账：令牌与审计（MCP 授权绑定留痕）
CREATE TABLE adc_device_bindings (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    device_id UUID REFERENCES adc_devices(id) ON DELETE CASCADE,
    grant_type VARCHAR(32) NOT NULL,           -- client_credentials / mtls / hmac
    token_fingerprint TEXT NOT NULL,           -- 令牌哈希（不存明文，SEC-13 精神）
    audience TEXT NOT NULL,                    -- RFC8707 受众绑定
    issued_at TIMESTAMPTZ, expires_at TIMESTAMPTZ,
    status VARCHAR(16) DEFAULT 'active',
    revoked_by VARCHAR(64), revoked_at TIMESTAMPTZ
);

-- 3. 新增 A2A 委派任务表：与 HITL 工单（ADR-05 的 PG 工单）联动
CREATE TABLE adc_a2a_tasks (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id VARCHAR(64) NOT NULL,
    a2a_task_id VARCHAR(128) NOT NULL,         -- A2A Task.id（幂等键）
    delegator_agent_id VARCHAR(128) NOT NULL,  -- 委派方 Agent 身份
    task_type VARCHAR(64) NOT NULL,            -- diagnose/schedule/patrol/maintenance
    a2a_state VARCHAR(32) NOT NULL,            -- 映射 A2A TaskState
    hitl_ticket_id UUID,                       -- 关联 HITL 工单（高危任务）
    result JSONB, created_at TIMESTAMPTZ DEFAULT NOW(),
    UNIQUE(a2a_task_id)
);
```

一致性边界：绑定状态机（unbound → pairing → bound → revoked）以 PG 为主存，Redis/Valkey 仅缓存在线会话；A2A 任务与 HITL 工单一对多关联，任务级幂等键为 a2a_task_id，重复委派消息（A2A 客户端重试）经唯一约束去重后返回既有任务状态。

---

## 3. A2A 协同调度设计

### 3.1 A2A 与 MCP 的分工

```text
                 外部编排 Agent (A2A 客户端)
                    |  委派任务 (A2A: message/task)
                    v
        +---------------------------------------+
        |   ADC 设备编排 Agent (A2A Server)      |
        |   Agent Card: 能力/任务类型/HITL 语义  |
        |   +-------------------------------+   |
        |   | 内部多 Agent                   |   |
        |   | Planner -> 设备选择 -> 执行    |   |
        |   |    ^               |          |   |
        |   |    |     HITL 审批 |          |   |
        |   +-------------------------------+   |
        +------------------+--------------------+
                           | MCP (agent-to-tool: tools/list, tools/call)
                           v
              +-------------------------+
              | A 类原生 MCP 设备 /      |
              | B 类 Legacy Bridge 设备  |
              +-------------------------+
```

- **MCP = 设备能力层**：回答"设备能做什么、怎么调"，对象是工具（tools），边界到单次调用为止 [S1][S2]。
- **A2A = Agent 协作层**：回答"任务交给谁、如何协商与验收"，对象是任务（Task），天然支持异步长任务、状态流式推送、人工介入（input-required 状态）与跨组织委派 [S2]。
- ADC 的价值定位由此收敛为：**MCP 之上把设备能力组织成可委派、可审批、可审计的任务**，A2A 是这道能力对外发布的标准接口。

### 3.2 ADC 的 A2A Agent Card 设计

A2A 的 Agent Card 是服务端发布的 JSON 元数据文档，描述身份（identity）、能力（capabilities）、技能（skills）、服务端点与认证要求（securitySchemes）；v1.0 支持签名 Agent Card（签名可加密验证 Agent 身份）、多租户端点与多协议绑定 [S2][S3]。ADC 的 Agent Card 设计：

```json
{
  "name": "ADC Device Orchestration Agent",
  "description": { "en": "...", "zh-CN": "..." },
  "version": "1.0.0",
  "protocolVersion": "1.0.0",
  "url": "https://adc.example.com/a2a",
  "securitySchemes": {
    "oauth2_cc": { "type": "oauth2", "flows": { "clientCredentials": {} } }
  },
  "skills": [
    { "id": "device-diagnose",
      "name": { "en": "Equipment Diagnosis", "zh-CN": "设备诊断" },
      "inputSchema": { "tenant": "string", "deviceGroup": "string", "symptom": "string" },
      "outputSchema": { "conclusion": "string", "evidence": "array", "hitlRecords": "array" } },
    { "id": "production-schedule",
      "name": { "en": "Flexible Line Rescheduling", "zh-CN": "柔性排产重构" } },
    { "id": "patrol-inspection",
      "name": { "en": "Patrol & Inspection", "zh-CN": "巡检" } },
    { "id": "maintenance-disposal",
      "name": { "en": "Maintenance Disposal", "zh-CN": "维护处置" },
      "inputSchema": { "actions": "array<toolAction>", "approval": "approvalPolicy" } }
  ],
  "capabilities": { "streaming": true, "pushNotifications": true },
  "x-adc": { "hitlSupported": true, "tenantRequired": true, "riskPolicyVersion": "2026.08" }
}
```

**HITL 在 A2A 委派中的语义**：委派任务在 inputSchema 中携带审批要求（approvalPolicy），高危处置类任务进入 A2A 任务状态机的 `input-required` 状态挂起，等人工审批后再继续——A2A 是 async-first 协议，明确为长时间运行与 human-in-the-loop 场景设计 [S2]，该状态正是把平台既有 HITL 状态机（评审文档 ADR-05 的 PG 工单）映射到 A2A 任务生命周期上。审批进度经 streaming 事件或 push notification 回传委派方（v1.0 支持两种投递机制）[S2][S3]。审批拒绝则任务进入 terminal 的 rejected 状态并携带审批记录。

**HITL 与 A2A 任务状态的映射契约**：

| 平台 HITL 工单状态 | A2A TaskState | 说明 |
|---|---|---|
| 未触发（低危/只读任务） | submitted → working → completed | 直接执行，无人工介入 |
| PENDING（等待审批） | input-required | 挂起；委派方经 GetTask 或推送感知 |
| APPROVED | working → completed/failed | 审批通过后继续执行 |
| REJECTED | rejected | 终态，附审批记录（审批人/意见） |
| EXPIRED（超时） | failed | 超时默认拒绝（fail-safe），附超时原因 |

委派方约定：收到 input-required 状态后不应取消任务（取消即等同于拒绝且丢失审计上下文），应按卡片指引推动审批闭环；双方 SDK 在 ADC 的 A2A 接入点统一校验该约定。

### 3.3 内部多 Agent 架构

| 内部 Agent | 职责 | 上下文策略 | 记忆策略 | 失败重试与降级 |
|---|---|---|---|---|
| 编排 Planner | 解析委派任务 → 分解为设备动作序列，选择任务模板 | 任务 schema + 租户设备能力快照（聚合工具目录摘要，非全量） | 无跨任务长期记忆；任务内状态入 A2A Task | 分解失败重试 2 次换模板；仍失败返回 failed + 原因 |
| 设备选择器 | 在候选设备中按负载/状态/能力匹配选出执行设备 | 设备心跳状态 + 工具兼容矩阵 + 历史成功率 | 设备健康评分滚动窗口（Redis） | 首选设备离线自动改选；无候选则 input-required 请求人工指定 |
| 执行器 | 经 MCP 调设备工具，聚合结果并回传 | 工具调用参数与返回值；结构化输出优先（outputSchema 校验） | 调用审计全量入 PG（ADR-06） | 幂等重试 3 次（幂等键=task_id+step_id）；长任务走 ADR-10 异步模型 |
| HITL 审批 Agent | 对高危动作生成审批工单、推送卡片、收口决策 | 工单上下文（设备/工具/参数/风险等级）+ 委派方信息 | 工单状态机入 PG（ADR-05） | 推送失败重试+降级短信；审批超时默认拒绝（fail-safe） |

关键设计约束：各 Agent 之间不共享隐式上下文，只经结构化消息传递；Planner 不做具体设备操作（防止幻觉直达硬件）；执行器不做风险评估（风险判定是 OPA 单一决策点，ADR-07）。降级链显式存在：设备全离线 → 任务挂起等待设备上线；LLM 网关不可用 → 固定规则模板完成只读诊断类任务（ai-algorithm-expert 原则：永远保留非 LLM 降级路径）。

上下文与记忆策略的细化依据：Planner 的上下文只注入"聚合工具目录摘要"而非全量 schema（全量注入会推高 token 成本并引入跨租户泄露风险，摘要按租户裁剪且脱敏）；设备选择器的健康评分放在 Redis 滚动窗口，天然支持多实例共享与过期淘汰；执行器的调用审计是唯一"事实记忆"——后续任务复盘与评测集回灌都从 PG 审计表取材，而不是依赖模型内部记忆。记忆边界原则：跨任务的知识沉淀走外部存储（PG/审计），Agent 内部只保留单任务工作记忆，防止记忆漂移与幻觉累积。

### 3.4 跨组织委派安全

- **Agent 身份**：外部队员 Agent 以 OAuth 2.1 客户端凭证接入 A2A 端点（securitySchemes 声明）；高价值协作方升级 mTLS。Agent Card 签名用于跨组织发现阶段的身份验证（v1.0 特性）[S3]。
- **租户绑定**：A2A v1.0 支持多租户端点；ADC 强制每个委派任务携带 tenant 并校验与调用方凭证的绑定关系，租户上下文只从凭证解析、绝不信任消息内声明（backend-expert 原则；对应 SEC-02 修复）。
- **任务级权限**：委派只授予"执行某类任务"的最小权限：任务类型白名单 + 设备范围限定 + 每任务动作数/风险等级上限；高危动作的 HITL 审批人必须来自目标租户组织，跨组织委派不得绕过审批。
- **审计与不可抵赖**：委派方身份、任务输入、审批决策全量入审计流（ADR-06），满足跨组织追责。

---

## 4. 协议与传输选型（ADR 摘要）

| 决策点 | 备选方案 | 选择 | 核心理由 |
|---|---|---|---|
| ADR-N01 设备侧 MCP 传输 | A. Streamable HTTP（标准传输）；B. 保留 WSS 私有隧道为主；C. HTTP+SSE（旧版） | A 为主、B 降级为 Legacy Bridge | Streamable HTTP 是当前 MCP 唯二标准传输之一（另一个 stdio 仅限子进程），2025-03-26 起取代 HTTP+SSE [S5]；标准传输才有生态互操作。WSS 保留解决 B 类 NAT 穿透，作为 custom transport 依然符合 MCP"传输无关"约束 [S5]。C 已被规范取代，不选 |
| ADR-N02 设备鉴权 | A. OAuth 2.1 客户端凭证（平台作客户端）；B. 仅 mTLS；C. 沿用 HMAC 挑战 | A（A 类默认），B 用于高安全设备叠加，C 仅 B 类桥接 | A 与 MCP 鉴权规范一致（Server=RS、Client=OAuth 2.1 client，RFC8414/9728/8707 标准元数据发现与 audience 绑定）[S4]，令牌短时、可撤销；B 最强但嵌入式证书成本高（ADR-04 终态）；C 仅覆盖 B 类 |
| ADR-N03 A2A 接入绑定 | A. JSON-RPC 2.0；B. HTTP+JSON/REST；C. gRPC | A | v1.0 提供三种标准绑定 [S2]；JSON-RPC 与既有 MCP 栈同源（消息形态一致），官方 Python/Go SDK 成熟，团队技术栈（Go）有 a2a-go SDK 可复用 |
| ADR-N04 A2A 版本基线 | A. 仅 v1.0；B. v0.3 与 v1.0 双协议并行 | B（过渡期） | Agent Card 在 v1.0 向后兼容演进，允许同时声明 v0.3 与 v1.0 协议 [S3]；双协议并行让存量客户端渐进迁移，避免单点切换 |
| ADR-N05 Agent Card 形态 | A. 每租户独立卡片（单租户端点）；B. 多租户单端点 + tenant 参数；C. 动态注册卡片 | B | v1.0 原生支持多租户端点 [S3]；C 引入未标准化的发现复杂度；B 运营成本最低且签名管理集中 |
| ADR-N06 工具风险元数据 | A. 仅标准 annotations；B. 仅平台 DB 策略；C. 标准 annotations + x-adc 扩展双层 | C | 规范明确 annotations 不可信且仅 hints [S6]，A 不安全；B 丢生态互操作；C 兼顾互操作与治理权威（2.4 节） |

---

## 5. i18n 设计

语言基线：en + zh-CN 首发，语言参数统一 `Accept-Language` 与消息体 `locale` 字段双通道，默认 en、缺译回退 zh-CN 或标准字段原文。

| 范围 | 方案 | 与全栈衔接 |
|---|---|---|
| 设备工具描述 | 标准 description 字段保持一种语言（默认 en）；`x-adc.i18n.description.<lang>` 承载多语言；聚合端点按 Agent locale 下发 | 工具缓存表（adc_device_tools）增加 i18n JSONB 列；LLM 网关按会话语言注入工具说明 |
| 审批卡片 | 企微/钉钉卡片文案入资源包，按审批人语言偏好渲染；标题/按钮/风险说明全部双语 | 原方案 6.3 节 notifiers 改造为文案模板 + 资源包注入，杜绝硬编码中文 |
| Agent Card | skills.name/description 为多语言对象；inputSchema/outputSchema 的 property description 双语；受控字段（x-adc.hitlSupported 等）单语 | A2A 发现阶段按请求方 Accept-Language 返回对应语言副本，签名对规范化 JSON 生效（与签名要求一致）[S2] |
| 错误码 | 错误码稳定英文 ID（如 HITL_EXPIRED、DEVICE_OFFLINE），客户端经资源包映射本地化文案；消息携带可检索 traceId | 与后端错误码体系统一：ID 是契约，文案是表现层，禁止以文案判分支 |
| HITL 工单与审计 | 工单展示文案双语；审计记录原文存储（不可翻译，保证证据效力），控制台按语言渲染说明 | 与评审文档 ADR-05/06 的 PG 工单与审计表衔接 |

回归约束：i18n 资源包纳入 CI 校验（缺 key 阻断、占位符数量一致），防止语言分支引入功能分叉。资源包管理约定：语言资源以 key-value JSON 组织并随服务版本发布，客户端/卡片渲染方自带语言包副本，服务端仅下发稳定 key（与错误码同一原则：key 是契约、文案可演进）；新增语言的合入不要求改协议，只要求补全资源包并过 CI 门禁。审批卡片渲染优先级：审批人显式语言偏好 → 组织默认语言 → 平台默认（zh-CN）。

---

## 6. 评测与验证方案（AI 能力部分）

### 6.1 任务定义与验收标准

- **评测对象**：A2A 委派任务在 ADC 多 Agent 编排链路中的端到端表现，含设备调度、HITL 触发、时延与幻觉四项核心指标。
- **成功判据**：任务在目标设备上执行且结果可校验（与预期动作集一致）；HITL 应触发时必触发、不应触发时不触发；高危动作未经审批零执行。
- **硬约束**：端到端时延（非 HITL 任务 P95 ≤ 30 秒含 LLM 推理与设备往返；HITL 挂起不计时延，审批后恢复 P95 ≤ 15 秒）；幻觉导致的无效/错误设备动作率 ≤ 1%；评测集规模基线 300 条，含 30 条对抗样本。
- **评测集构造**：60% 来自模拟设备沙箱（mock CNC/PLC/AGV，含离线、超时、参数越界注入），30% 来自真实设备联调记录回放，10% 为人工构造对抗样本（近义高危改写如 set_spindle_speed → set_rpm、跨租户设备 ID 注入、审批绕过尝试）。

### 6.2 指标与目标

| 用例组 | 数量 | 指标 | 目标 | 基线计划 | 优化后目标 |
|---|---|---|---|---|---|
| 设备调度正确性 | 120 | 任务成功率（设备选择/工具路由/参数正确） | ≥ 95% | 先用最强模型打上限，再降级到候选模型 | ≥ 97% |
| HITL 触发准确率 | 60 | 高危动作触发率（召回）/ 低危动作误触发率（误报） | 召回 100%，误报 ≤ 2% | 规则基线（OPA 策略）直接评测，LLM 仅作辅助判级 | 召回 100%，误报 ≤ 1% |
| 端到端时延 | 60 | P95 时延（非 HITL / HITL 恢复后） | ≤ 30s / ≤ 15s | 分段埋点定位 LLM 推理、MCP 往返、审批链路占比 | P95 ≤ 20s / ≤ 10s |
| 幻觉拦截 | 40 | 无效工具名/越权参数/虚构设备 ID 拦截率 | 100% 拦截于执行层 | 工具调用前置校验（schema 校验 + 设备存在性 + OPA）不依赖 LLM 自觉 | 保持 100%，拦截原因可解释 |
| 降级与故障注入 | 20 | 设备离线/网关降级下任务不丢、可恢复 | 100% 状态一致 | 断连重放与幂等键验证 | 不变 |

### 6.3 基线实验计划

1. **规则基线先行**：不接 LLM，用固定脚本模拟 Planner/执行器，验证 MCP 调用链、HITL 状态机、A2A 任务生命周期（input-required 挂起/恢复/拒绝）的正确性——协议层正确性是 AI 评测的前提。
2. **上限基线**：用最强模型（云 API）跑通全量评测集，得成功率与时延上限；随后逐级替换小模型/本地模型，对比同一评测集（ai-algorithm-expert 原则：一次只改一个变量）。
3. **对抗轮**：30 条对抗样本单独出报告，任何"未审批高危执行"为阻断项，直接退回修复。
4. **线上回灌**：上线后把 HITL 拒绝案例、设备离线改选案例、人工纠正的调度结果回灌评测集，每迭代扩充。

### 6.4 评测流水线与人工抽检

评测进 CI/发布门禁：协议一致性用例（A2A/MCP 契约测试）每次合并必跑；AI 指标评测集每发布候选版全量跑，附"指标对比上一版 + 失败样例"报告，任一核心指标劣化超过 3% 阻断发布。人工抽检按 5% 比例随机复查机器判分的任务结果（重点复查 HITL 边界案例与降级案例），机器判分与人工判分一致性低于 95% 时冻结该评测集判分规则并修订。沙箱环境支持故障注入脚本（设备离线/超时/参数越界/审批超时）作为标准前置用例，全部通过才进入真实设备联调阶段。

---

## 7. 风险与开放问题

| 风险 | 等级 | 缓解 |
|---|---|---|
| MCP 规范快速演进（2024-11/2025-03/2025-06 三版） | 中 | 锁定 2025-06-18 版本基线 + MCP-Protocol-Version 头协商 [S5]；升级经兼容矩阵灰度，聚合层屏蔽版本差异 |
| A2A 生态早期，委派方数量不确定 | 中 | Agent Card 同时声明 v0.3/v1.0（ADR-N04）；A2A 与平台自有 REST API 双通道并存，A2A 不取代既有接口 |
| 原生 MCP 设备尚未规模化（先发风险） | 高 | 首批发 A 类参考实现与认证测试套件；B 类 Legacy Bridge 保持主力营收路径，A 类按"先行者收益"评估而非短期收入 |
| annotations 不可信 / 设备伪造能力声明 | 高 | 风险与权限以平台 OPA 权威判定（2.4 节）；设备身份强认证；声明与行为不符的设备自动降权 |
| 跨组织委派放大攻击面（confused deputy / token passthrough） | 高 | 严格 audience 绑定校验、禁止令牌透传 [S4]；任务级最小权限 + 租户强制绑定（3.4 节） |
| 设备侧无出网能力（A 类假设失验） | 中 | A 类设备需具备可被平台访问的端点（本地网络/边缘接入点），不具备者归入 B 类；假设在首批真实设备验证 |
| i18n 与协议签名耦合（多语言 Agent Card 签名一致性） | 低 | 签名对象为语言无关结构 + 语言包外置引用；语言副本不改变受签名保护的断言字段 |

开放问题：设备授权服务器（AS）由设备内嵌还是平台托管尚未定案，倾向"平台托管 AS + 设备验证令牌"降低嵌入式复杂度，需与首批设备厂商联合评估；A2A 生态的 Agent 发现协议（well-known URI 注册）在 v1.0 已有定义 [S2]，但跨组织目录服务仍无成熟实践，暂不承诺。

---

## 附录：调研来源

- [S1] Google Developers Blog：Announcing the Agent2Agent Protocol (A2A)，2025-04-09，https://developers.googleblog.com/en/a2a-a-new-era-of-agent-interoperability/
- [S2] A2A 协议官方文档与规范（v1.0.0，Linux 基金会托管，Apache-2.0），https://a2a-protocol.org/latest/specification/
- [S3] A2A Blog：A2A Protocol Ships v1.0，https://a2a-protocol.org/latest/announcing-1.0/
- [S4] MCP 规范 2025-06-18：Authorization，https://modelcontextprotocol.io/specification/2025-06-18/basic/authorization
- [S5] MCP 规范 2025-06-18：Transports（Streamable HTTP），https://modelcontextprotocol.io/specification/2025-06-18/basic/transports
- [S6] MCP 规范 2025-06-18：Server Tools 与 schema/2025-06-18/schema.json（ToolAnnotations），https://modelcontextprotocol.io/specification/2025-06-18/server/tools
