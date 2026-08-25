# BRD-04 — Forking and Merging

**Document type:** Business Requirements Document
**Status:** Draft v1
**Last updated:** 2026-05-17
**Edition:** OSS (no subscription required)
**Maintainer:** Aplexica project

---

> This BRD records requirements and design intent. For currently shipped
> command syntax, use the [user guide](user-guide.md) and `aplexica help`.

## 1. Problem

The core multi-agent workflow Aplexica enables is: take a conversation that started in one agent and continue it in a different agent. Two variants matter:

1. **Continue in place.** The user wants the conversation history to flow across agents — they were chatting with Claude Code about a refactor, now they want Codex to pick up exactly where Claude Code left off. Both agents see the same history going forward.
2. **Branch off.** The user wants to try the same conversation a different way in a second agent without disturbing the original. The conversation in Agent A keeps going on its own; a new branch in Agent B explores a different path from the same fork point.

These are the same operation underneath: a conversation is an append-only history with named branches. "Continue in place" is `main` extended by another agent. "Branch off" is a new branch starting from a chosen event. Both require careful semantics around what "branch" means, how branches diverge, and how (and whether) they merge back.

The same model handles **conflict resolution**: when two agents (or two devices via a transport plugin) modify the same artifact concurrently, the daemon doesn't pick a winner — it puts the divergent change on a new branch and surfaces the conflict to the user.

## 2. Users and use cases

| Use case | Scenario |
|---|---|
| **Mid-conversation handoff** | "This is getting complicated; let me switch to Codex." Conversation continues on `main`, now under Codex. |
| **Compare two agents on the same prompt** | At turn 12, fork into Codex (`turn-12-codex`) and keep going in Claude Code (`main`). Compare results. |
| **Try a risky direction safely** | "Let me see what happens if I tell it to rewrite the schema." Fork a `schema-rewrite` branch, attempt, abandon if bad. |
| **Resolve a sync conflict** | The user accidentally edited the same memory in two agents at once. Daemon creates a conflict branch; user reviews and merges or rejects. |
| **Restore a prior state without losing the present** | "What was my user-profile memory three days ago?" Branch from that point, materialize into one agent, compare. |

## 3. Scope

