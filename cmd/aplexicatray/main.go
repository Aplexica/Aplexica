//go:build tray

// Command aplexicatray is the cross-platform system-tray indicator for
// Aplexica (BRD-03 §4.9). It spawns `aplexica-status status --watch
// --json --interval 5s` as a subprocess when that companion binary is
// present, decodes each snapshot, and drives a systray icon + menu through
// the platform-native event loop.
//
// Build with: go build -tags tray -o aplexicatray ./cmd/aplexicatray
// Or via Makefile: make tray
//
// The tray binary is decoupled from the daemon's internal packages —
// its only contract with `aplexica` is the JSON wire shape printed by
// `aplexica status --watch --json` and the CLI surface of `aplexica
// daemon stop|start` + `aplexica conflicts show`.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"fyne.io/systray"

	"github.com/aplexica/aplexica/internal/daemon"
	"github.com/aplexica/aplexica/internal/traycontrol"
)

// Tray timing flag defaults, as named constants so they live in one place and
// are exempt from the FR-10.6 magic-number lint. They MUST equal the published
// tray.active_window / tray.paused_window in internal/config (defaults.toml +
// config.Schema); guarded by TestTrayFlagDefaultsMatchPublishedConfig.
const (
	defaultActiveWindow    = 60 * time.Second
	defaultPausedThreshold = 10 * time.Minute
)

var (
	flagAplexica = flag.String("aplexica", "aplexica",
		"path to the aplexica CLI (resolved via $PATH if relative)")
	flagInterval = flag.Duration("interval", 5*time.Second,
		"polling interval passed to `aplexica status --watch`")
	flagLogDir = flag.String("log-dir", "",
		"daemon log directory exposed via the Open Logs menu (default ~/.aplexica/logs)")
	// Defaults MUST equal the published tray.active_window / tray.paused_window
	// values in internal/config (defaults.toml + config.Schema): 60s / 10m.
	// The paused threshold is retained for installer/config compatibility; the
	// current tray uses the explicit sync-pause state for the Paused icon.
	// Guarded by
	// TestTrayFlagDefaultsMatchPublishedConfig.
	flagActiveWindow = flag.Duration("active-window", defaultActiveWindow,
		"snapshot age below which the icon shows the Active state")
	flagPausedThreshold = flag.Duration("paused-threshold", defaultPausedThreshold,
		"deprecated quiet threshold retained for compatibility")
	// v0.43.0: forward to `aplexica status --watch` so the tray reads
	// from whichever daemon the user actually started. Empty values are
	// passed through and `aplexica status` falls back to its own
	// defaults (~/.aplexica/state and ~/.aplexica/state/conflicts).
	flagStateDir = flag.String("state-dir", "",
		"daemon state directory containing aplexicad.sock (default: aplexica's own default, ~/.aplexica/state)")
	flagConflictsRoot = flag.String("conflicts-root", "",
		"conflicts store root (default: aplexica's own default, <state-dir>/conflicts)")
	flagVersion = flag.Bool("version", false,
		"print the tray binary version and exit")
)

var statusWatchPath string

// trayVersion is the release-stamped version of the tray binary, surfaced via
// --version. It must remain a variable so release builds can set it with
// -X main.trayVersion=<version> and keep tray provenance aligned with the
// daemon and status helper built from the same source commit.
var trayVersion = "v1.0.0"

// trayEnabledByConfig loads the daemon's persisted config (the same
// config.json the daemon reads/writes at <state-dir>) and returns the
// effective tray.enabled value. The tray defaults to ENABLED on every OS
// (opt-out) via daemon.TrayEnabledDefault — so an unconfigured install
// shows the tray on macOS/Linux/Windows alike; the user opts out per host
// with tray.enabled=false.
//
// The lookup uses the configured --state-dir if set, otherwise the
// default ~/.aplexica/state. Missing config file is treated as
// "all defaults" — the platform-default kicks in.
func trayEnabledByConfig() bool {
	stateDir, ok := effectiveStateDir()
	if !ok {
		// Can't locate config; honor the platform default rather
		// than refusing to run.
		return daemon.TrayEnabledDefault()
	}
	cfg, err := daemon.LoadConfig(filepath.Join(stateDir, "config.json"))
	if err != nil {
		// Config-parse error: log + honor platform default. We do NOT
		// fail closed — better to let the user see a tray icon and
		// notice the broken config than to silently disappear.
		log.Printf("config load failed: %v (honoring platform default)", err)
		return daemon.TrayEnabledDefault()
	}
	return daemon.TrayEnabled(cfg)
}

