# Aplexica User Guide

This guide explains how the Aplexica command line, tray app, and local web UI
work together. It is written for the open source Aplexica daemon and avoids
private-business, service-specific, or personal information.

For the short install walkthrough, see [quickstart.md](quickstart.md). For
adapter-specific behavior, see [adapters/](adapters/).

## What Aplexica Runs

Aplexica installs three user-facing pieces:

- `aplexica`: the CLI. It installs and controls the background daemon, opens the
  local web UI, manages artifacts, resolves conflicts, configures routing rules,
  and performs backup/restore tasks.
- `aplexicatray`: the desktop tray or menu-bar companion. It watches daemon
  status and exposes common actions without opening a terminal.
- Local web UI: an embedded browser interface served by the daemon on loopback
  HTTP. It is opened from the tray or with `aplexica web open`.

The background daemon watches supported AI coding agents, imports their native
files into the Aplexica Canonical Format, and fans compatible artifacts back out
to other installed agents. The canonical store lives under the user's Aplexica
data directory and remains local unless the user explicitly configures an
optional remote transport plugin.

## First Run

The normal setup path is:

```bash
aplexica setup --install
```

The interactive wizard asks whether to enable the tray, enable the local web UI,
install and start Aplexica now, and open the web UI. A non-interactive install
can accept defaults:

```bash
aplexica setup --yes --install
```

After setup:

```bash
aplexica status
aplexica web open
```

`status` confirms daemon liveness, agent detection, artifact counts, and active
conflict count. `web open` mints a one-time local bootstrap URL and opens the
browser.

## CLI Overview

Run `aplexica --help` for the complete command list and
`aplexica <command> --help` for command-specific flags. The table below explains
the user-facing purpose of each command group.

| Command | What it does |
|---|---|
| `setup` | First-run wizard. Writes local defaults, optionally installs the daemon service and tray autostart, starts the daemon, and opens the web UI. |
| `status` | Shows daemon status, installed agents, artifact counts, unresolved conflicts, and optional JSON/watch output. |
| `daemon` | Starts, stops, restarts, reloads, installs, uninstalls, and tails logs for the background sync daemon. |
| `web` | Opens the local web UI, prints the listener port, issues one-time bootstrap URLs, revokes web sessions, and enables or disables the listener. |
| `tray` | Installs or uninstalls the per-user tray autostart entry. The daemon is managed separately. |
| `config` | Shows, edits, validates, diffs, and documents layered configuration. |
| `adapters` | Lists, enables, disables, and checks supported adapter installations. |
| `sync` | Chooses which agents receive synced data (`enable`, `disable`, `agents`), pauses and resumes sync, runs a one-shot reconciliation, and — when given a directory — runs a foreground sync session. The daemon is preferred for day-to-day use. |
| `backfill` | Materializes conversation history into this device's agents past the recent-history cap. Local only; prints a plan by default. |
| `watch` | Watches a directory and imports settled files through a named adapter. |
| `list` | Lists artifacts in the canonical store. |
| `show` | Shows metadata, event history, and payload summary for one artifact. |
| `import` | Imports a native agent file or database into the canonical store. |
| `export` | Exports a supported artifact back to an agent-native file tree. |
| `convert` | Transcodes an ACF bundle into a target adapter's native format. |
| `verify` | Validates a bundle against the schema and re-hashes every event. |
| `backup` | Exports the canonical store to a portable `.tar.gz` bundle. |
| `restore` | Restores canonical-store artifacts from a bundle. |
| `backups` | Lists native agent-state snapshots. These are snapshots of agent-owned files, not canonical-store bundles. |
| `restore-native` | Destructively restores agent-owned files from a native snapshot. A pre-restore snapshot is taken first so the operation is reversible. |
| `snapshot` | Adds snapshot events to bound replay cost for an artifact chain. |
| `retention` / `gc` | Runs retention and cleanup passes for snapshots, pruned history, and blob storage. |
| `pin` / `unpin` | Protects or unprotects an artifact from retention pruning and eviction. |
| `delete` | Permanently deletes an artifact from the canonical store. |
| `project` | Manages the local project registry used to map canonical project IDs to local paths. |
| `pending` | Lists project-scoped artifacts whose project is not linked locally yet. |
| `rules` | Lists, adds, removes, edits, tests, and reapplies selective-sync routing rules. |
| `tag` | Adds, removes, and lists artifact tags. |
| `event` | Performs event-level inspection and tag operations. |
| `conflicts` | Lists, inspects, and resolves conflicts detected by the daemon. |
| `branch` | Lists, creates, renames, archives, unarchives, and deletes artifact branches. |
| `fork` | Forks an artifact at a chosen event into a new branch, optionally targeting an agent. |
| `checkout` | Sets an agent's materialization pointer to a selected branch. |
| `log` | Prints the event log for an artifact. |
| `diff` | Diffs two branches or two events of an artifact. |
| `merge` | Merges one branch into another. |
| `resolve` | Starts an interactive conflict resolver. |
| `tool` | Lists tool artifacts, previews metadata, checks capabilities, and toggles secret sync for a tool. |
| `secret` / `list-secrets` | Manages secret values referenced by tool artifacts without printing secret values by default. |
| `remote` | Configures an optional remote-transport plugin. The daemon treats all compatible plugins through the same protocol. |
| `repair` | Offline canonical-store repair. `repair conversation` collapses echo-duplicated turns; `repair materialization` inspects or drains the native-write retry queue. |
| `doctor` | Produces a redacted diagnostic report suitable for support or bug reports. |
| `update` | Reports which installer owns this build and the exact command to upgrade it. |
| `version` | Prints build version, commit, and date. |
| `completion` | Generates shell completion scripts. |
| `keygen` / `keygen-age` | Generates signing or encryption keypairs for bundle workflows. |
| `hermes` | Runs Hermes-specific import/export helpers. |
| `orphans` | Inspects and cleans orphaned artifacts. |

