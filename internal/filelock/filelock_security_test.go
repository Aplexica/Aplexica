package filelock_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/filelock"
	"github.com/aplexica/aplexica/internal/privatefs"
	"github.com/stretchr/testify/require"
)

func TestAcquireNarrowsOwnedLegacyLockFile(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, privatefs.EnsureDir(dir, privatefs.DirPolicy{
		Access:      privatefs.AccessPrivate,
		RepairOwned: true,
	}))
	path := filepath.Join(dir, "legacy.lock")
	require.NoError(t, os.WriteFile(path, []byte("legacy"), 0o644))

	lock, err := filelock.Acquire(path, time.Second)
	require.NoError(t, err)
	require.NoError(t, lock.Close())

	root, err := privatefs.OpenRoot(dir, privatefs.DirPolicy{Access: privatefs.AccessPrivate})
	require.NoError(t, err)
	verified, err := root.OpenReadRegular("legacy.lock")
	require.NoError(t, err)
	require.NoError(t, verified.Close())
	require.NoError(t, root.Close())
}
