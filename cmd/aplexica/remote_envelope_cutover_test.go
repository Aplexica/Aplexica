package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/aplexica/aplexica/internal/daemon"
	"github.com/stretchr/testify/require"
)

func TestRemoteEnvelopeV2CutoverAllowsOnlyNeverMigratedAccountOverlap(t *testing.T) {
	root := filepath.Join(t.TempDir(), "identity")
	provider := daemon.NewVerifiedRosterProvider(root)

	// No signed roster or epoch was ever staged: this is the explicit legacy
	// overlap state for an existing account awaiting migration.
	require.False(t, remoteEnvelopeV2CutoverRequired(context.Background(), root, provider))

	require.NoError(t, os.MkdirAll(filepath.Join(root, "account"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(root, "account", "security-epoch.json"), []byte("partial"), 0o600))

	// Once cutover state exists, even incomplete/corrupt state cannot cause a
	// downgrade. Publication remains v2 and fails closed until state is repaired.
	require.True(t, remoteEnvelopeV2CutoverRequired(context.Background(), root, provider))
}

func TestRemoteEnvelopeV2CutoverFailsClosedOnUnreadableIdentityPath(t *testing.T) {
	root := filepath.Join(t.TempDir(), "identity-file")
	require.NoError(t, os.WriteFile(root, []byte("not a directory"), 0o600))
	require.True(t, remoteEnvelopeV2CutoverRequired(context.Background(), root, nil))
}
