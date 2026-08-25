# BRD-09 — Security and Trust Model (OSS Edition)

**Document type:** Business Requirements Document
**Status:** Draft v1
**Last updated:** 2026-08-06
**Edition:** OSS (Aplexica open-source)
**Maintainer:** Aplexica project

---

> This BRD records security requirements and design intent. The implementation
> and tests are authoritative for currently shipped cryptographic behavior.

## 1. Problem

Aplexica handles content developers consider personal and often sensitive: project secrets, internal architecture decisions, private memories, conversation history that touches anything from API keys to performance reviews. The trust posture has to be unambiguous — both technically (what the code actually does) and contractually (what users are promised).

This BRD specifies the threat model, cryptographic design, supply-chain posture, and OSS trust boundary for the open-source edition. The local core requires no account and sends no telemetry. Network activity is limited to explicit user actions and remote plugins the user deliberately enables.

## 2. Trust posture

**Aplexica OSS.** In the default local configuration, artifact content stays on the user's machine and the daemon does not initiate outbound connections. An explicit update check or configured remote plugin expands that boundary; its network and data access must be visible to the user and separately authorized.

## 3. Threat model

### 3.1 In-scope adversaries

| Adversary | OSS protection |
|---|---|
| **Local malware running as another user** | Private directories under `~/.aplexica/` use mode `0700`, sensitive files use mode `0600`, and sockets are restricted |
| **An attacker with physical access to the user's machine** | Locks on user account; encrypted disk recommended |
| **Supply-chain attack on Aplexica binaries** | AWS KMS-backed cosign signatures authorize `SHA256SUMS` and strict public provenance; trimmed CI builds come from a tagged commit. Substituting *forged* bytes is detected; replaying an *older, genuinely signed* release is **not** — the signature carries no ordering (see §7.3 and §11) |

### 3.2 Explicitly out-of-scope adversaries

- **A nation-state with the user's device.** If the user's machine itself is fully compromised by a sophisticated adversary, Aplexica cannot protect the data on it; the agents themselves are already compromised in that scenario.
- **A compromised LLM provider.** The model the agent talks to is outside Aplexica's trust boundary. Users who require model-level confidentiality should evaluate their model provider separately.

### 3.3 Assumed user behaviors

- Users keep their devices reasonably patched.
- Users do not share their account credentials.
- Users store sensitive credentials offline (paper, password manager, hardware token) where applicable.

## 4. Cryptographic design

### 4.1 Primitives

- **Bundle encryption:** the interoperable `age` format through `filippo.io/age`, using an X25519 recipient or an scrypt passphrase.
- **Remote-envelope encryption:** versions 2 and 3 use XChaCha20-Poly1305 for artifact bodies. The legacy version-1 decoder uses AES-256-GCM for backward compatibility.
- **Key wrapping:** versions 2 and 3 use X25519 + HKDF-SHA256 with XChaCha20-Poly1305 authenticated wrapping; the legacy version-1 format uses AES-256-GCM.
- **Digital signatures:** Ed25519 for product protocols; ECDSA P-256/SHA-256
  in AWS KMS for public release evidence.
- **Hashing:** SHA-256 for artifact identities, bundle hashes, key identifiers, and audit records.
- **Random number generation:** Go's `crypto/rand`, backed by the operating system.

The implementation in `internal/acf`, `internal/keys`, and `internal/keyrotation` is authoritative. Wire formats are versioned and bind contextual metadata as authenticated data where required.

### 4.2 Tool secrets

Tool artifacts (MCP server configs, hooks, plugins, etc. — see [02-brd-format-adapters.md](02-brd-format-adapters.md) §4.4) often reference sensitive values: API keys, OAuth tokens, credential file paths. Aplexica's secret-handling policy is strict:

