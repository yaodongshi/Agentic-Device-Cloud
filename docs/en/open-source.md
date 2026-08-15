# Open Source

> ADC follows the Odoo-style dual edition open source model: layered licenses per directory, a community edition anyone can self-host, and an enterprise edition whose source paying customers can read, audit, and modify. This page covers licenses, contribution rules, and community commitments.

## Licenses

| Directory | License | Rationale |
| --- | --- | --- |
| `core-sdk/` | Apache-2.0 | SDK and wire protocol get embedded in OEM firmware; zero copyleft, with patent grant |
| `ce/` | GNU LGPL-3.0 | Platform core: modifications must stay LGPL-3.0 (no closed forks), while linking and self-use stay free |
| `ee/` | Source-visible commercial license | Enterprise addons: source delivered with the subscription, modifiable for own use, no redistribution |

Docs and examples: Apache-2.0 for code examples, CC-BY-4.0 for tutorials.

Rules of thumb:

- Anything distributed inside device firmware must be permissively licensed (Apache-2.0, MIT, BSD).
- Anything that protects the platform from closed forks goes to `ce/` under LGPL-3.0.
- Governance, compliance, and scale capabilities live in `ee/`; scarcity comes from the license, not from hiding code.
- "Enterprise edition source is visible to subscribers" is the only accepted wording; never call EE "open source" or "closed source".

Dependency discipline: `core-sdk/` rejects any GPL-family dependency (full-tree scan, not just direct). `ce/` accepts Apache-2.0/MIT/BSD and LGPL, but never GPL-2-only, GPL-3, or AGPL. `ce/` uses Valkey instead of Redis for licensing reasons (RSALv2/SSPLv1 are not OSI-approved); the word Redis does not appear in deliverables.

## Contributing

The flow: fork the repository, sign the DCO, open a pull request, pass CI gates, get a maintainer review.

### 1. Sign-off (DCO)

Every commit must carry a Developer Certificate of Origin sign-off. It states that you own the code you are contributing and license it to the project under the target repository's license (`core-sdk/`: Apache-2.0; `ce/`: LGPL-3.0).

```text
Signed-off-by: Full Name <email@example.com>
```

Create signed commits with:

```bash
git commit -s
```

No individual CLA is required. Bulk corporate contributions need a company agreement confirming the employer waives adversarial claims.

### 2. Pull request requirements

- English commit messages, issues, and PR descriptions (English-first policy).
- Each PR addresses one concern, with a description of the change and why.
- CI gates must pass: license scan (directory whitelists), SBOM generation on release, vulnerability scan (govulncheck), secret scan (gitleaks), and test coverage (core paths in `ce/` at or above 60%).
- New features ship with documentation in the source language (`docs/en/`); translations follow within one minor release.

### 3. Edition boundary rules

ADC separates community and enterprise features by capability, not by size:

- Connection, protocol, and base capabilities belong to CE. Enterprise-only capabilities (cluster scheduling enhancements, audit reporting, metering, multi-level approval) must not be merged into `ce/`.
- If your PR implements an EE-bound feature, a maintainer will thank you and suggest reframing it as a seam interface, a hook, tests, or a protocol improvement.
- `ee/` accepts no external PRs; its source is visible to subscribers only.
- Seam interface contributions are welcome and credited; significant seam designs can earn a one-year enterprise license.

### 4. Review and promotion

- Committer promotion: 8 or more merged PRs within 12 months, nomination by 2 existing committers, approval by vote. Inactive for 12 months means emeritus status.
- Release cadence: monthly patch releases (security and fixes), quarterly minor releases (community features), semantic versioning, changelog and SBOM attached to every release, one-week feature freeze before release.

## Community commitments

| Item | Commitment |
| --- | --- |
| Issue first response | 24 hours (working days, rotating three timezone windows) |
| PR first review | 5 working days |
| PR merge or explicit rejection | within 10 working days |
| Security reports | via SECURITY.md private channel; acknowledged within 72 hours, fixed within 30 days, critical issues patched within 7 days |
| Roadmap | public; the next two quarters of community roadmap are visible |

## Security reporting

Do not open public issues for vulnerabilities. Use the private channel described in `SECURITY.md`:

- Acknowledgment within 72 hours
- Fix within 30 days for regular issues
- Patch release within 7 days for critical issues

## Trademark

No license grants trademark rights. Forks may not use the ADC name, logo, domain, or "official" wording. Factual compatibility statements ("based on ADC community edition") are allowed; implying official endorsement is not. Commercial integrators need a channel agreement.

## Internationalization

English is the primary language for README, docs, issues, PRs, changelogs, and release notes. Chinese mirrors are provided for key content. Maintainers answer Chinese questions in English, optionally with a Chinese explanation.

## Repository layout

```text
core-sdk/   Apache-2.0  edge SDK and wire protocol (Go + Python + Rust)
ce/         LGPL-3.0    community edition
ee/         commercial  enterprise addons (source-visible, subscription)
docs/       documentation site (en/ source, zh/ translations)
```

See [Roadmap](/en/roadmap) for what ships next.
