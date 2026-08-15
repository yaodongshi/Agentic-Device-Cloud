# Architecture

> ADC V1.0 follows a light split between control plane and data plane: 10 modules across four tiers, all clients behind a single unified API gateway. This page summarizes the system design defined in `design/30` (HLD).

## Tiered overview

```text
+------------------------------------+------------------------------------+
|          Control plane (EE)        |          Agent plane (CE)          |
|  M1 Admin API   M2 Console (Web)   |  M9 Python Agent Plane (FastAPI)   |
|  tenants/devices/keys/risk/policy  |  LLM gateway orchestration, Agent  |
|  audit query, usage                |  orchestration, A2A Agent Card,    |
|                                    |  eval toolchain (V1.0 minimal set) |
+-----------------+------------------+------------------+-----------------+
                  |                                    |
                  v                                    v
+---------------------------------------------------------------------+
|                 M10 Unified API Gateway (CE, Go)                    |
|  Single entrypoint for all clients. TLS termination, session        |
|  pass-through, rate limiting, contract validation, path routing.    |
|  /v1/admin/* -> M1   /v1/agent/* -> M4   /v1/devices/tunnel -> M3   |
|  /v1/hitl/* -> M5    /v2/agents/* -> M9                             |
+--------+-----------------+------------------+----------------+------+
         |                 |                  |                |
         v                 v                  v                v
+-----------------+-----------------+-----------------+-----------------+
|       Data plane (CE)                          |  M6 Audit        |
|  M3 Device Connector    M4 Agent API           |  Service (CE)    |
|  Legacy Bridge WSS,     API Key auth, tool     |  buffered events |
|  HMAC auth, sessions,   aggregation, risk      |  to PG, query    |
|  tool catalog, routing  check, HITL handoff,   |  API             |
|  M5 Approval Service    audit events           |                  |
|  HITL ticket state      |                      |                  |
|  machine (PG), IM       +----------+-----------+                  |
|  notification, signed   |          |                              |
|  callbacks              v          v                              |
|                 +--------------------------------+                 |
|                 |  PostgreSQL 16 (state of       |                 |
|                 |  record, audit, quotas)        |                 |
|                 |  Valkey 8 (routing index,      |                 |
|                 |  wakeup, audit buffer, limits) |                 |
|                 +----------------+---------------+                 |
+----------------------------------+---------------------------------+
                                   | WSS reverse tunnel over TLS
                                   | JSON-RPC 2.0, HMAC + nonce auth
+----------------------------------v---------------------------------+
|                Edge (SDK, core-sdk Apache-2.0)                     |
|  M8 Edge SDK: Rust reference SDK / C demo / Go & Python protocol   |
|  types. Wire protocol v1: tools/list, tools/call, heartbeat,       |
|  list_changed. Exponential backoff reconnect, local queue cache.   |
|  Devices: CNC / PLC / sensors / AGV ...                            |
+--------------------------------------------------------------------+
```

Legend: `[CE]` community edition (`ce/`, LGPL-3.0), `[EE]` enterprise edition (`ee/`, source-visible commercial), `[SDK]` edge SDK (`core-sdk/`, Apache-2.0).

## Modules

| Module | Tier | Edition | Responsibility (V1.0) |
| --- | --- | --- | --- |
| M1 Admin API | Control plane | EE | Tenant management, device registry (register, revoke, credential rotation), agent API key issuing, tool risk level configuration, approval policy, audit query, usage query |
| M2 Console | Control plane | EE | Vue 3 management console for the above; approval and audit browsing; i18n-ready UI |
| M3 Device Connector | Data plane | CE | Legacy Bridge WSS access, HMAC device auth, session management, tool catalog sync, routing index (Valkey TTL), tool call dispatch (local fast path, cross-node via message abstraction) |
| M4 Agent API | Data plane | CE | API Key auth with tenant binding, tool aggregation, risk check (DB `risk_level` in V1.0), HITL handoff, sync call timeout (15 s), three-dimension rate limiting, audit event publishing |
| M5 Approval Service | Data plane | CE (single-level) / EE (multi-level) | HITL ticket state machine (PostgreSQL), WeCom/DingTalk cards, signed callback verification, expiry fail-safe (default reject), wakeup of pending calls |
| M6 Audit Service | Data plane | CE | Buffered structured audit events to partitioned PG tables; query API for the console |
| M7 LLM gateway (bypass) | Sidecar | CE base / EE metering | litellm model proxy, token metering; optional, never on the device call path in V1.0 |
| M8 Edge SDK | Edge | SDK | Wire protocol v1, WSS client (TLS, backoff), HMAC signing, tool registration API, local queue cache (no auto replay) |
| M9 Python Agent Plane | Agent plane | CE | FastAPI/uvicorn. V1.0: eval toolchain and dev scripts. V1.5+: LLM gateway orchestration, agent orchestration, A2A Agent Card |
| M10 Unified API Gateway | Gateway | CE | Single entrypoint, path-prefix routing, TLS termination, session pass-through, rate limiting, contract validation |

## Dependency rules

