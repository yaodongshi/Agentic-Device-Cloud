---
layout: home

hero:
  name: Agentic Device Cloud
  text: 把每一台机器变成受治理的 MCP 工具节点
  tagline: ADC 是 AI 原生设备编排与治理平台。设备以标准 MCP 工具身份注册，AI Agent 发现并调用它们，高危操作在触及物理机器之前被人工审批熔断拦截。
  actions:
    - theme: brand
      text: 快速开始
      link: /zh/quickstart
    - theme: alt
      text: 架构
      link: /zh/architecture
    - theme: alt
      text: GitHub
      link: https://github.com/yaodongshi/Agentic-Device-Cloud

features:
  - title: 设备即 MCP 节点
    details: 设备通过可穿透 NAT 的 WSS 反向隧道注册自己的工具链（tools/list），任何 MCP 兼容的 Agent 都能直接发现与调用，无需协议胶水。
  - title: HITL 审批熔断
    details: 风险等级 2 及以上的操作必须经企业微信或钉钉交互卡片人工审批后才会执行。模型幻觉无法触碰物理机器。
  - title: 虚拟 MCP 聚合
    details: 每个租户一个命名空间化的 MCP 端点，聚合全部在线设备工具，Agent 零改造接入。
  - title: 多租户治理
    details: 租户、RBAC、配额、API Key，以及追加式审计轨迹（180 天以上，分区存储），满足受监管行业要求。
  - title: 统一 API 网关
    details: 单个 Go 网关路由到 Go 数据面与 Python Agent 面。客户端永远感知不到语言切分。
  - title: 安全基线
    details: HMAC 加 nonce 设备鉴权、全链路 TLS、Valkey 替代 Redis（许可原因）、限流、KEK 加密凭证。
  - title: 双版本开源
    details: 社区版 CE（LGPL-3.0）免费可自部署；企业版 EE（源码可见商业许可）提供治理、规模化与合规模块。
  - title: 边缘 SDK
    details: Apache-2.0 的 core-sdk 可放心嵌入 OEM 固件（Rust 参考 SDK、C 演示、Go 与 Python 双语言 wire 协议）。
---

## ADC 是什么

传统 IoT 平台把机器当作被动的数据源。ADC 反转了这个模型：每台设备都是一个标准的 MCP（模型上下文协议）工具节点，AI Agent 可以在工业级治理约束下发现、调用并编排它们。

```text
外部编排 Agent（A2A）
          | 任务委派
+---------v--------------------------------------+
| 统一 API 网关（Go）——唯一入口，语言无关路由     |
+---------+--------------------------------------+
     +----+----------------+
+----v-----+         +-----v-----+
| Go 数据面 |        | Python    |
|（WSS、    |        | Agent 面  |
| MCP、     |        |（LLM 网关、|
| HITL）    |        | A2A、评测）|
+----+-----+         +-----------+
     | WSS（反向隧道，NAT 穿透）
+----v---------------------------+
| 边缘 SDK（Rust / C）           |
| 设备到 MCP 工具节点            |
+--------------------------------+
```

## 快速开始

```bash
git clone https://github.com/yaodongshi/Agentic-Device-Cloud.git
cd Agentic-Device-Cloud
docker compose -f deploy/compose.yaml up -d --build
```

打开网关：`http://localhost:18080/`（健康聚合位于 `/healthz`）。

运行全链路 smoke 测试（设备隧道、工具聚合、HITL 审批、执行、审计）：

```bash
bash scripts/dev-smoke.sh
```

完整步骤见[快速开始](/zh/quickstart)。

## 仓库结构

```text
core-sdk/   Apache-2.0  边缘 SDK 与 wire 协议（Go + Python + Rust）
ce/         LGPL-3.0    社区版：连接器、Agent API、审批、审计、网关、迁移
ee/         商业许可    企业版模块（源码对订阅客户可见）
deploy/     Docker Compose 栈（gateway / adc / py-agent / postgres / valkey）
docs/       本文件站
design/     工程设计交付物（SRS、HLD、LLD、数据库、API、测试、部署）
doc/        产品规划基线（市场、竞品、PRD、商业模式）
openspec/   规格驱动的变更管理
```

## 许可证

延续 Odoo 式双版本开源的分层许可：

| 目录         | 许可证                     |
| ------------ | -------------------------- |
| `core-sdk/`  | Apache-2.0（可嵌入 OEM 固件）|
| `ce/`        | GNU LGPL-3.0               |
| `ee/`        | 源码可见商业许可           |

详见[开源](/zh/open-source)。

## 公司

ADC 由[广州众谛信息科技（Zodioo）](https://www.zodioo.com/)发起与资助，该公司是总部位于广州的 Odoo 开源 ERP 服务商。
