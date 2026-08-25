# BRD-01 — Local Backup & Restore

**Document type:** Business Requirements Document
**Status:** Draft v1
**Last updated:** 2026-05-17
**Edition:** OSS (no subscription required)
**Maintainer:** Aplexica project

---

> This BRD records requirements and design intent. For currently shipped
> command syntax, use the [user guide](user-guide.md) and `aplexica help`.

## 1. Problem

Developers who rely on an AI agent accumulate substantial personal state — memories, skills, conversation history — that exists only on a single computer in a proprietary format. A disk failure, an accidental delete, an OS reinstall, or a forced migration to a new machine destroys this state with no recovery path. There is no standard way to take that state and store it elsewhere.

Aplexica OSS solves this for one computer at a time: snapshot the state of any supported agent into a portable archive, store the archive wherever the user keeps backups (Time Machine, Restic, rsync, an external drive, etc.), and restore from it later — either back into the same agent or into a different agent.

## 2. Users and use cases

| Use case | Trigger | Outcome |
|---|---|---|
| **Disaster recovery** | Disk failure, OS reinstall, accidental delete | Restore yesterday's `.aplexica` archive into the same agent and resume work. |
| **Migration to a new computer** | Got a new laptop | Export from the old machine, copy the archive, import on the new machine. |
| **Trying a new agent** | Curious about Codex; have been using Claude Code | Export from Claude Code, run `aplexica convert`, import into Codex. Continue with full history available. |
| **Pre-experiment snapshot** | About to install a risky skill or wipe a memory | `aplexica export` a snapshot; revert from it if the experiment goes badly. |
| **Compliance / audit retention** | Internal policy requires a record of AI agent interactions | Schedule daily exports via cron; ship archives to compliant storage. |

## 3. Scope

In scope for this BRD:
- A CLI command surface for `export`, `import`, and `convert`.
- A portable archive format (`.aplexica` bundle).
- The semantics of what gets exported, what gets imported, and how conflicts are handled at import time.
- Behavior when the source and target are the same agent (round-trip) versus different agents (conversion).

Out of scope here (covered in other BRDs):
- The per-agent translation logic itself — see [02-brd-format-adapters.md](02-brd-format-adapters.md).
- Real-time sync — see [03-brd-local-realtime-sync.md](03-brd-local-realtime-sync.md).
- Forking and branching — see [04-brd-forking-and-merging.md](04-brd-forking-and-merging.md).
- Remote or cross-device backup via transport plugins — outside the scope of this doc.

## 4. Functional requirements

### 4.1 Export

- **FR-01.1** The CLI MUST support `aplexica export --agent <name> [--out <path>]` to produce a `.aplexica` bundle of the named agent's full state.
- **FR-01.2** The CLI MUST support `aplexica export --all [--out <path>]` to produce a single bundle covering every installed and Aplexica-recognized agent on the machine.
- **FR-01.3** When `--out` is omitted, the bundle MUST be written to `$PWD/aplexica-backup-<YYYYMMDD-HHMMSS>.aplexica`.
- **FR-01.4** Export MUST include all artifacts of types `memory`, `skill`, `tool`, and `conversation` that the source agent's adapter recognizes. For `tool` artifacts: configs are always included; secret *values* are included only when the tool's `syncSecrets` flag is `true` (default `false`). When `syncSecrets` is `false`, the bundle records the secret *name* and the bundle manifest includes a `missingSecrets` list so the importing user can set them — see [02-brd-format-adapters.md §4.4](02-brd-format-adapters.md).
- **FR-01.5** Export MUST be safe to run while the agent is actively in use. Files in flight are read consistently (snapshot read or stable read with retry).
- **FR-01.6** Export MUST complete without modifying the source agent's native storage in any way.
- **FR-01.7** The bundle MUST be a single file (a deterministic, compressed archive) that the user can copy, store, encrypt, or back up using their existing tools.
- **FR-01.8** The bundle MUST include a manifest with: schema version, source agent identifier and version, an opaque source-device identifier, export timestamp, artifact counts, total size, and the hash of each included artifact.
- **FR-01.9** The export operation MUST be idempotent over short intervals — running export twice in quick succession with no agent activity in between MUST produce bundles with identical artifact hashes (though manifest timestamps will differ).
- **FR-01.10** The CLI MUST support `--include` and `--exclude` flags taking artifact-type filters (`memory`, `skill`, `tool`, `conversation`) and tag filters (`--tag work`). For tool artifacts, an additional `--include-secrets` flag (default off) overrides each tool's `syncSecrets` flag and forces secret values into the bundle. A bundle containing secrets is highly sensitive: it is intended only for controlled disaster recovery, MUST be encrypted before it leaves the device, and MUST NOT be used for routine sharing. The CLI MUST also support **scope filters**: `--scope global|project|namespace` to filter by scope kind, `--project <id>` (repeatable) to include only specific projects, `--include-pending-projects` to include staged-but-unmaterialized projects in the bundle. Default behavior: include everything in scope on the source device, including pending projects.
- **FR-01.11** Export MUST print a summary on completion: agent(s), artifact counts by type, total compressed size, bundle path.

