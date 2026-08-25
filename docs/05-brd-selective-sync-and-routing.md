# BRD-05 — Selective Sync and Routing (OSS Edition)

**Document type:** Business Requirements Document
**Status:** Draft v1
**Last updated:** 2026-05-19
**Edition:** OSS (Aplexica open-source)
**Maintainer:** Aplexica project

---

> This BRD records requirements and design intent. For currently shipped
> command syntax, use the [user guide](user-guide.md) and `aplexica help`.

## 1. Problem

By default, sync is "all artifacts to all installed agents." That is right for many users most of the time, but it is wrong often enough that it cannot be the only option:

- A memory is private to one project and shouldn't propagate to an agent the developer uses for personal projects.
- An experimental skill should run in OpenClaw only — it isn't ready for production agents.
- A long conversation about a sensitive subject should stay local and never leave the originating machine.
- A team-shared skill set should reach Claude Code and Codex, but not Hermes or Kilo where it doesn't translate cleanly.
- A scratch branch of a conversation should live only in the agent the user is experimenting with — not back-propagate to where the conversation originated.

Selective sync gives the user precise, declarative control over which artifacts go to which agents. The rules engine is transport-agnostic: it applies to any sync transport — local fan-out among agents on the same machine today, and whatever transport plugins provide tomorrow.

## 2. Users and use cases

| Use case | Rule example |
|---|---|
| **Work / personal separation** | Memories tagged `work` go only to Claude Code and Codex; memories tagged `personal` go only to Hermes. |
| **Experimental sandbox** | Skills tagged `experimental` go only to OpenClaw. |
| **Sensitive conversation lockdown** | Conversation tagged `private` stays on the originating agent and is never propagated via any transport. |
| **Project isolation** | Memories with `project:sample-repo` go only to the developer's designated work agents. |
| **Power-user routing** | "Conversations originating in Codex sync to Hermes, but not the other way around." |
| **Forking respect** | The fork created in Codex is tagged `fork-of:<source-id>` automatically; the default rule for fork branches is "stay in the originating agent." |

## 3. Scope

In scope:
- The **tag** model: how artifacts acquire tags (manual, automatic, by-rule).
- The **sync rule** expression language: matching conditions and targeting expressions.
- The **routing engine**: how the daemon resolves rules at fan-out time.
- Default rules shipped with V1.
- Rule storage and versioning.

Out of scope:
- Transport-level delivery, retry, and fan-out guarantees — these are handled by transport plugins.
- Content-aware (DLP-style) routing — see [09-security-and-trust-model.md](09-security-and-trust-model.md).

## 4. Tags

### 4.1 Tag taxonomy

A tag is a string with optional namespaces using a colon:

- `work` — a flat tag.
- `project:sample-repo` — namespaced.
- `fork-of:0193ce1a-...` — used by the system to mark forks of a specific source.
- `sensitivity:high` — namespaced; users define their own.

Reserved namespaces (system-owned, users may not write to them):
- `aplexica:*`
- `fork-of:*`
- `device:*`
- `conflict:*`

### 4.2 Tag assignment

Three ways tags appear on an artifact:

1. **Manual.** `aplexica tag add <artifactId> <tag>` and `aplexica tag remove <artifactId> <tag>`. Idempotent. Edits the artifact's tag list in the canonical store; this is itself a syncable change.
2. **Adapter-provided.** When the adapter translates a native artifact to ACF, it may set tags based on native conventions (e.g., a Claude Code memory in `~/.claude/work/CLAUDE.md` might get a `work` tag).
3. **Rule-derived.** Users may define rules that *assign* tags rather than route artifacts (see section 5.5).

### 4.3 Tag metadata

`~/.aplexica/store/tags/<tagName>.json` carries optional metadata:

```jsonc
{
  "tag": "work",
  "description": "Anything related to my job",
  "color": "#3aa1ff",
  "scope": "personal",
  "createdAt": "2026-04-01T08:00:00Z"
}
```

Metadata is informational — sync rules match on tag names, not on metadata.

## 5. Sync rules

### 5.1 Rule shape

A rule has three parts: **match**, **route**, **mode**.