### Common CLI Workflows

Start or restart the full background service:

```bash
aplexica daemon install
aplexica daemon start
aplexica daemon restart
```

Open and secure the local web UI:

```bash
aplexica web open
aplexica web issue-token
aplexica web revoke-sessions
aplexica web disable
aplexica web enable
```

Inspect sync state:

```bash
aplexica status
aplexica status --watch
aplexica list
aplexica show <artifact-id>
aplexica event --help
```

Choose which agents receive data. Aplexica discovers installed agents and
imports from them automatically, but it writes to none of them until you say
so — the "discover, then await config" default:

```bash
aplexica sync agents
aplexica sync enable codex
aplexica sync enable --all
aplexica daemon reload
```

Pause and resume without disabling anything:

```bash
aplexica sync pause --for 2h
aplexica sync pause --agent codex
aplexica sync pause-status
aplexica sync resume
```

Resolve divergence:

```bash
aplexica conflicts list
aplexica conflicts show <artifact-id>
aplexica resolve <artifact-id>
```

Work with conversation branches:

```bash
aplexica branch list <artifact-id>
aplexica fork <artifact-id> --to-agent codex
aplexica checkout <artifact-id> --branch <branch> --agent <agent>
aplexica merge <artifact-id> --from <branch> --into <branch>
```

Back up and restore:

```bash
aplexica backup
aplexica restore <bundle-path>
aplexica backups list
aplexica restore-native --from <snapshot-id> --yes
```

### Giving a Newly Enabled Agent Your History

Enabling an agent does not hand it your whole archive. By default each agent
receives only the most recent conversations — ten, unless you have changed
`sync.convBackfill` or a rule's `route.historicalSyncDepth`. Everything from
that point forward syncs live and is never capped.

To push more history into this device's agents, use `backfill`. It prints a
plan and writes nothing until you pass `--apply`:

```bash
aplexica backfill
aplexica backfill --agent kilo --apply
aplexica backfill --depth 100 --apply
aplexica backfill --apply
```

Three things are worth knowing before you run it:

