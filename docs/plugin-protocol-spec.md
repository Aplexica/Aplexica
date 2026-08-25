# Aplexica Adapter Plugin Protocol — Normative Specification

**Status:** stable · **ABI version:** `1` · **Scope:** out-of-process adapter plugins

This document is the **normative** wire contract between the Aplexica daemon
(the *host*) and an external **adapter plugin** (a standalone executable). It
is accurate to the reference implementation in
`internal/plugin/{proto,host,proxy}`. If the prose here ever disagrees with
that code, the code wins — file a bug.

For a step-by-step Go authoring walkthrough, see
[`plugin-author-guide.md`](plugin-author-guide.md). The reference plugin is
[`cmd/aplexica-plugin-example`](../cmd/aplexica-plugin-example).

The key words **MUST**, **MUST NOT**, **SHOULD**, and **MAY** are used as in
RFC 2119.

---

## 1. Roles and the pure-translator invariant

There are exactly two roles:

- **Host (daemon).** Spawns the plugin as a subprocess, drives the JSON-RPC
  conversation, and **owns all store I/O**. The host mints event IDs,
  performs identity reconciliation (re-imports of the same source path
  resolve to the same artifact ID), computes hash-chain `parent_hash`
  values, and writes artifacts + events to the canonical store.

- **Plugin (adapter).** A **PURE TRANSLATOR**. On `import` it parses a native
  file into one or more typed payloads and returns them. On `export` it
  renders a payload back to a native file. **The plugin never touches a
  store, never mints IDs, and never computes hashes.**

> [!key-insight]
> The single most important invariant: `import` returns an
> `ImportResult{Imports: [...]}` describing what was parsed; the **daemon**
> then reconciles and persists each item. A plugin that tries to write a
> store is wrong by construction — it has no store handle.

This is what lets the daemon keep the canonical event log consistent across
many adapters (built-in and plugin) without trusting any single plugin to
get the hash chain right.

---

## 2. Transport

### 2.1 Channels

The host spawns the plugin executable and connects:

- the plugin's **stdin** to the host's request writer, and
- the plugin's **stdout** to the host's response reader.

The plugin's **stderr** is routed to the daemon log. A plugin **MAY** write
free-form diagnostics to stderr.

> [!warning]
> A plugin **MUST** write protocol frames to **stdout only**, and **MUST
> NOT** write anything else to stdout. A stray `fmt.Println` (which goes to
> stdout) corrupts the frame stream and the daemon will drop the plugin. All
> logging goes to **stderr**.

### 2.2 Framing

Messages are exchanged as **newline-delimited compact JSON frames**:

- Each frame is one JSON value followed by a single `\n` (`0x0A`).
- A frame **MUST NOT** contain any literal newline inside it — compact JSON
  only (no pretty-printing). Encoders like Go's `json.Marshal` already emit
  newline-free output.
- The maximum frame size is **64 MiB** (`64 * 1024 * 1024` bytes). A frame
  larger than this fails the reader with a "token too long" error, which the
  host surfaces as a transport failure and the plugin is dropped.

(Reference: `internal/plugin/proto/transport.go` — `FrameReader` /
`FrameWriter`, scanner buffer capped at 64 MiB.)

---

## 3. Message envelope (JSON-RPC 2.0)

Every frame is a JSON-RPC 2.0 **request** (host → plugin) or **response**
(plugin → host).

### 3.1 Request

```json
{ "jsonrpc": "2.0", "id": 1, "method": "initialize", "params": { ... } }
```

| Field     | Type            | Notes                                                            |
|-----------|-----------------|------------------------------------------------------------------|
| `jsonrpc` | string          | Always `"2.0"`.                                                  |
| `id`      | number/string   | Opaque correlation ID. Echoed back verbatim in the response.    |
| `method`  | string          | One of the seven method names in §4.                            |
| `params`  | object          | Method-specific; omitted/`{}` for parameterless methods.        |

### 3.2 Response (success)

```json
{ "jsonrpc": "2.0", "id": 1, "result": { ... } }
```

### 3.3 Response (error)

```json
{ "jsonrpc": "2.0", "id": 1, "error": { "code": -32001, "message": "..." } }
```

A response carries **exactly one** of `result` or `error`. The `error`
object's optional `data` field (raw JSON) is reserved and currently unused.

