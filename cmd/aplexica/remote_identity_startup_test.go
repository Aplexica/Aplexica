package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/aplexica/aplexica/internal/securityepoch"
	"github.com/stretchr/testify/require"
)

func TestRemoteIdentityStartupRejectsJournalBeforePluginExecution(t *testing.T) {
	identityRoot := filepath.Join(t.TempDir(), "identity")
	accountRoot := filepath.Join(identityRoot, "account")
	require.NoError(t, os.MkdirAll(accountRoot, 0o700))
	require.NoError(t, os.WriteFile(
		filepath.Join(accountRoot, securityepoch.TransitionJournalFilename),
		[]byte(`{"version":1,"corrupt":true}`), 0o600,
	))

	pluginExecuted := false
	startup, _, err := recoverRemoteIdentityStartup(context.Background(), identityRoot)
	if err == nil {
		// This mirrors the daemon's control flow: plugin verification/status is
		// reachable only after recovery returns a startup token.
		pluginExecuted = true
	}
	require.Error(t, err)
	require.Nil(t, startup)
	require.False(t, pluginExecuted, "no plugin process may execute before journal recovery succeeds")
}

func TestRemoteIdentityStartupRejectsCorruptNamespaceTransitionBeforePluginExecution(t *testing.T) {
	identityRoot := filepath.Join(t.TempDir(), "identity")
	namespaceRoot := filepath.Join(identityRoot, "namespaces", "01975a6a-4100-7000-8000-000000000001")
	require.NoError(t, os.MkdirAll(namespaceRoot, 0o700))
	require.NoError(t, os.WriteFile(
		filepath.Join(namespaceRoot, securityepoch.TransitionJournalFilename),
		[]byte(`{"version":1,"phase":"staged","corrupt":true}`), 0o600,
	))

	startup, _, err := recoverRemoteIdentityStartup(context.Background(), identityRoot)
	require.ErrorContains(t, err, "validate pending namespace device transition")
	require.Nil(t, startup)
}

func TestRemoteIdentityStartupReturnsCoordinatorOnlyAfterCleanRecovery(t *testing.T) {
	identityRoot := filepath.Join(t.TempDir(), "identity")
	require.NoError(t, os.MkdirAll(identityRoot, 0o700))
	startup, recovered, err := recoverRemoteIdentityStartup(context.Background(), identityRoot)
	require.NoError(t, err)
	require.False(t, recovered)
	require.NotNil(t, startup)
	require.NotNil(t, startup.coordinator)
	require.Equal(t, identityRoot, startup.coordinator.Root)
}
