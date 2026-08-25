package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
	"github.com/aplexica/aplexica/internal/adapter/claudecode"
	"github.com/aplexica/aplexica/internal/adapter/codex"
	"github.com/aplexica/aplexica/internal/adapter/hermes"
	"github.com/aplexica/aplexica/internal/adapter/kilo"
	"github.com/aplexica/aplexica/internal/adapter/openclaw"
	"github.com/aplexica/aplexica/internal/adapterstate"
	"github.com/aplexica/aplexica/internal/audit"
	"github.com/aplexica/aplexica/internal/blobstore"
	"github.com/aplexica/aplexica/internal/conflicts"
	"github.com/aplexica/aplexica/internal/daemon"
	"github.com/aplexica/aplexica/internal/generationactivation"
	"github.com/aplexica/aplexica/internal/hermesdb"
	"github.com/aplexica/aplexica/internal/hermeswatch"
	"github.com/aplexica/aplexica/internal/keyrotation"
	"github.com/aplexica/aplexica/internal/keys"
	"github.com/aplexica/aplexica/internal/metrics"
	"github.com/aplexica/aplexica/internal/nativebackup"
	"github.com/aplexica/aplexica/internal/pausestate"
	"github.com/aplexica/aplexica/internal/pending"
	"github.com/aplexica/aplexica/internal/plugin/manager"
	"github.com/aplexica/aplexica/internal/plugin/proto"
	"github.com/aplexica/aplexica/internal/plugin/secureexec"
	"github.com/aplexica/aplexica/internal/plugin/truststate"
	"github.com/aplexica/aplexica/internal/privatefs"
	"github.com/aplexica/aplexica/internal/project"
	"github.com/aplexica/aplexica/internal/projectdiscovery"
	"github.com/aplexica/aplexica/internal/rbac"
	"github.com/aplexica/aplexica/internal/retention"
	"github.com/aplexica/aplexica/internal/secrets"
	"github.com/aplexica/aplexica/internal/securityepoch"
	syncd "github.com/aplexica/aplexica/internal/sync"
	"github.com/aplexica/aplexica/internal/syncgate"
	"github.com/aplexica/aplexica/internal/syncrules"
	"github.com/aplexica/aplexica/internal/trayinstall"
	"github.com/aplexica/aplexica/internal/version"
	"github.com/aplexica/aplexica/internal/web"
	apiweb "github.com/aplexica/aplexica/internal/web/api"
	"github.com/aplexica/aplexica/internal/web/auth"
	"github.com/aplexica/aplexica/internal/web/sse"
	"github.com/spf13/cobra"
)

var (
	daemonDir      string
	daemonStateDir string
	daemonLogDir   string
	// Directory scanned for out-of-process adapter plugins. Empty
	// resolves to <state-dir>/plugins at runtime (see the serve body).
	// Each plugin lives in its own subdirectory holding a plugin.json
	// manifest plus the executable it names. Discovery, load, and
	// teardown are owned by internal/plugin/manager; any absence,
	// collision with a built-in, disablement, or per-plugin failure
	// degrades to the daemon's exact pre-plugin behavior.
	daemonPluginsDir          string
	daemonStoreRoot           string
	daemonSecretsRoot         string
	daemonQuiet               time.Duration
	daemonGuardWindow         time.Duration
	daemonRecursive           bool
	daemonHermesWatch         bool
	daemonHermesWatchInterval time.Duration
	daemonHermesDBPath        string

	// Per-kind snapshot cadence (v0.29.2; BRD-03 §4.8.1). Replaces
	// v0.29.1's single --snapshot-cadence-events flag.
	daemonSnapCadenceConv  int
	daemonSnapCadenceMem   int
	daemonSnapCadenceSkill int
	daemonSnapCadenceTool  int

	// Per-kind time-based snapshot trigger (v0.29.2; BRD-03 §4.8.1).
	daemonSnapMaxAgeConv  time.Duration
	daemonSnapMaxAgeMem   time.Duration
	daemonSnapMaxAgeSkill time.Duration
	daemonSnapMaxAgeTool  time.Duration

	// Disk-pressure sweep trigger (BRD-03 §4.8.2; FR-03.20). When the
	// store crosses this size in GB, the daemon runs ONE ordered
	// retention sweep (retention.RunPressureSweep): the attachments_only
	// OSS default evicts old attachment bytes + GCs them first (lossless
	// for text + chain), then snapshots, then — only if still over —
	// compacts history, re-checking the watermark at each phase boundary
	// and returning early once pressure is relieved. 0 = disabled.
	daemonStoreHighWatermarkGB float64

	// v0.62.0 BRD-02 §4.13.4 periodic project-detection scan.
	daemonProjectScanInterval     time.Duration
	daemonProjectScanRoots        []string
	daemonProjectScanMaxDepth     int
	daemonClaudeSessionScanWindow time.Duration

	// limits.max_artifact_size_mb resolved to bytes: 0 = built-in default
	// (64 MiB), negative = cap disabled. BRD-03 §4.3/§5.
	daemonMaxArtifactBytes int64

	// limits.max_session_file_mb resolved to bytes: 0 = built-in default
	// (512 MiB), negative = session cap disabled. The separate, larger
	// ingest cap for agent session transcripts (Claude/Codex) — aligned-
	// chain delta sync replicates them incrementally, so large transcripts
	// are admitted rather than refused at the generic artifact cap.
	daemonMaxSessionFileBytes int64

	// v0.99.0 BRD-04 §4.3.1 branch auto-archival lifecycle pass.
	daemonBranchAutoArchiveAfterDays int
	daemonBranchAutoArchiveInterval  time.Duration

	// v0.50.0 FR-03.26: when true, `aplexica daemon install` also
	// chains the tray autostart entry. The flag is bound to
	// daemonInstallCmd; the per-platform default kicks in when the
	// flag isn't explicitly set (macOS+Linux: false; Windows: true).
	daemonInstallTray bool

	// Part 3: when true, the daemon skips the one-time first-run native
	// safety snapshot of each discovered agent's global roots. Off by
	// default (the snapshot runs once on a fresh install).
	daemonNoInitialBackup bool

	// daemonWinDetachConsole (windows keep-alive task only): when set, serve
	// calls FreeConsole on startup so the Task-Scheduler-launched process shows
	// no console window. No effect on non-windows.
	daemonWinDetachConsole bool
)

// daemonPressureCheckInterval is how often the daemon walks the canonical
// store to check whether it exceeds StoreHighWatermarkGB. 5 minutes is
// long enough that the directory walk doesn't burn IO bandwidth and
// short enough that runaway growth is caught within one snapshot
// cadence. Not currently exposed as a flag.
const daemonPressureCheckInterval = 5 * time.Minute

// daemonLiveScanInterval is the runtime catch-up cadence over the broad
// daemon --dir. Production keeps this disabled: when --dir is a home folder,
// even stat-only recursive sweeps can burn CPU continuously. Targeted agent
// roots are still covered by daemonNativeLiveScanInterval, and Codex rollout
// sessions by daemonCodexSessionScanInterval.
const daemonLiveScanInterval = 0

// daemonNativeLiveScanInterval is the missed-event safety net for native agent
// roots. It intentionally skips the broad --dir sweep and scans only discovered
// agent roots. Realtime sync comes from platform watcher events plus the
// dedicated Codex/Claude pollers below. This is only a missed-event backstop,
// so keep it well clear of the five-second foreground path: recursively
// statting multi-gigabyte agent roots every five seconds consumed a steady
// fraction of a CPU even when no content changed.
const daemonNativeLiveScanInterval = 30 * time.Second

// daemonCodexSessionScanInterval is the hot path for Codex rollout transcripts.
// It checks only the current date-partitioned session dirs and lets the
// orchestrator's quiet-window/import cache decide when a file is ready.
const daemonCodexSessionScanInterval = 500 * time.Millisecond

// daemonClaudeSessionScanInterval is the hot path for Claude Code JSONL
// transcripts. It checks recently modified ~/.claude/projects files so a
// conversation continued immediately after remote materialization does not wait
// for the broad 5s native-root scan.
const daemonClaudeSessionScanInterval = 500 * time.Millisecond

// remoteMembershipRepublishDelay lets the membership-change callback return
// promptly while the role/recipient invalidations settle before retained-head
// republish asks the cloud plugin for the refreshed recipient roster.
const remoteMembershipRepublishDelay = 500 * time.Millisecond

// daemonProjectScanWarmup gives the orchestrator and startup initial scan a
// short window to settle before the first project auto-link sweep.
const daemonProjectScanWarmup = 10 * time.Second

// daemonPressureCache is the mutex-guarded snapshot of the canonical store's
// disk pressure shared between the disk-pressure goroutine (writer, one
// refresh per daemonPressureCheckInterval tick), the emergency-quota
// IngestGate (reader, via bytes()), and the status RPC (reader, via state()).
// All access goes through the methods so the gate sees a consistent cached
// size without ever walking the store itself (FR-03.21).
type daemonPressureCache struct {
	mu    sync.Mutex
	cfg   retention.Config
	state retention.PressureState
}

// setConfig records the retention config used to derive PressureState. Called
// once at wiring time before the goroutine starts refreshing.
func (c *daemonPressureCache) setConfig(cfg retention.Config) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cfg = cfg
}

// update recomputes the cached PressureState from a freshly-measured,
// classified store size. Called by the disk-pressure goroutine each tick.
func (c *daemonPressureCache) update(size retention.StoreSize) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.state = retention.ComputePressureState(size, c.cfg)
}

// bytes returns the last measured store size. Cheap (a single locked read);
// supplied to retention.EmergencyQuotaGate as its size accessor so the gate
// never walks the store.
func (c *daemonPressureCache) bytes() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state.Bytes
}

// snapshot returns the cached PressureState in the daemon.StorePressure shape
// the PressureProvider exposes to the status RPC, including the honest
// reclaimable/pinned split.
func (c *daemonPressureCache) snapshot() daemon.StorePressure {
	c.mu.Lock()
	defer c.mu.Unlock()
	return daemon.StorePressure{
		StoreBytes:              c.state.Bytes,
		StoreMaxBytes:           c.state.MaxBytes,
		StoreHighWatermarkBytes: c.state.HighWatermarkBytes,
		StoreReclaimableBytes:   c.state.ReclaimableBytes,
		StorePinnedBytes:        c.state.PinnedBytes,
		StoreEventLogBytes:      c.state.EventLogBytes,
		OverHighWatermark:       c.state.OverHighWatermark,
		OverEmergency:           c.state.OverEmergency,
		WatermarkUnreachable:    c.state.WatermarkUnreachable,
	}
}

// reharvestInterval is the debounced cadence on which the daemon re-runs the
// project-discovery harvest (projectdiscovery.Cache.Run): refresh the shared
// discovered-folders snapshot the pending handler reads, and refresh the
// agents set of already-registered folders. A timer (not the core watcher
// event loop) satisfies the spec's "debounced cadence" requirement. Two
// minutes is short enough that a freshly-active folder surfaces promptly and
// long enough that the harvest's cheap session-index reads stay negligible.
const reharvestInterval = 2 * time.Minute

var daemonCmd = &cobra.Command{
	Use:   "daemon",
	Short: "Manage the aplexica background sync daemon",
	Long: `The aplexica daemon runs aplexica sync in the background plus the
hermeswatch SQLite watcher for Hermes auto-sync. Use the subcommands to
start, stop, query status, and install as a system service.

Current limitations:
  - Single watched directory (set at start; restart to change).

SIGHUP behavior (unix only):
  - Rotates the daemon log file.
  - Re-reads <state-dir>/config.json and applies hot-reloadable fields
    live: logLevel, quiet, guardWindow, hermesWatchInterval. Non-hot
    fields (dir, recursive, stateDir, logDir, storeRoot, secretsRoot,
    hermesWatch, hermesDB) emit a "restart required" notice and continue
    with the prior value.
`,
}

// daemonProbeTimeout bounds the control-socket dial used to detect an
// already-running daemon (keeps `daemon start` idempotent).
const daemonProbeTimeout = 500 * time.Millisecond

// daemonAlreadyRunning reports whether a daemon is currently listening on the
// control socket. Re-running `daemon start` (e.g. the Windows keep-alive task's
// per-minute repetition) should be a clean no-op rather than spawning a serve
// that dies on the lock.
func daemonAlreadyRunning() bool {
	sock, err := daemonControlSocket()
	if err != nil {
		return false
	}
	c, err := net.DialTimeout("unix", sock, daemonProbeTimeout)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

func defaultDaemonWatchDir() error {
	if strings.TrimSpace(daemonDir) != "" {
		return nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("daemon: resolve default watched directory: %w", err)
	}
	if strings.TrimSpace(home) == "" {
		return fmt.Errorf("daemon: resolve default watched directory: home directory is empty")
	}
	daemonDir = home
	return nil
}

func trayBinaryName() string {
	if runtime.GOOS == "windows" {
		return "aplexicatray.exe"
	}
	return "aplexicatray"
}

// resolveTrayPath finds the companion tray binary that packaged installs must
// ship with aplexica. Prefer a sibling next to the currently-running aplexica
// executable so direct installs and Windows zip installs work even before PATH
// is refreshed; fall back to PATH for developer/source builds.
func resolveTrayPath(aplexicaPath string) (string, error) {
	if aplexicaPath != "" {
		exePath := aplexicaPath
		if real, err := filepath.EvalSymlinks(exePath); err == nil {
			exePath = real
		}
		sibling := filepath.Join(filepath.Dir(exePath), trayBinaryName())
		if fi, err := os.Stat(sibling); err == nil && !fi.IsDir() {
			return sibling, nil
		}
	}
	path, err := exec.LookPath("aplexicatray")
	if err != nil {
		return "", err
	}
	return path, nil
}

func trayLaunchArgs(aplexicaPath string) []string {
	args := []string{"--aplexica", aplexicaPath}
	if daemonStateDir != "" {
		args = append(args, "--state-dir", daemonStateDir)
	}
	if daemonLogDir != "" {
		args = append(args, "--log-dir", daemonLogDir)
	}
	return args
}

func trayOptions(trayPath, aplexicaPath string) trayinstall.Options {
	opts := trayinstall.Options{
		TrayPath:     trayPath,
		AplexicaPath: aplexicaPath,
		StateDir:     daemonStateDir,
		LogDir:       daemonLogDir,
	}
	return opts
}

func launchTrayCompanionIfEnabled(cfg *daemon.Config) error {
	if cfg == nil {
		configPath := filepath.Join(daemonStateDir, "config.json")
		loaded, err := daemon.LoadConfig(configPath)
		if err != nil {
			return err
		}
		cfg = loaded
	}
	if !daemon.TrayEnabled(cfg) {
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate executable: %w", err)
	}
	trayPath, err := resolveTrayPath(exe)
	if err != nil {
		return err
	}
	return startTrayCompanion(trayPath, exe)
}

func startTrayCompanion(trayPath, aplexicaPath string) error {
	if ok, reason := canLaunchTrayFromCurrentSession(); !ok {
		return fmt.Errorf("%s", reason)
	}
	c := exec.Command(trayPath, trayLaunchArgs(aplexicaPath)...)
	c.Stdout = nil
	c.Stderr = nil
	c.Stdin = nil
	c.SysProcAttr = detachSysProcAttr()
	if err := c.Start(); err != nil {
		return fmt.Errorf("start tray: %w", err)
	}
	if err := c.Process.Release(); err != nil {
		return fmt.Errorf("release tray: %w", err)
	}
	return nil
}

var daemonStartCmd = &cobra.Command{
	Use:   "start",
	Short: "Launch the daemon in the background",
	RunE: func(cmd *cobra.Command, args []string) error {
		// Idempotent: if a daemon is already responding on the control
		// socket, do nothing. This keeps the keep-alive repetition in the
		// Windows logon task (and any re-run of `daemon start`) a clean
		// no-op instead of spawning a serve that immediately dies on the
		// lock — and avoids any duplicate startup work.
		if daemonAlreadyRunning() {
			fmt.Fprintln(cmd.OutOrStdout(), "daemon: already running")
			if err := launchTrayCompanionIfEnabled(nil); err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "daemon: tray launch skipped: %v\n", err)
			}
			return nil
		}
		if err := defaultDaemonWatchDir(); err != nil {
			return err
		}
		// Self-exec: spawn "aplexica daemon serve" detached, return.
		exe, err := os.Executable()
		if err != nil {
			return fmt.Errorf("daemon: locate executable: %w", err)
		}
		childArgs := buildDaemonServeArgs(cmd)
		c := exec.Command(exe, childArgs...)
		c.Stdout = nil
		c.Stderr = nil
		c.Stdin = nil
		// Detach the child from this process group so Ctrl-C in this
		// terminal doesn't kill the daemon.
		c.SysProcAttr = detachSysProcAttr()
		if err := c.Start(); err != nil {
			return fmt.Errorf("daemon: start child: %w", err)
		}
		pid := c.Process.Pid
		// Detach — don't wait.
		if err := c.Process.Release(); err != nil {
			return fmt.Errorf("daemon: release child: %w", err)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "daemon: started pid=%d\n", pid)
		return nil
	},
}

// buildDaemonServeArgs constructs the argv for the self-exec'd
// `aplexica daemon serve` child. The always-forwarded flags carry the
// daemon's core wiring; the project-scan / branch-auto-archive /
// --config-set flags are forwarded ONLY when explicitly set on
// `daemon start` (cmd.Flags().Changed), so an override the user typed on
// start actually reaches the serving process instead of being silently
// dropped. Unset optional flags are omitted so the child keeps its own
// defaults / TOML-config values.
func buildDaemonServeArgs(cmd *cobra.Command) []string {
	args := []string{"daemon", "serve",
		"--dir", daemonDir,
		"--state-dir", daemonStateDir,
		"--log-dir", daemonLogDir,
		"--store", daemonStoreRoot,
		"--secrets-root", daemonSecretsRoot,
		"--quiet", daemonQuiet.String(),
		"--guard-window", daemonGuardWindow.String(),
		"--hermes-db", daemonHermesDBPath,
		"--hermes-watch-interval", daemonHermesWatchInterval.String(),
		fmt.Sprintf("--hermes-watch=%t", daemonHermesWatch),
		"--snapshot-cadence-conv", fmt.Sprintf("%d", daemonSnapCadenceConv),
		"--snapshot-cadence-mem", fmt.Sprintf("%d", daemonSnapCadenceMem),
		"--snapshot-cadence-skill", fmt.Sprintf("%d", daemonSnapCadenceSkill),
		"--snapshot-cadence-tool", fmt.Sprintf("%d", daemonSnapCadenceTool),
		"--snapshot-max-age-conv", daemonSnapMaxAgeConv.String(),
		"--snapshot-max-age-mem", daemonSnapMaxAgeMem.String(),
		"--snapshot-max-age-skill", daemonSnapMaxAgeSkill.String(),
		"--snapshot-max-age-tool", daemonSnapMaxAgeTool.String(),
		"--store-high-watermark-gb", fmt.Sprintf("%v", daemonStoreHighWatermarkGB),
		fmt.Sprintf("--no-initial-backup=%t", daemonNoInitialBackup),
		"--plugins-dir", daemonPluginsDir,
	}
	if daemonRecursive {
		args = append(args, "--recursive")
	}

	// Forward the advertised flags that were previously dropped, but only
	// when the operator actually set them on `daemon start`.
	if cmd.Flags().Changed("project-scan-interval") {
		args = append(args, "--project-scan-interval", daemonProjectScanInterval.String())
	}
	if cmd.Flags().Changed("project-scan-roots") {
		args = append(args, "--project-scan-roots", strings.Join(daemonProjectScanRoots, ","))
	}
	if cmd.Flags().Changed("project-scan-max-depth") {
		args = append(args, "--project-scan-max-depth", fmt.Sprintf("%d", daemonProjectScanMaxDepth))
	}
	if cmd.Flags().Changed("branch-auto-archive-after-days") {
		args = append(args, "--branch-auto-archive-after-days", fmt.Sprintf("%d", daemonBranchAutoArchiveAfterDays))
	}
	if cmd.Flags().Changed("branch-auto-archive-interval") {
		args = append(args, "--branch-auto-archive-interval", daemonBranchAutoArchiveInterval.String())
	}
	if cmd.Flags().Changed("config-set") {
		for _, kv := range daemonCLISets {
			args = append(args, "--config-set="+kv)
		}
	}
	return args
}