func effectiveStateDir() (string, bool) {
	if *flagStateDir != "" {
		return *flagStateDir, true
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", false
	}
	return filepath.Join(home, ".aplexica", "state"), true
}

func acquireInstanceLock() (*daemon.Lock, error) {
	stateDir, ok := effectiveStateDir()
	if !ok {
		return nil, fmt.Errorf("cannot resolve state dir for tray lock")
	}
	return daemon.Acquire(filepath.Join(stateDir, "aplexicatray.lock"))
}

func main() {
	flag.Parse()
	if *flagVersion {
		fmt.Println("aplexicatray", trayVersion)
		return
	}
	// The tray is a console-subsystem binary, so Windows hands it a console
	// window whenever it's launched (the Startup .lnk, a scheduled task, etc.).
	// Detach from it now — AFTER --version so that still prints — so the
	// background tray indicator shows no console window. No-op on non-windows.
	detachTrayConsole()
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	log.SetPrefix("aplexicatray: ")

	// v0.108.1: resolve the aplexica CLI path. The tray spawns
	// `aplexica status --watch` (and `aplexica daemon stop|start`,
	// `aplexica conflicts show`, `aplexica web open`) as subprocesses.
	// Under a launchd LaunchAgent the inherited PATH is the bare
	// /usr/bin:/bin:/usr/sbin:/sbin — it does NOT include Homebrew's
	// /opt/homebrew/bin or /usr/local/bin, so a PATH-relative
	// "aplexica" lookup fails and the tray exits before showing an
	// icon. Resolve a sibling next to our own executable first
	// (aplexica + aplexicatray always install into the same dir), so
	// the tray works regardless of the launchd PATH. Found in live
	// end-user testing on macOS.
	*flagAplexica = resolveAplexicaPath(*flagAplexica)
	statusWatchPath = resolveStatusWatchPath(*flagAplexica)

	if *flagLogDir == "" {
		home, _ := os.UserHomeDir()
		*flagLogDir = filepath.Join(home, ".aplexica", "logs")
	}

	// v0.50.0: honor the persisted tray.enabled config (FR-03.30).
	// When the user has explicitly opted out, exit cleanly with a one-line
	// message — the daemon keeps running.
	// The check happens BEFORE systray.Run so the binary doesn't even
	// create a NSStatusItem / DBus SNI / Shell_NotifyIcon registration
	// when disabled.
	if !trayEnabledByConfig() {
		log.Printf("tray disabled by config (~/.aplexica/state/config.json `tray.enabled = false` " +
			"or platform default). Daemon is unaffected. " +
			"To enable: edit the config file, OR run `aplexica tray install` (sets enabled=true).")
		return
	}
	instanceLock, err := acquireInstanceLock()
	if err != nil {
		log.Printf("another aplexicatray instance appears to be running (%v); exiting", err)
		return
	}
	defer instanceLock.Release()

	// signal.NotifyContext would cancel ctx without saying which signal did
	// it — and the SAME cancel is handed to the traycontrol socket below, so
	// a logout SIGTERM and an updater's `quit-for-update` were
	// indistinguishable at ctx.Done(). watchShutdownSignals traps the same
	// signals (still a clean, exit-0 shutdown) but records which one arrived;
	// quitForUpdate records the socket path. See shutdown.go.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopSignalWatch := watchShutdownSignals(ctx, cancel, shutdownSource)
	defer stopSignalWatch()
	stateDir, ok := effectiveStateDir()
	if !ok {
		log.Printf("cannot resolve tray control directory")
		return
	}
	controlServer := traycontrol.NewServer(filepath.Join(stateDir, traycontrol.SocketName),
		trayVersion, quitForUpdate(cancel, shutdownSource))
	if err := controlServer.Start(); err != nil {
		log.Printf("cannot start private tray control socket: %v", err)
		return
	}
	defer controlServer.Close()

	// Snapshot channel: 1-deep so a slow systray repaint doesn't stall
	// the decoder, and a slow decoder doesn't stack stale snapshots.
	snapshots := make(chan StatusSnapshot, 1)
	go func() {
		defer close(snapshots)
		// Supervise the status feed: when a daemon restart makes
		// `aplexica status --watch` exit, RECONNECT instead of letting the
		// feed close (which would quit the tray and drop the icon until next
		// login). The feed closes — and the tray quits — only when ctx is
		// cancelled (a genuine user-quit). See superviseStatus.
		superviseStatus(ctx, func(c context.Context) error {
			return runStatus(c, snapshots)
		}, statusReconnectMinBackoff, statusReconnectMaxBackoff)
	}()

	t := newTray(*flagAplexica)
	onReady := func() {
		t.onReady(*flagLogDir)
		go t.run(ctx, snapshots, *flagActiveWindow, *flagPausedThreshold)
	}
	onExit := func() {
		// Daemon keeps running per FR-03.29 — we only cancel the
		// subprocess ctx so the status-watch child exits cleanly.
		//
		// This is the fourth cancel source, and the least informative one:
		// it fires AFTER systray's event loop ended, whatever ended it. The
		// recorder is first-writer-wins, so a Quit click / signal /
		// quit-for-update that got here first keeps the real attribution and
		// this only labels a shutdown nothing else claimed.
		shutdownSource.record(reasonSystrayExit)
		cancel()
	}

	systray.Run(onReady, onExit)
}