- **It is local.** The pass writes native session files for agents on this
  machine and publishes nothing. Cross-device backfill is outside this command's
  supported scope.
- **A conversation is never materialized into the agent that wrote it.** So
  the per-agent counts differ, and backfilling only an agent's own
  conversations plans nothing.
- **The counts are eligibility counts,** not pending writes. The planner does
  not check what is already materialized, so an already-backfilled store
  re-plans the same numbers and the apply pass finds each session current.

A full backfill of a large store creates one native session per conversation
per agent. Run the dry run first and read the numbers.

### Repairing a Conversation

`repair` is for the rare case where a conversation's canonical head is wrong
rather than merely out of date. Both subcommands are read-only until you ask
otherwise:

```bash
aplexica repair materialization
aplexica repair conversation <artifact-id>
aplexica repair conversation --all
```

`repair materialization` lists native session writes the daemon could not
complete, with the agent and artifact for each. `repair conversation` collapses
turns that a fixed continuation bug echoed back into a thread, and prints the
proposed collapse without writing anything.

Applying a conversation repair is a fleet-wide operation, not a per-device one.
A peer still holding the duplicates classifies the repaired thread as a stale
redelivery and skips it, so a repair applied on one device alone gets undone.
Stop every daemon, repair each device, then restart them:

```bash
aplexica repair conversation --all --apply --device-id <this-device-id>
```

Pass `--device-id` or the repaired head is not eligible for the outbound sweep,
and peers keep serving the duplicated copy. Read this device's id from
`aplexica status --json` (`daemonInfo.localDeviceId`). The collapse is greedy,
so removing one block can expose another; rerun the dry run afterward and
repeat until it reports nothing.

### Repairing a Forked Claude Code Mirror

Some conversations stop appearing in `claude` `/resume` even though nothing is
missing from the canonical thread and cross-device sync is healthy. The cause
is a fork in the session file's parent chain: Claude Code appended its own
reply to a point in the conversation that Aplexica had already extended, so
Aplexica's rows sit on a branch the resume view never walks. That state is
permanent on disk, and `aplexica status` reports the write as deferred with the
reason `forked_mirror`.

The daemon can rebuild such a file, but only if you ask it to. Set
`sync.repairForkedMirrors` in `<state-dir>/config.json` (usually
`~/.aplexica/state/config.json`) and restart the daemon:

```json
{
  "sync": {
    "repairForkedMirrors": true
  }
}
```

```bash
aplexica daemon restart
```

It is off by default because it rewrites a file Claude Code also writes to. The
guarantees when it is on:

- Only Aplexica's own synthetic mirrors are eligible. A session Claude Code
  itself started is never rebuilt.
- A mirror is rebuilt only when **every** conversational row in it can be
  reproduced from the canonical thread. One row the canonical thread has never
  seen — including one that carries a slash command, an injected memory block,
  an image, an extended-thinking block, a tool call, or a sub-agent (Task)
  transcript — and the daemon leaves the file alone and keeps reporting it.
  A row whose visible text matches the canonical thread but which also carries
  a pasted image or reasoning the rebuild cannot re-emit counts as such a row.
- The rewrite reuses the same file, so the session id and its place in
  `/resume` do not change, and no second copy of the thread appears. It also
  reuses the row identifiers Claude Code itself wrote, so a session you have
  open at the time can keep appending to it.
- The pre-repair bytes are written and flushed to disk before the file is
  touched, and an interrupted rewrite is rolled back, so a crash mid-repair
  cannot leave an empty transcript.
- The pre-repair bytes are kept under
  `~/.aplexica/quarantine/claude-conversations/`, outside `~/.claude`, so
  nothing preserved there can show up as another session.
- The canonical store is not touched. This repairs one device's view; it is
  unrelated to `aplexica repair conversation`, which is fleet-wide.

## Tray App

`aplexicatray` is the desktop companion for the daemon. It starts at login when
installed with `aplexica setup --install`, `aplexica daemon install --tray`, or
`aplexica tray install`.

### Tray States