var daemonServeCmd = &cobra.Command{
	Use:    "serve",
	Short:  "Run the daemon in the foreground (used internally by `daemon start`)",
	Hidden: false, // visible — users can also run this directly under nohup, systemd-run, etc.
	RunE: func(cmd *cobra.Command, args []string) error {
		// FR-09.12: refuse to run with elevated privileges (root on
		// unix, an elevated/administrator token on Windows). The daemon
		// must run as the user's normal account so it writes history and
		// locks with the right ownership. Done before any console/config/
		// lock/log/store work so a privileged start fails fast and clean.
		if err := refusePrivilegedStartup(); err != nil {
			return err
		}
		// Windows keep-alive task launches us with --windows-detach-console so
		// the Task-Scheduler-spawned serve shows no console window. No-op on
		// non-windows and when the flag is unset (interactive serve keeps its
		// console for log output). Done first, before any startup work.
		if daemonWinDetachConsole {
			detachConsoleWindow()
		}
		// v0.27.0: optionally load config file from <state-dir>/config.json.
		// File values override compiled-in defaults; CLI flags that were
		// explicitly Changed() override file values. We apply the
		// precedence post-hoc: if a flag was NOT Changed and the config
		// file has a value, overwrite the flag-default with the file
		// value. The current effective config is then snapshotted for
		// diffing in the SIGHUP reload path.
		configPath := filepath.Join(daemonStateDir, "config.json")
		fileCfg, cerr := daemon.LoadConfig(configPath)
		if cerr != nil {
			return fmt.Errorf("daemon: load config: %w", cerr)
		}

		// One-time stderr notice when an existing
		// installation is upgraded to a version that ships the local
		// web UI. Detects "pre-W2 daemon" via the absence of a "web"
		// section in the on-disk config; persists defaults back to
		// suppress on subsequent starts. Brand-new installs (no
		// config file yet) and already-upgraded installs (web key
		// already present) are silent.
		//
		// Persistence failure is non-fatal: the daemon must keep
		// starting even if a transient config write error occurs.
		// We surface the failure on stderr so it's still visible.
		if _, ferr := daemon.EmitFirstRunWebNotice(configPath, os.Stderr); ferr != nil {
			fmt.Fprintf(os.Stderr,
				"aplexica: first-run-web notice persistence failed: %v\n", ferr)
		}

		applyConfigToFlags(cmd, fileCfg)

		// v0.74.0: layer the BRD-10 §10.1 TOML config system on top.
		// File-config (above) handles the v0.27.0 structured fields;
		// this new pass handles the documented defaults.toml schema
		// (project scan, retention, tray, etc.). Errors here are
		// returned but warnings are logged to stderr only.
		if err := applyDaemonConfigPackage(cmd); err != nil {
			return fmt.Errorf("daemon: apply config package: %w", err)
		}
		currentCfg := snapshotCurrentDaemonConfig()
		currentCfg.LogLevel = fileCfg.LogLevel // mirror the requested log level so future diffs notice changes
		// Seed the sync gate baseline from the file too — the gate has no
		// flag, so without this every reload sees SyncChanged=true on a
		// flag-derived zero baseline and re-applies + backfills for nothing.
		currentCfg.Sync = fileCfg.Sync

		// Acquire lock.
		lockPath := filepath.Join(daemonStateDir, "aplexicad.lock")
		lk, err := daemon.Acquire(lockPath)
		if err != nil {
			return err
		}
		defer lk.Release()

		// Open log.
		lg, logCloser, err := daemon.NewLogger(daemonLogDir)
		if err != nil {
			return err
		}
		defer logCloser.Close()
		// Apply the file-configured log level immediately (slog LevelVar
		// supports live updates, so SIGHUP can also flip this later).
		lg.SetLevel(daemon.ParseLogLevel(fileCfg.LogLevel))

		lg.Info("daemon starting", "pid", os.Getpid(), "dir", daemonDir, "recursive", daemonRecursive)
		if err := launchTrayCompanionIfEnabled(fileCfg); err != nil {
			lg.Warn("tray: companion launch skipped", "err", err)
		} else if daemon.TrayEnabled(fileCfg) {
			lg.Info("tray: companion launch requested")
		}

		// Build store + adapters + orchestrator.
		store := &acf.Store{Root: daemonStoreRoot}
		if err := store.Init(); err != nil {
			lg.Error("store init failed", "err", err)
			return err
		}

		// Disk-pressure cache + emergency-quota ingest gate (FR-03.21).
		// pressureCache holds the most recent store size and derived
		// PressureState, refreshed by the disk-pressure goroutine each
		// daemonPressureCheckInterval tick. The IngestGate consults only this
		// cached byte count (cheap — never walks the store), so the size it
		// sees can lag reality by up to one pressure-check interval. That
		// staleness is acceptable: the gate is a last-resort backstop that
		// fires AFTER the high-watermark sweep has had its chance, not a
		// byte-accurate limiter.
		//
		// gateCfg here is resolved once for the gate ceiling. The disk-
		// pressure goroutine below resolves its own copy for the watermark/
		// sweep; both read the same layered config so they agree. When
		// StoreMaxGB == 0 (unlimited) the gate stays nil (a no-op).
		pressureCache := &daemonPressureCache{}
		if gateEff, gerr := daemonRetentionEffective(); gerr != nil {
			lg.Warn("retention config load failed — emergency-quota ingest gate disabled", "err", gerr)
		} else if gateCfg, _, lerr := loadRetentionConfig(gateEff, retentionFlagOverrides{
			HighWatermarkGB:        daemonStoreHighWatermarkGB,
			HighWatermarkGBChanged: cmd.Flags().Changed("store-high-watermark-gb"),
		}); lerr != nil {
			lg.Warn("retention config resolve failed — emergency-quota ingest gate disabled", "err", lerr)
		} else if gateCfg.StoreMaxGB > 0 {
			pressureCache.setConfig(gateCfg)
			store.IngestGate = retention.EmergencyQuotaGate(pressureCache.bytes, gateCfg)
			lg.Info("emergency-quota ingest gate enabled",
				"store_max_gb", gateCfg.StoreMaxGB,
				"emergency_quota", gateCfg.StoreEmergencyQuota)
		}
		ss := &secrets.Store{Root: daemonSecretsRoot}
		if err := ss.Init(); err != nil {
			lg.Error("secrets init failed", "err", err)
			return err
		}
		// Rotation used to be unbounded. Reclaim an oversized legacy audit log
		// asynchronously so compression never delays the listener/sync startup
		// path; the secrets package serializes this with concurrent audit writes.
		go func() {
			if err := ss.MaintainAuditLog(); err != nil {
				lg.Warn("secret audit log maintenance failed", "err", err)
			}
		}()
		cc := claudecode.New()
		cc.SecretsStore = ss
		// Import Claude sessions as structured acf.conversation.v1 so they can
		// be transcoded into other agents' native sessions (cross-agent
		// conversation sync). Mirrors codex below.
		cc.CanonicalConversations = true
		// Opt-in repair for synthetic mirrors whose parentUuid graph forked
		// (sync.repairForkedMirrors in <state-dir>/config.json). Default off:
		// with the key absent the daemon's materialization behaviour is exactly
		// what it was before the repair existed. Startup-only, like the other
		// Sync knobs — a restart is required to change it.
		cc.RepairForkedMirrors = fileCfg.Sync.RepairForkedMirrors
		if cc.RepairForkedMirrors {
			lg.Info("forked conversation-mirror repair enabled (sync.repairForkedMirrors)",
				"scope", "synthetic Claude Code mirrors only",
				"note", "rebuilds in place only when every row in the mirror is provably canonical; pre-repair bytes are kept under ~/.aplexica/quarantine/claude-conversations")
		}
		cx := codex.New()
		cx.SecretsStore = ss
		// Import Codex sessions as structured acf.conversation.v1 events (vs
		// opaque jsonl) so they can be rendered into readable cross-agent
		// transcripts (the ConversationDocTarget materialization below).
		cx.CanonicalConversations = true
		k := kilo.New()
		k.SecretsStore = ss
		h := hermes.New()
		h.SecretsStore = ss
		oc := openclaw.New()
		oc.SecretsStore = ss
		// Import OpenClaw sessions as structured acf.conversation.v1 so
		// conversations born in its TUI materialize into the other agents'
		// session stores (outbound conversation sync — without this they
		// imported opaque and fanned out nowhere). Mirrors claude/codex above.
		oc.CanonicalConversations = true

		// Conflicts store — file-based; default ~/.aplexica/state/conflicts
		// per ADR-0038. Detection in the orchestrator records divergent
		// concurrent writes here instead of last-writer-wins; users resolve
		// via `aplexica conflicts resolve`.
		confStore := &conflicts.Store{Root: filepath.Join(daemonStateDir, "conflicts")}
		if err := confStore.Init(); err != nil {
			lg.Error("conflicts store init failed", "err", err)
			return err
		}

		cadence := map[acf.Kind]int{
			acf.KindConversation: daemonSnapCadenceConv,
			acf.KindMemory:       daemonSnapCadenceMem,
			acf.KindSkill:        daemonSnapCadenceSkill,
			acf.KindTool:         daemonSnapCadenceTool,
		}
		// v0.57.0: load the project registry so the orchestrator
		// can gate fan-out on project registration (BRD-02 §4.13
		// stage-and-wait). Missing registry file is OK — registry
		// initializes empty and every project-scope artifact stays
		// pending until the user runs `aplexica project link`.
		projectReg, perr := project.NewRegistry(filepath.Join(daemonStateDir, "projects.json"))
		if perr != nil {
			lg.Error("project registry load failed", "err", perr)
			return perr
		}
		auditRecorder := &audit.FileRecorder{Root: filepath.Join(daemonStateDir, "audit")}
		// Denied discovered folders the user dismissed from the pending list.
		// Kept beside projects.json so they don't keep re-surfacing each poll;
		// a load failure degrades to an empty (nothing-denied) store.
		deniedStore, derr := pending.LoadDenied(filepath.Join(daemonStateDir, "denied.json"))
		if derr != nil {
			lg.Warn("denied-projects store load failed; treating as empty", "err", derr)
		}
		// Dismissed "add agent X to project P" suggestions (keyed by
		// pending.SuggestionKey) so they stop re-appearing once dismissed.
		suggDismissed, serr := pending.LoadDenied(filepath.Join(daemonStateDir, "suggestions-dismissed.json"))
		if serr != nil {
			lg.Warn("dismissed-suggestions store load failed; treating as empty", "err", serr)
		}
		// Hand every adapter the SAME registry instance so a
		// registered LOCAL project's files stay project-scoped (the
		// scope-override in adapter.OpaqueParams.Registry). The same
		// projectReg also flows into the orchestrator Config below and
		// the web ProjectsHandler/pending harvest, so registration,
		// scope resolution, and fan-out all observe one source of truth.
		cc.Registry = projectReg
		cx.Registry = projectReg
		k.Registry = projectReg
		h.Registry = projectReg
		oc.Registry = projectReg

		// Register the primary --dir as an implicit LOCAL
		// project. Done BEFORE the orchestrator is built so scope resolution
		// observes the entry from the first import.
		registerImplicitDirProject(projectReg, lg, daemonDir)

		pauseStore := &pausestate.Store{Path: pausestate.DefaultPath(daemonStateDir)}
		quarantineTracker := syncd.DefaultQuarantineTracker()

		// v0.90.0 FR-03.8 adapter enable/disable filter. Disabled
		// adapters are skipped wholesale — they don't watch, Import,
		// or Export.
		ass := &adapterstate.Store{Path: adapterstate.DefaultPath(daemonStateDir)}
		disabledSet := ass.DisabledSet()
		all := []adapter.Adapter{cc, cx, k, h, oc}
		filtered := make([]adapter.Adapter, 0, len(all))
		for _, ad := range all {
			if _, off := disabledSet[ad.Name()]; off {
				lg.Info("adapter disabled by user; skipping",
					"name", ad.Name(),
					"state-file", adapterstate.DefaultPath(daemonStateDir))
				continue
			}
			filtered = append(filtered, ad)
		}

		// External adapter plugins (out-of-process, pure translators).
		// Purely additive: discover plugin.json manifests under the
		// plugins dir, spawn each as a subprocess, and append a proxy
		// that satisfies adapter.Adapter. The DAEMON still owns store
		// IO — the plugin returns a pure translation result and the
		// proxy's reconciler persists it (cf. proxy.Import).
		//
		// Fail-safe by construction: a plugin whose name collides with a
		// built-in or appears in disabledSet is skipped (the `skip` set
		// below); an absent/empty plugins dir, an unparseable manifest,
		// or any spawn/initialize failure all degrade to today's exact
		// behavior because manager.Load logs+skips per-plugin and never
		// errors for a bad plugin. The appended slice MUST be in place
		// before `filtered` is consumed by the discoveries loop and web
		// runtime. Only discovery-positive adapters are handed to sync.
		skip := make(map[string]struct{}, len(all)+len(disabledSet))
		for _, ad := range all {
			skip[ad.Name()] = struct{}{} // built-in names always win a collision
		}
		for name := range disabledSet {
			skip[name] = struct{}{} // user-disabled names are never loaded as plugins
		}
		pluginsDir := daemonPluginsDir
		if pluginsDir == "" {
			pluginsDir = filepath.Join(daemonStateDir, "plugins")
		}
		// DeviceID is "" here to match the existing publisher note (the
		// real device ID is populated from the secrets store post-
		// construction during pairing); built-ins record the same empty
		// provenance at this point.
		pluginMgr := manager.New(pluginsDir, store, "", version.Version, lg.Logger)
		defer pluginMgr.Close()
		pluginAdapters, perr := pluginMgr.Load(cmd.Context(), skip)
		if perr != nil {
			lg.Warn("adapter plugin load reported an internal error; continuing with whatever loaded",
				"plugins-dir", pluginsDir, "err", perr)
		}
		for _, l := range pluginMgr.Loaded() {
			lg.Info("adapter plugin loaded",
				"name", l.PluginName, "version", l.PluginVersion)
		}
		filtered = append(filtered, pluginAdapters...)

		// FR-03.3 (BRD-03 §4 / BRD-02 §4.13): detect installed agents at
		// startup via each adapter's Discover(). The map drives true
		// installed/not-installed presence in /api/agents + `aplexica
		// status`, and (Slice 2) the native global roots to watch.
		discoveries := make(map[string]adapter.Discovery, len(filtered))
		for _, ad := range filtered {
			d, derr := ad.Discover()
			if derr != nil {
				lg.Warn("adapter discovery failed; treating as not-installed",
					"name", ad.Name(), "err", derr)
				discoveries[ad.Name()] = adapter.Discovery{Installed: false, Detail: derr.Error()}
				continue
			}
			discoveries[ad.Name()] = d
			lg.Info("agent discovery",
				"name", ad.Name(), "installed", d.Installed,
				"roots", d.GlobalRoots, "detail", d.Detail)
		}
		runtimeAdapters := runtimeAdaptersFrom(filtered, discoveries)

		// v0.104.0 (FR-05.5/6/7): build the rules engine from the user's
		// rules.toml ONLY (safe-by-default; no always-on shipped
		// defaults — reverses BRD-05 §6 #1). Best-effort: a parse
		// failure falls back to an EMPTY engine (never the old defaults)
		// so a broken user file can't silently re-enable fan-out.
		rulesEngine, rerr := buildRulesEngine()
		if rerr != nil {
			lg.Warn("rules engine init fell back to an empty (deny-all) ruleset", "err", rerr)
		}

		// v0.107.0: per-daemon SSE event bus. Constructed unconditionally
		// so publishers (orchestrator below) can fire-and-forget regardless
		// of whether the web listener is enabled. When the listener is off,
		// the bus has zero subscribers and PublishKind is a no-op past
		// its sequence increment.
		eventBus := sse.NewBus()

		// Remote-transport plugin lifecycle. Construct
		// the runner if RemoteEnabled(fileCfg) is true; otherwise
		// leave it nil so the orchestrator publishes only locally.
		// The runner's Start spawns the plugin in a goroutine; Stop
		// is wired below in the orchestrator-cleanup defer chain.
		var remoteRunner *daemon.RemoteRunner
		// localDeviceID is this device's cloud identity (the plugin's
		// device_id). Seeded below from `<plugin> --status` when the plugin is
		// already paired; empty when unpaired (the orchestrator then forwards
		// for visibility without a cloud identity to dedupe against). Threaded
		// into the orchestrator Config (LocalDeviceID) for outbound origin
		// stamping + inbound loop prevention.
		var localDeviceID string
		// remotePublisher bridges the orchestrator's outbound hook to the
		// runner's Publish. Constructed below (it needs ctx for its pump) and
		// passed into the orchestrator Config; nil when remote is disabled.
		var remotePublisher *daemon.RemotePublishAdapter
		// End-to-end encryption: recipientResolver resolves the device set each
		// outbound event is sealed for; deviceKeyProv hands the orchestrator
		// this device's private wrap key to OPEN inbound envelopes. Both nil
		// when remote is disabled (the orchestrator then never encrypts/decrypts
		// — and with a nil resolver it drops outbound rather than send plaintext).
		var recipientResolver *daemon.RecipientResolver
		var deviceKeyProv daemon.DeviceKeyProvider
		var verifiedRosterProvider *daemon.VerifiedRosterProvider
		requireEnvelopeV2 := false
		var securityEpochCoordinator *securityepoch.Coordinator
		var generationActivationGate *generationactivation.PendingGate
		var generationActivationDriver *daemon.GenerationActivationDriver
		var rosterRenewalDriver *daemon.RosterRenewalDriver
		var deviceTransitionService *daemon.DeviceTransitionService
		var generationActivationTrigger chan struct{}
		var inboundInbox *daemon.InboundInbox
		var durableInboundCursors *daemon.DurableCursorStore
		var durableInboundGaps *daemon.DurableGapStore
		// prepareRemoteImmediately outlives the RemoteEnabled block: the
		// runner's post-connect RefreshIdentity hook (wired after the
		// orchestrator exists) re-runs queryPluginStatus with it.
		var prepareRemoteImmediately func(ctx context.Context, path string, args ...string) (preparedRemotePluginCommand, error)
		if daemon.RemoteEnabled(fileCfg) {
			observationSampleKey, observationKeyErr := daemon.LoadOrCreateRemoteSyncObservationSampleKey(ss)
			if observationKeyErr != nil {
				// Observation export is optional and must fail closed rather than
				// exposing guessable source identities. Core sync remains available.
				lg.Warn("remote: durable sync observation sample key unavailable; client observations disabled", "err", observationKeyErr)
			}
			identityRoot := filepath.Join(daemonStateDir, "identity")
			if err := privatefs.EnsureDir(identityRoot, privatefs.DirPolicy{
				Access:        privatefs.AccessPrivate,
				RepairOwned:   true,
				AllowExisting: true,
			}); err != nil {
				return fmt.Errorf("daemon: secure identity state: %w", err)
			}
			recoveredIdentity, recoveredGenesis, err := recoverRemoteIdentityStartup(cmd.Context(), identityRoot)
			if err != nil {
				// Recovery is intentionally before plugin verification/status,
				// RemoteRunner construction, publishers, resolvers, drivers, and
				// inbound callbacks. A corrupt or incomplete journal therefore
				// prevents even a one-shot plugin process from executing.
				return fmt.Errorf("daemon: remote identity startup: %w", err)
			}
			securityEpochCoordinator = recoveredIdentity.coordinator
			generationActivationGate = &generationactivation.PendingGate{IdentityRoot: identityRoot}
			generationActivationTrigger = make(chan struct{}, 1)
			if recoveredGenesis {
				lg.Info("remote: recovered existing-account identity transition before plugin startup")
			}
			remoteTrustStore := truststate.Store{Root: filepath.Join(daemonStateDir, "remote-plugin-trust")}
			prepareRemoteImmediately = func(ctx context.Context, path string, args ...string) (preparedRemotePluginCommand, error) {
				if path != fileCfg.Remote.Executable {
					return nil, errors.New("configured remote plugin path changed before launch")
				}
				verified, err := verifyRemotePluginWithCompiledTrust(path)
				if err != nil {
					return nil, err
				}
				_, err = remoteTrustStore.VerifyCurrent(path, verified, remotePluginTrustPolicy())
				if err != nil {
					return nil, err
				}
				return secureexec.Prepare(ctx, path, verified.Manifest.BinarySHA256, args...)
			}
			verifiedRemotePlugin, verifyErr := verifyRemotePluginWithCompiledTrust(fileCfg.Remote.Executable)
			remoteStartupAuthorized := verifyErr == nil
			if remoteStartupAuthorized {
				_, verifyErr = remoteTrustStore.VerifyCurrent(fileCfg.Remote.Executable, verifiedRemotePlugin, remotePluginTrustPolicy())
				remoteStartupAuthorized = verifyErr == nil
			}
			if !remoteStartupAuthorized {
				// Keep the local-only daemon and UI available, but do not execute a
				// single plugin subprocess. The runner repeats this fail-closed check
				// before every attempted spawn and surfaces the error in remote status.
				lg.Warn("remote: configured plugin is not authorized; cloud sync disabled until reinstalled", "err", verifyErr)
			}
			// Seed the device id from the plugin so outbound events carry a
			// real Origin and the orchestrator can tell local from remote.
			// Best-effort: an unpaired/missing plugin leaves it empty.
			if remoteStartupAuthorized {
				_, did, _ := queryPluginStatus(cmd.Context(), fileCfg.Remote.Executable, prepareRemoteImmediately)
				if did != "" {
					localDeviceID = did
					cc.SetDeviceID(did)
					cx.SetDeviceID(did)
					k.SetDeviceID(did)
					h.SetDeviceID(did)
					oc.SetDeviceID(did)
					lg.Info("remote: seeded device identity from plugin", "device_id", did)
				} else {
					lg.Info("remote: device id not yet known (plugin unpaired); outbound origin will be empty until pairing")
				}
			}
			remoteRunner = &daemon.RemoteRunner{
				Executable:              fileCfg.Remote.Executable,
				DeviceID:                localDeviceID,
				Version:                 version.Version,
				PublisherKeys:           remotePluginPublisherKeys(),
				PluginVerifier:          verifyRemotePluginWithCompiledTrust,
				TrustStore:              remoteTrustStore,
				TrustPolicy:             remotePluginTrustPolicy(),
				TransferRoot:            filepath.Join(daemonStateDir, "outbox", "staged"),
				ObservationSampleKey:    observationSampleKey,
				Logger:                  lg,
				RetainedInboundInterval: 200 * time.Millisecond,
				// OnInbound + OnConnState + OnEnumerateHint wired
				// after the orchestrator is constructed below so the
				// callbacks can reference orch directly.
			}
			// The publish adapter's pump lives for the daemon process lifetime
			// (cmd.Context()); it stops when the command context is cancelled.
			remotePublisher = daemon.NewRemotePublishAdapter(cmd.Context(), remoteRunner, filepath.Join(daemonStateDir, "outbox"), lg)
			inboundInbox = &daemon.InboundInbox{Root: filepath.Join(daemonStateDir, "inbox-v2")}
			durableInboundCursors = &daemon.DurableCursorStore{Root: filepath.Join(daemonStateDir, "durable-cursors")}
			durableInboundGaps = &daemon.DurableGapStore{Root: filepath.Join(daemonStateDir, "durable-gaps")}
			remoteRunner.DurableCursorStore = durableInboundCursors
			remoteRunner.DurableInbox = inboundInbox
			remotePublisher.SetSecurityEpochCoordinator(securityEpochCoordinator)
			remotePublisher.SetGenerationActivationGate(generationActivationGate)
			remotePublisher.SetProjectAuthorizer(projectReg.IsAuthorized)
			remoteRunner.OnCheckpointNeededV1 = func(notification proto.RemoteCheckpointNeededV1Notification) {
				if remotePublisher == nil {
					return
				}
				if err := remotePublisher.HandleCheckpointNeededV1(notification); err != nil {
					lg.Warn("remote: checkpoint request rejected", "request_id", notification.RequestID, "err", err)
				}
			}
			// Surface publisher-level conditions (oversized dead-letters) on
			// the SSE event bus so a thread that silently stops syncing is
			// visible to the web UI instead of only a Warn log.
			remotePublisher.SetEventNotifier(func(kind string, body map[string]any) {
				sseBusPublisher{bus: eventBus}.Publish(kind, body)
			})

			// End-to-end encryption wiring. The device key provider is backed by the
			// daemon's secrets store (the same X25519 key registered with the
			// cloud at pairing). The recipient resolver round-trips
			// remote.list_namespace_devices (cached) and ALWAYS includes this
			// device (so the sender decrypts its own re-imports). deviceIDFn is
			// read lazily off the runner so a device id learned at a later pairing
			// is picked up without a restart.
			deviceKeyProv = daemon.NewDeviceKeyProvider(ss)
			generationActivationDriver = &daemon.GenerationActivationDriver{
				IdentityRoot: identityRoot,
				Runner:       remoteRunner,
				Identity:     &keys.DeviceIdentityStore{Secrets: ss},
				Logger:       lg,
				Interval:     5 * time.Second,
				Trigger:      generationActivationTrigger,
			}
			rosterRenewalDriver = &daemon.RosterRenewalDriver{
				IdentityRoot: identityRoot,
				Runner:       remoteRunner,
				Identity:     &keys.DeviceIdentityStore{Secrets: ss},
				Security:     securityEpochCoordinator,
				Logger:       lg,
				Interval:     time.Minute,
			}
			verifiedRosterProvider = daemon.NewVerifiedRosterProvider(identityRoot)
			requireEnvelopeV2 = remoteEnvelopeV2CutoverRequired(cmd.Context(), identityRoot, verifiedRosterProvider)
			if requireEnvelopeV2 {
				lg.Info("remote: signed envelope v2 cutover is active")
			} else {
				lg.Info("remote: signed envelope v2 migration not established; using encrypted legacy overlap")
			}
			recipientResolver = daemon.NewRecipientResolverFromRunner(
				cmd.Context(),
				remoteRunner,
				remoteRunner.CurrentDeviceID,
				deviceKeyProv,
				lg,
			)
			deviceTransitionService = &daemon.DeviceTransitionService{
				IdentityRoot: identityRoot,
				Runner:       remoteRunner,
				Identity:     &keys.DeviceIdentityStore{Secrets: ss},
				Security:     securityEpochCoordinator,
				Publisher:    remotePublisher,
				Recipients:   recipientResolver,
				Logger:       lg,
				Interval:     5 * time.Second,
			}
		}

		backupAgentRoots := func() []nativebackup.AgentRoots {
			return agentRootsFromAdapters(filtered)
		}
		backupMgr := newNativeBackupManager(nativeBackupsRoot(), backupAgentRoots)
		var startupSafetySignatures map[string]string
		// Reclaim orphaned cloud-staging snapshot directories left by interrupted
		// or pre-fix scheduled cloud backups (the .cloud-staging disk leak) on
		// every start, independent of the safety-backup toggle below. Runs in a
		// goroutine: a large pre-fix leak can be 100+ GB and deleting it must not
		// block the daemon (web listener, sync) from coming up.
		go backupMgr.SweepCloudStaging(lg)
		// A killed cloud restore leaves both its encrypted download and extracted
		// tree because process-level defers cannot run. Reclaim those disposable
		// objects on every start without racing an active cloud operation.
		go backupMgr.SweepCloudDownloads(lg)
		backupBlocker := syncd.NewAdapterBlocker(nil)
		startupSafetyDoneCh := make(chan struct{})
		var startupSafetyDone <-chan struct{} = startupSafetyDoneCh
		if daemonNoInitialBackup {
			lg.Info("native safety backups disabled (--no-initial-backup)")
			close(startupSafetyDoneCh)
		} else {
			// Pin this pass to the discovery snapshot already used to construct
			// the runtime adapter set. This avoids another potentially expensive
			// CLI/Desktop discovery pass and gives the pending blocker and verifier
			// one exact topology to agree on.
			startupSafetyAgents := agentRootsFromDiscoveries(discoveries)
			startupSafetySignatures = nativeSafetyRootSignatures(startupSafetyAgents)
			startupSafetyDone = startNativeStartupSafety(backupMgr, lg, startupSafetyAgents, backupBlocker)
		}
		// Existing snapshots may predate today's cache/runtime/credential
		// exclusions even when creation of new safety snapshots is disabled.
		// Authenticate, rebuild, and reclaim them in background after the web
		// listener startup path; Manager.opMu serializes this with backup/restore.
		go func() {
			<-startupSafetyDone
			backupMgr.SweepNativeBackupHistory(lg)
		}()
		runtimeAdapterActivated := func(activated string, discovery adapter.Discovery) {
			if daemonNoInitialBackup {
				return
			}
			// The initial runtime-discovery callback observes the same adapter
			// topology as startup. Its side effects are already gated by the live
			// pending/success/failure blocker, so do not synchronously hash every
			// multi-gigabyte snapshot a second time. A new or changed topology is
			// not covered and retains the original synchronous safety pass.
			if startupSafetyCoversDiscovery(startupSafetySignatures, activated, discovery) {
				return
			}
			blocks := backupMgr.EnsureStartupSafety(lg)
			applyRuntimeBackupBlocks(backupBlocker, activated, blocks)
		}

		// FR-03.3 §4: watch each installed agent's native global root in
		// addition to the primary --dir. Edits in ~/.claude, ~/.codex, etc.
		// import into the canonical store. Deduped; a root shared by two
		// adapters is watched once. (Cross-agent fan-out of these imports
		// remains gated by the await-config SyncGate, Slice 3.)
		//
		// Hermes is EXCLUDED: it stores state in a single multi-megabyte
		// SQLite DB (~/.hermes/state.db) handled by the dedicated, incremental
		// `hermeswatch` loop. Routing it through the generic file watcher +
		// the synchronous startup InitialScan would re-import the whole DB on
		// every boot and block the web listener for tens of seconds. Skipping
		// it here lets hermeswatch own hermes (the only V1 adapter with a
		// bespoke native watcher).
		var nativeRoots []string
		var recursiveRoots []string
		var metadataRoots []string
		var watchFiles []string
		rootsByAdapter := map[string][]string{}
		seenRoot := map[string]struct{}{}
		for _, ad := range filtered {
			name := ad.Name()
			d := discoveries[name]
			_, runtimeDiscoverable := ad.(adapter.RuntimeDiscoverable)
			if dynamic, ok := ad.(adapter.RuntimeDiscoverable); ok {
				d = mergeDiscoveryRoots(d, dynamic.CandidateDiscovery())
			}
			if !d.Installed && !runtimeDiscoverable {
				continue
			}
			if name == "hermes" {
				// Watch ONLY hermes' memory subdir (<root>/memories), not
				// the whole ~/.hermes — but DO watch it: skipping hermes
				// wholesale made its memory sync silently export-only
				// (a memory created by hermes never reached any other
				// agent; found in E2E F2). The subdir holds a handful of
				// markdown files, so the InitialScan cost that justified
				// excluding state.db doesn't apply. Registered in
				// rootsByAdapter so path ownership attributes imports to
				// hermes (basename MEMORY.md collides with openclaw's).
				// Skipped when absent (created on first export; watched
				// from the next daemon start).
				for _, r := range d.GlobalRoots {
					mem := filepath.Join(r, "memories")
					if st, serr := os.Stat(mem); serr != nil || !st.IsDir() {
						continue
					}
					rootsByAdapter[name] = append(rootsByAdapter[name], mem)
					if _, dup := seenRoot[mem]; dup {
						continue
					}
					seenRoot[mem] = struct{}{}
					nativeRoots = append(nativeRoots, mem)
				}
				continue
			}
			// Record every root this agent owns (flat + recursive + single
			// watched files) so the source-picker can break extension ties
			// by path ownership.
			rootsByAdapter[name] = append(append(append(append([]string{}, d.GlobalRoots...), d.RecursiveRoots...), d.MetadataRoots...), d.WatchFiles...)
			for _, r := range d.GlobalRoots {
				if _, dup := seenRoot[r]; dup {
					continue
				}
				seenRoot[r] = struct{}{}
				nativeRoots = append(nativeRoots, r)
			}
			for _, r := range d.RecursiveRoots {
				if _, dup := seenRoot[r]; dup {
					continue
				}
				seenRoot[r] = struct{}{}
				recursiveRoots = append(recursiveRoots, r)
			}
			for _, r := range d.MetadataRoots {
				if _, dup := seenRoot[r]; dup {
					continue
				}
				seenRoot[r] = struct{}{}
				metadataRoots = append(metadataRoots, r)
			}
			for _, f := range d.WatchFiles {
				if _, dup := seenRoot[f]; dup {
					continue
				}
				seenRoot[f] = struct{}{}
				watchFiles = append(watchFiles, f)
			}
		}
		sort.Strings(nativeRoots) // deterministic order for logging/tests
		sort.Strings(recursiveRoots)
		sort.Strings(metadataRoots)
		sort.Strings(watchFiles)
		// Union of every agent's own native roots. Passed to project discovery
		// so an agent's config/data dir (e.g. ~/.config/kilo) is never offered
		// as a user "project" candidate (it's already watched as a native root).
		agentNativeRoots := append(append(append([]string{}, nativeRoots...), recursiveRoots...), metadataRoots...)
		if len(nativeRoots) > 0 || len(recursiveRoots) > 0 || len(metadataRoots) > 0 || len(watchFiles) > 0 {
			lg.Info("watching discovered native roots (FR-03.3 §4)",
				"roots", nativeRoots, "recursiveRoots", recursiveRoots, "metadataRoots", metadataRoots, "watchFiles", watchFiles)
		}

		// FR-03.3 await-config gate: discovered agents import + show, but
		// the daemon withholds cross-agent fan-out to any agent the user
		// hasn't enabled via `aplexica sync enable`. Default (empty Sync
		// config) denies every target.
		syncGate := syncgate.New(daemon.SyncGateConfig(*fileCfg))
		lg.Info("fan-out gate (await-config)",
			"all", fileCfg.Sync.All, "agents", fileCfg.Sync.Agents,
			"note", "discovered agents import to the store; fan-out is enabled only for configured agents")

		orch, err := syncd.NewOrchestrator(syncd.Config{
			Dir:                     daemonDir,
			AdditionalRoots:         nativeRoots,
			RecursiveRoots:          recursiveRoots,
			MetadataRoots:           metadataRoots,
			WatchFiles:              watchFiles,
			RootsByAdapter:          rootsByAdapter,
			Adapters:                runtimeAdapters,
			DynamicAdapterDiscovery: true,
			RuntimeAdapterActivated: runtimeAdapterActivated,
			Store:                   store,
			SyncGate:                syncGate,
			QuietPeriod:             daemonQuiet,
			GuardWindow:             daemonGuardWindow,
			LiveScanInterval:        daemonLiveScanInterval,
			// The daemon starts a dedicated 500ms Codex rollout poller below.
			// Keep the generic native scanner off ~/.codex/sessions so the two
			// loops cannot wedge behind the same hot conversation file.
			DedicatedCodexSessionScan: true,
			RecentClaudeSessionWindow: daemonClaudeSessionScanWindow,
			MaxArtifactBytes:          daemonMaxArtifactBytes,
			MaxSessionFileBytes:       daemonMaxSessionFileBytes,
			Recursive:                 daemonRecursive,
			ConflictStore:             confStore,
			ConflictWindow:            30 * time.Second,
			SnapshotCadence:           cadence,
			ProjectRegistry:           projectReg,
			PauseStore:                pauseStore,
			Quarantine:                quarantineTracker,
			AdapterBlocker:            backupBlocker,
			RulesEngine:               rulesEngine,
			EventPublisher:            sseBusPublisher{bus: eventBus},
			LocalDeviceID:             localDeviceID,
			RequireEnvelopeV2:         requireEnvelopeV2,
			VerifiedRosterProvider:    verifiedRosterProvider,
			V2IdentityProvider:        deviceKeyProv,
			NamespaceKeyProvider:      &keyrotation.NamespaceKeyStore{Root: filepath.Join(daemonStateDir, "identity", "namespace-keys")},
			Logger:                    lg,
		})
		if err != nil {
			lg.Error("orchestrator init failed", "err", err)
			return err
		}
		defer orch.Close()
		backupMgr.restoreCoordinator = orchestratorNativeRestoreCoordinator{orch: orch}

		// Wire the outbound remote-event hook and the inbound import
		// entrypoint now that BOTH the orchestrator and the runner exist. Set
		// only when remote is enabled so the OSS / un-configured daemon keeps a
		// nil publisher (a typed-nil interface would make the nil-guard pass
		// and then panic on the first commit). The orchestrator forwards each
		// locally-committed event via remotePublisher and imports each
		// remote.inbound batch via orch.ImportInbound (which marks the remote
		// origin so the outbound path never bounces it back — loop prevention).
		if remotePublisher != nil {
			orch.SetRemoteEventPublisher(remotePublisher)
			remotePublisher.SetRecoverySource(orch)
		}
		go runProjectRegistryController(cmd.Context(), projectReg, orch, remotePublisher, lg)
		// Continuous convergence: re-drive materializations that were lost to
		// a closed gate, an exhausted retry budget or a missed watcher event,
		// without waiting for a human to run `aplexica daemon reload`. It
		// declines to sweep a device whose store is unchanged and which owes
		// no writes, and backs off exponentially when it finds nothing, so a
		// healthy device converges toward almost no work.
		go orch.RunConvergence(cmd.Context())
		// Per-agent conversation-backfill caps (FR: bound the "enable agent →
		// replicate full history" flood). Default 10 / -1 = all when unset.
		orch.SetConvBackfill(fileCfg.Sync.ConvBackfill)
		// End-to-end encryption: install the recipient resolver (outbound seal) +
		// device key provider (inbound open). Set only when remote is enabled.
		// With these wired, OutboundEvent.Bytes is a ciphertext envelope and an
		// empty recipient set DROPS the event — the daemon never sends plaintext.
		if recipientResolver != nil {
			orch.SetRecipientResolver(recipientResolver)
			orch.SetDeviceKeyProvider(deviceKeyProv)
			orch.SetV2IdentityProvider(deviceKeyProv)
		}
		// ctlSrv is assigned below (after the web-API wiring); declared here so
		// the RefreshIdentity closure can propagate a rotated cloud identity
		// into the control-socket status response. The hook only runs after
		// remoteRunner.Start, which follows the assignment.
		var ctlSrv *daemon.ControlServer
		if remoteRunner != nil {
			// Pairing via `<plugin> --pair` on the CLI bypasses the
			// web API, so nothing tells this process the cloud device id
			// rotated — the daemon would stamp the RETIRED id on every
			// outbound event until restart. The plugin reconnects with the
			// fresh credentials after such a pair; re-query its identity on
			// every successful (re)connection and repropagate on change.
			remoteRunner.RefreshIdentity = func(ctx context.Context) {
				// The query runs INSIDE cloudIdentityMu: a --status result
				// obtained before a concurrent (re-)pair wrote fresh
				// credentials would otherwise be applied after the pair's own
				// propagation, regressing every component to the retired id
				// until the next reconnect — the same failure mode
				// reintroduced as a race. Under the lock, every applied id is
				// derived from credentials read after the previous apply.
				cloudIdentityMu.Lock()
				defer cloudIdentityMu.Unlock()
				_, did, _ := queryPluginStatus(ctx, fileCfg.Remote.Executable, prepareRemoteImmediately)
				if did == "" || did == remoteRunner.CurrentDeviceID() {
					return
				}
				applyCloudDeviceIdentityLocked(did, remoteRunner, orch, filtered, ctlSrv)
			}
			var admissionGate remoteAdmissionGate
			var durableRestartRepair durableInboundRestartRepair
			var durableInboundPhaseGate sync.Mutex
			remoteRunner.OnInbound = func(events []proto.RemoteEvent) {
				if err := checkGenerationActivationAdmission(generationActivationGate, events); err != nil {
					lg.Info("remote: legacy inbound blocked by pending generation activation", "err", err)
					return
				}
				orch.ImportInbound(events)
			}
			remoteRunner.OnInboundV2 = func(delivery proto.RemoteInboundDeliveryV2) proto.RemoteInboundAckV2 {
				retryAck := func(reason string) proto.RemoteInboundAckV2 {
					ack := proto.RemoteInboundAckV2{DeliveryID: delivery.DeliveryID, Outcomes: make([]proto.RemoteInboundEventOutcomeV2, len(delivery.Events))}
					for i := range ack.Outcomes {
						ack.Outcomes[i] = proto.RemoteInboundEventOutcomeV2{Index: uint32(i), Disposition: "retryable", ReasonCode: reason}
					}
					return ack
				}
				if len(delivery.Events) == 0 || securityEpochCoordinator == nil || inboundInbox == nil {
					return retryAck("admission-unavailable")
				}
				if err := checkGenerationActivationAdmission(generationActivationGate, delivery.Events); err != nil {
					lg.Info("remote: inbound blocked by pending generation activation", "err", err)
					return retryAck("generation-activation-pending")
				}
				remoteIdentity := remoteRunner.CurrentDeviceID()
				if len(delivery.Events) > 1 && !remoteRunner.SignedRedactionSafeBatchReady() {
					return retryAck("batch-capability-unavailable")
				}
				cursorBinding, bindErr := bindDurableInboundCursor(remoteIdentity, remoteRunner.SyncNegotiation(), delivery)
				if bindErr != nil {
					return retryAck("durable-metadata-invalid")
				}
				durableDelivery := cursorBinding != nil
				if durableDelivery {
					durableInboundPhaseGate.Lock()
					defer durableInboundPhaseGate.Unlock()
					if err := durableRestartRepair.ensure(inboundInbox, durableInboundCursors); err != nil {
						lg.Info("remote: durable inbound restart repair unavailable (will retry)", "err", err)
						return retryAck("cursor-repair-failed")
					}
					if err := durableInboundFinalizeBarrier(inboundInbox, durableInboundCursors, cursorBinding); err != nil {
						lg.Info("remote: durable inbound predecessor awaits native finalize", "err", err)
						return retryAck("predecessor-finalize-required")
					}
				}
				if terminal, ok := terminalPreAdmissionAck(delivery); ok && !durableDelivery {
					cached, err := inboundInbox.QuarantineTerminal(delivery, terminal)
					if err != nil {
						return retryAck("quarantine-commit-failed")
					}
					observeRemoteSyncCount(remoteRunner, proto.RemoteSyncMetricQuarantine, "terminal-quarantine:"+delivery.DeliveryID)
					return *cached
				}
				if !admissionGate.begin(time.Now()) {
					return retryAck("admission-backoff")
				}
				scope := delivery.Events[0].NamespaceID
				if scope == "" {
					scope = "account"
				}
				var admission daemon.InboundAdmissionV2
				var cached *proto.RemoteInboundAckV2
				err := securityEpochCoordinator.WithAdmission(scope, func(current securityepoch.SecurityEpoch) error {
					var err error
					if durableDelivery {
						admission, cached, err = inboundInbox.AdmitDurable(delivery, current, remoteIdentity)
					} else {
						admission, cached, err = inboundInbox.Admit(delivery, current)
					}
					return err
				})
				admissionGate.finish(time.Now(), err == nil)
				if err != nil {
					lg.Info("remote: inbound admission unavailable (will retry)", "err", err)
					return retryAck("admission-unavailable")
				}
				if cached != nil {
					if durableDelivery {
						if !durableCachedAckSafe(delivery, *cached) {
							return retryAck("durable-terminal-unsafe")
						}
						if delivery.StagedCheckpoint != nil {
							if err := remoteRunner.CompleteInboundStagedCheckpoint(delivery); err != nil {
								lg.Info("remote: staged inbound cleanup unavailable (will retry)", "err", err)
								return retryAck("staged-cleanup-failed")
							}
						}
						if err := advanceDurableInboundCursor(durableInboundCursors, cursorBinding, true); err != nil {
							lg.Info("remote: durable inbound cursor repair unavailable (will retry)", "err", err)
							return retryAck("cursor-commit-failed")
						}
						if err := pruneDurableInboundCompletion(inboundInbox, durableInboundCursors, cursorBinding); err != nil {
							lg.Info("remote: durable inbound completion pruning unavailable (will retry)", "err", err)
							return retryAck("inbox-prune-failed")
						}
						observeRemoteSyncCount(remoteRunner, proto.RemoteSyncMetricDuplicateDelivery, "durable-inbox-duplicate:"+delivery.DeliveryID)
					}
					return *cached
				}
				workingDelivery := delivery
				if delivery.StagedCheckpoint != nil {
					hydrated, hydrateErr := remoteRunner.HydrateInboundStagedCheckpoint(cmd.Context(), delivery)
					if hydrateErr != nil {
						lg.Info("remote: staged inbound checkpoint unavailable (will retry)", "err", hydrateErr)
						return retryAck("staged-checkpoint-unavailable")
					}
					workingDelivery = hydrated
				}
				importer := orch.ImportInboundResults
				if durableDelivery {
					importer = orch.ImportInboundCanonicalResults
				}
				results := importer(workingDelivery.Events)
				var gapRecoveryEvidence durableGapRecoveryEvidence
				if durableDelivery && durableGapNeedsRecovery(results) {
					if delivery.StagedCheckpoint != nil {
						return retryAck("staged-checkpoint-gap")
					}
					recovered, gapErr := recoverDurableInboundGap(cmd.Context(), durableInboundGaps, remoteRunner, durableInboundCursors, importer, cursorBinding, workingDelivery, results, &gapRecoveryEvidence)
					results = recovered
					if gapErr != nil {
						lg.Info("remote: durable inbound gap remains stopped", "delivery_id", delivery.DeliveryID, "err", gapErr)
						if errors.Is(gapErr, daemon.ErrDurableGapFull) {
							observeRemoteSyncCount(remoteRunner, proto.RemoteSyncMetricUnfillableGap, "unfillable-gap:"+delivery.DeliveryID)
						}
						if gapRecoveryEvidence.checkpointRestoreFailed {
							observeRemoteSyncCount(remoteRunner, proto.RemoteSyncMetricCheckpointRestoreFailure, "checkpoint-restore-failure:"+delivery.DeliveryID)
						}
					}
				}
				ack, terminal := inboundV2AckFromResults(workingDelivery, results, durableDelivery)
				if terminal && durableDelivery {
					gapKey, keyErr := durableGapKey(cursorBinding)
					if keyErr != nil {
						return retryAck("gap-metadata-invalid")
					}
					if err := durableInboundGaps.Resolve(gapKey, delivery.DeliveryID); err != nil {
						lg.Info("remote: durable inbound gap cleanup unavailable (will retry)", "err", err)
						return retryAck("gap-resolve-failed")
					}
				}
				if terminal {
					ack.NextCursor = delivery.Cursor
					if durableDelivery {
						ack.NextCursorDigest = delivery.CursorDigest
						ack.NextPosition = delivery.Position
						var finalizeEvidence proto.RemoteInboundFinalizeEvidenceV1
						var evidenceErr error
						if len(workingDelivery.Events) > 1 {
							finalizeEvidence, evidenceErr = durableInboundBatchFinalizeEvidence(
								remoteIdentity, delivery, results, orch.CanonicalEvidenceForTerminalInbound, &gapRecoveryEvidence,
							)
						} else {
							var canonicalEvidence syncd.InboundCanonicalEvidence
							resolveEvent := workingDelivery.Events[0]
							resolveOutcome := results[0]
							if covered, ok := gapRecoveryEvidence.covered[0]; ok {
								resolveEvent = covered.checkpoint.Event
								resolveOutcome = syncd.ImportApplied
							}
							canonicalEvidence, evidenceErr = orch.CanonicalEvidenceForTerminalInbound(resolveEvent, resolveOutcome)
							if evidenceErr == nil {
								finalizeEvidence = durableInboundFinalizeEvidence(remoteIdentity, delivery, canonicalEvidence)
								finalizeEvidence.CheckpointCoveragePlan, finalizeEvidence.CheckpointCoverageDigest, evidenceErr = durableCheckpointCoveragePlan(workingDelivery, &gapRecoveryEvidence)
								if evidenceErr == nil && finalizeEvidence.CheckpointCoveragePlan != "" {
									finalizeEvidence.FinalizeKind = proto.InboundFinalizeCheckpointCovered
								}
							}
						}
						if evidenceErr != nil {
							lg.Info("remote: durable inbound canonical evidence unavailable (will retry)", "err", evidenceErr)
							return retryAck("canonical-evidence-unavailable")
						}
						ack.FinalizeEvidence = &finalizeEvidence
					}
					var completeErr error
					if durableDelivery {
						completeErr = inboundInbox.CompleteDurable(delivery.DeliveryID, admission.InputSHA256, ack)
					} else {
						completeErr = inboundInbox.Complete(delivery.DeliveryID, admission.InputSHA256, ack)
					}
					if completeErr != nil {
						return retryAck("admission-commit-failed")
					}
					if delivery.StagedCheckpoint != nil {
						if err := remoteRunner.CompleteInboundStagedCheckpoint(delivery); err != nil {
							lg.Info("remote: staged inbound cleanup unavailable (will retry)", "err", err)
							return retryAck("staged-cleanup-failed")
						}
					}
					if durableDelivery {
						if err := advanceDurableInboundCursor(durableInboundCursors, cursorBinding, false); err != nil {
							lg.Info("remote: durable inbound cursor commit unavailable (will retry)", "err", err)
							return retryAck("cursor-commit-failed")
						}
						if err := pruneDurableInboundCompletion(inboundInbox, durableInboundCursors, cursorBinding); err != nil {
							lg.Info("remote: durable inbound completion pruning unavailable (will retry)", "err", err)
							return retryAck("inbox-prune-failed")
						}
					}
					if remoteInboundAckHasDisposition(ack, "quarantined") {
						observeRemoteSyncCount(remoteRunner, proto.RemoteSyncMetricQuarantine, "terminal-quarantine:"+delivery.DeliveryID)
					}
				}
				return ack
			}
			remoteRunner.OnInboundFinalizeV1 = func(params proto.RemoteInboundFinalizeV1Params) proto.RemoteInboundFinalizeV1Result {
				if !remoteRunner.SignedInboundFinalizeReady() {
					return proto.RemoteInboundFinalizeV1Result{ReasonCode: "capability-unavailable"}
				}
				if params.Evidence.BatchEventCount != 0 && !remoteRunner.SignedRedactionSafeBatchReady() {
					return proto.RemoteInboundFinalizeV1Result{ReasonCode: "capability-unavailable"}
				}
				return handleDurableInboundFinalize(
					&durableInboundPhaseGate,
					inboundInbox,
					durableInboundCursors,
					remoteRunner.CurrentDeviceID(),
					remoteRunner.SyncNegotiation(),
					params,
					orch.FinalizeInboundCanonicalEvidence,
				)
			}
		}

		// Wire the remote runner's notification callbacks
		// now that the orchestrator exists. remote.rules_update carries a
		// cloud-authored selective-sync ruleset; we rebuild a
		// syncrules.Engine from the pushed []syncrules.Rule (validated +
		// compiled via syncrules.New — the same path the local rules.toml
		// hot-reload uses) and swap it into the live orchestrator so
		// portal-edited routing applies without a daemon restart.
		//
		// Safe-by-default: a ruleset that fails validation is NOT applied
		// (the existing engine stays live) and the error is logged. An
		// EMPTY rules slice produces an empty engine — which denies all
		// cross-agent fan-out — matching the local safe-by-default
		// contract (a malformed/empty cloud push must never silently leave
		// stale routing OR widen fan-out).
		// cloudRuleStore retains the latest cloud-pushed ruleset so it can be
		// (a) merged with the user's rules.toml into the live engine and
		// (b) surfaced read-only in the local portal. Shared between the
		// OnRulesUpdate callback (writer) and the web API rules accessor
		// (reader, via webAPIDeps below).
		cloudRules := newCloudRuleStore()
		if remoteRunner != nil {
			remoteRunner.OnRulesUpdate = func(changeID string, rules []syncrules.Rule) {
				// Reject an invalid cloud ruleset outright (keep current engine).
				if _, err := syncrules.New(rules); err != nil {
					lg.Warn("remote: rejected cloud rules_update (validation failed; keeping current ruleset)",
						"change_id", changeID, "rule_count", len(rules), "err", err)
					return
				}
				cloudRules.set(rules)
				// Apply cloud rules MERGED over the user's local rules.toml so a
				// cloud push never clobbers local rules (and vice versa). A
				// malformed local file contributes no user rules — cloud rules
				// still apply (safe-by-default).
				user, uerr := loadUserRulesQuiet(userRulesPath())
				eng, nerr := syncrules.New(mergeRules(user.Sync.Rules, rules))
				if nerr != nil {
					lg.Warn("remote: rejected merged ruleset (validation failed; keeping current)",
						"change_id", changeID, "err", nerr)
					return
				}
				orch.SetRulesEngine(eng)
				if uerr != nil {
					lg.Warn("remote: applied cloud rules over a malformed local rules.toml (local rules ignored)",
						"err", uerr)
				}
				lg.Info("remote: applied cloud rules_update",
					"change_id", changeID, "rule_count", len(rules))
			}
		}

		// Client-side namespace key rotation. On a
		// namespace.key_rotated signal a SURVIVING daemon generates + wraps a
		// fresh content key for the surviving member devices, persists its own
		// plaintext copy, writes the wrapped blobs back to the namespace_keys
		// row, and broadcasts them; on a key broadcast it installs the blob
		// wrapped for its own device key. All key material work is local — the
		// control plane only bumps the key_version counter (zero-knowledge).
		//
		// Identity is resolved lazily from the runner's DeviceID (populated at
		// pairing), so rotation activates once this device is paired and
		// safely no-ops before then (an unknown device id is never a surviving
		// member).
		if remoteRunner != nil {
			// TEAM-ROTATION remains a hard closed gate. The legacy unsigned
			// first-writer-wins CAS protocol is intentionally not wired into a
			// production daemon; only the signed statement/finalized-manifest v2
			// path may install a namespace key.
			lg.Info("remote: team namespace rotation gate closed pending signed-manifest cutover")
		}

		// Client-side RBAC (defense-in-depth; the server stays
		// authoritative). Resolve + cache the caller's per-namespace role over
		// the same remote plugin so the local UI can reflect what the user may
		// do (GET /api/rbac/namespace/{id}) and client-side gates can refuse
		// unpermitted team operations before a round-trip. Identity is the
		// runner's DeviceID (populated at pairing): unpaired => deny-safe
		// no-access. A membership_changed enumerate-hint drops the cache so a
		// role change reaches this device within the 60-second window.
		var roleService *daemon.RoleService
		if remoteRunner != nil {
			roleService = daemon.NewRoleService(
				cmd.Context(),
				remoteRunner,
				func() rbac.Identity {
					return rbac.Identity{DeviceID: remoteRunner.CurrentDeviceID()}
				},
				lg,
			)
			// Chain the cache-invalidation onto the existing enumerate-hint
			// callback so wiring a membership change here does not clobber any
			// hint handler set elsewhere.
			prevHint := remoteRunner.OnEnumerateHint
			remoteRunner.OnEnumerateHint = func(reason string) {
				if reason == "membership_changed" {
					roleService.InvalidateAll()
					// A roster change alters the end-to-end encryption recipient set, so drop
					// the resolver's device-list cache too (a removed device
					// must stop receiving; a new one must start).
					if recipientResolver != nil {
						recipientResolver.InvalidateAll()
					}
					go func() {
						time.Sleep(remoteMembershipRepublishDelay)
						n, err := orch.RepublishLocalRemoteHeads(cmd.Context())
						if err != nil {
							lg.Warn("remote: republish local heads after membership change failed", "err", err)
							return
						}
						lg.Info("remote: republished local heads after membership change", "count", n)
					}()
				}
				if prevHint != nil {
					prevHint(reason)
				}
			}
			// Client-side RBAC: install the desync-safe write gate on
			// the orchestrator now that BOTH exist (the orchestrator is built
			// before the role service). The orchestrator consults this only at
			// its namespace-commit chokepoint and only ever blocks on a
			// DEFINITIVE deny; an OSS / un-paired daemon (remoteRunner == nil)
			// leaves it nil, so behavior is unchanged. The server stays
			// authoritative.
			orch.SetWriteAuthorizer(roleService)
		}

		// v0.91.0 FR-03.14: Prometheus metrics endpoint.
		// Disabled by default; operators opt in via the config keys
		// metrics.enabled = true + metrics.listen = "host:port".
		// FR-10.4 mandates loopback-only by default; we honor that
		// via defaults.toml.
		metricsReg := metrics.NewRegistry(time.Now().UTC())
		// NFR-10 §5.2: seed every mandated metric family so the /metrics
		// scrape always exposes the full set with correct TYPE/HELP, even
		// before any event has touched a counter. Then wire the orchestrator's
		// sync_latency_seconds observer. Observation is unconditional (an
		// in-memory histogram bump); the series is only EXPOSED over HTTP when
		// the endpoint is enabled below — matching how metricsReg already
		// exists regardless of metrics.enabled.
		seedMandatedMetricFamilies(metricsReg)
		orch.SetSyncLatencyObserver(&syncLatencyMetric{reg: metricsReg})
		if effEnabled, _, ok := readDaemonConfigKey(cmd, "metrics.enabled"); ok && effEnabled == "true" {
			listenAddr := "127.0.0.1:9090"
			if v, _, ok := readDaemonConfigKey(cmd, "metrics.listen"); ok && v != "" {
				listenAddr = v
			}
			mux := http.NewServeMux()
			// Wrap the registry handler so the gauges that reflect live daemon
			// state (NFR-10 §5.2 queue_depth) are refreshed at scrape time from
			// the orchestrator before the registry renders. Cheap (a debouncer
			// pending-count read); runs only on an actual scrape.
			base := metricsReg.Handler()
			mux.HandleFunc("/metrics", func(w http.ResponseWriter, req *http.Request) {
				metricsReg.SetGauge("queue_depth", int64(orch.PendingImports()), nil)
				base.ServeHTTP(w, req)
			})
			srv := &http.Server{
				Addr:              listenAddr,
				Handler:           mux,
				ReadHeaderTimeout: 5 * time.Second,
			}
			go func() {
				lg.Info("metrics endpoint listening", "addr", listenAddr)
				if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
					lg.Error("metrics endpoint exited", "err", err)
				}
			}()
			defer func() {
				shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				_ = srv.Shutdown(shutdownCtx)
			}()
		}

		// Control server. v0.39.0 wires the orchestrator as the
		// Activity provider so the control-socket "status" response
		// can overlay LastActivity (which the tray indicator reads to
		// drive its Active/Paused state machine without proxying via
		// tick-arrival liveness).
		sockPath := filepath.Join(daemonStateDir, "aplexicad.sock")
		ctlSrv = daemon.NewControlServer(sockPath, &daemon.StatusInfo{
			PID:        os.Getpid(),
			StartedAt:  time.Now().UTC(),
			WatchedDir: daemonDir,
			Version:    version.Version,
			// The cloud identity seeded from the plugin above (empty when
			// unpaired/disabled). CLI commands that author store events read
			// it back so their provenance matches the daemon's and the
			// outbound sweep can publish their heads.
			LocalDeviceID: localDeviceID,
		}, orch)
		// v0.75.0 FR-10.8: cross-platform hot-reload trigger via the
		// control socket. The Reloader callback re-runs the TOML
		// config layer apply and returns a short report. SIGHUP (Unix
		// only) ALSO invokes the same logic via its existing handler
		// (see runReloadFromSighup).
		ctlSrv.SetReloader(func() (any, error) {
			report, err := reloadDaemonConfigPackage(cmd)
			if err != nil {
				return nil, err
			}
			lg.Info("config reload via control socket", "report", report)
			return report, nil
		})
		// `aplexica backfill`: forced LOCAL conversation backfill. Scope is
		// validated against the reserved sync.cloudBackfill gate (read at
		// startup); the local pass materializes into this device's agents only
		// and never publishes to the relay. Apply plans synchronously, then
		// runs on an orchestrator background goroutine (joined by Close) so
		// the control response is not bounded by a 3,000-conversation write.
		ctlSrv.SetBackfillRunner(func(agents []string, depth int, scope string, dryRun bool) (any, error) {
			if err := daemon.ValidateBackfillScope(scope, fileCfg.Sync.CloudBackfill); err != nil {
				return nil, err
			}
			if dryRun {
				return orch.ForcedConversationBackfillPlan(agents, depth)
			}
			plan, err := orch.StartForcedConversationBackfill(agents, depth)
			if err != nil {
				return nil, err
			}
			lg.Info("forced local conversation backfill started",
				"conversations", plan.Conversations, "depth", depth, "targets", plan.Targets)
			return map[string]any{"started": true, "plan": plan}, nil
		})
		// FR-03.21: surface store disk pressure on `aplexica status`. The
		// provider reads the cached PressureState the disk-pressure goroutine
		// refreshes each tick. Always wired — when the cap is disabled
		// (StoreMaxGB=0) the cache reports MaxBytes=0 and the status renderer
		// prints nothing for store pressure.
		ctlSrv.SetPressureProvider(pressureCache.snapshot)
		if remoteRunner != nil || remotePublisher != nil {
			ctlSrv.SetSyncEvidenceProvider(func(ctx context.Context) daemon.SyncEvidenceStatus {
				status := daemon.SyncEvidenceStatus{}
				if remotePublisher != nil {
					status.Outbox = remotePublisher.OutboxEvidenceStatus(time.Now().UTC())
				}
				if remoteRunner != nil {
					remoteStatus, err := remoteRunner.Status(ctx)
					if err == nil {
						status.RemoteAvailable = true
						status.Remote = &remoteStatus
					}
				}
				return status
			})
		}
		ctlSrv.SetProjectRemover(func(id string) error {
			return revokeProject(projectReg, orch, remotePublisher, auditRecorder, id)
		})
		ctlSrv.SetNativeRestorer(func(ctx context.Context, backupID, agent string) (any, error) {
			dir, err := resolveBackupDir(backupMgr.backupsRoot, backupID)
			if err != nil {
				return nil, err
			}
			return backupMgr.Restore(ctx, dir, agent)
		})
		if generationActivationDriver != nil {
			ctlSrv.SetGenerationActivationRequester(func() {
				select {
				case generationActivationTrigger <- struct{}{}:
				default:
				}
			})
		}
		if deviceTransitionService != nil {
			ctlSrv.SetDeviceTransitionSubmitter(deviceTransitionService.SubmitPlan)
		}
		if err := ctlSrv.Start(); err != nil {
			lg.Error("control server bind failed", "err", err)
			return err
		}

		// v0.107.0: optional local web UI listener. The HTTP server
		// binds 127.0.0.1+::1 only and serves the embedded SPA + REST
		// and SSE surface for the local web UI.
		// The daemon owns the server's lifecycle; ctx cancellation
		// from EITHER the control-socket stop path OR the parent
		// signal handler shuts the listener down via
		// http.Server.Shutdown.
		//
		// Skipped silently when cfg.Web.Enabled == false (tri-state
		// pointer; nil falls back to WebEnabledDefault() = true).
		// A construction error logs WARN and proceeds without the
		// web UI — the daemon's CLI surface must keep working even
		// if the listener fails (e.g. port conflict on a non-zero
		// configured Port).
		//
		// The web handlers below are constructed BEFORE
		// runBody owns the daemon's long-lived run context. serveCtx
		// holds that context once runBody sets it, so the ProjectsHandler
		// onRegister callback can start its folder watcher on the
		// long-lived context instead of the per-request HTTP context.
		serveCtx := &serveCtxHolder{}
		// Adapters that can report the dirs they've run in are
		// harvested on demand by pendingFn so GET /api/pending surfaces
		// discovered-but-unregistered folders alongside artifact-pending
		// projects.
		harvestSources := harvestSourcesFrom(runtimeAdapters)
		// Shared discovered-folders cache. Populated by an initial harvest
		// at daemon startup and refreshed on the reharvestInterval timer (both
		// in runBody, where the long-lived run context exists). pendingFn reads
		// this snapshot so the startup/background result and the on-demand path
		// agree — and so discovery works with the local web UI disabled. The
		// cache only ever holds CANDIDATES; the refresh path calls
		// projectReg.RefreshAgents (a no-op for unregistered paths), never
		// auto-watching, auto-registering, or auto-importing a folder.
		discoveryCache := &projectdiscovery.Cache{}
		var webSrv *web.Server
		var backupAccessor *nativeBackupsWebAccessor
		if daemon.WebEnabled(fileCfg) {
			webOpts := web.Options{
				Bind:        daemon.WebBind(fileCfg),
				Port:        fileCfg.Web.Port,
				PortInfoDir: daemonStateDir,
				Version:     version.Version,
				// Persist the local session table (0700 state-dir) with a
				// long TTL so a daemon restart no longer forces a fresh
				// tray bootstrap — the browser cookie keeps working after a
				// page refresh. FR: local UI session survives restart.
				SessionTTL: auth.DefaultSessionTTL,
			}
			if s, werr := web.NewServer(webOpts); werr != nil {
				lg.Warn("web: construct server failed; local web UI disabled this run", "err", werr)
			} else {
				webSrv = s
				// Wire the UDS control-server callbacks so the CLI
				// subcommands can mint bootstrap URLs and revoke
				// sessions via the existing socket. Both callbacks
				// surface "listener not bound yet" until Start
				// completes its bind, via web.Server's own checks.
				ctlSrv.SetWebTokenIssuer(func() (string, error) {
					return webSrv.IssueTokenURL()
				})
				ctlSrv.SetWebBootstrapFileIssuer(func() (string, error) { return webSrv.IssueBootstrapFile() })
				ctlSrv.SetWebSessionRevoker(func() int {
					return webSrv.Sessions().RevokeAll()
				})

				// Mount the protected REST endpoints behind RequireSession + RequireCSRF.
				// Accessors are constructed once here so the
				// handlers share a single view of the daemon's
				// runtime (orchestrator, store, conflicts, pending,
				// project registry, pause state).
				deps := &webAPIDeps{
					store:       store,
					adapters:    filtered,
					discoveries: discoveries,
					conf:        confStore,
					pauseStore:  pauseStore,
					projectReg:  projectReg,
					orch:        orch,
					secretsRoot: daemonSecretsRoot,
					pendingFn: func() ([]pending.Project, error) {
						// Union artifact-pending projects
						// with folders discovered from agent session metadata.
						// The discovered set comes from the SHARED cache that
						// the startup harvest and the reharvestInterval timer
						// keep current (so the on-demand and background views
						// agree, and discovery works even with the web UI
						// disabled). Fast path: read the cached snapshot. Fall
						// back to an on-demand HarvestAll only when the cache is
						// still empty (e.g. a GET that races the startup harvest)
						// so the endpoint never serves a stale-empty list.
						// A harvest error degrades gracefully to the
						// artifact-only list rather than failing the endpoint.
						// HarvestAll (not Harvest) so AgentSuggestions can see
						// already-registered folders; ListWithDiscovered still
						// excludes registered projects from the pending list itself.
						discovered := discoveryCache.Snapshot()
						var herr error
						if discovered == nil {
							discovered, herr = projectdiscovery.HarvestAll(harvestSources, daemonStateDir, agentNativeRoots...)
						}
						var list []pending.Project
						var lerr error
						if herr != nil {
							list, lerr = pending.List(store, projectReg)
						} else {
							list, lerr = pending.ListWithDiscovered(store, projectReg, discovered)
						}
						if lerr != nil {
							return nil, lerr
						}
						// Flag/append the folders the user dismissed so the SPA
						// can split them into the denied list.
						list = pending.ApplyDenied(list, deniedStore.List())
						// Append "add agent X to registered project P" suggestions
						// when discovery saw an agent active in a project it isn't
						// part of yet (only when the harvest succeeded).
						if herr == nil {
							list = append(list, pending.AgentSuggestions(projectReg, discovered, suggDismissed)...)
						}
						return list, nil
					},
					rulesPath:     userRulesPath(),
					configPath:    configPath,
					stateDir:      daemonStateDir,
					backupsRoot:   nativeBackupsRoot(),
					backupMgr:     backupMgr,
					backupBlocker: backupBlocker,
					startedAt:     time.Now().UTC(),
					pid:           os.Getpid(),
					watchedDir:    daemonDir,
					version:       version.Version,
					remoteRunner:  remoteRunner,
					ctl:           ctlSrv,
					roleService:   roleService,
					remoteCfg:     fileCfg.Remote,
					cloudRules:    cloudRules,
					logger:        lg,
					daemonCtx:     serveCtx.get,
				}
				webSrv.UseProtected(apiweb.NewDaemonHandler(&daemonWebAccessor{deps: deps}))
				webSrv.UseProtected(apiweb.NewAgentsHandler(&agentsWebAccessor{deps: deps}))
				webSrv.UseProtected(apiweb.NewEventsHandler(&eventsWebAccessor{deps: deps}))
				webSrv.UseProtected(apiweb.NewRulesHandler(&rulesWebAccessor{deps: deps}))
				webSrv.UseProtected(apiweb.NewSyncHandler(&syncWebAccessor{deps: deps}))
				backupAccessor = &nativeBackupsWebAccessor{deps: deps, jobs: newNativeBackupJobManager()}
				webSrv.UseProtected(apiweb.NewNativeBackupsHandler(backupAccessor))
				webSrv.UseProtected(apiweb.NewConflictsHandler(&conflictsWebAccessor{deps: deps}))
				webSrv.UseProtected(apiweb.NewConversationsHandler(&conversationsWebAccessor{deps: deps}))
				webSrv.UseProtected(apiweb.NewConversationBranchesHandler(&conversationBranchesWebAccessor{deps: deps}))
				webSrv.UseProtected(apiweb.NewPendingHandler(&pendingWebAccessor{deps: deps}))
				// Project list / manual add / approve-with-
				// scope. onRegister fires AFTER the registry upsert: it
				// live-watches the newly-registered folder (so future edits
				// import) and backfills its parked artifacts (RefanOutByProject
				// materializes anything that was staged-and-waiting on this
				// project). The watcher is started on the daemon's long-lived
				// run context (serveCtx.get(), set by runBody) — NOT the HTTP
				// request context, which is cancelled when the request returns
				// and would otherwise kill the brand-new watcher immediately.
				webSrv.UseProtected(apiweb.NewProjectsHandlerWithDeny(projectReg, deniedStore, func(projectID, path string) error {
					ctx := serveCtx.get()
					if ctx == nil {
						// Defensive: the listener only starts inside runBody,
						// which sets serveCtx before binding — so by the time a
						// request reaches here ctx is always populated. Fall
						// back to a non-cancellable context rather than panic if
						// that invariant ever changes.
						ctx = context.Background()
					}
					if err := orch.WatchFolder(ctx, path); err != nil {
						return err
					}
					_, _ = orch.RefanOutByProject(projectID)
					return nil
				}, func(_ /*projectID*/, path string) error {
					// onUnregister: stop watching the removed folder so it goes
					// idle and the next discovery pass re-surfaces it as pending.
					return orch.UnwatchFolder(path)
				}).WithSuggestionsDismissed(suggDismissed).
					WithProjectMemory(func(projectID string) ([]apiweb.ProjectMemoryFile, error) {
						// Effective project memory per agent file: the composed
						// content the daemon syncs (Claude's CLAUDE.md base + folded
						// auto-memories, Codex's AGENTS.md, ...), so the user can
						// confirm cross-agent parity at a glance.
						arts, lerr := store.ListArtifacts(acf.KindMemory)
						if lerr != nil {
							return nil, lerr
						}
						memDec := func(e acf.Event) (string, error) {
							p, derr := acf.DecodeMemoryPayload(e)
							return p.Content, derr
						}
						var out []apiweb.ProjectMemoryFile
						for _, a := range arts {
							if a.Scope != acf.ScopeProject || a.Project == nil || a.Project.ID != projectID {
								continue
							}
							content, tomb, cerr := adapter.ReplayOpaqueContent(store, acf.KindMemory, a.ArtifactID, memDec)
							if cerr != nil || tomb {
								continue
							}
							out = append(out, apiweb.ProjectMemoryFile{
								Name:         a.Name,
								SourcePath:   a.SourcePath,
								Content:      content,
								SyncedAgents: a.SyncedAgents,
							})
						}
						sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
						return out, nil
					}))
				webSrv.UseProtected(apiweb.NewConfigHandler(&configWebAccessor{deps: deps}))
				webSrv.UseProtected(apiweb.NewTransportHandler(transportWebAccessor{}))
				webSrv.UseProtected(apiweb.NewOnboardingHandler(&onboardingWebAccessor{deps: deps}))
				webSrv.UseProtected(apiweb.NewRemoteHandler(&remoteWebAccessor{deps: deps}))
				webSrv.UseProtected(apiweb.NewRBACHandler(&rbacWebAccessor{deps: deps}))

				// v0.107.0 W5: SSE event stream. The bus is a new
				// per-daemon-process pub/sub; publishers (orchestrator,
				// conflict detector, rule engine) call eventBus.PublishKind
				// to broadcast. Subscribers connect to GET /api/events/stream
				// via the SSE handler registered here.
				webSrv.UseProtected(sse.New(eventBus))
			}
		}

		// v0.32.0: wrap the ctx-driven body in a closure so it can be
		// called from EITHER the foreground signal.NotifyContext path
		// OR daemon.RunAsService (Windows SCM). All resource setup
		// above (lock, logger, store, secrets, adapters, orchestrator,
		// control server) stays in the outer RunE scope so its defers
		// — lk.Release(), logCloser.Close(), orch.Close() — remain
		// attached to the function's lifetime and fire on every exit
		// path. The closure only owns the goroutines + the SIGHUP
		// handler whose lifetimes are bounded by ctx.
		runBody := func(parentCtx context.Context) error {
			// Derive a cancelable child ctx so runBody can shut
			// orch.Run down when the control socket receives a stop
			// request, independent of how the caller scoped parentCtx
			// (signal-aware on the foreground path; SCM-cancel-aware
			// under RunAsService). The derived cancel also propagates
			// caller cancellation through to every goroutine spawned
			// below.
			ctx, cancel := context.WithCancel(parentCtx)
			defer cancel()
			// Publish the long-lived run context to the
			// holder the ProjectsHandler onRegister closure reads. Done
			// BEFORE the web listener starts below so any onRegister
			// request observes a live (non-request-scoped) context for
			// its WatchFolder call.
			serveCtx.set(ctx)

			// Roll the daemon log at local midnight for the
			// life of the run context. This is the only rotation trigger
			// on Windows (no SIGHUP) and complements the SIGHUP-driven
			// Rotate on Unix. The goroutine stops on ctx cancellation.
			lg.StartMidnightRotation(ctx)

			go func() {
				<-ctlSrv.Done()
				lg.Info("control-server stop request received")
				cancel()
			}()

			// Run the project-discovery harvest at startup and then on a
			// debounced timer for the rest of the daemon's life. This makes
			// discovery independent of the web UI (the old behavior harvested
			// ONLY inside the GET /api/pending handler, so a freshly-active
			// folder surfaced only when someone opened the pending page, and
			// the agents set of registered folders never got refreshed).
			// Cache.Run does an initial Reharvest immediately (populating the
			// shared snapshot pendingFn reads), then re-harvests every
			// reharvestInterval, refreshing already-registered folders' agents
			// sets via projectReg.RefreshAgents. It is bounded by ctx and
			// stops on shutdown. It NEVER auto-watches, auto-registers, or
			// auto-imports a discovered folder — harvesting only surfaces
			// candidates into the pending list (approval-gate invariant).
			go discoveryCache.Run(ctx, harvestSources, projectReg, daemonStateDir, reharvestInterval, agentNativeRoots...)

			// v0.107.0: start the local web UI listener if one was
			// constructed above. Its lifetime is bounded by ctx —
			// either the control-socket stop path or the parent
			// signal handler shuts it down via http.Server.Shutdown.
			// We don't add its goroutine to the resource WaitGroup
			// because http.Server.Shutdown is synchronous from the
			// Server.Start side; the next iteration of runBody won't
			// race because the daemon process exits at this point.
			// Remote-transport plugin supervisor. The
			// runner spawns the plugin in its own goroutine, restarts
			// on crash, and tears down on ctx cancellation. We don't
			// block daemon startup on the plugin's initialize success —
			// if the plugin is broken, the daemon still serves the
			// local CLI surface and the runner will log Warn-level
			// errors.
			if remoteRunner != nil {
				remoteRunner.Start(ctx)
				if deviceTransitionService != nil {
					go deviceTransitionService.Run(ctx)
				}
				if generationActivationDriver != nil {
					go generationActivationDriver.Run(ctx)
				}
				if rosterRenewalDriver != nil {
					go rosterRenewalDriver.Run(ctx)
				}
				lg.Info("remote: plugin runner started",
					"executable", fileCfg.Remote.Executable,
					"sync_mode", daemon.RemoteSyncMode(fileCfg))

				// Sync driver loop: (1) register this device's wrap public key
				// with the account (remote.register_wrap_key) so it becomes a
				// valid E2E encryption recipient without a re-pair, (2) subscribe
				// to the namespaces this device can see so the plugin admits
				// inbound events, and (3) BACKFILL via remote.enumerate/
				// remote.fetch — pull events committed while this device was
				// offline/unsubscribed or while the relay/plugin was down, which
				// the live remote.inbound push path can't recover. Re-runs on a
				// slow cadence so a reconnect re-registers + re-subscribes +
				// re-reconciles without a daemon restart. Best-effort: a
				// reconnecting/unpaired plugin just retries on the next tick.
				// Bounded by ctx. (Backfill is end-to-end functional only once
				// the cloud relay implements enumerate/fetch; against the in-repo
				// default handler it is a correct no-op.)
				go runRemoteSyncDriver(ctx, remoteRunner, deviceKeyProv.Public, orch.ImportInboundResults, nil, lg, sseBusPublisher{bus: eventBus})
				go runRemoteChangedHeadRepublisher(ctx, remoteRunner.ConnState, orch.RepublishLocalRemoteHeads, lg)
				// Slow retained-baseline backfill trickle: artifacts older
				// than the changed-head sweep's recent window never get a
				// retained baseline otherwise (a peer stays diverged on them
				// until their next head change).
				go runRemoteRetainedBackfillTicker(ctx, remoteRunner.ConnState, orch.BackfillLocalRemoteHeads, lg)
			}

			if webSrv != nil {
				go func() {
					if err := webSrv.Start(ctx); err != nil {
						lg.Warn("web: server exited unexpectedly", "err", err)
					}
				}()
				lg.Info("web: local UI listener starting",
					"bind", daemon.WebBind(fileCfg),
					"configured_port", fileCfg.Web.Port,
					"portinfo", filepath.Join(daemonStateDir, "portinfo.json"))
			}

			go func() {
				ticker := time.NewTicker(1 * time.Minute)
				defer ticker.Stop()
				for {
					select {
					case <-ctx.Done():
						return
					case <-ticker.C:
						if backupAccessor != nil {
							backupMgr.RunScheduledIfDue(lg, func(kind string, agents []string, destination string) (nativebackup.BackupInfo, error) {
								return backupAccessor.CreateKind(ctx, kind, agents, destination)
							})
						} else {
							backupMgr.RunScheduledIfDue(lg, nil)
						}
					}
				}
			}()

			// Optional: Hermes auto-watch. Spawn a poll-based watcher
			// that re-exports Hermes sessions from ~/.hermes/state.db
			// into the canonical store. Skips cleanly (with an INFO
			// log) if state.db does not exist or the flag is disabled.
			// The *slog.Logger returned by daemon.NewLogger satisfies
			// hermeswatch.Logger natively (Info / Error with variadic
			// args).
			//
			// hw is captured for the SIGHUP handler below so live
			// hermeswatch-interval reloads can call hw.SetInterval.
			// nil when disabled or state.db is missing — the handler
			// logs a "skipped" notice in that case.
			var hwWG sync.WaitGroup
			var hw *hermeswatch.Watcher
			if daemonHermesWatch {
				// Validate the schema, not just existence: a stale 0-byte (or
				// otherwise non-Hermes) state.db passes an os.Stat check but
				// then fails every 5s poll forever. Skip the loop entirely
				// unless the file is a real Hermes DB.
				if err := hermesdb.Validate(daemonHermesDBPath); err == nil {
					hw = &hermeswatch.Watcher{
						Adapter:   h,
						Store:     store,
						DBPath:    daemonHermesDBPath,
						Interval:  daemonHermesWatchInterval,
						Direction: hermeswatch.DirectionBoth,
						StateFile: filepath.Join(daemonStateDir, "hermeswatch.state.json"),
						Logger:    lg,
						// Propagate a turn the user adds by resuming a materialized
						// conversation in Hermes to the other agents + the relay. The
						// hermeswatch import path imports straight to the store and
						// bypasses handleEvent's fan-out/forward tail, so without this
						// the turn would only cross on the next restart's catch-up.
						OnImported: func(c context.Context, ids []string) {
							orch.IngestExternalConversations(c, h, ids)
						},
					}
					hwWG.Add(1)
					go func() {
						defer hwWG.Done()
						// hermeswatch imports/exports state.db directly instead of
						// flowing through the orchestrator's AdapterBlocker. Hold its
						// first tick until the same startup proof succeeds (or the user
						// durably replaces/overrides a failed backup).
						if !waitForNativeStartupSafety(ctx, startupSafetyDone, backupBlocker, "hermes") {
							return
						}
						if err := hw.Run(ctx); err != nil {
							lg.Error("hermeswatch terminated", "err", err)
						}
					}()
					lg.Info("hermeswatch enabled", "db", daemonHermesDBPath, "interval", daemonHermesWatchInterval)
				} else {
					lg.Info("hermeswatch skipped", "reason", "not a valid Hermes state.db", "path", daemonHermesDBPath, "err", err)
				}
			}

			// Time-based snapshotter (v0.29.2; BRD-03 §4.8.1).
			// Background goroutine that walks the store on a fixed
			// tick (1h) and snapshots any artifact whose latest event
			// is older than its kind's max-age AND is not itself a
			// snapshot. Per-kind thresholds are captured by reference
			// here at startup — the time-based fields are restart-
			// required (the live setter + SIGHUP wiring only cover
			// the event-count cadence map).
			//
			// Skips entirely when every kind's max-age is 0; otherwise
			// starts even if some kinds are disabled (the tick
			// function silently skips those).
			maxAge := map[acf.Kind]time.Duration{
				acf.KindConversation: daemonSnapMaxAgeConv,
				acf.KindMemory:       daemonSnapMaxAgeMem,
				acf.KindSkill:        daemonSnapMaxAgeSkill,
				acf.KindTool:         daemonSnapMaxAgeTool,
			}
			anyMaxAge := daemonSnapMaxAgeConv > 0 || daemonSnapMaxAgeMem > 0 ||
				daemonSnapMaxAgeSkill > 0 || daemonSnapMaxAgeTool > 0
			// v0.34.0: hand the goroutine a retention.Runner so the
			// SIGHUP handler can hot-reload SnapshotMaxAge* via
			// snapRunner.SetMaxAge — previously these were restart-
			// required because the goroutine captured the map by
			// reference at startup. The Runner is constructed
			// unconditionally so the SIGHUP handler has a non-nil
			// reference even when the snapshotter is disabled at boot;
			// it just isn't .Run()-started in that case.
			snapRunner := retention.NewRunner(store, maxAge)
			var snapWG sync.WaitGroup
			if anyMaxAge {
				snapWG.Add(1)
				go func() {
					defer snapWG.Done()
					if err := snapRunner.Run(ctx, 1*time.Hour); err != nil {
						lg.Error("time-based snapshotter terminated", "err", err)
					}
				}()
				lg.Info("time-based snapshotter started",
					"tick", (1 * time.Hour).String(),
					"max_age_conv", daemonSnapMaxAgeConv,
					"max_age_mem", daemonSnapMaxAgeMem,
					"max_age_skill", daemonSnapMaxAgeSkill,
					"max_age_tool", daemonSnapMaxAgeTool)
			} else {
				lg.Info("time-based snapshotter disabled — all per-kind max-ages are 0")
			}

			// Disk-pressure retention sweep (FR-03.20; BRD-03 §4.8.2).
			// Periodic goroutine that walks the canonical store every
			// daemonPressureCheckInterval (5m); when total size crosses the
			// configured watermark it runs ONE ordered retention sweep
			// (retention.RunPressureSweep) instead of only force-snapshotting:
			// the attachments_only OSS default evicts old attachment bytes and
			// GCs them first (lossless for text + chain), then snapshots, then
			// — only if still over — compacts history. Each phase re-checks the
			// watermark and the sweep returns early the moment pressure is
			// relieved.
			//
			// The retention.Config and the effective watermark are resolved
			// ONCE at goroutine start from the layered config with
			// the daemon flags overlaid (loadRetentionConfig): an explicit
			// --store-high-watermark-gb wins, else StoreMaxGB*StoreHighWatermark,
			// else the legacy absolute value. A 0 watermark disables the path.
			// SIGHUP hot-reload of the watermark stays out of scope (restart-
			// required like StoreRoot).
			var pressureWG sync.WaitGroup
			var retCfg retention.Config
			var watermarkBytes int64
			if retEff, eerr := daemonRetentionEffective(); eerr != nil {
				lg.Error("retention config load failed — disk-pressure sweep disabled", "err", eerr)
			} else if cfg, wm, lerr := loadRetentionConfig(retEff, retentionFlagOverrides{
				HighWatermarkGB:        daemonStoreHighWatermarkGB,
				HighWatermarkGBChanged: cmd.Flags().Changed("store-high-watermark-gb"),
			}); lerr != nil {
				lg.Error("retention config resolve failed — disk-pressure sweep disabled", "err", lerr)
			} else {
				retCfg = cfg
				watermarkBytes = wm
			}
			// The disk-pressure goroutine runs whenever either the sweep
			// watermark is active OR the emergency-quota gate is wired
			// (store.IngestGate != nil — i.e. StoreMaxGB>0). The gate needs a
			// freshly-measured cached size every tick even when the sweep
			// watermark resolves to 0, otherwise the cached size stays 0 and
			// the gate never fires.
			if watermarkBytes > 0 || store.IngestGate != nil {
				pressureBlobs := &blobstore.Store{Root: store.BlobsDir()}
				pressureWG.Add(1)
				go func() {
					defer pressureWG.Done()
					// Seed the cache once up front so the emergency-quota gate
					// has a real measurement before the first tick fires
					// (otherwise it reads 0 for up to one full interval).
					if size, _, err := retention.CheckPressure(daemonStoreRoot, watermarkBytes); err == nil {
						pressureCache.update(size)
					}
					t := time.NewTicker(daemonPressureCheckInterval)
					defer t.Stop()
					for {
						select {
						case <-ctx.Done():
							return
						case <-t.C:
							size, over, err := retention.CheckPressure(daemonStoreRoot, watermarkBytes)
							if err != nil {
								lg.Error("pressure check failed", "err", err)
								continue
							}
							// Refresh the cached PressureState every tick (reuse
							// this measurement; the gate + status RPC read it).
							pressureCache.update(size)
							if !over {
								continue
							}
							lg.Warn("store exceeds high watermark — running retention sweep",
								"bytes", size.Bytes,
								"watermark_bytes", watermarkBytes,
								"attachments_only", retCfg.AttachmentsOnly)
							// overWatermark re-measures the store on each phase
							// boundary so the sweep can early-exit once pressure
							// is relieved (best-effort: a measurement error is
							// treated as "still over" so the sweep proceeds).
							overWatermark := func() bool {
								_, stillOver, cerr := retention.CheckPressure(daemonStoreRoot, watermarkBytes)
								if cerr != nil {
									lg.Error("pressure re-check failed during sweep", "err", cerr)
									return true
								}
								return stillOver
							}
							rep, serr := retention.RunPressureSweep(ctx, store, pressureBlobs, retCfg, overWatermark)
							if serr != nil {
								lg.Error("retention sweep partial failure", "err", serr,
									"actions", len(rep.Actions), "bytes_reclaimed", rep.BytesSaved)
								continue
							}
							lg.Info("retention sweep complete under pressure",
								"actions", len(rep.Actions), "bytes_reclaimed", rep.BytesSaved)
						}
					}
				}()
				lg.Info("disk-pressure retention sweep enabled",
					"watermark_bytes", watermarkBytes,
					"attachments_only", retCfg.AttachmentsOnly,
					"interval", daemonPressureCheckInterval.String())
			} else {
				lg.Info("disk-pressure retention sweep disabled (watermark resolves to 0)")
			}

			// v0.62.0 BRD-02 §4.13.4 periodic project-detection scan.
			// Ticks every daemonProjectScanInterval (default 60m;
			// 0 disables). Walks daemonProjectScanRoots looking for
			// VCS markers, cross-references detections against
			// pending.List, and for each match auto-links the
			// project (registry.Add) + triggers refanout. Conservative:
			// random repos the user has cloned but never sent
			// artifacts for are NOT auto-added to the registry — only
			// detections that match a pending project (artifacts
			// already waiting in the canonical store) trigger
			// auto-link.
			var projectScanWG sync.WaitGroup
			if daemonProjectScanInterval > 0 {
				roots := daemonProjectScanRoots
				if len(roots) == 0 {
					roots = pending.DefaultScanRoots()
				}
				maxDepth := daemonProjectScanMaxDepth
				// FR-03.8/FR-03.11: piggyback the periodic expired-pause
				// cleanup on this same tick. pausestate.IsPaused already
				// treats Until<now as resumed (so sync BEHAVIOR is correct
				// without this), but a `--for` pause stays on disk and the
				// `sync pause-status` / tray displays keep showing it as
				// active until something calls CleanExpired. Best-effort:
				// log and continue on error.
				sweepExpiredPauses := func() {
					if err := pauseStore.CleanExpired(time.Now().UTC()); err != nil {
						lg.Warn("expired-pause cleanup failed", "err", err)
					}
				}
				projectScanWG.Add(1)
				go func() {
					defer projectScanWG.Done()
					t := time.NewTicker(daemonProjectScanInterval)
					defer t.Stop()
					// Run once at startup after a short warmup
					// (orchestrator + initial-scan need a moment to
					// settle); subsequent runs follow the ticker.
					select {
					case <-ctx.Done():
						return
					case <-time.After(daemonProjectScanWarmup):
					}
					sweepExpiredPauses()
					projectScanTick(ctx, lg, store, projectReg, orch, roots, maxDepth)
					for {
						select {
						case <-ctx.Done():
							return
						case <-t.C:
							sweepExpiredPauses()
							projectScanTick(ctx, lg, store, projectReg, orch, roots, maxDepth)
						}
					}
				}()
				lg.Info("project-detection scan enabled",
					"interval", daemonProjectScanInterval.String(),
					"roots", roots,
					"max_depth", maxDepth)
			} else {
				lg.Info("project-detection scan disabled (project-scan-interval=0)")
			}

			// v0.99.0 BRD-04 §4.3.1 branch auto-archive lifecycle pass.
			var branchLifecycleWG sync.WaitGroup
			if daemonBranchAutoArchiveAfterDays > 0 {
				stale := time.Duration(daemonBranchAutoArchiveAfterDays) * 24 * time.Hour
				archiver := retention.NewAutoArchiveRunner(store, stale, daemonBranchAutoArchiveInterval)
				branchLifecycleWG.Add(1)
				go func() {
					defer branchLifecycleWG.Done()
					if err := archiver.Run(ctx); err != nil {
						lg.Error("branch auto-archive lifecycle terminated", "err", err)
					}
				}()
				lg.Info("branch auto-archive lifecycle started",
					"stale_after", stale.String(),
					"interval", daemonBranchAutoArchiveInterval.String())
			} else {
				lg.Info("branch auto-archive lifecycle disabled (branch-auto-archive-after-days=0)")
			}

			// SIGHUP → rotate the daemon log file + reload config
			// (FR-03.16 complete in v0.27.0; all hot fields live in
			// v0.27.1). On Windows this is a no-op (no SIGHUP); on
			// unix the handler goroutine lives for the rest of ctx
			// and on each signal rotates the log then runs the shared
			// applyJSONConfigReload pass (daemon_json_reload.go).
			// Installed AFTER orchestrator and hermeswatch are wired
			// so the handler has live references to them.
			installSighupHandler(ctx, lg, configPath, &currentCfg, orch, hw, snapRunner)

			// Upgrade the control-socket reloader installed earlier
			// (pre-orchestrator it could only re-pull the TOML layers).
			// Now that orch / hermeswatch / snapRunner are wired,
			// `aplexica daemon reload` applies BOTH the TOML tunables
			// AND the <state-dir>/config.json hot fields — log level,
			// quiet, guard window, snapshot cadence/max-age, and the
			// FR-03.3 sync gate (with backfill to newly-enabled
			// agents) — the same set SIGHUP applies on Unix. This is
			// what makes `aplexica sync enable X` + `daemon reload`
			// actually take effect without a restart.
			// prevRulesSig tracks the last-applied merged ruleset so a
			// reload can tell whether the rules actually changed (and
			// therefore whether newly-allowed targets need a backfill).
			// Seeded from the boot-time user rules; cloud rules arriving
			// later via the remote callback may make the first reload
			// look like a change — harmless, RefanOutAll is idempotent.
			prevRulesSig := func() string {
				user, _ := loadUserRulesQuiet(userRulesPath())
				return rulesSignature(mergeRules(user.Sync.Rules, cloudRules.get()))
			}()
			ctlSrv.SetReloader(func() (any, error) {
				report, rerr := reloadDaemonConfigPackage(cmd)
				if rerr != nil {
					return nil, rerr
				}
				applied, jerr := applyJSONConfigReload(lg, configPath, &currentCfg, orch, hw, snapRunner)
				if jerr != nil {
					return nil, jerr
				}
				if len(applied) > 0 {
					report += "  config.json hot fields applied: " + strings.Join(applied, ", ") + "\n"
				} else {
					report += "  config.json: no hot-field changes\n"
				}
				// Rules: rebuild the selective-sync engine from rules.toml
				// merged with the current cloud ruleset — the same swap the
				// portal accessor's hotReload performs — so `aplexica rules
				// add/edit/remove` + reload applies without a restart. On an
				// actual ruleset change, kick a backfill: newly-allowed
				// targets need EXISTING artifacts re-fanned, not just future
				// events (mirrors the sync-gate backfill above).
				user, uerr := loadUserRulesQuiet(userRulesPath())
				merged := mergeRules(user.Sync.Rules, cloudRules.get())
				eng, nerr := syncrules.New(merged)
				if uerr != nil || nerr != nil {
					report += "  rules.toml: NOT applied (parse error; engine left unchanged)\n"
					lg.Warn("rules reload skipped — parse error", "user_err", uerr, "new_err", nerr)
				} else {
					orch.SetRulesEngine(eng)
					sig := rulesSignature(merged)
					if sig != prevRulesSig {
						prevRulesSig = sig
						report += fmt.Sprintf("  rules engine rebuilt (%d rules) — backfill started\n", len(merged))
						go func() {
							n, ferr := orch.RefanOutAll(context.Background())
							if ferr != nil {
								lg.Warn("fan-out backfill failed (rules changed via reload)", "err", ferr)
								return
							}
							lg.Info("fan-out backfill complete (rules changed via reload)", "artifacts_refanned", n)
						}()
					} else {
						report += fmt.Sprintf("  rules engine rebuilt (%d rules, unchanged)\n", len(merged))
					}
				}
				lg.Info("config reload via control socket", "report", report)
				return report, nil
			})

			// v0.75.0 FR-10.8: also pipe SIGHUP through the new TOML
			// reload pass. The existing installSighupHandler handles
			// the legacy <state-dir>/config.json reload (LogLevel /
			// Quiet / GuardWindow / etc.); this companion handler
			// re-pulls the BRD-10 §10.1 layers and logs the diff.
			installTOMLSighupHandler(ctx, cmd, lg)

			// Live-watch every registered LOCAL project
			// folder so edits in those folders import (and fan out
			// folder-locally). Runs in a goroutine because orch.Run below
			// blocks until ctx is cancelled; a short settle lets Run wire
			// its primary + AdditionalRoots watchers first (WatchFolder is
			// documented safe-to-call after Run has started). The primary
			// --dir is skipped — orch already watches it — to avoid a
			// double watcher on the same tree. Best-effort per folder:
			// a watch error logs and the loop continues. The long-lived
			// ctx (not a request ctx) bounds these watchers' lifetimes.
			primaryDir := daemonDir
			if abs, aerr := filepath.Abs(daemonDir); aerr == nil {
				primaryDir = abs
			}
			go func() {
				select {
				case <-ctx.Done():
					return
				case <-time.After(2 * time.Second):
				}
				for _, e := range projectReg.List() {
					if e.EffectiveScope() != "local" {
						continue
					}
					abs, aerr := filepath.Abs(e.Path)
					if aerr != nil {
						abs = e.Path
					}
					if abs == primaryDir {
						continue // orchestrator already watches --dir
					}
					if err := orch.WatchFolder(ctx, abs); err != nil {
						lg.Warn("seed-watch registered local project failed",
							"id", e.ID, "path", abs, "err", err)
						continue
					}
					lg.Info("seed-watching registered local project", "id", e.ID, "path", abs)
				}
			}()

			if err := orch.ReopenWatchersBeforeRun(); err != nil {
				lg.Error("watcher reopen before live run failed", "err", err)
				return err
			}
			// Initial reconciliation scan — catch up on files that already
			// exist (FR-03.4). Run it in the background after fresh watcher
			// sources are installed so the daemon can start the live watch loop
			// immediately. Large native histories can take minutes to walk; they
			// must not delay new edits, runtime catch-up scans, or remote sync.
			go func() {
				lg.Info("initial reconciliation scan starting")
				if err := orch.InitialScan(ctx); err != nil {
					lg.Error("initial reconciliation scan failed", "err", err)
					return
				}
				lg.Info("initial reconciliation scan complete")
			}()
			go func() {
				lg.Info("native live scan starting", "interval", daemonNativeLiveScanInterval)
				orch.RunNativeLiveScan(ctx, daemonNativeLiveScanInterval)
			}()
			go func() {
				lg.Info("codex session live scan starting", "interval", daemonCodexSessionScanInterval)
				ticker := time.NewTicker(daemonCodexSessionScanInterval)
				defer ticker.Stop()
				for {
					select {
					case <-ctx.Done():
						return
					case <-ticker.C:
						if n := orch.ScanRecentCodexSessions(ctx); n > 0 {
							lg.Info("codex session live scan processed candidates", "count", n)
						}
					}
				}
			}()
			go func() {
				lg.Info("claude session live scan starting", "interval", daemonClaudeSessionScanInterval)
				ticker := time.NewTicker(daemonClaudeSessionScanInterval)
				defer ticker.Stop()
				for {
					select {
					case <-ctx.Done():
						return
					case <-ticker.C:
						if n := orch.ScanRecentClaudeSessions(ctx); n > 0 {
							lg.Info("claude session live scan processed candidates", "count", n)
						}
					}
				}
			}()
			lg.Info("daemon ready", "socket", sockPath)
			orch.Run(ctx)
			lg.Info("daemon stopping")
			_ = ctlSrv.Stop()
			// Wait for the hermeswatch goroutine to exit cleanly. ctx
			// is already canceled by here (orch.Run returned because
			// ctx.Done fired), so hw.Run will be returning imminently.
			hwWG.Wait()
			// Time-based snapshotter goroutine — same story, waits on
			// ctx cancel so .Wait() returns immediately once Run()
			// observes it.
			snapWG.Wait()
			// Disk-pressure goroutine (v0.34.0) — same shutdown
			// contract. Tick loop returns when ctx.Done fires.
			pressureWG.Wait()
			// Project-detection scan (v0.62.0) — same shutdown
			// contract.
			projectScanWG.Wait()
			return nil
		}

		// Windows SCM-launched path. svc.IsWindowsService returns true
		// when the process was started by the Service Control Manager; in
		// that case we route the body through daemon.RunAsService so SCM's
		// Stop / Shutdown / Interrogate requests reach us. NOTE: as of the
		// per-user-task migration, `aplexica daemon install` no longer
		// registers an SCM service (it installs a logon Scheduled Task —
		// see internal/daemon/install_windows.go), so this branch only
		// fires for a binary hosted under SCM manually. On non-windows the
		// IsWindowsService stub always returns (false, nil) and we fall
		// through to the foreground signal-aware path.
		if isSvc, err := daemon.IsWindowsService(); err != nil {
			lg.Error("daemon: detect SCM context", "err", err)
		} else if isSvc {
			return runAsWindowsService(lg.Logger, runBody)
		}

		// Foreground path: signal-aware ctx. runBody internally
		// derives a cancelable child ctx and wires the
		// ctlSrv.Done()-watch goroutine onto it, so the foreground
		// caller only needs to supply Interrupt/SIGTERM
		// responsiveness here.
		ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		return runBody(ctx)
	},
}

var daemonStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Query the running daemon's status",
	RunE: func(cmd *cobra.Command, args []string) error {
		sockPath := filepath.Join(daemonStateDir, "aplexicad.sock")
		resp, err := daemon.SendCommand(sockPath, daemon.Request{Command: "status"})
		if err != nil {
			return fmt.Errorf("daemon: not running or unreachable (%w)", err)
		}
		if !resp.OK {
			return fmt.Errorf("daemon: %s", resp.Error)
		}
		// Pretty-print the StatusInfo.
		if d, ok := resp.Data.(map[string]any); ok {
			fmt.Fprintf(cmd.OutOrStdout(), "daemon: running\n")
			fmt.Fprintf(cmd.OutOrStdout(), "  pid:        %v\n", d["pid"])
			fmt.Fprintf(cmd.OutOrStdout(), "  started:    %v\n", d["startedAt"])
			fmt.Fprintf(cmd.OutOrStdout(), "  watching:   %v\n", d["watchedDir"])
			fmt.Fprintf(cmd.OutOrStdout(), "  version:    %v\n", d["version"])
		} else {
			fmt.Fprintf(cmd.OutOrStdout(), "%+v\n", resp.Data)
		}
		return nil
	},
}

var daemonReloadCmd = &cobra.Command{
	Use:   "reload",
	Short: "Trigger a hot config reload",
	Long: `Send a "reload" command over the control socket. The daemon
	re-reads the config layers and applies any hot-reloadable
	changes; restart-required keys are logged with old/new values and
	a "takes effect on next daemon start" annotation.

On Unix, sending SIGHUP to the daemon process achieves the same
result. This command exists for Windows (no SIGHUP) and for users
who prefer not to hunt down a PID.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		sockPath := filepath.Join(daemonStateDir, "aplexicad.sock")
		resp, err := daemon.SendCommand(sockPath, daemon.Request{Command: "reload"})
		if err != nil {
			return fmt.Errorf("daemon reload: %w", err)
		}
		if !resp.OK {
			return fmt.Errorf("daemon reload: %s", resp.Error)
		}
		if s, ok := resp.Data.(string); ok {
			fmt.Fprint(cmd.OutOrStdout(), s)
		} else {
			fmt.Fprintf(cmd.OutOrStdout(), "%v\n", resp.Data)
		}
		return nil
	},
}

var daemonStopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop the running daemon (graceful shutdown via control socket)",
	RunE: func(cmd *cobra.Command, args []string) error {
		sockPath := filepath.Join(daemonStateDir, "aplexicad.sock")
		resp, err := daemon.SendCommand(sockPath, daemon.Request{Command: "stop"})
		if err != nil {
			return fmt.Errorf("daemon: not running or unreachable (%w)", err)
		}
		if !resp.OK {
			return fmt.Errorf("daemon: %s", resp.Error)
		}
		fmt.Fprintln(cmd.OutOrStdout(), "daemon: stop signal sent")
		return nil
	},
}

var daemonRestartCmd = &cobra.Command{
	Use:   "restart",
	Short: "Stop the running daemon and start a fresh one",
	Long: `Stops the running daemon via the control socket, waits for the
socket to disappear (the daemon's signal that the process has exited),
then re-runs ` + "`aplexica daemon start`" + ` with the current invocation's
flags.

If the daemon isn't running, restart simply starts it. Useful when a
config change requires daemon-level restart (per the
restart_required schema field surfaced by SIGHUP / control-socket
reload).`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		sockPath := filepath.Join(daemonStateDir, "aplexicad.sock")

		// Best-effort stop. Ignore "not running" — restart is also the
		// happy path for "start fresh."
		if resp, err := daemon.SendCommand(sockPath, daemon.Request{Command: "stop"}); err == nil && resp.OK {
			fmt.Fprintln(cmd.OutOrStdout(), "daemon: stop signal sent")
		} else {
			fmt.Fprintln(cmd.OutOrStdout(), "daemon: was not running (or unreachable); starting fresh")
		}

		// Wait for the socket to disappear (the daemon removes it on
		// graceful shutdown). Bounded wait so a wedged daemon doesn't
		// block restart forever.
		deadline := time.Now().Add(restartStopWait)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(sockPath); os.IsNotExist(err) {
				break
			}
			time.Sleep(restartPollInterval)
		}

		// Re-invoke start via the same machinery the start subcommand uses.
		return daemonStartCmd.RunE(cmd, args)
	},
}

// restart timing constants (BRD-03 §4 wedged-daemon recovery).
const (
	restartStopWait     = 5 * time.Second
	restartPollInterval = 50 * time.Millisecond
)

var daemonLogsCmd = &cobra.Command{
	Use:   "logs",
	Short: "Print the daemon log (--follow streams new lines)",
	Long: `Tails the daemon's current log file (typically
~/.aplexica/logs/aplexicad.log). With --follow, streams new lines as
they're appended (Ctrl-C to stop).

	The log is line-oriented JSON; pipe through your favorite JSON tool
	for filtering:

  aplexica daemon logs | jq '. | select(.level == "ERROR")'`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		path := filepath.Join(daemonLogDir, "aplexicad.log")
		f, err := os.Open(path)
		if err != nil {
			if os.IsNotExist(err) {
				return fmt.Errorf("daemon log not found at %s — daemon may not have started yet", path)
			}
			return err
		}
		defer f.Close()

		if _, err := io.Copy(cmd.OutOrStdout(), f); err != nil {
			return err
		}
		if !daemonLogsFollow {
			return nil
		}

		// Follow: poll for new content. The log file may rotate (the
		// daemon does midnight-local rotation per FR-10.3), in which
		// case we re-open the file on the next poll.
		ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		ticker := time.NewTicker(logsFollowPollInterval)
		defer ticker.Stop()
		lastSize, _ := f.Seek(0, io.SeekCurrent)
		for {
			select {
			case <-ctx.Done():
				return nil
			case <-ticker.C:
				// Check for rotation: if the file is shorter than where
				// we are, it rotated; reopen.
				info, err := os.Stat(path)
				if err != nil {
					continue
				}
				if info.Size() < lastSize {
					_ = f.Close()
					nf, ferr := os.Open(path)
					if ferr != nil {
						continue
					}
					f = nf
					lastSize = 0
					if _, err := io.Copy(cmd.OutOrStdout(), f); err != nil {
						return err
					}
					lastSize, _ = f.Seek(0, io.SeekCurrent)
					continue
				}
				if info.Size() > lastSize {
					if _, err := io.Copy(cmd.OutOrStdout(), f); err != nil {
						return err
					}
					lastSize, _ = f.Seek(0, io.SeekCurrent)
				}
			}
		}
	},
}

var (
	daemonLogsFollow       bool
	logsFollowPollInterval = 200 * time.Millisecond
)

var daemonInstallCmd = &cobra.Command{
	Use:   "install",
	Short: "Register the daemon as a per-user service (auto-start at login)",
	Long: `Writes a platform-appropriate service definition and registers
it so the daemon auto-starts at user login — as the logged-in user, so
os.UserHomeDir() resolves to your profile and the adapters discover your
agents:

  - macOS: launchd LaunchAgent at ~/Library/LaunchAgents/com.aplexica.aplexicad.plist
  - Linux: systemd --user unit at ~/.config/systemd/user/aplexicad.service
  - Windows: a per-user logon-triggered Scheduled Task ("Aplexica Sync
    Daemon") with an InteractiveToken principal — runs as the logged-in
    user, no Administrator rights and no stored password. (Earlier
    versions registered a LocalSystem SCM service, which ran as SYSTEM
    and therefore discovered none of the user's agents; that path was
    retired in favor of the per-user task.)

The installed unit runs the daemon with the flags you pass to install.
Re-running install updates the definition. Most fields can also be reloaded live by editing
<state-dir>/config.json and sending SIGHUP to the daemon (unix) — see ` + "`aplexica daemon`" + `
help for which fields are hot-reloadable.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := defaultDaemonWatchDir(); err != nil {
			return err
		}
		exe, err := persistentExecutable()
		if err != nil {
			return fmt.Errorf("daemon: locate persistent executable: %w", err)
		}
		opts := daemon.InstallOptions{
			AplexicaPath: exe,
			Dir:          daemonDir,
			StoreRoot:    daemonStoreRoot,
			SecretsRoot:  daemonSecretsRoot,
			StateDir:     daemonStateDir,
			LogDir:       daemonLogDir,
			Recursive:    daemonRecursive,
		}
		if daemonQuiet > 0 {
			opts.Quiet = daemonQuiet.String()
		}
		if daemonGuardWindow > 0 {
			opts.GuardWindow = daemonGuardWindow.String()
		}
		// Only forward hermes flags the user explicitly set, so the
		// daemon's own defaults stay in effect when the user doesn't
		// care (rather than freezing whatever cobra default we happen
		// to ship with today into every installed service definition).
		if cmd.Flags().Changed("hermes-watch") {
			hw := daemonHermesWatch
			opts.HermesWatch = &hw
		}
		if cmd.Flags().Changed("hermes-watch-interval") {
			opts.HermesWatchInterval = daemonHermesWatchInterval.String()
		}
		if cmd.Flags().Changed("hermes-db") {
			opts.HermesDB = daemonHermesDBPath
		}
		inst, err := daemon.New(opts)
		if err != nil {
			return err
		}
		// Persist the watched directory before registering/starting the
		// platform service. The tray uses this value to restart a stopped
		// daemon when no live status snapshot is available.
		if err := setDaemonWatchDirInConfig(daemonStateDir, daemonDir); err != nil {
			return fmt.Errorf("daemon install: persist watched directory: %w", err)
		}
		if err := inst.Install(); err != nil {
			return fmt.Errorf("daemon install (%s): %w", inst.PlatformLabel(), err)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "daemon: installed as %s (watching %s)\n", inst.PlatformLabel(), daemonDir)

		// v0.50.0: FR-03.26 — the daemon installer also registers the
		// tray's autostart entry. The default (daemon.TrayEnabledDefault)
		// is now ON for every OS (opt-out), so a stock install always sets
		// up the tray. User can override with --tray=false.
		want := daemonInstallTray
		if !cmd.Flags().Changed("tray") {
			want = daemon.TrayEnabledDefault()
		}
		if !want {
			return nil
		}
		// Resolve aplexicatray next to the aplexica binary first, then on
		// PATH. A packaged install must ship both; missing tray is a broken
		// install, not a soft-degraded mode.
		trayPath, err := resolveTrayPath(exe)
		if err != nil {
			return fmt.Errorf("daemon install: --tray requested but aplexicatray is missing: %w", err)
		}
		tInst, err := trayinstall.New(trayOptions(trayPath, exe))
		if err != nil {
			return fmt.Errorf("daemon install: tray options: %w", err)
		}
		if err := tInst.Install(); err != nil {
			return fmt.Errorf("daemon install: tray autostart install failed (%s): %w",
				tInst.PlatformLabel(), err)
		}
		// Persist the enabled flag so the tray binary runs on next launch
		// even if the user previously opted out.
		if err := setTrayEnabledInConfig(daemonStateDir, true); err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(),
				"daemon install: WARNING — tray autostart installed but failed to set tray.enabled in config: %v\n", err)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "daemon: tray autostart installed as %s (target %s)\n",
			tInst.PlatformLabel(), trayPath)
		if err := startTrayCompanion(trayPath, exe); err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "daemon install: WARNING — tray immediate launch failed: %v\n", err)
		}
		return nil
	},
}

