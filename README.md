<p align="center">
  <h1 align="center">Agentic Device Cloud (ADC)</h1>
  <p align="center"><strong>An AI-native device orchestration &amp; governance platform — turn every machine into a governed MCP tool node.</strong></p>
</p>

<p align="center">
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-Layered%20(Apache%2F%20LGPL%2F%20Commercial)-blue" alt="License"></a>
  <a href="https://github.com/yaodongshi/Agentic-Device-Cloud"><img src="https://img.shields.io/github/stars/yaodongshi/Agentic-Device-Cloud" alt="Stars"></a>
  <img src="https://img.shields.io/badge/Go-1.26-00ADD8" alt="Go">
  <img src="https://img.shields.io/badge/Python-3.14-3776AB" alt="Python">
  <img src="https://img.shields.io/badge/status-Early%20Development-orange" alt="Status">
</p>

> 中文简介见文末 [项目简介（中文）](#项目简介中文)。发起公司：广州中迪科技（Zodioo），见 [Company](#company)。

---

## What is ADC?

Traditional IoT platforms treat machines as passive data sources. ADC inverts the model: **every device becomes a standard MCP (Model Context Protocol) tool node** that AI agents can discover, call and orchestrate — under industrial-grade governance.

```
External orchestrators (A2A)
          │ task delegation
┌─────────▼──────────────────────────────┐
│  Unified API Gateway (Go)  — single    │
│  entrypoint, language-agnostic routing │
└─────────┬──────────────────────────────┘
     ┌────┴───────────────┐
┌────▼─────┐        ┌─────▼─────┐
│ Go data  │        │ Python    │
│ plane    │        │ agent     │
│ (WSS,    │        │ plane     │
│ MCP,     │        │ (LLM GW,  │
│ HITL)    │        │ A2A, eval)│
└────┬─────┘        └───────────┘
     │ WSS (reverse tunnel, NAT traversal)
┌────▼──────────────────────────┐
│  Edge SDK (C core / Rust)     │
│  devices → MCP tool nodes     │
└───────────────────────────────┘
```

## Core capabilities

| Capability | Description |
|-----------|-------------|
| **Device as MCP node** | Devices register their toolchains (`tools/list`) over a NAT-piercing WSS reverse tunnel |
| **HITL approval firewall** | High-risk operations (risk level 0-3) are blocked until a human approves via WeCom/DingTalk interactive cards — model hallucinations cannot touch physical machines |
| **Virtual MCP aggregation** | One namespaced MCP endpoint per tenant; any MCP-compatible agent plugs in with zero changes |
| **Multi-tenant governance** | Tenants, RBAC, quotas, API keys, append-only audit trail (180+ days, partitioned) |
| **Unified API gateway** | Single entrypoint routing to Go data plane and Python agent plane — frontends never see the language split |
| **Security baseline** | HMAC+nonce device auth, TLS everywhere, Valkey (not Redis — licensing), rate limiting, KEK-encrypted credentials |
| **Open Odoo-style dual edition** | CE (LGPL-3) free &amp; self-hostable; EE (source-visible commercial) adds governance/scale/compliance modules |

## Quick start

```bash
git clone https://github.com/yaodongshi/Agentic-Device-Cloud.git
cd Agentic-Device-Cloud
docker compose -f deploy/compose.yaml up -d --build
```

Open the gateway: **http://localhost:18080/** (health aggregation at `/healthz`).

Run the full smoke test (device tunnel → tool aggregation → HITL approval → execution → audit):

```bash
bash scripts/dev-smoke.sh
```

## Repository layout

```
core-sdk/   Apache-2.0 — edge SDK & wire protocol (Go + Python)
ce/         LGPL-3.0   — community edition: connector, agent API, approval, audit, gateway, migrations
ee/         commercial — enterprise modules (planned)
deploy/     docker compose stack (gateway / adc / py-agent / postgres / valkey)
design/     approved engineering design suite (SRS, HLD, LLD, DB, API, tests, deployment)
doc/        product planning baselines (market, competitive, PRD, business model, open source strategy)
openspec/   spec-driven change management
```

## Documentation

- [Design suite](design/) — SRS / UIUX / HLD / LLD / database / API / test plan / deployment (Chinese)
- [V1.0 execution plan](design/80-V1.0上线执行计划.md)
- Docs site: planned (docs site is part of the V1.0 release, see the execution plan)

## Roadmap

| Milestone | Scope |
|-----------|-------|
| ✅ Phase 0 | Security hardening (SEC-01~14), dual-repo licenses, Docker stack, 10/10 smoke |
| Sprint 3-9 (V1.0) | Admin API + management console, production HITL (WeCom/DingTalk), cluster routing, edge SDK (Rust box + C demo), CI/CD, docs site, open-source launch |
| V1.5 | Native MCP device onboarding (OAuth 2.1), full Python agent plane (LLM gateway, A2A agent card, four-agent orchestration), i18n UI |
| V2.0 | Protocol adapter ecosystem (FANUC/Siemens/Modbus/OPC-UA), OEM white-label, tool marketplace |

## Company

**ADC is initiated and sponsored by [Zodioo / 广州中迪科技](https://www.zodioo.com/).**

Guangzhou Zhongdi Technology (Zodioo) is an Odoo open-source ERP service provider based in Guangzhou, China. The company delivers Odoo implementation, customization, upgrades and training, with industry solutions for discrete manufacturing, cosmetics/daily-chemicals, cross-border trade &amp; e-commerce, and retail. Rooted in "共同合作，构建开源" (collaborate and build open source), Zodioo brings its years of enterprise digitalization experience to ADC — extending the Odoo-style dual-edition open source philosophy into the AI-native device orchestration domain.

## License

Layered licensing (see [LICENSE](LICENSE)):

| Directory | License |
|-----------|---------|
| `core-sdk/` | Apache-2.0 (embeddable in OEM firmware) |
| `ce/` | GNU LGPL-3.0 |
| `ee/` | Source-visible commercial license |

## Contributing

Contribution guidelines, DCO and issue templates are part of the open-source launch (see the [execution plan](design/80-V1.0上线执行计划.md)). Early feedback via GitHub issues is welcome.

---

## 项目简介（中文）

Agentic Device Cloud (ADC) 是 AI 原生设备编排与治理平台：把物理设备抽象为标准 MCP 工具节点，让 AI Agent 可在 HITL 人工审批熔断的约束下安全地发现、调用与编排设备。项目由广州中迪科技（Zodioo，Odoo 开源 ERP 服务商）发起，延续 Odoo 双版本开源理念：社区版 CE（LGPL-3）免费自部署，企业版 EE 源码可见商业许可。技术栈：Go 数据面 + Python Agent 面 + 统一 API 网关 + Valkey/PostgreSQL，Docker 一键部署。