- **Secret values live in `~/.aplexica/secrets/`**, separate from the canonical store. The directory uses mode `0700` and sensitive files use mode `0600` on Unix; Windows access is restricted to the owning user.
- **Tool artifacts never embed raw secret values.** They reference secrets via named placeholders (`${secret:<name>}`). Adapters that encounter inline secrets in native config files MUST externalize them at ingestion (per FR-02.15 in [02-brd-format-adapters.md](02-brd-format-adapters.md)).
- **Auditing.** Every secret-value rotation and every secret read/write produces a local audit log entry in `~/.aplexica/logs/secrets-audit.jsonl`.

## 5. Authentication

In the default local configuration, authentication is OS-level: the daemon runs under the owning user account and the control socket is restricted to that user (FR-09.14). A configured remote plugin may add its own account and device-pairing flow, expanding the trust boundary explicitly; it does not change who may access the daemon's local control surface.

## 6. OSS trust boundary

The OSS daemon is the trusted ground truth on each device. External plugins (sync, transport, backup) interact via the daemon's documented plugin API. Plugins are extensions; the daemon never depends on any plugin.

If a user uninstalls a plugin, the OSS daemon continues to work normally; the canonical store is unaffected.

## 7. Supply chain

### 7.1 Release requirements

- Release candidates are produced by an ephemeral CI job from an annotated release tag. Action revisions and the Go and GoReleaser versions are pinned, while the hosted runner image and macOS SDK are not. Builds use `-trimpath`, `-buildid=`, and a commit-derived module timestamp to remove known variable inputs. Bit-for-bit reproduction from a second independent environment is not claimed: the Darwin artifacts need cgo (CoreServices and Cocoa), every target is built once, and there is no rebuild-and-compare step.
- Published artifacts must be covered by `SHA256SUMS` and its AWS KMS-backed cosign bundle `SHA256SUMS.sigstore.json`; verifying the reviewed public key and each listed digest authenticates the ten build artifacts.
- Every release must also publish `aplexica.provenance.sigstore.json`, a KMS-authorized in-toto SLSA v1 statement with the same exact ten subjects. A checked-in verifier enforces the public builder, tag, source, dependency, and no-unknown-field policy in addition to cosign's cryptographic checks. No independent builder or SLSA level is claimed.
- A CycloneDX SBOM (`aplexica.sbom.cdx.json`) must be published alongside every release and listed in `SHA256SUMS`.
- The public installation guide must accurately mark a channel unavailable until its verification path is usable end to end.

### 7.2 Dependencies

- Dependency updates are reviewed rather than auto-merged.
- Cryptographic dependencies are limited to the Go standard library, `golang.org/x/crypto`, and `filippo.io/age` unless a reviewed design changes that set.
- `govulncheck` runs for pull requests, changes to `main`, and the scheduled security workflow; reachable vulnerabilities block release.
- The advisory updater deliberately does not embed a second signature verifier. It reads one release-metadata response, reports availability, and installs nothing; users authenticate downloaded bytes with the documented public-key procedure. This keeps Sigstore/TUF dependency trees out of the daemon.

### 7.3 Distribution

