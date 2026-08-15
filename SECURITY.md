# Security Policy

This document describes how security vulnerabilities in the ADC (Agentic Device Cloud) project are reported, triaged, fixed and disclosed.

## Supported versions

Security fixes are provided for:

- the **latest minor release** (recommended for production);
- the **previous minor release** (security fixes only);
- **pre-release versions (0.x)**: best effort, no SLA.

Security fixes are shipped as patch releases following the monthly patch cadence. Critical fixes may be released out of band between scheduled releases.

| Version | Support |
| --- | --- |
| Latest minor (V1.x) | Yes |
| Previous minor | Yes, security fixes only |
| 0.x pre-release | Best effort |

## Scope

In scope for this policy:

- `core-sdk/` — edge SDK and wire protocol (Apache-2.0)
- `ce/` — community edition platform (LGPL-3.0)
- `deploy/`, `scripts/` — deployment artifacts and automation
- Public documentation and the docs site

Out of scope:

- **`ee/` enterprise modules**: EE source code is distributed only to subscribed customers under a commercial license. EE customers must report vulnerabilities through their commercial support channel (account manager), not through this policy.
- Vulnerabilities in third-party dependencies that are already publicly disclosed upstream and tracked in our SBOM and advisories.
- Issues that require an already-compromised administrator account, physical access to hardware, or a deployment that ignores the hardening baseline in the deployment guide.

## Reporting a vulnerability

**Do NOT open a public GitHub issue.**

Send a report to **security@zodioo.com**. Please include:

1. Affected component and version (for example `ce/` v1.0.0, gateway service);
2. A description of the vulnerability and the attack scenario;
3. Steps to reproduce or a proof of concept;
4. Potential impact (confidentiality, integrity, privilege escalation, denial of service);
5. Any suggested fix or mitigation;
6. Whether you would like public credit.

A PGP key for encrypted communication will be published on this page once available; contact us first via the address above if you need encrypted channel before then.

## Response and remediation SLA

Reports are acknowledged within **48 hours** on business days, then triaged and rated with CVSS v4.0:

| Severity | CVSS v4.0 | Acknowledgment | Remediation |
| --- | --- | --- | --- |
| Critical | 9.0–10.0 | 48 hours | Patch release within 7 days |
| High | 7.0–8.9 | 48 hours | Fix within 30 days (next patch release) |
| Medium | 4.0–6.9 | 48 hours | Fix within 90 days (next minor release) |
| Low | 0.1–3.9 | 48 hours | Maintainer discretion, next minor or major release |

## Disclosure process

1. Reporter sends the report to security@zodioo.com.
2. Maintainers acknowledge within 48 hours (business days).
3. Triage and fix coordination on a private branch; reporter receives status updates.
4. The fix ships in a patch release, together with a security advisory (GitHub Security Advisory) and a CHANGELOG entry.
5. Public credit is given to the reporter unless anonymity is requested.
6. Default disclosure policy: coordinated disclosure at patch release time, or a public advisory within 90 days of the report, whichever comes first.

## Deployment security baseline

Security fixes assume a deployment that follows the hardening baseline described in the deployment guide (design/60):

- TLS enabled on the gateway (`ADC_TLS_ENABLE`) with certificate injection via secrets;
- Valkey ACL accounts per service, TLS port only, no plaintext port;
- Only the nginx ingress exposed to the network;
- Backups (RPO 5 min / RTO 30 min) and quarterly recovery drills;
- Audit retention configured (180 days default).
