package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aplexica/aplexica/internal/config"
	"github.com/spf13/cobra"
)

// tomlReloadMu serializes BRD-10 §10.1 TOML-layer reload passes. The
// SIGHUP-TOML handler (cmd_daemon_toml_reload_unix.go) and the
// control-socket reloader (ctlSrv.SetReloader in cmd_daemon.go, invoked
// from a per-connection `go s.handleConn` with no lock around the
// callback) can both call reloadDaemonConfigPackage concurrently, and a
// `kill -HUP` can race a concurrent `aplexica daemon reload`. That path
// writes the package globals daemonProjectScanInterval/MaxDepth/Roots
// and reads daemonProjectScanRoots (a slice header) in the pre/post
// snapshots, so without serialization `go test -race` flags a data race.
//
// This mirrors jsonReloadMu in daemon_json_reload.go, which already
// guards the parallel config.json reload path (applyJSONConfigReload).
var tomlReloadMu sync.Mutex

// applyDaemonConfigPackage layers the BRD-10 §10.1 TOML-based config
// system on top of the v0.27.0 daemon.LoadConfig(json) path. It loads
// the merged effective config (shipped → system → user → project →
// env → --config-set) and, for every CLI flag the user did NOT
// explicitly set, overrides the in-memory variable with the config
// value.
//
// This is the v0.74.0 wiring that makes the documented defaults.toml
// keys actually drive the daemon's runtime behavior (per FR-10.6 /
// FR-10.7: every tunable parameter MUST originate in a configuration
// layer).
//
// daemonCLISets is populated by a new `--config-set key=value`
// repeatable flag on the serve / start commands (layer 6).
var daemonCLISets []string

func applyDaemonConfigPackage(cmd *cobra.Command) error {
	sys, usr, _ := config.DefaultSources()
	// Project layer: when daemonDir looks like a project root with a
	// <daemonDir>/.aplexica/config.toml file, consult it. Defensive:
	// any read error in that path is a silent skip (the file is
	// optional per BRD-10 §10.1).
	projectPath := ""
	if daemonDir != "" {
		candidate := daemonDir + "/.aplexica/config.toml"
		if _, err := os.Stat(candidate); err == nil {
			projectPath = candidate
		}
	}

	eff, err := config.Load(config.LoadOptions{
		SystemPath:   sys,
		UserPath:     usr,
		ProjectPath:  projectPath,
		Env:          os.Environ(),
		CLIOverrides: daemonCLISets,
	})
	if err != nil {
		return err
	}

	// Range-check; surface warnings to stderr but never fail startup
	// on warnings (BRD-10 §10.2 — unknown keys are a warning).
	errs, warns := config.SchemaValidate(eff)
	for _, w := range warns {
		cmd.ErrOrStderr().Write([]byte("config WARN: " + w + "\n"))
	}
	for _, e := range errs {
		cmd.ErrOrStderr().Write([]byte("config ERR : " + e + "\n"))
	}

	// overrideIfUnset applies the config value when the cobra flag at
	// `flagName` was NOT explicitly Changed by the user. `configKey` is
	// the dotted-key shape stored in the effective config (e.g.
	// "daemon.project_scan_interval"); `flagName` is the kebab-case
	// cobra flag (e.g. "project-scan-interval").
	overrideIfUnset := func(flagName, configKey string, apply func(string)) {
		if cmd.Flags().Changed(flagName) {
			return
		}
		v, _, ok := eff.Get(configKey)
		if !ok {
			return
		}
		apply(v)
	}

	parseDur := func(s string) (time.Duration, bool) {
		d, err := time.ParseDuration(durationCanonicalForDaemon(s))
		if err != nil {
			return 0, false
		}
		return d, true
	}

	// daemon.project_scan_interval → --project-scan-interval
	overrideIfUnset("project-scan-interval", "daemon.project_scan_interval", func(v string) {
		if d, ok := parseDur(v); ok {
			daemonProjectScanInterval = d
		}
	})
	// daemon.project_scan_max_depth → --project-scan-max-depth
	overrideIfUnset("project-scan-max-depth", "daemon.project_scan_max_depth", func(v string) {
		if n, err := strconv.Atoi(v); err == nil {
			daemonProjectScanMaxDepth = n
		}
	})
	// daemon.project_scan_roots → --project-scan-roots
	overrideIfUnset("project-scan-roots", "daemon.project_scan_roots", func(v string) {
		// Stored as TOML-array-rendered string, e.g. "[]" or "[a, b]".
		v = strings.TrimSpace(v)
		v = strings.TrimPrefix(v, "[")
		v = strings.TrimSuffix(v, "]")
		v = strings.TrimSpace(v)
		if v == "" {
			daemonProjectScanRoots = nil
			return
		}
		parts := strings.Split(v, ",")
		for i := range parts {
			parts[i] = strings.TrimSpace(parts[i])
		}
		daemonProjectScanRoots = parts
	})
	if v, _, ok := eff.Get("daemon.claude_session_scan_window"); ok {
		if d, ok := parseDur(v); ok {
			daemonClaudeSessionScanWindow = d
		}
	}
	// limits.max_artifact_size_mb → orchestrator Config.MaxArtifactBytes.
	// 0 keeps the orchestrator's built-in 64 MiB default; negative disables
	// the cap (the orchestrator treats any negative value as "no cap").
	if v, _, ok := eff.Get("limits.max_artifact_size_mb"); ok {
		if b, ok := daemonSizeCapBytes(v); ok {
			daemonMaxArtifactBytes = b
		}
	}
	// limits.max_session_file_mb → orchestrator Config.MaxSessionFileBytes:
	// the separate, larger ingest cap for agent session transcripts
	// (Claude/Codex). 0 keeps the orchestrator's built-in 512 MiB default;
	// negative disables the session cap.
	if v, _, ok := eff.Get("limits.max_session_file_mb"); ok {
		if b, ok := daemonSizeCapBytes(v); ok {
			daemonMaxSessionFileBytes = b
		}
	}

	return nil
}

