//go:build darwin

package trayinstall

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The tray logs EXCLUSIVELY through the stdlib `log` package, i.e. to stderr
// (cmd/aplexicatray/main.go sets log flags + prefix and never calls
// log.SetOutput). launchd routes a job's stdio to /dev/null unless the plist
// names StandardOutPath / StandardErrorPath, so every diagnostic the tray
// emitted under the LaunchAgent — including the reason it shut down — was
// destroyed. These tests pin the redirect into the tray's configured log
// directory, and pin the safety rules that keep the redirect from turning
// "no diagnostics" into "the tray never launches".

func plistBodyFor(t *testing.T, opts Options) string {
	t.Helper()
	inst := &launchdTrayInstaller{opts: opts}
	body, err := inst.generatePlist()
	if err != nil {
		t.Fatalf("generatePlist: %v", err)
	}
	return string(body)
}

// With an explicit LogDir, both stdio streams are redirected into that
// directory. One file for both streams: the tray writes only to stderr, and a
// single file keeps the existing "Open Logs" menu item (menu.go openPath(logDir))
// one click away from the tray's own log.
func TestDarwinPlistRedirectsStdioIntoLogDir(t *testing.T) {
	dir := t.TempDir()
	s := plistBodyFor(t, Options{
		TrayPath: "/usr/local/bin/aplexicatray",
		LogDir:   dir,
	})
	want := filepath.Join(dir, trayLaunchdLogName)
	for _, key := range []string{"StandardOutPath", "StandardErrorPath"} {
		if !strings.Contains(s, "<key>"+key+"</key>") {
			t.Errorf("plist missing <key>%s</key>\n--- body ---\n%s", key, s)
		}
	}
	if n := strings.Count(s, "<string>"+want+"</string>"); n != 2 {
		t.Errorf("plist names %s %d time(s), want 2 (StandardOutPath + StandardErrorPath)\n--- body ---\n%s",
			want, n, s)
	}
}

// The launchd log file MUST NOT collide with the daemon's own rotated log
// (internal/daemon/log.go writes ~/.aplexica/logs/aplexicad.log and rotates it
// out from under itself). launchd holds its redirect fd open and never rotates,
// so sharing a path would have the two fighting over the same inode.
func TestDarwinPlistLogNameDoesNotCollideWithDaemonLog(t *testing.T) {
	if trayLaunchdLogName == "aplexicad.log" {
		t.Fatalf("tray launchd log name %q collides with the daemon's rotated log", trayLaunchdLogName)
	}
	if !strings.HasPrefix(trayLaunchdLogName, "tray.") {
		t.Errorf("tray launchd log name %q should be tray-namespaced so it is "+
			"obviously not a daemon log and not subject to the daemon's rotator",
			trayLaunchdLogName)
	}
}

// No LogDir configured: the plist must still redirect, into the SAME default
// the tray binary itself uses for --log-dir (~/.aplexica/logs), so "Open Logs"
// and the launchd redirect agree.
func TestDarwinPlistFallsBackToDefaultLogDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	s := plistBodyFor(t, Options{TrayPath: "/usr/local/bin/aplexicatray"})
	want := filepath.Join(home, ".aplexica", "logs", trayLaunchdLogName)
	if !strings.Contains(s, "<string>"+want+"</string>") {
		t.Errorf("plist does not fall back to the default log dir %s\n--- body ---\n%s", want, s)
	}
}

// Degrade safely: when no absolute log path can be resolved at all, the keys
// must be OMITTED. A StandardErrorPath launchd cannot open risks the job
// failing to spawn — strictly worse than the missing diagnostics it fixes.
func TestDarwinPlistOmitsLogPathsWhenUnresolvable(t *testing.T) {
	cases := map[string]Options{
		"relative LogDir": {TrayPath: "/bin/aplexicatray", LogDir: "relative/logs"},
		"no home":         {TrayPath: "/bin/aplexicatray"},
	}
	for name, opts := range cases {
		t.Run(name, func(t *testing.T) {
			if name == "no home" {
				t.Setenv("HOME", "")
			}
			s := plistBodyFor(t, opts)
			for _, key := range []string{"StandardOutPath", "StandardErrorPath"} {
				if strings.Contains(s, key) {
					t.Errorf("plist emits %s with an unresolvable log dir\n--- body ---\n%s", key, s)
				}
			}
		})
	}
}

