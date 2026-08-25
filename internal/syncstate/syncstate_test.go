package syncstate

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStore_GetDefaultsToFalse(t *testing.T) {
	s := &Store{Path: filepath.Join(t.TempDir(), "ss.json")}
	v, err := s.Get("never-set")
	require.NoError(t, err)
	require.False(t, v, "default (FR-02.16) must be false")
}

func TestStore_SetThenGet(t *testing.T) {
	s := &Store{Path: filepath.Join(t.TempDir(), "ss.json")}
	require.NoError(t, s.Set("a", true))

	v, err := s.Get("a")
	require.NoError(t, err)
	require.True(t, v)

	// Untouched IDs still default to false.
	v, err = s.Get("b")
	require.NoError(t, err)
	require.False(t, v)
}

func TestStore_RetainsExplicitFalse(t *testing.T) {
	// Set false → Get returns false (same as default), but All() must
	// still record the explicit entry.
	s := &Store{Path: filepath.Join(t.TempDir(), "ss.json")}
	require.NoError(t, s.Set("a", true))
	require.NoError(t, s.Set("a", false))

	all, err := s.All()
	require.NoError(t, err)
	v, ok := all["a"]
	require.True(t, ok, "explicit false entry must be retained")
	require.False(t, v)
}

func TestStore_DeleteForArtifact(t *testing.T) {
	s := &Store{Path: filepath.Join(t.TempDir(), "ss.json")}
	require.NoError(t, s.Set("a", true))
	require.NoError(t, s.DeleteForArtifact("a"))

	all, err := s.All()
	require.NoError(t, err)
	_, ok := all["a"]
	require.False(t, ok, "DeleteForArtifact must remove the entry")
}

func TestStore_PersistsAcrossLoads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ss.json")
	s1 := &Store{Path: path}
	require.NoError(t, s1.Set("a", true))
	require.NoError(t, s1.Set("b", false))

	s2 := &Store{Path: path}
	v, err := s2.Get("a")
	require.NoError(t, err)
	require.True(t, v)
	v, err = s2.Get("b")
	require.NoError(t, err)
	require.False(t, v)
}

func TestStore_AtomicWrite(t *testing.T) {
	// Verify the on-disk file gets created with sensible permissions
	// after a Set (atomicfile under the hood).
	path := filepath.Join(t.TempDir(), "ss.json")
	s := &Store{Path: path}
	require.NoError(t, s.Set("a", true))
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.False(t, info.IsDir())
}

func TestDefaultPath(t *testing.T) {
	// filepath.Join uses the platform separator. Build the expected
	// value the same way so this test passes on Windows too (backslash)
	// and POSIX (forward slash).
	got := DefaultPath("/var/state")
	require.Equal(t, filepath.Join("/var/state", "tool-sync-secrets.json"), got)
}
