package adapterstate

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStore_DefaultEverythingEnabled(t *testing.T) {
	s := &Store{Path: filepath.Join(t.TempDir(), "a.json")}
	require.False(t, s.IsDisabled("codex"))
}

func TestStore_DisableThenEnable(t *testing.T) {
	s := &Store{Path: filepath.Join(t.TempDir(), "a.json")}
	require.NoError(t, s.Disable("codex"))
	require.True(t, s.IsDisabled("codex"))
	require.False(t, s.IsDisabled("claude-code"))

	require.NoError(t, s.Enable("codex"))
	require.False(t, s.IsDisabled("codex"))
}

func TestStore_DisableIdempotent(t *testing.T) {
	s := &Store{Path: filepath.Join(t.TempDir(), "a.json")}
	require.NoError(t, s.Disable("codex"))
	require.NoError(t, s.Disable("codex"))

	st, err := s.Load()
	require.NoError(t, err)
	require.Len(t, st.Disabled, 1)
}

func TestStore_EnableIdempotent(t *testing.T) {
	s := &Store{Path: filepath.Join(t.TempDir(), "a.json")}
	require.NoError(t, s.Enable("never-disabled"))
	st, err := s.Load()
	require.NoError(t, err)
	require.Empty(t, st.Disabled)
}

func TestStore_PersistsAcrossLoads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.json")
	s1 := &Store{Path: path}
	require.NoError(t, s1.Disable("codex"))
	require.NoError(t, s1.Disable("kilo"))

	s2 := &Store{Path: path}
	require.True(t, s2.IsDisabled("codex"))
	require.True(t, s2.IsDisabled("kilo"))
	require.False(t, s2.IsDisabled("claude-code"))
}

func TestStore_DisabledSet(t *testing.T) {
	s := &Store{Path: filepath.Join(t.TempDir(), "a.json")}
	require.NoError(t, s.Disable("a"))
	require.NoError(t, s.Disable("c"))
	require.NoError(t, s.Disable("b"))

	set := s.DisabledSet()
	require.Len(t, set, 3)
	for _, n := range []string{"a", "b", "c"} {
		_, ok := set[n]
		require.True(t, ok, "expected %s in disabled set", n)
	}
}

func TestStore_DisabledIsSortedOnDisk(t *testing.T) {
	s := &Store{Path: filepath.Join(t.TempDir(), "a.json")}
	require.NoError(t, s.Disable("c"))
	require.NoError(t, s.Disable("a"))
	require.NoError(t, s.Disable("b"))

	st, err := s.Load()
	require.NoError(t, err)
	require.Equal(t, []string{"a", "b", "c"}, st.Disabled)
}