```toml
[[sync.rules]]
name = "work-memories-to-work-agents"
match.kind = "any"               # "all" requires every condition; "any" requires at least one
match.tag = ["work", "project:sample-repo"]
match.type = ["memory"]
match.agentSource = ["claude-code", "codex"]

route.agents = ["claude-code", "codex"]

mode = "live"                    # "live" / "scheduled" / "manual" — transport plugins may use this
```

### 5.2 Matching

A rule matches an artifact if its match block is satisfied. Supported predicates:

| Predicate | Meaning |
|---|---|
| `match.tag` | Tags on the artifact. With `kind=all`, every listed tag must be present. With `kind=any`, at least one. |
| `match.type` | Artifact type: `memory`, `skill`, `tool`, `conversation`. |
| `match.toolKind` | For tool artifacts: tool kind. One or more of `mcp-server`, `subagent`, `hook`, `slash-command`, `plugin`. |
| `match.toolCapability` | For tool artifacts: a capability the tool declares (`network`, `shell-execute`, `filesystem-write`, etc.). Useful for restrictive routing — "don't sync any tool that can write files to the work machine." |
| `match.scope.kind` | Artifact scope kind: `global`, `project`, `namespace`. See [02-brd-format-adapters.md §4.13](02-brd-format-adapters.md). |
| `match.scope.project.id` | For project-scoped artifacts: project ID. Accepts globs (e.g., `github.com/work-org/*`). |
| `match.scope.project.ephemeral` | For project-scoped artifacts: match on `ephemeral: true` (ad-hoc projects). |
| `match.scope.namespace` | For namespace-scoped artifacts: namespace ID. |
| `match.agentSource` | The agent the artifact originated from. |
| `match.deviceSource` | The device identity the artifact originated from, when the active transport plugin provides device identity. |
| `match.size` | Size predicate, e.g., `"< 1MB"`. |
| `match.path` | A glob against `nativeRef.path` (rare; useful for path-based migrations). |
| `match.branchName` | For conversations: branch name regex. |

`match.kind` defaults to `all`. Predicates are AND'd within `all`, OR'd within `any`.

### 5.3 Routing

`route.agents` is a list of agent-name patterns the artifact may (or may not) be materialized into. Each entry is either a **positive pattern** (a bare name like `claude-code`, or the glob `*` for "all") or a **negative pattern** prefixed with `!` (e.g., `!openclaw` to deny). Evaluation: start from the implied universe (`*` if the list contains no positive patterns; otherwise the union of positive matches), then subtract anything matching a negative pattern. Examples:

- `route.agents = ["claude-code", "codex"]` — only these two agents.
- `route.agents = ["!openclaw"]` — all installed agents except OpenClaw.
- `route.agents = ["*", "!openclaw"]` — same as the above; the `*` is implied when only negatives are present.
- `route.agents = ["claude-code", "!claude-code"]` — error at config validation: contradictory.

Omitting `route.agents` entirely means "all installed agents" (i.e., implicitly `["*"]`).

**Decided 2026-05-18 (was OQ-05.1):** Negative patterns inside `route.agents` replace the previous separate `route.agentsExclude` field. Single-mechanism is cleaner; the syntax is borrowed from familiar tools like `gitignore` and `rsync`.

`route.remote = "exclude"` prevents the artifact from being propagated via any remote transport plugin, regardless of other rules. Useful for sensitive content that must never leave the local machine.

### 5.4 Mode

Rules can specify the mode under which they apply. Modes in V1: `live`, `scheduled`, `manual`. Local fan-out is effectively always `live`. The `mode` field is transport-agnostic — transport plugins may use it to schedule or gate delivery. In V1, local-only deployments may ignore `mode`.

### 5.4.1 Skill compatibility mode (decided 2026-05-18, was OQ-05.3)

