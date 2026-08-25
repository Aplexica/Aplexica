# Adapter spec: hermes

**Adapter version:** 0.2.0 (as of v0.86.0)
**Native storage:** `~/.hermes/` (per-user config + SQLite session store)
**Conformance status:** all 7 BRD-02 §5.4 categories passing

## Native filename → ACF kind mapping

| Native basename | ACF kind | Notes |
|---|---|---|
| `MEMORY.md` | `memory` | Hermes's primary memory form. |
| `USER.md` | `memory` | User-profile memory. Preserved on Export when `artifact.Name == "USER.md"`. |
| `AGENTS.md` | `memory` | AAIF cross-tool standard (since v0.78.0). NativePath returns AGENTS.md when `artifact.Name` matches; otherwise defaults to MEMORY.md. |
| `SKILL.md` | `skill` | Agent Skills Open Standard. |
| `config.yaml`, `hermes.yaml`, `hermes.yml` | `tool` | Hermes config YAML. The MCP server section uses `mcp_servers` (snake_case), not `mcpServers`. |
| `*.db` | `conversation` | SQLite-backed session store. `ImportConversationsFromDB(store, path, since=0)` reads ALL sessions; the daemon's `hermeswatch` package polls for incremental changes. |

## Tool kinds supported

| Kind | Status |
|---|---|
| `mcp-server` | Native (`mcp_servers` section of config.yaml) |
| `hook` | Native (hermes hook scripts) |
| `subagent` | M2+ |
| `slash-command` | M2+ |
| `plugin` | M2+ |

## Known fidelity gaps

- **SQLite conversation Import**: `ImportConversationsFromDB` mints one conversation artifact per session row at Import time. The daemon's `internal/hermeswatch` keeps the canonical store in sync via incremental polls (interval tunable via `daemon.hermeswatch_interval`).
- **YAML key normalization**: hermes uses snake_case (`mcp_servers`); cross-adapter conversion to other adapters' camelCase / nested-mcp schemas is handled by the per-adapter `Export` path. Hand-edited YAML formatting (extra blank lines, anchor references) is not preserved.
- **No conversation Export to fresh DB on stage-and-wait**: when a project-scope conversation artifact is staged via `aplexica project link`, hermes's `ExportConversationsToDB` requires an existing target DB (or it creates one); cross-device init of a fresh hermes install is M2 work.

## Capabilities matrix

```json
{
  "name": "hermes",
  "artifacts": {"memory": true, "skill": true, "tool": true, "conversation": true},
  "tools": ["mcp-server", "hook"],
  "nativeBasenames": [
    "MEMORY.md", "USER.md", "AGENTS.md", "SKILL.md",
    "config.yaml", "hermes.yaml", "hermes.yml"
  ]
}
```

## Conformance results

All 7 §5.4 categories pass. Conversation round-trip exercises the
`acf.hermes.session.v1` SessionBundleFormat decode/encode path.
