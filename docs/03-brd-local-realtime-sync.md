# BRD-03 — Local Real-Time Sync

**Document type:** Business Requirements Document
**Status:** Draft v1
**Last updated:** 2026-05-17
**Edition:** OSS (no subscription required)
**Maintainer:** Aplexica project

---

> This BRD records requirements and design intent. For currently shipped
> command syntax, use the [user guide](user-guide.md) and `aplexica help`.

## 1. Problem

A developer who uses two or more AI agents on the same computer pays a real cost: every time one agent learns something — a new project memory, a freshly installed skill, the latest conversation about a refactor — the other agents are now stale. The developer has two options today, both bad:

1. **Manually duplicate state**, copying memories and skills between agents and never being sure they're in sync.
2. **Accept staleness**, treating each agent as a fresh start each time, and losing the compounding value of context.

Aplexica OSS solves this on one machine: the sync daemon (`aplexica daemon`) watches each installed agent's native storage, translates changes into the canonical store, and fans them out to the other installed agents according to user-defined rules. The developer keeps using each agent normally; the daemon does the rest.

## 2. Users and use cases

| Use case | Scenario |
|---|---|
| **Multi-agent daily driver** | Developer runs Claude Code and Codex side by side. A memory written in one shows up in the other within seconds. |
| **Side-by-side comparison** | Developer asks the same question in two agents and compares answers, knowing both agents start from the same context. |
| **Skill propagation** | Developer installs a `code-review` skill in their primary agent; the other installed agents that support skills receive it automatically. |
| **Convention shift between agents** | Developer changes the project's `CLAUDE.md` from inside Claude Code; the daemon updates the equivalent state in every other installed agent that supports project-level memory. |
| **Background safety net** | The daemon writes every change to the canonical store on the way through, providing an always-current local backup independent of any single agent. |

## 3. Scope

