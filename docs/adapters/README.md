# Per-agent adapter specs

Per FR-02.3: "Every V1 adapter MUST publish a per-agent spec document
describing native → ACF mappings, tool translations, and known
fidelity gaps." This directory holds one spec per V1 adapter.

The path here matches each adapter's `Capabilities().NotesURL` value,
so consumers of the JSON Schema (e.g. `aplexica config schema`) can
follow the link from machine-readable to human-readable docs.

| Adapter | Spec | Native storage |
|---|---|---|
| claude-code | [claude-code.md](claude-code.md) | `~/.claude/` |
| codex | [codex.md](codex.md) | `~/.codex/` |
| hermes | [hermes.md](hermes.md) | `~/.hermes/` |
| openclaw | [openclaw.md](openclaw.md) | `~/.openclaw/` |
| kilo | [kilo.md](kilo.md) | Kilo workspace + user config/data roots |

Each spec documents:

1. **Native filename → ACF kind mapping** — the Import dispatch table.
2. **Tool kinds supported** — which of {mcp-server, subagent, hook, slash-command, plugin} are native vs. M2+.
3. **Known fidelity gaps** — the deltas the user should expect on a round-trip.
4. **Capabilities matrix** — JSON snippet of the adapter's `Capabilities()` return.
5. **Conformance results** — summary of BRD-02 §5.4 categories.

The matrix is also queryable at runtime:

```sh
aplexica adapters list                # all 5 adapters
aplexica adapters check <name>        # in-process conformance subset
go test ./internal/adapter/<name>     # full BRD-02 §5.4 suite
```
