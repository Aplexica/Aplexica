//go:build linux

package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// leakedSystemctlCalls records attempts to reach the real systemctl from
// inside the test binary. It must stay empty: see TestMain.
var leakedSystemctlCalls struct {
	mu    sync.Mutex
	calls [][]string
}

// TestMain replaces execSystemctl for the whole test binary so that no test
// can start, stop or restart the real aplexicad.service of the machine
// running the suite. A test that exercises the controller installs a
// recorder with recordSystemctl instead.
func TestMain(m *testing.M) {
	execSystemctl = func(args ...string) ([]byte, error) {
		leakedSystemctlCalls.mu.Lock()
		defer leakedSystemctlCalls.mu.Unlock()
		leakedSystemctlCalls.calls = append(leakedSystemctlCalls.calls, append([]string(nil), args...))
		return nil, fmt.Errorf("test attempted to run the real `systemctl %s`", strings.Join(args, " "))
	}
	code := m.Run()
	leakedSystemctlCalls.mu.Lock()
	leaked := leakedSystemctlCalls.calls
	leakedSystemctlCalls.mu.Unlock()
	if len(leaked) > 0 {
		fmt.Fprintf(os.Stderr,
			"\nFAIL: %d systemctl invocation(s) escaped the test hook and would have reached\n"+
				"the real %s on this machine. Use recordSystemctl instead:\n  %v\n",
			len(leaked), systemdServiceName, leaked)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

// systemctlRecorder captures the argv of every systemctl invocation instead
// of running it, and answers each one with out and err.
type systemctlRecorder struct {
	calls [][]string
	out   []byte
	err   error
}

// recordSystemctl swaps the controller's systemctl hook for a recorder for
// the duration of one test, restoring TestMain's poison afterwards.
func recordSystemctl(t *testing.T) *systemctlRecorder {
	t.Helper()
	rec := &systemctlRecorder{}
	prev := execSystemctl
	execSystemctl = func(args ...string) ([]byte, error) {
		rec.calls = append(rec.calls, append([]string(nil), args...))
		return rec.out, rec.err
	}
	t.Cleanup(func() { execSystemctl = prev })
	return rec
}

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

// Each verb must reach systemd as exactly one `systemctl --user <verb>` on
// the installed unit. For stop that is the point of the change: stopping the
// unit, rather than the process behind it, is what leaves systemd agreeing
// that the daemon is down, and starting the unit is what keeps the daemon
// under systemd instead of in the caller's session.
func TestSystemdControllerRunsOneSystemctlCommandPerVerb(t *testing.T) {
	ctl := ServiceControllerForPlatform()
	for _, tc := range []struct {
		verb string
		run  func() error
	}{
		{"start", ctl.Start},
		{"stop", ctl.Stop},
		{"restart", ctl.Restart},
	} {
		t.Run(tc.verb, func(t *testing.T) {
			rec := recordSystemctl(t)
			if err := tc.run(); err != nil {
				t.Fatal(err)
			}
			want := [][]string{{"--user", tc.verb, "aplexicad.service"}}
			if !reflect.DeepEqual(rec.calls, want) {
				t.Fatalf("ran systemctl %v, want %v", rec.calls, want)
			}
		})
	}
}

// A failed systemctl call must come back as an error naming the command and
// carrying systemctl's own explanation, which the CLI prints as-is. A user
// on a machine without a systemd user instance has nothing else to go on.
func TestSystemdControllerReportsSystemctlFailures(t *testing.T) {
	ctl := ServiceControllerForPlatform()
	for _, tc := range []struct {
		verb string
		run  func() error
	}{
		{"start", ctl.Start},
		{"stop", ctl.Stop},
	} {
		t.Run(tc.verb, func(t *testing.T) {
			rec := recordSystemctl(t)
			rec.out = []byte("Failed to connect to bus: No medium found\n")
			rec.err = errors.New("exit status 1")

			err := tc.run()
			if err == nil {
				t.Fatal("a failed systemctl call must be reported")
			}
			for _, want := range []string{
				"systemctl --user " + tc.verb + " aplexicad.service",
				"exit status 1",
				"Failed to connect to bus: No medium found",
			} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q", err, want)
				}
			}
		})
	}
}