The conversation is strictly **synchronous and serial**: the host sends one
request and reads exactly one response before sending the next. There are no
notifications and no plugin-initiated requests in ABI `1`.

(Reference: `internal/plugin/proto/rpc.go`.)

---

## 4. Methods

There are exactly **seven** methods. Each maps one-to-one to a `Handler`
method in the SDK.

| Method           | Direction      | Purpose                                            |
|------------------|----------------|----------------------------------------------------|
| `initialize`     | host → plugin  | Handshake: exchange ABI + declare kinds/formats.   |
| `import`         | host → plugin  | Translate a native file → ACF payloads.            |
| `export`         | host → plugin  | Render a payload → native file.                    |
| `native_path`    | host → plugin  | Where does this artifact live natively?            |
| `handles_format` | host → plugin  | Can you render this `(kind, format)`?              |
| `capabilities`   | host → plugin  | Declare the adapter's full surface.                |
| `shutdown`       | host → plugin  | Reserved graceful-stop method (see §4.7).           |

### 4.1 `initialize`

First call after spawn. The host advertises its ABI/version; the plugin
replies with identity and declared kinds/formats. The host **MUST** verify
`abi_version` matches `"1"` exactly and drops the plugin otherwise.

**Params** (`InitializeParams`):

| Field            | Type   | Notes                          |
|------------------|--------|--------------------------------|
| `abi_version`    | string | The host's ABI. Always `"1"`.  |
| `daemon_version` | string | Host build version (info only).|
| `device_id`      | string | May be empty in this release.  |

**Result** (`InitializeResult`):

| Field            | Type                  | Notes                                                  |
|------------------|-----------------------|--------------------------------------------------------|
| `plugin_name`    | string                | Unique adapter name. MUST match `manifest.name`.       |
| `plugin_version` | string                | Plugin build version.                                  |
| `abi_version`    | string                | MUST be `"1"`.                                          |
| `kinds`          | array of kind strings | ACF kinds handled (e.g. `["memory"]`). Non-empty.      |
| `formats`        | object kind→[strings] | Optional; per-kind format list (e.g. `{"memory":["markdown"]}`). |

### 4.2 `import`

The host asks the plugin to translate one native file.

**Params** (`ImportParams`):

| Field         | Type   | Notes                                                  |
|---------------|--------|--------------------------------------------------------|
| `native_path` | string | Absolute path to the native file to import.            |
| `context_dir` | string | The project/context directory the import is scoped to. |
| `caused_by`   | string | Optional upstream event ID for provenance.             |

**Result** (`ImportResult`):

| Field     | Type                     | Notes                                       |
|-----------|--------------------------|---------------------------------------------|
| `imports` | array of `ImportedItem`  | One per artifact parsed. May be empty.      |
| `secrets` | array of `NamedSecret`   | Optional. **See note below — dropped today.**|

`ImportedItem`:

| Field         | Type        | Notes                                                            |
|---------------|-------------|------------------------------------------------------------------|
| `kind`        | kind string | ACF kind, e.g. `"memory"`.                                       |
| `scope`       | scope string| `"global"`, `"project"`, or `"namespace"`.                      |
| `name`        | string      | Human label (typically the basename).                           |
| `source_path` | string      | The native path this item came from (drives re-import identity).|
| `payload`     | object      | Kind-typed payload JSON (e.g. an `acf.MemoryPayload`).          |

`NamedSecret`: `{ "name": string, "value": string }`.

> [!gap]
> **Secrets are dropped in this release.** The proxy currently discards
> `ImportResult.secrets` (no secrets store is wired into the plugin path
> yet). A plugin **SHOULD NOT** rely on secret persistence; the example
> plugin emits none. Wiring a secrets store is a planned follow-up.

**Fall-through:** if the file is *not* this plugin's, the plugin **MUST**
return an error with code `-32001`
(`CodeUnrecognizedNativeFile`) — **not** an empty `imports` and **not** a
generic error. That code tells the sync orchestrator to try the next
adapter. Any other error code is treated as a real failure for this file.

### 4.3 `export`

The host has already replayed the event chain and hands the plugin the
**current** payload to render. The plugin does not see raw events.

**Params** (`ExportParams`):

| Field      | Type     | Notes                                          |
|------------|----------|------------------------------------------------|
| `artifact` | object   | The ACF `Artifact` metadata (id, kind, name…). |
| `payload`  | object   | Current kind-typed payload JSON.               |
| `dest_path`| string   | Absolute path to write the native file to.     |

