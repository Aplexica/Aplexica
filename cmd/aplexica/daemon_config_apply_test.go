package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

// makeDaemonServeCmd constructs a minimal cobra.Command with the same
// flag set the daemon's serve command exposes (the relevant subset
// for testing the config-package apply path). Keeps the test
// hermetic against init() reordering.
func makeDaemonServeCmd(t *testing.T) *cobra.Command {
	t.Helper()
	c := &cobra.Command{Use: "serve"}
	c.Flags().DurationVar(&daemonProjectScanInterval, "project-scan-interval", 60*time.Minute, "")
	c.Flags().StringSliceVar(&daemonProjectScanRoots, "project-scan-roots", nil, "")
	c.Flags().IntVar(&daemonProjectScanMaxDepth, "project-scan-max-depth", 6, "")
	c.Flags().StringSliceVarP(&daemonCLISets, "config-set", "c", nil, "")
	return c
}

func TestApplyDaemonConfigPackage_ShippedDefaults(t *testing.T) {
	// Reset to a known starting state.
	daemonProjectScanInterval = 0
	daemonProjectScanMaxDepth = 0
	daemonProjectScanRoots = nil
	daemonClaudeSessionScanWindow = 0
	daemonCLISets = nil
	t.Cleanup(func() {
		daemonProjectScanInterval = 0
		daemonProjectScanMaxDepth = 0
		daemonProjectScanRoots = nil
		daemonClaudeSessionScanWindow = 0
		daemonCLISets = nil
	})

	c := makeDaemonServeCmd(t)
	c.SetArgs([]string{}) // no flags

	var stderr bytes.Buffer
	c.SetErr(&stderr)
	require.NoError(t, c.ParseFlags([]string{}))

	require.NoError(t, applyDaemonConfigPackage(c))

	// Shipped default: 60m / 6.
	require.Equal(t, 60*time.Minute, daemonProjectScanInterval)
	require.Equal(t, 6, daemonProjectScanMaxDepth)
}