var daemonUninstallCmd = &cobra.Command{
	Use:   "uninstall",
	Short: "Remove the daemon's service registration",
	RunE: func(cmd *cobra.Command, args []string) error {
		exe, _ := os.Executable()
		// AplexicaPath and Dir are required by Validate but irrelevant
		// for uninstall — supply placeholder values so the dispatch works.
		inst, err := daemon.New(daemon.InstallOptions{
			AplexicaPath: exe,
			Dir:          "/",
		})
		if err != nil {
			return err
		}
		if err := inst.Uninstall(); err != nil {
			return fmt.Errorf("daemon uninstall (%s): %w", inst.PlatformLabel(), err)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "daemon: uninstalled (%s)\n", inst.PlatformLabel())
		return nil
	},
}

func init() {
	home, _ := os.UserHomeDir()

	// Shared flags on the parent so subcommands inherit them via Persistent.
	// The default honors APLEXICA_STATE_DIR (via defaultStateDir) so the
	// daemon and the `aplexica web …` subcommands resolve the same path.
	defaultState, err := defaultStateDir()
	if err != nil {
		defaultState = filepath.Join(home, ".aplexica", "state")
	}
	daemonCmd.PersistentFlags().StringVar(&daemonStateDir, "state-dir",
		defaultState,
		"Daemon state directory (lock file + control socket live here)")
	daemonCmd.PersistentFlags().StringVar(&daemonLogDir, "log-dir",
		filepath.Join(home, ".aplexica", "logs"),
		"Daemon log directory")

	defaultHermesDB := filepath.Join(home, ".hermes", "state.db")

	// Flags that only `start`, `serve`, and `install` need.
	for _, c := range []*cobra.Command{daemonStartCmd, daemonServeCmd, daemonInstallCmd} {
		c.Flags().StringVar(&daemonDir, "dir", "",
			"Directory to watch (defaults to your home for start/install; required for serve)")
		c.Flags().StringVar(&daemonStoreRoot, "store",
			filepath.Join(home, ".aplexica", "store"),
			"Canonical store root directory")
		c.Flags().StringVar(&daemonSecretsRoot, "secrets-root",
			filepath.Join(home, ".aplexica", "secrets"),
			"Secrets store root directory")
		c.Flags().DurationVar(&daemonQuiet, "quiet", 500*time.Millisecond,
			"Debouncer quiet period")
		c.Flags().DurationVar(&daemonGuardWindow, "guard-window", 5*time.Second,
			"Recursion guard window")
		c.Flags().BoolVarP(&daemonRecursive, "recursive", "r", false,
			"Watch the directory tree recursively")
		c.Flags().StringVar(&daemonPluginsDir, "plugins-dir", "",
			"Directory to scan for external adapter plugins (default: <state-dir>/plugins)")
	}
	_ = daemonServeCmd.MarkFlagRequired("dir")

	// Hermes auto-watch flags. Bound to start + serve + install so that
	// `daemon install --hermes-watch-interval 10s ...` bakes the user's
	// choice into the generated service definition (v0.13.2). The install
	// RunE uses cmd.Flags().Changed() to detect explicit user values, so
	// the cobra defaults below do not leak into InstallOptions.
	for _, c := range []*cobra.Command{daemonStartCmd, daemonServeCmd, daemonInstallCmd} {
		c.Flags().BoolVar(&daemonHermesWatch, "hermes-watch", true,
			"Auto-export Hermes sessions when ~/.hermes/state.db exists (set false to disable)")
		c.Flags().DurationVar(&daemonHermesWatchInterval, "hermes-watch-interval", 5*time.Second,
			"Hermes watch poll interval")
		c.Flags().StringVar(&daemonHermesDBPath, "hermes-db", defaultHermesDB,
			"Hermes state.db path (used when --hermes-watch is true)")
	}

	// Retention auto-snapshot cadence (v0.29.2; BRD-03 §4.8.1). Per-kind
	// event-count thresholds + per-kind time-based triggers. Bound to
	// start + serve + install so the chosen cadence is baked into
	// installed service definitions. SIGHUP-reloadable for the cadence
	// fields via SnapshotCadenceChanged in daemon.Diff; the time-based
	// trigger is restart-required for now (the background ticker reads
	// the map by reference at startup, not under a live setter).
	//
	// Defaults per BRD-03 §4.8.1:
	//   cadence — conversation 100, memory/skill/tool 50.
	//   max-age — conversation 24h, memory/skill/tool 7d.
	for _, c := range []*cobra.Command{daemonStartCmd, daemonServeCmd, daemonInstallCmd} {
		c.Flags().IntVar(&daemonSnapCadenceConv, "snapshot-cadence-conv", 100,
			"Conversation auto-snapshot every N events per artifact after each primary import (0 disables).")
		c.Flags().IntVar(&daemonSnapCadenceMem, "snapshot-cadence-mem", 50,
			"Memory auto-snapshot every N events per artifact after each primary import (0 disables).")
		c.Flags().IntVar(&daemonSnapCadenceSkill, "snapshot-cadence-skill", 50,
			"Skill auto-snapshot every N events per artifact after each primary import (0 disables).")
		c.Flags().IntVar(&daemonSnapCadenceTool, "snapshot-cadence-tool", 50,
			"Tool auto-snapshot every N events per artifact after each primary import (0 disables).")
		c.Flags().DurationVar(&daemonSnapMaxAgeConv, "snapshot-max-age-conv", 24*time.Hour,
			"Conversation: take a snapshot when the latest event is older than D and is not itself a snapshot (0 disables).")
		c.Flags().DurationVar(&daemonSnapMaxAgeMem, "snapshot-max-age-mem", 7*24*time.Hour,
			"Memory: take a snapshot when the latest event is older than D and is not itself a snapshot (0 disables).")
		c.Flags().DurationVar(&daemonSnapMaxAgeSkill, "snapshot-max-age-skill", 7*24*time.Hour,
			"Skill: take a snapshot when the latest event is older than D and is not itself a snapshot (0 disables).")
		c.Flags().DurationVar(&daemonSnapMaxAgeTool, "snapshot-max-age-tool", 7*24*time.Hour,
			"Tool: take a snapshot when the latest event is older than D and is not itself a snapshot (0 disables).")
		c.Flags().Float64Var(&daemonStoreHighWatermarkGB, "store-high-watermark-gb", 0,
			"When the canonical store exceeds this size in GB, run an ordered retention sweep: evict old attachment bytes plus GC, then snapshot, then compact history only if still over. 0 disables. Checked every 5 minutes.")

		// v0.74.0 BRD-10 §10.1 layer 6 — repeatable CLI override.
		c.Flags().StringSliceVarP(&daemonCLISets, "config-set", "c", nil,
			"Override config key=value, repeatable")

		// v0.62.0 BRD-02 §4.13.4 periodic project-detection scan.
		c.Flags().DurationVar(&daemonProjectScanInterval, "project-scan-interval", 60*time.Minute,
			"How often the daemon walks --project-scan-roots looking for VCS repos that match pending projects and auto-links them. 0 disables.")
		c.Flags().StringSliceVar(&daemonProjectScanRoots, "project-scan-roots", nil,
			"Comma-separated filesystem roots to walk for VCS-repo detection. Defaults to $HOME when empty.")
		c.Flags().IntVar(&daemonProjectScanMaxDepth, "project-scan-max-depth", 6,
			"Max depth below each scan root the daemon walks looking for VCS repos. 0 = root only.")

		// v0.99.0 BRD-04 §4.3.1 branch auto-archive lifecycle pass.
		const defaultAutoArchiveDays = 90
		c.Flags().IntVar(&daemonBranchAutoArchiveAfterDays, "branch-auto-archive-after-days", defaultAutoArchiveDays,
			"Auto-archive branches with no events newer than this. 0 disables.")
		c.Flags().DurationVar(&daemonBranchAutoArchiveInterval, "branch-auto-archive-interval", 24*time.Hour,
			"How often the daemon runs the branch auto-archive lifecycle pass.")

		// Part 3: one-time first-run native safety snapshot. On a fresh
		// install (no ~/.aplexica/backups/.initial-done marker) the daemon
		// snapshots each discovered agent's native global roots into
		// ~/.aplexica/backups/pre-sync-<RFC3339>/ so the user can roll back
		// to their pre-Aplexica state. --no-initial-backup disables it.
		c.Flags().BoolVar(&daemonNoInitialBackup, "no-initial-backup", false,
			"Skip the one-time first-run native safety snapshot of each discovered agent's global roots.")
	}

	// serve-only, hidden: the Windows keep-alive scheduled task launches
	// `daemon serve … --windows-detach-console` so the monitored (and
	// RestartOnFailure-restarted) serve has no console window.
	daemonServeCmd.Flags().BoolVar(&daemonWinDetachConsole, "windows-detach-console", false,
		"Windows-only: FreeConsole on startup to hide the console window (used by the keep-alive task).")
	_ = daemonServeCmd.Flags().MarkHidden("windows-detach-console")

	// v0.50.0: FR-03.26 + §4.9.4. The --tray bool flag lets the user
	// opt out of the tray-autostart chain. The default is true on every
	// desktop OS; users who explicitly pass --tray=false override it.
	daemonInstallCmd.Flags().BoolVar(&daemonInstallTray, "tray", daemon.TrayEnabledDefault(),
		"register the aplexicatray autostart entry alongside the daemon "+
			"(default: true on every OS; packaged installs include aplexicatray)")

	daemonCmd.AddCommand(daemonStartCmd)
	daemonCmd.AddCommand(daemonServeCmd)
	daemonCmd.AddCommand(daemonStatusCmd)
	daemonCmd.AddCommand(daemonStopCmd)
	daemonCmd.AddCommand(daemonReloadCmd)
	daemonCmd.AddCommand(daemonRestartCmd)
	daemonLogsCmd.Flags().BoolVarP(&daemonLogsFollow, "follow", "f", false,
		"stream new lines as they're appended (Ctrl-C to stop)")
	daemonCmd.AddCommand(daemonLogsCmd)
	daemonCmd.AddCommand(daemonInstallCmd)
	daemonCmd.AddCommand(daemonUninstallCmd)
	rootCmd.AddCommand(daemonCmd)
}