### 4.2 Import

- **FR-01.12** The CLI MUST support `aplexica import <bundle> --agent <name>` to restore a bundle's artifacts into the named target agent.
- **FR-01.13** The CLI MUST support `aplexica import <bundle> --agent <name> --dry-run`, which produces the full diff that would be applied without modifying the target.
- **FR-01.14** Import MUST handle three cases:
  - **Round-trip** — source agent in the bundle equals target agent. Artifacts are restored verbatim where possible.
  - **Cross-agent migration** — source agent differs from target agent. The bundle is interpreted through ACF, then materialized to the target via that agent's adapter.
  - **Mixed bundle** (`--all` export) — bundle contains state from multiple agents; user MUST specify `--agent` to select which target to import into, or `--agent-map <source>=<target>,…` for explicit routing.
- **FR-01.15** Import MUST detect collisions (an artifact in the bundle already exists in the target's current state) and offer four resolution modes via `--on-collision`:
  - `prompt` (default for interactive sessions): ask the user per-collision.
  - `skip`: keep the target's existing version.
  - `replace`: overwrite with the bundle's version.
  - `branch`: import the bundle's version as a new branch (only meaningful for conversations).
- **FR-01.16** Import MUST be atomic per artifact — a failure mid-import leaves the target in a self-consistent state with a partial-import report written to `~/.aplexica/logs/imports/<timestamp>.json`.
- **FR-01.17** Import MUST print a summary on completion: artifacts imported, artifacts skipped, conflicts, conversion warnings, target agent restart required (if any).
- **FR-01.18** Import MUST require the target agent's process to be **stopped** for any agent whose storage format does not tolerate concurrent writes (documented per-adapter). The CLI MUST detect a running process and either (a) refuse, (b) offer to stop it, or (c) proceed in safe mode — based on a per-adapter declaration.

### 4.3 Convert

- **FR-01.19** The CLI MUST support `aplexica convert <bundle> --to <agent> [--out <path>]` to transcode a bundle from its source agent's format into another agent's format **without importing it**. This is a tooling primitive for users who want to inspect, modify, or ship the converted bundle separately.
- **FR-01.20** Convert MUST produce a new bundle whose manifest records both the original source agent and the conversion target.
- **FR-01.21** Convert MUST produce a **conversion report** alongside the output bundle listing every fidelity loss (tool call that has no equivalent in the target agent, memory format the target cannot represent, skill that requires a runtime the target lacks). The report MUST be human-readable Markdown.

### 4.4 Bundle format

- **FR-01.22** Bundles MUST be a single file with the extension `.aplexica`.
- **FR-01.23** The file MUST be a deterministic ZIP archive (no embedded timestamps in entries beyond the manifest) so that two equivalent exports produce byte-identical bundles when sorted deterministically.
- **FR-01.24** The bundle MUST contain at the root:
  - `manifest.json` — schema version, source agent(s), host, timestamp, artifact counts, hashes, signature.
  - `acf/memories/*.json` — one file per memory artifact, in ACF.
  - `acf/skills/*.json` — one file per skill artifact, in ACF.
  - `acf/tools/*.json` — one file per tool artifact, in ACF.
  - `acf/conversations/*.jsonl` — one file per conversation (event log).
  - `native/<agent>/*` (optional) — verbatim copies of the source agent's native files when the adapter elects to preserve them for round-trip fidelity.
- **FR-01.25** Bundles MUST be readable by `aplexica show` (manifest dump) and `aplexica explore <bundle>` (interactive browse) without import.
- **FR-01.26** Bundles MUST validate against the ACF JSON Schema published with the release; `aplexica verify <bundle>` is the entry point.

### 4.5 Bundle signing

**Backups are signed by default.** On the first `aplexica backup`, Aplexica creates or loads a private Ed25519 key at `~/.aplexica/keys/backup-signing.ed25519`, protected as private user state. A detached `<bundle>.sig` file covers the SHA-256 hash of the exact bundle bytes using the documented `acf-sig v1` format.

- **FR-01.29** The first signed backup MUST create its signing key with restrictive permissions and MUST never transmit the private key.
- **FR-01.30** `aplexica backup` MUST sign by default. `--key <private-key>` selects an explicit signing key, and `--unsigned` is the explicit per-invocation opt-out.
- **FR-01.31** `aplexica restore --verify --pubkey <public-key> --key-id <sha256-key-id> <bundle>` MUST verify the detached signature and the independently pinned full key ID before restoring. A missing, malformed, or mismatched signature is a hard failure when verification is requested.
- **FR-01.32** `aplexica keygen` MUST generate a separate Ed25519 private/public key pair when the user wants an explicitly managed signing identity. `aplexica keygen-age` generates an unrelated `age` recipient identity for bundle encryption.
- **FR-01.33** The signature format MUST be offline-capable, versioned, and independent of any hosted identity provider.

### 4.6 `export --all` produces one mega-bundle (decided 2026-05-18, was OQ-01.2)

When `aplexica export --all` is used, the result is a **single bundle file** covering every installed and recognized agent on the machine, not one bundle per agent. The manifest enumerates per-agent sections; the canonical store path mirrors the multi-agent layout.

- **FR-01.35** `aplexica export --all` MUST produce a single `.aplexica` bundle whose manifest's `sourceAgents` field is a list of every agent included.
- **FR-01.36** The bundle's `acf/` tree MUST partition artifacts by type (memory, skill, tool, conversation) but tag each artifact with its source agent in its ACF metadata (`createdBy.agent`), so an importer can route correctly with `--agent-map` (see FR-01.14).
- **FR-01.37** Manifest counters in mega-bundles MUST present both per-agent and per-type breakdowns, so `aplexica show <mega-bundle>` is immediately readable.

Rationale (recorded): one mega-bundle is the most natural unit for backup ("this is my entire Aplexica state on this machine on this date"), avoids the proliferation of small files for users with many agents, and the manifest's per-agent partitioning means selective import is still trivial (`aplexica import <mega-bundle> --agent claude-code` ignores the other agents' contents).

