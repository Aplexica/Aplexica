//go:build darwin

package daemon

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLaunchdControllerManagedTracksThePlist(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	ctl := ServiceControllerForPlatform()
	if ctl == nil {
		t.Fatal("darwin must ship a service controller")
	}
	if ctl.Managed() {
		t.Fatal("Managed must be false when no LaunchAgent is installed")
	}

	agents := filepath.Join(home, "Library", "LaunchAgents")
	if err := os.MkdirAll(agents, 0o755); err != nil {
		t.Fatal(err)
	}
	plist := filepath.Join(agents, launchdLabel+".plist")
	if err := os.WriteFile(plist, []byte("<plist/>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !ctl.Managed() {
		t.Fatalf("Managed must be true once %s exists", plist)
	}
}

func TestLaunchdControllerLabelMatchesTheInstaller(t *testing.T) {
	ctl := ServiceControllerForPlatform()
	installer := &launchdInstaller{}
	if ctl.Label() != installer.PlatformLabel() {
		t.Fatalf("controller label %q must match installer label %q", ctl.Label(), installer.PlatformLabel())
	}
}
