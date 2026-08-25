package generationactivation

import (
	"crypto/sha256"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/aplexica/aplexica/internal/privatefs"
	"github.com/stretchr/testify/require"
)

func TestLoadSecurityEpochIsStrictAndReadOnly(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, privatefs.EnsureDir(dir, privatefs.DirPolicy{Access: privatefs.AccessPrivate, RepairOwned: true}))
	path := filepath.Join(dir, "security-epoch.json")
	state := SecurityEpochState{
		Version: 1, ScopeType: "account", ScopeID: "scope-a", RosterHash: sha256.Sum256([]byte("roster")),
		AccessGeneration: 1, AccessSetHash: sha256.Sum256([]byte("access")), BarrierID: sha256.Sum256([]byte("barrier")),
		TreeHeadDigest: sha256.Sum256([]byte("tree")), KeyMode: "recipient-wrap-v2", CoordinatorGeneration: 1,
	}
	raw, err := json.Marshal(state)
	require.NoError(t, err)
	root, err := privatefs.OpenRoot(dir, privatefs.DirPolicy{Access: privatefs.AccessPrivate})
	require.NoError(t, err)
	require.NoError(t, root.WriteFile(filepath.Base(path), raw, privatefs.FilePolicy{RejectWritableByOthers: true}))
	require.NoError(t, root.Close())
	loaded, err := LoadSecurityEpoch(path)
	require.NoError(t, err)
	require.Equal(t, state, loaded)

	require.NoError(t, os.WriteFile(path, append(raw, []byte("{}")...), 0o600))
	_, err = LoadSecurityEpoch(path)
	require.ErrorIs(t, err, ErrInvalidState)

	missing := filepath.Join(dir, "missing", "security-epoch.json")
	_, err = LoadSecurityEpoch(missing)
	require.Error(t, err)
	_, statErr := os.Stat(filepath.Dir(missing))
	require.ErrorIs(t, statErr, os.ErrNotExist)
}
