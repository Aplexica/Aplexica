//go:build darwin

package trayinstall

import (
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const launchdTrayLabel = "com.aplexica.tray"

// trayLaunchdLogName is the file launchd's StandardOutPath/StandardErrorPath
// redirect points at, inside the tray's configured log directory.
//
// One file for BOTH streams: the tray writes diagnostics exclusively through
// the stdlib `log` package (stderr), so a separate stdout file would always be
// empty, and a single file keeps ordering intact if anything ever prints to
// stdout. The name is tray-namespaced and carries `.launchd` so it is
// unmistakably (a) not a daemon log and (b) owned by launchd's redirect rather
// than by the daemon's own rotator — internal/daemon/log.go writes
// aplexicad.log in this same directory and rotates it out from under itself,
// which would corrupt a launchd redirect fd pointed at the same path.
const trayLaunchdLogName = "tray.launchd.log"

// execLaunchctl is the SINGLE choke point through which this file shells out
// to launchctl. It is a package variable only so the darwin tests can replace
// it, and that indirection is load-bearing rather than cosmetic:
//
// `launchctl unload -w <plist>` resolves the job by the Label INSIDE the
// plist, not by the plist's path. A test that writes a production-labelled
// plist into t.TempDir() and then calls Install/Uninstall therefore boots out
// AND persistently disables the real gui/$UID/com.aplexica.tray agent on
// whatever machine ran the test. The disable is written to
// /var/db/com.apple.xpc.launchd/disabled.<uid>.plist and survives logout and
// reboot, so the menu-bar icon never comes back until someone runs
// `launchctl enable` by hand.
//
// TestMain in trayinstall_darwin_test.go poisons this hook so no test in this
// package can reach the real launchctl again; TestDarwinLaunchctlHasOneCallSite
// keeps every future call site funnelled through here.
var execLaunchctl = func(args ...string) ([]byte, error) {
	return exec.Command("launchctl", args...).CombinedOutput()
}

func newPlatformInstaller(opts Options) Installer {
	return &launchdTrayInstaller{opts: opts}
}

type launchdTrayInstaller struct {
	opts             Options
	plistDirOverride string // tests override the default ~/Library/LaunchAgents path
}

func (l *launchdTrayInstaller) PlatformLabel() string { return "launchd LaunchAgent (tray)" }

func (l *launchdTrayInstaller) plistPath() string {
	if l.plistDirOverride != "" {
		return filepath.Join(l.plistDirOverride, launchdTrayLabel+".plist")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", launchdTrayLabel+".plist")
}

func (l *launchdTrayInstaller) Install() error {
	path := l.plistPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("trayinstall: plist dir: %w", err)
	}
	content, err := l.generatePlistWithLog(l.ensuredLaunchdLogPath())
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		return fmt.Errorf("trayinstall: write plist: %w", err)
	}
	// Best-effort unload of any prior registration. We use `launchctl
	// unload` (the legacy form) for max-compat across 10.13+ macOS
	// releases. If the user is on modern macOS where unload is a
	// thin wrapper around bootout, this still works.
	_, _ = execLaunchctl("unload", path)
	if out, err := execLaunchctl("load", "-w", path); err != nil {
		return fmt.Errorf("trayinstall: launchctl load: %w (output: %s)",
			err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (l *launchdTrayInstaller) Uninstall() error {
	path := l.plistPath()
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil // idempotent
	}
	_, _ = execLaunchctl("unload", "-w", path)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("trayinstall: remove plist: %w", err)
	}
	return nil
}

// launchdLogPath returns the absolute file launchd should redirect the tray's
// stdio into, or "" when no such file can be named.
//
// It resolves the SAME directory the tray binary itself uses: the configured
// LogDir, else ~/.aplexica/logs (cmd/aplexicatray/main.go's --log-dir default).
// Keeping them identical means the tray's own "Open Logs" menu item — which
// just opens that directory — now shows the tray's log alongside the daemon's.
//
// Returns "" for a relative dir or an unresolvable home: launchd requires
// absolute Standard*Path values, and a path it cannot open is worse than no
// redirect at all.
func (l *launchdTrayInstaller) launchdLogPath() string {
	dir := l.opts.LogDir
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return ""
		}
		dir = filepath.Join(home, ".aplexica", "logs")
	}
	if !filepath.IsAbs(dir) {
		return ""
	}
	return filepath.Join(dir, trayLaunchdLogName)
}

