//go:build !windows

package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/aplexica/aplexica/internal/daemon"
	"github.com/aplexica/aplexica/internal/hermeswatch"
	"github.com/aplexica/aplexica/internal/retention"
	syncd "github.com/aplexica/aplexica/internal/sync"
)

// runAsWindowsService is the non-windows stub. daemon.IsWindowsService
// always returns (false, nil) on non-windows so the branch in
// daemonServeCmd.RunE that would call this never fires; the stub
// exists purely so the build-tag-free cmd_daemon.go references compile
// on every platform. Returning an error here would mask a real
// daemon.IsWindowsService bug; we panic instead.
func runAsWindowsService(_ *slog.Logger, _ func(ctx context.Context) error) error {
	panic("runAsWindowsService called on non-windows — daemon.IsWindowsService should have returned false")
}

// detachSysProcAttr returns a SysProcAttr that puts the child process into
// its own session so Ctrl-C in the parent's terminal doesn't kill the
// daemon. On Unix this means Setsid: true.
func detachSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}

// detachConsoleWindow is a no-op on non-windows (no console-window concept).
// The windows-tagged build calls FreeConsole; --windows-detach-console is
// accepted but inert here.
func detachConsoleWindow() {}

// geteuidFn is the source of the effective uid, factored into a package var
// so refusePrivilegedStartup can be unit-tested without actually running as
// root (the test stubs it to 0). Defaults to the real os.Geteuid.
var geteuidFn = os.Geteuid

// refusePrivilegedStartup refuses to start the daemon when running as root
// (euid 0). FR-09.12 requires the daemon run as the user's normal account, not
// root: a root-owned daemon would write conversation history and locks with
// the wrong ownership and broadens the blast radius of any compromise. The
// windows-tagged build provides a same-signature implementation that detects
// token elevation instead.
func refusePrivilegedStartup() error {
	if geteuidFn() == 0 {
		return fmt.Errorf("aplexica daemon: refusing to run as root (FR-09.12); run as your normal user")
	}
	return nil
}

// installSighupHandler wires SIGHUP to (1) rotate the daemon log file
// and (2) reload <state-dir>/config.json for the lifetime of ctx
// (v0.27.0 closed FR-03.16; v0.27.1 now applies all hot fields live).
//
// On each SIGHUP:
//  1. Calls lg.Rotate() — Rotate errors are reported on stderr because
//     the logger itself may be in a half-state during the rotation it
//     just failed to perform.
//  2. Runs applyJSONConfigReload (daemon_json_reload.go), the hot-field
//     apply pass shared with the control-socket "reload" command: it
//     re-reads configPath, diffs via daemon.ApplyReload, applies every
//     hot field (log level, quiet, guard window, hermes interval,
//     snapshot cadence/max-age, sync gate + backfill), and advances
//     *currentCfg to the new baseline. Parse errors are logged and the
//     handler continues (the daemon keeps running with the prior config).
//
// On Windows there is no SIGHUP; the windows-tagged stub of this
// function is a no-op.
func installSighupHandler(
	ctx context.Context,
	lg *daemon.RotatingLogger,
	configPath string,
	currentCfg *daemon.Config,
	orch *syncd.Orchestrator,
	hw *hermeswatch.Watcher,
	snapRunner *retention.Runner,
) {
	sighup := make(chan os.Signal, 1)
	signal.Notify(sighup, syscall.SIGHUP)
	go func() {
		defer signal.Stop(sighup)
		for {
			select {
			case <-ctx.Done():
				return
			case <-sighup:
				lg.Info("SIGHUP received — rotating log + reloading config")
				if err := lg.Rotate(); err != nil {
					fmt.Fprintf(os.Stderr, "aplexica daemon: log rotate failed: %v\n", err)
				}
				// Hot-field apply logic is shared with the control-socket
				// "reload" command — see daemon_json_reload.go.
				if _, cerr := applyJSONConfigReload(lg, configPath, currentCfg, orch, hw, snapRunner); cerr != nil {
					lg.Error("SIGHUP config reload failed", "err", cerr, "path", configPath)
				}
			}
		}
	}()
}