**Result** (`ExportResult`): `{ "written": bool }`.

### 4.4 `native_path`

Given an artifact and a context directory, where would its native file live?

**Params** (`NativePathParams`): `{ "artifact": Artifact, "context_dir": string }`.

**Result** (`NativePathResult`):

| Field      | Type   | Notes                                                   |
|------------|--------|---------------------------------------------------------|
| `path`     | string | Computed native path (empty when unsupported).          |
| `supports` | bool   | `false` when this plugin does not place that kind here. |

### 4.5 `handles_format`

Gate used by the orchestrator before fan-out export: does this plugin handle
a `(kind, format)` pair?

**Params** (`HandlesFormatParams`): `{ "kind": string, "format": string }`.

**Result** (`HandlesFormatResult`): `{ "handles": bool }`.

### 4.6 `capabilities`

Declares the adapter's full surface so the host can present it like a
built-in adapter.

**Params** (`CapabilitiesParams`): empty `{}`.

**Result** (`CapabilitiesResult`):

| Field              | Type                  | Notes                                              |
|--------------------|-----------------------|----------------------------------------------------|
| `name`             | string                | Adapter name (matches `plugin_name`).              |
| `artifacts`        | `ArtifactSupport`     | Which kinds are produced (see below).              |
| `tools`            | array of strings      | Tool kinds claimed (often empty).                  |
| `native_basenames` | array of strings      | Native filenames this adapter recognizes.          |
| `basename_to_kind` | object string→kind    | Optional map from basename to ACF kind.            |
| `notes_url`        | string                | Optional docs URL.                                 |

`ArtifactSupport`: `{ "memory": bool, "skill": bool, "tool": bool, "conversation": bool }`.

The SDK's `BaseCapabilitiesHandler` returns a conservative default
(all four artifact kinds `true`, no tools/basenames). Real plugins
**SHOULD** override `capabilities` to advertise only what they handle.

### 4.7 `shutdown`

**Params** (`ShutdownParams`): empty `{}`.
**Result** (`ShutdownResult`): empty `{}`.

`shutdown` is reserved in the ABI for hosts that prefer an explicit stop
signal. The current Aplexica daemon does **not** call `shutdown` for adapter
plugins — it tears them down by closing the plugin's stdin, so the host's
`Serve` loop reads EOF and exits cleanly. A plugin **MUST** treat a stdin EOF
as a normal exit (return `nil` from its serve loop), and — if a host does send
`shutdown` — **MUST** return from it promptly.

---

## 4.8 Error codes

| Code     | Constant                     | Meaning                                                              |
|----------|------------------------------|---------------------------------------------------------------------|
| `-32700` | `CodeParseError`             | Frame was not valid JSON (JSON-RPC standard).                       |
| `-32600` | `CodeInvalidRequest`         | Malformed request envelope (JSON-RPC standard).                    |
| `-32601` | `CodeMethodNotFound`         | Unknown method name (JSON-RPC standard).                           |
| `-32602` | `CodeInvalidParams`          | Params failed to decode (JSON-RPC standard).                       |
| `-32603` | `CodeInternalError`          | JSON-RPC standard internal error.                                  |
| `-32001` | `CodeUnrecognizedNativeFile` | **Fall-through:** not this plugin's file; try the next adapter.    |
| `-32002` | `CodeParseErrorPlugin`       | The file IS this plugin's but is malformed.                       |
| `-32003` | *(reserved)*                 | Reserved (`chain_invalid`); not emitted in ABI 1.                 |
| `-32004` | `CodeFormatUnsupported`      | `handles_format` said yes but export cannot render this payload.  |
| `-32005` | `CodeIOError`                | Plugin could not read/write the requested path.                   |
| `-32006` | `CodeSecretExtractionFailed` | Tool import could not extract a declared secret.                  |
| `-32099` | `CodeInternal`               | Catch-all for plugin bugs.                                         |

How errors are produced and consumed:

- A `Handler` method returns a `*RPCError{Code, Message}` to choose a
  specific code. Any **other** (non-`RPCError`) error returned by a handler
  is reported as `-32099` (`CodeInternal`).
- `-32001` has special fall-through semantics: the orchestrator catches it
  (`proto.IsUnrecognized`) and tries the next adapter for that file, rather
  than recording a failure.

