package main

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/daemon"
)

// configOnly forces the setup wizard's bootstrap step OFF for tests that only
// care about the config-writing path, and restores the flag afterward.
func configOnly(t *testing.T) {
	t.Helper()
	old := setupFlagInstall
	setupFlagInstall = "no"
	t.Cleanup(func() { setupFlagInstall = old })
}

func TestParseYN(t *testing.T) {
	cases := []struct {
		in      string
		wantVal bool
		wantOK  bool
	}{
		{"y", true, true},
		{"yes", true, true},
		{"YES", true, true},
		{"true", true, true},
		{"1", true, true},
		{"n", false, true},
		{"no", false, true},
		{"NO", false, true},
		{"false", false, true},
		{"0", false, true},
		{"", false, false},
		{"maybe", false, false},
		{"hmm", false, false},
	}
	for _, c := range cases {
		gotVal, gotOK := parseYN(c.in)
		if gotVal != c.wantVal || gotOK != c.wantOK {
			t.Errorf("parseYN(%q) = (%v,%v), want (%v,%v)", c.in, gotVal, gotOK, c.wantVal, c.wantOK)
		}
	}
}

func TestAskYNReturnsDefaultOnEmptyInput(t *testing.T) {
	in := bufio.NewReader(strings.NewReader("\n"))
	var out, errOut bytes.Buffer
	got, err := askYN(in, &out, &errOut, "Q?", true, "", false)
	if err != nil {
		t.Fatalf("askYN: %v", err)
	}
	if !got {
		t.Errorf("askYN returned false on empty input; default was true")
	}
}

func TestAskYNHonorsPreset(t *testing.T) {
	in := bufio.NewReader(strings.NewReader("y\n")) // would normally pick true
	var out, errOut bytes.Buffer
	got, err := askYN(in, &out, &errOut, "Q?", true, "no", false)
	if err != nil {
		t.Fatalf("askYN: %v", err)
	}
	if got {
		t.Errorf("preset=no but got true")
	}
}

func TestAskYNRejectsInvalidPreset(t *testing.T) {
	in := bufio.NewReader(strings.NewReader("y\n"))
	var out, errOut bytes.Buffer
	if _, err := askYN(in, &out, &errOut, "Q?", true, "maybe", false); err == nil {
		t.Error("expected error for invalid preset")
	}
}