### 4.7 Import phase ordering

Bundles carry four artifact types that reference each other: tools provide capabilities, skills can use tools, conversations reference tools and skills, memories are independent. When a bundle is imported (either round-trip into the same agent or cross-agent migration), the importer follows a **fixed four-phase order** so that by the time a phase begins, every artifact it might reference has already been imported.

**Migration import order:** `tool` → `memory` → `skill` → `conversation`.

Within each phase, artifacts may import in any order (no dependencies within type). Across phases, the order is fixed.

This ordering matters only for the bulk-import path (`aplexica import`, `aplexica convert`). The real-time sync path naturally satisfies the same dependency invariant via event-time ordering — events are applied in the order they were produced, so a `skill_create` event always precedes a `tool_call` event that references that skill. See [03-brd-local-realtime-sync.md §4.7](03-brd-local-realtime-sync.md).

- **FR-01.38** Import MUST proceed in phase order: `tool` → `memory` → `skill` → `conversation`. Each phase MUST complete before the next phase begins. Within a phase, artifacts may import in any order.
- **FR-01.39** A phase failure (e.g., the target agent's adapter refuses to materialize a tool kind it doesn't support) MUST NOT block subsequent phases by default. The phase aggregates fidelity warnings and the importer proceeds. Users can request strict behavior via `aplexica import --atomic`, which aborts on any phase failure and rolls back successfully-imported artifacts.
- **FR-01.40** The final migration fidelity report MUST list missing dependencies in dependency order (tools first, then skills referencing missing tools, then conversations referencing missing tools or skills). This ordering helps users fix foundational gaps first; resolving a missing tool typically un-breaks every skill and conversation that referenced it.
- **FR-01.41** A conversation containing `tool_call` events that reference tools whose secrets are not yet set on the target machine MUST still import successfully. Historical `tool_call` / `tool_result` events render as recorded — they are part of the conversation's history, not live invocations. **New** invocations of that tool *after* import will hit the standard "missing secret" UX in the target agent (per the secrets store described in [02-brd-format-adapters.md §4.4.1](02-brd-format-adapters.md)). This is the Option-A semantic — historical fidelity is preserved; the user is prompted to set secrets only when they make a new call.
- **FR-01.42** Project-scoped artifacts in a bundle MUST be imported per the "stage and wait" rule (see [02-brd-format-adapters.md §4.13.4](02-brd-format-adapters.md)): if the target device does not have the project locally, the artifacts land in the pending-project staging area and the import report lists "pending projects" so the user knows what to clone. If the target has the project locally (matched by canonical project ID, typically git remote URL), artifacts materialize natively at that path.
- **FR-01.43** The import summary MUST distinguish artifacts by scope: counts and statuses for global artifacts, per-project counts for project-scoped artifacts (with materialized vs. pending split), and per-namespace counts for namespace-scoped artifacts.

