//go:build linux

package daemon

import (
	"os"
	"path/filepath"
	"testing"
)

// Managed is the gate that decides whether `daemon restart` hands the job to
// systemd or self-execs a replacement. Getting it wrong in the false
// direction reintroduces the orphan that dies with the caller's session, so
// it must track the unit file exactly.
func TestSystemdControllerManagedTracksTheUnitFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	ctl := ServiceControllerForPlatform()
	if ctl == nil {
		t.Fatal("linux must ship a service controller")
	}
	if ctl.Managed() {
		t.Fatal("Managed must be false when no unit is installed")
	}

	unitDir := filepath.Join(home, ".config", "systemd", "user")
	if err := os.MkdirAll(unitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	unit := filepath.Join(unitDir, systemdServiceName)
	if err := os.WriteFile(unit, []byte("[Unit]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !ctl.Managed() {
		t.Fatalf("Managed must be true once %s exists", unit)
	}

	// A directory at the unit path is not a unit. Without the regular-file
	// check a stray directory would route restarts to systemd, which would
	// then fail on a unit it does not have.
	if err := os.Remove(unit); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(unit, 0o755); err != nil {
		t.Fatal(err)
	}
	if ctl.Managed() {
		t.Fatal("a directory at the unit path must not count as an installed unit")
	}
}

func TestSystemdControllerLabelMatchesTheInstaller(t *testing.T) {
	ctl := ServiceControllerForPlatform()
	installer := &systemdInstaller{}
	if ctl.Label() != installer.PlatformLabel() {
		t.Fatalf("controller label %q must match installer label %q so status output uses one vocabulary",
			ctl.Label(), installer.PlatformLabel())
	}
}