func TestSetupWithIOPersistsConfig(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("APLEXICA_STATE_DIR", dir)
	configOnly(t)

	// Answer: y (tray), y (web), n (open)
	in := strings.NewReader("y\ny\nn\n")
	var out, errOut bytes.Buffer
	if err := setupWithIO(in, &out, &errOut); err != nil {
		t.Fatalf("setupWithIO: %v", err)
	}

	cfg, err := daemon.LoadConfig(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.Tray.Enabled == nil || !*cfg.Tray.Enabled {
		t.Errorf("Tray.Enabled = %v, want pointer-to-true", cfg.Tray.Enabled)
	}
	if cfg.Web.Enabled == nil || !*cfg.Web.Enabled {
		t.Errorf("Web.Enabled = %v, want pointer-to-true", cfg.Web.Enabled)
	}
}

func TestSetupSkipsOpenQuestionWhenWebDisabled(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("APLEXICA_STATE_DIR", dir)
	configOnly(t)

	// Answer: y (tray), n (web) — should skip the open prompt
	in := strings.NewReader("y\nn\n")
	var out, errOut bytes.Buffer
	if err := setupWithIO(in, &out, &errOut); err != nil {
		t.Fatalf("setupWithIO: %v", err)
	}
	cfg, _ := daemon.LoadConfig(filepath.Join(dir, "config.json"))
	if cfg.Web.Enabled == nil || *cfg.Web.Enabled {
		t.Errorf("Web.Enabled = %v, want pointer-to-false", cfg.Web.Enabled)
	}
	// The "Open the web UI now?" prompt must not appear when web is off.
	if strings.Contains(out.String(), "Open the web UI now") {
		t.Error("wizard asked about opening web UI even though web was disabled")
	}
}

func TestSetupWithIO_InstallRunsBootstrap(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("APLEXICA_STATE_DIR", dir)

	// Force the bootstrap on with a fixed watched dir, and swap in a fake
	// runner so no real subprocesses are spawned.
	oldInstall, oldDir, oldCloud, oldRunner, oldProbe := setupFlagInstall, setupFlagDir, setupFlagCloud, newSetupRunner, setupDaemonProbe
	setupFlagInstall = "yes"
	setupFlagDir = "/watched"
	setupFlagCloud = ""
	fake := &fakeSetupRunner{}
	newSetupRunner = func(out, errOut io.Writer) setupRunner { return fake }
	setupDaemonProbe = func() error { return nil }
	t.Cleanup(func() {
		setupFlagInstall, setupFlagDir, setupFlagCloud, newSetupRunner, setupDaemonProbe = oldInstall, oldDir, oldCloud, oldRunner, oldProbe
	})

	// tray=y, web=y, (install preset=yes → no prompt), open=n
	in := strings.NewReader("y\ny\nn\n")
	var out, errOut bytes.Buffer
	if err := setupWithIO(in, &out, &errOut); err != nil {
		t.Fatalf("setupWithIO: %v", err)
	}

	want := [][]string{{"daemon", "install", "--dir", "/watched", "--tray=true"}}
	if !reflect.DeepEqual(fake.calls, want) {
		t.Fatalf("bootstrap calls = %v, want %v", fake.calls, want)
	}
	if !strings.Contains(out.String(), "Aplexica is set up and running") {
		t.Errorf("missing success summary; out=%q", out.String())
	}
	if !strings.Contains(out.String(), "First startup can take a few minutes") {
		t.Errorf("missing first-start wait guidance; out=%q", out.String())
	}
}

func TestSetupReadyTimeoutAllowsNativeSafetyBackup(t *testing.T) {
	if setupDaemonReadyTimeout < 2*time.Minute {
		t.Fatalf("setupDaemonReadyTimeout = %s, want at least 2m for native safety backups", setupDaemonReadyTimeout)
	}
}

func TestWaitForSetupDaemonRetriesUntilReady(t *testing.T) {
	oldProbe := setupDaemonProbe
	attempts := 0
	setupDaemonProbe = func() error {
		attempts++
		if attempts < 3 {
			return errors.New("not ready")
		}
		return nil
	}
	t.Cleanup(func() { setupDaemonProbe = oldProbe })

	if err := waitForSetupDaemon(100*time.Millisecond, time.Millisecond); err != nil {
		t.Fatalf("waitForSetupDaemon: %v", err)
	}
	if attempts != 3 {
		t.Fatalf("probe attempts = %d, want 3", attempts)
	}
}

func TestWaitForSetupDaemonReportsTimeout(t *testing.T) {
	oldProbe := setupDaemonProbe
	setupDaemonProbe = func() error { return errors.New("socket unavailable") }
	t.Cleanup(func() { setupDaemonProbe = oldProbe })

	err := waitForSetupDaemon(0, 0)
	if err == nil || !strings.Contains(err.Error(), "socket unavailable") {
		t.Fatalf("waitForSetupDaemon error = %v, want last probe error", err)
	}
}

func TestSetupConfigOnlyPrintsRunnableDaemonCommands(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("APLEXICA_STATE_DIR", dir)
	configOnly(t)

	var out, errOut bytes.Buffer
	if err := setupWithIO(strings.NewReader("y\ny\nn\n"), &out, &errOut); err != nil {
		t.Fatalf("setupWithIO: %v", err)
	}
	for _, command := range []string{
		`aplexica daemon install --dir "$HOME" --tray`,
		`aplexica daemon start --dir "$HOME"`,
	} {
		if !strings.Contains(out.String(), command) {
			t.Errorf("missing command %q; out=%q", command, out.String())
		}
	}
}

func TestSetupHeadlessFlagsSkipPrompts(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("APLEXICA_STATE_DIR", dir)
	configOnly(t)
	// Use the package-global flag vars; restore on exit.
	old := setupFlagTray
	setupFlagTray = "no"
	setupFlagWeb_ := setupFlagWeb
	setupFlagWeb = "yes"
	setupFlagOpen_ := setupFlagOpen
	setupFlagOpen = "no"
	t.Cleanup(func() {
		setupFlagTray = old
		setupFlagWeb = setupFlagWeb_
		setupFlagOpen = setupFlagOpen_
	})

	in := strings.NewReader("") // no prompts expected
	var out, errOut bytes.Buffer
	if err := setupWithIO(in, &out, &errOut); err != nil {
		t.Fatalf("setupWithIO headless: %v", err)
	}

	cfg, _ := daemon.LoadConfig(filepath.Join(dir, "config.json"))
	if cfg.Tray.Enabled == nil || *cfg.Tray.Enabled {
		t.Errorf("Tray.Enabled = %v, want false (--tray=no)", cfg.Tray.Enabled)
	}
	if cfg.Web.Enabled == nil || !*cfg.Web.Enabled {
		t.Errorf("Web.Enabled = %v, want true (--web=yes)", cfg.Web.Enabled)
	}
}
