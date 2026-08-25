# Adapter spec: claude-code

**Adapter version:** 0.8.0
**Surfaces:** Claude Code CLI + Claude Code Desktop (Code tab)
**Native storage:** shared `~/.claude/` state + project files; Desktop session catalog is read-only
**Per BRD-02 §6.1:** primary AAIF AGENTS.md consumer
**Conformance status:** all 7 BRD-02 §5.4 categories passing

Claude Code Desktop embeds the same Claude Code engine as the CLI. It is
therefore a second **surface** of the `claude-code` adapter, not a second
adapter identity. Registering both against `~/.claude` would double-import the
same files and race on materialization. Anthropic documents the shared engine,
configuration, default worktree layout, separate histories, and `/desktop`
handoff in its [Desktop reference](https://code.claude.com/docs/en/desktop).

The two surfaces are optional and independently detected. A CLI-only install
keeps the existing adapter behavior; a Desktop-only install uses the same
logical adapter without requiring the standalone CLI. Candidate native roots
and the Desktop topology token are polled at runtime, so installing either
surface later activates it and backfills existing context. Conversations use
the configured bounded historical depth for source-less peer-device history;
local sessions already present in shared storage are not synthesized again. A
newly discovered native root passes the normal pre-sync safety snapshot gate
before its first import or outbound write.

## Desktop integration

- CLI and Desktop consume the same user/project `CLAUDE.md`, settings, MCP,
  plugin, hook, and skill files. Aplexica synchronizes the artifact types it
  implements today: memory, skills, MCP configuration, and conversations. It
  does not claim adapter mappings for settings, hooks, or plugins.
- Desktop creates isolated Git worktrees under
  `<project>/.claude/worktrees/`. Aplexica reads Desktop's session catalog to
  map those sessions back to `originCwd`, then mirrors project memory and
  skills into verified **active** worktrees. A catalog entry
  cannot redirect writes elsewhere: the target must be an existing linked Git
  worktree below the documented project-local root.
- On macOS the read-only catalog is under
  `~/Library/Application Support/Claude/claude-code-sessions/`. The equivalent
  MSIX-virtualized `LocalCache/Roaming/Claude/claude-code-sessions` location is
  preferred on Windows, with the legacy per-user roaming path retained;
  Linux beta uses compatible XDG config candidates. Aplexica does not watch,
  back up, or synthesize those app-owned records.
- Desktop-authored local sessions still use ordinary Claude Code JSONL
  transcripts under `~/.claude/projects/`, so the existing conversation
  importer captures them.
- Anthropic intentionally keeps CLI and Desktop session lists separate. There
  is no supported non-interactive API for creating a Desktop sidebar record.
  A synchronized conversation remains resumable with
  `claude --resume <artifact-id>`; from that terminal session, `/desktop` is
  Anthropic's supported handoff into the app. Aplexica does not write the
  private Desktop catalog schema.

`CLAUDE.md` is read when a session starts. Start a new Desktop session when a
global instruction changes during an already-running session. Project mirrors
make the updated file available to the running worktree, but do not rewrite the
model's existing context window.

## Continuing a Claude-origin conversation in another agent

Claude Code does not expose a supported API for inserting turns into an
already-running terminal session, and it does not participate in Aplexica's
file lock/merge protocol. Aplexica nevertheless preserves one conversation
entry: when another agent adds turns to a Claude-origin conversation, the
adapter appends them to the original Claude JSONL and its existing
`parentUuid` chain. The append is allowed only when the current visible chain
is an exact prefix of the canonical conversation and two byte/inode checks
prove that Claude did not write concurrently.

If Claude writes during that validation, or its local chain is ahead or
divergent, Aplexica leaves the file untouched and waits for the watcher to
import that native change before retrying. It never creates a “continuation”
or recovery session for the same canonical thread. Legacy Aplexica-generated
duplicates carrying the same authenticated thread marker are moved to the
recoverable `~/.aplexica/quarantine/claude-conversations/` area after a
successful reconciliation, so they no longer appear in `/resume`.

An already-open Claude terminal cannot redraw itself to show externally
appended turns. Exit to the resume picker and reopen the same entry to see the
updated history; no second conversation should appear.

## Native filename → ACF kind mapping

| Native basename | ACF kind | Notes |
|---|---|---|
| `CLAUDE.md` | `memory` | Claude Code's primary memory form. `NativePath` always writes this filename for memory artifacts (i.e. AGENTS.md is read but Export emits CLAUDE.md). |
| `AGENTS.md` | `memory` | AAIF cross-tool standard. Read since v0.78.0; written as CLAUDE.md (Claude Code's primary form). |
| `SKILL.md` | `skill` | Agent Skills Open Standard. User skills live under `~/.claude/skills/<name>/`; project skills under `.claude/skills/<name>/`. |
| `.mcp.json` | `tool` | Per-project MCP server config. Round-trips byte-equal for the standard `{"mcpServers": {...}}` shape. Secrets in env blocks are externalized to `~/.aplexica/secrets/<server>.<envKey>` and replaced with `${secret:…}` placeholders (ADR-0027 secret isolation). |
| `~/.claude.json` | `tool` | User-scope MCP configuration shared by CLI and Desktop Code sessions. |
| `*.jsonl` (session logs) | `conversation` | Claude Code's shared transcript format. `--canonical` encodes as `acf.conversation.v1`. |

## Tool kinds supported

| Kind | Status | Native form |
|---|---|---|
| `mcp-server` | Native | `.mcp.json` `mcpServers` entries. |
| `subagent` | Not yet | Native Claude feature, but the adapter has no import/export mapping yet. |
| `hook` | Not yet | Native Claude feature, but the adapter has no import/export mapping yet. |
| `slash-command` | Not yet | Native Claude feature, but the adapter has no import/export mapping yet. |
| `plugin` | Not yet | Native Claude feature, but the adapter has no import/export mapping yet. |

## Known fidelity gaps

- **Tool secret-externalization**: when an inbound `.mcp.json` carries inline env-block secrets matching the regex catalog in `internal/mcp/secrets.go`, the adapter externalizes them to the secrets store + emits a warning. The user is responsible for re-supplying the secret via `aplexica secret set` if they want it to persist; otherwise the placeholder remains in the canonical artifact.
- **Conversation format**: default Import preserves Claude Code's native JSONL shape (lossless re-export). The `--canonical` flag converts to `acf.conversation.v1` which IS lossy back to Claude Code's native (some tool-call metadata is dropped); use the default for round-trip.
- **AGENTS.md → CLAUDE.md rename on Export**: this is intentional. Claude Code's primary memory form is CLAUDE.md; the AGENTS.md filename is honored on Import (AAIF compatibility) but Export materializes to the primary form. If the user wants the round-tripped file to keep its AGENTS.md name, they MUST use `aplexica export` directly with an `AGENTS.md` destination path.
- **Desktop conversation sidebar:** outbound conversation materialization writes
  a native CLI-resumable transcript. Use Claude Code's `/desktop` handoff to
  move it into Desktop; private catalog writes are deliberately unsupported.
- **Custom Desktop worktree roots:** v0.8.0 mirrors only the documented
  default `<project>/.claude/worktrees/` layout. Custom external worktree roots
  receive no live mirror; use the default location or synchronize the custom
  worktree separately.
- **Project MCP in linked worktrees:** user-scope MCP in `~/.claude.json` is
  shared immediately. Aplexica does not copy secret-expanded project
  `.mcp.json` into linked worktrees because its safe tracked-file check cannot
  treat a linked-worktree `.git` pointer as an ordinary local index.

## Capabilities matrix

```json
{
  "name": "claude-code",
  "surfaces": ["cli", "desktop"],
  "artifacts": {"memory": true, "skill": true, "tool": true, "conversation": true},
  "tools": ["mcp-server"],
  "nativeBasenames": ["CLAUDE.md", "AGENTS.md", "SKILL.md", ".mcp.json", ".claude.json"]
}
```

## Conformance results

`go test ./internal/adapter/claudecode -run TestConformance -count=1`:

- **round-trip**: AGENTS.md / SKILL.md / CLAUDE.md / `.mcp.json` all pass.
- **idempotency**: second Export matches first for every fixture.
- **watch-correctness**: write-twice produces `[create, update]` event types.
- **capability-declaration**: every advertised basename Imports.
- **performance-scan**: passes the documented conformance target.
- **cross-conversion**: passes for every (source, target) pair against the universal AGENTS.md fixture.
- **recursion-guard**: 5-adapter test passes.
