# BRD-02 — Format Adapters and the Canonical Format

**Document type:** Business Requirements Document
**Status:** Draft v1
**Last updated:** 2026-05-17
**Edition:** OSS (no subscription required)
**Maintainer:** Aplexica project

---

> This BRD records requirements and design intent. For currently shipped
> command syntax, use the [user guide](user-guide.md) and `aplexica help`.

## 1. Problem

Every agent stores its state in a proprietary format on disk. Claude Code uses one structure, Codex another, Hermes another, OpenClaw another, Kilo another. For Aplexica to back up, restore, sync, convert, and route artifacts across these agents, every operation has to be expressed in a single agent-agnostic vocabulary — otherwise the system would require N×N translators (25 in V1) and each new agent would require touching every other adapter.

The Aplexica Canonical Format (ACF) is the agent-agnostic vocabulary. Each agent has one **adapter** that translates between that agent's native files and ACF. All Aplexica internals operate on ACF; native formats only exist at the boundary.

## 2. Users and use cases

| Role | Use case |
|---|---|
| **End user** | Doesn't see ACF directly. Sees the consequences: smooth conversion, lossless round-trip, transparent fidelity reports. |
| **Adapter author** (Aplexica core team, then third-party) | Implements `read → ACF` and `ACF → write` for an agent. Needs a stable ACF schema, a clear adapter API, conformance tests. |
| **Power user** | Reads ACF for their own scripting (artifacts are JSON / JSONL — diff-friendly, grep-friendly). |

## 3. Scope

In scope:
- The ACF schema for the four artifact types: memory, skill, tool, conversation.
- The canonical store layout on disk.
- The adapter API (function signatures, contract, lifecycle).
- Conformance tests that every adapter must pass.
- Fidelity-reporting expectations.
- The five V1 logical adapters: Claude Code, Codex, Hermes, OpenClaw, Kilo.
  One adapter may serve multiple user-facing surfaces that share an engine and
  native storage. In particular, Claude Code CLI/Desktop and Codex
  CLI/Desktop MUST NOT be registered as duplicate adapter owners.
- Multi-surface adapters MUST detect each surface independently, MUST operate
  when only one surface is installed, and MUST activate a later CLI/Desktop
  installation without writing native state while both surfaces are absent.

Out of scope:
- Transport plugins (cross-device sync, remote backup) — outside the scope of this doc.
- Sync rule expression — see [05-brd-selective-sync-and-routing.md](05-brd-selective-sync-and-routing.md).
- Branch/fork semantics — see [04-brd-forking-and-merging.md](04-brd-forking-and-merging.md). ACF is branch-aware; the semantics live there.

## 4. The canonical format (ACF)

### 4.1 Top-level principles

1. **JSON for memories and skills, JSONL for conversation event logs.** Diff-friendly. Grep-friendly. Streamable.
2. **Append-only at the event log level.** Existing events are never edited. Edits are expressed as new events that supersede prior ones (`type: "redaction"`, `type: "amendment"`).
3. **Hash-addressable.** Every event carries a SHA-256 hash of its content. Hashes chain by referencing the parent event's hash, forming a Merkle log per artifact.
4. **Self-describing via schema version.** Every ACF document carries a `acfSchemaVersion` field. Readers refuse newer versions with a clear error.
5. **Stable identifiers.** Each artifact has a UUIDv7 ID assigned at first ingestion. The ID is stable for the artifact's lifetime, even as it is converted between agents.
6. **Provenance everywhere.** Every event records its source device, source agent, and the user who triggered it (when applicable).

### 4.2 Artifact: memory

