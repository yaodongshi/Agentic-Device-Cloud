---
layout: home

hero:
  name: Agentic Device Cloud
  text: Turn every machine into a governed MCP tool node
  tagline: ADC is an AI-native device orchestration and governance platform. Devices register as standard MCP tools, agents discover and call them, and a human approval firewall blocks high-risk operations before they reach physical machines.
  actions:
    - theme: brand
      text: Quick Start
      link: /en/quickstart
    - theme: alt
      text: Architecture
      link: /en/architecture
    - theme: alt
      text: GitHub
      link: https://github.com/yaodongshi/Agentic-Device-Cloud

features:
  - title: Device as MCP node
    details: Devices register their toolchains (tools/list) over a NAT-piercing WSS reverse tunnel. Any MCP-compatible agent can discover and call them without protocol glue.
  - title: HITL approval firewall
    details: Operations at risk level 2 or higher are blocked until a human approves via WeCom or DingTalk interactive cards. Model hallucinations cannot touch physical machines.
  - title: Virtual MCP aggregation
    details: One namespaced MCP endpoint per tenant aggregates every online device tool. Agents plug in with zero changes.
  - title: Multi-tenant governance
    details: Tenants, RBAC, quotas, API keys, and an append-only audit trail (180+ days, partitioned) for regulated industries.
  - title: Unified API gateway
    details: A single Go gateway routes to the Go data plane and the Python agent plane. Clients never see the language split.
  - title: Security baseline
    details: HMAC plus nonce device auth, TLS everywhere, Valkey instead of Redis (licensing), rate limiting, KEK-encrypted credentials.
  - title: Open dual edition
    details: Community edition (LGPL-3.0) is free and self-hostable. Enterprise edition (source-visible commercial) adds governance, scale, and compliance modules.
  - title: Edge SDK
    details: Apache-2.0 core-sdk embeds cleanly into OEM firmware (Rust reference SDK, C demo, Go and Python wire protocol types).
---

## What is ADC?

Traditional IoT platforms treat machines as passive data sources. ADC inverts the model: every device becomes a standard MCP (Model Context Protocol) tool node that AI agents can discover, call, and orchestrate under industrial-grade governance.

```text
External orchestrators (A2A)
          | task delegation
+---------v--------------------------------------+
| Unified API Gateway (Go) - single entrypoint,  |
| language-agnostic routing                      |
+---------+--------------------------------------+
     +----+----------------+
+----v-----+         +-----v-----+
| Go data  |         | Python    |
| plane    |         | agent     |
| (WSS,    |         | plane     |
| MCP,     |         | (LLM GW,  |
| HITL)    |         | A2A, eval)|
+----+-----+         +-----------+
     | WSS (reverse tunnel, NAT traversal)
+----v---------------------------+
| Edge SDK (Rust / C core)       |
| devices to MCP tool nodes      |
+--------------------------------+
```

## Quick start

```bash
git clone https://github.com/yaodongshi/Agentic-Device-Cloud.git
cd Agentic-Device-Cloud
docker compose -f deploy/compose.yaml up -d --build
```

Open the gateway: `http://localhost:18080/` (health aggregation at `/healthz`).

Run the full smoke test (device tunnel, tool aggregation, HITL approval, execution, audit):

```bash
bash scripts/dev-smoke.sh
```

See [Quick Start](/en/quickstart) for the full walkthrough.

## Repository layout

```text
core-sdk/   Apache-2.0  edge SDK and wire protocol (Go + Python + Rust)
ce/         LGPL-3.0    community edition: connector, agent API, approval,
                        audit, gateway, migrations
ee/         commercial  enterprise modules (source-visible, subscription)
deploy/     docker compose stack (gateway / adc / py-agent / postgres / valkey)
docs/       this documentation site
design/     engineering design suite (SRS, HLD, LLD, DB, API, tests, deployment)
doc/        product planning baselines (market, competitive, PRD, business model)
openspec/   spec-driven change management
```

## License

Layered licensing, following the Odoo-style dual edition model:

| Directory    | License                                    |
| ------------ | ------------------------------------------ |
| `core-sdk/`  | Apache-2.0 (embeddable in OEM firmware)    |
| `ce/`        | GNU LGPL-3.0                               |
| `ee/`        | Source-visible commercial license          |

Details: [Open Source](/en/open-source).

## Company

ADC is initiated and sponsored by [Zodioo / 广州众谛信息科技](https://www.zodioo.com/), an Odoo open-source ERP service provider based in Guangzhou, China.
