// SPDX-License-Identifier: AGPL-3.0-or-later

// Command aplexica-plugin-example is a reference adapter plugin for the
// Aplexica daemon. It is a PURE TRANSLATOR for a single markdown memory
// file (MEMORY.md): it never touches the canonical store — the daemon owns
// all store IO and reconciles the ImportResult this plugin returns.
//
// It speaks the newline-delimited JSON-RPC plugin protocol over stdin
// (requests) and stdout (responses). ALL diagnostics MUST go to stderr:
// anything written to stdout corrupts the protocol stream.
//
// Install: place the built binary and its plugin.json under
// <state-dir>/plugins/example-memory/ (default ~/.aplexica/state/plugins).
// See docs/plugin-author-guide.md.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/aplexica/aplexica/pkg/adapterplugin"
)

func main() {
	// ServeStdio blocks until the daemon closes stdin (EOF) or a transport
	// error occurs. A clean shutdown returns nil.
	if err := adapterplugin.ServeStdio(context.Background(), &Handler{}); err != nil {
		// Diagnostics to stderr ONLY — stdout carries protocol frames.
		fmt.Fprintf(os.Stderr, "%s: serve: %v\n", pluginName, err)
		os.Exit(1)
	}
}
