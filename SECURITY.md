# Security Policy

Aplexica is a tool that handles agent state — memories, conversation history, MCP server configurations including secret references — across a user's local machine. We take security seriously.

This document defines our **responsible-disclosure policy** for reporting security vulnerabilities and the **timelines and supported versions** we commit to.

## Reporting a vulnerability

**Do not file a public GitHub issue for a security vulnerability.**

Please report it privately, in one of these channels (in order of preference):

| Channel | How |
|---|---|
| GitHub Private Security Advisory | [Open one here](https://github.com/Aplexica/Aplexica/security/advisories/new) — preferred |
| Email | `security@aplexica.com` |

For non-security questions, see [README.md](README.md) for the public-issue tracker and discussions.

When you report, please include:

1. **A description of the vulnerability** — what is it, what does it do, what's the impact.
2. **Reproduction steps** — the exact sequence to trigger it, or a minimal proof-of-concept.
3. **Affected versions** — which `aplexica` / `aplexicatray` versions you tested.
4. **Your platform** — operating system, architecture, install method (Homebrew, apt, direct archive, source).
5. **Your assessment of severity** — using CVSS 3.1 if you can; informal description is fine otherwise.
6. **How you'd like to be credited** — full name, handle, "anonymous", or "no credit"; default is "no credit" until you confirm.

We will:

- **Acknowledge your report within 72 hours** (3 business days).
- **Assign a tracking ID** within 1 week.
- **Send a confirmed-or-rejected determination** within 2 weeks.
- **Keep you informed** at least every 14 days while we work on a fix.

## Supported versions

Only the **latest tagged release** receives security fixes unless a security
advisory explicitly names another supported version.

| Version range | Supported |
|---|---|
| [Latest tagged release](https://github.com/Aplexica/Aplexica/releases/latest) | ✅ Yes |
| Anything older | ❌ No — upgrade to the latest release |

## Disclosure timeline

We follow a **coordinated disclosure** model. Default timeline from valid report to public fix:

| Day | Milestone |
|---|---|
| 0 | Report received |
| ≤ 3 | Acknowledgement sent |
| ≤ 14 | Determination sent (confirmed / rejected / need more info) |
| ≤ 30 | Patch ready in private branch (severity-dependent) |
| ≤ 60 | Public release with fix |
| 90 | Public disclosure with CVE and reporter credit (if requested) |

For **critical severity** vulnerabilities affecting confidentiality of user secrets or canonical-store content, we may accelerate this timeline. For lower-severity issues we may extend it.

If the timeline expires without a fix or coordination from us, the reporter is welcome to disclose publicly. In practice we aim for the 60-day window for all valid reports.

## Out of scope

The following are **not** security vulnerabilities under this policy:

- **Theoretical issues without a proof-of-concept** — please demonstrate.
- **Social-engineering attacks against the project maintainers.**
- **Denial-of-service against an attacker's own machine** (local-only).
- **Missing security hardening that isn't a CVE-class issue** — please open a regular feature request.
- **Bugs in third-party agent products** (Claude Code, Codex, Hermes, OpenClaw, Kilo) — report those to the agent vendor; we will coordinate with them if an Aplexica adapter is involved.
- **Bugs in third-party plugins** — report to the plugin author.
- **Replay of an older, genuinely signed release.** The AWS KMS signature over `SHA256SUMS` carries no ordering, so an older release remains valid by design. Exact versioned filenames and provenance policy prevent one tag's evidence from satisfying another tag, while `aplexica update` keeps only a local highest-seen-version floor. That floor protects nothing on a fresh install. See [docs/install/verify.md](docs/install/verify.md#anti-rollback) and §7.3 of the trust model.

## In scope

We consider the following **in scope** for this policy:

- **Confidentiality** — anything that leaks secret values, conversation content, or canonical-store data to an unauthorized party.
- **Integrity** — anything that lets an unprivileged process mutate the canonical store, bypass the hash chain, forge events, or tamper with bundle signatures.
- **Authentication / authorization** — anything that bypasses file-mode permissions, the plugin sandbox, or the daemon's local-only control socket.
- **Supply chain** — anything that lets an attacker substitute a compromised binary in the official release artifacts, or that makes a substituted or forged artifact pass the documented verification commands. (Serving an unmodified *older* signed release is the one carved-out case — see "Out of scope" above.)
- **Sensitive logging** — anything that writes raw secret values to logs, the `doctor` report, or any user-shareable diagnostic.

The public threat model is documented in
[`docs/09-security-and-trust-model.md`](docs/09-security-and-trust-model.md).

## Safe harbor

Aplexica will not pursue civil or criminal action against, or law-enforcement investigation of, anyone who:

1. Acts in good faith to identify, report, and fix vulnerabilities.
2. Does not access, modify, or exfiltrate user data beyond what is necessary to demonstrate the vulnerability.
3. Does not perform attacks that could cause loss of service or data for other users.
4. Reports privately first per this policy, and does not publicly disclose before the coordinated window expires.

We will provide this safe harbor in any forum or jurisdiction that recognizes it.

## Hall of fame

We will credit reporters in the project's annual security report and on the [security/](https://github.com/Aplexica/Aplexica/security) advisories page, unless they request otherwise.

---

*This policy is adapted from the OpenSSF [Disclosure Policy template](https://github.com/ossf/oss-vulnerability-guide). Last updated 2026-08-06.*
