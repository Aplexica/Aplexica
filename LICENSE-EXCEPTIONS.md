# Aplexica AGPL-3.0 — Plugin Linking Exception

> **Version 1.0 — Effective 2026-07-31**

## Summary

Aplexica OSS (`aplexica`, `aplexica-status`, `aplexicatray`) is licensed under
the **GNU Affero General Public License v3.0** ([LICENSE](LICENSE)).

This document grants an additional permission ("linking exception") to
the AGPL-3.0 terms, narrowly scoped to the **out-of-process plugin
boundary** defined by the Aplexica Plugin Protocol.

## The exception

> Notwithstanding the terms of the GNU Affero General Public License,
> version 3, the maintainers of Aplexica grant the following additional
> permission:
>
> A **plugin** that
>
> 1. runs in its own operating-system process,
> 2. communicates with the daemon mode of `aplexica` (or any other Aplexica binary) **only**
>    through the documented JSON-RPC plugin protocol over stdio, as
>    defined by [the public Aplexica Plugin Protocol](docs/plugin-protocol-spec.md),
>    and
> 3. does **not** statically or dynamically link any code from the
>    Aplexica source tree into its own binary,
>
> shall **not** be considered a "derivative work" of Aplexica for the
> purposes of AGPL-3.0 §0 ("Definitions") and §1 ("Source Code").
>
> Plugin authors are therefore free to license their plugins under any
> license of their choosing, including proprietary licenses, and are
> not required to make plugin source code available under AGPL-3.0
> when they distribute the plugin or operate it as a network service.
>
> This exception does **not** alter any other obligation under
> AGPL-3.0 with respect to modifications of the Aplexica source tree
> itself. Any modification of files inside the Aplexica source tree
> (including the plugin protocol definition files) remains subject to
> the full terms of AGPL-3.0.

## Scope and rationale

The Aplexica plugin protocol is a **clean architectural boundary**:

- Plugins are separate executables spawned by the `aplexica` daemon process as child
  processes.
- All communication crosses a process boundary via stdio pipes
  carrying JSON-RPC messages.
- The protocol is versioned (`proto.ABIVersion`) and documented as a
  stable interface in `internal/plugin/proto/messages.go`.
- No Aplexica code is loaded into the plugin's address space.
- No plugin code is loaded into the `aplexica` daemon process's address space.

This is materially the same boundary the GNU project treats as
**non-derivative** in well-established FSF guidance on independent
programs that communicate via pipes / sockets / IPC. The "system
library exception" in GPL-3.0 §1 and FSF's published opinions on
"separate programs vs. derivative works" both inform the same
conclusion.

Stating the exception explicitly removes ambiguity for first-party
proprietary plugins and third-party plugins (community adapters that may need
to ship under non-AGPL terms).

## What this exception does NOT permit

- **Vendoring AGPL-licensed Aplexica code into a proprietary
  product.** Direct linking, source inclusion, or republication of
  any file under `internal/`, `cmd/`, `pkg/`, or any other directory
  of the Aplexica source tree remains under AGPL-3.0.
- **Forking AGPL files and re-licensing the fork.** Modifications of
  the Aplexica source tree itself remain under AGPL-3.0.
- **Bundling Aplexica binaries into a closed-source distribution.**
  Redistribution of the `aplexica`, `aplexica-status`, or `aplexicatray`
  binaries remains subject to AGPL-3.0's source-availability
  requirements.

## How to verify your plugin qualifies

Your plugin qualifies for this exception if **all** of the following
are true:

- [ ] The plugin executable is built from sources that contain **zero
      lines of code copied from the Aplexica source tree.**
- [ ] The plugin executable does not link (statically or dynamically)
      against any library produced from the Aplexica source tree.
- [ ] The plugin's only communication with the daemon mode of `aplexica` (or any other
      Aplexica binary) is the JSON-RPC stdio protocol defined in
      `internal/plugin/proto/`.
- [ ] The plugin runs as a separate operating-system process.

If all four boxes are checked, you may license your plugin under any
license — including a proprietary commercial license — without
triggering AGPL-3.0's source-availability obligations.

## Contributor agreement

This linking exception is granted by the Aplexica project maintainers
to all users of the published Aplexica binaries and source.
Contributors to the Aplexica source tree retain copyright in their
contributions and submit them under the
[Contributor License Agreement](CONTRIBUTING.md#contributor-license-agreement-cla),
which permits the project to publish those contributions under AGPL-3.0 with
this exception applied to the same plugin boundary.

## Questions

For clarification of how this exception applies to a specific plugin
or distribution, contact the maintainers via a GitHub issue (label:
`question:license`) or `hello@aplexica.com` for private inquiries.

---

*This document is provided as a clear statement of the maintainers'
intent. It is not a substitute for legal advice. Plugin authors with
material business risk should engage their own legal counsel.*