// Install must CREATE the directory the redirect points at. launchd needs the
// parent directory of Standard*Path to exist; a fresh install where nothing has
// written ~/.aplexica/logs yet would otherwise hand launchd an unopenable path.
func TestDarwinInstallCreatesLogDir(t *testing.T) {
	rec := useRecorder(t)
	dir := t.TempDir()
	tray := filepath.Join(dir, "aplexicatray-fake")
	if err := os.WriteFile(tray, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	logDir := filepath.Join(dir, "never", "created", "logs")
	inst := &launchdTrayInstaller{
		opts:             Options{TrayPath: tray, LogDir: logDir},
		plistDirOverride: dir,
	}
	if err := inst.Install(); err != nil {
		t.Fatalf("Install: %v", err)
	}
	fi, err := os.Stat(logDir)
	if err != nil {
		t.Fatalf("Install did not create the launchd log dir %s: %v", logDir, err)
	}
	if !fi.IsDir() {
		t.Fatalf("%s is not a directory", logDir)
	}
	body, err := os.ReadFile(inst.plistPath())
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(logDir, trayLaunchdLogName)
	if !strings.Contains(string(body), "<string>"+want+"</string>") {
		t.Errorf("installed plist does not point stdio at %s\n--- body ---\n%s", want, body)
	}
	if len(rec.snapshot()) == 0 {
		t.Errorf("Install did not go through the stubbed launchctl hook")
	}
}

// If the log directory cannot be created, Install must still succeed AND must
// drop the redirect keys rather than register a job launchd may refuse to spawn.
func TestDarwinInstallDropsLogPathsWhenDirCannotBeCreated(t *testing.T) {
	useRecorder(t)
	dir := t.TempDir()
	tray := filepath.Join(dir, "aplexicatray-fake")
	if err := os.WriteFile(tray, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A regular FILE where the log directory should be: MkdirAll fails with
	// ENOTDIR, which is the closest reproducible stand-in for an unwritable
	// destination that does not require root.
	blocked := filepath.Join(dir, "blocked")
	if err := os.WriteFile(blocked, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	inst := &launchdTrayInstaller{
		opts:             Options{TrayPath: tray, LogDir: filepath.Join(blocked, "logs")},
		plistDirOverride: dir,
	}
	if err := inst.Install(); err != nil {
		t.Fatalf("Install must not fail because a diagnostics dir is unavailable: %v", err)
	}
	body, err := os.ReadFile(inst.plistPath())
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"StandardOutPath", "StandardErrorPath"} {
		if strings.Contains(string(body), key) {
			t.Errorf("installed plist emits %s pointing at a directory that could not be created\n--- body ---\n%s",
				key, body)
		}
	}
}

// An EXISTING but unwritable log directory must also drop the keys.
//
// os.MkdirAll returns nil for any directory that already exists, whatever its
// mode or owner — "the directory exists" is not "launchd can open a file in
// it". launchd opens StandardOutPath/StandardErrorPath itself, as the user,
// with O_WRONLY|O_APPEND|O_CREAT; if that open fails the job can fail to
// spawn, which is strictly worse than the missing diagnostics this redirect
// exists to restore. So the installer has to attempt launchd's open, not just
// MkdirAll.
func TestDarwinInstallDropsLogPathsWhenDirIsUnwritable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permission bits, so the premise cannot be set up")
	}
	useRecorder(t)
	dir := t.TempDir()
	tray := filepath.Join(dir, "aplexicatray-fake")
	if err := os.WriteFile(tray, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	logDir := filepath.Join(dir, "existing-readonly-logs")
	if err := os.Mkdir(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// r-x------: lookups succeed, creating an entry does not.
	if err := os.Chmod(logDir, 0o500); err != nil {
		t.Fatal(err)
	}
	// Restore write permission before t.TempDir's RemoveAll: cleanups run
	// LIFO and TempDir registered its own first, so this one runs earlier.
	t.Cleanup(func() { _ = os.Chmod(logDir, 0o700) })

	// Premise, spelled out because it is the whole bug: MkdirAll is happy…
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatalf("premise broken: MkdirAll on an existing read-only dir returned %v, want nil", err)
	}
	// …and the open launchd actually performs is not.
	logFile := filepath.Join(logDir, trayLaunchdLogName)
	if f, err := os.OpenFile(logFile, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644); err == nil {
		_ = f.Close()
		_ = os.Remove(logFile)
		t.Skip("filesystem does not enforce directory write permission; cannot set up the premise")
	}

	inst := &launchdTrayInstaller{
		opts:             Options{TrayPath: tray, LogDir: logDir},
		plistDirOverride: dir,
	}
	if err := inst.Install(); err != nil {
		t.Fatalf("Install must not fail because a diagnostics dir is unwritable: %v", err)
	}
	body, err := os.ReadFile(inst.plistPath())
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"StandardOutPath", "StandardErrorPath"} {
		if strings.Contains(string(body), key) {
			t.Errorf("installed plist emits %s pointing into a directory launchd cannot write to; "+
				"the job may then fail to spawn, which is worse than having no tray log at all"+
				"\n--- body ---\n%s", key, body)
		}
	}
}

// The happy path must still leave a file launchd can append to — i.e. the
// writability check is a real open, and its side effect is the same one
// launchd would have had.
func TestDarwinInstallLeavesAnAppendableLogFile(t *testing.T) {
	useRecorder(t)
	dir := t.TempDir()
	tray := filepath.Join(dir, "aplexicatray-fake")
	if err := os.WriteFile(tray, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	logDir := filepath.Join(dir, "logs")
	inst := &launchdTrayInstaller{
		opts:             Options{TrayPath: tray, LogDir: logDir},
		plistDirOverride: dir,
	}
	if err := inst.Install(); err != nil {
		t.Fatalf("Install: %v", err)
	}
	logFile := filepath.Join(logDir, trayLaunchdLogName)
	f, err := os.OpenFile(logFile, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatalf("launchd's own open of %s would fail: %v", logFile, err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(inst.plistPath())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "<string>"+logFile+"</string>") {
		t.Errorf("installed plist does not point stdio at the verified path %s\n--- body ---\n%s",
			logFile, body)
	}
}

// The deliberate existing tray keys must survive untouched.
func TestDarwinPlistKeepsExistingKeysAlongsideLogPaths(t *testing.T) {
	s := plistBodyFor(t, Options{TrayPath: "/bin/aplexicatray", LogDir: t.TempDir()})
	for _, want := range []string{
		"<key>Label</key>",
		"<string>com.aplexica.tray</string>",
		"<key>RunAtLoad</key>",
		"<key>KeepAlive</key>",
		"<key>SuccessfulExit</key>",
		"<false/>",
		"<key>LimitLoadToSessionType</key>",
		"<string>Aqua</string>",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("plist lost pre-existing key %q\n--- body ---\n%s", want, s)
		}
	}
}
