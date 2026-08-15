# API Reference

> The full V1.0 API contract, condensed from `design/33`. Covers the Admin API (REST), the Agent API (MCP-compatible), the device tunnel (JSON-RPC 2.0 over WSS), HITL callbacks, and operational endpoints. 27 endpoints in total.

## API styles

| Surface | Style | Transport | Edition |
| --- | --- | --- | --- |
| Admin API | REST, resource paths, JSON | HTTPS | Enterprise (EE) |
| Agent API | MCP-compatible (standard MCP Streamable HTTP from V1.5) | HTTPS | Community (CE) |
| Device tunnel | JSON-RPC 2.0 over WSS (Legacy Bridge, B-class devices) | WSS (TLS mandatory) | Community (CE) |
| HITL callback | Signed POST callback + landing page GET | HTTPS | Community (CE) single-level |

## Common conventions

- All APIs use the `/v1` prefix. Requests are JSON with `Content-Type: application/json`; unknown request fields are ignored; responses never carry undefined fields.
- Timestamps are RFC3339 UTC (for example `2026-08-14T08:30:00Z`). The console renders them in the tenant timezone.
- Field names are `snake_case` and mirror `design/32` table columns.
- `device_id`, `request_id`, `ticket_id`, and `log_id` are UUIDs; `device_code` is a business-unique code (letters, digits, hyphen, underscore, length 1-128).
- Pagination: offset style (`page`, `page_size`, default 20, max 200, returns `items` + `total`) for list pages; cursor style (`limit`, `cursor`, returns `items` + `next_cursor`) for audit logs.
- Idempotency: `Idempotency-Key` (UUID) supported on `POST /v1/tenants`, `POST /v1/devices`, `POST /v1/agent-keys`, `POST /v1/devices/import`. Reuse within 24 hours returns the original response; same key with a different body returns 409 code 10009.
- Versioning: `/v1` is the API major version. Minor version is declared in the `X-ADC-Version` response header (for example `2026-08`). Only additive changes: no rename, no deletion, no semantic change without a `/v2` bump.
- All business endpoints return `X-RateLimit-Limit`, `X-RateLimit-Remaining`, `X-RateLimit-Reset`, `X-ADC-Request-ID`, and `X-ADC-Version` headers. Rate limit exceeded returns 429 code 10006 with `Retry-After`.

## Authentication

| Surface | Method | Credential | Tenant context |
| --- | --- | --- | --- |
| Admin API | Session auth (cookie `adc_session`, HttpOnly, Secure, SameSite=Lax) + RBAC | Cookie | Parsed from the session |
| Agent API | API Key | `X-ADC-Key: adc_<id>_<secret>` or `Authorization: Bearer adc_<id>_<secret>` | Parsed from the key binding |
| Device tunnel | HMAC-SHA256 + nonce + timestamp | WSS upgrade headers | Parsed from the device registry |
| HITL callback | HMAC-SHA256 (per-ticket one-time key) | `X-ADC-Timestamp`, `X-ADC-Nonce`, `X-ADC-Signature` headers | Parsed from the ticket |
| Ops endpoints | None | Not applicable | Not applicable (internal networks only) |

Hard rule (SEC-02): no business endpoint accepts `X-Tenant-ID` as the source of tenant context. The tenant always comes from the credential. A mismatched explicit tenant parameter returns 403 code 13007.

Admin roles: `platform_admin` (all Admin APIs, the only role that creates or disables tenants), `tenant_admin` (all resources of its own tenant), `approver` (read-only device list and ticket queries; approval actions go through the HITL callback), `auditor` (read-only everywhere).

## Endpoint master table