// resolveAplexicaPath turns the --aplexica flag into the binary the
// tray should exec. Resolution order:
//
//  1. An explicit path (absolute, or containing a path separator) is
//     honored verbatim — the operator asked for that exact binary.
//  2. The bare default "aplexica": prefer a sibling next to the tray's
//     own executable (aplexica and aplexicatray install into the same
//     directory: /opt/homebrew/bin, /usr/local/bin, %ProgramFiles%\…).
//     This is what makes the tray work under a launchd LaunchAgent
//     whose PATH omits the install dir.
//  3. Fall back to $PATH lookup.
//  4. Last resort: return the literal so the existing
//     "not found in $PATH" error still surfaces clearly.
func resolveAplexicaPath(flagVal string) string {
	if flagVal != "aplexica" &&
		(filepath.IsAbs(flagVal) || strings.ContainsRune(flagVal, filepath.Separator)) {
		return flagVal
	}
	if exe, err := os.Executable(); err == nil {
		if real, lerr := filepath.EvalSymlinks(exe); lerr == nil {
			exe = real
		}
		if sibling := resolveSiblingIn(filepath.Dir(exe)); sibling != "" {
			return sibling
		}
	}
	if p, err := exec.LookPath(flagVal); err == nil {
		return p
	}
	return flagVal
}

// resolveSiblingIn returns the absolute path to an `aplexica`
// (`aplexica.exe` on Windows) executable located in dir, or "" if no
// such file exists there. Extracted from resolveAplexicaPath so the
// sibling-resolution branch is unit-testable against an injectable
// directory rather than the test binary's own os.Executable() dir.
func resolveSiblingIn(dir string) string {
	name := "aplexica"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	sibling := filepath.Join(dir, name)
	if fi, err := os.Stat(sibling); err == nil && !fi.IsDir() {
		return sibling
	}
	return ""
}

func statusHelperBinaryName() string {
	if runtime.GOOS == "windows" {
		return "aplexica-status.exe"
	}
	return "aplexica-status"
}

