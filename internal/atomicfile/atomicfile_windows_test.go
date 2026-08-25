//go:build windows

package atomicfile_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/aplexica/aplexica/internal/atomicfile"
	"github.com/aplexica/aplexica/internal/privatefs"
	"github.com/stretchr/testify/require"
)

func TestPrivateWritesProtectWindowsDACLAcrossAllWritePaths(t *testing.T) {
	dir := t.TempDir()
	fresh := filepath.Join(dir, "fresh")
	replaced := filepath.Join(dir, "replaced")
	unchanged := filepath.Join(dir, "unchanged")

	require.NoError(t, atomicfile.WriteFile(fresh, []byte("fresh"), 0o600))
	require.NoError(t, os.WriteFile(replaced, []byte("old"), 0o600))
	require.NoError(t, atomicfile.WriteFile(replaced, []byte("new"), 0o600))
	require.NoError(t, os.WriteFile(unchanged, []byte("same"), 0o600))
	require.NoError(t, atomicfile.WriteFile(unchanged, []byte("same"), 0o600))

	// Hardening the parent after the writes cannot retroactively protect a
	// child's DACL, so strict retained-root opens prove each atomic path did it.
	require.NoError(t, privatefs.EnsureDir(dir, privatefs.DirPolicy{
		Access:      privatefs.AccessPrivate,
		RepairOwned: true,
	}))
	root, err := privatefs.OpenRoot(dir, privatefs.DirPolicy{Access: privatefs.AccessPrivate})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, root.Close()) })
	for _, name := range []string{"fresh", "replaced", "unchanged"} {
		f, err := root.OpenReadRegular(name)
		require.NoError(t, err, name)
		require.NoError(t, f.Close())
	}
}
