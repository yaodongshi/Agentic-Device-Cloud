# Device SDK

> The `core-sdk/` repository (Apache-2.0) holds the wire protocol v1 and the edge SDKs that turn machines into MCP tool nodes. This guide walks through the Rust SDK, the V1.0 reference implementation.

## What is in core-sdk

| Component | Language | Status | Purpose |
| --- | --- | --- | --- |
| wire protocol v1 | Go + Python | V1.0 | JSON-RPC 2.0 + MCP extension types, contract-driven from the same OpenAPI/JSON Schema |
| Rust SDK (`adc-edge-sdk`) | Rust | V1.0 | B-class device client: WSS reverse tunnel, HMAC auth, tool registry, reconnect with backoff |
| C demo | C | V1.0 | MCU-class demonstration of the same wire contract |
| Python SDK | Python | V1.5+ | Linux box-level wrapper of the official MCP Python SDK, reusing the Python protocol types |

The SDK is designed for OEM firmware embedding, so it is zero-copyleft (Apache-2.0): static linking into device firmware never triggers source disclosure obligations.

## Quick start (Rust)

Prerequisites: Rust toolchain (stable, edition 2021).

### 1. Add the dependency

The crate lives in the monorepo; pin it by path today and by version once the crate is published:

```toml
[dependencies]
adc-edge-sdk = { path = "../core-sdk/rust" }
serde_json = "1"
tokio = { version = "1", features = ["full"] }
```

### 2. Register your tools

A tool is a definition (`ToolDef`) plus a handler closure. Definitions are reported to the platform on `tools/list`; handlers receive the `arguments` map of `tools/call`.

```rust
use std::sync::Arc;

use adc_edge_sdk::client::{ClientConfig, DeviceClient, Event};
use adc_edge_sdk::tools::{ToolDef, ToolRegistry};
use adc_edge_sdk::wire::{ToolCallResult, ToolContent};

fn ok_text(text: impl Into<String>) -> ToolCallResult {
    ToolCallResult {
        content: vec![ToolContent::text(text)],
        is_error: false,
    }
}

fn log_event(event: &Event) {
    match event {
        Event::Connecting { attempt, url } => {
            println!("connecting (attempt {attempt}): {url}");
        }
        Event::Connected => println!("tunnel online"),
        Event::ToolsListServed { tool_count, .. } => {
            println!("tools synced: {tool_count} registered");
        }
        Event::ToolCallServed { tool, is_error, .. } => {
            println!("tool call served: {tool} is_error={is_error}");
        }
        Event::Kicked { reason } => {
            println!("kicked by server: {}", reason.as_deref().unwrap_or("unknown"));
        }
        Event::Disconnected { reason, retry_in, attempt } => {
            println!("disconnected ({reason}); retrying in {retry_in:?} (attempt {attempt})");
        }
        Event::Notification { method } => println!("notification: {method}"),
        Event::HeartbeatSent => {}
    }
}

#[tokio::main]
async fn main() {
    let tunnel_url = std::env::var("ADC_TUNNEL_URL")
        .unwrap_or_else(|_| "ws://127.0.0.1:18080/v1/devices/tunnel".to_string());
    let device_id = std::env::var("ADC_DEVICE_CODE")
        .unwrap_or_else(|_| "cnc-demo-01".to_string());
    let secret = std::env::var("ADC_DEVICE_SECRET")
        .expect("ADC_DEVICE_SECRET is required");

    let registry = ToolRegistry::new();

    // Read-only observation: risk 0, executes without approval.
    registry
        .register(
            ToolDef::new("get_spindle_status", "Read CNC spindle RPM and temperature")
                .with_risk(0)
                .with_schema_version("1"),
            |_args| {
                // Read from your machine here.
                ok_text(r#"{"status":"RUNNING","rpm":1200,"temperature_celsius":38.2}"#)
            },
        )
        .expect("register get_spindle_status");

    // Write operation: risk 2, the platform blocks it until a human approves.
    registry
        .register(
            ToolDef::new("set_spindle_speed", "Set CNC spindle RPM")
                .with_risk(2)
                .with_schema(serde_json::json!({
                    "type": "object",
                    "properties": { "rpm": { "type": "number" } },
                    "required": ["rpm"]
                }))
                .with_schema_version("1"),
            |args| {
                let rpm = args.get("rpm").and_then(|v| v.as_f64()).unwrap_or(0.0);
                // Drive your machine here.
                ok_text(format!("spindle speed set to {rpm} RPM"))
            },
        )
        .expect("register set_spindle_speed");

    let config = ClientConfig::new(tunnel_url, device_id, secret);
    let client = DeviceClient::new(config, Arc::new(registry)).with_event_handler(log_event);

    tokio::select! {
        result = client.run() => eprintln!("client exited: {result:?}"),
        _ = tokio::signal::ctrl_c() => println!("received Ctrl-C, shutting down"),
    }
}
```