A memory is a fact or instruction the agent retains across sessions. Different agents represent memories as Markdown files (Claude Code's `CLAUDE.md`), structured JSON profiles, prompt prefixes, or rule documents. ACF normalizes them.

```jsonc
{
  "acfSchemaVersion": "1.0",
  "id": "0193ce1a-1b50-7000-a9b8-e09e30dbb33f",
  "type": "memory",
  "title": "User profile",
  "content": {
    "kind": "markdown",
    "body": "# User profile\n\n…"
  },
  "tags": ["user", "core"],
  "scope": { "kind": "global" },
  "createdAt": "2026-04-12T08:14:22Z",
  "updatedAt": "2026-05-17T19:00:00Z",
  "createdBy": {
    "device": "example-laptop",
    "agent": "claude-code",
    "agentVersion": "2.x.x"
  },
  "nativeRef": {
    "agent": "claude-code",
    "path": ".claude/memory/user_profile.md"
  },
  "history": [
    { "eventId": "…", "hash": "…", "parentHash": null, "op": "create", "at": "2026-04-12T08:14:22Z" },
    { "eventId": "…", "hash": "…", "parentHash": "…", "op": "update", "at": "2026-05-17T19:00:00Z" }
  ]
}
```

- **`content.kind`** is one of `markdown`, `text`, `json`, `yaml`. Adapters pick the kind closest to the native format and document the mapping.
- **`tags`** drive selective-sync routing (see [05-brd-selective-sync-and-routing.md](05-brd-selective-sync-and-routing.md)).
- **`nativeRef`** is hint metadata — the adapter that originated the memory used this path; it is informational, not authoritative.
- **`history`** is the append-only event log condensed; full event payloads live in `events/memories/<id>.jsonl`.

### 4.3 Artifact: skill

A skill is a reusable instruction set or plugin the agent can invoke. Across agents this concept is called variously: skill, prompt, mode, module, command, slash command, sub-agent. ACF unifies them.

```jsonc
{
  "acfSchemaVersion": "1.0",
  "id": "0193ce1a-1b50-7000-a9b8-e09e30dbb33f",
  "type": "skill",
  "name": "code-review",
  "description": "Review a code change for security, performance, correctness.",
  "instructions": {
    "kind": "markdown",
    "body": "# code-review\n\nWhen invoked, …"
  },
  "trigger": {
    "kind": "invocation",
    "patterns": ["/code-review", "review this code"]
  },
  "tools": ["read", "grep", "git"],
  "capabilities": ["filesystem-read", "shell-execute"],
  "tags": ["engineering"],
  "scope": { "kind": "global" },
  "createdAt": "2026-04-01T12:00:00Z",
  "updatedAt": "2026-05-10T08:00:00Z",
  "createdBy": { … },
  "nativeRef": { "agent": "claude-code", "path": ".claude/skills/code-review/SKILL.md" },
  "compatibility": {
    "claude-code": "native",
    "codex": "instructions-only",
    "hermes": "native",
    "openclaw": "instructions-only",
    "kilo": "unsupported"
  },
  "history": [ … ]
}
```

- **`trigger.kind`** is one of `invocation` (slash command / explicit call), `automatic` (proactive, the agent decides), `lifecycle` (e.g., on session start).
- **`tools`** and **`capabilities`** declare what the skill needs to run. If a target agent doesn't expose a required capability, conversion produces a fidelity warning.
- **`compatibility`** is filled in by the source adapter at write-time based on a known compatibility matrix; it is advisory. The target adapter checks it when materializing.

### 4.4 Artifact: tool

A tool is a user-installed extension to an agent — distinct from the built-in tools (Read, Write, Bash, Grep, etc.) that ship with the agent. Across agents, "tool added later" takes several forms: MCP server configurations, custom subagents, hooks, slash commands, and plugins. ACF unifies them under one artifact type.

Tools accumulate the same way memories and skills do. A developer who has wired up 15 MCP servers (Gmail, Linear, GitHub, JetBrains, internal company tools, …) has invested real time in that configuration — losing it on a new machine, or having to re-do it for a second agent, is the N× tax Aplexica exists to eliminate.

```jsonc
{
  "acfSchemaVersion": "1.0",
  "id": "0193ce1a-1b50-7000-a9b8-e09e30dbb33f",
  "type": "tool",
  "kind": "mcp-server",
  "name": "github",
  "description": "GitHub MCP server for repo / issue / PR operations.",
  "config": {
    "kind": "json",
    "body": {
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-github"],
      "env": {
        "GITHUB_TOKEN": "${secret:github-token}"
      }
    }
  },
  "capabilities": ["network", "github-api", "issue-tracker"],
  "secretsRefs": ["github-token"],
  "syncSecrets": false,
  "tags": ["work"],
  "scope": {
    "kind": "project",
    "project": {
      "id": "github.com/example/sample-repo",
      "path": "/Users/example/work/sample-repo",
      "displayName": "sample-repo",
      "vcs": "git",
      "ephemeral": false
    }
  },
  "createdAt": "2026-04-01T12:00:00Z",
  "updatedAt": "2026-05-10T08:00:00Z",
  "createdBy": { … },
  "nativeRef": { "agent": "claude-code", "path": ".mcp.json#servers.github" },
  "compatibility": {
    "claude-code": "native",
    "codex": "native",
    "hermes": "native",
    "openclaw": "native",
    "kilo": "native"
  },
  "history": [ … ]
}
```

Field semantics:

- **`kind`** is one of `mcp-server`, `subagent`, `hook`, `slash-command`, `plugin`. Adapters MUST declare which kinds they support in `compatibilityMatrix()`. MCP servers are universally supported because MCP is the cross-agent standard; the other kinds vary in support per agent.
- **`config.kind`** matches the native serialization (`json`, `yaml`, `toml`, `markdown`, `script`). **`config.body`** is the verbatim native configuration object — the entry that goes into `.mcp.json`, the body of a hook script, the YAML of a subagent definition, etc. Configs are first-class portable artifacts.
- **`capabilities`** declares what the tool can do — `network`, `filesystem-read`, `filesystem-write`, `shell-execute`, plus tool-specific tags (`github-api`, `gmail-api`, etc.). Used by selective-sync rules and DLP detector hooks (see [05-brd-selective-sync-and-routing.md](05-brd-selective-sync-and-routing.md)).
- **`secretsRefs`** lists the named secrets this tool requires. They appear in `config.body` as `${secret:<name>}` placeholders. The actual secret values are NEVER in the artifact itself.
- **`syncSecrets`** is the per-tool opt-in flag for whether secret values should sync alongside the config. Default `false`: configs sync everywhere, secrets stay local. When `true`: the named secrets are pulled from the secrets store and included with the artifact (encrypted in the canonical store; transport plugins MAY apply additional encryption in transit). Users flip this per tool via `aplexica tool sync-secrets <id> --enable`.
- **`compatibility`** is supplied by the source adapter at write-time. MCP servers typically show as `native` across all five V1 agents (since MCP is universal). Other kinds vary: a Claude Code subagent may be `native` in Claude Code but `instructions-only` (rendered as a documented prompt) in Codex.

#### 4.4.1 Secrets store

Secret values live in a separate, locally-restricted directory: `~/.aplexica/secrets/`, with mode `0700` on Unix and ACL-restricted on Windows. Format:

```
~/.aplexica/secrets/
├── github-token            # one file per named secret
├── linear-api-key
└── gmail-oauth.json
```

Each secret has a JSON sidecar `~/.aplexica/secrets/.meta/<name>.json` recording: `createdAt`, `updatedAt`, `usedByTools` (list of tool artifact IDs), `syncEnabled` (bool — mirrors the most recent `syncSecrets` flag for any tool using it). The CLI provides `aplexica secret set/get/list/delete/rotate/sync-enable/sync-disable`.

When `syncSecrets` is `true` for any tool referencing a secret, the secret value is included in the canonical store wrapped with the user's content key — the same encryption that protects everything else. The daemon sets a `secretsPresent: true` metadata flag on the event envelope so DLP detector hooks can intercept before the event is forwarded via the plugin API.

When `syncSecrets` becomes `false` again (or the user runs `aplexica secret sync-disable <name>`), the secret is removed from the canonical store on the next event cycle. Past events that carried the secret are not retroactively rewritten — see security-and-trust-model §4.4 for the documented forward-only erasure semantics.

### 4.5 Artifact: conversation

A conversation is the unit that exhibits the most fidelity-sensitive structure. It is stored as an append-only event log of turns and tool interactions. Each turn is a single event.

The conversation header (`acf/conversations/<id>.json`):

```jsonc
{
  "acfSchemaVersion": "1.0",
  "id": "0193ce1a-1b50-7000-a9b8-e09e30dbb33f",
  "type": "conversation",
  "title": "Refactor auth middleware",
  "branches": {
    "main": { "head": "<eventId>", "createdAt": "2026-05-01T10:00:00Z" },
    "experiment-zod": { "head": "<eventId>", "parent": "main", "forkedAt": "<eventId>", "createdAt": "2026-05-08T15:23:00Z" }
  },
  "tags": ["work", "auth"],
  "scope": {
    "kind": "project",
    "project": {
      "id": "github.com/example/sample-repo",
      "displayName": "sample-repo",
      "vcs": "git"
    }
  },
  "createdBy": { … },
  "originAgent": "claude-code",
  "currentlyMaterializedIn": {
    "claude-code": "main",
    "codex": "experiment-zod"
  }
}
```

The event log (`acf/events/conversations/<id>.jsonl`), one JSON object per line:

```jsonc
{ "eventId":"…", "hash":"…", "parentHash":null,    "branch":"main", "at":"…", "type":"turn", "role":"user", "content":[{"kind":"text","text":"Refactor auth middleware to use Zod."}] }
{ "eventId":"…", "hash":"…", "parentHash":"…",     "branch":"main", "at":"…", "type":"turn", "role":"assistant", "content":[{"kind":"text","text":"I'll start by …"}] }
{ "eventId":"…", "hash":"…", "parentHash":"…",     "branch":"main", "at":"…", "type":"tool_call", "tool":"read", "input":{"path":"src/auth.ts"}, "callId":"…" }
{ "eventId":"…", "hash":"…", "parentHash":"…",     "branch":"main", "at":"…", "type":"tool_result", "callId":"…", "output":"…", "truncated":false }
{ "eventId":"…", "hash":"…", "parentHash":"…",     "branch":"experiment-zod", "at":"…", "type":"fork", "from":"<eventId-on-main>", "originAgent":"codex", "rationale":"continue in Codex" }
```

Event types in V1:
- `turn` — user or assistant message (with multi-part content: text, image refs, code blocks).
- `tool_call` — model-issued tool invocation.
- `tool_result` — observed result of a tool invocation (paired with `callId`).
- `system_note` — out-of-band annotations (rare; not all agents support).
- `fork` — branch divergence point.
- `merge` — branch convergence point.
- `redaction` — supersedes a prior event's content with a redacted version (event stays, content is replaced).
- `amendment` — adds metadata to a prior event without changing its content.
- `snapshot` — encodes the materialized state of the artifact at this point. Snapshots enable pruning of older events that are superseded by the snapshot. They flow through real-time sync and the branch model identically to any other event. See §4.12.

Adapters MUST map their agent's native conversation representation to this event-log form, and back.

#### Optional event tags

Every event MAY carry an optional **`tags`** array — short string labels users (or rules) attach to specific events for filtering, navigation, and annotation. Distinct from artifact tags (which apply to whole artifacts and drive sync rules); event tags are per-event, primarily for `aplexica log --event-tag <name>` queries and human navigation of long histories.

```jsonc
{ "eventId":"…", "hash":"…", "branch":"main", "at":"…", "type":"turn", "role":"assistant",
  "content":[…],
  "tags":["decision-point", "before-refactor"] }
```

Examples of useful event tags:
- `decision-point` — "this is where I committed to the design"
- `experiment-start` / `experiment-end` — bracket exploratory work
- `breaking-change` — mark when the agent committed to a non-trivial rewrite
- `gold` — "this turn produced the keeper output"
- Custom user-defined values

Reserved tag namespaces on events: `aplexica:*`, `auto:*` (system-applied). Users cannot write to these.

Adapters MUST preserve event tags through round-trip translation. Adapters that materialize into agents whose native format has no equivalent annotation MUST store event tags in a sidecar or as adapter-specific metadata, not silently drop them.

### 4.6 Tool-call portability

The hardest part of conversation portability is tool calls: each agent has its own set of tools with its own names, parameters, and result shapes.

ACF approach:

1. **Preserve verbatim.** A `tool_call` event records the original tool name, input, and call ID exactly as the source agent saw them. This is the source of truth.
2. **Normalize where the mapping is unambiguous.** ACF defines a *canonical tool taxonomy* covering the common operations: `read`, `write`, `edit`, `glob`, `grep`, `bash`, `web_fetch`, `web_search`, `image`. Adapters set a `canonical` field alongside the verbatim record:

    ```jsonc
    { "type":"tool_call", "tool":"Read", "canonical":{"name":"read","input":{"path":"..."}}, "input":{ ... }, … }
    ```

3. **Adapter handles the inverse** at materialization. When writing the conversation into a target agent, the adapter looks at `canonical` first, picks the closest native tool, and translates parameters. If no native equivalent exists, the adapter renders the tool call as an annotated text block in the materialized history and adds a fidelity warning to the conversion report.

The canonical tool taxonomy is intentionally small in V1. Agents have many more tools (database access, image generation, MCP servers, etc.); these all preserve verbatim in the event log and are NOT normalized in V1. They appear as annotated text blocks in target agents that lack the equivalent capability.

### 4.7 Canonical store layout

The canonical store is one directory:

```
~/.aplexica/store/
├── meta.json                          # store-level metadata, schema version
├── devices/
│   └── <deviceId>.json                # device identity and capabilities
├── acf/
│   ├── memories/
│   │   └── <artifactId>.json
│   ├── skills/
│   │   └── <artifactId>.json
│   ├── tools/
│   │   └── <artifactId>.json
│   └── conversations/
│       └── <artifactId>.json
├── events/
│   ├── memories/
│   │   └── <artifactId>.jsonl
│   ├── skills/
│   │   └── <artifactId>.jsonl
│   ├── tools/
│   │   └── <artifactId>.jsonl
│   └── conversations/
│       └── <artifactId>.jsonl
├── refs/
│   └── <agent>/                       # per-agent materialization pointers
│       └── <artifactId>.json          # which branch / which head is currently in this agent's native storage
└── tags/
    └── <tagName>.json                 # tag metadata (description, color, scope)
```

A sibling directory, separately permissioned, holds secret values referenced by tool artifacts (see §4.4.1):

```
~/.aplexica/secrets/              # mode 0700 on Unix; ACL-restricted on Windows
├── <secretName>                   # raw secret value (string, JSON, or binary)
└── .meta/
    └── <secretName>.json          # metadata: createdAt, updatedAt, usedByTools, syncEnabled
```

The store is the single source of truth for Aplexica. Native agent storage is treated as a view of the store, projected by the relevant adapter. The secrets directory is separate from the store on disk (different permissions, different sync rules) and is intentionally NOT part of the canonical store's content-addressable hash chain — the same tool artifact ID can reference a different secret value on each device.

### 4.8 Artifact dependencies

The four artifact types reference each other through specific fields in ACF. These references form a small dependency graph that the importer (see [01-brd-backup-restore.md §4.7](01-brd-backup-restore.md)) and the live sync engine use to maintain consistency.

```
                  tools (provide capabilities)
                    ▲                         ▲
                    │ skill.tools[]           │ conversation.tool_call.tool
                    │ skill.capabilities      │
                    │                         │
                  skills ───────────────────► conversations
                                                ▲
                                                │ conversation references memories
                                                │ in turn content (soft / textual)
                                                │
                                              memories (independent)
```

- **Skills depend on tools.** A skill's `tools[]` array (defined in §4.3) names the tools it expects to be available. Skills also declare `capabilities[]` — Aplexica matches required capabilities to available tools at materialization time.
- **Conversations depend on tools.** Each `tool_call` event (§4.5) names the tool that was invoked. The `canonical` field gives the cross-agent normalization where possible; otherwise the verbatim `tool` field is the dependency edge.
- **Conversations depend on skills.** When a conversation's events include a slash-command invocation or skill reference, that's an edge to the named skill.
- **Conversations reference memories softly.** Conversations may mention memories in turn content (e.g., "remember the user prefers TypeScript"), but the reference is textual, not structural — the conversation imports successfully whether or not the memory is present. No hard dependency.
- **Memories are independent.** They reference nothing else (a memory may *mention* a skill in its prose, but again that's textual, not structural).

This graph drives:

- **Migration import order:** `tool` → `memory` → `skill` → `conversation` (memories ordered before skills for convenience, but they could equally well run in parallel). See [01-brd-backup-restore.md §4.7](01-brd-backup-restore.md).
- **Live sync ordering:** Events are applied in event-time order, which naturally satisfies the dependency invariant because users physically can't reference a tool before installing it. See [03-brd-local-realtime-sync.md §4.7](03-brd-local-realtime-sync.md).
- **Fidelity-report ordering:** Missing-dependency warnings are surfaced in dependency order — tools first, then skills referencing them, then conversations. Users resolve foundational gaps before downstream ones.

### 4.9 Binary attachments (decided 2026-05-18, was OQ-02.2)

Conversations and memories frequently include binary data — screenshots pasted into prompts, image responses from vision tools, file uploads. ACF encodes these **inline as Base64** within the artifact's JSON / JSONL payload, not as out-of-band blobs.

```jsonc
{
  "eventId": "…", "type": "turn", "role": "user",
  "content": [
    { "kind": "text", "text": "Why does this look wrong?" },
    {
      "kind": "image",
      "mimeType": "image/png",
      "encoding": "base64",
      "data": "iVBORw0KGgoAAAANSUhEUgAAA…"
    }
  ]
}
```

**Rationale.** Base64 inlining keeps each artifact self-contained — a single ACF JSON/JSONL file plus its event log contains every byte needed to reconstruct the artifact. No second store, no reference-integrity problems, no orphaned blobs after a fork or branch operation. Diff-friendly, grep-friendly, and trivially portable. Cost: roughly 33% storage overhead vs raw binary, and large attachments (multi-megabyte images, recorded video) inflate the canonical store faster than a content-addressable blob store would.

**Storage implications.** Inline base64 makes storage growth meaningfully faster for users who paste many images. The local canonical store has a configurable disk-quota (`limits.store_max_gb`, see [10-non-functional-requirements.md §10](10-non-functional-requirements.md)) with intelligent eviction of aged caches and archived branches before user artifacts.

**Size limits.**

- **FR-02.23** Individual attachments larger than the configurable per-event size limit (default 16 MB after base64 encoding) MUST be rejected at ingestion time with a clear error. The user can either re-paste the attachment compressed, or raise the limit in config, or — in V2 — opt into the content-addressable blob store path.
- **FR-02.24** Adapters MUST detect attachments approaching the limit (>80% of cap) and surface a warning in `aplexica status` so the user is not surprised by ingestion failures.

**Future compatibility.** The schema remains versioned so a later format can add chunked publishing or content-addressable blobs without silently changing the V1 representation. V1 favors the simplicity and portability of inline payloads.

### 4.10 Schema evolution and compatibility (decided 2026-05-18, was OQ-02.3)

ACF schema evolution MUST be **bidirectionally backward-compatible** between major versions. A V2 daemon must read V1 stores, and — critically — a V1 daemon must continue to read most of a V2 store. This is a strong design constraint: it means V2 changes can be additive only.

**Permitted V2 changes:**

- Add new **optional** fields to existing artifact types. Optional fields have explicit defaults that match V1 behavior.
- Add new artifact types — V1 readers ignore them, treating their files as opaque (the canonical store doesn't break).
- Add new event types — V1 readers skip unknown types but continue replaying known events.
- Add new tool kinds, content kinds, or capability tags — V1 readers treat unknowns as opaque.
- Add new optional fields to events (timestamps, provenance metadata, etc.).

**Forbidden V2 changes:**

- Remove existing fields.
- Change the meaning or required-ness of existing fields.
- Change existing field types (e.g., string → object).
- Change the structure of existing event types.

**Breaking changes are V3.** Any schema change that violates the above rules requires a major version bump to ACF v3.0, at which point a migration tool ships and both V1 and V2 daemons emit "version too new" errors when they encounter v3 stores.

The `acfSchemaVersion` field on every artifact and event remains the source of truth. Readers refuse a newer major version with a clear error; readers parse all known minor versions transparently.

- **FR-02.25** V2 changes to the ACF schema MUST be additive-only per the rules above. Breaking changes require an ACF major-version bump and a dedicated migration tool, and are out of scope for any V1-to-V2 transition.
- **FR-02.26** The reference daemon implementation MUST include a `aplexica schema check <store-path>` command that validates a store's schema version compatibility with the running daemon and reports any reasons for incompatibility.
- **FR-02.27** Every release MUST publish the ACF schema (JSON Schema) under semantic versioning. Minor versions are additive; major versions are breaking.

### 4.11 Agent version tracking (decided 2026-05-18, was OQ-02.4)

Every event in the canonical store records the version of the agent that produced it, in addition to the agent name and source device. This supports forensic analysis when agent behavior changes between versions, helps adapters apply version-conditional logic when a target agent has shifted its native format, and gives users a clear audit trail.

Field shape, in the `createdBy` provenance block:

```jsonc
"createdBy": {
  "device": "example-laptop",
  "agent": "claude-code",
  "agentVersion": "2.7.3",
  "agentBuild": "stable",
  "adapterVersion": "1.2.0"
}
```

- **`agentVersion`** is the version string the source agent reports for itself. Format is agent-defined (semver, calendar version, git SHA), but adapters MUST capture it as a stable string.
- **`agentBuild`** is an optional channel hint (`stable`, `beta`, `nightly`, etc.) when the agent distinguishes them.
- **`adapterVersion`** is the Aplexica adapter version that did the translation. Helpful when diagnosing adapter-specific bugs.

- **FR-02.28** Every adapter's `toAcf` MUST populate `createdBy.agentVersion` for every event. If the source agent does not expose its version, the adapter MUST record `"unknown"` rather than omitting the field. Omitting the field is non-conformant.
- **FR-02.29** Adapters MUST record `createdBy.adapterVersion` matching the adapter package version at runtime.
- **FR-02.30** `aplexica show <bundle>` and `aplexica log <artifactId>` MUST surface version information in human-readable output, so users can see at a glance which agent (and version) produced which event.
- **FR-02.31** Routing rules MAY include a `match.agentVersion` predicate (a semver range) for users who want to filter by version. Stretch capability; document but do not require in V1.

### 4.12 Snapshots

Snapshots are first-class ACF events that encode the materialized state of an artifact at a point in time. They serve two purposes: they bound the cost of replay (a reader doesn't have to replay from genesis; it replays from the latest snapshot), and they enable pruning of older events that are superseded by the snapshot.

Snapshots apply to **all four artifact types** (memory, skill, tool, conversation), though their typical use differs:

- **Conversations:** snapshots act as periodic checkpoints in the linear event log. Events older than a snapshot — and not on any active branch's ancestor chain — become candidates for pruning. Snapshot cadence default: every 100 events OR every 24 hours, whichever fires first.
- **Memories / skills / tools:** snapshots act as rollback points in the update chain. The artifact's primary JSON file is always the current state; snapshots in the event log mark restorable historical states. Default cadence: every 50 events OR weekly.

#### 4.12.1 Snapshot event shape

```jsonc
{
  "eventId": "01f4e5a1-...",
  "hash": "sha256:...",
  "parentHash": "<prev event>",
  "branch": "main",
  "type": "snapshot",
  "at": "2026-05-18T14:00:00Z",
  "by": { "device": "...", "agent": "...", "agentVersion": "..." },
  "encodes": {
    "artifactId": "<id>",
    "stateHash": "sha256:...",
    "eventCountSince": 100,
    "previousSnapshot": "<eventId of prior snapshot or null>"
  },
  "payload": { /* materialized artifact body, identical to what acf/<type>/<id>.json would contain at this moment */ }
}
```

The `payload` field carries the full state. Snapshots are larger than typical events but bounded — they replace many smaller events for the purpose of replay, so total store growth is sublinear.

#### 4.12.2 Pruning gated by snapshots

Once a snapshot event `Sₖ` is durably published, events between `Sₖ₋₁` and `Sₖ` become **pruning candidates**. They are not deleted immediately — a configurable grace period (default 7 days, see [03-brd-local-realtime-sync.md §4.8](03-brd-local-realtime-sync.md)) keeps them in a separate `events/.compacted/` directory before final deletion. This allows rollback and forensic inspection during the grace window.

Pruning is **branch-aware**: an event that is an ancestor of any current branch head MUST NOT be pruned even if a snapshot covers it. The retention engine walks the branch graph before deleting.

#### 4.12.3 Real-time sync interaction

Snapshots flow through the live-sync path as ordinary events. A device that comes online after a long offline period can fetch the most recent snapshot + events since the snapshot, instead of replaying every event from genesis. This is a substantial bandwidth and storage win for laggy or briefly-offline devices.

A transport plugin that coordinates multi-device sync tracks per-device cursors and only allows local pruning past a snapshot once every peer in the namespace has acknowledged the snapshot.

- **FR-02.32** Snapshot events MUST encode the full materialized state of the artifact at the snapshot point. A reader given a snapshot plus any events later than the snapshot's `at` timestamp MUST be able to reconstruct the artifact's current state without earlier events.
- **FR-02.33** Snapshot creation MUST be idempotent: producing two snapshots for the same state hash and the same branch in rapid succession MUST not produce duplicate snapshot events (the second call is a no-op).
- **FR-02.34** The daemon's retention engine (see [03-brd-local-realtime-sync.md §4.8](03-brd-local-realtime-sync.md)) MUST NOT prune events that are ancestors of any active branch head, regardless of snapshot coverage.

### 4.13 Artifact scope

Every artifact has a **scope** that determines where it applies. Three scope kinds in V1:

- **`global`** — applies user-wide. Lives in the agent's user-level storage (`~/.claude/`, `~/.codex/`, etc.). Default scope.
- **`project`** — applies only when the user is working in a specific project. Lives in the project's directory (`./.claude/`, `./AGENTS.md`, etc.). Project identity is canonical (git remote URL) when available; path-derived fallback for non-git projects.
- **`namespace`** — applies to a logical namespace shared across devices via a transport plugin. The namespace value is opaque to the OSS daemon; plugins are responsible for defining membership and access control.

Scope is a first-class field on every artifact and on every event. It drives where the artifact materializes natively, which routing rules apply, and how migration handles the artifact across devices that may have different projects cloned locally.

#### 4.13.1 Scope field shape

```jsonc
{
  "id": "...",
  "type": "memory",
  "scope": {
    "kind": "project",
    "project": {
      "id": "github.com/example/sample-repo",
      "path": "/Users/example/work/sample-repo",
      "displayName": "sample-repo",
      "vcs": "git",
      "ephemeral": false
    }
  },
  ...
}
```

For `kind: "global"` the inner objects are omitted. For `kind: "namespace"`, a single `"namespace": "<id>"` field replaces `project`.

- **`project.id`** is the canonical project identifier. For git repos, this is the normalized remote URL (lowercased host, no `.git` suffix, no protocol). Aplexica derives this on first detection and records it; the user can override.
- **`project.path`** is the local-device working directory. **It may differ per device** — the same git repo can be at `/Users/example/work/repo` on the laptop and `/home/example/code/repo` on the desktop. The path is a hint, not part of the canonical identity.
- **`project.vcs`** is `git`, `hg`, or `none`. Non-VCS projects get a stable path-derived ID (`local:<hash>:<dirname>`) and `vcs: "none"`.
- **`project.ephemeral: true`** marks ad-hoc directories that shouldn't sync — see §4.13.5.

#### 4.13.2 Native location mapping per agent

Each adapter declares how scope maps to native storage:

| Agent | Global storage | Project storage |
|---|---|---|
| **Claude Code (CLI + Desktop)** | `~/.claude/` (memory and skills) plus `~/.claude.json` (user MCP); app session catalog is read-only | `./CLAUDE.md`, `./AGENTS.md`, `./.claude/skills/`, `./.mcp.json`; memory and skills receive guarded mirrors in default active Desktop worktrees |
| **Codex (CLI + Desktop)** | `~/.codex/` plus `$HOME/.agents/skills/` | `./AGENTS.md`, `./.agents/skills/`, `./.codex/config.toml` |
| **Hermes** | `~/.hermes/` (typical) | `./.hermes/`, project-local config |
| **OpenClaw** | `~/.openclaw/` (typical) | project-local config |
| **Kilo** | Kilo's user-level workspace | project-level rules and modes |

Adapters scan **both** locations during `discover()` and tag artifacts inbound with the correct scope.

#### 4.13.3 Project identity strategy

Project ID derivation, in priority order:

1. **Git remote (origin) URL**, normalized: `github.com/owner/repo`, `gitlab.com/owner/repo`, `bitbucket.org/owner/repo`, self-hosted GitLab/Gitea hosts as full normalized URL. This is the canonical case.
2. **Other VCS remote** (mercurial, etc.) when present.
3. **Stable path-derived ID** for non-VCS projects: `local:<sha256(absolute-path)[:6]>:<dirname>`. Visible to the user, renameable via `aplexica project rename <id> <new-display-name>` (rename only affects the display name, not the ID).
4. **Manual override**: the user can declare a project ID for any directory via `aplexica project init <name>`. Useful when projects span multiple repos or are organized in non-standard ways.

Two devices opening the same git repo at different paths produce the same project ID and Aplexica reconciles automatically. Two devices opening different repos at the same path produce different project IDs.

#### 4.13.4 Cross-device project materialization — "stage and wait"

When a project-scoped artifact arrives on a device where the project repo is **not currently present locally**, the default behavior is **stage and wait**:

- The artifact arrives in the canonical store with its scope intact.
- It is NOT materialized to any agent's native storage yet.
- It appears in `aplexica status` under a "Pending projects" section with an indicator like:
  ```
  Pending projects (artifacts staged, not materialized):
    github.com/example/sample-repo      12 artifacts (3 memories, 4 skills, 1 tool, 4 conversations)
        — clone the repo locally and run `aplexica project link github.com/example/sample-repo <path>`
  ```
- When the user clones the repo locally, the daemon detects the git remote on its next scan (or via `aplexica project link`) and materializes the pending artifacts automatically.
- If the user never clones the project, the artifacts remain staged indefinitely. They consume canonical-store space (subject to disk-quota policy) but never appear in any agent.

This default is safest: no surprise files appear in unexpected places on a new device.

#### 4.13.5 Ad-hoc directories default to global scope

When the agent is invoked in a directory that is not a git repository AND has no recognized non-VCS project marker (no `aplexica project init` previously run) AND no parent directory contains a recognized project, any state produced is **tagged as global scope** rather than as a new project.

Rationale: users often invoke agents in `~/scratch/`, `/tmp/play/`, `Downloads`, or temporary one-off directories. Treating each as its own project would pollute the canonical store with hundreds of throwaway "projects" that the user never wanted to track. Global is the safe default; the user can promote to project scope explicitly when they want to.

A user who wants per-directory tracking even for ad-hoc work can run `aplexica project init --ephemeral` in the directory, which creates a project with `ephemeral: true` set. Default sync rules (see [05-brd-selective-sync-and-routing.md](05-brd-selective-sync-and-routing.md)) exclude `ephemeral: true` projects from cross-device sync.

#### 4.13.6 Functional requirements

- **FR-02.35** Every artifact MUST have a `scope` field. Absent values are treated as `scope: { "kind": "global" }` for backward compatibility per the additive-only schema rule (§4.10).
- **FR-02.36** Adapters MUST scan both global and project storage locations during `discover()` and `scan()`. Inbound translation MUST tag each artifact with the correct scope.
- **FR-02.37** Adapters MUST detect git remote URLs (and other supported VCS remotes) to determine canonical project IDs. Adapters MUST fall back to path-derived IDs for non-VCS projects.
- **FR-02.38** Cross-device project materialization MUST default to "stage and wait": project-scoped artifacts for projects not present locally arrive in the canonical store but are not materialized natively until the project is detected.
- **FR-02.39** When a previously-pending project is detected locally (via clone, `aplexica project link`, or filesystem scan), the daemon MUST materialize the pending artifacts to that project's native files automatically within one event cycle.
- **FR-02.40** Ad-hoc directories with no detectable project identity MUST default to global scope. Per-directory tracking requires explicit `aplexica project init`.
- **FR-02.41** The CLI MUST provide: `aplexica project list`, `aplexica project show <id>`, `aplexica project init [--ephemeral]`, `aplexica project link <id> <path>`, `aplexica project rename <id> <display-name>`, `aplexica project unlink <id>`, `aplexica project materialize <id>` (force-materialize pending artifacts for a known project).
- **FR-02.42** Every event MUST carry the scope of the artifact it belongs to. Cross-scope event references (e.g., a fork that crosses from project A to global) are not permitted in V1; they produce an explicit error.

## 5. Adapter API

An adapter is a Go package loaded by the `aplexica` daemon. In V1, first-party adapters are compiled into the main binary. Extension boundaries are versioned so the adapter API can evolve without silently breaking existing integrations.

### 5.1 Lifecycle

```
daemon startup
  → adapter.init({ store, config })
  → adapter.discover()                          // is this agent installed? where?
  → adapter.watch(sourcePaths, onChange)        // begin observing native storage
  → adapter.scan()                              // initial reconciliation
  → … runtime …
  → adapter.shutdown()                          // graceful stop
```

### 5.2 Required methods

| Method | Purpose |
|---|---|
| `init(ctx)` | Initialization. Receives `store` accessor and `config` (user settings + capabilities). |
| `discover()` | Detects whether and where the agent is installed on this machine. Returns paths or null. |
| `scan()` | Walks native storage and emits inbound events for every artifact found. Used at startup and on `aplexica resync`. |
| `watch(paths, onChange)` | Registers a filesystem watcher. Calls `onChange(event)` when native storage changes. |
| `toAcf(nativeArtifact) → AcfArtifact` | Translates one native artifact into ACF. |
| `fromAcf(acfArtifact) → NativeArtifact` | Translates one ACF artifact into native form. Returns a fidelity report. |
| `applyOutbound(acfEvent) → ApplyResult` | Materializes a single ACF event into the agent's native storage. |
| `compatibilityMatrix()` | Returns this adapter's declared capabilities (which tools, which content kinds, which event types). |
| `shutdown()` | Releases watchers, flushes pending work. |

### 5.3 Adapter contract

- **Idempotency.** Applying the same outbound event twice MUST be a no-op.
- **Read consistency.** When reading native files that may be open in the agent, adapters MUST use platform-appropriate stable-read strategies (copy-then-read on Windows where exclusive locks are common, snapshot-read where supported, retry on transient EBUSY).
- **Bounded recursion guard.** Outbound writes MUST be tagged so the adapter's own watcher doesn't observe them as a fresh native change and trigger an inbound loop.
- **Fidelity report.** Every `fromAcf` and `applyOutbound` call returns a structured report listing any lossy mappings. The daemon aggregates these for the user.
- **Determinism.** Two calls to `toAcf` against the same native input MUST produce equal ACF artifacts (modulo a `lastSeenAt` timestamp).
- **No network calls.** Adapters operate exclusively on the local filesystem.

### 5.4 Adapter conformance tests

A conformance test suite is shipped with the project. Every adapter MUST pass:

1. **Round-trip.** Snapshot native storage → `toAcf` → `fromAcf` → diff against original. Differences are limited to documented non-semantic noise.
2. **Idempotency.** Apply an outbound event twice; second application is a no-op.
3. **Watch correctness.** Trigger a known sequence of native changes; the emitted event sequence MUST match a golden file.
4. **Cross-conversion.** Take a representative bundle from each of the other V1 agents; convert through this adapter; produce an apply report.
5. **Recursion guard.** Materialize an outbound event; ensure no inbound event is fired for the same write.
6. **Performance.** Initial scan of a 1 GB native storage tree MUST complete in under 30 seconds.
7. **Capability declaration.** `compatibilityMatrix()` MUST match the adapter's actual behavior — checked by running canonical tool calls through it and asserting the declared outcome.

## 6. V1 adapter scope

| Adapter | Native storage location (typical) | Special considerations |
|---|---|---|
| **Claude Code** | CLI + Desktop share `~/.claude/` and project-local state; Desktop adds an app-owned catalog and automatic worktrees | Read catalog metadata only to normalize `originCwd`; mirror project memory and skills only into validated active worktrees. Do not synthesize Desktop sidebar records. |
| **Codex** | CLI + Desktop share `~/.codex/`; skills use user/project `.agents/skills` | Register synchronized rollouts through `codex app-server`; never mutate the private thread-index SQLite schema. |
| **Hermes** | `~/.hermes/` | Per-user configuration and SQLite conversation state. |
| **OpenClaw** | `~/.openclaw/` | Per-user configuration, workspace content, and agent session stores. |
| **Kilo** | Kilo Code's configured workspace | Skill model differs (modes, custom instructions). |

Detailed per-adapter mappings (native field → ACF field tables, tool-name translation tables, capability matrices) live in *adapter spec documents* written as part of each adapter's implementation plan, not in this BRD.

## 6.1 Industry-standard portable artifacts (V1 first-class support)

Two open ecosystem formats are handled directly as first-class native artifacts. They are not merely conversion targets; reading and writing them directly avoids an unnecessary translation layer.

**AGENTS.md** — repo-level project instructions, AAIF project, adopted by 25+ tools (Claude Code, Codex, Cursor, OpenCode, Goose, Cline, Windsurf, and others). Aplexica treats AGENTS.md as a project-scoped memory artifact: every adapter MUST read it from repository roots, MUST write changes to it when materializing project-scoped memories, and MUST preserve verbatim sections the user maintains by hand.

**SKILL.md** (Agent Skills Open Standard) — open spec since December 2025, adopted by 32 tools. Aplexica treats SKILL.md as the canonical native format for the skills artifact type for every agent that supports the standard. The adapter's `fromAcf` for a skill artifact produces SKILL.md output where supported; `toAcf` parses SKILL.md inputs natively. The conversion lossiness this avoids — translating a skill through ACF and back into the same SKILL.md — is significant; preserving the standard format end-to-end is the right approach.

**MCP server exposure** — Aplexica's canonical store MUST expose an MCP server interface so MCP-aware tools can read from it. The capability is in scope for the local sync daemon — see [03-brd-local-realtime-sync.md](03-brd-local-realtime-sync.md) — and is referenced here because adapters must declare which artifacts are safe to expose through that interface.

### 6.1.1 Coexistence with third-party skill managers (closed decision 2026-05-18)

Skill-management tools like [skillkit](https://github.com/rohitg00/skillkit) (Apache 2.0, ~46 supported agents) cover the broader cross-agent skill-format-translation problem at greater breadth than Aplexica's V1 5-agent scope. We considered integrating skillkit as a runtime dependency and **decided not to** — the cost (Node.js runtime in our distribution, external version coupling, architectural muddiness against the filesystem-native story) exceeds the benefit for V1, where SKILL.md plus 5-agent shims handle the dominant case.

Coexistence policy:

- **FR-02.21** When skillkit (or a similar third-party skill manager) is detected on the user's machine, Aplexica's skill adapter MUST NOT modify files that skillkit owns or stomp on its config. Detection is by well-known binary names on `PATH` plus standard config-dir presence; the daemon logs the detection and surfaces it in `aplexica status` as an informational line.
- **FR-02.22** Aplexica MUST NOT depend on skillkit (or any other Node.js or other-language-runtime tool) at runtime. The daemon is a single static binary plus its adapter ring; third-party tools are coexistence partners, not dependencies.

Broader native skill-mapping coverage may be added through the normal public contribution process. Any reused implementation must retain the notices required by its license.

## 7. Functional requirements

- **FR-02.1** ACF v1.0 schema MUST be finalized, JSON-Schema-published, and frozen before any adapter ships.
- **FR-02.2** Every V1 adapter MUST pass every conformance test in section 5.4.
- **FR-02.3** Every V1 adapter MUST publish a per-agent spec document describing native → ACF mappings, tool translations, and known fidelity gaps.
- **FR-02.4** The canonical tool taxonomy (read/write/edit/glob/grep/bash/web_fetch/web_search/image) MUST be defined with a JSON Schema for each canonical tool's input.
- **FR-02.5** The CLI MUST provide `aplexica adapters list` showing each logical adapter, its version, its detection status (installed / not installed / overridden via env), its user-facing surfaces (for example `cli`, `desktop`), and its declared capabilities.
- **FR-02.6** The CLI MUST provide `aplexica adapters check <name>` to run the conformance suite against a local installation.
- **FR-02.7** Adapters MUST emit a structured fidelity report (Markdown + JSON) every time a lossy conversion occurs and surface a one-line summary to the user.
- **FR-02.8** The store MUST be re-readable by a future Aplexica version that supports a newer ACF schema (forward compatibility via version-explicit migrations).
- **FR-02.9** Every V1 adapter MUST read **AGENTS.md** files from repository roots in the agent's project scope, MUST round-trip them through ACF without lossy conversion, and MUST write changes back to AGENTS.md when materializing project-scoped memory artifacts. Verbatim sections (sections the user maintains by hand) MUST be preserved across round-trips.
- **FR-02.10** Every V1 adapter MUST read **SKILL.md** files (per the Agent Skills Open Standard) as the canonical native format for skill artifacts where the agent supports the standard. `fromAcf` for a skill artifact MUST produce SKILL.md output where supported. SKILL.md → ACF → SKILL.md MUST be byte-identical for skills that use only standard-spec features.
- **FR-02.11** The canonical store MUST expose an MCP server interface readable by any MCP client, with artifacts visible per the user's selective-sync rules (see [05-brd-selective-sync-and-routing.md](05-brd-selective-sync-and-routing.md)). This is the integration point for MCP-aware tools that are not first-class Aplexica adapters.
- **FR-02.12** The conformance suite MUST include AGENTS.md round-trip tests and SKILL.md round-trip tests for every adapter that declares support for the respective standard.

### 7.1 Tool artifact requirements

- **FR-02.13** Every V1 adapter MUST discover, read, and round-trip **MCP server configurations** as `tool` artifacts of kind `mcp-server`. MCP-server round-trip MUST be byte-identical for the config portion (the `command`, `args`, `env`, and any agent-specific extension fields) modulo documented non-semantic noise (e.g., key ordering in JSON).
- **FR-02.14** Each V1 adapter MUST document, in its per-agent spec, which other tool kinds it supports natively: `subagent`, `hook`, `slash-command`, `plugin`. Unsupported kinds MUST be declared in `compatibilityMatrix()`. Materializing an unsupported kind into that agent produces an annotated-document fallback and a fidelity-report entry.
- **FR-02.15** Tool artifacts MUST reference secrets by named placeholders (`${secret:<name>}`) in `config.body`. Adapters MUST NOT extract or store raw secret values into the canonical store regardless of how they appear in native config files. When inbound translation encounters an inline secret in the native file, the adapter MUST detect it (using a regex catalog of known secret patterns plus heuristics on env-var-like keys), externalize it to `~/.aplexica/secrets/<name>`, rewrite the tool artifact to use the placeholder, and emit a structured warning.
- **FR-02.16** The default value of `syncSecrets` on every tool artifact MUST be `false`. Users opt in per tool via `aplexica tool sync-secrets <id> --enable`. Opting in for a tool implies opting in for every secret in that tool's `secretsRefs`.
- **FR-02.17** When `syncSecrets` flips from `true` to `false`, the daemon MUST remove the secret value from the canonical store within one event cycle and mark the affected secret name's `syncEnabled` field as `false`. Past events that carried the secret remain in the log; this is forward-only erasure (documented in [09-security-and-trust-model.md](09-security-and-trust-model.md)).
- **FR-02.18** The CLI MUST provide `aplexica tool list`, `aplexica tool show <id>`, `aplexica tool sync-secrets <id> --enable|--disable`, `aplexica tool capabilities <id>`. The CLI MUST also provide `aplexica secret list`, `aplexica secret set <name>`, `aplexica secret get <name>`, `aplexica secret delete <name>`, `aplexica secret rotate <name>`.
- **FR-02.19** Routing rules MUST be able to match on `match.type = "tool"` and on `match.toolKind = "mcp-server"` (or other kinds). See [05-brd-selective-sync-and-routing.md](05-brd-selective-sync-and-routing.md).
- **FR-02.20** Outbound materialization of a tool artifact into an agent's native storage MUST NOT include the actual secret values when `syncSecrets` is `false`. Adapters MUST either (a) reference an environment variable the user is expected to set locally, or (b) emit a stub config with a clear comment marking the secret as missing and a one-line `aplexica` command the user can run to set it.

## 8. Non-functional requirements

| ID | Requirement |
|---|---|
| **NFR-02.1** | Adapter discovery for all five V1 agents on a typical developer machine completes in under 1 second total. |
| **NFR-02.2** | Initial scan throughput per adapter: at least 50 MB/s on a 2023-vintage laptop SSD. |
| **NFR-02.3** | Watching a directory of 10,000 artifacts adds less than 50 MB resident memory per adapter. |
| **NFR-02.4** | ACF artifacts (JSON and JSONL) MUST be readable with standard tooling (`jq`, `grep`, `git diff`). No binary formats inside the store. |
| **NFR-02.5** | The full ACF JSON Schema for all artifact types and event types MUST fit in a single 200 KB file shipped with the release. |

## 9. Out of scope

- A canonical tool taxonomy that covers every conceivable tool. V1 covers the common nine and leaves everything else as verbatim-only.
- Conversation summarization or compression. ACF stores conversation history as-recorded; lossy summarization is a separate feature for a possible V2.
- Cross-agent skill *execution*. Aplexica does not run skills; it stores and routes them. Whether a skill runs successfully in the target agent is the target agent's responsibility.
- Third-party adapter SDK and developer documentation. V1 supports first-party adapters only.

## 10. Acceptance criteria

V1 is feature-complete for this BRD when:

1. The ACF JSON Schema is published and frozen at v1.0.
2. All five V1 adapters pass the conformance suite on macOS, Linux, and Windows.
3. Each V1 adapter has a published spec document.
4. The canonical tool taxonomy is documented and used by all five adapters.
5. A power user can `cat` any artifact file in the canonical store and read it without tooling.

## 11. Open questions

All open questions for this BRD were resolved on 2026-05-18:

- ~~**OQ-02.1** MCP semantics in canonical tool taxonomy~~ — **Decided: use a small canonical taxonomy for V1.** MCP-specific tool calls preserve their verbatim representation when no canonical mapping exists.
- ~~**OQ-02.2** Binary attachments — base64 vs blob store~~ — **Decided: inline base64**, with configurable local storage caps. See §4.9. The versioned format leaves room for content-addressable storage later.
- ~~**OQ-02.3** Migration policy when ACF v2 ships~~ — **Decided: V1 and V2 MUST be bidirectionally backward-compatible** via additive-only V2 changes. Breaking changes are V3. No explicit migration tool needed for V1↔V2. See §4.10.
- ~~**OQ-02.4** Agent version tracking in event provenance~~ — **Decided: required.** Every adapter populates `createdBy.agentVersion`, `createdBy.agentBuild`, and `createdBy.adapterVersion`. See §4.11.

## 12. Dependencies

- This BRD is a prerequisite for [01-brd-backup-restore.md](01-brd-backup-restore.md), [03-brd-local-realtime-sync.md](03-brd-local-realtime-sync.md), [04-brd-forking-and-merging.md](04-brd-forking-and-merging.md), and [05-brd-selective-sync-and-routing.md](05-brd-selective-sync-and-routing.md). Those documents assume ACF and the adapter API as described here.
