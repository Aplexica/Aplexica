# Appendix A — Glossary (OSS Edition)

**Document type:** Glossary
**Status:** Draft v1
**Last updated:** 2026-05-19
**Edition:** OSS (Aplexica open-source)
**Maintainer:** Aplexica project

A single, authoritative definition of each term used across the Aplexica documents. When two documents disagree, this glossary wins; flag the disagreement as a doc bug.

| Term | Definition |
|---|---|
| **ACF** | Aplexica Canonical Format. The schema used inside the canonical store. See [02-brd-format-adapters.md](02-brd-format-adapters.md). |
| **Adapter** | A plugin module that translates one agent's native on-disk state into ACF (inbound) and ACF back into that agent's native state (outbound). Each supported agent has exactly one adapter. |
| **Agent** | An AI coding tool whose state Aplexica manages. V1 agents: Claude Code, Codex, Hermes, OpenClaw, Kilo. |
| **aplexica** | The CLI binary. |
| **Aplexica daemon** | The local sync daemon, run through `aplexica daemon`. It runs as a user-level background process under launchd, `systemd --user`, or a Windows logon Scheduled Task. |
| **Aplexica OSS** | This open-source project (AGPL-3.0). Provides backup, restore, conversion, local sync, forking, and selective sync — all on a single machine. |
| **Artifact** | A unit of agent state Aplexica tracks. Four artifact types in V1: `memory`, `skill`, `tool`, `conversation`. |
| **Branch** | A named line of history for a conversation. Defaults to `main`. Each fork creates a new branch. |
| **Canonical Store** | The Aplexica-managed directory (default `~/.aplexica/store/`) where artifacts live in ACF, regardless of which agents are installed. Source of truth for everything Aplexica does. |
| **Checkout** | Selecting which branch of a conversation is currently materialized into a given agent's native storage. |
| **Conversation** | A single ongoing dialog between a user and an agent. Stored as an append-only event log of turns plus tool calls and results. |
| **Device** | A computer running an Aplexica daemon. Each device has a stable, user-readable name and an opaque device-ID. |
| **Event** | A single immutable record in the canonical store's append-only log. Has a unique hash, a parent hash, a branch, a timestamp, a source device, an originating agent, and a payload. |
| **Event log** | The append-only sequence of events for a single artifact. Replaying the log from genesis reconstructs the artifact's current state. |
| **Fan-out** | The daemon's operation of taking an inbound event from one adapter and routing it to other adapters on the same machine, filtered by sync rules. |
| **Fork** | Creating a new branch from an existing point in a conversation's history, typically to continue the conversation in a different direction or in a different agent. |
| **Inbound** | An event flowing from a native agent's storage into the canonical store. (Adapter terminology.) |
| **Memory** | An artifact representing a fact the agent remembers about the user, the project, or the team. Different agents store this differently (CLAUDE.md, AGENTS.md, profile JSON, etc.); the adapter normalizes. |
| **Merge** | Combining two divergent branches into a single branch. Manual operation; user resolves conflicts. |
| **Namespace** | An opaque, daemon-shared identifier that groups artifacts into a local logical scope. Used by `scope.kind = "namespace"` entries to identify a shared scope on the local machine (see BRD-02 §4.13). Not an RBAC or multi-user concept in the OSS edition. |
| **Native format** | The on-disk format used by a specific agent. Distinct from ACF. The adapter knows both. |
| **Outbound** | An event flowing from the canonical store back into a native agent's storage. (Adapter terminology.) |
| **Sync rule** | A user-defined or default policy that decides which artifacts go to which agents. Sync rules govern local fan-out on the current machine. |
| **Skill** | An artifact representing a reusable instruction set or plugin the user has installed in an agent. Different agents call this different things (skills, prompts, modes, modules); the adapter normalizes. |
| **Snapshot** | Two distinct meanings: (1) An ACF event of `type: "snapshot"` that encodes the materialized state of an artifact at a point in time, used to bound replay cost and enable pruning of older events. See [02-brd-format-adapters.md §4.12](02-brd-format-adapters.md). (2) A point-in-time export of artifacts to a portable archive file (`.aplexica` bundle), produced by `aplexica export`. Context disambiguates; the first meaning is the more common one in operational contexts. |
| **Compaction** | The retention engine's operation of moving older events covered by a snapshot into the staging area (`events/.compacted/`) where they remain accessible during the grace period before final deletion. See [03-brd-local-realtime-sync.md §4.8](03-brd-local-realtime-sync.md). |
| **Retention engine** | The component of the daemon that creates snapshots, evicts old attachments, and prunes events. Runs on snapshot creation (primary) and on disk-pressure (emergency). See [03-brd-local-realtime-sync.md §4.8](03-brd-local-realtime-sync.md). |
| **Pruning** | The deletion of events older than a snapshot, after the grace period elapses. Pruning is branch-ancestor-safe: an event reachable from any active branch head is never pruned. |
| **Grace period** | The configurable window (default 7 days) during which events that have been compacted remain accessible in `events/.compacted/` before being deleted permanently. Allows rollback and forensic inspection. |
| **Attachment-only eviction** | A lighter retention mode that evicts binary attachment payloads (base64 blobs) from old events while preserving event metadata and text content. OSS default. See [03-brd-local-realtime-sync.md §4.8.3](03-brd-local-realtime-sync.md). |
| **GC** | Garbage collection. The combined snapshot-creation + attachment-eviction + event-pruning pass the retention engine performs. Invoked automatically; user can also run `aplexica gc`. |
| **Pin / pinning** | Tagging an artifact with `pinned` or `keep-forever` (or any tag in `retention.pin_tags`) to exempt it from all retention policies. |
| **Scope** | A first-class attribute of every artifact declaring where it applies. Two primary kinds in V1 OSS: `global` (user-wide) and `project` (specific project/repo). See [02-brd-format-adapters.md §4.13](02-brd-format-adapters.md). |
| **Global scope** | `scope.kind = "global"`. The artifact applies user-wide on every device the user owns; lives in the agent's user-level storage (`~/.claude/`, `~/.codex/`, etc.). Default scope when none is detected. |
| **Project scope** | `scope.kind = "project"`. The artifact applies only when the user is working in a specific project; lives in project-local storage (`./.claude/`, `./AGENTS.md`, etc.). Identified by canonical project ID. |
| **Project ID** | Canonical identifier for a project, derived from the git remote URL when available (e.g., `github.com/example/sample-repo`) and from a stable path-derived hash when not. The same git repo at different local paths on different devices produces the same project ID. |
| **Pending project** | A project whose artifacts have been imported or restored on the local machine but whose repo is not currently cloned locally. Artifacts stage in `~/.aplexica/store/pending/<projectId>/` and materialize automatically when the project is detected. |
| **Ephemeral project** | A project explicitly created in an ad-hoc directory via `aplexica project init --ephemeral`. Default sync rules exclude ephemeral projects from local multi-agent fan-out. |
| **Tag** | A label attached to an artifact (or a class of artifacts) that sync rules can match on. Tags are how users express "this memory is work-only," "these skills are experimental," etc. |
| **Tool** | An artifact representing a user-installed extension to an agent — distinct from the built-in tools (Read, Write, Bash, Grep, etc.) that ship with the agent. Five tool kinds in V1: `mcp-server`, `subagent`, `hook`, `slash-command`, `plugin`. MCP server configurations are the dominant case. |
| **Secret** | A sensitive value (API key, OAuth token, credential file) referenced by a tool artifact via a named placeholder. Secrets live in a separate, locally-restricted directory (`~/.aplexica/secrets/`). By default they are NEVER synced; the user opts in per tool via `aplexica tool sync-secrets --enable`. |
| **syncSecrets** | A per-tool-artifact boolean flag. `false` (default): tool configs sync normally but secret values stay local. `true`: secret values are included in the canonical store and synced alongside the tool artifact. |
| **Turn** | A single exchange in a conversation — a user message and the agent's response (which may include tool calls and tool results). The atomic unit of conversation history in ACF. |
