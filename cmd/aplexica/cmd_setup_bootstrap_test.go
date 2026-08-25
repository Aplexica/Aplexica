package main

import (
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
)

// fakeSetupRunner records the subcommands the bootstrap would shell out to and
// can simulate a failure on the first call whose joined args start with failOn.
type fakeSetupRunner struct {
	calls   [][]string
	failOn  string
	failErr error
}

func (f *fakeSetupRunner) run(args ...string) error {
	f.calls = append(f.calls, append([]string(nil), args...))
	if f.failOn != "" && strings.HasPrefix(strings.Join(args, " "), f.failOn) {
		return f.failErr
	}
	return nil
}

func TestRunSetupBootstrap_LocalOnly(t *testing.T) {
	f := &fakeSetupRunner{}
	if err := runSetupBootstrap(f, "/home/u", true, "", io.Discard); err != nil {
		t.Fatalf("runSetupBootstrap: %v", err)
	}
	want := [][]string{{"daemon", "install", "--dir", "/home/u", "--tray=true"}}
	if !reflect.DeepEqual(f.calls, want) {
		t.Fatalf("calls = %v, want %v", f.calls, want)
	}
}

func TestRunSetupBootstrap_TrayFalse(t *testing.T) {
	f := &fakeSetupRunner{}
	if err := runSetupBootstrap(f, "/home/u", false, "", io.Discard); err != nil {
		t.Fatalf("runSetupBootstrap: %v", err)
	}
	want := [][]string{{"daemon", "install", "--dir", "/home/u", "--tray=false"}}
	if !reflect.DeepEqual(f.calls, want) {
		t.Fatalf("calls = %v, want %v", f.calls, want)
	}
}

func TestRunSetupBootstrap_WithCloudInstallsPluginFirst(t *testing.T) {
	f := &fakeSetupRunner{}
	if err := runSetupBootstrap(f, "/home/u", false, "/opt/plugin", io.Discard); err != nil {
		t.Fatalf("runSetupBootstrap: %v", err)
	}
	// Cloud plugin MUST be installed before the daemon, so the daemon spawns
	// it as it starts.
	want := [][]string{
		{"remote", "install", "/opt/plugin"},
		{"daemon", "install", "--dir", "/home/u", "--tray=false"},
	}
	if !reflect.DeepEqual(f.calls, want) {
		t.Fatalf("calls = %v, want %v", f.calls, want)
	}
}

func TestRunSetupBootstrap_PropagatesExactCloudTrustBootstrap(t *testing.T) {
	f := &fakeSetupRunner{}
	trust := cloudPluginBootstrapOptions{InitialSequence: 2, InitialRollbackFloor: 1,
		InitialInventorySHA256: strings.Repeat("a", 64)}
	if err := runSetupBootstrapWithTrust(f, "/home/u", false, "/opt/plugin", trust, io.Discard); err != nil {
		t.Fatal(err)
	}
	want := []string{"remote", "install", "/opt/plugin", "--initial-sequence", "2", "--initial-rollback-floor", "1", "--initial-inventory-sha256", strings.Repeat("a", 64)}
	if !reflect.DeepEqual(f.calls[0], want) {
		t.Fatalf("cloud install = %v, want %v", f.calls[0], want)
	}
}

func TestRunSetupBootstrap_StopsWhenCloudInstallFails(t *testing.T) {
	f := &fakeSetupRunner{failOn: "remote install", failErr: errors.New("boom")}
	err := runSetupBootstrap(f, "/home/u", true, "/opt/plugin", io.Discard)
	if err == nil || !strings.Contains(err.Error(), "cloud plugin") {
		t.Fatalf("want cloud plugin error, got %v", err)
	}
	// Must NOT proceed to daemon install after a cloud failure.
	if len(f.calls) != 1 {
		t.Fatalf("expected to stop after remote install, calls=%v", f.calls)
	}
}

func TestRunSetupBootstrap_StopsWhenDaemonInstallFails(t *testing.T) {
	f := &fakeSetupRunner{failOn: "daemon install", failErr: errors.New("boom")}
	err := runSetupBootstrap(f, "/home/u", true, "", io.Discard)
	if err == nil || !strings.Contains(err.Error(), "daemon install") {
		t.Fatalf("want daemon install error, got %v", err)
	}
}