### 4.8 Discovery and dry-run

- **FR-01.27** `aplexica list-agents` MUST output every supported agent and indicate which are detected as installed on the current machine (based on the standard storage path or an environment override).
- **FR-01.28** `aplexica status` MUST output, for each installed agent, the artifact counts currently visible to the adapter and the last export/import timestamp.

## 5. Non-functional requirements

| ID | Requirement |
|---|---|
| **NFR-01.1** | Export of a 1 GB-equivalent agent state MUST complete in under 60 seconds on a 2023-vintage laptop. |
| **NFR-01.2** | Import (dry-run) MUST complete in under 5 seconds for a 100 MB bundle. |
| **NFR-01.3** | Bundle compression ratio MUST be at least 3:1 for typical conversation-history payloads (which are highly compressible text). |
| **NFR-01.4** | Export and import operations MUST be cancelable via `Ctrl-C` without leaving the target agent in an inconsistent state. |
| **NFR-01.5** | All CLI operations MUST emit machine-readable JSON output when `--json` is passed. |
| **NFR-01.6** | All CLI operations MUST exit with stable exit codes documented per command. |
| **NFR-01.7** | The bundle format MUST be forward-compatible: a v2 reader MUST accept v1 bundles. The reader MUST refuse newer bundles with a clear error rather than silently best-effort. |

## 6. Out of scope

- Key escrow or recovery for encrypted bundles. Native `age` encryption supports a recipient key or a passphrase read from non-terminal standard input; Aplexica cannot recover a lost passphrase or private key.
- Selective restore at the *sub-artifact* level (e.g., "import only turn 14 onward of conversation X"). V1 supports per-artifact selection only.
- A GUI for export/import. V1 is CLI-only.
- Backing up the agent's binary, configuration files, or model weights. Aplexica is concerned with the developer's state, not the agent's installation.

## 7. Acceptance criteria

V1 of backup & restore is complete when:

1. A developer can run `aplexica export --agent claude-code` on macOS, Linux, and Windows and produce a valid bundle.
2. A developer can run `aplexica import <bundle> --agent claude-code` and the target agent, when restarted, reflects the imported state in its UI.
3. A developer can run `aplexica convert <bundle> --to codex` and the produced bundle imports into Codex without error and with the documented fidelity report.
4. All five V1 agents pass a round-trip test: export → import into the same agent → diff the resulting native storage against the source. Differences are limited to documented non-semantic noise (e.g., file mtimes).
5. All five V1 agents pass a cross-conversion test: export → convert to each of the other four → import → verify ACF round-trip equivalence (the original ACF event log matches the re-imported ACF event log modulo documented lossy mappings).
6. `aplexica verify` rejects a deliberately corrupted bundle with a clear error.
7. `--json` output is documented and stable across all CLI commands in this BRD.

## 8. Resolved decisions

All open questions for this BRD were resolved on 2026-05-18:

- **OQ-01.1 — Decided: backups are signed by default** using the versioned Ed25519 `acf-sig v1` detached-signature format. See §4.5.
- **OQ-01.2 — Decided: `aplexica export --all` produces one mega-bundle** covering every installed agent. See §4.6.
- **OQ-01.3 — Decided: anonymized backup is supported.** The anonymized path removes known personal and secret patterns and excludes raw attachment blobs and secret values.

## 9. Dependencies

- ACF schema must be finalized — see [02-brd-format-adapters.md](02-brd-format-adapters.md).
- Per-agent adapters for all five V1 agents must be implemented — see [02-brd-format-adapters.md](02-brd-format-adapters.md).
- Canonical store layout — see [02-brd-format-adapters.md](02-brd-format-adapters.md) and [03-brd-local-realtime-sync.md](03-brd-local-realtime-sync.md).