1. **One-way, no cycles.** The arrow directions above are the only allowed call directions.
2. **External clients depend only on M10.** Frontends and agents configure a single gateway address; no direct access to business services or databases.
3. **Control plane and data plane never call each other.** Shared data converges through PostgreSQL.
4. **Business modules only use the message abstraction interface.** No direct Valkey client calls, so the phase 2 swap to NATS JetStream changes no business code.
5. **The LLM gateway bypass never touches the main chain.** Its failure cannot affect device calls.
6. **The console is the only web consumer of M1.** Audit queries go through M6 or M1 delegation.
7. **M9 is process-isolated from the data plane.** Interaction only via internal HTTP and the message bus, always exposed through the gateway.

## Key data flows

### Device onboarding (Legacy Bridge)

```text
Device (SDK)        M3 Connector           Valkey              PostgreSQL
   |  WSS handshake (TLS, Origin allowlist) |                     |
   |---------------------->|                |                     |
   |  HMAC auth (DB credential load,        |                     |
   |  nonce + timestamp check)              |                     |
   |---------------------->|                |                     |
   |<-----session established------|        |                     |
   |  tools/list (with version)   |         |                     |
   |---------------------->|                |                     |
   |<-----tool catalog (risk_level included)--|                     |
   |  routing index: device -> node, TTL 90 s, renew every 30 s    |
   |---------------------->|-------->|       |                     |
   |  tool catalog async-synced to registry  |-------->|           |
   |  tools/list_changed (incremental)      |         |            |
   |  disconnect -> connector clears index   |         |           |
```

### Tool call and HITL approval

```text
Agent      M4 Agent API      M5 Approval      Valkey      M3 Connector     Device
  | tools/call |                |               |             |             |
  |----------->| API Key auth + tenant binding |             |             |
  |            | risk check: risk_level < 2 -> direct route                |
  |            | risk_level >= 2 -> create PG ticket (PENDING)             |
  |            | push IM card (signed callback, bilingual)                 |
  |<-- 202 pending_hitl + ticket_id |          |             |             |
  |  approver clicks signed action URL + OAuth identity                     |
  |            | CAS update PENDING -> APPROVED/REJECTED                    |
  |            | publish wakeup event ----->|                |             |
  |            | route: local direct or cross-node via Valkey               |
  |            |------------------------------------------->|             |
  |            |                                            |--tools/call-->|
  |            |<-------------------------------------------|<--result-----|
  |<-- 200 result | audit event (async) -> buffer -> M6 -> PG                |
```

### Audit flow

```text
Business modules     Valkey (List buffer)     M6 Audit Service     PostgreSQL
  structured event -> RPUSH adc:audit:buffer
                         BLPOP batch consume -> validate -> batch insert
  (append-only, partitioned, 180+ days retention)
```

## Key decisions (V1.0)

| Decision | Choice | V1.0 boundary |
| --- | --- | --- |
| ADR-01 Service split | Light control/data plane split | Shared `ce/` codebase, separate process assembly |
| ADR-02 Message channel | NATS JetStream (target) | V1.0 uses a Valkey adapter behind the message abstraction interface |
| ADR-04 Device credentials | HMAC + nonce + timestamp first, mTLS later | Full rollout on the Legacy Bridge |
| ADR-05 HITL state | PostgreSQL state machine + Valkey wakeup | Direct implementation; tickets are durable and non-repudiable |
| ADR-07 Risk engine | OPA (target) | V1.0 uses DB `risk_level` behind the RiskPolicy interface |
| ADR-12 Repository licensing | Monorepo: `core-sdk/` Apache-2.0, `ce/` LGPL-3.0, `ee/` commercial | Seam interfaces for EE assembly |
| ADR-13 Storage | Valkey instead of Redis (licensing) | Valkey 8 in the V1.0 deployment stack |
| ADR-15 Device transport | Native MCP Streamable HTTP (target), WSS bridge for B-class | V1.0 ships only the Legacy Bridge; A-class onboarding is V1.5 |
| ADR-19 Unified gateway | Go gateway for routing, auth pass-through, rate limiting, contract validation | Frontend-agnostic principle; one gateway address |
| ADR-20 Dual language | Go data plane + Python agent plane | V1.0 delivers the Python eval toolchain; full agent plane from V1.5 |

## Version boundaries

| Capability | V1.0 | V1.5 |
| --- | --- | --- |
| Device onboarding | B-class Legacy Bridge (WSS) | A-class native MCP devices, OAuth 2.1 binding |
| Agent API | MCP-compatible shape, no handshake | Standard MCP Streamable HTTP with `initialize` |
| Risk policy | DB `risk_level`, default 2 (approve first) | OPA policy engine |
| Messaging | Valkey adapter | NATS JetStream |
| HITL | Single-level approval | Multi-level chains, policy-based exemptions |
| Agent plane | Eval toolchain | LLM gateway, four-agent orchestration, A2A Agent Card |

## Security model summary

Six trust boundaries, all defended in V1.0: device (WSS + TLS + HMAC challenge), agent (HTTPS + API Key + tenant binding), approver callback (signed URL + one-time HMAC callback + OAuth identity), inter-node (Valkey ACL + TLS + message signing), notification channels (webhook keys via env injection), and admin (HTTPS + session + RBAC). See `doc/05` for SEC-01 through SEC-25 and the API Reference for the concrete header and error contracts.