(Reference: `internal/plugin/proto/codes.go`,
`internal/plugin/host/serve.go`.)

---

## 5. Manifest (`plugin.json`)

The daemon reads `plugin.json` from each plugin subdirectory **before**
spawning the executable, and re-verifies the same identity in the
`initialize` handshake.

### 5.1 Schema

| Field              | Type                 | Required | Notes                                                              |
|--------------------|----------------------|----------|--------------------------------------------------------------------|
| `manifest_version` | number               | yes      | MUST be `1`.                                                       |
| `name`             | string               | yes      | Unique adapter name. Collides-with-builtin ⇒ skipped (see §6).    |
| `version`          | string               | yes      | Plugin version.                                                   |
| `abi_version`      | string               | yes      | MUST equal `"1"`.                                                 |
| `executable`       | string               | yes      | Executable name/path; relative paths resolve against the subdir.  |
| `kind`             | string               | no       | `"adapter"` (default when omitted) or `"remote"`.                |
| `kinds`            | array of kind strings| see rule | **Adapter:** MUST list ≥1 kind. **Remote:** MUST be empty/absent. |
| `formats`          | object kind→[strings]| no       | Per-kind format list.                                            |
| `homepage`         | string               | no       | —                                                                |
| `author`           | string               | no       | —                                                                |
| `license`          | string               | no       | —                                                                |

### 5.2 Validation rules

A manifest is rejected (and the plugin not loaded) unless **all** hold:

1. `manifest_version == 1`.
2. `name`, `version`, `abi_version`, `executable` are all non-empty.
3. `abi_version == "1"`.
4. If `kind` is `"adapter"` or omitted: `kinds` lists **at least one** kind.
5. If `kind` is `"remote"`: `kinds` is **empty**. (Remote plugins are a
   separate, kind-agnostic surface and are **out of scope** for adapter
   authors — the adapter loader skips remote manifests.)
6. Any other `kind` value is rejected.

(Reference: `internal/plugin/proto/manifest.go` — `Manifest.Validate`.)

### 5.3 Example

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

---

## 6. Discovery, lifecycle, and degradation

- **Discovery.** The daemon scans `<plugins-dir>/*/plugin.json` (one
  subdirectory per plugin). The default `plugins-dir` is
  `<state-dir>/plugins` (`~/.aplexica/state/plugins`); it is overridable via
  the `--plugins-dir` daemon flag. An absent directory is **not** an error —
  zero plugins load and the daemon behaves exactly as before.

- **Skip rules.** A discovered plugin is **silently skipped** (logged, not
  fatal) when its `name`:
  - collides with a built-in adapter (`claude-code`, `codex`, `kilo`,
    `hermes`, `openclaw`), or
  - is in the daemon's disabled-adapters set, or
  - its manifest fails validation, or
  - its `kind` is `"remote"` (not an adapter).

- **Spawn + handshake.** For each surviving plugin the daemon spawns the
  executable (`cmd.Dir` = the plugin's subdirectory) and performs the
  `initialize` handshake. A plugin that fails to spawn, exits immediately,
  emits garbage, or never replies is **logged and skipped** — it never
  crashes the daemon and never blocks startup.

- **Shutdown.** On daemon shutdown each loaded plugin's stdin is closed
  (EOF), which ends its serve loop; the process is then waited on with a
  short timeout, then killed if it overruns. (The `shutdown` RPC is reserved
  in the ABI but not used by the current daemon for adapter plugins.)

- **Hot reload.** Plugins are discovered and loaded **once at startup**.
  Adding/removing a plugin requires a daemon restart in this release.

---

## 7. Security and trust (known limitation)

In this release the daemon **exec's any executable** placed in the plugins
directory, with the daemon's privileges. There is **no signature check, no
allowlist, and no sandbox**. The blast radius is bounded by the default
location being under the user's own state directory, but a hostile binary
dropped there runs as the user. A manifest-signing / allowlist /
capability-sandbox gate is a planned follow-up. **Only install plugins you
trust.**

---

## 8. Versioning

`abi_version` is a single integer-as-string. The daemon refuses any plugin
whose `abi_version` does not **exactly** match the value it supports
(currently `"1"`). Adding optional fields or new methods is **not** a
breaking change and does **not** bump the ABI; removing/renaming a field or
changing a method's semantics **does**.