| State | Meaning |
|---|---|
| Starting | The tray has launched but has not received a status snapshot yet. |
| Idle | The daemon is reachable and healthy, with no recent activity. |
| Active | The daemon has seen recent import or fan-out activity. |
| Conflict | One or more artifacts have divergent heads that need review. |
| Paused | The daemon has been quiet longer than the configured paused threshold or sync was stopped from the tray. |
| Error | The daemon is unreachable or status polling failed. |

### Tray Menu

| Menu item | Function |
|---|---|
| Status | Read-only line showing current state and watched directory when known. |
| Conflicts `(N)` | Shows unresolved conflicts. Clicking a conflict opens the conflict CLI view; overflow opens the full conflict list. |
| Pending projects `(N)` | Shows project IDs or paths waiting for a local project link. Clicking a row opens the pending-project CLI flow. |
| Open Aplexica | Mints a fresh one-time web UI bootstrap URL and opens the default browser. |
| Open watched directory | Reveals the daemon's watched directory in the OS file manager. |
| Pause sync / Resume sync | Stops or starts daemon sync from the tray. Quitting the tray does not stop the daemon. |
| Pause sync for... | Pauses for 1 hour, 4 hours, until restart, or a custom duration. Timed pauses auto-resume. |
| Open logs | Opens the daemon log directory. |
| Open status report | Runs `aplexica status` in a terminal. |
| Open config | Opens the local daemon configuration file in the default editor. |
| About Aplexica | Shows tray and daemon version information. |
| Quit Aplexica Tray | Closes the tray indicator only. The daemon keeps running. |

## Local Web UI

The local web UI is served by the daemon on loopback. It is protected by a
one-time bootstrap token and session cookies. Open it from the tray with
`Open Aplexica` or from a terminal:

```bash
aplexica web open
```

The sidebar is grouped into Overview, Sync, and System areas. The top bar
contains search placeholder controls, the sidebar toggle, and log out. The
footer links to the portal source commit.

### Dashboard

The Dashboard is the operational home page.

Daemon panel fields:

| Field or control | Meaning |
|---|---|
| Version | Version reported by the running daemon. |
| PID | Operating-system process ID. |
| State | Idle, active, or paused state derived from daemon status. |
| Uptime | How long the daemon process has been running. |
| Pending imports | Debounced filesystem changes waiting to be imported. |
| Pause / Resume | Pauses or resumes daemon sync. |

Analytics panels:

| Panel | Meaning |
|---|---|
| Agent sync activity | Stacked bars for recent sync events by agent and artifact kind. |
| Sync in / out by agent | Inbound and outbound event counts per agent. |

Other sections:

| Section | Meaning |
|---|---|
| Agents | Cards for installed agents, with status and artifact counts. |
| Recent activity | Live event rows. Each row shows event type, title, and relative time. |

### Agents

The Agents page shows supported AI coding agents and whether they were detected
on this machine.

Header controls:

| Field or control | Meaning |
|---|---|
| Connected count | Installed agents divided by total supported agents. |
| Enable sync for all / Disable sync for all | Toggles cross-agent fan-out to all installed agents. Discovery and import still run. |

Agent row fields:

| Field or control | Meaning |
|---|---|
| Agent | Human-readable agent name and description. |
| Status | Active, idle, paused, attention, or not installed. |
| Artifacts | Count of canonical-store artifacts attributed to that agent. |
| Adapter | Aplexica adapter version, not necessarily the agent's own version. |
| Last activity | Most recent known activity for that agent. |
| Sync on / Sync off | Per-agent fan-out toggle. |

Agent detail fields:

| Field or control | Meaning |
|---|---|
| Status badges | Installed state, sync state, artifact count, and last activity. |
| Adapter version | Version of the Aplexica adapter. |
| Agent version | Placeholder for the agent's native version when detection is available. |
| Watched locations | Native roots and registered project paths watched for this agent. Registered project paths can be removed. |
| Namespaces | Namespaces currently associated with the agent's artifacts. |
| Recent events | Event type, detail, and timestamp rows for that agent. |

