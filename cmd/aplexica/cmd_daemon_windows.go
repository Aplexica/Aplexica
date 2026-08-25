//go:build windows

package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"

	"github.com/aplexica/aplexica/internal/daemon"
	"github.com/aplexica/aplexica/internal/hermeswatch"
	"github.com/aplexica/aplexica/internal/retention"
	syncd "github.com/aplexica/aplexica/internal/sync"
)

// detachSysProcAttr returns a SysProcAttr that detaches a daemon child from the
// parent's console on Windows and suppresses any transient console window.
const _DETACHED_PROCESS = 0x00000008
const _CREATE_NO_WINDOW = 0x08000000
const windowsNonInteractiveSessionID uint32 = 0

func detachSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		CreationFlags: _DETACHED_PROCESS | _CREATE_NO_WINDOW,
		HideWindow:    true,
	}
}

// hideRemotePluginWindow suppresses the console window for short-lived remote
// plugin CLI invocations launched by the local web UI (pair/status/unpair/
// connect-check). The plugin is a console-subsystem binary; without this,
// Windows briefly flashes a terminal for each button click.
func hideRemotePluginWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: _DETACHED_PROCESS | _CREATE_NO_WINDOW,
		HideWindow:    true,
	}
}

// canLaunchTrayFromCurrentSession prevents SSH/session-0 commands from
// starting an invisible tray process that would take the single-instance lock
// away from the real desktop session. The daemon/logon task launches again in
// the interactive session, where the tray can show normally.
func canLaunchTrayFromCurrentSession() (bool, string) {
	var sessionID uint32
	if err := windows.ProcessIdToSessionId(uint32(os.Getpid()), &sessionID); err != nil {
		return true, ""
	}
	if sessionID == windowsNonInteractiveSessionID {
		return false, "current Windows session is non-interactive; desktop daemon/logon startup will launch the tray"
	}
	return true, ""
}

// detachConsoleWindow detaches this process from its console (FreeConsole) so a
// `daemon serve` launched by the Windows keep-alive scheduled task shows no
// console window. Best-effort: when no console is attached FreeConsole simply
// fails, which we ignore.
func detachConsoleWindow() {
	_, _, _ = syscall.NewLazyDLL("kernel32.dll").NewProc("FreeConsole").Call()
}

// isProcessElevated reports whether the current process token is elevated from
// a UAC perspective. Factored into a package var so refusePrivilegedStartup
// can be unit-tested without an actual elevated token (the test stubs it).
// The underlying windows.Token.IsElevated already fails safe — on any
// token-query error it reports false (non-elevated) — so a legitimate
// non-admin start is never bricked by a probe failure.
var isProcessElevated = func() bool {
	return windows.GetCurrentProcessToken().IsElevated()
}

// refusePrivilegedStartup refuses to start the daemon when the process is
// running elevated (UAC). FR-09.12 requires the daemon run as the user's
// normal account, not an administrator: an elevated daemon would write
// conversation history and locks with the wrong integrity level and broadens
// the blast radius of any compromise. The non-windows build provides a
// same-signature implementation that refuses an euid-0 (root) start.
func refusePrivilegedStartup() error {
	if isProcessElevated() {
		return fmt.Errorf("aplexica daemon: refusing to run elevated/as administrator (FR-09.12); run as your normal user")
	}
	return nil
}

// runAsWindowsService routes daemon serve through Windows SCM via
// daemon.RunAsService. Called from cmd_daemon.go's daemonServeCmd.RunE
// after detecting SCM via daemon.IsWindowsService(). The body is the
// closure assembled in RunE; RunAsService blocks until SCM signals
// Stop or Shutdown, then waits for body to return before reporting
// Stopped.
func runAsWindowsService(lg *slog.Logger, body func(ctx context.Context) error) error {
	return daemon.RunAsService(lg, body)
}

// installSighupHandler is a no-op on Windows: syscall.SIGHUP does not
// exist, and Windows daemons that need rotation use the platform-native
// log-roll mechanism (or just restart the service). The unix build
// wires SIGHUP to rotate-log + reload-config (v0.27.0 closes FR-03.16).
// Windows config reload is a separate follow-up that needs a different
// trigger (named pipe message, Windows Service control code, or a
// control-socket command rather than signal-driven).
func installSighupHandler(
	_ context.Context,
	_ *daemon.RotatingLogger,
	_ string,
	_ *daemon.Config,
	_ *syncd.Orchestrator,
	_ *hermeswatch.Watcher,
	_ *retention.Runner,
) {
}
