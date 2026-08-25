# Adapter spec: kilo

**Adapter version:** 0.1.0 (as of v0.86.0)
**Native storage:** Kilo Code workspace files plus Kilo user config/data roots
**Conformance status:** all 7 BRD-02 §5.4 categories passing, including canonical conversation import/materialization

## Native filename → ACF kind mapping

| Native basename | ACF kind | Notes |
|---|---|---|
| `AGENTS.md` | `memory` | AAIF cross-tool standard (kilo's primary memory form). |
| `AGENT.md` | `memory` | Singular variant some kilo workspaces use. |
| `.kilo/rules/*.md` / `.kilocode/rules*.md` | `memory` | Project rule files are imported as memory and exported back to the same rules-relative path. |
| `SKILL.md` | `skill` | Agent Skills Open Standard. Project skills live under `.kilo/skills/<name>/SKILL.md`; global skills live under `~/.kilo/skills/<name>/SKILL.md`. |
| `kilo.jsonc` | `tool` | Kilo's MCP server config. Project files live at `<project>/kilo.jsonc`; global config lives at `~/.config/kilo/kilo.jsonc`. JSONC supports comments. |
| `mcp.json` | `tool` | Legacy `.kilocode/mcp.json` MCP config. |
| `*.db` / (DB only) | `conversation` | Current Kilo session data lives in `kilo.db` under the user data root. The adapter imports sessions into canonical `acf.conversation.v1` artifacts. `*.jsonl` imports are explicitly rejected because Kilo does not use per-conversation JSONL logs. |

## Tool kinds supported

| Kind | Status |
|---|---|
| `mcp-server` | Native (`mcp` table in `kilo.jsonc`; legacy `.kilocode/mcp.json` accepted on import) |
| `subagent` | Kilo's "modes" model is conceptually related but NOT 1:1 mapped to ACF subagent yet (M2+ work; the kilo-modes vocabulary needs a richer mapping than the V1 subagent kind exposes). |
| `hook` | M2+ |
| `slash-command` | M2+ |
| `plugin` | M2+ |

## Known fidelity gaps

- **Conversation materialization path**: Kilo does not expose a stable direct-write API for `kilo.db`, so Aplexica does not write the database directly. Instead, the adapter imports Kilo-owned sessions from `kilo.db`, converts them to canonical `acf.conversation.v1` events, and materializes canonical conversations back into Kilo through the version-stable `kilo export` / `kilo import` interchange format when the `kilo` CLI is available. Generic file-style `Export()` remains not applicable for conversations; fan-out uses the higher-fidelity `ConversationSessionTarget` path.
- **Internal checkpoints**: Kilo `reasoning`, `step-start`, `step-finish`, `compaction`, and unknown part types are preserved as canonical `system_note` events tagged `internal-checkpoint` plus a `kilo:*` tag. User/assistant text remains `turn`; tool parts become `tool_call` / `tool_result`.
- **Modes / custom-instructions**: kilo's "modes" feature isn't currently mapped to any ACF tool kind. M2 work tracks adding a custom `kilo-mode` tool kind (or extending the existing `subagent` kind with a `mode` capability flag) once the kilo-modes vocabulary stabilizes.
- **JSONC parsing**: comments in `kilo.jsonc` are stripped during Import; Export produces standard JSON. Future versions may preserve comments via a JSONC-preserving codec.
- **Project discovery and conversation scope**: the adapter harvests project directories from the current Kilo session DB at `<XDG_DATA_HOME>/kilo/kilo.db` (Linux), `~/Library/Application Support/kilo/kilo.db` (macOS), or `~/.local/share/kilo/kilo.db`. Imported conversations are project-scoped only when the Kilo session `directory` resolves to a VCS-backed project; otherwise they remain global.

## Capabilities matrix

```json
{
  "name": "kilo",
  "artifacts": {"memory": true, "skill": true, "tool": true, "conversation": true},
  "tools": ["mcp-server"],
  "nativeBasenames": ["AGENTS.md", "AGENT.md", "SKILL.md", "kilo.jsonc", "mcp.json"]
}
```

## Conformance results

All applicable §5.4 categories pass. Kilo conversation conversion is canonical:
Kilo sessions import from `kilo.db` as `acf.conversation.v1`, and canonical
conversation heads materialize into Kilo through `kilo import` when the Kilo CLI
is present. `*.jsonl` remains unsupported because Kilo's native conversation
surface is DB/interchange-based rather than per-conversation JSONL files.