### Event Stream

The Event stream page combines live server-sent events with paginated history.

Controls:

| Field or control | Meaning |
|---|---|
| Event kind filter | Filters to daemon, agent, imported, synced, checkpoint, refused, conflict, pending, or rule events. |
| Agent filter | Filters to agents that appear in the loaded events. |
| Collapse repeats | Folds repeated live pings or repeated artifact history into fewer rows. |
| Live status badge | Shows live, reconnecting, or disconnected state for the event stream. |
| Load older events | Fetches the next page of historical events. |

Event row fields:

| Field | Meaning |
|---|---|
| Source badge | Live or historical event indicator. |
| Event type | Human-readable event kind. |
| Artifact kind | Memory, skill, tool, conversation, or other when available. |
| Title | Event summary. |
| Metadata | Agent, path, artifact ID, size, reason, or target-agent details when available. |
| Timestamp | Local formatted event time. |

### Routing Rules

Routing rules decide which artifacts fan out where and which tags or restrictions
apply.

Rules list fields:

| Field | Meaning |
|---|---|
| Name | Rule identifier. Opens the detail page. |
| Match | Human-readable summary of artifact type, tags, source, scope, path, or other matchers. |
| Targets | Agents that receive matching artifacts. Empty target list means all installed agents. |
| Effect | Additional behavior, such as local-only routing, excluding secret values, assigning tags, or other advanced effects. |

Top-level controls:

| Control | Meaning |
|---|---|
| Add preset | Opens recommended rule bundles and applies selected presets. |
| Add rule | Opens the rule creation form. |

Add/edit rule fields:

| Field or control | Meaning |
|---|---|
| Name | Stable rule name. Names are disabled on edit; rename by creating a new rule and deleting the old one. |
| Mode | Optional sync mode: unset, live, scheduled, or manual. |
| Scheduled-sync interval (seconds) | Appears when Mode is scheduled. Defaults to `900` seconds, or 15 minutes. |
| Artifact types | Memory, skill, tool, and conversation checkboxes. No selection matches every type. |
| Match tags | Tags an artifact must already carry to match the rule. Free-text chips are allowed. |
| Target agents | Agents that should receive matching artifacts. No selection means all installed agents. The originating-agent option targets the source agent. |
| Assign tags | Tags added to matching artifacts. |
| Sync off device | When enabled, matching artifacts may use configured off-device transport. When disabled, matching artifacts stay local. |
| Include secrets | Whether secret values referenced by tool artifacts are allowed to sync. |

Rule detail also shows advanced matchers such as path, branch name, scope,
tool kind, source agent, source device, or skill mode as read-only rows when
they exist. The practical form preserves those advanced values when saving.

### Conflicts

The Conflicts page lists artifacts with divergent heads.

List fields:

| Field | Meaning |
|---|---|
| Artifact | Readable title when available, plus artifact ID. |
| Kind | Artifact kind, such as memory, skill, tool, or conversation. |
| Heads | Number of competing heads. |
| Actions | Opens the conflict detail page. |

Conflict detail fields:

| Field or section | Meaning |
|---|---|
| What changed | Analysis summary, recommendation, auto-resolve note, and highlighted differences. |
| Head A / Head B | Side-by-side details for the competing heads. |
| Readable summary | Human-readable summary of a head when analysis exists. |
| First visible content | Primary visible content extracted from a head when available. |
| Source agent | Agent that wrote the head. |
| Event ID | Canonical event identifier. |
| Content SHA-256 | Shortened content hash. |
| Timestamp | Event timestamp. |
| Payload preview | Expandable JSON or text preview when manual review is needed. |
| Accept A / Accept B | Chooses one head as the winner. |
| Edit manual merge | Opens a textarea to paste merged content. Not shown for conversation conflicts. |

Auto-resolvable conflicts show the analysis and no manual action requirement.

### Forking

The Forking page manages conversation branches.

Conversation search and rows:

| Field or control | Meaning |
|---|---|
| Search conversations | Filters recent conversations by text. |
| Conversation title | Readable conversation title. |
| Source agent | Agent that originated the conversation when known. |
| Description | Additional summary text when available. |
| Artifact ID | Canonical conversation artifact ID. |
| Turns | Number of conversation turns. |
| Branches | Number of branches on the artifact. |
| Updated | Last update timestamp. |

Branch panel controls:

| Field or control | Meaning |
|---|---|
| Fork from | Source branch head or event used as the fork point. |
| Agent | Target agent for the forked branch. |
| Branch | Optional branch name. If blank, Aplexica chooses a branch name. |
| Rationale | Optional note explaining why the fork was created. |
| Fork conversation | Creates the branch and materializes it for the selected agent. |
| Checkout branch | Branch to materialize into an agent. |
| Checkout agent | Agent that should receive the selected branch. |
| Checkout | Moves the selected agent's materialization pointer to the branch. |

Branch table fields:

| Field | Meaning |
|---|---|
| Branch | Branch name and fork source hash when known. |
| Events | Number of events on that branch. |
| Last event | Last event timestamp. |
| State | Active, archived, or merged. |
| Materialized | Agents currently materialized on the branch. |

### Projects

Projects map local folders to project-scoped artifacts.

Add folder fields:

| Field or control | Meaning |
|---|---|
| Path | Absolute or local folder path to register. |
| Scope | Local for folder-specific memory, global for broader memory scope. |
| Add project | Saves the project registration. |

Project table fields:

| Field or control | Meaning |
|---|---|
| Path | Display name and absolute path. |
| Scope | Local or global. |
| Agents | Agents allowed for this project. Empty means all agents. |
| View memory | Expands effective memory files for the project. |
| Edit | Edits scope and agent selection. |
| Remove | Removes the registry entry; it does not delete project files. |

Edit fields:

| Field | Meaning |
|---|---|
| Scope | Local or global. |
| Agents | Installed-agent checkboxes. Leaving all unselected means all agents. |

Effective memory fields:

| Field | Meaning |
|---|---|
| File name | Memory file name. |
| Synced agents | Agents receiving that memory file. |
| Content | Composed memory content as synced by the daemon. |

### Pending Projects

Pending projects are folders or project IDs Aplexica noticed but has not fully
registered on this device.

Suggestion rows:

| Field or control | Meaning |
|---|---|
| Suggested agents | Agents newly observed in an already registered project. |
| Add agents | Adds those agents to the registered project's agent set. |
| Dismiss | Dismisses the suggestion. |

Pending table fields:

| Field | Meaning |
|---|---|
| Path or project ID | Local sample path if known, otherwise canonical project ID. |
| Agents | Agents associated with the pending row. |
| Last active | Relative time of the most recent activity. |
| Type | Discovered folder, artifact-sourced project, or pathless artifact. |
| Actions | Approve/link or deny. |

Approve dialog fields:

| Field | Meaning |
|---|---|
| Path | Folder path being approved. |
| Scope | Local or global. |
| Agents | Agent checkboxes. Empty means all installed agents. |

Link dialog fields:

| Field | Meaning |
|---|---|
| Local path | Path that should be associated with a pathless or artifact-sourced project ID. |

Denied rows can be approved later or restored to the active pending list.

### Remote Connection

Some builds show an optional remote connection page when a compatible remote
transport plugin is configured. The open source daemon treats this as a plugin
protocol and does not require any specific provider.

States:

| State | Meaning |
|---|---|
| Not configured | No remote plugin is configured for this daemon. |
| Pairing wizard | A plugin is configured but this device is not paired. |
| Connected card | This device is paired and the plugin reports its connection state. |

Pairing fields:

| Field or control | Meaning |
|---|---|
| Pairing code | One-time code or token from the remote provider. |
| Device name | Optional friendly name for this machine. |
| Pair this device | Exchanges the code through the plugin and stores device credentials. |

Connected fields:

| Field or control | Meaning |
|---|---|
| Device ID | Opaque remote-device identifier. |
| Account ID | Opaque account or tenant identifier from the provider. |
| Relay connection | Connected, connecting, starting, disconnected, or unknown. |
| Test connection | Asks the plugin to verify current connectivity. |
| Pair a different device / re-pair | Reopens the pairing wizard. |
| Unpair this device | Removes local remote credentials and stops remote sync on this machine. |

### Backups

The Backups page manages native agent-state snapshots. These are separate from
canonical-store bundle backups.

Metric cards:

| Field | Meaning |
|---|---|
| Protected | Agents with usable native snapshots divided by detected agents. |
| Remote backup availability | Whether the configured backup destination is available. If no provider is configured, backups remain local. |
| History | Local and remote backup counts. |
| Storage | Total displayed size of known backups. |

Safety table fields:

| Field or control | Meaning |
|---|---|
| Agent | Agent name. |
| Status | Protected, backup required, blocked, overridden, or related safety state. |
| Roots | Number of native roots or the latest error. |
| Last backup | Relative time of the newest snapshot for that agent. |
| Backup | Starts a snapshot for that agent. |
| Override | Allows sync to proceed despite a backup blocker. Use only after understanding the risk. |

Manual backup fields:

| Field or control | Meaning |
|---|---|
| Local destination | Writes the snapshot to local storage. |
| Remote destination | Enabled only when a compatible provider is configured. |
| Agent checkboxes | Selected agents to snapshot. Empty means all agents. |
| Back up selected | Starts a snapshot for selected agents and destination. |
| Back up all | Starts a snapshot for all agents. |
| Cancel | Cancels a running backup job. |

Schedule fields:

| Field or control | Meaning |
|---|---|
| Enabled | Turns scheduled native snapshots on or off. |
| Presets | Hourly, 6 hours, 12 hours, daily, or weekly intervals. |
| Custom minutes | Custom interval in minutes, minimum 15. |
| Destination | Local or remote provider when available. |
| Agent checkboxes | Agents covered by the schedule. Empty means all agents. |
| Save schedule | Persists the schedule. |
| Next run | Next scheduled run when known. |

History filters:

| Field | Meaning |
|---|---|
| From / To | Date range filters. |
| Agent | Shows backups containing a selected agent. |
| Type | Manual, scheduled, pre-sync safety snapshot, or pre-restore undo snapshot. |
| Location | Local or remote. |
| Reset | Clears filters. |

History table fields:

| Field or control | Meaning |
|---|---|
| Created | Snapshot creation time. |
| Location | Local or remote and encryption status when known. |
| Type | Manual, scheduled, safety, or undo snapshot. |
| Agents | Agents recorded in the snapshot. |
| Size | Display size. |
| Files | Number of files in the snapshot. |
| Device | Origin device name when known. |
| Restore scope | For multi-agent snapshots, choose all agents or one agent before restoring. |
| Restore | Opens destructive restore confirmation. |
| Delete | Opens deletion confirmation. |

Retention fields:

| Field | Meaning |
|---|---|
| Keep | Per-agent number of native backups to retain. Values are clamped between 1 and 100. |
| Save retention | Persists retention limits. |

Delete confirmation requires typing the displayed phrase. Restore confirmation
overwrites live native agent files, but Aplexica takes a pre-restore snapshot
first so the operation can be reversed.

### Settings

Settings exposes safe-to-edit daemon configuration.

| Field or control | Meaning |
|---|---|
| Open raw config | Copies or displays the local config path. |
| Log level | Trace, debug, info, warn, or error. |
| Hermes watch interval | How often Aplexica polls Hermes `state.db` for new or changed conversation sessions. Use a duration such as `5s`, `30s`, or `1m`. |
| Save | Writes the configuration patch. |

### Transport

Transport controls how Aplexica reaches beyond this machine when a remote
transport is configured.

Status fields:

| Field or control | Meaning |
|---|---|
| Mode | Current transport mode, such as local-only or bring-your-own relay. |
| Available transports | Modes supported by the current daemon and plugins. |
| Switch to local-only | Disables off-machine transport without removing local data. |