// applyConfigToFlags overlays file-config values onto package-level flag
// variables for any flag the user did NOT explicitly set on the CLI.
// Mirrors the v0.13.2 cmd.Flags().Changed() pattern used in the install
// command. The result is the v0.27.0 precedence chain:
//
//	compiled-in default < config-file value < explicit CLI flag
//
// Bool flags are tri-state-ish: cobra's Changed() detects whether the
// user typed the flag at all, regardless of the value. We honor that.
func applyConfigToFlags(cmd *cobra.Command, c *daemon.Config) {
	if !cmd.Flags().Changed("dir") && c.Dir != "" {
		daemonDir = c.Dir
	}
	if !cmd.Flags().Changed("state-dir") && c.StateDir != "" {
		daemonStateDir = c.StateDir
	}
	if !cmd.Flags().Changed("log-dir") && c.LogDir != "" {
		daemonLogDir = c.LogDir
	}
	if !cmd.Flags().Changed("store") && c.StoreRoot != "" {
		daemonStoreRoot = c.StoreRoot
	}
	if !cmd.Flags().Changed("secrets-root") && c.SecretsRoot != "" {
		daemonSecretsRoot = c.SecretsRoot
	}
	if !cmd.Flags().Changed("quiet") && c.Quiet > 0 {
		daemonQuiet = c.Quiet
	}
	if !cmd.Flags().Changed("guard-window") && c.GuardWindow > 0 {
		daemonGuardWindow = c.GuardWindow
	}
	if !cmd.Flags().Changed("recursive") && c.Recursive {
		daemonRecursive = c.Recursive
	}
	if !cmd.Flags().Changed("hermes-watch") && c.HermesWatch {
		// Asymmetric: the file can only flip hermes-watch ON. We can't
		// distinguish "file said false" from "field absent" (both
		// marshal to omitted). To disable, set hermes-watch=false on
		// the CLI explicitly. This matches the v0.13.2 install pattern.
		daemonHermesWatch = c.HermesWatch
	}
	if !cmd.Flags().Changed("hermes-watch-interval") && c.HermesWatchInterval > 0 {
		daemonHermesWatchInterval = c.HermesWatchInterval
	}
	if !cmd.Flags().Changed("hermes-db") && c.HermesDB != "" {
		daemonHermesDBPath = c.HermesDB
	}
	// Per-kind snapshot cadence + max-age (v0.29.2). Same asymmetric
	// semantics as hermes-watch: omitted zero fields can't disable from
	// the file (omitempty marshals 0 as absent). To explicitly disable a
	// kind, pass --snapshot-cadence-<kind>=0 (or --snapshot-max-age-<kind>=0)
	// on the CLI.
	if !cmd.Flags().Changed("snapshot-cadence-conv") && c.SnapshotCadenceConversation > 0 {
		daemonSnapCadenceConv = c.SnapshotCadenceConversation
	}
	if !cmd.Flags().Changed("snapshot-cadence-mem") && c.SnapshotCadenceMemory > 0 {
		daemonSnapCadenceMem = c.SnapshotCadenceMemory
	}
	if !cmd.Flags().Changed("snapshot-cadence-skill") && c.SnapshotCadenceSkill > 0 {
		daemonSnapCadenceSkill = c.SnapshotCadenceSkill
	}
	if !cmd.Flags().Changed("snapshot-cadence-tool") && c.SnapshotCadenceTool > 0 {
		daemonSnapCadenceTool = c.SnapshotCadenceTool
	}
	if !cmd.Flags().Changed("snapshot-max-age-conv") && c.SnapshotMaxAgeConversation > 0 {
		daemonSnapMaxAgeConv = c.SnapshotMaxAgeConversation
	}
	if !cmd.Flags().Changed("snapshot-max-age-mem") && c.SnapshotMaxAgeMemory > 0 {
		daemonSnapMaxAgeMem = c.SnapshotMaxAgeMemory
	}
	if !cmd.Flags().Changed("snapshot-max-age-skill") && c.SnapshotMaxAgeSkill > 0 {
		daemonSnapMaxAgeSkill = c.SnapshotMaxAgeSkill
	}
	if !cmd.Flags().Changed("snapshot-max-age-tool") && c.SnapshotMaxAgeTool > 0 {
		daemonSnapMaxAgeTool = c.SnapshotMaxAgeTool
	}
	// v0.34.0 disk-pressure watermark — same asymmetric semantics.
	if !cmd.Flags().Changed("store-high-watermark-gb") && c.StoreHighWatermarkGB > 0 {
		daemonStoreHighWatermarkGB = c.StoreHighWatermarkGB
	}
}

