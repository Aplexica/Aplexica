package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestResolveTrayPathPrefersSibling(t *testing.T) {
	dir := t.TempDir()
	aplexicaPath := filepath.Join(dir, "aplexica")
	trayPath := filepath.Join(dir, trayBinaryName())
	require.NoError(t, os.WriteFile(aplexicaPath, []byte(""), 0o755))
	require.NoError(t, os.WriteFile(trayPath, []byte(""), 0o755))

	got, err := resolveTrayPath(aplexicaPath)
	require.NoError(t, err)
	trayPath, err = filepath.EvalSymlinks(trayPath)
	require.NoError(t, err)
	require.Equal(t, trayPath, got)
}

func TestTrayOptionsBakeExplicitDaemonPaths(t *testing.T) {
	oldState, oldLog := daemonStateDir, daemonLogDir
	daemonStateDir = filepath.Join("state", "dir")
	daemonLogDir = filepath.Join("log", "dir")
	t.Cleanup(func() {
		daemonStateDir = oldState
		daemonLogDir = oldLog
	})

	opts := trayOptions(filepath.Join("bin", trayBinaryName()), filepath.Join("bin", "aplexica"))
	require.Equal(t, filepath.Join("bin", trayBinaryName()), opts.TrayPath)
	require.Equal(t, filepath.Join("bin", "aplexica"), opts.AplexicaPath)
	require.Equal(t, filepath.Join("state", "dir"), opts.StateDir)
	require.Equal(t, filepath.Join("log", "dir"), opts.LogDir)
}

func TestTrayLaunchArgsIncludeStateAndLogDirs(t *testing.T) {
	oldState, oldLog := daemonStateDir, daemonLogDir
	daemonStateDir = "/tmp/state"
	daemonLogDir = "/tmp/logs"
	t.Cleanup(func() {
		daemonStateDir = oldState
		daemonLogDir = oldLog
	})

	require.Equal(t, []string{
		"--aplexica", "/opt/aplexica/bin/aplexica",
		"--state-dir", "/tmp/state",
		"--log-dir", "/tmp/logs",
	}, trayLaunchArgs("/opt/aplexica/bin/aplexica"))
}
