// Package trayinstall registers the aplexica tray indicator as a
// per-user LOGIN autostart entry. Distinct from internal/daemon which
// registers the always-running background sync daemon; the tray is
// only meant to run when the user is logged in, and user-quitting it
// MUST NOT respawn it until next login.
//
// macOS  : ~/Library/LaunchAgents/com.aplexica.tray.plist
//
//	(RunAtLoad=true, KeepAlive{SuccessfulExit:false} — respawn on
//	 crash/abnormal exit, but NOT on a clean user-quit)
//
// Linux  : ~/.config/autostart/aplexica-tray.desktop
//
//	(XDG Autostart spec)
//
//	Windows: %APPDATA%\Microsoft\Windows\Start Menu\Programs\Startup\
//	         Aplexica Tray.lnk
//	         (created via PowerShell WScript.Shell COM bindings)
//
// Idempotency: Install on an already-installed entry overwrites cleanly;
// Uninstall on a non-installed entry returns nil. Matches the contract
// of internal/daemon's Installer interface.
package trayinstall

import (
	"errors"
	"fmt"
	"time"
)

// ErrNotSupported is returned by Installer methods on platforms where
// the tray autostart backend isn't implemented (currently only the
// three V1 platforms — macOS, Linux, Windows — are supported).
var ErrNotSupported = errors.New("trayinstall: not supported on this platform")

// Options is the platform-agnostic input. TrayPath is required;
// everything else is optional flag-forwarding (v0.40.0) — when a field
// is unset (empty string / zero duration), the autostart entry omits
// the corresponding CLI flag and the tray binary uses its own defaults.
type Options struct {
	// TrayPath is the absolute path to the aplexicatray binary the
	// autostart entry will exec. The CLI typically resolves this via
	// exec.LookPath("aplexicatray") before calling. Required.
	TrayPath string

	// AplexicaPath, when set, is forwarded as `--aplexica <path>` so
	// the tray binary launches a specific aplexica CLI (useful when
	// the user has multiple installs or wants a known-good path
	// baked into the autostart entry).
	AplexicaPath string

	// Interval, when non-zero, is forwarded as `--interval <D>`.
	Interval time.Duration

	// LogDir, when set, is forwarded as `--log-dir <path>` — drives
	// the tray's "Open logs" menu item.
	LogDir string

	// ActiveWindow, when non-zero, is forwarded as `--active-window <D>`.
	ActiveWindow time.Duration

	// PausedThreshold, when non-zero, is forwarded as `--paused-threshold <D>`.
	PausedThreshold time.Duration

	// StateDir, when set, is forwarded as `--state-dir <path>` so the
	// tray binary points its `aplexica status --watch` subprocess at a
	// non-default daemon state directory. v0.43.0 (closes a bug found
	// during the macOS smoke test: without this, the tray only ever
	// sees daemons at `~/.aplexica/state`).
	StateDir string

	// ConflictsRoot, when set, is forwarded as `--conflicts-root <path>`.
	ConflictsRoot string
}

func (o Options) Validate() error {
	if o.TrayPath == "" {
		return fmt.Errorf("Options: TrayPath is required")
	}
	return nil
}

// extraArgs returns the forwarded CLI flags as a string slice in stable
// order, suitable for splicing into a platform-specific command line.
// Shared by all three installers so the flag order stays consistent
// across launchd / .desktop / .lnk.
func (o Options) extraArgs() []string {
	var args []string
	if o.AplexicaPath != "" {
		args = append(args, "--aplexica", o.AplexicaPath)
	}
	if o.Interval > 0 {
		args = append(args, "--interval", o.Interval.String())
	}
	if o.LogDir != "" {
		args = append(args, "--log-dir", o.LogDir)
	}
	if o.ActiveWindow > 0 {
		args = append(args, "--active-window", o.ActiveWindow.String())
	}
	if o.PausedThreshold > 0 {
		args = append(args, "--paused-threshold", o.PausedThreshold.String())
	}
	if o.StateDir != "" {
		args = append(args, "--state-dir", o.StateDir)
	}
	if o.ConflictsRoot != "" {
		args = append(args, "--conflicts-root", o.ConflictsRoot)
	}
	return args
}

// Installer is the platform-specific autostart registration surface.
// Implementations MUST be idempotent.
type Installer interface {
	Install() error
	Uninstall() error
	PlatformLabel() string
}

// New validates opts and returns the platform-appropriate Installer.
func New(opts Options) (Installer, error) {
	if err := opts.Validate(); err != nil {
		return nil, err
	}
	return newPlatformInstaller(opts), nil
}

// unsupportedInstaller is the fallback used by trayinstall_default.go.
type unsupportedInstaller struct {
	platform string
}

func (u *unsupportedInstaller) Install() error {
	return fmt.Errorf("%w: %s; launch aplexicatray manually via your platform's login-items UI",
		ErrNotSupported, u.platform)
}
func (u *unsupportedInstaller) Uninstall() error {
	return fmt.Errorf("%w: %s", ErrNotSupported, u.platform)
}
func (u *unsupportedInstaller) PlatformLabel() string {
	return "not supported (" + u.platform + ")"
}
