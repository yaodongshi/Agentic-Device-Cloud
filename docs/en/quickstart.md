# Quick Start

> Get a full ADC stack running locally with Docker Compose, then verify the complete chain: device tunnel, tool aggregation, HITL approval, execution, and audit. About 5 minutes.

## Prerequisites

| Requirement | Version | Notes |
| --- | --- | --- |
| Docker Engine | 24+ | With the Compose v2 plugin (`docker compose`) |
| Go toolchain | 1.24+ | Required only for the smoke test, which runs the mock device via `go run` |
| curl | any | For health checks |
| Free ports | 18080, 18082 | 18080 is the gateway entrypoint; 18082 is used by the dev smoke script internally |

The stack starts five containers: `gateway`, `adc` (Go data plane), `py-agent` (Python agent plane), `postgres` (16), and `valkey` (8). All data lives in dedicated Compose volumes (`adc_pg_data`, `adc_valkey_data`) and a dedicated network, so it does not interfere with other local stacks.

## Step 1: Clone and start the stack

```bash
git clone https://github.com/yaodongshi/Agentic-Device-Cloud.git
cd Agentic-Device-Cloud
docker compose -f deploy/compose.yaml up -d --build
```

The first run builds the Go and Python images, which takes a few minutes. Subsequent runs start in seconds.

Check that all five containers are healthy:

```bash
docker compose -f deploy/compose.yaml ps
```

## Step 2: Verify the gateway

The unified API gateway is the single entrypoint for all client traffic. Its health endpoint aggregates the stack status:

```bash
curl http://localhost:18080/healthz
```

Expected output:

```json
{"status":"ok"}
```

You can also open `http://localhost:18080/` in a browser.

## Step 3: Run the smoke test

The smoke script drives the full business chain through the gateway: health check, mock device tunnel over WSS, tool aggregation, a high-risk tool call that triggers HITL approval, the approval callback, execution, and audit log verification.

```bash
bash scripts/dev-smoke.sh
```

Expected output ends with a PASS/FAIL summary. A healthy stack passes every step (10/10).

```text
--- 1. 网关健康检查
  PASS: healthz
--- 2. mock 设备接入隧道
  PASS: mock 设备在线（经网关 WSS 透传）
...
  PASS: 审计日志可查
SUMMARY: PASS=10 FAIL=0
```

## What is running

| Service | Container | Role | Ports |
| --- | --- | --- | --- |
| gateway | `adc-gateway` | Unified API gateway, the only external entrypoint | 18080 |
| adc | `adc-app` | Go data plane: device connector, agent API, approval, audit | internal |
| py-agent | `adc-pyagent` | Python agent plane (FastAPI, evaluation toolchain in V1.0) | internal |
| postgres | `adc-postgres` | State and audit store (PostgreSQL 16) | internal |
| valkey | `adc-valkey` | Routing index, approval wakeup, audit buffer (Valkey 8) | internal |

Dev credentials are defaults for local development only (`adc` / `adc_dev_only`). Override via environment variables (`ADC_PG_PASSWORD`, `ADC_VALKEY_PASSWORD`, and so on) before production use.

## Stop and clean up

```bash
docker compose -f deploy/compose.yaml down            # stop, keep volumes
docker compose -f deploy/compose.yaml down -v         # stop and delete volumes
```

## Troubleshooting

| Symptom | Cause | Fix |
| --- | --- | --- |
| `bind: address already in use` | Port 18080 or 18082 is taken | Free the port, or change the port mapping in `deploy/compose.yaml` |
| Smoke test fails at "mock 设备接入隧道" | Mock device cannot reach the tunnel through the gateway | Wait for all containers to become healthy, then re-run the script |
| Smoke test fails at "无法获取设备密钥" | The `adc` container has not finished seeding | Wait for `adc-app` to be healthy (`docker compose ps`) |
| Image build fails | Docker cache or toolchain mismatch | Run `docker compose -f deploy/compose.yaml build --no-cache` |
| `go: command not found` during smoke | Go toolchain missing | Install Go 1.24+ (only needed for the mock device) |

## Next steps

- [Architecture](/en/architecture) - how the gateway, data plane, agent plane, and edge SDK fit together
- [API Reference](/en/api-reference) - full endpoint and error code reference
- [Device SDK](/en/device-sdk) - register your own device tools with the Rust SDK
- [Open Source](/en/open-source) - licenses, contribution flow, and community rules
