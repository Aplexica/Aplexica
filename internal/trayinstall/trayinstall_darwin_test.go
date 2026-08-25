//go:build darwin

package trayinstall

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// launchctlRecorder captures the argv of every launchctl invocation the
// installer makes, instead of executing it.
type launchctlRecorder struct {
	mu    sync.Mutex
	calls [][]string
}

func (r *launchctlRecorder) run(args ...string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, append([]string(nil), args...))
	return nil, nil
}

func (r *launchctlRecorder) snapshot() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([][]string(nil), r.calls...)
}

// realLaunchctlCalls counts attempts to shell out to the REAL launchctl from
// inside the test binary. It must stay zero: see TestMain.
var realLaunchctlCalls struct {
	mu    sync.Mutex
	calls [][]string
}

// TestMain replaces the package's launchctl hook for the whole test binary so
// that NO test here can touch the machine's real launchd domain.
//
// This is a regression guard: TestDarwinInstallRoundtrip
// used to call the production Uninstall(), which runs
// `launchctl unload -w <plist>`. launchctl resolves the job by the Label
// inside the plist — `com.aplexica.tray` — not by the plist's path, so writing
// the plist into t.TempDir() did NOT sandbox anything. Every `go test ./...`
// on a Mac could boot out the running tray and write a persistent
// disable into /var/db/com.apple.xpc.launchd/disabled.<uid>.plist. The tray
// died silently (launchd's bootout SIGTERM is trapped by the tray's
// signal.NotifyContext and converted to a clean exit 0, so there was no crash
// report and KeepAlive{SuccessfulExit:false} declined to respawn), and the
// disable that survived reboot.
func TestMain(m *testing.M) {
	execLaunchctl = func(args ...string) ([]byte, error) {
		realLaunchctlCalls.mu.Lock()
		defer realLaunchctlCalls.mu.Unlock()
		realLaunchctlCalls.calls = append(realLaunchctlCalls.calls, append([]string(nil), args...))
		return nil, fmt.Errorf("test attempted to run the real `launchctl %s`", strings.Join(args, " "))
	}
	code := m.Run()
	realLaunchctlCalls.mu.Lock()
	leaked := realLaunchctlCalls.calls
	realLaunchctlCalls.mu.Unlock()
	if len(leaked) > 0 {
		fmt.Fprintf(os.Stderr,
			"\nFAIL: %d launchctl invocation(s) escaped the test hook and would have hit the real\n"+
				"gui/$UID/%s service on this machine (booting out and PERSISTENTLY disabling the\n"+
				"user's menu-bar tray). Inject a launchctlRecorder instead:\n  %v\n",
			len(leaked), launchdTrayLabel, leaked)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

func TestDarwinPlistGeneration(t *testing.T) {
	inst := &launchdTrayInstaller{opts: Options{TrayPath: "/usr/local/bin/aplexicatray"}}
	body, err := inst.generatePlist()
	if err != nil {
		t.Fatal(err)
	}
	s := string(body)
	for _, want := range []string{
		"<string>com.aplexica.tray</string>",
		"<string>/usr/local/bin/aplexicatray</string>",
		"<key>RunAtLoad</key>",
		"<true/>",
		"<key>LimitLoadToSessionType</key>",
		"<string>Aqua</string>",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("plist missing %q\n--- body ---\n%s", want, s)
		}
	}
	// KeepAlive{SuccessfulExit:false} makes launchd respawn the tray after a
	// CRASH or abnormal exit (e.g. a daemon restart that killed its status
	// feed) while still honoring a clean user-quit (exit 0 → not respawned).
	for _, want := range []string{
		"<key>KeepAlive</key>",
		"<key>SuccessfulExit</key>",
		"<false/>",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("plist must set KeepAlive{SuccessfulExit:false} for self-healing; missing %q\n--- body ---\n%s", want, s)
		}
	}
}

// TestDarwinPlistFlagForwarding (v0.40.0) verifies each forwarded
// Options field becomes a ProgramArguments entry; absent options stay
// absent so the generated plist remains backward-compatible with
// v0.37.0 callers.
func TestDarwinPlistFlagForwarding(t *testing.T) {
	inst := &launchdTrayInstaller{opts: Options{
		TrayPath:        "/usr/local/bin/aplexicatray",
		AplexicaPath:    "/opt/aplexica/bin/aplexica",
		Interval:        2 * time.Second,
		LogDir:          "/var/log/aplexica",
		ActiveWindow:    45 * time.Second,
		PausedThreshold: 10 * time.Minute,
		StateDir:        "/var/lib/aplexica/state",
		ConflictsRoot:   "/var/lib/aplexica/state/conflicts",
	}}
	body, err := inst.generatePlist()
	if err != nil {
		t.Fatal(err)
	}
	s := string(body)
	for _, want := range []string{
		"<string>--aplexica</string>",
		"<string>/opt/aplexica/bin/aplexica</string>",
		"<string>--interval</string>",
		"<string>2s</string>",
		"<string>--log-dir</string>",
		"<string>/var/log/aplexica</string>",
		"<string>--active-window</string>",
		"<string>45s</string>",
		"<string>--paused-threshold</string>",
		"<string>10m0s</string>",
		"<string>--state-dir</string>",
		"<string>/var/lib/aplexica/state</string>",
		"<string>--conflicts-root</string>",
		"<string>/var/lib/aplexica/state/conflicts</string>",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("plist missing forwarded flag %q\n--- body ---\n%s", want, s)
		}
	}
}

