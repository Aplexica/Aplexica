//go:build linux

package trayinstall

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestLinuxDesktopGeneration(t *testing.T) {
	x := &xdgAutostartInstaller{opts: Options{TrayPath: "/usr/local/bin/aplexicatray"}}
	s := x.generateDesktop()
	for _, want := range []string{
		"[Desktop Entry]",
		"Type=Application",
		"Name=Aplexica Tray",
		"Exec=/usr/local/bin/aplexicatray",
		"Terminal=false",
		"X-GNOME-Autostart-enabled=true",
	} {
		if !strings.Contains(s, want) {
			t.Errorf(".desktop missing %q\n--- body ---\n%s", want, s)
		}
	}
}

// TestLinuxDesktopFlagForwarding (v0.40.0) — same shape as the darwin
// test, but for .desktop's Exec= line.
func TestLinuxDesktopFlagForwarding(t *testing.T) {
	x := &xdgAutostartInstaller{opts: Options{
		TrayPath:        "/usr/local/bin/aplexicatray",
		AplexicaPath:    "/opt/aplexica/bin/aplexica",
		Interval:        2 * time.Second,
		LogDir:          "/var/log/aplexica",
		ActiveWindow:    45 * time.Second,
		PausedThreshold: 10 * time.Minute,
	}}
	s := x.generateDesktop()
	for _, want := range []string{
		"--aplexica /opt/aplexica/bin/aplexica",
		"--interval 2s",
		"--log-dir /var/log/aplexica",
		"--active-window 45s",
		"--paused-threshold 10m0s",
	} {
		if !strings.Contains(s, want) {
			t.Errorf(".desktop missing forwarded fragment %q\n--- body ---\n%s", want, s)
		}
	}
}

func TestLinuxDesktopQuotingSpaces(t *testing.T) {
	x := &xdgAutostartInstaller{opts: Options{
		TrayPath: "/usr/local/bin/aplexicatray",
		LogDir:   "/var/log with spaces/aplexica",
	}}
	s := x.generateDesktop()
	if !strings.Contains(s, `--log-dir "/var/log with spaces/aplexica"`) {
		t.Errorf(".desktop did not quote spaces in log-dir:\n%s", s)
	}
}

func TestLinuxInstallRoundtrip(t *testing.T) {
	dir := t.TempDir()
	x := &xdgAutostartInstaller{
		opts:        Options{TrayPath: "/fake/aplexicatray"},
		dirOverride: dir,
	}
	if err := x.Install(); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if _, err := os.Stat(x.desktopPath()); err != nil {
		t.Fatalf("desktop file not written: %v", err)
	}
	// Re-install should overwrite cleanly (idempotent).
	if err := x.Install(); err != nil {
		t.Fatalf("Install (second): %v", err)
	}
	if err := x.Uninstall(); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if _, err := os.Stat(x.desktopPath()); !os.IsNotExist(err) {
		t.Errorf("desktop file not removed: stat err=%v", err)
	}
	if err := x.Uninstall(); err != nil {
		t.Errorf("Uninstall idempotency: %v", err)
	}
}