// snapshotCurrentDaemonConfig captures the effective config for diffing
// on SIGHUP. Returns a Config populated from the current package-level
// flag variables. The caller is expected to set LogLevel separately
// (since cmd_daemon.go has no log-level flag — that field is config-file
// only).
func snapshotCurrentDaemonConfig() daemon.Config {
	return daemon.Config{
		Dir:                         daemonDir,
		StateDir:                    daemonStateDir,
		LogDir:                      daemonLogDir,
		StoreRoot:                   daemonStoreRoot,
		SecretsRoot:                 daemonSecretsRoot,
		Quiet:                       daemonQuiet,
		GuardWindow:                 daemonGuardWindow,
		Recursive:                   daemonRecursive,
		HermesWatch:                 daemonHermesWatch,
		HermesWatchInterval:         daemonHermesWatchInterval,
		HermesDB:                    daemonHermesDBPath,
		SnapshotCadenceConversation: daemonSnapCadenceConv,
		SnapshotCadenceMemory:       daemonSnapCadenceMem,
		SnapshotCadenceSkill:        daemonSnapCadenceSkill,
		SnapshotCadenceTool:         daemonSnapCadenceTool,
		SnapshotMaxAgeConversation:  daemonSnapMaxAgeConv,
		SnapshotMaxAgeMemory:        daemonSnapMaxAgeMem,
		SnapshotMaxAgeSkill:         daemonSnapMaxAgeSkill,
		SnapshotMaxAgeTool:          daemonSnapMaxAgeTool,
		StoreHighWatermarkGB:        daemonStoreHighWatermarkGB,
	}
}