> **Release authority (revised 2026-08-09).** Release authority is a
> non-exportable asymmetric AWS KMS key. The reviewed public trust anchor is
> `aplexica-release.pub`. GitHub Actions may exchange its signed OIDC token for
> a short-lived AWS session, but GitHub/Fulcio does not sign a release artifact
> or provenance statement. The signing job cannot publish, and the publication
> job cannot obtain AWS signing authority.
>
> Every channel remains a byte transport only. No registry, mirror, download
> host, package manager, GitHub account, or maintainer workstation can produce
> a valid release signature without the repository-bound AWS role and KMS key.
>
> **Anti-rollback limit.** A KMS signature over `SHA256SUMS` carries no
> ordering: each tag is signed independently, and an older, genuinely signed
> release verifies cleanly forever. What downgrade protection remains is weak
> and local, never cryptographic: exact versioned filenames and the provenance
> tag policy prevent one tag's evidence from satisfying another tag;
> `aplexica update` keeps a local floor at the highest version that machine has
> already seen, which protects nothing on a fresh install; and `brew upgrade`
> will not move an installation backwards, the only package-manager protection
> that exists today, since WinGet is unpublished and the `.deb` is installed as
> a local file with no repository behind it. This is a real reduction in
> the security posture and is recorded as out of scope in §11; see
> [docs/install/verify.md](install/verify.md#anti-rollback) for the user-facing
> statement of the same limit.

## 8. Privacy

### 8.1 What is collected

**Default local operation:** no telemetry or artifact content leaves the device. Explicit update checks and user-configured remote plugins are outside that default and must disclose their network behavior.

## 9. Functional requirements

- **FR-09.1** OSS daemon MUST make zero outbound network connections in default configuration.
- **FR-09.2** A plugin that requests network access MUST declare that capability and MUST require explicit user enablement before the daemon starts it.
- **FR-09.12** OSS daemon MUST refuse to run as root (Unix) or with elevated privileges (Windows).
- **FR-09.13** Private directories under `~/.aplexica/` MUST use mode `0700` and sensitive files MUST use mode `0600` on Unix; on Windows, access MUST be restricted to the owning user.
- **FR-09.14** Sockets used for the daemon control channel MUST be restricted to the owning user.
- **FR-09.15** Logs MUST NOT contain decrypted artifact content. Pattern-based detectors MUST scan log output as a guard.
- **FR-09.16** A documented Responsible Disclosure policy MUST be published before public launch with a contact address, expected response time, and a safe-harbor commitment.
- **FR-09.17** A public security.txt file MUST be served from `aplexica.com/.well-known/security.txt`.

## 10. Non-functional requirements

| ID | Requirement |
|---|---|
| **NFR-09.6** | Code paths handling cryptography MUST achieve 100% test coverage of branches in CI. |

## 11. Out of scope

- **Anti-rollback in the release verification model.** A valid KMS signature has no monotonic ordering and remains valid after newer releases exist. Exact filenames and provenance prevent cross-tag substitution, while the updater's highest-seen floor is local and protects nothing on a fresh install. See §7.3 and [docs/install/verify.md](install/verify.md#anti-rollback).
- Post-quantum cryptography. V1 uses standard primitives. The protocol is versioned so that a future migration to PQ primitives is feasible.
- Hardware-key custody (e.g., requiring a YubiKey for daily operations). May be a future feature.
- "Forget specific events" cryptographic erasure (cryptoshredding individual events). The architecture allows revoking access to a namespace via key rotation, but not erasing a single event from devices that have already decrypted it.
- A formal common-criteria or FedRAMP certification. Possibly future.

## 12. Acceptance criteria

V1 of the OSS security and trust model is complete when:

1. The OSS daemon, running with default configuration and no external plugin installed, produces zero outbound packets in a 24-hour network trace.
2. Every published release carries KMS-authorized checksum and provenance bundles that verify with `aplexica-release.pub`; exact artifact digests and the public provenance policy pass for every macOS, Linux, and Windows artifact. The train builds each target once on a hosted runner and makes no independent rebuild-and-compare claim.
3. Every channel marked available in the installation guide resolves to the intended release and its integrity metadata.
4. The release SBOM is published and parseable by standard tooling.
5. `security.txt` is served and includes a working contact channel.

## 13. Resolved decisions

- **OQ-09.1 — Static encryption format for `.aplexica` bundles.** **Decision: use AGE** (`age-encryption.org/v1`). Chosen for interoperability (off-the-shelf `age` tooling can decrypt user bundles without Aplexica installed), small auditable spec, and modern primitives that match the rest of §4.1. Aplexica-specific metadata rides in a sidecar manifest within the bundle rather than extending the AGE format.
- **OQ-09.3 — Trust in third-party package channels.** **Decision: a package channel is transport, not release authority.** A channel is marked available only when its artifacts are tied to a specific authenticated release and the documented verification path works end to end.

## 14. Dependencies

- Non-functional posture (uptime, recovery) — see [10-non-functional-requirements.md](10-non-functional-requirements.md).
