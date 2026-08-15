# AGENTS.md

ADC（Agentic Device Cloud）产品化工作区。当前无生产代码，仓库由产品规划文档、专家角色技能与 OpenSpec 变更管理组成。任何"写代码实现"类任务应先通过 openspec 变更提出（`/opsx-propose`），批准后执行（`/opsx-apply`）。

## 工作方式（最重要）

- **产品级工作必须先调用专家**：设计/调研/评审/定价等任务，先用 `skill` 工具加载对应专家技能（`architecture-expert`、`product-manager-expert`、`security-expert`、`open-source-expert` 等，共 28 位，见 `.opencode/skills/`），按其方法论与输出模板产出。用户明确要求"调用专家"。
- 专家分域：市场 `market-analyst`；竞品 `competitive-analyst`；商业模式 `business-model-expert`；增长 `growth-marketing-expert`；产品 `product-manager-expert`；UX/UI `ux/ui-design-expert`；文档 `technical-writing-expert`；架构 `architecture-expert`；后端 `backend-expert`（另有 `go-expert`）；前端 `frontend-expert`；数据库 `database-expert`；DevOps `devops-expert`；安全 `security-expert`；性能 `performance-expert`；AI/算法 `ai-algorithm-expert`；开源 `open-source-expert`；嵌入式固件 `firmware-expert`；工业协议 `iot-protocol-expert`；i18n `i18n-l10n-expert`；测试 `qa-testing-expert`；项目管理 `project-management-expert`；交付 `implementation-delivery-expert`；业务顾问 `business-consultant-expert`；客户成功 `customer-success-expert`；技术支持 `technical-support-expert`；合规法务 `compliance-legal-expert`。
- **多专家协作任务**：并行派出子代理（`task` 工具），每个子代理的 brief 中要求其自行 Read 对应 SKILL.md 与相关基线文档，再产出/修订文件。

## 文档体系（doc/）

- `doc/Agentic Device Cloud.md` — 技术 PoC 基线（含全部 Go 代码）。已过评审，存在 25 项安全问题（见 05），**代码不可直接上线**。
- `doc/01~07` — 产品化规划基线：01 市场调研 / 02 竞品分析 / 03 PRD（FR-xxx）/ 04 商业模式与定价 / 05 架构评审与演进路线（SEC-xxx、ADR-xxx、GAP-xxx）/ 06 开源战略 / 07 设备 MCP 原生接入与 A2A 方案。
- `design/` — 软件工程设计交付物（待审批，见 design/00 索引与审批状态）：10 SRS / 20 UIUX / 30 HLD / 31 LLD / 32 数据库 / 33 API / 40 测试计划 / 50 项目开发计划 / 60 部署方案。V1.0 范围、技术栈、安全红线等设计基线以 design/00 第二节为准。
- 修订规划文档时，**必须全文扫描残留矛盾表述**（历史教训：三仓许可、版本命名、地域口径曾多次不一致）。
- 文档纪律：全中文、无 emoji；测算数字标注来源或"假设"；市场文档用【事实】【推断】【猜测】三级标注。

## 战略基线（所有文档必须保持一致，改动需全量同步）

- Odoo 双版本开源：`core-sdk/`（Apache-2.0，SDK+wire 协议）→ `ce/` 社区版（LGPL-3）→ `ee/` 企业版（源码可见商业许可，禁止再分发）。禁止使用"企业版闭源/专业版/Open Core 闭源"等旧表述。
- 设备主线：原生 MCP Server 设备鉴权绑定（OAuth 2.1 客户端凭证）；原 WSS 反向隧道降级 Legacy Bridge（B 类 SDK 桥接设备）；C 类哑设备不直接支持。
- A2A：平台发布 Agent Card 接受外部编排委派；内部四 Agent（Planner/设备选择/执行/HITL 审批）；MCP 管工具、A2A 管协作。
- 全栈 i18n：中英双语首发、社区英文优先；中国+全球双主场。
- 技术栈：Go + Vue 3 + PostgreSQL + **Valkey（不是 Redis，许可原因，ADR-13）** + NATS JetStream（阶段 2 起）+ MCP Streamable HTTP。
- 执行优先级（来自专家结论）：① 假设 1 付费意愿验证（20 家访谈+5 家试点，6 个月内）→ ② 阶段 0 安全加固（SEC-01~14）→ ③ V0.5→V1.0 交付与开源发布 → ④ 灯塔客户 5-10 家。

## OpenSpec

- 变更流程：`/opsx-propose <名称>` 提出 → 批准 → `/opsx-apply` 执行 → 完成后 `/opsx-archive`。
- 当前活跃变更：`phase0-security-hardening`（阶段 0 安全加固，任务与验收见 openspec/changes/phase0-security-hardening/）。
- 项目上下文与规则：`openspec/config.yaml`；规范沉淀在 `openspec/specs/`。
- 变更文档用中文；proposal 必须含"非目标"节；tasks 每项 ≤ 2 小时且可验证，安全任务引用 SEC-xx 编号。

## 其他约定

- Git 已初始化（main 分支），未配置远端。提交仅在被要求时进行。
- 本仓库无构建/测试/CI；`.opencode/` 下的 package.json 仅承载 plugin 依赖，勿在其中加业务依赖。
- 修改 opencode 配置或技能后，提醒用户重启 opencode 生效。
