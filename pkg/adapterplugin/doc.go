// SPDX-License-Identifier: AGPL-3.0-or-later

// Package adapterplugin is the public SDK for writing out-of-process
// Aplexica adapter plugins in Go.
//
// An adapter plugin is a standalone executable the Aplexica daemon spawns
// as a subprocess and speaks a small JSON-RPC 2.0 protocol to over
// stdin/stdout. The plugin is a PURE TRANSLATOR: it parses a native file
// into typed ACF payloads on import and renders a payload back to a native
// file on export. It never touches a store — the daemon owns all
// reconciliation and persistence.
//
// # Quick start
//
// Implement Handler (embed BaseCapabilitiesHandler for a sane default
// Capabilities) and call ServeStdio from main:
//
//	package main
//
//	import (
//	    "context"
//	    "fmt"
//	    "os"
//
//	    "github.com/aplexica/aplexica/pkg/adapterplugin"
//	)
//
//	func main() {
//	    if err := adapterplugin.ServeStdio(context.Background(), &Handler{}); err != nil {
//	        fmt.Fprintln(os.Stderr, "plugin exited:", err)
//	        os.Exit(1)
//	    }
//	}
//
// Then drop the compiled binary plus a plugin.json into
// <state-dir>/plugins/<name>/ (default ~/.aplexica/state/plugins/<name>/)
// and restart the daemon.
//
// # Wire contract
//
//   - Newline-delimited compact JSON frames over stdin/stdout (no embedded
//     newlines; 64 MB max frame).
//   - JSON-RPC 2.0 request/response envelopes.
//   - ABIVersion handshake: the plugin MUST echo ABIVersion exactly.
//   - stdout carries protocol frames ONLY. All diagnostics go to stderr.
//
// The seven methods are: initialize, import, export, native_path,
// handles_format, capabilities, shutdown — one per Handler method.
//
// See docs/plugin-author-guide.md for a step-by-step walkthrough and
// docs/plugin-protocol-spec.md for the normative wire specification. The
// reference implementation lives in cmd/aplexica-plugin-example.
//
// # Payload types
//
// Plugins build ACF payloads (acf.MemoryPayload, acf.SkillPayload,
// acf.ToolPayload, acf.ConversationPayload) and the acf.Kind / acf.Scope
// constants from package github.com/aplexica/aplexica/internal/acf. NOTE:
// internal/acf is import-restricted to this module, so a genuinely external
// third-party module cannot import it yet. For now, author your plugin
// inside this module (as cmd/aplexica-plugin-example does). Promoting the
// acf payload types to a stable pkg/ path is a planned follow-up.
package adapterplugin