### 3. Register the device and run

First register the device in the platform to get its secret:

```bash
curl -X POST http://localhost:18080/v1/admin/devices \
  -H "Content-Type: application/json" \
  -d '{"device_code":"cnc-demo-01","name":"CNC demo","device_type":"cnc","auth_type":"hmac"}'
```

Then run your binary with the credential in the environment (never log it):

```bash
export ADC_DEVICE_CODE=cnc-demo-01
export ADC_DEVICE_SECRET=<secret from the response>
export ADC_TUNNEL_URL=ws://127.0.0.1:18080/v1/devices/tunnel
cargo run --release
```

Expected output:

```text
connecting (attempt 0): ws://127.0.0.1:18080/v1/devices/tunnel
tunnel online
tools synced: 2 registered
```

The platform issues `tools/list` within 5 seconds of connection; your tools become visible to agents through the aggregated MCP endpoint.

### 4. Reference device

The SDK ships a ready-made example at `core-sdk/rust/src/bin/echo_device.rs` (binary `echo_device`) that registers `get_status` (risk 0) and `set_speed` (risk 2) and logs every lifecycle event. Run it with the same three environment variables.

## Configuration

`ClientConfig` fields:

| Field | Default | Meaning |
| --- | --- | --- |
| `tunnel_url` | required | Tunnel endpoint, `ws://` for dev, `wss://` for production (TLS mandatory) |
| `device_id` | required | Device code from the platform device registry |
| `secret` | required | Device secret, plain in memory, injected via environment, never logged |
| `heartbeat_interval` | 30 s | Application-level heartbeat period; the platform renews the routing index every 30 s |
| `connect_timeout` | 10 s | Per-attempt connect timeout |
| `jitter` | 0.2 | Reconnect jitter amplitude (plus/minus 20%) |

## Connection lifecycle

1. Dial the tunnel URL with fresh HMAC headers on every attempt.
2. Answer the platform `tools/list` (issued within 5 s of connect).
3. Dispatch `tools/call` requests to registered handlers; a panicking handler is caught and reported as `isError`, so a misbehaving tool never kills the connection.
4. Send `heartbeat` every `heartbeat_interval`; the platform acks with `heartbeat_ack`. WebSocket pings are answered automatically.
5. On disconnect, reconnect with exponential backoff over the ladder 1/2/4/8/16/30/60 seconds plus jitter; a `retry_after_secs` hint in the close frame overrides the ladder for that attempt.
6. On `kick` (credential revoked, frozen, duplicate connection), disconnect and wait for the operator; do not reconnect in a tight loop.

## HMAC authentication (SEC-03)

Every handshake carries four headers; the signature string is:

```text
signing string = device_id + "\n" + timestamp + "\n" + nonce
signature = hex(HMAC-SHA256(device_secret, signing string))
```

| Header | Content |
| --- | --- |
| `X-Device-ID` | Device code |
| `X-ADC-Timestamp` | Unix seconds, within 300 s of server time |
| `X-ADC-Nonce` | Random hex, 8-32 bytes, unique per attempt |
| `X-ADC-Signature` | Hex HMAC as above |

The SDK generates a fresh nonce and timestamp per attempt (`auth::generate_nonce`, `auth::hmac_sign`). Credentials support dual-slot rotation: a rotated credential keeps working for a 24 h transition window.

## Risk levels

`with_risk(level)` on a `ToolDef` sets the device-suggested risk (0-3). The value is advisory only: the platform stores the authoritative `risk_level` per tool (default 2, approve before use) and enforces HITL on calls at level 2 or higher. Tools without a registered risk level inherit the tenant default.

## Behavior notes

- **Offline queue**: keep a local persistent FIFO cache of outbound events while disconnected. The SDK does not auto-replay after reconnect in V1.0; replay policy opens up in V1.5.
- **Duplicate connections**: the platform keeps only the newest session per device; the old one receives `kick`.
- **Message limit**: single messages are capped at 512 KB; the gateway disconnects on violation.
- **Tests**: the SDK is covered by unit tests under `core-sdk/rust/tests/` (wire, auth, backoff, tools); run `cargo test` in `core-sdk/rust`.

## What changes in V1.5

A-class devices with a native MCP server runtime will onboard through OAuth 2.1 client credentials (`/v1/bindings`) instead of the WSS tunnel, which is downgraded to the Legacy Bridge for B-class devices. Wire protocol v1 stays backward compatible.