// remoteSyncDriverInterval bounds receive-side cloud catch-up latency. Live
// MQTT delivery lands in the plugin immediately, but the daemon still pulls the
// plugin's inbound log through this driver; keep it sub-second so
// device-cloud-device materialization stays inside the 3s live-sync SLO.
const remoteSyncDriverDefaultInterval = 500 * time.Millisecond

var remoteSyncDriverInterval = remoteSyncDriverDefaultInterval

// remoteSyncDriverWarmup is the initial delay before the driver's first tick,
// so the plugin's initialize handshake can land first. A package var (not a
// const) so tests can shrink it.
const remoteSyncDriverDefaultWarmup = 500 * time.Millisecond

var remoteSyncDriverWarmup = remoteSyncDriverDefaultWarmup

// The changed-head pass is a safety net for a missed watcher/import signal;
// ordinary local commits publish immediately and membership changes invoke an
// explicit pass. Scanning every artifact once a minute is needlessly expensive
// on long-lived stores with thousands of artifacts, even when every head is
// unchanged. Five minutes preserves bounded self-repair without producing a
// visible periodic CPU spike on idle machines.
const remoteChangedHeadRepublishDefaultInterval = 5 * time.Minute

var remoteChangedHeadRepublishInterval = remoteChangedHeadRepublishDefaultInterval

// remoteRetainedBackfillInterval paces the slow retained-baseline backfill
// trickle (Orchestrator.BackfillLocalRemoteHeads): every tick publishes the
// next small batch of never-baselined artifact heads, oldest-first, so the
// whole store eventually has retained catch-up state on the relay without a
// reconnect flood. A package var so tests can shrink it.
const remoteRetainedBackfillDefaultInterval = 10 * time.Minute

var remoteRetainedBackfillInterval = remoteRetainedBackfillDefaultInterval

// remoteSyncDriverRepublishInterval throttles retained-head safety sweeps from
// the fast receive-side driver. Live local edits publish immediately in the
// orchestrator; the sweep only repairs reconnect/catch-up state and must not
// scan the whole store twice per second while idle.
const remoteSyncDriverRepublishDefaultInterval = time.Minute

var remoteSyncDriverRepublishInterval = remoteSyncDriverRepublishDefaultInterval

const remoteRegisterWrapKeyTimeout = 10 * time.Second

func runRemoteChangedHeadRepublisher(
	ctx context.Context,
	connState func() string,
	republishRemoteHeads func(context.Context) (int, error),
	lg interface {
		Info(msg string, args ...any)
		Warn(msg string, args ...any)
		Error(msg string, args ...any)
	},
) {
	if republishRemoteHeads == nil {
		return
	}
	ticker := time.NewTicker(remoteChangedHeadRepublishInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if connState != nil && connState() != "connected" {
				continue
			}
			n, err := republishRemoteHeads(ctx)
			if err != nil {
				lg.Warn("remote: changed-head republish failed (will retry)", "err", err)
				continue
			}
			if n > 0 {
				lg.Info("remote: republished changed local heads", "count", n)
			}
		}
	}
}

// runRemoteRetainedBackfillTicker drives the slow retained-baseline backfill
// trickle: on every remoteRetainedBackfillInterval tick — and only while the
// plugin reports "connected" (publishing against a dead transport would just
// pile events into the outbox) — it runs one bounded oldest-first
// Orchestrator.BackfillLocalRemoteHeads pass. Companion to
// runRemoteChangedHeadRepublisher, which owns the fast newest-first sweep.
func runRemoteRetainedBackfillTicker(
	ctx context.Context,
	connState func() string,
	backfillRemoteHeads func(context.Context) (int, error),
	lg interface {
		Info(msg string, args ...any)
		Warn(msg string, args ...any)
		Error(msg string, args ...any)
	},
) {
	if backfillRemoteHeads == nil {
		return
	}
	ticker := time.NewTicker(remoteRetainedBackfillInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if connState != nil && connState() != "connected" {
				continue
			}
			n, err := backfillRemoteHeads(ctx)
			if err != nil {
				lg.Warn("remote: retained baseline backfill failed (will retry)", "err", err)
				continue
			}
			if n > 0 {
				lg.Info("remote: backfilled retained baselines for never-published heads", "count", n)
			}
		}
	}
}

