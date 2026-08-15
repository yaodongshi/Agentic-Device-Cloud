# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

### Changed

### Fixed

## [0.1.0] - TBD

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
