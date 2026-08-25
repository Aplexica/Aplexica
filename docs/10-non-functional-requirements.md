# Non-Functional Requirements (OSS Edition)

**Document type:** Business Requirements Document
**Status:** Draft v1
**Last updated:** 2026-05-19
**Edition:** OSS (Aplexica open-source)
**Maintainer:** Aplexica project

---

> This BRD records requirements and design intent. For currently shipped
> behavior and commands, use the [user guide](user-guide.md) and release notes.

## 1. Purpose

The capability BRDs each list their own non-functional requirements scoped to that capability. This document consolidates the **cross-cutting** quality attributes: performance, reliability, observability, supported platforms, internationalization, accessibility, and the basics of how Aplexica behaves as a system independent of any one feature.

When a per-capability NFR conflicts with this document, this document wins.

## 2. Supported platforms (V1)

| Platform | OS versions | Architecture |
|---|---|---|
| **macOS** | 13 Ventura and newer | x86_64, arm64 |
| **Linux** | glibc 2.31+ (Ubuntu 20.04, Debian 11, RHEL 9, Fedora 36, and newer); modern systemd | x86_64, arm64 |
| **Windows** | Windows 10 version 1909 and newer; Windows 11 | x86_64, arm64 |

**Implementation language:** Go. The daemon, CLI, tray app, and first-party adapters are built per supported platform. cgo is permitted where an OS integration requires it; the core daemon and adapter ring remain pure Go for portability and reproducible builds.

### 2.1 Canonical path convention

All BRDs reference `~/.aplexica/` as the canonical location of daemon state, configuration, logs, and the canonical store. This is a logical path that resolves to the user-home dotfile on every supported platform:

| Platform | Concrete location |
|---|---|
| macOS | `~/.aplexica/` — literal. |
| Linux | `~/.aplexica/` — literal. (Future opt-in for full XDG Base Directory compliance using `~/.config/aplexica/` and `~/.local/share/aplexica/` is tracked separately.) |
| Windows | `%USERPROFILE%\.aplexica\` — literal, treated identically to `~/.aplexica/` on Unix. |

### 2.2 Linux specifics

- Both `systemd --user` and a fallback `--detach` daemon mode are supported. Distributions without systemd (e.g., Alpine with OpenRC) get the `--detach` mode plus instructions to integrate with their init system.
- Wayland and X11 desktops both supported for desktop notifications.

### 2.3 Windows specifics

- The daemon registers a per-user, logon-triggered Scheduled Task so it runs in the same user context as the agents it observes.
- File-locking semantics on Windows are explicitly accounted for in adapter code (copy-then-read for any file an agent may have open).
- Daemon state and the canonical store live under `%USERPROFILE%\.aplexica\` (see section 2.1). Logs and ephemeral caches the user may want to exclude from roaming profiles can be redirected to `%LOCALAPPDATA%\Aplexica\` via config.

### 2.4 macOS specifics

- Release metadata states the signing and notarization status of each macOS artifact; installation documentation must not imply a stronger status than the artifact has.
- LaunchAgent installed in `~/Library/LaunchAgents/com.aplexica.aplexicad.plist`.
- Daemon state and the canonical store live under `~/.aplexica/` (see section 2.1).

## 3. Performance

| Metric | Target |
|---|---|
| **Daemon startup time** | < 2 seconds from process spawn to first watcher registered |
| **Initial scan (per adapter, 1 GB native state)** | < 30 seconds |
| **Local sync end-to-end latency (95th percentile, <1 MB artifact)** | < 2 seconds |
| **Memory footprint (idle, 5 adapters, no transport plugin)** | < 200 MB resident |
| **CPU usage (typical workload)** | < 1% averaged over 60-second windows on one core |
| **Disk usage growth (typical user, no media)** | < 100 MB / month |
| **Export of 1 GB-equivalent state** | < 60 seconds |
| **Import dry-run of 100 MB bundle** | < 5 seconds |
| **CLI cold-start (any command)** | < 200 ms |
| **CLI command response time for any read-only operation** | < 500 ms |

All targets are validated on a 2023-vintage laptop baseline: M2 MacBook Air with 16 GB RAM, mid-range Intel/AMD with 16 GB, NVMe SSD.

## 4. Reliability

### 4.1 Daemon

- Daemon MUST be supervised by the platform's per-user startup mechanism (launchd, `systemd --user`, or a Windows logon Scheduled Task).
- Daemon MUST restart automatically on crash with exponential backoff.
- After 5 consecutive crashes within 10 minutes, the daemon MUST enter a "quarantined" state where the OS init system stops auto-restarting it and a user-visible warning is generated. The user can re-enable with `aplexica daemon repair`.
- The canonical store MUST never be corrupted by a daemon crash. Writes use atomic-rename or journaling. The store is recoverable from the event log in the worst case.

### 4.2 Data integrity

- Every event in the canonical store carries a SHA-256 content hash. `aplexica verify` re-hashes and confirms.
- Periodic background integrity checks detect corruption without modifying healthy artifacts.
- The store carries an internal version number; mismatched daemon versions refuse to start.

### 4.3 Recovery time and recovery point

| Scenario | Target |
|---|---|
| Daemon crash | RTO < 10 seconds (auto-restart); RPO 0 (event log is durable) |

Transport plugin recovery targets (network outages, remote-service unavailability, account recovery) are defined in the transport plugin's own BRD.

## 5. Observability

### 5.1 Logging

- Logs in line-oriented JSON.
- Fields: `ts`, `level`, `component`, `event`, `details`.
- Levels: `trace`, `debug`, `info`, `warn`, `error`. Default level: `info`.
- Log destination: `~/.aplexica/logs/` with daily rotation, 30-day retention.
- `aplexica daemon logs [--follow]` is the user-facing surface; pretty-prints to terminal, JSON when piped.

### 5.2 Metrics

- Daemon exposes a Prometheus-format metrics endpoint on a configurable local loopback port (off by default; enabled by config).
- Metrics families: daemon_uptime_seconds, adapter_events_inbound_total, adapter_events_outbound_total, adapter_errors_total, store_size_bytes, store_events_total, conflict_count, queue_depth, sync_latency_seconds (histogram).
- Transport plugins MAY register additional metric families via the plugin API; any plugin-defined metrics are namespaced by plugin ID.

### 5.3 Tracing

- OpenTelemetry-compatible trace export, off by default, opt-in via config.
- Spans for: inbound event processing, outbound event materialization, rule evaluation, and any plugin-defined spans registered through the plugin API.

### 5.4 User-facing diagnostics

- `aplexica status` is the primary user-facing diagnostic. Shows: daemon state, per-adapter state, conflict counts, sync queues, last error per adapter, and per-plugin connectivity state (if a plugin is loaded).
- `aplexica doctor` runs an interactive diagnostic and emits a redacted, shareable report file the user can attach to a support ticket. Redaction scrubs paths under `$HOME`, email addresses, and known secret patterns.

## 6. Internationalization

### 6.1 V1 launch language

- English only for CLI strings and error messages.
- All user-facing strings MUST be externalized in a translation catalog (gettext-format or equivalent) from day one so additional languages can be added without code changes.

### 6.2 Additional languages

- Additional catalogs are accepted based on contributor participation and user demand.
- The catalog and formatting APIs must keep localization changes separate from core behavior.

### 6.3 Text handling

- All file paths, identifiers, and metadata are UTF-8 throughout.
- The store handles Unicode normalization (NFC) for filenames internally.
- Date/time display follows the user's locale; storage is always UTC ISO-8601.

## 7. Accessibility

### 7.1 CLI

- CLI output MUST be readable on monochrome terminals (no information conveyed by color alone).
- Tables MUST degrade to plain-text on narrow terminals.
- Long-running operations MUST show progress that is screen-reader compatible (interleaved status lines, no overwriting cursor tricks unless `--tty-progress` is explicitly used).

## 8. Compatibility and versioning

### 8.1 Version policy

- Semantic versioning (`MAJOR.MINOR.PATCH`).
- ACF schema versioning is independent of binary versioning.
- Release frequency is driven by tested changes and security needs; the project does not promise a fixed calendar cadence.

### 8.2 Forward and backward compatibility

- Newer daemons MUST read older canonical stores. Migrations are run on first startup of a newer version against an older store.
- Older daemons MUST refuse to operate on newer canonical stores with a clear "upgrade required" error rather than corrupting data.
- The ACF JSON Schema is versioned; readers refuse newer schema versions with a clear error.

### 8.3 Adapter compatibility

- Each adapter declares the range of agent versions it supports.
- An adapter detects an agent version outside its supported range and surfaces a warning rather than silently failing.

## 9. Resource accounting

- The daemon MUST honor a configurable disk quota for the canonical store (`retention.store_max_gb` in config). A value of `0` disables quota-driven pressure handling; with a finite cap, reclamation follows the retention policy before ingestion is refused.
- The daemon MUST honor a configurable max-event-size (`limits.max_artifact_size_mb`). Events above the limit are flagged in `aplexica status` with options to ingest manually with `--force`.
- The daemon MUST honor a configurable RAM budget and reduce in-memory caches if approaching the limit. Defaults appropriate per platform.

## 10. Configuration architecture (the golden rule)

**The golden rule: no hardcoded values in source code.** Every tunable parameter — cadence, threshold, timeout, retry count, buffer size, port, path pattern, default limit, sleep interval — lives in a configuration file. The shipped binary embeds a `defaults.toml` that captures every default; users override at the user, system, or project level without rebuilding.

This is non-negotiable. Code review MUST reject any change that introduces a magic number, magic string, or hardcoded constant outside the small set of legitimate exceptions in §10.4. This principle exists for four concrete reasons:

- **Tweakable** — power users adjust behavior to their workloads without filing a feature request.
- **Debuggable** — operators can compare two installs by diffing config, not by reading source.
- **Auditable** — security reviewers see the entire behavioral surface in one place.
- **Serviceable** — support can ship a config tweak instead of a patch release.

The vision document treats this as a top-line guiding principle (see [00-vision.md §7](00-vision.md), item 8). This section is the enforceable, concrete form of that principle.

### 10.1 Configuration layers

Aplexica applies configuration in layered precedence, lowest priority first:

| Layer | Path / source | Typical owner |
|---|---|---|
| **1. Shipped defaults** | `defaults.toml` embedded in the binary at build time | Aplexica maintainers |
| **2. System config** | `/etc/aplexica/config.toml` (Unix) or `%PROGRAMDATA%\Aplexica\config.toml` (Windows) — optional | IT department / sysadmin |
| **3. User config** | `~/.aplexica/config.toml` | The user |
| **4. Project config** | `<project-root>/.aplexica/config.toml` — optional | The user / project owner |
| **5. Environment variables** | `APLEXICA_<KEY>=<value>` | Per-process / shell session |
| **6. CLI flags** | `--config-set <key>=<value>` per command | Per-invocation, transient |

A later layer overrides earlier ones at per-parameter granularity. Layer 1 always provides a value; subsequent layers replace only the keys they set. Project config (layer 4) is consulted only when the daemon is operating in a project context (see [02-brd-format-adapters.md §4.13](02-brd-format-adapters.md)).

### 10.2 Configuration schema and validation

- Every config parameter MUST have a published JSON Schema entry describing its type, range, default value, unit, and human-readable description.
- The full schema MUST be exposed via `aplexica config schema` (machine-readable JSON) and `aplexica config docs` (human-readable Markdown).
- On daemon startup, the merged configuration MUST be validated against the schema. Invalid values produce a clear error naming the layer, the key, the value, and the constraint that was violated.
- Unknown keys (typos, deprecated keys from old versions) produce warnings at startup, not failures.

### 10.3 Tooling

- `aplexica config show [--key <path>]` — print the effective merged config (or a specific key), with provenance per key (which layer set it).
- `aplexica config set <key> <value> [--layer user|system|project]` — write to the named layer (default `user`).
- `aplexica config unset <key> [--layer user|system|project]` — remove from the named layer.
- `aplexica config diff` — show what differs between the shipped defaults and the current effective config.
- `aplexica config validate <file>` — validate an arbitrary file against the schema (useful for CI).
- `aplexica config edit` — open the user config in `$EDITOR` and re-validate on save.

### 10.4 Legitimate exceptions

A small set of constants are permitted in code:

- **Magic protocol values** that aren't configurable by definition (e.g., the literal string `"aplexica/v1"` in protocol handshakes; the JSON Schema URL; canonical wire-format key names).
- **Algorithmic constants** that are part of well-known specifications (e.g., the AES-256 block size, the BIP39 wordlist, hash output sizes).
- **Build metadata** populated at compile time (version string, commit SHA, build date).
- **Test fixtures** in `_test` / `tests/` files (where the test IS the documentation of intent).

Anything else — every threshold, cadence, retry budget, timeout, size limit, port number, file-extension list, default path, regex pattern, log-rotation count — lives in `defaults.toml`. When in doubt, put it in the config.

### 10.5 Functional requirements

- **FR-10.6** Every tunable parameter the daemon, adapters, CLI, or any component reads at runtime MUST originate in a configuration layer (§10.1), not as a literal constant in source. CI MUST include a linter check (e.g., a Rust `clippy` lint or a Go `vet` pass plus a custom check) that fails on uncategorized magic numbers in non-test source files.
- **FR-10.7** The shipped `defaults.toml` MUST be a complete configuration — running the daemon with no other layers present MUST produce a working system with the documented defaults.
- **FR-10.8** Configuration MUST hot-reload on `SIGHUP` (Unix) or `aplexica daemon reload` (cross-platform). Parameters that cannot be hot-reloaded (e.g., listening ports, daemon socket paths) MUST be documented as `restart_required: true` in the schema; changing them produces a warning that the daemon needs a restart.
- **FR-10.9** Every parameter MUST have a JSON Schema entry; the schema is the source of truth for what's configurable. The schema MUST ship with every release and MUST be versioned alongside the binary.
- **FR-10.10** `aplexica config show` MUST display provenance per key (which layer provided the effective value), so users can debug "why is this value what it is" without spelunking through files.
- **FR-10.11** The shipped `defaults.toml` MUST contain inline comments explaining every parameter — purpose, units, valid range, when to change it. The file is documentation for the user; it ships as part of the binary's distribution.

## 11. Functional requirements

(This BRD is mostly non-functional by definition; these few are cross-cutting functional requirements that don't fit cleanly in any other BRD.)

- **FR-10.1** `aplexica status` MUST produce a consistent snapshot of daemon and plugin state and complete in under 1 second.
- **FR-10.2** `aplexica doctor` MUST produce a redacted diagnostic report under 5 MB.
- **FR-10.3** Logs MUST roll over at midnight local time daily, with up to 30 retained log files.
- **FR-10.4** Metrics endpoint MUST be opt-in and exposed only on the loopback interface unless explicitly bound elsewhere.
- **FR-10.5** Telemetry from OSS MUST be off and unconfigurable in the default build.

## 12. Non-functional requirements

(All in the body of this document.)

## 13. Out of scope

- Specific monitoring integrations (Datadog, Honeycomb, etc.). Generic OpenTelemetry + Prometheus output suffices; users build their own integrations.
- Mobile platforms (iOS, Android). The agents being managed don't run on mobile; not in V1.
- Browser-only sync (the daemon is a process, not a browser extension). Future possibility.

## 14. Acceptance criteria

V1 is complete for non-functional requirements when:

1. Every platform artifact marked available in the installation guide produces a working daemon that meets the startup-time target.
2. Performance targets in section 3 are met on the baseline reference hardware in a load test scripted in the test harness.
3. Reliability targets are validated by chaos tests (random daemon kills, FS errors, network partitions); no data loss in the canonical store.
4. Observability commands (`status`, `logs`, `doctor`) work as specified.
5. CLI is fully usable on a monochrome terminal and via screen reader.
6. The shipped `defaults.toml` is a complete, validated, fully-commented config; running the daemon with no overrides produces a working system. CI lint catches any magic numbers introduced in non-test source.

## 15. Dependencies

This BRD applies cross-cuttingly to every other BRD in the OSS set. The configuration architecture section in particular is the substrate every other BRD's "default value" or "configurable" statement rests on.
