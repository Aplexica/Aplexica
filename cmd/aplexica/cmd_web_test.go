package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/aplexica/aplexica/internal/daemon"
)

// TestSetWebEnabledRoundTrip exercises the on-disk flip the enable/
// disable subcommands perform. Doesn't spin up a daemon — we just
// confirm that the config file moves between the two states.
func TestSetWebEnabledRoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("APLEXICA_STATE_DIR", dir)

	// Pre-existing config with no [web] section
	pre := &daemon.Config{Dir: "/some/watched/dir", LogLevel: "info"}
	cfgPath := filepath.Join(dir, "config.json")
	if err := daemon.WriteConfig(cfgPath, pre); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	// Disable
	if err := setWebEnabled(false); err != nil {
		t.Fatalf("setWebEnabled(false): %v", err)
	}
	after1, err := daemon.LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if after1.Web.Enabled == nil || *after1.Web.Enabled {
		t.Fatalf("Web.Enabled = %v, want pointer-to-false", after1.Web.Enabled)
	}

	// Enable
	if err := setWebEnabled(true); err != nil {
		t.Fatalf("setWebEnabled(true): %v", err)
	}
	after2, _ := daemon.LoadConfig(cfgPath)
	if after2.Web.Enabled == nil || !*after2.Web.Enabled {
		t.Fatalf("Web.Enabled = %v, want pointer-to-true", after2.Web.Enabled)
	}
}

func TestSetWebEnabledCreatesStateDirIfMissing(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, "deeply", "nested", "state")
	t.Setenv("APLEXICA_STATE_DIR", stateDir)

	if err := setWebEnabled(true); err != nil {
		t.Fatalf("setWebEnabled: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "config.json")); err != nil {
		t.Errorf("config.json must exist after enable: %v", err)
	}
}

func TestDaemonStatePathHonorsEnvOverride(t *testing.T) {
	t.Setenv("APLEXICA_STATE_DIR", "/tmp/aplexica-test-env")
	got, err := daemonStatePath()
	if err != nil {
		t.Fatalf("daemonStatePath: %v", err)
	}
	if got != "/tmp/aplexica-test-env" {
		t.Errorf("path = %q, want env override", got)
	}
}

// TestDaemonAndWebStateDirAgree asserts the daemon's --state-dir default
// and the web CLI's daemonStatePath() resolve to the SAME directory.
// Previously the daemon flag default was a plain ~/.aplexica/state and
// never read APLEXICA_STATE_DIR, while the web CLI honored the env var,
// so the two surfaces diverged whenever the env override was set.
func TestDaemonAndWebStateDirAgree(t *testing.T) {
	// Env-set: both must point at the override.
	t.Setenv("APLEXICA_STATE_DIR", "/tmp/aplexica-parity-env")
	web, err := daemonStatePath()
	if err != nil {
		t.Fatalf("daemonStatePath: %v", err)
	}
	dmn, err := defaultStateDir()
	if err != nil {
		t.Fatalf("defaultStateDir: %v", err)
	}
	if web != dmn {
		t.Fatalf("env-set divergence: web=%q daemon=%q", web, dmn)
	}
	if dmn != "/tmp/aplexica-parity-env" {
		t.Errorf("daemon default = %q, want env override", dmn)
	}

	// Env-unset: both must fall back to the same default path.
	t.Setenv("APLEXICA_STATE_DIR", "")
	web2, err := daemonStatePath()
	if err != nil {
		t.Fatalf("daemonStatePath: %v", err)
	}
	dmn2, err := defaultStateDir()
	if err != nil {
		t.Fatalf("defaultStateDir: %v", err)
	}
	if web2 != dmn2 {
		t.Fatalf("env-unset divergence: web=%q daemon=%q", web2, dmn2)
	}
}