// runRemoteSyncDriver is the receive-side and key-registration driver loop. On
// a slow cadence it:
//
//  1. REGISTERS this device's X25519 wrap public key with the control plane
//     (remote.register_wrap_key) so it becomes a valid encryption recipient
//     WITHOUT a re-pair. This is the missing piece that makes 2-device sync
//     complete: until both devices have registered, the account device list
//     can't return both wrap pubkeys. Registration is idempotent server-side;
//     we register once successfully then stop re-registering (but retry on
//     failure / after a reconnect, since a plugin restart re-execs the runner).
//  2. asks the plugin which namespaces this device can see (remote.enumerate)
//     and subscribes to each so the plugin admits inbound events for them.
//
// It runs an initial pass shortly after start (giving the plugin a moment to
// complete its initialize handshake) and then on a ticker until ctx is
// cancelled. Best-effort throughout: a reconnecting/unpaired plugin returns
// ErrRemoteReconnecting; the next tick retries. devicePubFn returns this
// device's wrap public key (the same key used for inbound decrypt); a nil
// devicePubFn disables registration (registration is a no-op).
// remoteSyncDriverClient is the narrow slice of *daemon.RemoteRunner the sync
// driver needs. Declared so the driver is unit-testable with a fake. The
// RemoteRunner satisfies it (it has RegisterWrapKey, Enumerate, Subscribe, and
// Fetch).
type remoteSyncDriverClient interface {
	RegisterWrapKey(ctx context.Context, pub []byte) error
	Enumerate(ctx context.Context, params proto.RemoteEnumerateParams) (proto.RemoteEnumerateResult, error)
	Subscribe(ctx context.Context, namespaceID string) error
	Fetch(ctx context.Context, params proto.RemoteFetchParams) (proto.RemoteFetchResult, error)
	RestartCount() uint64
}

// remoteFetchPageLimit bounds one remote.fetch page; the driver follows
// NextCursor to walk a branch. remoteFetchMaxPages is a safety cap so a
// misbehaving plugin (e.g. a NextCursor that never empties) can't spin forever.
const (
	remoteFetchPageLimit   = 100
	remoteFetchMaxPages    = 1000
	remoteFetchCallTimeout = 30 * time.Second
)

// remoteFetchRetryableReason is the structured-log reason logged when reconcile
// stops advancing its resume cursor at an inbound event that failed a transient
// import (ImportRetryable). The cursor is left at the prior position so the next
// tick refetches that event (FR-03.13 — no silent loss).
const remoteFetchRetryableReason = "inbound import transiently failed; not advancing cursor past it"

// remoteFetchBackoffBase/Max bound the per-(namespace,branch) retry cadence
// after an inbound import fails transiently (ImportRetryable). Without this a
// permanently-failing event (e.g. an envelope wrapped for a stale device key)
// is refetched and re-decrypted twice per second forever.
const (
	defaultRemoteFetchBackoffBase = 2 * time.Second
	defaultRemoteFetchBackoffMax  = 5 * time.Minute
	// remoteFetchBackoffShiftCap bounds the doubling exponent so the
	// left-shift below can never overflow a Duration; the delay is clamped
	// to remoteFetchBackoffMax anyway well before the cap bites.
	remoteFetchBackoffShiftCap = 8
)

// Package vars so tests can shrink them.
var remoteFetchBackoffBase = defaultRemoteFetchBackoffBase
var remoteFetchBackoffMax = defaultRemoteFetchBackoffMax

// remoteFetchWedgedThreshold is the consecutive-failure count (same event id)
// at which the driver declares the branch wedged and surfaces it on the event
// bus. 10 failures ≈ 17 minutes of exponential backoff.
const remoteFetchWedgedThreshold = 10

func runRemoteSyncDriver(
	ctx context.Context,
	runner remoteSyncDriverClient,
	devicePubFn func() ([keys.X25519KeySize]byte, error),
	importInbound func([]proto.RemoteEvent) []syncd.ImportOutcome,
	republishRemoteHeads func(context.Context) (int, error),
	lg interface {
		Info(msg string, args ...any)
		Warn(msg string, args ...any)
		Error(msg string, args ...any)
	},
	events syncd.EventPublisher, // nil-safe; wedged-branch surfacing
) {
	// Brief initial delay so the plugin's initialize handshake can land before
	// the first call (avoids a guaranteed first-tick ErrRemoteReconnecting).
	select {
	case <-ctx.Done():
		return
	case <-time.After(remoteSyncDriverWarmup):
	}

	registered := false
	observedRestartCount := runner.RestartCount()
	var cachedWrapPub [keys.X25519KeySize]byte
	wrapPubLoaded := false
	wrapPubLoadFailureLogged := false
	registerFailureLogged := false

	registerWrapKey := func() {
		if registered || devicePubFn == nil {
			return
		}
		if !wrapPubLoaded {
			pub, err := devicePubFn()
			if err != nil {
				if !wrapPubLoadFailureLogged {
					lg.Warn("remote: load wrap pubkey for registration failed (will retry)", "err", err)
					wrapPubLoadFailureLogged = true
				}
				return
			}
			cachedWrapPub = pub
			wrapPubLoaded = true
			wrapPubLoadFailureLogged = false
		}
		rctx, cancel := context.WithTimeout(ctx, remoteRegisterWrapKeyTimeout)
		err := runner.RegisterWrapKey(rctx, cachedWrapPub[:])
		cancel()
		if err != nil {
			// Keep the sub-second retry cadence for fast recovery, but do not
			// persist the same expected reconnect/unpaired error twice per
			// second. The successful registration below is the recovery signal.
			if !registerFailureLogged {
				lg.Info("remote: register wrap key not ready (will retry)", "err", err)
				registerFailureLogged = true
			}
			return
		}
		registered = true
		registerFailureLogged = false
		lg.Info("remote: registered device wrap pubkey with the account")
	}

	subscribedNamespaces := make(map[string]struct{})
	subscribeFailureLogged := false
	subscribe := func(res proto.RemoteEnumerateResult) {
		nss := make([]string, 0, len(res.Namespaces))
		for _, ns := range res.Namespaces {
			if ns.NamespaceID != "" {
				if _, alreadySubscribed := subscribedNamespaces[ns.NamespaceID]; alreadySubscribed {
					continue
				}
				nss = append(nss, ns.NamespaceID)
			}
		}
		if len(nss) == 0 {
			return
		}
		var firstErr error
		subscribed := 0
		for _, namespaceID := range nss {
			if err := runner.Subscribe(ctx, namespaceID); err != nil {
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			subscribedNamespaces[namespaceID] = struct{}{}
			subscribed++
		}
		if firstErr != nil {
			if !subscribeFailureLogged {
				lg.Warn("remote: subscribe to one or more namespaces failed", "err", firstErr)
				subscribeFailureLogged = true
			}
		} else {
			subscribeFailureLogged = false
		}
		if subscribed > 0 {
			lg.Info("remote: subscribed to namespaces", "count", subscribed, "namespaces", nss)
		}
	}

	// cursors tracks the last imported event id per (namespace, branch) so each
	// reconcile pass resumes FORWARD instead of re-fetching all history. It is
	// an EventID (UUIDv7), matching RemoteFetchParams.Since /
	// RemoteBranchManifest.TipEventID — NOT the SHA-256 content hash (feeding a
	// hash where the plugin expects an event-id cursor would re-fetch full
	// history or no-op). Touched only by this single goroutine, so no lock.
	cursors := map[string]string{}
	// branchRetry tracks per-(namespace,branch) backoff after a transient
	// inbound import failure, keyed like cursors. Same-goroutine only.
	type branchRetryState struct {
		failures    int
		lastEventID string
		nextAttempt time.Time
	}
	branchRetry := map[string]*branchRetryState{}
	// reconcile is the remote.fetch BACKFILL: for each enumerated branch, page
	// remote.fetch forward from the local cursor and feed every event to the
	// import sink (idempotent: ImportInbound dedupes by EventID). This recovers
	// events committed while this device was offline, unsubscribed, or while the
	// relay/plugin was down — the live remote.inbound push path can't.
	//
	// Best-effort: a reconnecting plugin / fetch error just retries next tick.
	// NOTE: end-to-end backfill requires the cloud relay to implement
	// remote.enumerate/remote.fetch; against the in-repo BaseRemoteHandler
	// default (empty manifest) this is a correct no-op.
	reconcile := func(res proto.RemoteEnumerateResult) {
		if importInbound == nil {
			return
		}
		for _, ns := range res.Namespaces {
			for _, br := range ns.Branches {
				key := ns.NamespaceID + "\x1f" + br.BranchID
				if st, ok := branchRetry[key]; ok && time.Now().Before(st.nextAttempt) {
					continue // backing off a previously-failed inbound import
				}
				cursor := cursors[key]
				if cursor != "" && cursor == br.TipEventID {
					continue // already caught up to the relay's branch tip
				}
				last := cursor
				stoppedAtRetryable := false
				fetchErrored := false
				var retryableEventID string
				for page := 0; page < remoteFetchMaxPages; page++ {
					fctx, cancel := context.WithTimeout(ctx, remoteFetchCallTimeout)
					fres, ferr := runner.Fetch(fctx, proto.RemoteFetchParams{
						NamespaceID: ns.NamespaceID,
						BranchID:    br.BranchID,
						Since:       cursor,
						Limit:       remoteFetchPageLimit,
					})
					cancel()
					if ferr != nil {
						lg.Info("remote: fetch not ready (will retry)",
							"namespace", ns.NamespaceID, "branch", br.BranchID, "err", ferr)
						fetchErrored = true
						break
					}
					if len(fres.Events) > 0 {
						// Advance the resume cursor ONLY through events that were
						// durably consumed or intentionally dropped (Applied /
						// Deduped / Skipped / Rejected — and DeferredNeedsBaseline:
						// a lane=live conversation delta with an unknown parent is
						// deliberately dropped, since refetching can never make a
						// non-self-contained delta apply; recovery arrives via the
						// origin's retained-lane baseline instead, so stopping the
						// cursor on it would wedge the branch). On the FIRST
						// transient failure (ImportRetryable) STOP — leaving `last`
						// at the prior event so the next tick refetches the failed
						// one (FR-03.13 — no silent loss). A Retryable at index 0
						// keeps `last` at its prior cursor value (no advance).
						outcomes := importInbound(fres.Events)
						for i, ev := range fres.Events {
							if i < len(outcomes) && outcomes[i] == syncd.ImportRetryable {
								stoppedAtRetryable = true
								retryableEventID = ev.EventID
								if branchRetry[key] == nil {
									lg.Warn("remote: stopping backfill at failed inbound import",
										"namespace", ns.NamespaceID, "branch", br.BranchID,
										"event_id", ev.EventID, "reason", remoteFetchRetryableReason)
								}
								break
							}
							last = ev.EventID
						}
					}
					if stoppedAtRetryable {
						break
					}
					if fres.NextCursor == "" {
						break
					}
					cursor = fres.NextCursor
				}
				if stoppedAtRetryable {
					st := branchRetry[key]
					if st == nil || st.lastEventID != retryableEventID {
						st = &branchRetryState{lastEventID: retryableEventID}
						branchRetry[key] = st
					}
					st.failures++
					shift := st.failures - 1
					if shift > remoteFetchBackoffShiftCap {
						shift = remoteFetchBackoffShiftCap
					}
					delay := remoteFetchBackoffBase << uint(shift)
					if delay > remoteFetchBackoffMax {
						delay = remoteFetchBackoffMax
					}
					st.nextAttempt = time.Now().Add(delay)
					if st.failures == 1 || st.failures == remoteFetchWedgedThreshold {
						lg.Warn("remote: inbound import failing; backing off branch",
							"namespace", ns.NamespaceID, "branch", br.BranchID,
							"event_id", retryableEventID, "failures", st.failures,
							"retry_in", delay, "reason", remoteFetchRetryableReason)
					}
					if st.failures == remoteFetchWedgedThreshold && events != nil {
						events.Publish("remote.backfill_wedged", map[string]any{
							"namespace": ns.NamespaceID,
							"branch":    br.BranchID,
							"event_id":  retryableEventID,
							"failures":  st.failures,
						})
					}
				} else if !fetchErrored {
					// Clear retry state only when a pass genuinely completes.
					// A pass cut short by a Fetch error (relay reconnecting)
					// proves nothing about the previously-failing event — under
					// a flapping connection, resetting here would keep snapping
					// the exponential backoff to base and hold the failure
					// counter below the wedged threshold forever.
					delete(branchRetry, key)
				}
				if last != "" {
					cursors[key] = last
				}
			}
		}
	}

	var lastRepublish time.Time
	republishIfDue := func() {
		if republishRemoteHeads == nil {
			return
		}
		now := time.Now()
		if !lastRepublish.IsZero() && now.Sub(lastRepublish) < remoteSyncDriverRepublishInterval {
			return
		}
		lastRepublish = now
		n, rerr := republishRemoteHeads(ctx)
		if rerr != nil {
			lg.Warn("remote: republish local heads failed (will retry)", "err", rerr)
			return
		}
		if n > 0 {
			lg.Info("remote: republished changed local heads for retained sync", "count", n)
		}
	}

	enumerateFailureLogged := false
	tick := func() {
		if restartCount := runner.RestartCount(); restartCount != observedRestartCount {
			// A replacement child has empty process-local registration and
			// subscription state. Keep the cached daemon-owned public key, but
			// replay each plugin-owned binding exactly once for the new child.
			observedRestartCount = restartCount
			registered = false
			registerFailureLogged = false
			clear(subscribedNamespaces)
			subscribeFailureLogged = false
			enumerateFailureLogged = false
		}
		registerWrapKey()
		res, err := runner.Enumerate(ctx, proto.RemoteEnumerateParams{})
		if err != nil {
			if !enumerateFailureLogged {
				lg.Info("remote: enumerate not ready (will retry)", "err", err)
				enumerateFailureLogged = true
			}
			return
		}
		enumerateFailureLogged = false
		subscribe(res)
		reconcile(res)
		republishIfDue()
	}

	tick()
	t := time.NewTicker(remoteSyncDriverInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			tick()
		}
	}
}

// projectScanTick is the v0.62.0 BRD-02 §4.13.4 periodic auto-link
// worker body — called from the goroutine in serveCmd. Walks every
// configured root looking for VCS markers, cross-references detections
// with the current pending-project list, and for each match:
//   - adds the detected project to the local registry
//   - triggers a refanout pass so the project's previously-pending
//     artifacts materialize to the linked path
//
// Conservative: random repos the user clones outside any pending
// flow are NOT touched. Only repos whose canonical ID matches a
// pending project (artifacts already in the canonical store
// waiting) get auto-linked.
//
// Errors are logged and don't short-circuit the tick — best-effort
// throughout. Acceptable cost: a missed match this tick is picked up
// next tick.
func projectScanTick(
	ctx context.Context,
	lg interface {
		Info(msg string, args ...any)
		Warn(msg string, args ...any)
		Error(msg string, args ...any)
	},
	store *acf.Store,
	reg *project.Registry,
	orch *syncd.Orchestrator,
	roots []string,
	maxDepth int,
) {
	if err := ctx.Err(); err != nil {
		return
	}
	plist, err := pending.List(store, reg)
	if err != nil {
		lg.Error("project-scan: enumerate pending failed", "err", err)
		return
	}
	if len(plist) == 0 {
		// Nothing pending → no point walking the filesystem.
		return
	}
	detected, _ := pending.Scan(roots, maxDepth)
	matches := pending.MatchPending(detected, plist)
	if len(matches) == 0 {
		return
	}
	for _, m := range matches {
		entry := project.Entry{
			ID:          m.Info.ID,
			Path:        m.Path,
			VCS:         m.Info.VCS,
			DisplayName: filepath.Base(m.Path),
		}
		if _, exists := reg.Get(m.Info.ID); exists {
			// Already linked since the last refanout attempt — skip
			// (the regular orchestrator import path will pick up
			// future edits).
			continue
		}
		if err := reg.Add(entry); err != nil {
			lg.Warn("project-scan: registry add failed",
				"id", m.Info.ID, "path", m.Path, "err", err)
			continue
		}
		n, ferr := orch.RefanOutByProject(m.Info.ID)
		if ferr != nil {
			lg.Warn("project-scan: refanout failed",
				"id", m.Info.ID, "err", ferr)
			continue
		}
		lg.Info("project-scan: auto-linked pending project",
			"id", m.Info.ID, "path", m.Path, "materialized", n)
	}
}

// serveCtxHolder bridges the lifetime gap between when the web handlers
// are CONSTRUCTED (in the outer serve scope, before the daemon's
// long-lived run context exists) and when that context becomes
// available (inside runBody, which derives it from the parent
// signal/SCM-aware context). The ProjectsHandler's onRegister callback
// must start its folder watcher on the LONG-LIVED context — never the
// per-request HTTP context, which is cancelled the instant the request
// returns and would immediately kill the new watcher.
//
// runBody calls set() once it owns the run context; the onRegister
// closure calls get() lazily at request time (long after set ran, since
// the web listener only starts inside runBody too). The mutex keeps the
// set→get handoff race-free even though, in practice, set always
// happens-before the first get.
type serveCtxHolder struct {
	mu  sync.RWMutex
	ctx context.Context
}

func (h *serveCtxHolder) set(ctx context.Context) {
	h.mu.Lock()
	h.ctx = ctx
	h.mu.Unlock()
}

func (h *serveCtxHolder) get() context.Context {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.ctx
}

// cloudIdentityMu serializes every propagation of the cloud device identity,
// and — in the RefreshIdentity hook — the --status query the propagated id is
// derived from. Serializing only the writes would not be enough: the hook
// runs on its own goroutine per plugin reconnection, so a stale query result
// could land after a newer apply and leave the fleet stamping a retired id.
var cloudIdentityMu sync.Mutex

// applyCloudDeviceIdentity propagates a (re-)paired cloud device id to every
// component that stamps or compares it: the runner (plugin handshakes + lazy
// deviceIDFn readers), the orchestrator (outbound Origin, inbound loop
// prevention, cross-device discrimination), each adapter's event provenance,
// and the control-socket status response (which CLI adapter construction
// reads via cliCloudDeviceID before authoring events — a status frozen at the
// boot seed would hand every `aplexica import` the retired identity). The
// orchestrator setter logs the old→new rotation at WARN. An empty id is a
// no-op: an unpaired/unreachable plugin must not erase a known identity
// mid-flight.
func applyCloudDeviceIdentity(id string, runner *daemon.RemoteRunner, orch *syncd.Orchestrator, adapters []adapter.Adapter, status *daemon.ControlServer) {
	cloudIdentityMu.Lock()
	defer cloudIdentityMu.Unlock()
	applyCloudDeviceIdentityLocked(id, runner, orch, adapters, status)
}

// applyCloudDeviceIdentityLocked is applyCloudDeviceIdentity for callers
// already holding cloudIdentityMu (the RefreshIdentity hook, whose identity
// query must share the critical section with the apply).
func applyCloudDeviceIdentityLocked(id string, runner *daemon.RemoteRunner, orch *syncd.Orchestrator, adapters []adapter.Adapter, status *daemon.ControlServer) {
	if id == "" {
		return
	}
	if runner != nil {
		runner.SetDeviceID(id)
	}
	if orch != nil {
		orch.SetLocalDeviceID(id)
	}
	for _, ad := range adapters {
		if s, ok := ad.(adapter.DeviceIDSetter); ok {
			s.SetDeviceID(id)
		}
	}
	if status != nil {
		status.SetLocalDeviceID(id)
	}
}

func installedAdaptersFrom(adapters []adapter.Adapter, discoveries map[string]adapter.Discovery) []adapter.Adapter {
	out := make([]adapter.Adapter, 0, len(adapters))
	for _, ad := range adapters {
		if discoveries[ad.Name()].Installed {
			out = append(out, ad)
		}
	}
	return out
}

// runtimeAdaptersFrom keeps positively discovered adapters plus adapters that
// explicitly support late installation. Runtime-discoverable adapters are
// availability-checked again by the orchestrator before every outbound write,
// so retaining a startup-negative Claude/Codex adapter cannot create native
// state for an agent that is still absent.
func runtimeAdaptersFrom(adapters []adapter.Adapter, discoveries map[string]adapter.Discovery) []adapter.Adapter {
	out := make([]adapter.Adapter, 0, len(adapters))
	for _, ad := range adapters {
		if discoveries[ad.Name()].Installed {
			out = append(out, ad)
			continue
		}
		if _, ok := ad.(adapter.RuntimeDiscoverable); ok {
			out = append(out, ad)
		}
	}
	return out
}

func mergeDiscoveryRoots(actual, candidate adapter.Discovery) adapter.Discovery {
	actual.GlobalRoots = appendUniquePaths(actual.GlobalRoots, candidate.GlobalRoots...)
	actual.RecursiveRoots = appendUniquePaths(actual.RecursiveRoots, candidate.RecursiveRoots...)
	actual.MetadataRoots = appendUniquePaths(actual.MetadataRoots, candidate.MetadataRoots...)
	actual.WatchFiles = appendUniquePaths(actual.WatchFiles, candidate.WatchFiles...)
	return actual
}

func appendUniquePaths(paths []string, candidates ...string) []string {
	seen := make(map[string]struct{}, len(paths)+len(candidates))
	for _, path := range paths {
		if path != "" {
			seen[filepath.Clean(path)] = struct{}{}
		}
	}
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		clean := filepath.Clean(candidate)
		if _, duplicate := seen[clean]; duplicate {
			continue
		}
		seen[clean] = struct{}{}
		paths = append(paths, clean)
	}
	return paths
}

// harvestSourcesFrom returns the subset of adapters that can report the
// directories they've run in (adapter.ProjectDirSource — claude-code,
// codex). The result feeds projectdiscovery.Harvest so the pending list
// can surface as-yet-unregistered folders the user has actually worked
// in. Adapters that don't implement the capability contribute nothing.
func harvestSourcesFrom(adapters []adapter.Adapter) []projectdiscovery.HarvestSource {
	var out []projectdiscovery.HarvestSource
	for _, ad := range adapters {
		if src, ok := ad.(projectdiscovery.HarvestSource); ok {
			out = append(out, src)
		}
	}
	return out
}