func resolveStatusHelperIn(dir string) string {
	sibling := filepath.Join(dir, statusHelperBinaryName())
	if fi, err := os.Stat(sibling); err == nil && !fi.IsDir() {
		return sibling
	}
	return ""
}

// resolveStatusWatchPath returns the command used only for the tray's live
// status watcher. Packaged installs include a second copy of the CLI named
// aplexica-status so process monitors can distinguish the watcher from the
// real daemon process. Older/source installs keep working by falling back to
// the main aplexica CLI.
func resolveStatusWatchPath(aplexicaPath string) string {
	if aplexicaPath != "" {
		if helper := resolveStatusHelperIn(filepath.Dir(aplexicaPath)); helper != "" {
			return helper
		}
		if real, err := filepath.EvalSymlinks(aplexicaPath); err == nil && real != aplexicaPath {
			if helper := resolveStatusHelperIn(filepath.Dir(real)); helper != "" {
				return helper
			}
		}
	}
	if helper, err := exec.LookPath(statusHelperBinaryName()); err == nil {
		return helper
	}
	return aplexicaPath
}

// runStatus spawns `aplexica-status status --watch --json --interval <D>`
// when the helper exists, otherwise `aplexica status --watch ...`, and
// pushes one StatusSnapshot per decoded line onto out. Returns when ctx
// cancels or the subprocess exits.
func runStatus(ctx context.Context, out chan StatusSnapshot) error {
	args := []string{"status", "--watch", "--json", "--interval", flagInterval.String()}
	if *flagStateDir != "" {
		args = append(args, "--state-dir", *flagStateDir)
	}
	if *flagConflictsRoot != "" {
		args = append(args, "--conflicts-root", *flagConflictsRoot)
	}
	cmdPath := statusWatchPath
	if cmdPath == "" {
		cmdPath = resolveStatusWatchPath(*flagAplexica)
	}
	cmd := exec.CommandContext(ctx, cmdPath, args...)
	// Discard the child's stderr to the null device. Do NOT wire it to
	// os.Stderr: once the tray has FreeConsole'd, os.Stderr is its detached
	// console handle, and handing it to the child keeps that console alive —
	// every line the child writes then flashes the console window (the Windows
	// "flickering terminal"). nil → os/exec connects stderr to the null device.
	cmd.Stderr = nil
	// The `aplexica status --watch` child is a console-subsystem CLI, so Windows
	// would otherwise give it its own console window. hideChildConsole spawns it
	// DETACHED_PROCESS (windows) / no-op elsewhere, so no terminal appears.
	hideChildConsole(cmd)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", cmdPath, err)
	}
	log.Printf("spawned %s %s (pid %d)",
		cmdPath, strings.Join(args, " "), cmd.Process.Pid)

	decodeErr := decodeLoop(ctx, stdout, out)
	waitErr := cmd.Wait()
	switch {
	case ctx.Err() != nil:
		return nil
	case decodeErr != nil && decodeErr != io.EOF:
		return decodeErr
	case waitErr != nil:
		return fmt.Errorf("%s status exited: %w", filepath.Base(cmdPath), waitErr)
	}
	return nil
}

// decodeLoop reads one StatusSnapshot per stdout line and pushes onto
// out. The channel is bidirectional here (not send-only) so we can
// drain a stale buffered snapshot under backpressure.
func decodeLoop(ctx context.Context, r io.Reader, out chan StatusSnapshot) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		if ctx.Err() != nil {
			return nil
		}
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var snap StatusSnapshot
		if err := json.Unmarshal(line, &snap); err != nil {
			log.Printf("decode error (ignored): %v: %.200s", err, string(line))
			continue
		}
		// Drop-stale on backpressure: if the receiver hasn't drained
		// the buffer, replace the queued snapshot with the new one
		// rather than blocking the subprocess pipe. The freshest
		// snapshot is the only one we care about.
		select {
		case out <- snap:
		case <-ctx.Done():
			return nil
		default:
			select {
			case <-out:
			default:
			}
			select {
			case out <- snap:
			case <-ctx.Done():
				return nil
			}
		}
	}
	return sc.Err()
}