// mbShift converts a `[limits] *_mb` config count to bytes (1 MB = 1<<20).
const mbShift = 20

// daemonSizeCapBytes resolves a `[limits] *_mb` config value into the
// orchestrator's byte convention: negative → -1 (cap disabled), zero → 0
// (keep the orchestrator's built-in default), positive → MB shifted to
// bytes. Not-a-number returns ok=false (leave the current value alone,
// matching how the other apply paths skip unparseable values).
func daemonSizeCapBytes(v string) (int64, bool) {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return 0, false
	}
	switch {
	case n < 0:
		return -1, true
	case n == 0:
		return 0, true
	default:
		return int64(n) << mbShift, true
	}
}

// reloadDaemonConfigPackage is invoked by the SIGHUP handler (Unix) and
// the control-socket "reload" command (cross-platform) to re-pull
// every documented tunable from the BRD-10 §10.1 layers and re-apply
// to the running daemon's in-memory variables. Returns a short
// human-readable report.
//
// Per FR-10.8, parameters whose schema entry carries
// RestartRequired = true are NOT applied live — they're logged as
// "needs restart" with the old vs. new value. For v0.75.0 every
// daemon.* tunable is marked restart_required because the project-
// scan goroutine captures its parameters at startup; hot-reloadable
// per-subsystem wiring will come incrementally as each subsystem
// (retention, tray, conflicts) gains a config-driven setter.
func reloadDaemonConfigPackage(cmd *cobra.Command) (string, error) {
	// Serialize the whole pre-snapshot/apply/post-snapshot sequence
	// against itself and the control-socket reloader (both reach this
	// function concurrently): applyDaemonConfigPackage writes the
	// package-global daemon vars while the snapshots read them.
	tomlReloadMu.Lock()
	defer tomlReloadMu.Unlock()

	// Snapshot the current values BEFORE re-applying so we can diff.
	pre := daemonRuntimeSnapshot()

	if err := applyDaemonConfigPackage(cmd); err != nil {
		return "", err
	}

	post := daemonRuntimeSnapshot()
	return diffDaemonRuntime(pre, post), nil
}

// daemonRuntimeSnapshot captures the package-global daemon variables
// that the TOML config can drive. Pre/post comparison reports what
// the reload would change.
type daemonRuntimeState struct {
	ProjectScanInterval     time.Duration
	ProjectScanMaxDepth     int
	ProjectScanRoots        []string
	ClaudeSessionScanWindow time.Duration
	MaxArtifactBytes        int64
	MaxSessionFileBytes     int64
}

func daemonRuntimeSnapshot() daemonRuntimeState {
	roots := make([]string, len(daemonProjectScanRoots))
	copy(roots, daemonProjectScanRoots)
	return daemonRuntimeState{
		ProjectScanInterval:     daemonProjectScanInterval,
		ProjectScanMaxDepth:     daemonProjectScanMaxDepth,
		ProjectScanRoots:        roots,
		ClaudeSessionScanWindow: daemonClaudeSessionScanWindow,
		MaxArtifactBytes:        daemonMaxArtifactBytes,
		MaxSessionFileBytes:     daemonMaxSessionFileBytes,
	}
}