func TestApplyDaemonConfigPackage_UserOverridesShipped(t *testing.T) {
	tmp := t.TempDir()
	// Point HOME at tmp so the user-layer file is under our control.
	t.Setenv("HOME", tmp)
	// Windows: os.UserHomeDir() consults USERPROFILE, not HOME.
	t.Setenv("USERPROFILE", tmp)
	require.NoError(t, os.MkdirAll(filepath.Join(tmp, ".aplexica"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(tmp, ".aplexica", "config.toml"),
		[]byte(`[daemon]
project_scan_interval = "30m"
project_scan_max_depth = 10
project_scan_roots = ["/tmp/code"]
`), 0o644))

	daemonProjectScanInterval = 0
	daemonProjectScanMaxDepth = 0
	daemonProjectScanRoots = nil
	daemonClaudeSessionScanWindow = 0
	daemonCLISets = nil
	t.Cleanup(func() {
		daemonProjectScanInterval = 0
		daemonProjectScanMaxDepth = 0
		daemonProjectScanRoots = nil
		daemonClaudeSessionScanWindow = 0
		daemonCLISets = nil
	})

	c := makeDaemonServeCmd(t)
	require.NoError(t, c.ParseFlags([]string{}))
	require.NoError(t, applyDaemonConfigPackage(c))

	require.Equal(t, 30*time.Minute, daemonProjectScanInterval)
	require.Equal(t, 10, daemonProjectScanMaxDepth)
	require.Equal(t, []string{"/tmp/code"}, daemonProjectScanRoots)
}

func TestApplyDaemonConfigPackage_CLIBeatsUser(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	// Windows: os.UserHomeDir() consults USERPROFILE, not HOME.
	t.Setenv("USERPROFILE", tmp)
	require.NoError(t, os.MkdirAll(filepath.Join(tmp, ".aplexica"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(tmp, ".aplexica", "config.toml"),
		[]byte(`[daemon]
project_scan_interval = "30m"
`), 0o644))

	daemonProjectScanInterval = 0
	daemonClaudeSessionScanWindow = 0
	daemonCLISets = nil
	t.Cleanup(func() {
		daemonProjectScanInterval = 0
		daemonClaudeSessionScanWindow = 0
		daemonCLISets = nil
	})

	c := makeDaemonServeCmd(t)
	require.NoError(t, c.ParseFlags([]string{
		"-c", "daemon.project_scan_interval=5m",
	}))
	require.NoError(t, applyDaemonConfigPackage(c))

	require.Equal(t, 5*time.Minute, daemonProjectScanInterval,
		"--config-set must beat the user TOML")
}

func TestApplyDaemonConfigPackage_ExplicitFlagBeatsConfig(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	// Windows: os.UserHomeDir() consults USERPROFILE, not HOME.
	t.Setenv("USERPROFILE", tmp)
	require.NoError(t, os.MkdirAll(filepath.Join(tmp, ".aplexica"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(tmp, ".aplexica", "config.toml"),
		[]byte(`[daemon]
project_scan_interval = "30m"
`), 0o644))

	daemonProjectScanInterval = 0
	daemonClaudeSessionScanWindow = 0
	daemonCLISets = nil
	t.Cleanup(func() {
		daemonProjectScanInterval = 0
		daemonClaudeSessionScanWindow = 0
		daemonCLISets = nil
	})

	c := makeDaemonServeCmd(t)
	require.NoError(t, c.ParseFlags([]string{
		"--project-scan-interval", "1m",
	}))
	require.NoError(t, applyDaemonConfigPackage(c))

	require.Equal(t, time.Minute, daemonProjectScanInterval,
		"explicit --flag must beat config (cobra Changed() check)")
}

func TestApplyDaemonConfigPackage_WarningsOnUnknownKey(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	// Windows: os.UserHomeDir() consults USERPROFILE, not HOME.
	t.Setenv("USERPROFILE", tmp)
	require.NoError(t, os.MkdirAll(filepath.Join(tmp, ".aplexica"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(tmp, ".aplexica", "config.toml"),
		[]byte(`[daemon]
project_scan_interval = "30m"
typo_key = 42
`), 0o644))

	daemonProjectScanInterval = 0
	daemonClaudeSessionScanWindow = 0
	daemonCLISets = nil
	t.Cleanup(func() {
		daemonProjectScanInterval = 0
		daemonClaudeSessionScanWindow = 0
		daemonCLISets = nil
	})

	c := makeDaemonServeCmd(t)
	var stderr bytes.Buffer
	c.SetErr(&stderr)
	require.NoError(t, c.ParseFlags([]string{}))
	require.NoError(t, applyDaemonConfigPackage(c),
		"unknown keys must NOT fail startup (BRD-10 §10.2)")

	require.Contains(t, stderr.String(), "WARN")
	require.Contains(t, stderr.String(), "typo_key")
}

func TestDurationCanonicalForDaemon(t *testing.T) {
	require.Equal(t, "168h", durationCanonicalForDaemon("7d"))
	require.Equal(t, "720h", durationCanonicalForDaemon("30d"))
	require.Equal(t, "60m", durationCanonicalForDaemon("60m"))
}

// limits.max_artifact_size_mb (BRD-03 §4.3/§5) → daemonMaxArtifactBytes.
func TestApplyDaemonConfigPackage_MaxArtifactSize(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)
	require.NoError(t, os.MkdirAll(filepath.Join(tmp, ".aplexica"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(tmp, ".aplexica", "config.toml"),
		[]byte("[limits]\nmax_artifact_size_mb = 512\n"), 0o644))

	daemonMaxArtifactBytes = 0
	daemonCLISets = nil
	t.Cleanup(func() { daemonMaxArtifactBytes = 0; daemonCLISets = nil })

	c := makeDaemonServeCmd(t)
	c.SetArgs([]string{})
	require.NoError(t, c.ParseFlags([]string{}))
	require.NoError(t, applyDaemonConfigPackage(c))
	require.Equal(t, int64(512)<<20, daemonMaxArtifactBytes,
		"user-layer MB value must reach the orchestrator cap in bytes")
}

func TestApplyDaemonConfigPackage_MaxArtifactSizeDisabled(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)
	require.NoError(t, os.MkdirAll(filepath.Join(tmp, ".aplexica"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(tmp, ".aplexica", "config.toml"),
		[]byte("[limits]\nmax_artifact_size_mb = -1\n"), 0o644))

	daemonMaxArtifactBytes = 0
	daemonCLISets = nil
	t.Cleanup(func() { daemonMaxArtifactBytes = 0; daemonCLISets = nil })

	c := makeDaemonServeCmd(t)
	c.SetArgs([]string{})
	require.NoError(t, c.ParseFlags([]string{}))
	require.NoError(t, applyDaemonConfigPackage(c))
	require.Equal(t, int64(-1), daemonMaxArtifactBytes,
		"-1 must disable the cap (orchestrator treats negative as no-cap)")
}

// limits.max_session_file_mb maps to daemonMaxSessionFileBytes.
// Agent session transcripts get their own, larger ingest cap so multi-week
// Claude/Codex sessions keep syncing without raising the generic artifact cap.
func TestApplyDaemonConfigPackage_MaxSessionFileSize(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)
	require.NoError(t, os.MkdirAll(filepath.Join(tmp, ".aplexica"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(tmp, ".aplexica", "config.toml"),
		[]byte("[limits]\nmax_session_file_mb = 1024\n"), 0o644))

	daemonMaxSessionFileBytes = 0
	daemonCLISets = nil
	t.Cleanup(func() { daemonMaxSessionFileBytes = 0; daemonCLISets = nil })

	c := makeDaemonServeCmd(t)
	c.SetArgs([]string{})
	require.NoError(t, c.ParseFlags([]string{}))
	require.NoError(t, applyDaemonConfigPackage(c))
	require.Equal(t, int64(1024)<<20, daemonMaxSessionFileBytes,
		"user-layer MB value must reach the orchestrator session cap in bytes")
}

func TestApplyDaemonConfigPackage_MaxSessionFileSizeShippedDefault(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)

	daemonMaxSessionFileBytes = 0
	daemonCLISets = nil
	t.Cleanup(func() { daemonMaxSessionFileBytes = 0; daemonCLISets = nil })

	c := makeDaemonServeCmd(t)
	c.SetArgs([]string{})
	require.NoError(t, c.ParseFlags([]string{}))
	require.NoError(t, applyDaemonConfigPackage(c))
	require.Equal(t, int64(512)<<20, daemonMaxSessionFileBytes,
		"shipped default (512 MB) must drive the session cap with no user config")
}

func TestApplyDaemonConfigPackage_MaxSessionFileSizeDisabled(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)
	require.NoError(t, os.MkdirAll(filepath.Join(tmp, ".aplexica"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(tmp, ".aplexica", "config.toml"),
		[]byte("[limits]\nmax_session_file_mb = -1\n"), 0o644))

	daemonMaxSessionFileBytes = 0
	daemonCLISets = nil
	t.Cleanup(func() { daemonMaxSessionFileBytes = 0; daemonCLISets = nil })

	c := makeDaemonServeCmd(t)
	c.SetArgs([]string{})
	require.NoError(t, c.ParseFlags([]string{}))
	require.NoError(t, applyDaemonConfigPackage(c))
	require.Equal(t, int64(-1), daemonMaxSessionFileBytes,
		"-1 must disable the session cap (orchestrator treats negative as no-cap)")
}
