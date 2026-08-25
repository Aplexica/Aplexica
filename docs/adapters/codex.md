# Adapter spec: codex

**Adapter version:** 0.5.0
**Surfaces:** Codex CLI + native Codex desktop app (including its bundled ChatGPT host)
**Native storage:** shared default `~/.codex/` + `$HOME/.agents/skills`
**Per BRD-02 §6:** primary AGENTS.md producer
**Conformance status:** all 7 BRD-02 §5.4 categories passing

Codex CLI and the native desktop app use the same Codex host, configuration,
rollout store, and `CODEX_HOME`. They are two surfaces of one `codex` adapter;
creating a second adapter over the same roots would duplicate imports and race
on files. OpenAI documents the shared skill surface and current locations in
[Build skills](https://learn.chatgpt.com/docs/build-skills), and the integration
protocol in [Codex App Server](https://learn.chatgpt.com/docs/app-server).

CLI and Desktop are independently optional. A CLI-only installation never
launches app-server just because the CLI binary provides that subcommand; a
Desktop-only installation is discovered from its bundled Codex host and shared
storage. Candidate roots and surface probes are rechecked while the daemon is
running, so installing either surface later activates synchronization and
backfills existing context. Conversation history uses the configured bounded
historical depth for source-less peer-device history rather than replaying the
entire store or duplicating locally authored sessions. A newly discovered
native root passes the normal pre-sync safety snapshot gate before its first
import or outbound write.

## Desktop integration

- Shared memories, user MCP configuration, auth, and rollouts continue to use
  the default `~/.codex`. Project MCP configuration materializes to the
  supported `.codex/config.toml` location. Chromium/Electron application data
  is UI state and is not synchronized.
- After Aplexica writes a synchronized rollout, it asks a short-lived
  `codex app-server` process to `thread/resume` the deterministic thread ID.
  Codex itself loads the thread through its documented protocol, allowing its
  normal inventory to discover it. Aplexica never opens or writes Codex's
  SQLite DB.
- Each canonical thread and branch has exactly one deterministic rollout.
  Aplexica appends only after verifying the complete byte/inode snapshot. If
  Codex writes concurrently or the rollout is locally ahead, synchronization
  waits for the watcher import and retries that same pathname; it never creates
  a remote or recovery conversation. Legacy generated siblings are moved to
  `~/.aplexica/quarantine/codex-conversations/` after reconciliation.
- Registration is bounded and best-effort. If a bundled/standalone app-server
  is absent or incompatible, the rollout remains successfully materialized and
  resumable from the CLI. `codex app-server` is currently experimental, so the
  adapter deliberately treats registration as version-sensitive fallback work.
- Current Codex skills live at `$HOME/.agents/skills/<name>/SKILL.md` and
  project `.agents/skills/<name>/SKILL.md`, both consumed by CLI and Desktop.
  Legacy `~/.codex/skills` is still watched as a migration input but is no
  longer an outbound destination.
- On Windows, native Codex and the desktop app share
  `%USERPROFILE%\.codex`. A CLI inside WSL has a separate Linux home unless
  `CODEX_HOME` is explicitly pointed at the Windows directory. The adapter
  recognizes the Store/MSIX package and the app's per-user
  `%LOCALAPPDATA%\OpenAI\Codex\bin` or package-cache app-server copy, including
  a single version/hash directory beneath `bin`.

## Native filename → ACF kind mapping

| Native basename | ACF kind | Notes |
|---|---|---|
| `AGENTS.md` | `memory` | Codex's primary memory form (AAIF). `NativePath` for memory artifacts writes AGENTS.md. |
| `SKILL.md` | `skill` | Agent Skills Open Standard under user/project `.agents/skills/<name>/`. |
| `config.toml` | `tool` | Codex MCP server configs: user `~/.codex/config.toml` or trusted-project `.codex/config.toml`. Different from claude-code's `.mcp.json`. |
| `*.jsonl` (session logs) | `conversation` | Codex session format (per-event JSONL). v0.16.0 supports `--canonical` for cross-agent format. |

## Tool kinds supported

| Kind | Status |
|---|---|
| `mcp-server` | Native (TOML `mcp_servers` table) |
| `subagent` | Not yet; no adapter import/export mapping |
| `slash-command` | Not yet; no adapter import/export mapping |
| `hook` | Not yet |
| `plugin` | Not yet |

## Known fidelity gaps

- **TOML preservation**: comments and key ordering in user-edited `*.toml` files are not preserved through ACF round-trip. The canonical store holds the parsed semantic value; Export re-serializes via the toml library's default formatter. Operators who hand-format their codex config should expect cosmetic-only changes on round-trip.
- **No AAIF.md output for project-scope memory**: codex's NativePath always writes AGENTS.md (Codex IS the AAIF producer for memory). On Import from claude-code's `CLAUDE.md`, the adapter does NOT support that filename — claude-code's CLAUDE.md must first be converted to AGENTS.md via the orchestrator's cross-adapter fan-out.
- **Conversation tool-call metadata**: cross-agent conversation conversion (`acf.conversation.v1` format) normalizes tool-call shape; codex-specific tool-call metadata (e.g. token counts) is dropped.
- **Desktop indexing fallback:** app-server registration errors do not fail the
  durable rollout write. This preserves CLI availability even when the desktop
  runtime is missing or being upgraded.
- **Custom `CODEX_HOME`:** v0.5.0 targets the supported default `~/.codex`
  location. A daemon launched with a custom `CODEX_HOME` is not yet remapped by
  the adapter.

## Capabilities matrix

```json
{
  "name": "codex",
  "surfaces": ["cli", "desktop"],
  "artifacts": {"memory": true, "skill": true, "tool": true, "conversation": true},
  "tools": ["mcp-server"],
  "nativeBasenames": ["AGENTS.md", "SKILL.md", "config.toml"]
}
```

## Conformance results

All 7 BRD-02 §5.4 categories pass. Codex is the alphabetically-first
primary claimant for AGENTS.md, so cross-adapter fan-out tests
(e.g. `TestOrchestrator_Memory_FansOutFromCodex`) drive Import via
this adapter.