| # | Method | Path | Auth | Purpose | Edition |
| --- | --- | --- | --- | --- | --- |
| 1 | POST | /v1/auth/login | none (issues session) | Admin login, session cookie | EE |
| 2 | GET | /v1/tenants | session + RBAC | Tenant list (platform admin) | EE |
| 3 | POST | /v1/tenants | session + RBAC | Create tenant with quota | EE |
| 4 | GET | /v1/tenants/{tenant_id} | session + RBAC | Tenant detail and quota | EE |
| 5 | PATCH | /v1/tenants/{tenant_id} | session + RBAC | Update quota, disable/enable | EE |
| 6 | POST | /v1/devices | session + RBAC | Register device, issue credential | EE |
| 7 | GET | /v1/devices | session + RBAC | Device list and filters | EE |
| 8 | PATCH | /v1/devices/{device_id} | session + RBAC | Freeze/unfreeze, revoke, reset credential | EE |
| 9 | POST | /v1/devices/import | session + RBAC | CSV bulk import (V1.5 reserved) | EE |
| 10 | GET | /v1/devices/{device_id}/tools | session + RBAC | Device tools and risk levels | EE |
| 11 | PATCH | /v1/devices/{device_id}/tools | session + RBAC | Tool risk level and enable/disable | EE |
| 12 | POST | /v1/agent-keys | session + RBAC | Issue Agent API Key | EE |
| 13 | GET | /v1/agent-keys | session + RBAC | API Key list | EE |
| 14 | DELETE | /v1/agent-keys/{key_id} | session + RBAC | Revoke API Key | EE |
| 15 | GET | /v1/approval-tickets | session + RBAC | Approval ticket list | EE |
| 16 | GET | /v1/audit-logs | session + RBAC | Audit log query and export | EE |
| 17 | GET | /v1/usage | session + RBAC | Quota usage | EE |
| 18 | GET | /v1/org/users | session + RBAC | Organization members and roles | EE |
| 19 | GET | /v1/agent/mcp/tools | API Key | Aggregated tool list | CE |
| 20 | POST | /v1/agent/mcp/tools/call | API Key | Tool call (with HITL interception) | CE |
| 21 | GET | /v1/agent/mcp/tools/call/{request_id} | API Key | Pending HITL call result query | CE |
| 22 | WSS | /v1/devices/tunnel | HMAC + nonce | Device reverse tunnel (JSON-RPC 2.0) | CE |
| 23 | POST | /v1/hitl/callback | HMAC signature | Approval decision callback | CE |
| 24 | GET | /v1/hitl/action | URL signature | Approval card landing page | CE |
| 25 | GET | /healthz | none | Liveness probe | CE |
| 26 | GET | /readyz | none | Readiness probe (dependency check) | CE |
| 27 | GET | /metrics | none (internal) | Prometheus metrics | CE |

## Admin API

### POST /v1/auth/login

Issues a session for the admin console.

```json
{
  "username": "itadmin@example-factory.com",
  "password": "********",
  "mfa_code": "123456"
}
```

Response 200 (session delivered via `Set-Cookie: adc_session=...`):

```json
{
  "user_id": "u_9f8e7d6c",
  "display_name": "Factory IT Admin",
  "tenant_id": "t_1a2b3c4d",
  "role": "tenant_admin",
  "expires_at": "2026-08-15T08:30:00Z"
}
```

Errors: 10001 (invalid format), 10002 (wrong credentials; 5 consecutive failures lock the account for 15 minutes and write an audit event), 13002 (tenant disabled), 13001 (tenant not found). `mfa_code` is required when MFA is enabled in EE.

### POST /v1/devices

Registers a device and issues its credential. The credential is returned exactly once; only its hash is stored.

```json
{
  "device_code": "cnc-lathe-01",
  "name": "Lathe 01",
  "device_type": "cnc",
  "auth_type": "hmac",
  "group_ids": [],
  "metadata": { "workshop": "A1", "vendor": "Siemens" }
}
```

Response 201:

```json
{
  "device_id": "d_7a6b5c4d",
  "device_code": "cnc-lathe-01",
  "name": "Lathe 01",
  "device_type": "cnc",
  "auth_type": "hmac",
  "status": "offline",
  "credential": { "secret": "adc_dev_sec_xxxxxxxxxxxxxxxx" },
  "metadata": { "workshop": "A1", "vendor": "Siemens" },
  "created_at": "2026-08-14T08:00:00Z"
}
```

Errors: 11008 (device code exists), 10001 (invalid code, SEC-20 format), 11010 (device quota exceeded), 10003. `auth_type` is one of `token`, `hmac`, `mtls`; with mTLS, `credential.cert_url` replaces `secret`.

### PATCH /v1/devices/{device_id}

Lifecycle operations via the `op` field: `freeze` (kick and block), `unfreeze`, `revoke_credential` (device must re-register), `reset_credential` (new credential, old one valid for a 24 h transition), `update_meta`. `change_reason` is mandatory and written to the audit trail.