Bring-your-own relay fields:

| Field | Meaning |
|---|---|
| Broker URL | Relay broker URL. Must be a valid URL. |
| Client certificate path | Optional path to the mTLS client certificate. |
| Client key path | Optional path to the mTLS private key. |
| CA certificate path | Optional path to a certificate authority file. |
| Namespaces | Optional comma-separated namespace list. |
| Save BYO relay | Saves the relay configuration when the mode is supported. |

### Help

The Help page links to user documentation, source code, security reporting, and
a reminder that the UI can be reopened from the tray.

### Onboarding

The onboarding flow walks through initial local setup.

| Step | Meaning |
|---|---|
| Install daemon | Confirms the browser is connected to a running daemon. |
| Connect your agents | Waits for supported agents to be installed and detected. |
| Wait for your first sync | Waits for the first sync event. |
| Done | Links back to the Dashboard. |

## Troubleshooting

### The web UI says the session expired

The local UI session is tied to the daemon. Reopen the UI from the tray or run:

```bash
aplexica web open
```

### The tray says the daemon is unavailable

Check daemon status and logs:

```bash
aplexica status
aplexica daemon logs --follow
```

Restart if needed:

```bash
aplexica daemon restart
```

### An agent does not appear

Confirm the agent is installed and that Aplexica can discover its native roots:

```bash
aplexica adapters list
aplexica status
```

The daemon imports from installed agents even if fan-out to a specific agent is
disabled.

### A folder appears under Pending projects

Open the Pending projects page and approve the folder, or link it from the CLI:

```bash
aplexica pending list
aplexica project link <project-id> <local-path>
```

### An agent is missing older conversations

Expected. Enabling an agent back-fills only the most recent conversations into
it; everything after that syncs live. Push more history in with `aplexica
backfill` — see [Giving a Newly Enabled Agent Your History](#giving-a-newly-enabled-agent-your-history).

Note that a conversation is never materialized into the agent that authored it,
so an agent will not show you its own conversations arriving from elsewhere.

### A conversation shows the same turns twice

Check whether the duplicate is in the canonical head or only in one agent's
session file:

```bash
aplexica repair conversation <artifact-id>
```

That prints the proposed collapse without writing. If it reports nothing, the
canonical thread is clean and the duplicate is in a native session file that
will be rewritten on the next materialization.

If it does report a collapse, apply it on every device with all daemons
stopped, as described under [Repairing a Conversation](#repairing-a-conversation).
Repairing one device alone does not hold: peers still holding the duplicates
treat the repaired thread as a stale redelivery and re-serve their copy.

### A conversation stopped receiving updates

Canonical sync and the native session file are separate steps, and it is
usually the second one that is stuck. Check the retry queue:

```bash
aplexica repair materialization
```

An entry there means the daemon has the turns but could not write them into
that agent's session. The common cause is a session file that diverged from
canonical — Aplexica will not overwrite an agent's own session, so it defers.

Each entry names which way it diverged, and the two have different answers:

- `diverged` — the agent's **own** session holds turns canonical lacks while
  canonical holds turns it lacks. Run `aplexica repair conversation
  <artifact-id>` (a dry run; it prints what it would collapse).
- `mirror_diverged` — the copy Aplexica maintains for that agent holds a turn
  canonical has not imported yet. There is nothing to repair in canonical; the
  write clears itself once that turn is imported.
- `forked_mirror` — see "Repairing a Forked Claude Code Mirror" above.

Confirm canonical is actually current before assuming sync is at fault:

```bash
aplexica log <artifact-id>
```

If the turns are present in the log, sync is working and only the native write
is outstanding. Remote sync runs on a schedule rather than instantly, so a
recently authored turn may simply not have swept yet.

### A backup restore looks wrong

Native restores take a pre-restore snapshot before overwriting files. List
snapshots and restore the pre-restore snapshot if you need to undo:

```bash
aplexica backups list
aplexica restore-native --from <pre-restore-snapshot-id> --yes
```
