# Quickstart — 10-minute walkthrough

This guide takes you from a fresh install to working two-agent sync. It assumes you already have at least one supported AI coding agent installed locally (Claude Code, Codex, Hermes, OpenClaw, or Kilo Code).

## 1. Install

Pick your platform:

Homebrew has not yet been advanced to the verified binary formula. On macOS and
Linux, install the matching release archive per
[Manual install from a verified archive](install/direct.md). On Windows, follow
[Install on Windows](install/windows.md): download the v1.0.74 `.zip`, unzip,
and run the executables.

```bash
# Debian / Ubuntu — after following install/apt.md for an exact version/arch
sudo apt install ./aplexica_X.Y.Z_amd64.deb
```

To authenticate what you downloaded, verify the release's `SHA256SUMS` with
`cosign verify-blob` and check your artifact against it — see
[Verify a release](install/verify.md).

Verify:

```bash
aplexica --version
```

## 2. Run the setup wizard

```bash
aplexica setup
```

The wizard asks four Y/n questions (the fourth appears only when the web UI is
enabled):

1. **Enable the system tray app?** (recommended on desktop)
2. **Enable the local web UI?** (recommended — it's how non-CLI features are surfaced)
3. **Install and start Aplexica now?** (defaults to yes)
4. **Open the web UI now?** (launches your default browser at the loopback URL)

Your answers are written to `~/.aplexica/state/config.json`. You can change
them later with `aplexica config set`.

If you accept the default install prompt, the wizard installs the daemon's
user-level service (launchd on macOS, systemd-user on Linux, Task Scheduler on
Windows) so it starts at login. Confirm:

```bash
aplexica status
```

A healthy result reports `Daemon: running`, followed by the current agent
detection state and conflict summary. Process IDs, paths, timestamps, store
pressure, and artifact counts vary by machine.

## 3. Let your agents create state

Use your AI coding agent normally for a few minutes — write a memory, install a skill, have a conversation. Aplexica's daemon watches each adapter's native location (e.g. `~/.claude/CLAUDE.md`, `~/.codex/AGENTS.md`) and translates new state into the canonical store within ~2 seconds.

After a short session:

```bash
aplexica list
```

Once artifacts have been imported, the table has `ID`, `KIND`, `NAME`,
`SCOPE`, `SOURCE`, and `CREATED` columns. Copy an ID from the first column for
the examples below. If nothing has been imported yet, the command reports
that no artifacts match.

## 4. Watch fan-out happen

Aplexica discovers your installed agents and imports from them right away, but
it does not write to any of them until you say so. That is deliberate — call it
"discover, then await config." Fan-out requires both a matching routing rule
and an enabled destination.

Create a file named `quickstart-codex-rule.toml` with this rule:

```toml
[[sync.rules]]
name = "quickstart-to-codex"
match.kind = "any"
match.type = ["memory", "skill", "tool", "conversation"]
route.agents = ["codex"]
```

Then add the rule, enable Codex, and reload the daemon:

```bash
aplexica rules add quickstart-codex-rule.toml
aplexica sync enable codex
aplexica daemon reload
```

`rules add` copies the validated rule into `~/.aplexica/rules.toml`, so you can
delete the temporary TOML file afterward. Confirm the policy against an ID
from `aplexica list`:

```bash
aplexica rules test <artifact-id>
```

The explanation should name `quickstart-to-codex` as a matched rule and list
`codex` as an allowed agent. New matching changes can now fan out to Codex.

Conversations fan out too, but a newly enabled agent receives only your most
recent ones — not the whole archive. Everything after that syncs live. Use
`aplexica backfill` when you want an agent to have more history.

The whole fan-out is **deterministic and lossless** — no summarization layer. The second agent sees the same memory the first one wrote, in the format it expects.

## 5. Browse the local web UI

If you enabled the web UI during setup, click the tray icon → **Open Aplexica**, or run:

```bash
aplexica web open
```

The dashboard shows:

- **Agents** — which adapters are detected and active.
- **Events** — the last few imports, fan-outs, conflicts, and rule firings, with a live stream.
- **Conflicts** — divergent writes that need your decision.
- **Pending projects** — directories Aplexica noticed but hasn't been told to manage yet.
- **Rules** — the routing rules deciding what fans out where.

## 6. Make a backup

```bash
aplexica backup ~/aplexica-backup.tar.gz --unsigned
aplexica restore ~/aplexica-backup.tar.gz --peek --unsigned-ok
```

`backup` exports the whole canonical store to a portable bundle; `restore`
reads one back. `--peek` prints what a bundle contains without writing, and
`--dry-run` classifies every artifact as would-add or already-exists. Bundles
can also be encrypted (`--encrypt`) and scrubbed of PII (`--anonymize`). This
walkthrough explicitly creates an unsigned bundle and acknowledges that fact
when reading it, so it does not create or use a signing key.

Signed backups are the default when `--unsigned` is omitted. To inspect or
restore one, retain its public key and the full key ID printed by `backup`,
then pass them with `--pubkey` and `--key-id`.

Two other things are also called snapshots, and neither is this one:
`aplexica snapshot <artifact-id>` adds a replay checkpoint to a single
artifact's event log, and `aplexica backups list` shows the automatic snapshots
of agent-owned files taken before a destructive native restore. See
[docs/01-brd-backup-restore.md](01-brd-backup-restore.md) for the full surface.

## 7. Fork a conversation across agents

Power-user move: take a conversation started in Claude Code, branch it, and continue in Codex.

```bash
# Find the conversation artifact ID
aplexica list --kind conversation

# Copy the ID from the ID column, then choose an event from its log.
aplexica log <conversation-id>

# Fork it into a Codex branch, diverging at the event you choose
aplexica fork <conversation-id> --from <event-id> --to-agent codex --branch codex-experiment

# Now both branches exist independently
aplexica log --graph <conversation-id>
```

The graph now shows the original branch and `codex-experiment` independently.

You can keep writing in Codex on the new branch; the original Claude Code conversation is untouched. If you later want to merge insights back:

```bash
aplexica merge <conversation-id> --from codex-experiment --into main
# Conflicts? Resolve with `aplexica conflicts list` and the web UI's side-by-side view.
```

See [docs/04-brd-forking-and-merging.md](04-brd-forking-and-merging.md) for the complete branch model.

## 8. Selective sync

Tags are routing metadata; adding a tag does not change routing unless an
active rule matches it. There is no always-on private rule. The broad
`quickstart-to-codex` rule above would continue to allow Codex even after an
artifact is tagged `private`.

To replace that broad rule with an origin-only rule for private artifacts,
create `private-to-origin.toml` with:

```toml
[[sync.rules]]
name = "private-to-origin"
match.kind = "all"
match.tag = ["private"]
route.agents = ["__originatingAgent__"]
route.remote = "exclude"
```

Replace the broad rule and reload before applying the tag; otherwise the
running daemon could still route the tag change under the old policy:

```bash
aplexica rules add private-to-origin.toml
aplexica rules remove quickstart-to-codex
aplexica daemon reload
aplexica tag add <artifact-id> private
aplexica rules test <artifact-id>
```

The explanation should name `private-to-origin`, allow only the artifact's
originating agent, and report `remote-allowed: false`. With the broad rule
removed, untagged artifacts match no rule and fan out nowhere. This changes
subsequent routing decisions; it does not retract a copy already delivered
under the earlier broad rule.

You can also write per-agent rules — see [docs/05-brd-selective-sync-and-routing.md](05-brd-selective-sync-and-routing.md).

## What's next?

- Read the [vision](00-vision.md) if you want to understand the architecture decisions.
- Read the [BRDs](.) (`01-` through `05-`) for the full feature spec.
- For per-agent specifics (file paths, supported artifact kinds, version compatibility), see [docs/adapters/](adapters/).
- Run into a bug? [File one](https://github.com/Aplexica/Aplexica/issues/new/choose).

## Common follow-ups

| Task | Command |
|---|---|
| Inspect an artifact | `aplexica show <artifact-id>` |
| See which agents receive data | `aplexica sync agents` |
| Let an agent receive data | `aplexica rules add <file>`, `aplexica sync enable <agent>`, then `aplexica daemon reload` |
| Pause sync | `aplexica sync pause` (or tray → Pause) |
| Resume | `aplexica sync resume` |
| Give an agent more history | `aplexica backfill` (dry run), then `--apply` |
| Tail the log | `aplexica daemon logs --follow` |
| Disable the web UI | `aplexica web disable` |
| Update Aplexica | Use the same channel that installed it; see [install/update.md](install/update.md) |
| Reset to defaults | Back up `~/.aplexica/`, move it aside, then run `aplexica setup` |
| Run the diagnostic report | `aplexica doctor` |

For a complete CLI reference, run `aplexica help`, or `aplexica <command> --help`
for any command's flags. The [user guide](user-guide.md) explains what each
command group is for.
