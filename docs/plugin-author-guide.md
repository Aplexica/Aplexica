# Writing an Aplexica Adapter Plugin (Go)

This guide walks you through building an **out-of-process adapter plugin**
for the Aplexica daemon using the Go SDK at
`github.com/aplexica/aplexica/pkg/adapterplugin`.

- For the exact wire contract, read
  [`plugin-protocol-spec.md`](plugin-protocol-spec.md) (normative).
- For a complete, working reference, read
  [`cmd/aplexica-plugin-example`](../cmd/aplexica-plugin-example) — a
  pure-translator `memory(markdown)` plugin for `MEMORY.md` files. Everything
  below mirrors that plugin.

---

## What an adapter plugin is

An adapter plugin is a **standalone executable** that the daemon spawns as a
subprocess and talks to over stdin/stdout. It teaches Aplexica how to
**import** a native agent file into the canonical format and **export** it
back out.

A plugin is a **PURE TRANSLATOR**:

- On **import**, you parse a native file and return typed payloads.
- On **export**, you render a payload back to a native file.

You never write a store, never mint IDs, never compute hashes. The **daemon**
does all of that. This is the single most important rule — see
[the pure-translator invariant](plugin-protocol-spec.md#1-roles-and-the-pure-translator-invariant).

> [!warning]
> Write **all** logging to **stderr**. stdout is the protocol channel; a
> stray `fmt.Println` corrupts it and the daemon drops your plugin.

---

## Step 1 — Implement `Handler`

Embed `adapterplugin.BaseCapabilitiesHandler` (it gives you a default
`Capabilities` method) and implement the other six methods. The full
interface:

```go
type Handler interface {
    Initialize(ctx, InitializeParams)    (InitializeResult, error)
    Import(ctx, ImportParams)            (ImportResult, error)
    Export(ctx, ExportParams)            (ExportResult, error)
    NativePath(ctx, NativePathParams)    (NativePathResult, error)
    HandlesFormat(ctx, HandlesFormatParams) (HandlesFormatResult, error)
    Capabilities(ctx, CapabilitiesParams) (CapabilitiesResult, error)  // from BaseCapabilitiesHandler
    Shutdown(ctx, ShutdownParams)        (ShutdownResult, error)
}
```

### Initialize

Return your identity and the kinds/formats you handle. `abi_version` **must**
be `adapterplugin.ABIVersion`.

```go
func (Handler) Initialize(_ context.Context, _ adapterplugin.InitializeParams) (adapterplugin.InitializeResult, error) {
    return adapterplugin.InitializeResult{
        PluginName:    "example-memory",
        PluginVersion: "0.1.0",
        ABIVersion:    adapterplugin.ABIVersion,
        Kinds:         []acf.Kind{acf.KindMemory},
        Formats:       map[acf.Kind][]string{acf.KindMemory: {"markdown"}},
    }, nil
}
```

### Import — the heart of a plugin

Decide whether the file is yours. If not, **return the unrecognized code**
so the orchestrator falls through to the next adapter — do **not** return an
empty result or a generic error:

```go
func (Handler) Import(_ context.Context, p adapterplugin.ImportParams) (adapterplugin.ImportResult, error) {
    base := filepath.Base(p.NativePath)
    if base != "MEMORY.md" {
        return adapterplugin.ImportResult{}, &adapterplugin.RPCError{
            Code:    adapterplugin.CodeUnrecognizedNativeFile,
            Message: "not a MEMORY.md file",
        }
    }
    content, err := os.ReadFile(p.NativePath)
    if err != nil {
        return adapterplugin.ImportResult{}, &adapterplugin.RPCError{
            Code: adapterplugin.CodeIOError, Message: err.Error(),
        }
    }
    payload, err := acf.EncodePayload(acf.MemoryPayload{
        Format:  "markdown",
        Content: string(content),
    })
    if err != nil {
        return adapterplugin.ImportResult{}, &adapterplugin.RPCError{
            Code: adapterplugin.CodeInternal, Message: err.Error(),
        }
    }
    return adapterplugin.ImportResult{
        Imports: []adapterplugin.ImportedItem{{
            Kind:       acf.KindMemory,
            Scope:      acf.ScopeGlobal,
            Name:       base,
            SourcePath: p.NativePath,
            Payload:    json.RawMessage(payload),
        }},
    }, nil   // NO store writes — the daemon reconciles + persists.
}
```

`SourcePath` is what lets the daemon recognize a re-import of the same file
and route it to the **same** artifact ID (producing an `update` event rather
than a new `create`).

### Export

You receive the **current** payload (the daemon already replayed the event
chain). Decode it and write the native file:

```go
func (Handler) Export(_ context.Context, p adapterplugin.ExportParams) (adapterplugin.ExportResult, error) {
    var mp acf.MemoryPayload
    if err := json.Unmarshal(p.Payload, &mp); err != nil {
        return adapterplugin.ExportResult{}, &adapterplugin.RPCError{
            Code: adapterplugin.CodeFormatUnsupported, Message: err.Error(),
        }
    }
    if err := os.WriteFile(p.DestPath, []byte(mp.Content), 0o644); err != nil {
        return adapterplugin.ExportResult{}, &adapterplugin.RPCError{
            Code: adapterplugin.CodeIOError, Message: err.Error(),
        }
    }
    return adapterplugin.ExportResult{Written: true}, nil
}
```

### NativePath / HandlesFormat / Capabilities

- `NativePath`: for a supported kind, return where the file goes
  (e.g. `filepath.Join(p.ContextDir, p.Artifact.Name)`) and `Supports: true`;
  otherwise `Supports: false`.
- `HandlesFormat`: return `true` only for the `(kind, format)` pairs you
  actually render (e.g. `kind == acf.KindMemory && format == "markdown"`).
- `Capabilities`: override the embedded default to advertise exactly your
  surface — set only the artifact kinds you produce, plus
  `NativeBasenames` / `BasenameToKind`.

### Shutdown

Usually empty: `return adapterplugin.ShutdownResult{}, nil`.

---

## Step 2 — `main`

Call `ServeStdio`. It wires the protocol to stdin/stdout and blocks until the
daemon closes the connection.

```go
package main

import (
    "context"
    "fmt"
    "os"

    "github.com/aplexica/aplexica/pkg/adapterplugin"
)

func main() {
    if err := adapterplugin.ServeStdio(context.Background(), &Handler{}); err != nil {
        fmt.Fprintln(os.Stderr, "plugin exited with error:", err)
        os.Exit(1)
    }
}
```

---

## Step 3 — Payload shapes

Build payloads from `github.com/aplexica/aplexica/internal/acf`. Encode with
`acf.EncodePayload(...)` (byte-identical to `json.Marshal`, which the
hash chain depends on). The four payload types:

| Kind                   | Type                     | Fields                                  |
|------------------------|--------------------------|-----------------------------------------|
| `acf.KindMemory`       | `acf.MemoryPayload`      | `Format`, `Content`                     |
| `acf.KindSkill`        | `acf.SkillPayload`       | `Format`, `Content`                     |
| `acf.KindTool`         | `acf.ToolPayload`        | `Format`, `Content` (secrets redacted)  |
| `acf.KindConversation` | `acf.ConversationPayload`| `Format`, `Content` or `Events`         |

Scopes: `acf.ScopeGlobal`, `acf.ScopeProject`, `acf.ScopeNamespace`.

> [!gap]
> **Import boundary.** `internal/acf` is import-restricted to this module, so
> a genuinely *external* third-party module cannot import it yet. For now,
> author your plugin **inside this module** (as the example does). Promoting
> the acf payload/kind types to a stable `pkg/` path is a planned follow-up.

---

## Step 4 — Write `plugin.json`

Place a `plugin.json` next to your executable. Required fields:
`manifest_version` (`1`), `name`, `version`, `abi_version` (`"1"`),
`executable`, and — for adapters — at least one `kinds` entry.

```json
{
  "manifest_version": 1,
  "name": "example-memory",
  "version": "0.1.0",
  "abi_version": "1",
  "executable": "aplexica-plugin-example",
  "kind": "adapter",
  "kinds": ["memory"],
  "formats": { "memory": ["markdown"] },
  "license": "AGPL-3.0-or-later"
}
```

Full schema + validation rules:
[plugin-protocol-spec.md §5](plugin-protocol-spec.md#5-manifest-pluginjson).

---

## Step 5 — Install the plugin

Each plugin lives in its **own subdirectory** under the daemon's plugins
directory, holding both the `plugin.json` and the executable:

```
<state-dir>/plugins/
└── example-memory/
    ├── plugin.json
    └── aplexica-plugin-example      # your built binary
```

The default plugins directory is `<state-dir>/plugins`
(`~/.aplexica/state/plugins`). Override it with the daemon flag
`--plugins-dir <path>`.

Build and install the example:

```sh
go build -o example-memory/aplexica-plugin-example ./cmd/aplexica-plugin-example
cp cmd/aplexica-plugin-example/plugin.json example-memory/plugin.json
mkdir -p ~/.aplexica/state/plugins/example-memory
cp example-memory/* ~/.aplexica/state/plugins/example-memory/
# restart the daemon
```

On restart you should see a log line like `adapter plugin loaded` with your
name and version.

---

## Discovery, disabling, and collisions

The daemon loads plugins **once at startup** (no hot reload — restart to pick
up changes). A discovered plugin is **silently skipped** (logged, never
fatal) when:

- **Name collision** — your `name` matches a built-in adapter
  (`claude-code`, `codex`, `kilo`, `hermes`, `openclaw`). Pick a unique name.
- **Disabled** — your `name` is in the daemon's disabled-adapters set.
- **Invalid manifest** — `plugin.json` fails validation.
- **Spawn failure** — the executable is missing, exits instantly, prints
  garbage on stdout, or never answers `initialize`.

None of these crash the daemon; absent a plugins directory, the daemon runs
exactly as it did before.

---

## Common pitfalls

- **Logging to stdout.** Corrupts the frame stream. Use stderr only.
- **Returning a generic error from `Import` for "not my file".** The
  orchestrator only falls through on `CodeUnrecognizedNativeFile` (`-32001`).
  Any other error is recorded as a real failure.
- **Writing a store / minting IDs.** You have no store handle by design. Just
  return `ImportResult` — the daemon reconciles and persists.
- **Pretty-printing JSON in payloads.** Use `acf.EncodePayload`; the hash
  chain depends on byte-exact, newline-free encoding.
- **Relying on secrets.** `ImportResult.Secrets` is currently dropped (no
  secrets store wired into the plugin path yet). Don't depend on it.

---

## Security note

The daemon **exec's any binary** in the plugins directory with the daemon's
privileges — there is no signing/allowlist/sandbox in this release. The
default location is under your own state directory, but only install plugins
you trust. See
[plugin-protocol-spec.md §7](plugin-protocol-spec.md#7-security-and-trust-known-limitation).

---

## Reference

- SDK package: `github.com/aplexica/aplexica/pkg/adapterplugin`
- Example plugin: [`cmd/aplexica-plugin-example`](../cmd/aplexica-plugin-example)
- Normative wire spec: [`plugin-protocol-spec.md`](plugin-protocol-spec.md)