// ensuredLaunchdLogPath is launchdLogPath plus the safety step that makes the
// redirect installable: launchd opens StandardOutPath/StandardErrorPath itself,
// as the user, and a job whose stdio it cannot open can fail to spawn. On a
// fresh install nothing has created ~/.aplexica/logs yet, so create it here.
//
// MkdirAll is necessary but NOT sufficient. It returns nil for ANY directory
// that already exists, whatever its mode or owner — so "the directory exists"
// says nothing about whether a file can be created in it. The only honest
// check is to perform launchd's own open (O_WRONLY|O_APPEND|O_CREAT) and keep
// the keys only if it succeeds. Its side effect — an empty log file — is
// exactly what launchd would have created on the next launch anyway.
//
// If the directory cannot be created, or the file cannot be opened, we return
// "" and the plist simply omits the keys: losing the diagnostics is a bad day,
// but a tray that never launches is strictly worse than the bug this redirect
// exists to diagnose.
func (l *launchdTrayInstaller) ensuredLaunchdLogPath() string {
	path := l.launchdLogPath()
	if path == "" {
		return ""
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return ""
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return ""
	}
	_ = f.Close()
	return path
}

// generatePlist renders the LaunchAgent for the resolved log destination
// WITHOUT creating anything on disk. Install uses generatePlistWithLog with an
// ensured (created) directory instead.
func (l *launchdTrayInstaller) generatePlist() ([]byte, error) {
	return l.generatePlistWithLog(l.launchdLogPath())
}

func (l *launchdTrayInstaller) generatePlistWithLog(logPath string) ([]byte, error) {
	// ProgramArguments[0] is the tray binary path; v0.40.0 adds
	// flag-forwarding via Options.extraArgs() so users can bake their
	// chosen --interval / --log-dir / etc. into the autostart entry.
	args := append([]string{l.opts.TrayPath}, l.opts.extraArgs()...)
	var argsXML strings.Builder
	for _, a := range args {
		argsXML.WriteString("    <string>")
		xml.EscapeText(&argsXML, []byte(a)) //nolint:errcheck
		argsXML.WriteString("</string>\n")
	}

	// LimitLoadToSessionType=Aqua means "GUI sessions only" — exactly
	// the right scope for a tray indicator. RunAtLoad=true launches at
	// login.
	//
	// KeepAlive{SuccessfulExit:false} makes launchd RESPAWN the tray
	// whenever it exits ABNORMALLY (a crash, a kill, or — the common case
	// — the tray quitting itself because a daemon restart closed its
	// status feed), but NOT when the user quits it cleanly (the Quit menu
	// item returns exit 0). This is what stops "every daemon restart kills
	// the tray and the icon never comes back." Earlier this key was
	// omitted entirely to honor user-quit, which also disabled crash
	// recovery; SuccessfulExit:false gives us both. (The tray also now
	// reconnects to a restarted daemon instead of quitting — see
	// cmd/aplexicatray supervises its status feed — so KeepAlive is the
	// belt-and-suspenders for genuine crashes.)
	// Stdio redirect. Without these keys launchd sends the job's stdout and
	// stderr to /dev/null — and the tray logs EXCLUSIVELY to stderr via the
	// stdlib `log` package, so every diagnostic it ever emitted under the
	// LaunchAgent (including why it shut down) was destroyed. Both streams go
	// to one file; see trayLaunchdLogName for why that name.
	//
	// Growth bound: the tray emits a handful of lines per lifetime (startup
	// resolution, subprocess spawns, menu-action errors, one shutdown line),
	// and launchd truncates nothing, so this file grows by ~a few hundred
	// bytes per login. That is small enough to leave unrotated; the daemon's
	// far chattier log stays on its own rotator in the same directory.
	var stdioXML string
	if logPath != "" {
		var escaped strings.Builder
		xml.EscapeText(&escaped, []byte(logPath)) //nolint:errcheck
		stdioXML = fmt.Sprintf(`  <key>StandardOutPath</key>
  <string>%s</string>
  <key>StandardErrorPath</key>
  <string>%s</string>
`, escaped.String(), escaped.String())
	}

	xmlBody := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>%s</string>
  <key>ProgramArguments</key>
  <array>
%s  </array>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <dict>
    <key>SuccessfulExit</key>
    <false/>
  </dict>
  <key>LimitLoadToSessionType</key>
  <string>Aqua</string>
%s</dict>
</plist>
`, launchdTrayLabel, argsXML.String(), stdioXML)
	return []byte(xmlBody), nil
}