```json
{
  "op": "reset_credential",
  "change_reason": "Engineering change; old credential retired",
  "name": "Lathe 01 (A1 workshop)"
}
```

Response 200 includes `credential` only for `op=reset_credential`. Errors: 11001, 10001, 11003.

### POST /v1/agent-keys

Issues an Agent API Key. The full key appears exactly once; only the hash is stored.

```json
{
  "name": "Scheduling Agent",
  "scopes": { "allowed_tools": ["*"] },
  "expires_at": "2026-11-14T08:00:00Z"
}
```

Response 201:

```json
{
  "key_id": "k_3c4d5e6f",
  "name": "Scheduling Agent",
  "key": "adc_3c4d5e6f_xH7kP2mQ9vL4nB8tR6wY",
  "key_prefix": "adc_3c4d5e6f",
  "scopes": { "allowed_tools": ["*"] },
  "status": "active",
  "expires_at": "2026-11-14T08:00:00Z",
  "created_at": "2026-08-14T09:10:00Z"
}
```

`scopes.allowed_tools` matches tool names: `*` for all, `device_code__*` prefixes, `*__tool_name` suffixes; an empty array grants nothing. Errors: 10001 (expired or beyond 365 days), 13003 (key quota exceeded). Revocation takes effect immediately (401 on next use).

### GET /v1/devices/{device_id}/tools and PATCH /v1/devices/{device_id}/tools

