package nativebackup

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func writeExclusionFixture(t *testing.T, root, rel string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte(rel), 0o600))
}

func manifestPathsBelowRoot(man Manifest, agent, root string) []string {
	prefix := filepath.ToSlash(filepath.Join(agent, relativize(root))) + "/"
	var out []string
	for _, entry := range man.Agents[0].Roots {
		out = append(out, strings.TrimPrefix(entry.Path, prefix))
	}
	sort.Strings(out)
	return out
}

func TestSnapshot_ExcludePathsOmitExactSubtreeWithoutPrefixCollision(t *testing.T) {
	root := filepath.Join(t.TempDir(), "agent")
	for _, rel := range []string{"keep.txt", "cache/drop.txt", "cache2/keep.txt"} {
		writeExclusionFixture(t, root, rel)
	}
	dest := filepath.Join(t.TempDir(), "manual")

	man, err := Snapshot([]AgentRoots{{
		Name:         "agent",
		Roots:        []string{root},
		ExcludePaths: []string{filepath.Join(root, "cache")},
	}}, dest)
	require.NoError(t, err)
	require.Equal(t, []string{"cache2/keep.txt", "keep.txt"}, manifestPathsBelowRoot(man, "agent", root))
	_, err = os.Stat(filepath.Join(dest, "agent", relativize(root), "cache"))
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestSnapshot_GenericGeneratedComponentsAreExactAndGitIsPreserved(t *testing.T) {
	root := filepath.Join(t.TempDir(), "agent")
	for _, rel := range []string{
		"keep.txt",
		"node_modules/drop.js",
		"node_modules_notes/keep.txt",
		".venv/drop.py",
		"venv/drop.py",
		"myvenv/keep.txt",
		"__pycache__/drop.pyc",
		"pycache_notes/keep.txt",
		".DS_Store",
		".git/objects/unpublished",
	} {
		writeExclusionFixture(t, root, rel)
	}

	man, err := Snapshot([]AgentRoots{{Name: "agent", Roots: []string{root}}}, filepath.Join(t.TempDir(), "manual"))
	require.NoError(t, err)
	require.Equal(t, []string{
		".git/objects/unpublished",
		"keep.txt",
		"myvenv/keep.txt",
		"node_modules_notes/keep.txt",
		"pycache_notes/keep.txt",
	}, manifestPathsBelowRoot(man, "agent", root))
}

func TestSnapshot_RejectsInvalidExcludePathsBeforeCreatingDestination(t *testing.T) {
	root := filepath.Join(t.TempDir(), "agent")
	writeExclusionFixture(t, root, "keep.txt")

	for _, tc := range []struct {
		name     string
		excluded string
	}{
		{name: "relative", excluded: "cache"},
		{name: "outside root", excluded: filepath.Join(t.TempDir(), "outside")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dest := filepath.Join(t.TempDir(), "manual")
			_, err := Snapshot([]AgentRoots{{Name: "agent", Roots: []string{root}, ExcludePaths: []string{tc.excluded}}}, dest)
			require.Error(t, err)
			_, statErr := os.Stat(dest)
			require.ErrorIs(t, statErr, os.ErrNotExist)
		})
	}
}
