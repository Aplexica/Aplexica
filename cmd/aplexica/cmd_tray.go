package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/aplexica/aplexica/internal/daemon"
	"github.com/aplexica/aplexica/internal/i18n"
	"github.com/aplexica/aplexica/internal/trayinstall"
	"github.com/spf13/cobra"
)

// updateDaemonConfig loads the sparse daemon config, applies update, and
// writes it back without discarding unrelated user settings.
//
// stateDir == "" → defaults to ~/.aplexica/state, matching the daemon's
// own default.
func updateDaemonConfig(stateDir string, update func(*daemon.Config)) error {
	if stateDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("locate home: %w", err)
		}
		stateDir = filepath.Join(home, ".aplexica", "state")
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return fmt.Errorf("mkdir state dir: %w", err)
	}
	cfgPath := filepath.Join(stateDir, "config.json")
	cfg, err := daemon.LoadConfig(cfgPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	update(cfg)
	if err := daemon.WriteConfig(cfgPath, cfg); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return nil
}

// setTrayEnabledInConfig persists tray.enabled = enabled to the daemon
// config at <state-dir>/config.json. v0.50.0 — used by `aplexica tray
// install` to mark the tray enabled, and by `aplexica tray uninstall`
// to mark the user's opt-out (so subsequent aplexicatray launches honor
// the explicit choice instead of falling back to the platform default).
func setTrayEnabledInConfig(stateDir string, enabled bool) error {
	return updateDaemonConfig(stateDir, func(cfg *daemon.Config) {
		cfg.Tray.Enabled = &enabled
	})
}

// setDaemonWatchDirInConfig records the directory selected by `daemon
// install`. The service definition also carries --dir, but the tray cannot
// inspect every platform's service manager when the daemon is stopped. Keeping
// the value in the shared sparse config lets "Start daemon" recover it.
func setDaemonWatchDirInConfig(stateDir, watchedDir string) error {
	return updateDaemonConfig(stateDir, func(cfg *daemon.Config) {
		cfg.Dir = watchedDir
	})
}

// daemonServiceInstalled returns true when the user-scope daemon
// service definition exists at the canonical per-platform path.
// Best-effort — returns false on any error (which the caller treats
// as "not installed, print warning").
//
//   - macOS  : ~/Library/LaunchAgents/com.aplexica.aplexicad.plist
//   - Linux  : ~/.config/systemd/user/aplexicad.service
//   - Windows: stub returns true (Windows SCM service registration is
//     gated by Admin and harder to probe without elevating;
//     skip the warning rather than emit a spurious one)
func daemonServiceInstalled() bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return true // can't check — don't warn
	}
	var p string
	switch runtime.GOOS {
	case "darwin":
		p = filepath.Join(home, "Library", "LaunchAgents", "com.aplexica.aplexicad.plist")
	case "linux":
		p = filepath.Join(home, ".config", "systemd", "user", "aplexicad.service")
	case "windows":
		return true // see comment above
	default:
		return true
	}
	if _, err := os.Stat(p); err == nil {
		return true
	}
	return false
}

var (
	trayFlagAplexica        string
	trayFlagInterval        time.Duration
	trayFlagLogDir          string
	trayFlagActiveWindow    time.Duration
	trayFlagPausedThreshold time.Duration
	trayFlagStateDir        string
	trayFlagConflictsRoot   string
)

// trayCmd hosts the autostart-installer subcommands for the
// aplexicatray indicator. Lives on the main aplexica binary (not
// tag-gated) so the verb is discoverable from every install — users
// with a non-tray build still see the verb and the friendly
// "build aplexicatray first" hint.
var trayCmd = &cobra.Command{
	Use:   "tray",
	Short: "Manage the Aplexica tray indicator's per-user autostart entry",
	Long: `aplexica tray install/uninstall registers (or removes) a per-user
LOGIN-only autostart entry for the aplexicatray binary, which is the
cross-platform system-tray indicator for the daemon.

Distinct from "aplexica daemon install": the daemon registers an
always-running background service; the tray registers a login-only
GUI helper that the user can quit without it respawning.

Platforms:
  - macOS  : ~/Library/LaunchAgents/com.aplexica.tray.plist
  - Linux  : ~/.config/autostart/aplexica-tray.desktop
  - Windows: %APPDATA%\Microsoft\Windows\Start Menu\Programs\Startup\
             Aplexica Tray.lnk

Packaged installs place aplexicatray next to aplexica and the command
prefers that sibling binary. Source builds may also provide aplexicatray
on PATH; build it with "make tray" (or "go build -tags tray -o ...
./cmd/aplexicatray").`,
}