In scope:
- The `aplexica daemon` process: lifecycle, supervision, configuration, observability.
- Filesystem watching strategy per platform (macOS, Linux, Windows).
- The inbound pipeline: native change → adapter → canonical event → store.
- The outbound pipeline: canonical event → routing → adapter → native write.
- Loop prevention (the daemon's own writes must not feed back as inbound events).
- Conflict handling between concurrent local changes (covered briefly here; deep treatment in [04-brd-forking-and-merging.md](04-brd-forking-and-merging.md)).
- Pausing, resuming, and disabling sync for specific agents or specific artifacts.

Out of scope:
- Routing rules and selective sync expression — see [05-brd-selective-sync-and-routing.md](05-brd-selective-sync-and-routing.md).
- Forking and merging mechanics — see [04-brd-forking-and-merging.md](04-brd-forking-and-merging.md).
- Cross-device sync via transport plugins — outside the scope of this doc.

## 4. Architecture

### 4.1 Process model

`aplexica daemon` is a single long-running user-level process. On each platform:

- **macOS:** installed as a `launchd` user agent (LaunchAgent), starts at login, restarts on crash.
- **Linux:** installed as a `systemd --user` service, starts at login, restarts on crash.
- **Windows:** installed as a per-user service via the Windows Service Control Manager, starts at login, restarts on crash.

The daemon runs entirely as the user — never as root or admin. It has access to the same files the user has access to and no more.

A single instance per user per machine. A lock file in `~/.aplexica/state/aplexicad.lock` prevents two instances from running concurrently against the same store.

### 4.2 Internal structure

```
                   ┌─────────────────────────────────────────┐
                   │           aplexica daemon              │
                   │                                         │
   native FS ───►  │  ┌─────────────────┐                    │
                   │  │ Adapter watcher │   inbound pipeline │
                   │  └────────┬────────┘                    │
                   │           ▼                             │
                   │  ┌─────────────────┐                    │
                   │  │  Debounce +     │                    │
                   │  │  coalesce       │                    │
                   │  └────────┬────────┘                    │
                   │           ▼                             │
                   │  ┌─────────────────┐                    │
                   │  │  Adapter.toAcf  │                    │
                   │  └────────┬────────┘                    │
                   │           ▼                             │
                   │  ┌─────────────────┐                    │
                   │  │ Canonical store │   (append event)   │
                   │  │   (event log)   │                    │
                   │  └────────┬────────┘                    │
                   │           ▼                             │
                   │  ┌─────────────────┐                    │
                   │  │ Routing engine  │   (sync rules)     │
                   │  └────────┬────────┘                    │
                   │           ▼                             │
                   │  ┌─────────────────┐                    │
                   │  │  Adapter.       │   outbound         │
                   │  │  applyOutbound  │   pipeline         │
                   │  └────────┬────────┘                    │
                   │           ▼                             │
                   └───────────┼─────────────────────────────┘
                               ▼
                          native FS (other agents)
```

### 4.3 Filesystem watching

Per platform:

- **macOS:** `FSEvents` for coarse directory-level notifications, supplemented with stat polling for sub-second precision when the user is actively interacting.
- **Linux:** `inotify` directly. A watch budget is enforced (`fs.inotify.max_user_watches`); the daemon warns the user if it cannot register all required watches and falls back to periodic scans for the overflow.
- **Windows:** `ReadDirectoryChangesW` with overlapped I/O. Robust against the per-buffer overflow conditions Windows imposes on busy directories.

Across platforms:
- Watch only the directories the relevant adapters declare. The daemon does not crawl the user's home directory.
- Symlinks are followed only when the target is inside an adapter-declared root; otherwise ignored.
- Artifacts larger than the configurable `limits.max_artifact_size_mb` threshold (64 MB by default) are flagged rather than ingested automatically. Attachment events also follow the lower attachment limit defined in [02-brd-format-adapters.md §4.9](02-brd-format-adapters.md).

**Global vs project storage.** Each adapter declares **two** sets of paths: its **global** storage (e.g., `~/.claude/`, `~/.codex/`) and its **project** storage pattern (e.g., `./.claude/`, `./AGENTS.md`, `./CLAUDE.md`). The daemon watches the global paths permanently and discovers project storage dynamically as the user works in project directories. See [02-brd-format-adapters.md §4.13](02-brd-format-adapters.md) for the scope model.

**Project discovery.** When the daemon observes an agent reading or writing inside a directory that contains project-storage markers (`.claude/`, `.git/`, `AGENTS.md`, etc.), it registers that directory as an active project and starts watching its project-storage paths. The project's canonical ID is derived per §4.13.3. Aplexica maintains an active-projects list in `~/.aplexica/projects.json` that survives daemon restarts.

**Pending-project staging.** Project-scoped artifacts received via a transport plugin (or via `aplexica import`) for projects not currently registered on the device land in `~/.aplexica/store/pending/<projectId>/` and are not materialized. The daemon attempts to match pending projects to newly-detected local projects on every scan; matches trigger automatic materialization per FR-02.39.

### 4.4 Inbound debouncing and coalescing

Filesystem events are noisy. A single user action (saving a memory) may trigger several FS events (write, rename, stat). The daemon:

- Coalesces events for the same file occurring within a 250 ms window into a single logical change.
- Waits for a 500 ms quiet period on a path before reading, to let editors finish atomic-write-and-rename.
- Hashes the file content; if the hash matches the last-known hash, drops the event.

### 4.5 Outbound recursion guard

When the daemon writes to a native file (because another agent's change is being propagated), the adapter's own watcher will see that write and would otherwise trigger an inbound event, causing a loop. Each adapter MUST tag its outbound writes (either via xattr on macOS/Linux, an ADS on Windows, or a sidecar marker file) and ignore inbound events whose tag matches a recently completed outbound write.

A second-line defense: every event in the canonical store carries an `originDevice` and a `causedBy` event hash. The routing engine refuses to materialize an event into an agent if that event's `causedBy` chain already includes a materialization by that same agent — breaking any cycle within at most one round-trip.

### 4.6 Conflict detection

Concurrent local conflicts on the same machine are rare but possible (e.g., the user edits `CLAUDE.md` by hand at the same moment a different agent updates it).

- Last-writer-wins is **not** the policy. The daemon detects content divergence (different new hashes from different sources within the conflict window) and creates a new branch on the affected artifact rather than discarding work. The user sees a notification and can resolve via `aplexica resolve <artifactId>`.
- **Initial scan divergence (decided 2026-05-18, was OQ-03.2):** When the daemon's initial reconciliation scan at startup finds the same artifact ID in two different adapters with diverging contents (e.g., a memory exists in both Claude Code and Codex with different bodies), the same divergent-branch policy applies. The daemon creates a divergent branch per source, surfaces the conflict in `aplexica status` and on the tray indicator, and requires the user to resolve via `aplexica resolve <artifactId>` before either version is propagated to other agents. The daemon does NOT pick a "source of truth" automatically and does NOT refuse to start — it operates normally for non-conflicting artifacts and surfaces the conflicts as work items.
- Deep treatment of branch creation, merge UI, and conflict resolution flows lives in [04-brd-forking-and-merging.md](04-brd-forking-and-merging.md).

### 4.7 Event ordering and dependency satisfaction

Artifact types reference each other (tools → skills → conversations; see [02-brd-format-adapters.md §4.8](02-brd-format-adapters.md) for the dependency graph). The live-sync path satisfies these dependencies automatically without an explicit phasing logic, because **events are applied in event-time order**.

The mechanism:

1. Every ACF event carries an `at` timestamp (recorded by the originating adapter when the source agent wrote the underlying native file).
2. The daemon's outbound pipeline applies events to a target adapter in monotonically increasing `at` order. An event with timestamp T MUST be applied after every event with timestamp < T that targets the same adapter, and before every event with timestamp > T.
3. The user physically cannot use a tool before installing it. If the user wires up an MCP server at T=0 and invokes it in a conversation at T=1, the source agent's native filesystem holds the `tool` artifact at T=0 and the conversation `tool_call` at T=1. The watcher emits inbound events in that order; the fan-out applies them to other agents in that order. By the time the conversation event reaches Agent B, the tool event already has.

This is in contrast to the **bulk-import** path (a `.aplexica` bundle), where all artifacts arrive simultaneously and no inherent time order exists across artifact types. The importer there uses the explicit phase ordering described in [01-brd-backup-restore.md §4.7](01-brd-backup-restore.md): `tool` → `memory` → `skill` → `conversation`.

Edge case — clock skew when events arrive via a transport plugin: when events from multiple devices interleave, two events with adjacent timestamps could plausibly be reordered. The daemon uses **causal ordering** via the event log's parent-hash chain to recover the true ordering — if event B's `parentHash` references event A, then A must apply before B regardless of timestamps. This is sufficient for dependency satisfaction in V1; the transport plugin is responsible for any deeper ordering guarantees at the wire level.

### 4.8 Retention engine and snapshots

The canonical store grows monotonically as events accumulate. Inline-Base64 binary attachments (per [02-brd-format-adapters.md §4.9](02-brd-format-adapters.md)) accelerate this growth. The retention engine bounds the store's size without breaking append-only semantics, branch/fork operations, or real-time sync.

The mechanism rests on **snapshots** (defined in [02-brd-format-adapters.md §4.12](02-brd-format-adapters.md)) and a careful pruning policy that respects the branch graph.

#### 4.8.1 Snapshot cadence

The retention engine creates snapshots automatically on these triggers, whichever fires first per artifact type:

| Artifact type | Event-count trigger | Time trigger |
|---|---|---|
| **Conversation** | every 100 events | every 24 hours |
| **Memory** | every 50 updates | every 7 days |
| **Skill** | every 50 updates | every 7 days |
| **Tool** | every 50 updates | every 7 days |

Triggers are evaluated per artifact, not globally. An artifact that hasn't changed in a week won't get a snapshot just because the timer fired — there's nothing new to encode.

Manual snapshots are always available: `aplexica snapshot <artifact-id>` creates one on demand, useful before risky operations.

#### 4.8.2 Pruning policy

Pruning runs on two triggers:

- **On-snapshot (primary):** Every time a snapshot is published, the retention engine evaluates which events between the previous snapshot and this one can be pruned. This is the routine, predictable path.
- **Disk-pressure (emergency):** When a finite `retention.store_max_gb` is configured and the store exceeds the `retention.store_high_watermark` fraction (0.80 by default), the engine accelerates: it may force snapshots on artifacts that don't normally need them, prune more aggressively within grace periods, and surface a warning. The shipped `store_max_gb = 0` default disables quota-driven pressure handling.

Pruning rules:

1. **Branch-ancestor protection.** An event that is an ancestor of any current branch's `head` MUST NOT be pruned, regardless of snapshot coverage. The engine walks the branch graph before deletion.
2. **Grace period.** Events covered by a snapshot move to `~/.aplexica/store/events/.compacted/` and stay there for a configurable grace period (default 7 days). They remain accessible via `aplexica log --include-compacted`. After grace, they are deleted irrecoverably.
3. **Pin exemption.** Artifacts tagged `pinned` or `keep-forever` are exempt from event pruning entirely. Pinned attachments are also exempt from attachment-only eviction.
4. **Snapshots themselves are never pruned** under default policy. They are the long-term storage.

#### 4.8.3 Attachment-only eviction (OSS default)

A separate, lighter retention mode applies before full event pruning: **attachment-only eviction**. When the canonical store grows past the soft watermark and event pruning isn't yet warranted, the engine evicts binary attachment payloads from old events while keeping the event metadata and text content intact.

An evicted attachment becomes a placeholder:

```jsonc
{
  "kind": "image",
  "mimeType": "image/png",
  "encoding": "base64",
  "data": null,
  "evicted": {
    "at": "2026-05-18T14:00:00Z",
    "reason": "disk-pressure",
    "originalSize": 1456320,
    "contentHash": "sha256:..."
  }
}
```

The conversation text remains fully readable; old images appear as `[attachment evicted, image/png, 1.4 MB, hash abc123]` placeholders in the UI. Recent attachments (default: under 30 days old) are protected.

This mode is **on by default for OSS** because it preserves the part of conversations users actually re-read (text) while shedding the part that dominates storage cost (Base64 blobs). Users who require full-fidelity image retention can disable it with `retention.attachments_only = false`, which makes the engine fall back to full event pruning at the soft watermark.

#### 4.8.4 Configuration knobs

```toml
[retention]
# Snapshot cadence (overrides defaults from §4.8.1)
snapshot_after_events.conversation = 100
snapshot_after_events.memory       = 50
snapshot_after_events.skill        = 50
snapshot_after_events.tool         = 50
snapshot_after_time.conversation   = "24h"
snapshot_after_time.memory         = "7d"
snapshot_after_time.skill          = "7d"
snapshot_after_time.tool           = "7d"

# Pruning policy
grace_period_days       = 7
keep_last_n_snapshots   = "all"          # or an integer; default keep all snapshots forever
attachments_only        = true            # OSS default; false = use full event pruning
attachment_min_age_days = 30              # don't evict attachments younger than this

# Disk-pressure
store_max_gb           = 0                # unlimited; set a finite cap explicitly
store_high_watermark   = 0.80             # with a cap, trigger emergency pruning at 80%
store_emergency_quota  = 0.95             # with a cap, refuse new ingestion above 95%

# Pin-tag exemption
pin_tags = ["pinned", "keep-forever"]    # artifacts with these tags exempt from any pruning
```

#### 4.8.5 CLI surface

- `aplexica snapshot <artifact-id>` — produce an explicit snapshot now.
- `aplexica snapshot list <artifact-id>` — show all snapshots for an artifact.
- `aplexica gc` — run a manual retention pass (creates due snapshots, evicts attachments, prunes events). Honors all config policy.
- `aplexica gc --dry-run` — preview what would happen.
- `aplexica retention show` — show the current retention configuration.
- `aplexica retention set <key> <value>` — update a retention setting.
- `aplexica retention preview` — show which artifacts have how many old events, what would be pruned next, projected storage savings.
- `aplexica pin <artifact-id> [--tag <name>]` — pin an artifact (defaults to the `pinned` tag).
- `aplexica unpin <artifact-id>` — remove the pin tag.
- `aplexica log --include-compacted` — include events in the grace-period `.compacted/` directory in the log output.
- `aplexica restore <eventId>` — restore an event from `.compacted/` back into the active log (only valid during grace period).

#### 4.8.6 Transport-plugin coordination

In single-device use, the retention engine runs entirely locally. When a transport plugin is active, the engine coordinates with the plugin via the plugin API:

- An event MAY be pruned locally only after a snapshot covering it has been published AND acknowledged by every device in the namespace (the transport plugin tracks per-device cursors).
- A transport plugin MAY maintain longer retention than any individual device and MAY serve old events to a device that needs to inspect history past its local pruning window.

#### 4.8.7 Functional requirements

- **FR-03.16** The retention engine MUST create snapshots automatically on the cadence defined in §4.8.1, configurable per artifact type.
- **FR-03.17** Pruning MUST be branch-ancestor-safe: events that are ancestors of any active branch's head MUST NOT be pruned.
- **FR-03.18** Pruning candidates MUST move through a grace-period staging area (`events/.compacted/`) before final deletion. The default grace period MUST be 7 days; the grace period MUST be user-configurable.
- **FR-03.19** Artifacts tagged with any of `retention.pin_tags` MUST be exempt from event pruning AND from attachment-only eviction.
- **FR-03.20** The OSS default retention mode MUST be `attachments_only = true`, which preserves all event metadata and text content while evicting binary attachment payloads older than `attachment_min_age_days` (default 30 days) when the canonical store approaches `store_high_watermark`.
- **FR-03.21** Disk-pressure emergency pruning MUST trigger above `store_high_watermark` (default 80% of `store_max_gb`) and MUST surface a user-visible warning in `aplexica status`. New event ingestion MUST be refused above `store_emergency_quota` (default 95%).
- **FR-03.22** The CLI MUST provide the commands listed in §4.8.5.
- **FR-03.23** `aplexica gc --dry-run` MUST produce a structured report (Markdown + JSON) listing every action that would be taken, with cumulative space-saved estimates, before any changes occur.
- **FR-03.24** When a transport plugin is active, the local retention engine MUST consult the plugin's per-device acknowledgment cursors before pruning past a snapshot. Pruning past a snapshot MUST be allowed only when every peer device in the namespace has acknowledged the snapshot — or when the user explicitly overrides via `aplexica gc --force-local-only` (which bypasses plugin coordination for the affected artifact until the user re-syncs).
- **FR-03.25** Snapshot events MUST be exempt from pruning under default policy; the `keep_last_n_snapshots` config knob allows the user to opt into pruning very old snapshots if needed, but never below the most-recent-2 floor.

### 4.9 Status indicator (tray app) — V1 scope

The daemon ships with a lightweight, cross-platform **status indicator** that lives in the system's standard tray/menubar area. It is the primary always-visible surface for daemon state — users glance at the icon to know whether sync is healthy, paused, conflicted, or errored, without running a CLI command. Promoted into V1 scope by decision on 2026-05-18 (was OQ-03.3).

#### 4.9.1 Per-platform integration

| Platform | Surface | Implementation |
|---|---|---|
| **macOS** | Menubar item (NSStatusItem) | Native, signed/notarized. |
| **Linux** | StatusNotifierItem (KDE/GNOME/Plasma; uses `libayatana-appindicator` for broad compatibility) plus a fallback to `xembed` for older trays | Single binary; auto-detects desktop environment. |
| **Windows** | System tray icon (Shell_NotifyIcon) | Native, signed. |

The indicator runs as a small companion process (`aplexica-tray`) spawned by the daemon on user login when enabled. It communicates with the daemon over the existing local control endpoint (Unix domain socket / named pipe); no separate IPC surface.

#### 4.9.2 Visible state

The icon's appearance reflects daemon state at a glance:

| Icon state | Meaning |
|---|---|
| **Idle (default)** | Daemon running, no activity, no conflicts. |
| **Active (subtle motion or alt icon)** | Daemon is processing an event (debounced; flicker-free). |
| **Pending (badge)** | Pending project artifacts staged but not materialized; conflicts unresolved; storage approaching quota. Badge count reflects the most urgent number. |
| **Warning (yellow/orange)** | Adapter quarantined, network drop, plugin auth issue, quota near limit. |
| **Error (red)** | Daemon crashed, store corrupted, fatal config error. Clicking the icon opens recovery options. |
| **Paused (alt icon)** | User has paused sync. |
| **Disabled** | Indicator launched but user disabled via `tray.enabled = false`. |

#### 4.9.3 Menu contents

Right-click (or click on macOS) opens a small menu:

- **Status: ...** (read-only header summarizing the current state)
- **Conflicts (N) →** opens a sub-menu listing artifacts in conflict; selecting one opens the resolver
- **Pending projects (N) →** lists pending projects with "Link to local path..." actions
- **Pause sync** / **Resume sync**
- **Pause sync for ...** (1 hour, until restart, custom)
- **Open status report** (`aplexica status` rendered in the default terminal or in a small window)
- **Open logs**
- **Open config**
- **Quit indicator** (does NOT stop the daemon; just hides the indicator until next login)

The menu is intentionally minimal — it surfaces state and offers fast escape hatches. Deeper operations stay in the CLI.

#### 4.9.4 Accessibility and opt-out

- The indicator is **opt-in by default on some platforms and opt-out on others** based on platform conventions: macOS opt-in (users are sensitive to menubar clutter), Linux opt-in (because tray support varies), Windows opt-out (system tray is the conventional surface).
- Users can disable the indicator entirely with `tray.enabled = false` in config. Daemon runs normally without it.
- The indicator MUST be accessible: tooltips, keyboard-navigable menus where the platform allows, screen-reader-readable state strings.
- The indicator MUST NOT auto-focus, steal attention, or display modal dialogs except in fatal-error situations where the user's action is required (e.g., "store corrupted — please run `aplexica doctor`").

#### 4.9.5 Functional requirements

- **FR-03.26** The daemon installer MUST install the `aplexica-tray` companion binary alongside the daemon binary on all V1 platforms (macOS, Linux, Windows).
- **FR-03.27** The indicator MUST reflect daemon state changes within 2 seconds (it subscribes to daemon state-change notifications over the control endpoint, not polls).
- **FR-03.28** The indicator MUST handle daemon restart gracefully — reconnect to the control endpoint with exponential backoff, do not crash.
- **FR-03.29** Quitting the indicator MUST NOT stop the daemon. Stopping the daemon MUST NOT leave the indicator in a stuck state.
- **FR-03.30** The indicator MUST be disable-able via `tray.enabled = false` in the user config. Disabled state MUST persist across logins.
- **FR-03.31** All indicator strings MUST be in the externalized translation catalog (see [10-non-functional-requirements.md §6](10-non-functional-requirements.md)) so future localization works without code changes.
- **FR-03.32** The indicator MUST consume less than 50 MB of resident memory at idle and less than 1% of one CPU core averaged over typical workloads.

## 5. Configuration

**Reminder: the golden rule (see [00-vision.md §7](00-vision.md), item 8, and [10-non-functional-requirements.md §11](10-non-functional-requirements.md)) applies in full to the daemon.** Every value the daemon reads at runtime — watcher debounce windows, retry budgets, queue depths, snapshot cadences, conflict-window timing, recursion-guard windows, watch-budget overflow thresholds — originates in the layered configuration architecture. None of these values are constants in source code.

The user-facing config file is `~/.aplexica/config.toml`. The CLI edits it. But this file is layer 3 of the layered model — it inherits everything from the shipped `defaults.toml` (layer 1) and any optional system config (layer 2), and is itself overridden by project config (layer 4), env vars (layer 5), and CLI flags (layer 6). See [10-non-functional-requirements.md §11.1](10-non-functional-requirements.md) for the full layering model.

Sample of the user-facing config (showing only common overrides; defaults.toml is much longer):

```toml
[daemon]
# What the daemon does on first launch with no prior state.
bootstrap = "import-from-all-installed"  # or "empty"

[adapters.claude-code]
enabled = true
root = "~/.claude"

[adapters.codex]
enabled = true
root = "~/.codex"

[adapters.hermes]
enabled = true
root = "~/.hermes"

[adapters.openclaw]
enabled = false           # opt out of syncing this agent even though installed

[adapters.kilo]
enabled = true

[sync]
default_rule = "all-to-all"   # see selective-sync BRD for richer expressions

[limits]
max_artifact_size_mb = 64
inotify_watch_budget = 8192
```

The daemon hot-reloads the config on SIGHUP (Unix) or via `aplexica daemon reload`. Changes take effect without restarting the daemon.

## 6. Functional requirements

- **FR-03.1** The daemon MUST install itself as a per-user service on macOS, Linux, and Windows via `aplexica daemon install`.
- **FR-03.2** The daemon MUST start automatically at user login.
- **FR-03.3** The daemon MUST detect installed agents at startup using each adapter's `discover()` method.
- **FR-03.4** The daemon MUST perform an initial reconciliation scan at startup for every enabled adapter and bring the canonical store up to date with current native state.
- **FR-03.5** The daemon MUST emit log entries for every inbound event, outbound event, conflict, and adapter error.
- **FR-03.6** Logs MUST be written to `~/.aplexica/logs/` with daily rotation and a 30-day retention cap.
- **FR-03.7** The daemon MUST expose a local control endpoint (Unix domain socket on Unix; named pipe on Windows). The CLI talks to the daemon via this endpoint.
- **FR-03.8** The CLI MUST support:
  - `aplexica daemon status` — running state, version, uptime, watched agents.
  - `aplexica daemon start` / `stop` / `restart` / `reload`.
  - `aplexica daemon install` / `uninstall`.
  - `aplexica daemon logs [--follow]`.
  - `aplexica adapters list` and `aplexica adapters enable|disable <name>`.
  - `aplexica sync pause [--agent <name>] [--for <duration>]`.
  - `aplexica sync resume [--agent <name>]`.
  - `aplexica sync run-once --from <a> --to <b>` (force a one-shot reconciliation between two adapters).
- **FR-03.9** The daemon MUST detect when an adapter is operating against an agent that is currently writing and apply the platform-appropriate stable-read strategy.
- **FR-03.10** The daemon MUST batch outbound writes to a single agent during high-frequency change bursts so the target agent's process is not flooded.
- **FR-03.11** The daemon MUST honor an explicit per-agent pause and stop generating outbound writes to that agent until resumed.
- **FR-03.12** The daemon MUST refuse to start (with a clear error) if the canonical store schema is from a newer Aplexica version than the installed binary supports.
- **FR-03.13** The daemon MUST handle EBUSY, EACCES, ENOSPC, and equivalent platform errors gracefully — the affected operation is retried with exponential backoff up to a per-error budget; persistent failures surface as user-visible warnings without crashing the daemon.
- **FR-03.14** The daemon MUST expose a Prometheus-style metrics endpoint on a local loopback port (off by default; enabled by config) for users who want to integrate with their own monitoring.
- **FR-03.15** The daemon MUST detect adapter crashes and restart the adapter with bounded retries (3 attempts in 10 minutes, then quarantine the adapter and continue running others).

## 7. Non-functional requirements

| ID | Requirement |
|---|---|
| **NFR-03.1** | End-to-end latency from a native write in Agent A to the equivalent native write in Agent B MUST be under 2 seconds at the 95th percentile on a typical developer machine, assuming both agents are idle and the artifact is small (<1 MB). |
| **NFR-03.2** | Daemon memory footprint at rest (no recent activity, five adapters enabled) MUST be under 200 MB resident. |
| **NFR-03.3** | Daemon CPU usage MUST average under 1% of one core during typical workloads. |
| **NFR-03.4** | Daemon MUST survive a system sleep / wake cycle and resume watching without manual intervention. |
| **NFR-03.5** | The daemon MUST NOT block agent operations. A slow inbound pipeline must never prevent a user from continuing to use Agent A normally. |
| **NFR-03.6** | The daemon MUST start in under 2 seconds at user login (measured from process spawn to first watcher registered). |
| **NFR-03.7** | Logs MUST be human-readable line-oriented JSON. |

## 8. Out of scope

- Cross-machine sync. The daemon is intentionally single-machine; cross-device transport is handled by a separate transport plugin via the plugin API.
- An agent chat or artifact-authoring GUI. The tray and local dashboard provide daemon status and operational controls only.
- Watching arbitrary directories the user designates outside adapter scope.
- Real-time sync against agents that store state in a remote-only service (e.g., a hosted ChatGPT account). Such agents are not eligible for V1; ACF requires local files to operate on.

## 9. Acceptance criteria

V1 is complete for local real-time sync when:

1. The daemon installs and starts at login on macOS, Linux, and Windows.
2. With Claude Code and Codex both installed, writing a memory in one results in the equivalent memory in the other within 2 seconds (95th percentile).
3. Installing a skill in any V1 agent causes equivalent skills (or annotated documents, when conversion is lossy) to appear in every other V1 agent within 5 seconds.
4. A 24-hour soak test on a representative developer workload shows zero unintended sync loops and zero data corruption events in the canonical store.
5. Pausing sync for an agent stops outbound writes to that agent immediately and the pause persists across daemon restart.
6. The daemon recovers from a forced kill (`kill -9`) and re-establishes the canonical store consistent state on next start.

## 10. Resolved decisions

All open questions for this BRD were resolved on 2026-05-18:

- ~~**OQ-03.1** Auto-start agent process when materializing into a non-running agent~~ — **Decided: V1 no.** Materializing into a non-running agent writes to its native storage; the user starts the agent when they want to. Any future auto-start behavior must be explicit, configurable, and off by default.
- ~~**OQ-03.2** Initial-scan divergence policy~~ — **Decided: create a divergent branch and require user resolution.** Same as the runtime conflict policy in §4.6. The daemon does not pick a source of truth automatically and does not refuse to start.
- ~~**OQ-03.3** Tray-icon GUI as V1~~ — **Decided: V1 yes.** Tray app is firm V1 scope, not a stretch goal. Full design in §4.9.

## 11. Dependencies

- ACF and adapter API — see [02-brd-format-adapters.md](02-brd-format-adapters.md).
- Branch and conflict model — see [04-brd-forking-and-merging.md](04-brd-forking-and-merging.md).
- Routing rules — see [05-brd-selective-sync-and-routing.md](05-brd-selective-sync-and-routing.md).