func diffDaemonRuntime(pre, post daemonRuntimeState) string {
	var b strings.Builder
	b.WriteString("config reload report:\n")
	changes := 0
	if pre.ProjectScanInterval != post.ProjectScanInterval {
		b.WriteString("  daemon.project_scan_interval " +
			pre.ProjectScanInterval.String() + " → " +
			post.ProjectScanInterval.String() +
			" (restart_required: takes effect on next daemon start)\n")
		changes++
	}
	if pre.ProjectScanMaxDepth != post.ProjectScanMaxDepth {
		b.WriteString("  daemon.project_scan_max_depth " +
			strconv.Itoa(pre.ProjectScanMaxDepth) + " → " +
			strconv.Itoa(post.ProjectScanMaxDepth) +
			" (restart_required: takes effect on next daemon start)\n")
		changes++
	}
	if !stringSlicesEqual(pre.ProjectScanRoots, post.ProjectScanRoots) {
		b.WriteString("  daemon.project_scan_roots changed (" +
			strconv.Itoa(len(pre.ProjectScanRoots)) + " → " +
			strconv.Itoa(len(post.ProjectScanRoots)) + " entries)" +
			" (restart_required: takes effect on next daemon start)\n")
		changes++
	}
	if pre.ClaudeSessionScanWindow != post.ClaudeSessionScanWindow {
		b.WriteString("  daemon.claude_session_scan_window " +
			pre.ClaudeSessionScanWindow.String() + " → " +
			post.ClaudeSessionScanWindow.String() +
			" (restart_required: takes effect on next daemon start)\n")
		changes++
	}
	if pre.MaxArtifactBytes != post.MaxArtifactBytes {
		writeByteCapDiff(&b, "limits.max_artifact_size_mb",
			pre.MaxArtifactBytes, post.MaxArtifactBytes)
		changes++
	}
	if pre.MaxSessionFileBytes != post.MaxSessionFileBytes {
		writeByteCapDiff(&b, "limits.max_session_file_mb",
			pre.MaxSessionFileBytes, post.MaxSessionFileBytes)
		changes++
	}
	if changes == 0 {
		b.WriteString("  (no changes; effective config matches in-memory state)\n")
	}
	return b.String()
}

// writeByteCapDiff renders one restart-required `[limits]` cap change line
// for the reload report (values are the resolved byte counts, not the
// config-file MB).
func writeByteCapDiff(b *strings.Builder, key string, pre, post int64) {
	fmt.Fprintf(b, "  %s %d → %d bytes (restart_required: takes effect on next daemon start)\n",
		key, pre, post)
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// readDaemonConfigKey returns the merged-effective value for a config
// key, honoring the same layer precedence applyDaemonConfigPackage
// uses. Helper for one-shot lookups (e.g. "is metrics.enabled true at
// daemon-start time?") where building Effective from scratch is
// overkill. Empty layer return ("") indicates the key isn't set.
func readDaemonConfigKey(cmd *cobra.Command, key string) (value string, layer string, ok bool) {
	sys, usr, _ := config.DefaultSources()
	projectPath := ""
	if daemonDir != "" {
		candidate := daemonDir + "/.aplexica/config.toml"
		if _, err := os.Stat(candidate); err == nil {
			projectPath = candidate
		}
	}
	eff, err := config.Load(config.LoadOptions{
		SystemPath:   sys,
		UserPath:     usr,
		ProjectPath:  projectPath,
		Env:          os.Environ(),
		CLIOverrides: daemonCLISets,
	})
	if err != nil {
		return "", "", false
	}
	v, lr, ok := eff.Get(key)
	if !ok {
		return "", "", false
	}
	return v, lr.String(), true
}

// durationCanonicalForDaemon mirrors internal/config.durationCanonical
// so the daemon can parse "7d" / "30d" the same way `config validate`
// does. Kept as a local helper to avoid widening internal/config's
// exported API surface.
func durationCanonicalForDaemon(s string) string {
	if strings.HasSuffix(s, "d") {
		n := strings.TrimSuffix(s, "d")
		if v, err := strconv.Atoi(n); err == nil {
			return strconv.Itoa(v*24) + "h"
		}
	}
	return s
}