var trayInstallCmd = &cobra.Command{
	Use:   "install",
	Short: "Register aplexicatray to auto-start at login",
	RunE: func(cmd *cobra.Command, args []string) error {
		self, err := persistentExecutable()
		if err != nil {
			return fmt.Errorf("tray install: locate persistent aplexica executable: %w", err)
		}
		path, err := resolveTrayPath(self)
		if err != nil {
			return fmt.Errorf("tray install: aplexicatray not found next to aplexica or on PATH: %w", err)
		}
		// Best-effort daemon-installed check. The tray autostart is
		// pointless if the daemon isn't going to be running — print a
		// soft warning but proceed regardless (the user might be on
		// a developer workflow that starts the daemon by hand, OR
		// they might be installing the tray before the daemon
		// intentionally). Per-platform paths mirror
		// internal/daemon/install_<goos>.go's locations.
		if !daemonServiceInstalled() {
			fmt.Fprintln(cmd.ErrOrStderr(),
				"tray install: WARNING — `aplexica daemon install` does not appear to have been run. "+
					"The tray autostart is a no-op until the daemon is running. "+
					"Run `aplexica daemon install --dir <path>` first, OR start the daemon manually with "+
					"`aplexica daemon start --dir <path>` each session.")
		}
		aplexicaPath := trayFlagAplexica
		if aplexicaPath == "" {
			aplexicaPath = self
		}
		inst, err := trayinstall.New(trayinstall.Options{
			TrayPath:        path,
			AplexicaPath:    aplexicaPath,
			Interval:        trayFlagInterval,
			LogDir:          trayFlagLogDir,
			ActiveWindow:    trayFlagActiveWindow,
			PausedThreshold: trayFlagPausedThreshold,
			StateDir:        trayFlagStateDir,
			ConflictsRoot:   trayFlagConflictsRoot,
		})
		if err != nil {
			return err
		}
		if err := inst.Install(); err != nil {
			return fmt.Errorf("tray install (%s): %w", inst.PlatformLabel(), err)
		}
		// v0.50.0: persist tray.enabled = true so future aplexicatray
		// launches don't refuse to run because of an earlier opt-out.
		// Best-effort — surface as a warning, not a hard error, since
		// the autostart entry IS installed.
		if err := setTrayEnabledInConfig(trayFlagStateDir, true); err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(),
				"tray install: WARNING — installed autostart entry but failed to set tray.enabled in config: %v\n",
				err)
		}
		fmt.Fprint(cmd.OutOrStdout(),
			i18n.Tf("cmd_tray_installed_format", inst.PlatformLabel(), path))
		return nil
	},
}

var trayUninstallCmd = &cobra.Command{
	Use:   "uninstall",
	Short: "Remove the tray indicator's autostart entry",
	RunE: func(cmd *cobra.Command, args []string) error {
		// For uninstall, TrayPath isn't used by any backend — supply a
		// placeholder so the validator passes. Matches the daemon
		// uninstaller's pattern in cmd_daemon.go.
		inst, err := trayinstall.New(trayinstall.Options{TrayPath: "/placeholder"})
		if err != nil {
			return err
		}
		if err := inst.Uninstall(); err != nil {
			return fmt.Errorf("tray uninstall (%s): %w", inst.PlatformLabel(), err)
		}
		// v0.50.0: explicit opt-out. Persist tray.enabled = false so
		// the tray binary refuses to start even when launched manually
		// (e.g., the user might have a stale alias or shell history).
		// Best-effort — surface as a warning, not a hard error.
		if err := setTrayEnabledInConfig(trayFlagStateDir, false); err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(),
				"tray uninstall: WARNING — removed autostart entry but failed to set tray.enabled=false in config: %v\n",
				err)
		}
		fmt.Fprint(cmd.OutOrStdout(),
			i18n.Tf("cmd_tray_uninstalled_format", inst.PlatformLabel()))
		return nil
	},
}

func init() {
	// Optional flag-forwarding for the autostart entry (v0.40.0). When
	// any of these is set, the corresponding `--<flag>` is baked into
	// the generated plist / .desktop / .lnk. Defaults (empty / 0) mean
	// "let aplexicatray use its own defaults" — backward-compatible
	// with v0.37.0's argument-free autostart entry.
	trayInstallCmd.Flags().StringVar(&trayFlagAplexica, "aplexica", "",
		"path to the aplexica CLI baked into the autostart entry (default: current aplexica executable)")
	trayInstallCmd.Flags().DurationVar(&trayFlagInterval, "interval", 0,
		"polling interval forwarded to aplexicatray (0 = tray uses its own default)")
	trayInstallCmd.Flags().StringVar(&trayFlagLogDir, "log-dir", "",
		"daemon log directory the tray's 'Open logs' menu reveals (default: ~/.aplexica/logs)")
	trayInstallCmd.Flags().DurationVar(&trayFlagActiveWindow, "active-window", 0,
		"snapshot age below which the tray shows the Active state (0 = default)")
	trayInstallCmd.Flags().DurationVar(&trayFlagPausedThreshold, "paused-threshold", 0,
		"deprecated quiet threshold forwarded to aplexicatray for compatibility (0 = default)")
	trayInstallCmd.Flags().StringVar(&trayFlagStateDir, "state-dir", "",
		"daemon state directory the tray watches (default: aplexica's own default, ~/.aplexica/state)")
	trayInstallCmd.Flags().StringVar(&trayFlagConflictsRoot, "conflicts-root", "",
		"conflicts store root the tray watches (default: <state-dir>/conflicts)")

	trayCmd.AddCommand(trayInstallCmd)
	trayCmd.AddCommand(trayUninstallCmd)
	rootCmd.AddCommand(trayCmd)
}
