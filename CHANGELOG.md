# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.2.0] - 2026-08-15

### Added
- Unified entry: console (nginx SPA) is the single public URL; login works out of the box
- Admin API wired into the server (tenants/devices/api-keys/risk policies/audit/approval-tickets)
- Seed dataset: demo tenant, 2 devices, 3 tools, demo API key, demo audit events, demo PENDING ticket
- Tenant-scoped demo accounts (tenant-admin/approver)
- Dashboard landing page, onboarding guide, actionable empty states
- Contract alignment: call envelopes, callback error-code ladder, cross-tenant 403
- Gateway rate limiter live (Valkey token buckets); /metrics via unified entry
- CI coverage gate enabled (>=70%, measured 75.3%); docs Pages workflow; install handbook

### Fixed
- Valkey ACL username wiring (audit/metering/wake bus WRONGPASS)
- Nullable column scan crashes (adminapi devices, approval tickets)
- Concurrent rate-limit over-admission; JSONB null marshaling; FROZEN status case mismatch

### Added

### Changed

### Fixed

## [0.2.0] - 2026-08-15

### Added
- Console dashboard landing page (KPI cards, recent audit, first-login guide)
- Demo dataset (2 devices, 3 tools, API key, audit samples, pending ticket) + tenant-scoped demo accounts
- GET /v1/admin/approval-tickets endpoint; frontend auto tenant scoping
- /metrics through the unified entry; gateway Valkey token-bucket rate limiter live
- Ops monitor page (Grafana embed, platform_admin), GitHub Pages docs workflow, OpenSSF Scorecard workflow
- Pilot operations docs (deployment guide, weekly report template, onboarding SOP, notification checklist)
- Full-detail breakdown plan (design/82)

### Fixed
- Contract drift: low-risk call envelope, callback error-code ladder, cross-tenant 403
- ratelimit float64 clock precision bug (500ms boundary false rejection)
- Valkey ACL username wiring (audit/metering/wake bus WRONGPASS)
- FROZEN status case mismatch, JSONB null marshaling

### Changed
- Coverage gate enabled in CI (75.3% >= 70%); 10-round -race stability verified

## [0.1.0] - 2026-08-15

### Added
- Phase 0 security hardening complete (SEC-01~14, 20/20 openspec tasks)
- Unified API gateway (Plan A: Go data plane + Python agent plane, frontend-agnostic)
- Device connector (WSS tunnel, HMAC+nonce auth, kick-old, TTL renewal)
- Agent API (tool aggregation, DB risk policy, 202-pending HITL flow, BLOCKED_BY_HITL)
- Approval service (PG CAS state machine, signed callbacks, WeCom/DingTalk notifiers)
- Audit pipeline (at-least-once, partitioned, 180d retention) + metering + rate limiting
- Admin API (tenants/devices/api-keys/risk policies/audit query) + session auth + RBAC
- Management console (Vue 3: devices, tools & risk, approvals, audit, api-keys, tenants, monitor)
- Python agent plane (FastAPI eval harness, v1.0 minimal set)
- Rust edge SDK (WSS client, backoff reconnect, tool registry, echo + modbus examples)
- Docker compose stack (gateway/adc/py-agent/postgres/valkey, multi-arch images)
- CI/CD (go/python/console/docker workflows), observability (prometheus/grafana), backup scripts
- Bilingual docs site (VitePress), community files, DJ gap analysis

Initial ADC platform MVP release (release date to be filled in by the release
workflow owner when the v0.1.0 tag is cut).

### Added

- Go data plane under `ce/` (LGPL-3.0): device connector with WSS long connections,
  Agent API, approval (HITL) state machine and audit pipeline.
- Wire protocol SDK under `core-sdk/` (Apache-2.0) with Go and Python implementations.
- Unified API gateway (Go) as the single front-door entry point for all API traffic.
- Python agent plane minimal set under `ce/py-agent` (V1.0 scope: eval harness
  toolchain, CLI-only, managed with uv and a frozen `uv.lock`).
- Vue 3 admin console under `ce/console`: tenant/device management, approval cards
  and audit views.
- Docker Compose deployment (single host, dev profile) with PostgreSQL 16 and
  Valkey 8 (ACL-enabled), TLS termination at the edge.
- CI quality gates (GitHub Actions): Go gofmt/vet/test(-race)/coverage, Python
  ruff/pytest/uv lock, frontend lint/typecheck/unit/E2E, dual-arch image builds
  (linux/amd64 + linux/arm64) with SBOM generation and CRITICAL vulnerability
  blocking.

### Security

- Phase-0 hardening implemented: per-device authentication and HMAC tokens,
  rate limiting, HITL gating, audit immutability and TLS/ACL baselines
  (SEC-01 to SEC-14 verification covered by the test plan).

[Unreleased]: https://github.com/yaodongshi/Agentic-Device-Cloud/compare/v0.1.0...dev
[0.1.0]: https://github.com/yaodongshi/Agentic-Device-Cloud/releases/tag/v0.1.0