In scope:
- Branch model for conversations (V1 forking applies primarily to conversations; memories and skills get a simpler conflict-resolution model since they don't have ordered turns).
- The `aplexica fork`, `aplexica merge`, `aplexica branch`, `aplexica checkout`, `aplexica log`, `aplexica diff`, and `aplexica resolve` CLI commands.
- The semantics of fork events, merge events, and concurrent-write conflict events in the ACF event log.
- Conflict UX: how the user becomes aware of, inspects, and resolves a conflict.
- Per-agent materialization pointers: which branch each agent currently sees.

Out of scope:
- Routing — which agents *should* see which branches — see [05-brd-selective-sync-and-routing.md](05-brd-selective-sync-and-routing.md).
- The wire-level cross-device merge protocol. The same branch model applies when events arrive via a transport plugin; only the transport layer differs.

## 4. The branch model

### 4.1 Conversations

Every conversation has at least one branch, named `main`. Branches are named pointers into the event log. Each event records which branch it belongs to. Branches share history up to a fork point; they diverge afterward.

```
main:         E0 ── E1 ── E2 ── E3 ── E4 ── E5
                              \
zod-rewrite:                    E3a ── E4a ── E5a
```

Above, `zod-rewrite` was forked at `E3`. `E3` is the last shared event. Continuing on `main` adds events to `main`; continuing on `zod-rewrite` adds events to `zod-rewrite`.

Materialization is per-agent: each adapter knows which branch is currently "checked out" in its native storage. So Claude Code can be on `main` while Codex is on `zod-rewrite`, and both see consistent (but different) histories.

### 4.2 Memories and skills

Memories and skills don't have ordered turns; "branching" makes less sense. V1 model:

- The artifact has a single linear history (a chain of `update` events).
- Conflicts are detected at write-time. If two writes arrive that both update from the same parent (concurrent updates), the daemon stores both as **sibling events** and marks the artifact as "in conflict." The user resolves by choosing one, the other, or producing a merge in `aplexica resolve`.
- A resolved memory becomes a normal linear history again. The unmerged sibling is kept in the log for audit; it is not deleted.

This is intentionally simpler than the conversation branch model — memories and skills are smaller, less ordered, and rarely benefit from long-lived divergent branches.

### 4.3 Branch lifecycle

- **Created** by an explicit `aplexica fork` or by the daemon when a sync conflict requires it.
- **Active** while any agent has it checked out.
- **Stale** if no event has been added for a configurable interval (default 90 days; see [10-non-functional-requirements.md §11](10-non-functional-requirements.md) for the config-layering rule).
- **Archived** by `aplexica branch archive <name>` — history retained, no longer offered in default branch lists, no agent allowed to check it out without an `--include-archived` flag.
- **Merged** — declared merged into another branch; no further events appended.

#### 4.3.1 Automatic archival of stale branches (decided 2026-05-18, was OQ-04.2)

The daemon's retention engine (see [03-brd-local-realtime-sync.md §4.8](03-brd-local-realtime-sync.md)) runs a daily lifecycle pass that automatically transitions branches from `stale` to `archived` after the configured staleness threshold (`branches.auto_archive_after_days`, default **90 days**).

- Automatic archival writes a system-generated `archive` annotation to the branch metadata; it does **not** delete any events.
- Archived branches do not appear in default `aplexica branch list` output; `--include-archived` shows them.
- Archived branches cannot be checked out without explicit `--include-archived`. Active sync to them stops.
- Users can extend the staleness window per-artifact or per-branch by adding the `do-not-archive` tag or by pinning the artifact.
- Users can revive an archived branch with `aplexica branch unarchive <branch>`.
- The 90-day default is config-driven; users can shorten or extend it. The branches.auto_archive_after_days value is one of the parameters required to live in `defaults.toml` per the golden rule.

## 5. Operations

### 5.1 `aplexica fork`

```
aplexica fork <artifactId> --from <eventId> --to-agent <name> [--branch <name>]
```

- Creates a new branch starting at the chosen event.
- Records a `fork` event in the new branch's log referencing the parent event and the originating agent.
- Updates the target agent's materialization pointer to the new branch.
- The source agent's materialization is unchanged.
- If `--branch` is omitted, a name is derived: `<short-event-id>-<target-agent>`.

After fork, the two agents can be used independently. Sync rules determine whether any later events on the new branch are visible to the source agent (default: no).

### 5.2 `aplexica branch`

```
aplexica branch list <artifactId>
aplexica branch create <artifactId> <name> --from <eventId>
aplexica branch rename <artifactId> <old> <new>
aplexica branch archive <artifactId> <name>
aplexica branch delete <artifactId> <name>          # only allowed if branch was archived first
```

### 5.3 `aplexica checkout`

```
aplexica checkout <artifactId> --branch <name> --agent <agentName>
```

- Materializes the named branch into the named agent's native storage.
- The agent's process MUST be stopped first (or auto-stopped/restarted) if its native format does not tolerate live overwrites; this is per-adapter behavior.

### 5.4 `aplexica merge`

```
aplexica merge <artifactId> --from <branch> --into <branch> [--strategy <name>]
```

- Combines two branches.
- For conversations, V1 supports the `fast-forward` strategy (only allowed when the `--into` branch is a strict prefix of `--from`) and the `manual` strategy (the user provides a merge plan via an interactive resolver).
- For memories and skills, V1 supports `ours`, `theirs`, and `manual` strategies.
- A successful merge produces a `merge` event in the destination branch with a payload that records both parent events.
- A failed or unconfirmed merge does NOT modify either branch.

V1 does **not** ship automatic semantic merge of conversation content. The interactive resolver shows the diverging segments side-by-side and lets the user pick which events to keep, which to drop, and which to rewrite. The resulting merge is reviewed before being committed.

#### 5.4.1 N-way merges with three or more branches (decided 2026-05-18, was OQ-04.3)

When more than two branches need to be reconciled — typically when multiple devices have diverged (Device A on branch `main`, Device B on `branch-b`, Device C on `branch-c`, all unmerged) — V1's policy is:

**The user MUST explicitly select which branch becomes the main (destination) branch.** All other branches are then merged into that selected main, sequentially, with the standard merge resolver applied between each pair.

Concrete UX:

```
$ aplexica merge --artifact <id>
Detected 3 diverging branches:
  main          (head: a1b2..., last event 2 days ago, 12 events ahead)
  branch-b      (head: c3d4..., last event 1 day ago, 8 events ahead)
  branch-c      (head: e5f6..., last event 3 days ago, 5 events ahead)

Which branch should become the destination?
  > main
    branch-b
    branch-c

Selected destination: main. Will merge branch-b and branch-c into main in that order.
Continue? [y/N]
```

After the user selects, the resolver runs merge-from-`branch-b`-into-`main`, then merge-from-`branch-c`-into-`main`. Each pairwise merge gets the standard manual resolver UI. Two `merge` events are recorded in the destination, each referencing the appropriate parent.

Rationale: synthesizing a true N-way merge across heterogeneous branches is the kind of automatic-semantic-merge work V1 explicitly excludes. Selecting a destination and then doing N–1 pairwise merges is conceptually simple, gives the user clear control, and produces a clean merge-event chain that the audit log and `aplexica log --graph` render legibly.

The user can choose a different sequencing order by running the pairwise merges manually (`aplexica merge --from branch-c --into main` first, then `--from branch-b --into main`). N-way mode is the safe-default convenience flow.

### 5.5 `aplexica log` and `aplexica diff`

```
aplexica log <artifactId> [--branch <name>] [--graph] [--event-tag <tag>]
aplexica diff <artifactId> --branch <a> --to <b>
aplexica diff <artifactId> --event <e> --to <e2>
```

- `log` prints branches and events in a git-log-like format. `--graph` draws ASCII branch topology. `--event-tag <tag>` filters to events bearing the named tag (see §5.8).
- `diff` highlights divergent content. For conversations, diff is event-by-event with content shown.

### 5.6 `aplexica resolve`

```
aplexica resolve <artifactId>
```

- Interactive resolver for an artifact in conflict.
- Lists divergent branches/siblings with previews.
- Lets the user pick a resolution (one side, the other, or a merge).
- Records the resolution as a `merge` (conversations) or `resolution` (memories/skills) event.

### 5.7 Daemon-initiated conflict branches

When the daemon detects concurrent local writes that produce divergence:

1. It accepts both writes — neither is discarded.
2. It creates a branch (or, for memories/skills, sibling events).
3. It emits a desktop notification (when notifications are available) and writes a record to `~/.aplexica/conflicts/<artifactId>.json`.
4. It marks the affected artifact as "in conflict" in `aplexica status`.
5. Both agents that originated the conflicting writes continue using their respective versions until the user resolves.

### 5.8 Event tags (decided 2026-05-18, was OQ-04.4)

Events MAY be tagged for filtering, navigation, and annotation. Distinct from artifact tags (which apply to whole artifacts and drive sync rules). Event tags are per-event; they help users navigate long histories ("show me every event tagged `decision-point` in this conversation") and serve as bookmarks for things worth remembering. See [02-brd-format-adapters.md §4.5](02-brd-format-adapters.md) for the schema.

CLI:

```
aplexica event tag <eventId> add <tag>           # tag an event
aplexica event tag <eventId> remove <tag>        # untag
aplexica event tag <eventId> list                # show all tags on the event
aplexica event tag list-all <artifactId>        # all event tags used in this artifact's history
aplexica log <artifactId> --event-tag <tag>     # filter log by event tag
```

Tags applied by the user are stored verbatim. Tags applied by Aplexica system events (`auto:archived`, `aplexica:merge-conflict-marker`, etc.) use reserved namespaces the user cannot write to.

Event tags propagate through real-time sync and round-trip through bundle export/import alongside the events they belong to.

## 6. Event semantics

### 6.1 `fork`

```jsonc
{
  "eventId": "…", "hash": "…", "parentHash": "<parent-event-on-source>",
  "branch": "<new-branch-name>",
  "type": "fork",
  "from": "<eventId-on-source-branch>",
  "originAgent": "<target-agent-name>",
  "rationale": "<optional user-supplied note>",
  "at": "2026-05-17T20:00:00Z",
  "by": { "device": "…", "user": "…" }
}
```

### 6.2 `merge`

```jsonc
{
  "eventId": "…", "hash": "…", "parentHash": "<head-of-into-branch>",
  "branch": "<into-branch>",
  "type": "merge",
  "from": "<head-of-from-branch>",
  "strategy": "fast-forward" | "manual" | "ours" | "theirs",
  "resolutionNotes": "<free text>",
  "at": "…", "by": { … }
}
```

### 6.3 `conflict` (daemon-emitted marker, not in event log itself)

A conflict is not an event in the event log; it is a state file at `~/.aplexica/conflicts/<artifactId>.json` that lists the divergent heads and offers context. Resolving it via `aplexica resolve` writes a real `merge` or `resolution` event.

## 7. Functional requirements

- **FR-04.1** Every conversation MUST have an explicit branch on every event. There is no implicit "current branch" in the event log.
- **FR-04.2** Forks MUST be O(1) — they create a single event referencing the fork point, not a copy of prior history.
- **FR-04.3** The CLI MUST provide `fork`, `branch`, `checkout`, `merge`, `log`, `diff`, `resolve` with the signatures in section 5.
- **FR-04.4** `aplexica log --graph` MUST render a readable branch topology on a 100-column terminal for conversations with up to 50 branches.
- **FR-04.5** `aplexica checkout` MUST verify with the target adapter that the agent process is in a safe state before overwriting, and refuse (or offer to restart the agent) if not.
- **FR-04.6** A merged branch's events MUST remain queryable via `log` and `diff` indefinitely; merge does not delete history.
- **FR-04.7** The daemon MUST emit a desktop notification (where the platform supports it) within 10 seconds of detecting a sync conflict.
- **FR-04.8** The CLI MUST present conflict counts in `aplexica status` and never let a conflict be silently forgotten.
- **FR-04.9** Branch names MUST be lower-case, hyphen-delimited identifiers with a max length of 64 characters; the CLI MUST normalize user input.
- **FR-04.10** All branch operations MUST be journaled to `~/.aplexica/logs/branch-ops.jsonl` for forensics and audit.
- **FR-04.11** Forking a conversation that is currently being synced to multiple agents under a sync rule MUST automatically update the target agent's materialization pointer to the new branch; the source agent's pointer is unaffected.
- **FR-04.12** Materializing a branch into an agent MUST be atomic from the agent's perspective — either the full branch is in place or the prior state is.
- **FR-04.13** Continuing a conversation in a second agent MUST be explicit fork only — there is no scratch/auto-fork mode in V1. The user runs `aplexica fork` to create a new branch; otherwise the conversation stays on `main` and the second agent shares the original history.
- **FR-04.14** The daemon MUST run a daily lifecycle pass that auto-archives branches whose most recent event is older than `branches.auto_archive_after_days` (default 90 days). Archived branches do not appear in default `branch list`; `--include-archived` reveals them. Archival writes a system annotation; it does NOT delete events.
- **FR-04.15** Users MUST be able to extend the staleness window per-branch via the `do-not-archive` tag, or per-artifact by pinning. `aplexica branch unarchive <branch>` reverses an archival.
- **FR-04.16** When a merge involves more than two branches, the resolver MUST prompt the user to select which branch becomes the destination ("main") and MUST then perform N–1 pairwise merges from each other branch into the destination, applying the standard manual resolver per pair. The user MAY override by running the pairwise merges manually in a different order.
- **FR-04.17** Events MAY carry an optional `tags: ["..."]` array. The CLI MUST provide `aplexica event tag <eventId> add/remove/list` and `aplexica log --event-tag <tag>` filtering. System-reserved namespaces (`aplexica:*`, `auto:*`) MUST be rejected by user-facing tag commands.
- **FR-04.18** Event tags MUST propagate through real-time sync and round-trip through bundle export/import alongside the events they belong to. Adapters that materialize into agents lacking a native annotation MUST preserve event tags as adapter-specific sidecar metadata.

## 8. Non-functional requirements

| ID | Requirement |
|---|---|
| **NFR-04.1** | `fork` MUST complete in under 500 ms regardless of conversation length. |
| **NFR-04.2** | `log` for a conversation with 5,000 events MUST render in under 2 seconds. |
| **NFR-04.3** | `merge` interactive resolver MUST present a paginated diff that remains responsive for branches with up to 1,000 diverging events. |
| **NFR-04.4** | The event log file format MUST remain `grep`-friendly and `jq`-friendly. |

## 9. Out of scope

- Automatic semantic merge of conversation content (i.e., having an LLM produce a "best of both branches" version). Tempting and obvious but out of scope for V1.
- Visual branch UI (GitKraken-style). V1 ships CLI only; a GUI is V2+.
- Cross-artifact merge ("merge memory X from branch A into branch B of conversation Y"). Each artifact's branches are independent.
- Branch protection rules (allow-list of who can write to which branch). Relevant in team contexts and handled by access-control plugins; out of scope for V1 OSS.

## 10. Acceptance criteria

V1 of forking and merging is complete when:

1. A developer can fork a conversation from Claude Code into Codex with one command and continue in Codex without disturbing the Claude Code copy.
2. After working independently in both agents for ten more turns each, `aplexica log --graph` correctly renders the two divergent branches.
3. `aplexica merge --strategy manual` produces a usable interactive resolver that lets the developer pick events from each branch.
4. A deliberate concurrent edit of the same memory file from two agents produces a sibling-event conflict, a desktop notification, and a working `aplexica resolve` flow.
5. A merged branch is still inspectable via `log` and `diff`.
6. Conflict counts in `aplexica status` are accurate after a forced soak test that produces 50 conflicts.

## 11. Open questions

All open questions for this BRD were resolved on 2026-05-18:

- ~~**OQ-04.1** "Scratch" auto-fork mode~~ — **Decided: explicit fork only.** V1 has no auto-fork mode; users run `aplexica fork` to create a new branch. See FR-04.13.
- ~~**OQ-04.2** Branch auto-archival policy~~ — **Decided: auto-archive after 90 days of inactivity.** Configurable via `branches.auto_archive_after_days` (the value is in `defaults.toml` per the golden rule). See §4.3.1 and FR-04.14.
- ~~**OQ-04.3** UX for 3+-way merges~~ — **Decided: user selects the destination branch; N-1 pairwise merges follow.** See §5.4.1 and FR-04.16.
- ~~**OQ-04.4** Event tags~~ — **Decided: yes, add them.** Optional `tags` array on events, with CLI for tagging and filtering. See §5.8 and FR-04.17 through FR-04.18.

## 12. Dependencies

- ACF schema (event types and branch model) — see [02-brd-format-adapters.md](02-brd-format-adapters.md).
- Daemon (originates conflict notifications, applies materialization) — see [03-brd-local-realtime-sync.md](03-brd-local-realtime-sync.md).
- Routing rules (which branch flows to which agent) — see [05-brd-selective-sync-and-routing.md](05-brd-selective-sync-and-routing.md).