The read endpoint returns the authoritative tool list with platform-configured risk levels:

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
    }
  ],
  "total": 1,
  "page": 1,
  "page_size": 50
}
```

The write endpoint updates risk levels and enablement per tool name; downgrading a tool from risk 2 or higher to 1 or lower requires the `X-ADC-Confirm: true` header. Config changes reach the data plane within 30 seconds.

```json
{
  "changes": [
    { "name": "set_spindle_speed", "risk_level": 2, "is_enabled": true }
  ],
  "change_reason": "Line safety review decision"
}
```

Errors: 11001, 11005, 10001.

## Agent API

### GET /v1/agent/mcp/tools

Tenant-scoped aggregated tool list. Auth: API Key (tenant resolved from the key, never from headers).

```json
{
  "tools": [
    {
      "name": "cnc-lathe-01__get_spindle_status",
      "description": "[cnc-lathe-01] Read CNC spindle RPM and temperature",
      "input_schema": { "type": "object" },
      "device_code": "cnc-lathe-01",
      "device_name": "Lathe 01",
      "device_status": "online",
      "risk_level": 0,
      "schema_version": 1
    }
  ]
}
```

Only tools of online devices appear. The aggregated `name` (`device_code__tool_name`) is permanent for ecosystem compatibility; the structured `device_code` field is the recommended way to call. Tools without a configured risk level fall back to the tenant default (2, approve before use). P95 latency target: 200 ms.

### POST /v1/agent/mcp/tools/call

Calls a device tool. Risk level below 2 executes directly; risk level 2 or higher creates an HITL ticket and suspends the call.

Structured form (recommended):

```json
{
  "device_code": "cnc-lathe-01",
  "tool_name": "set_spindle_speed",
  "arguments": { "rpm": 4200 },
  "timeout_seconds": 20
}
```

Compatible form (resolves the `name` prefix; parse failure returns 10001):

```json
{
  "name": "cnc-lathe-01__set_spindle_speed",
  "arguments": { "rpm": 4200 }
}
```

`timeout_seconds` range: 1-60, default 15.

Response 1: direct execution, 200.

```json
{
  "request_id": "rq_6a7b8c9d",
  "status": "completed",
  "content": [
    { "type": "text", "text": "{\"status\":\"RUNNING\",\"rpm\":4200,\"temperature_celsius\":38.2}" }
  ],
  "is_error": false,
  "execution_duration_ms": 328
}
```

Response 2: HITL approval required, 202.

```json
{
  "request_id": "rq_6a7b8c9d",
  "status": "pending_hitl",
  "ticket_id": "hitl_8b7c6d5e",
  "expire_at": "2026-08-14T08:20:00Z",
  "status_url": "/v1/agent/mcp/tools/call/rq_6a7b8c9d"
}
```

Response 3: direct execution with device-side business failure, 200 with `is_error: true`.

Errors: 10002, 10003 (key scope excludes the tool), 11001, 11002 (device offline), 11003 (device frozen), 11005, 11006, 11007 (timeout), 12007 (high-risk tool without a configured approval flow: fail-safe reject), 13003 (call quota exceeded), 10006.

### GET /v1/agent/mcp/tools/call/{request_id}

Polls a suspended HITL call. Auth: the API Key that owns the request (cross-key access returns 10004).

Approved and executed, 200:

```json
{
  "request_id": "rq_6a7b8c9d",
  "status": "completed",
  "content": [ { "type": "text", "text": "Spindle speed set to 4200 RPM successfully" } ],
  "is_error": false,
  "execution_duration_ms": 2540
}
```

Still pending, 200: same shape as the 202 above with `status: "pending_hitl"`.

Rejected or expired: 403 with the BLOCKED_BY_HITL error structure:

```json
{
  "code": 12006,
  "message": "Operation blocked by HITL approval",
  "message_key": "errors.hitl.blocked",
  "trace_id": "4f8a2c1e9b3d4f5a8c6d7e8f9a0b1c2d",
  "details": [
    { "ticket_id": "hitl_8b7c6d5e", "decision": "rejected", "approver": "emp_zhangwei", "comment": "Parameters differ from the process sheet" }
  ]
}
```

Pending records are queryable for 24 hours after the ticket reaches a terminal state. If the device goes offline while suspended, the ticket is voided and the query returns 403 with details code 11002.

## Device tunnel (WSS /v1/devices/tunnel)

B-class devices connect via the Legacy Bridge. Upgrade request headers (HMAC signature, SEC-03):

```text
X-Device-ID: <device_id>
X-ADC-Timestamp: 1755117000          # Unix seconds
X-ADC-Nonce: <random hex, 8-32 bytes>
X-ADC-Signature: <hex(HMAC-SHA256(device_secret, signing string))>
signing string = device_id + "\n" + timestamp + "\n" + nonce
```

Verification: timestamp within a 300 s window; nonce cached for 10 minutes (replay returns 401 code 11012); bad signature returns 401 code 11011; revoked or frozen devices return 401 code 11004 or 403 code 11003. The credential is shown once at issuance and stored hashed.

Within 5 seconds of connection the gateway issues `tools/list`. Channel messages are JSON-RPC 2.0.

Gateway to device, `tools/list` request:

```json
{ "jsonrpc": "2.0", "id": "g-0001", "method": "tools/list" }
```

Device response:

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

`risk_suggestion` is the device's advisory value; the platform-configured `risk_level` is authoritative (SEC-09).

Gateway to device, `tools/call` request:

```json
{
  "jsonrpc": "2.0",
  "id": "g-0002",
  "method": "tools/call",
  "params": { "name": "set_spindle_speed", "arguments": { "rpm": 4200 } }
}
```

Device success response (`isError: false`); a business failure keeps the same shape with `isError: true`:

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

Protocol-level errors use the JSON-RPC error object, for example `{"code": -32004, "message": "tool execution failed"}`.

Heartbeat (device-initiated notification; the gateway acks):

```json
{ "jsonrpc": "2.0", "method": "heartbeat", "params": { "sdk_version": "1.2.0", "sent_at": "2026-08-14T08:29:55Z" } }
```

```json
{ "jsonrpc": "2.0", "method": "heartbeat_ack", "params": { "server_time": "2026-08-14T08:29:55Z" } }
```

Kick notification (credential revoked, frozen, or duplicate connection; the device should disconnect):

```json
{ "jsonrpc": "2.0", "method": "kick", "params": { "reason": "credential_revoked" } }
```

Channel constraints: 512 KB max message size (gateway disconnects and alerts on violation); offline after 60 s without pong; reconnect with exponential backoff starting at 1 s, capped at 60 s (SDK behavior); a duplicate connection replaces the old session and only the newest is kept.

## HITL callbacks

### POST /v1/hitl/callback

Approval decision callback forwarded by the WeCom/DingTalk callback server. One-time consumption. Headers: `X-ADC-Timestamp`, `X-ADC-Nonce`, `X-ADC-Signature` (signing string = `timestamp + "\n" + nonce + "\n" + raw request body`, HMAC with the per-ticket key), plus optional `X-ADC-Request-ID`.

```json
{
  "ticket_id": "hitl_8b7c6d5e",
  "decision": "approve",
  "approver": "emp_zhangwei",
  "comment": "Verified against the process sheet"
}
```

Response 200:

```json
{
  "ticket_id": "hitl_8b7c6d5e",
  "status": "approved"
}
```

Errors: 12004 (bad signature), 12001, 12002 (already handled; nonce and key are consumed once), 12003 (expired), 12005 (approver not on the ticket list), 10001 (decision must be `approve` or `reject`). The approver identity comes from WeCom/DingTalk OAuth, never hardcoded.

### GET /v1/hitl/action

Landing page behind the approval card buttons. Parameters: `ticket_id`, `decision`, `expire`, `sig`, where `sig = hex(HMAC-SHA256(callback_secret, ticket_id + "." + decision + "." + expire))` and `expire` is the ticket expiry in Unix seconds. A valid pending ticket redirects (302) to the console confirmation page, which performs the actual callback; a bad signature returns a 401 page; a handled ticket returns a 409 page.

## Ops endpoints

| Endpoint | Response | Notes |
| --- | --- | --- |
| GET /healthz | 200 `{"status":"ok"}` | Liveness; no dependency checks |
| GET /readyz | 200 with `dependencies` map (postgres, valkey, nats, notify_channels); 503 when any is down | Readiness, includes notification channel availability |
| GET /metrics | Prometheus text format | Internal only; `adc_` prefixed metrics: online devices, tools/call count and latency histograms, HITL interception and approval timeliness, rate limit counters |

## Error structure

All non-2xx JSON responses use one envelope:

```json
{
  "code": 11001,
  "message": "Device not found",
  "message_key": "errors.device.not_found",
  "trace_id": "4f8a2c1e9b3d4f5a8c6d7e8f9a0b1c2d",
  "details": [
    { "field": "device_id", "reason": "no row matches" }
  ]
}
```

The HTTP status code carries transport semantics; `code` carries business semantics; `message_key` is the stable i18n key (V1.0 returns Chinese text, tenant language from V1.5); `trace_id` correlates device access, tool calls, HITL tickets, and audit logs.

### Business error codes

| Range | Codes |
| --- | --- |
| 10xxx General | 10001 invalid params (400), 10002 unauthenticated (401), 10003 forbidden (403), 10004 not found (404), 10005 conflict (409), 10006 rate limited (429), 10007 internal error (500), 10008 body too large (413), 10009 idempotency key conflict (409), 10010 invalid JSON (400), 10011 unsupported media type (415) |
| 11xxx Device | 11001 device not found (404), 11002 device offline (503), 11003 device frozen (403), 11004 credential invalid or revoked (401), 11005 tool not found (404), 11006 tool disabled (403), 11007 tool call timeout (504), 11008 device code exists (409), 11009 import row errors (422), 11010 device quota exceeded (403), 11011 signature check failed (401), 11012 nonce replay (401) |
| 12xxx Approval | 12001 ticket not found (404), 12002 ticket already handled (409), 12003 ticket expired (409), 12004 callback signature failed (401), 12005 approver not authorized (403), 12006 blocked by HITL (403), 12007 approval flow not configured (503), 12008 notification push failed (503) |
| 13xxx Tenant | 13001 tenant not found (404), 13002 tenant disabled (403), 13003 tenant quota exceeded (403), 13004 tenant code duplicate (409), 13005 role not found (404), 13006 org member not found (404), 13007 cross-tenant access (403) |
| 14xxx Audit | 14001 invalid audit query (400), 14002 export limit exceeded (422), 14003 export task not found (404), 14004 audit logs are immutable (403) |

### JSON-RPC error codes (device tunnel)

| Code | Meaning |
| --- | --- |
| -32600 / -32601 / -32602 / -32603 | Invalid request / method not found / invalid params / internal error (JSON-RPC 2.0) |
| -32001 | Device not registered or frozen |
| -32002 | Tool not found |
| -32003 | Tool disabled (platform `is_enabled=false`) |
| -32004 | Tool execution failed (device returned `isError=true`) |
| -32005 | Call timeout (15 s default; long tasks move to the async model in phase 2) |

## Stability guarantees

V1.0 freezes the endpoint set and error code ranges. Within V1.5 everything is additive: the `name` aggregation format (`device_code__tool_name`), the HMAC signature format, and the error code segments are cross-version stable contracts that clients can safely depend on. See the V1.5 evolution matrix in `design/33` section 5 for the MCP Streamable HTTP, OAuth 2.1 bindings, and A2A plans.
