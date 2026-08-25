//go:build windows

package main

import (
	"context"

	"github.com/aplexica/aplexica/internal/daemon"
	"github.com/spf13/cobra"
)

// installTOMLSighupHandler is a no-op on Windows — syscall.SIGHUP
// doesn't exist. Windows users invoke `aplexica daemon reload`, which
// hits the cross-platform control-socket Reloader wired in
// cmd_daemon.go (v0.75.0).
func installTOMLSighupHandler(_ context.Context, _ *cobra.Command, _ *daemon.RotatingLogger) {
}
