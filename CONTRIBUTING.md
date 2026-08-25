# Contributing to ADC

Welcome, and thank you for your interest in contributing to Agentic Device Cloud (ADC).

ADC is an AI-native device orchestration and governance platform: every device becomes a standard MCP tool node that AI agents can discover, call and orchestrate under industrial-grade governance. The project follows an Odoo-style dual-edition open source model:

| Directory | License |
| --- | --- |
| `core-sdk/` | Apache-2.0 (edge SDK and wire protocol) |
| `ce/` | GNU LGPL-3.0 (community edition platform) |
| `ee/` | Source-visible commercial license (enterprise modules, subscribers only) |

The community language is **English first**: use English for issues, pull requests, code comments, commit messages and documentation source (`docs/en/`). Chinese and other languages are welcome as translations.

## Code of Conduct

All participants must follow the [Code of Conduct](CODE_OF_CONDUCT.md). Be kind, be constructive, stay on topic.

## Before you contribute: edition boundaries

Governance, compliance and scale capabilities (multi-tenant RBAC/SSO, audit center, multi-level approval, cluster HA, LLM metering, private-delivery tooling) belong to the **enterprise edition (`ee/`)**, not the community edition:

- PRs that implement EE-only features **cannot be merged into `ce/` or `core-sdk/`**. Maintainers will thank you and suggest a community-compatible alternative: a seam interface, a hook, tests, or protocol improvements.
- The `ee/` directory does **not** accept external PRs (its source is only visible to subscribers).
- Seam interface design contributions are welcome and credited; significant contributions may qualify for the enterprise contributor reward program.

## Communication

- **Bug reports and feature requests**: GitHub issues, using the provided templates.
- **Questions and discussions**: GitHub Discussions (opened as part of the open-source launch).
- **Security vulnerabilities**: never in public. See [SECURITY.md](SECURITY.md).

## Developer Certificate of Origin (DCO)

Every commit must include a sign-off trailer:

```
Signed-off-by: Your Name <your.email@example.com>
```

Add it automatically with `git commit -s`.

By signing off you certify under the [Developer Certificate of Origin v1.1](https://developercertificate.org) that you have the right to submit the contribution, and that you license it under the license of the target repository: **Apache-2.0** for `core-sdk/`, **LGPL-3.0** for `ce/`. This is the inbound license grant; no personal CLA is required.

- CI blocks any PR whose commits lack the sign-off (DCO bot).
- If you contribute on behalf of an employer, make sure the employer agrees with the DCO. Bulk corporate contributions additionally require a company contribution statement confirming the employer waives competing claims.

## Pull request workflow

1. Search existing issues and discussions first.
2. Open an issue using the template and discuss the approach with maintainers.
3. Fork the repository and create a branch from `main` (for example `fix/device-reconnect-race`).
4. Implement the change with unit tests; run the full test suite locally.
5. Commit with Conventional Commits messages and DCO sign-off.
6. Push and open a pull request.
7. CI gates must all be green: DCO check, lint, vet, unit tests with coverage threshold, license scan, SBOM generation, secret scan.
8. Maintainers review; address feedback; the PR is squashed and merged by a maintainer.

Keep pull requests small and focused on one change. Large changes without prior discussion will likely be asked to be split.

## Code style

- **Go** (`ce/`, `core-sdk/`): `gofmt`, `go vet`, tests run with `go test -race ./...`.
- **Python** (`ce/py-agent/`, `core-sdk/python/`): `ruff check` and `ruff format`.
- **Frontend** (`ce/console/`): ESLint and Prettier per the workspace config.
- **Commit messages**: Conventional Commits (`feat:`, `fix:`, `docs:`, `refactor:`, `chore:`, `test:`), written in English.
- **Comments**: English, explaining why rather than what.
- **Dependencies**: no new dependency without a license review; the CI license whitelist is per-repository (`core-sdk/` strictly Apache-2.0/MIT/BSD/ISC, no copyleft; `ce/` additionally allows LGPL-3.0).

## Testing

- New code requires unit tests; the CI coverage threshold applies to `ce/` core paths.
- Behavioral changes must update documentation: `docs/en/` is the source language, translations follow.
- End-to-end verification: `docker compose -f deploy/compose.yaml up -d` then `bash scripts/dev-smoke.sh`.

## Translation workflow

English (`ce/console/src/i18n/locales/en.ts` and `docs/en/`) is the source language. Chinese (`ce/console/src/i18n/locales/zh-CN.ts` and `docs/zh/`) follows in the same pull request for user-facing release content. Language-resource changes do not require business-code changes.

1. Add or revise the English source key. Use semantic keys and named placeholders such as `{name}`; do not build sentences by concatenating translated fragments.
2. Update the Chinese resource with the same key, value type, and placeholders. Follow the frozen terms in [`docs/i18n-glossary.md`](docs/i18n-glossary.md).
3. Run `cd ce/console && npm run i18n:test`, then run `npm run build`.
4. Request review from both a domain owner and a Chinese reviewer. Machine translation may be used only as a draft; user-facing text requires human review.
5. If context requires a non-standard high-risk term, add the exact key to `scripts/i18n-term-allowlist.json` and explain the exception in the pull request. Wildcards and blanket exclusions are not accepted.

CI blocks missing keys, type drift, placeholder drift, empty values, and unapproved high-risk terminology. Translation-only pull requests may be merged independently when these gates and the Console build pass.

## Response SLA

Maintainers commit to:

- **Issues**: first response within **24 hours** on business days, following a three-window timezone rotation (UTC+8 / UTC+1 / UTC-8).
- **Pull requests**: review and merge — or explicit rejection with reasons — within **5 business days**.

The SLA covers first response and triage, not resolution time. Under exceptional load the team will announce an honest downgrade rather than silently miss the commitment.

## Release cadence

- Monthly patch releases (security and bug fixes), quarterly minor releases (community features).
- Semantic versioning; every release ships a CHANGELOG and an SBOM.
- The public roadmap covers the next two quarters of community-edition work.

## Committer promotion

Contributors can become committers: at least 8 merged PRs in 12 months, nomination by 2 existing committers, and a successful vote. Committers inactive for 12 consecutive months become emeritus.

## Questions

Open a GitHub Discussion, or contact the maintainers at security@zodioo.com (administrative topics only; see SECURITY.md for vulnerabilities).
