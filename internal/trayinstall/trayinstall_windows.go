//go:build windows

package trayinstall

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const (
	lnkFileName             = "Aplexica Tray.lnk"
	legacyScheduledTaskName = "Aplexica Tray"
)

func newPlatformInstaller(opts Options) Installer {
	return &startupLnkInstaller{opts: opts}
}

type startupLnkInstaller struct {
	opts        Options
	dirOverride string // tests can substitute a fake Startup dir
}

func (s *startupLnkInstaller) PlatformLabel() string { return "Windows Startup (.lnk)" }

// startupDir returns the per-user Startup folder. The canonical path is
//
//	%APPDATA%\Microsoft\Windows\Start Menu\Programs\Startup
//
// which on Windows resolves to e.g. C:\Users\<user>\AppData\Roaming\...
// when the user has roaming profiles enabled. Tests can override.
func (s *startupLnkInstaller) startupDir() string {
	if s.dirOverride != "" {
		return s.dirOverride
	}
	if appdata := os.Getenv("APPDATA"); appdata != "" {
		return filepath.Join(appdata, "Microsoft", "Windows", "Start Menu", "Programs", "Startup")
	}
	// Last-resort fallback if APPDATA isn't set (unusual on Windows).
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "AppData", "Roaming", "Microsoft", "Windows", "Start Menu", "Programs", "Startup")
}

func (s *startupLnkInstaller) lnkPath() string {
	return filepath.Join(s.startupDir(), lnkFileName)
}

func (s *startupLnkInstaller) Install() error {
	if err := os.MkdirAll(s.startupDir(), 0o755); err != nil {
		return fmt.Errorf("trayinstall: startup dir: %w", err)
	}
	// Build a PowerShell one-liner that creates the .lnk via the
	// WScript.Shell COM object. Quote the paths defensively because
	// %APPDATA% (and the user's installation path) may contain spaces.
	lnk := s.lnkPath()
	// v0.40.0 flag-forwarding: any non-zero option becomes an argv
	// fragment on the shortcut. We assemble a single space-separated
	// argument string and feed it into the .lnk's Arguments property.
	// Individual values are PowerShell-quoted via psQuote and also
	// double-quoted in case Windows execution-time argument parsing
	// re-splits on whitespace.
	var argsLine string
	for _, a := range s.opts.extraArgs() {
		if argsLine != "" {
			argsLine += " "
		}
		argsLine += winQuoteArg(a)
	}

	psScript := fmt.Sprintf(
		legacyScheduledTaskCleanupScript()+
			`$ws = New-Object -ComObject WScript.Shell; `+
			`$s = $ws.CreateShortcut(%s); `+
			`$s.TargetPath = %s; `+
			`$s.WorkingDirectory = %s; `+
			`$s.Arguments = %s; `+
			`$s.Description = 'Aplexica cross-agent sync indicator'; `+
			`$s.Save()`,
		psQuote(lnk), psQuote(s.opts.TrayPath), psQuote(filepath.Dir(s.opts.TrayPath)),
		psQuote(argsLine))

	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", psScript)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("trayinstall: powershell create-shortcut: %w (output: %s)",
			err, strings.TrimSpace(string(out)))
	}
	return nil
}

func legacyScheduledTaskCleanupScript() string {
	return fmt.Sprintf(
		`Get-ScheduledTask -TaskName %s -ErrorAction SilentlyContinue | `+
			`Unregister-ScheduledTask -Confirm:$false -ErrorAction Stop; `,
		psQuote(legacyScheduledTaskName),
	)
}

func (s *startupLnkInstaller) Uninstall() error {
	if err := os.Remove(s.lnkPath()); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("trayinstall: remove .lnk: %w", err)
	}
	return nil
}

// psQuote wraps a string in PowerShell single-quotes, escaping any
// embedded single quotes by doubling them (PowerShell's literal-string
// rule). Yields a fragment that drops into a PowerShell -Command string
// as a single argument.
func psQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// winQuoteArg wraps an argument for the .lnk's Arguments string so
// Windows CommandLineToArgvW re-splits it as one argv element. If the
// value contains whitespace or double-quotes, wrap in double-quotes
// and backslash-escape internal quotes per the standard MSVCRT rules.
func winQuoteArg(s string) string {
	if s == "" {
		return `""`
	}
	needsQuoting := false
	for _, r := range s {
		if r == ' ' || r == '\t' || r == '"' {
			needsQuoting = true
			break
		}
	}
	if !needsQuoting {
		return s
	}
	var b []byte
	b = append(b, '"')
	for _, c := range s {
		if c == '"' {
			b = append(b, '\\')
		}
		b = append(b, byte(c))
	}
	b = append(b, '"')
	return string(b)
}