// TestDarwinPlistFlagsOmittedWhenEmpty asserts the v0.37.0 zero-flag
// behavior remains the default.
func TestDarwinPlistFlagsOmittedWhenEmpty(t *testing.T) {
	inst := &launchdTrayInstaller{opts: Options{TrayPath: "/bin/aplexicatray"}}
	body, _ := inst.generatePlist()
	s := string(body)
	for _, banned := range []string{"--interval", "--log-dir", "--active-window", "--paused-threshold", "--aplexica", "--state-dir", "--conflicts-root"} {
		if strings.Contains(s, banned) {
			t.Errorf("plist contains unexpected flag %q with empty Options: %s", banned, s)
		}
	}
}

// useRecorder swaps the package's launchctl hook for a recorder for the
// duration of one test, restoring TestMain's poison afterwards.
func useRecorder(t *testing.T) *launchctlRecorder {
	t.Helper()
	rec := &launchctlRecorder{}
	prev := execLaunchctl
	execLaunchctl = rec.run
	t.Cleanup(func() { execLaunchctl = prev })
	return rec
}

// TestDarwinInstallRoundtrip exercises the FULL Install + Uninstall against a
// dir-override path with launchctl stubbed out. Before the fix this test
// deliberately skipped Install() (it would have bootstrapped the agent into
// the real session) but still called Uninstall() — which shells out to
// `launchctl unload -w` and, because launchctl keys off the Label inside the
// plist rather than its path, deregistered and persistently DISABLED the real
// com.aplexica.tray on the machine running the tests. With the hook injected,
// both halves are now safe to exercise.
func TestDarwinInstallRoundtrip(t *testing.T) {
	rec := useRecorder(t)
	dir := t.TempDir()
	tray := filepath.Join(dir, "aplexicatray-fake")
	if err := os.WriteFile(tray, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	inst := &launchdTrayInstaller{
		opts:             Options{TrayPath: tray},
		plistDirOverride: dir,
	}
	plistPath := inst.plistPath()

	if err := inst.Install(); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if _, err := os.Stat(plistPath); err != nil {
		t.Fatalf("plist not written: %v", err)
	}
	if err := inst.Uninstall(); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if _, err := os.Stat(plistPath); !os.IsNotExist(err) {
		t.Errorf("plist not removed after Uninstall: stat err=%v", err)
	}
	// Idempotent: second Uninstall is a no-op success that must not
	// re-issue a launchctl command.
	if err := inst.Uninstall(); err != nil {
		t.Errorf("Uninstall idempotency: %v", err)
	}

	want := [][]string{
		{"unload", plistPath},
		{"load", "-w", plistPath},
		{"unload", "-w", plistPath},
	}
	got := rec.snapshot()
	if len(got) != len(want) {
		t.Fatalf("launchctl calls = %v, want %v", got, want)
	}
	for i := range want {
		if strings.Join(got[i], " ") != strings.Join(want[i], " ") {
			t.Errorf("launchctl call %d = %v, want %v", i, got[i], want[i])
		}
	}
}

// TestDarwinUninstallTargetsTheProductionLabel documents WHY the injected hook
// is mandatory: the plist Uninstall hands to launchctl always carries the
// production Label, so redirecting the plist's directory to t.TempDir()
// sandboxes the FILE but not the SERVICE. `launchctl unload -w` on this file
// boots out and persistently disables gui/$UID/com.aplexica.tray — the real
// menu-bar agent — no matter where the file lives.
func TestDarwinUninstallTargetsTheProductionLabel(t *testing.T) {
	rec := useRecorder(t)
	dir := t.TempDir()
	inst := &launchdTrayInstaller{
		opts:             Options{TrayPath: filepath.Join(dir, "aplexicatray-fake")},
		plistDirOverride: dir,
	}
	body, err := inst.generatePlist()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(inst.plistPath(), body, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := inst.Uninstall(); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	calls := rec.snapshot()
	if len(calls) != 1 || strings.Join(calls[0], " ") != "unload -w "+inst.plistPath() {
		t.Fatalf("launchctl calls = %v, want one `unload -w <plist>`", calls)
	}
	if !strings.Contains(string(body), "<string>"+launchdTrayLabel+"</string>") {
		t.Fatalf("plist handed to `launchctl unload -w` does not carry the %s label:\n%s",
			launchdTrayLabel, body)
	}
}

// TestDarwinLaunchctlHasOneCallSite keeps every launchctl invocation funnelled
// through execLaunchctl. A new `exec.Command("launchctl", …)` added directly to
// Install/Uninstall would bypass TestMain's poison and start clobbering the
// host machine's real tray agent again.
func TestDarwinLaunchctlHasOneCallSite(t *testing.T) {
	src, err := os.ReadFile("trayinstall_darwin.go")
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(src), `exec.Command("launchctl"`); n != 1 {
		t.Fatalf(`trayinstall_darwin.go has %d direct exec.Command("launchctl", …) call sites, want exactly 1 `+
			`(the execLaunchctl hook). Route new calls through execLaunchctl so tests cannot `+
			`bootout/disable the real %s service.`, n, launchdTrayLabel)
	}
}