When a skill artifact is routed to an agent that does not natively support the skill (per the adapter's compatibility matrix from [02-brd-format-adapters.md §4.3](02-brd-format-adapters.md)), the rule can declare how to handle the mismatch via `route.skillMode`:

```toml
[[sync.rules]]
name = "experimental-skills-strict"
match.tag = ["experimental"]
match.type = ["skill"]
route.skillMode = "strict"        # exclude unsupported entirely (default: "lossy")
```

Values:

- **`lossy`** (default) — the existing V1 behavior. When a skill cannot be expressed natively in the target agent, the adapter renders it as an annotated document (e.g., the skill's body as a Markdown note inside the agent's memory area) and emits a fidelity-report entry. The skill is preserved but degraded.
- **`strict`** — the skill is excluded from materialization in any agent that does not natively support it. No annotated-document fallback; the skill simply doesn't appear in that agent. Fidelity-report entry still emitted so the user knows what was excluded.

Rationale: `lossy` is the right default because most users prefer "some version of the skill" over "nothing." But for users who actively curate skills and don't want noisy annotated-document approximations cluttering their agents, `strict` gives clean exclusion. Common use case: experimental skills that only make sense in their native agent.

### 5.5 Tag-assigning rules

A rule may *assign* tags rather than (or in addition to) routing. This handles cases like "automatically tag memories from my work agents as `work`":

```toml
[[sync.rules]]
name = "auto-tag-work"
match.agentSource = ["claude-code", "codex"]
match.type = ["memory"]
assign.tags = ["work"]
```

A tag-assigning rule runs at ingestion time. The assigned tag is stored in the canonical artifact and persists across syncs.

### 5.6 Rule precedence

Multiple rules may match a single artifact. V1 resolution is deterministic and additive:

1. **Routing.** Each matching rule contributes positive patterns (which add to the allowed-set) and negative patterns (which remove from it). The artifact's effective allowed targets are: `(union of positive matches across all matching rules) minus (union of negative matches)`. A `route.remote="exclude"` setting in any matching rule removes remote transport targets entirely. Within a single rule, contradictions (e.g., `route.agents = ["claude-code", "!claude-code"]`) are config-validation errors. Across rules, a negative pattern in *any* matching rule wins — there is no way to "re-allow" something a different rule denied.
2. **Tags.** Tags assigned by tag-assigning rules are unioned with existing tags.
3. **Mode.** Highest-precedence mode wins where multiple rules apply, with `live > scheduled > manual`.
4. **Ties broken by rule order in the config file.** Earlier rules in the file take precedence for non-additive fields.

### 5.7 Explicit exclusion (via negative patterns)

To express "never sync this to OpenClaw," use a negative pattern:

```toml
[[sync.rules]]
name = "no-openclaw"
match.tag = ["private"]
route.agents = ["!openclaw"]
```

Negative patterns always win over positive patterns — there is no way to "re-allow" via a later rule. This is the single mechanism for exclusion; the previous separate `route.agentsExclude` field has been removed (decision 2026-05-18, was OQ-05.1).

## 6. Starter presets (opt-in; NOT shipped as always-on defaults)

> **Safe-by-default (revised 2026-05-29 — reverses the original #1 below).**
> Aplexica no longer ships ANY always-on rule. On a fresh install the
> running engine holds **zero** rules, so `Evaluate` is deny-by-default and
> the daemon fans out **nowhere** — it discovers agents, imports their
> native state read-only, and watches, but performs **no** cross-agent
> sync until the user adds a rule. The five entries below are now offered
> as **opt-in presets** the user can apply from the UI (individually or as
> a `recommended-starter-set` bundle) or hand-add to `~/.aplexica/rules.toml`.
> Applying a preset simply writes the corresponding rule into the user
> file. This deny-by-default behavior is part of the public routing contract.
>
> Rationale: an empty `route.agents` implies the whole agent universe
> (local fan-out to every installed agent), so even the "guard" rules are
> not purely restrictive — keeping ANY rule active would cause surprise
> cross-agent sync on first run. The only truly safe zero-config state is
> zero rules.

The preset catalog (each can be applied independently; `default-all-to-all`
recreates the classic zero-config "sync everything everywhere" behavior):

```toml
# 1. Fan everything out to every installed and enabled agent.
#    PRESET ONLY — this is the classic behavior, now opt-in (was the shipped default).
[[sync.rules]]
name = "default-all-to-all"
match.kind = "any"
match.type = ["memory", "skill", "tool", "conversation"]
# no route.agents → all installed agents

# 2. Forks stay where they were forked.
[[sync.rules]]
name = "fork-respects-origin"
match.tag = ["fork-of:*"]
route.agents = ["__originatingAgent__"]   # special token; resolves to the agent that did the fork

# 3. Reserved-tagged artifacts stay local — never propagated via any remote transport.
[[sync.rules]]
name = "private-stays-local"
match.tag = ["private", "secret"]
route.remote = "exclude"

# 4. Tool secret values stay local by default (the syncSecrets flag must be flipped per tool).
#    This is enforced separately by the secrets-handling layer, but a matching rule documents intent.
[[sync.rules]]
name = "tool-secrets-default-local"
match.type = ["tool"]
route.includeSecrets = false              # default; user overrides per tool via syncSecrets

# 5. Ephemeral projects (created via `aplexica project init --ephemeral` in ad-hoc dirs)
#    stay on the originating agent by default. The user can sync them by adding an explicit rule.
[[sync.rules]]
name = "ephemeral-projects-stay-local"
match.scope.kind = ["project"]
match.scope.project.ephemeral = true
route.remote = "exclude"
```

Because these are presets rather than always-on shipped rules, every rule
in the file is fully user-owned: users may apply, delete, edit, or
supplement any of them (this is what made the old shipped defaults
un-editable). The `tool-secrets-default-local` preset documents what is
also enforced independently by the security model — see
[09-security-and-trust-model.md](09-security-and-trust-model.md) §4.4 —
so even with NO rules active, tool secret *values* stay local by default;
the secrets layer is not gated on this rule.

## 7. Configuration surface

- Rules live in `~/.aplexica/config.toml` under `[[sync.rules]]` entries.
- The CLI provides `aplexica rules list`, `aplexica rules add`, `aplexica rules edit`, `aplexica rules remove`, `aplexica rules test <artifactId>` (shows which rules match and the resolved routing).
- `aplexica rules test <artifactId>` is the primary debugging surface; users get a deterministic answer to "why is this artifact going / not going to that agent?"
- Rule edits hot-reload like other config changes.

### 7.1 Retroactive rule application (decided 2026-05-18, was OQ-05.2)

By default, **new tag-assigning rules apply only to artifacts ingested AFTER the rule is added**. Existing artifacts are not re-tagged automatically. This is the safe default — re-tagging at scale would be surprising, expensive, and potentially destructive (it could trigger new sync events for every affected artifact).

To opt in to retroactive application, the user runs:

```
aplexica rules apply --retroactive [--rule <name>] [--dry-run]
```

- Without `--rule`, every tag-assigning rule is evaluated against every existing artifact.
- With `--rule <name>`, only that rule is applied retroactively.
- `--dry-run` shows what would be changed without writing anything; the output is a structured report (Markdown + JSON) listing per-artifact diffs (tags-to-add, tags-to-remove, tags-unchanged) and the cascading sync events the change would trigger.
- Running without `--dry-run` produces the same report after applying, plus the count of new sync events emitted.

The same rule applies to routing-rule changes: changing `route.agents = [...]` on an existing rule does NOT retroactively un-materialize artifacts from agents the rule no longer allows. The user has to either `aplexica rules apply --retroactive` or use the explicit orphan-cleanup flow (see FR-05.10).

## 8. Functional requirements

- **FR-05.1** Tags MUST be a first-class attribute of every artifact and MUST persist through native ↔ ACF round-trips.
- **FR-05.2** Adapters MUST be able to set tags during inbound translation; the daemon documents the per-adapter tag conventions.
- **FR-05.3** The CLI MUST provide `tag add`, `tag remove`, `tag list <artifactId>`, `tag rename`, `tag describe`.
- **FR-05.4** Tag changes MUST themselves be syncable changes (visible to other agents subject to rules).
- **FR-05.5** Sync rules MUST be defined in `config.toml` in the format in section 5.1.
- **FR-05.6** Rule evaluation MUST be deterministic and explainable. `aplexica rules test` MUST list every rule that matched and the merged routing decision.
- **FR-05.7** Routing decisions MUST be cached so a high-frequency stream of events does not re-evaluate every rule per event; the cache is invalidated on any rule or tag change.
- **FR-05.8** Default rules MUST cover the all-to-all case so a user with no custom configuration gets sane behavior.
- **FR-05.9** `route.remote = "exclude"` MUST prevent the artifact from being propagated via any remote transport plugin under any other rule.
- **FR-05.10** When a routing decision changes such that an artifact previously synced to an agent should no longer be — the daemon MUST NOT delete that artifact from the agent's native storage automatically. Instead it marks the artifact as "orphaned in <agent>" in `aplexica status` and offers an `aplexica orphans clean <agent>` command for the user to act explicitly.
- **FR-05.11** Reserved tag namespaces (`aplexica:*`, `fork-of:*`, `device:*`, `conflict:*`) MUST be rejected by user-facing tag commands.
- **FR-05.12** When an adapter is disabled or uninstalled, rules referencing it MUST still validate (a missing target is a no-op, not an error).
- **FR-05.13** Rule changes MUST be journaled to `~/.aplexica/logs/rule-changes.jsonl`.
- **FR-05.14** `route.agents` MUST accept both positive patterns (bare names; `*` as a wildcard) and negative patterns (`!name`). Config validation MUST reject contradictory patterns within a single rule (e.g., `["claude-code", "!claude-code"]`). A negative pattern in any matching rule wins over positive patterns in other rules — there is no way to "re-allow" a denied target.
- **FR-05.15** Tag-assigning rules MUST NOT apply retroactively by default. Existing artifacts MUST keep their existing tags when a new tag-assigning rule is added. Retroactive application is opt-in via `aplexica rules apply --retroactive [--rule <name>] [--dry-run]`.
- **FR-05.16** Rules MAY include `route.skillMode = "lossy" | "strict"`. `lossy` (default) renders unsupported skills as annotated documents in the target agent; `strict` excludes them entirely with a fidelity-report entry.

## 9. Non-functional requirements

| ID | Requirement |
|---|---|
| **NFR-05.1** | Rule evaluation MUST add no more than 5 ms median to the inbound pipeline for a config with 50 rules. |
| **NFR-05.2** | `aplexica rules test` MUST return in under 1 second for a config with 200 rules. |
| **NFR-05.3** | Tag list operations MUST scale to artifacts with up to 100 tags without UI degradation. |

## 10. Out of scope

- Conditional routing based on *content* (e.g., "if the memory mentions PII"). Content-inspection rules require a separate privacy and security design — see [09-security-and-trust-model.md](09-security-and-trust-model.md).
- Rule recommendation engine ("you might want to add a rule for…"). Possible V2 nice-to-have.
- Per-event routing in a conversation. V1 routes at the artifact granularity; a conversation goes (or doesn't go) as a whole.
- Time-based rules ("don't sync between 10pm and 8am"). Niche; deferred.

## 11. Acceptance criteria

V1 of selective sync is complete when:

1. A developer can add a tag-based rule that routes `work` memories to a subset of agents, and the daemon's behavior matches.
2. `aplexica rules test <id>` produces a clear, accurate explanation for any artifact.
3. The default `all-to-all` rule plus the `fork-respects-origin` rule give a no-configuration user the right behavior in both base and fork cases.
4. A `private` tag prevents an artifact from being propagated via any remote transport plugin, regardless of what transport plugins are installed.
5. Removing an agent from a rule's allow-list does not delete data from that agent's storage; it surfaces an orphan instead.
6. Rules survive daemon restarts and changes are journaled.

## 12. Resolved decisions

All open questions for this BRD were resolved on 2026-05-18:

- ~~**OQ-05.1** Negative patterns vs. dedicated Exclude fields~~ — **Decided: accept negative patterns; remove `agentsExclude`.** Single-mechanism approach borrowed from `gitignore` / `rsync`. See §5.3 and FR-05.14.
- ~~**OQ-05.2** Retroactive application of new tag-assigning rules~~ — **Decided: no retroactive re-tagging by default; opt-in via `aplexica rules apply --retroactive`.** See §7.1 and FR-05.15.
- ~~**OQ-05.3** UX for unsupported skills~~ — **Decided: add `route.skillMode = "strict"` flag** for clean exclusion; `lossy` (annotated document) remains default. See §5.4.1 and FR-05.16.
- ~~**OQ-05.4** Selective-subscribe mode for low-bandwidth scenarios~~ — **Decided: outside V1 scope.** The versioned routing model leaves room for it later.

## 13. Dependencies

- ACF artifact tag fields and event log — see [02-brd-format-adapters.md](02-brd-format-adapters.md).
- Local daemon and fan-out machinery — see [03-brd-local-realtime-sync.md](03-brd-local-realtime-sync.md).
- Security model and secrets handling — see [09-security-and-trust-model.md](09-security-and-trust-model.md).
