package main

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDefaultDaemonWatchDirUsesHome(t *testing.T) {
	oldDir := daemonDir
	daemonDir = ""
	t.Setenv("HOME", "/tmp/aplexica-test-home")
	t.Setenv("USERPROFILE", "/tmp/aplexica-test-home")
	t.Cleanup(func() { daemonDir = oldDir })

	require.NoError(t, defaultDaemonWatchDir())
	require.Equal(t, "/tmp/aplexica-test-home", daemonDir)
}

func TestDaemonDirIsRequiredOnlyForServe(t *testing.T) {
	require.NoError(t, daemonStartCmd.ValidateRequiredFlags())
	require.NoError(t, daemonInstallCmd.ValidateRequiredFlags())
	require.ErrorContains(t, daemonServeCmd.ValidateRequiredFlags(), `required flag(s) "dir" not set`)
}
