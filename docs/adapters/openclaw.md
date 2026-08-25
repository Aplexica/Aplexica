# Adapter spec: openclaw

**Adapter version:** 0.3.0 (as of v0.86.0)
**Native storage:** `~/.openclaw/` (per-user config, workspace, and agent sessions)
**Conformance status:** all 7 BRD-02 §5.4 categories passing

## Native filename → ACF kind mapping

| Native basename | ACF kind | Notes |
|---|---|---|
| `MEMORY.md` | `memory` | openclaw's project-memory form. |
| `AGENTS.md` | `memory` | AAIF cross-tool standard. |
| `CLAUDE.md` | `memory` | Read-only compatibility with claude-code-style memory files openclaw users may have inherited. |
| `DREAMS.md` | `memory` | openclaw-specific reflection-memory form. |
| Daily-note filenames (e.g. `2026-05-24.md`) | `memory` | openclaw's date-prefixed memory format. |
| `SKILL.md` | `skill` | Agent Skills Open Standard. |
| `openclaw.json`, `openclaw.jsonc`, `openclaw.json5` | `tool` | openclaw config. The MCP server section uses a NESTED schema: `{"mcp": {"servers": {...}}}` (not the flat `mcpServers`). JSONC and JSON5 variants support comments + trailing commas. |
| `*.jsonl` | `conversation` | openclaw session JSONL. |

## Tool kinds supported

| Kind | Status |
|---|---|
| `mcp-server` | Native (nested `mcp.servers` table) |
| `slash-command` | Native |
| `subagent` | M2+ |
| `hook` | M2+ |
| `plugin` | M2+ |

## Known fidelity gaps

- **Nested MCP schema**: openclaw's `{"mcp":{"servers":{}}}` shape is normalized through ACF and re-emitted as the same nested shape. Cross-adapter conversion to flat `mcpServers` happens via the per-adapter Export path.
- **JSONC / JSON5 → JSON re-serialize**: comments and trailing commas in user-edited config are lost on round-trip. The canonical store keeps the semantic value; Export produces standard JSON.
- **Daily notes**: the `isDailyNoteFilename` predicate matches date-shaped filenames; non-date `*.md` files in the project root are NOT auto-classified as memory (they fall through to the "unrecognized filename" error). Users with unusual naming should rename.

## Capabilities matrix

```json
{
  "name": "openclaw",
  "artifacts": {"memory": true, "skill": true, "tool": true, "conversation": true},
  "tools": ["mcp-server", "slash-command"],
  "nativeBasenames": [
    "MEMORY.md", "AGENTS.md", "CLAUDE.md", "DREAMS.md", "SKILL.md",
    "openclaw.json", "openclaw.jsonc", "openclaw.json5"
  ]
}
```

## Conformance results

All 7 §5.4 categories pass. Per-fixture round-trip results
documented in the per-test logs.
