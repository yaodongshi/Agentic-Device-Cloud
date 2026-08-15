# Roadmap

> Public roadmap, aligned with the execution priority agreed in the product baseline. Two quarters of community roadmap are always visible; dates are targets, not promises.

## Execution priority

1. Validate the willingness-to-pay assumption: 20 customer interviews plus 5 pilot deployments within 6 months.
2. Phase 0 security hardening (SEC-01 through SEC-14).
3. V0.5 to V1.0 delivery and the open source launch.
4. 5-10 lighthouse customers.

## Phase 0 - Done

| Item | Scope |
| --- | --- |
| Security hardening | SEC-01 through SEC-14 fixes: signed HITL callbacks, tenant-bound API keys, device HMAC nonce anti-replay, TLS everywhere, ticket state in PostgreSQL, rate limiting, audit persistence, routing index renewal |
| Layered licenses | Three-directory licensing: `core-sdk/` Apache-2.0, `ce/` LGPL-3.0, `ee/` commercial |
| Docker stack | Full Compose deployment (gateway, adc, py-agent, postgres, valkey) |
| Smoke test | End-to-end 10/10: tunnel, aggregation, HITL, execution, audit |

## V1.0 (Sprint 3-9)

| Item | Scope |
| --- | --- |
| Admin API and console | Tenant and device registry, tool risk configuration, approval policy, audit browsing, usage (enterprise edition) |
| Production HITL | WeCom and DingTalk cards, signed callbacks, OAuth approver identity, expiry fail-safe |
| Cluster routing | Cross-node tool call routing over the message abstraction |
| Edge SDK | Rust reference SDK, C demo, Go and Python wire protocol types |
| CI/CD | License scan, SBOM, vulnerability and secret scans, multi-arch images |
| Docs site | This site: quick start, architecture, API reference, SDK guide, open source rules |
| Open source launch | Governance files, DCO, public roadmap, first release with SBOM |

## V1.5

| Item | Scope |
| --- | --- |
| Native MCP devices | A-class devices onboard via OAuth 2.1 client credentials; WSS tunnel becomes the Legacy Bridge for B-class devices |
| Python agent plane | LLM gateway orchestration (multi-provider routing, metering, failover), agent orchestration (Planner, device selection, executor, HITL), A2A Agent Card publishing and delegation |
| Standard MCP | Agent API upgrades to MCP Streamable HTTP with `initialize` handshake |
| Messaging and policy | NATS JetStream replaces the Valkey adapter; OPA replaces the DB risk policy; both behind frozen interfaces |
| i18n UI | Full Chinese and English console, tenant-language error messages |
| Long tasks | Async execution model beyond the 60 s sync ceiling |

## V2.0

| Item | Scope |
| --- | --- |
| Protocol adapter ecosystem | FANUC, Siemens, Modbus, OPC-UA adapters |
| OEM white-label | Branded deployments for equipment vendors (channel agreement required) |
| Tool marketplace | Community and vendor tool catalogs |
| A2A depth | Agent Card and task delegation per the A2A specification |

## Version stability promises

- Endpoints, fields, and error codes shipped in V1.0 stay compatible within V1.5 (additive only).
- The aggregated tool naming (`device_code__tool_name`), the HMAC signature format, and the error code segments are cross-version stable contracts.
- The wire protocol v1 ships in `core-sdk/` with semantic versioning and Go/Python implementations generated from the same contract.
