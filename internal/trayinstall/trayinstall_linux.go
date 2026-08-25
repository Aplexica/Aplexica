//go:build linux

package trayinstall

import (
	"fmt"
	"os"
	"path/filepath"
)

const desktopFileName = "aplexica-tray.desktop"

func newPlatformInstaller(opts Options) Installer {
	return &xdgAutostartInstaller{opts: opts}
}

type xdgAutostartInstaller struct {
	opts        Options
	dirOverride string // tests override the default ~/.config/autostart path
}

func (x *xdgAutostartInstaller) PlatformLabel() string { return "XDG autostart (.desktop)" }

func (x *xdgAutostartInstaller) desktopPath() string {
	if x.dirOverride != "" {
		return filepath.Join(x.dirOverride, desktopFileName)
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "autostart", desktopFileName)
}

func (x *xdgAutostartInstaller) Install() error {
	path := x.desktopPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("trayinstall: autostart dir: %w", err)
	}
	if err := os.WriteFile(path, []byte(x.generateDesktop()), 0o644); err != nil {
		return fmt.Errorf("trayinstall: write .desktop: %w", err)
	}
	return nil
}

func (x *xdgAutostartInstaller) Uninstall() error {
	if err := os.Remove(x.desktopPath()); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("trayinstall: remove .desktop: %w", err)
	}
	return nil
}

func (x *xdgAutostartInstaller) generateDesktop() string {
	// XDG Autostart spec: a Type=Application entry placed in
	// $XDG_CONFIG_HOME/autostart/ is launched once when the user
	// session starts. X-GNOME-Autostart-enabled is a hint understood
	// by GNOME's autostart manager; on KDE/Plasma the desktop file
	// alone is sufficient.
	//
	// v0.40.0 flag-forwarding splices Options.extraArgs() into the
	// Exec= line. The XDG spec leaves shell-quoting handling up to
	// each desktop environment; values containing spaces should be
	// double-quoted. We keep things simple by single-quoting
	// individual values via desktopQuote.
	exec := x.opts.TrayPath
	for _, a := range x.opts.extraArgs() {
		exec += " " + desktopQuote(a)
	}
	return fmt.Sprintf(`[Desktop Entry]
Type=Application
Name=Aplexica Tray
Comment=Aplexica cross-agent sync indicator
Exec=%s
Terminal=false
X-GNOME-Autostart-enabled=true
`, exec)
}

// desktopQuote wraps a string in double-quotes when it contains
// whitespace or a special character. Aligns with the XDG Desktop
// Entry spec's "exec key" quoting rules; for our use the conservative
// "always single-pass quote when needed" approach is enough.
func desktopQuote(s string) string {
	for _, r := range s {
		if r == ' ' || r == '\t' || r == '"' || r == '\\' {
			// Backslash-escape any embedded double quotes / backslashes,
			// then wrap.
			var b []byte
			b = append(b, '"')
			for _, c := range s {
				if c == '"' || c == '\\' {
					b = append(b, '\\')
				}
				b = append(b, byte(c))
			}
			b = append(b, '"')
			return string(b)
		}
	}
	return s
}
