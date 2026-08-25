//go:build !windows

package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/aplexica/aplexica/internal/daemon"
	"github.com/spf13/cobra"
)

// installTOMLSighupHandler is the v0.75.0 companion to
// installSighupHandler. It re-runs reloadDaemonConfigPackage on each
// SIGHUP so the BRD-10 §10.1 TOML config layers reload alongside the
// v0.27.0 JSON-config-driven hot fields.
//
// Each fire is logged with the diff report. Restart-required keys
// surface as "takes effect on next daemon start" in the report;
// hot-reloadable keys would apply live (none in v0.75.0).
//
// On Windows there is no SIGHUP; the build-tagged stub at
// cmd_daemon_toml_reload_windows.go is a no-op. Users on Windows
// invoke `aplexica daemon reload` instead, which goes through the
// control-socket Reloader callback wired in cmd_daemon.go.
func installTOMLSighupHandler(ctx context.Context, cmd *cobra.Command, lg *daemon.RotatingLogger) {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGHUP)
	go func() {
		defer signal.Stop(sig)
		for {
			select {
			case <-ctx.Done():
				return
			case <-sig:
				report, err := reloadDaemonConfigPackage(cmd)
				if err != nil {
					lg.Error("SIGHUP TOML reload failed", "err", err)
					continue
				}
				lg.Info("SIGHUP TOML reload", "report", report)
			}
		}
	}()
}
