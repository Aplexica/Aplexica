package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/stretchr/testify/require"
)

func runTagCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	t.Cleanup(func() {
		tagStoreRoot = ""
		tagJSON = false
		tagDescribeDesc = ""
		tagDescribeColor = ""
		tagDescribeScope = ""
	})
	return runRoot(t, append([]string{"tag"}, args...)...)
}

func TestTag_AddListRemove(t *testing.T) {
	tmp := t.TempDir()
	storeRoot := filepath.Join(tmp, "store")
	id := seedMemoryArtifact(t, storeRoot, "claude-code", "# x\n")

	out, err := runTagCmd(t, "add", id, "work", "--store", storeRoot)
	require.NoError(t, err, "add:\n%s", out)
	require.Contains(t, out, "added tag")

	out, err = runTagCmd(t, "list", id, "--store", storeRoot)
	require.NoError(t, err)
	require.Contains(t, out, "work")

	out, err = runTagCmd(t, "list", "--store", storeRoot)
	require.NoError(t, err)
	require.Contains(t, out, "work")

	out, err = runTagCmd(t, "remove", id, "work", "--store", storeRoot)
	require.NoError(t, err)
	require.Contains(t, out, "removed tag")
}

func TestTag_RejectsReservedNamespaces(t *testing.T) {
	tmp := t.TempDir()
	storeRoot := filepath.Join(tmp, "store")
	id := seedMemoryArtifact(t, storeRoot, "claude-code", "# x\n")

	for _, tag := range []string{"aplexica:audit", "fork-of:abcd", "device:laptop", "conflict:1"} {
		out, err := runTagCmd(t, "add", id, tag, "--store", storeRoot)
		require.Error(t, err, "expected error for %s; out:\n%s", tag, out)
		require.True(t,
			strings.Contains(out, "reserved") || strings.Contains(err.Error(), "reserved"),
			"want reserved-namespace rejection for %s; got out=%q err=%v", tag, out, err)
	}
}

func TestTag_RenameAcrossArtifacts(t *testing.T) {
	tmp := t.TempDir()
	storeRoot := filepath.Join(tmp, "store")
	id1 := seedMemoryArtifact(t, storeRoot, "claude-code", "# 1\n")
	id2 := seedMemoryArtifact(t, storeRoot, "claude-code", "# 2\n")

	for _, id := range []string{id1, id2} {
		_, err := runTagCmd(t, "add", id, "work", "--store", storeRoot)
		require.NoError(t, err)
	}

	out, err := runTagCmd(t, "rename", "work", "work-project", "--store", storeRoot)
	require.NoError(t, err)
	require.Contains(t, out, "renamed")

	store := &acf.Store{Root: storeRoot}
	for _, id := range []string{id1, id2} {
		a, err := store.ReadArtifact(acf.KindMemory, id)
		require.NoError(t, err)
		require.Contains(t, a.Tags, "work-project")
		require.NotContains(t, a.Tags, "work")
	}
}

func TestTag_Describe(t *testing.T) {
	tmp := t.TempDir()
	storeRoot := filepath.Join(tmp, "store")
	id := seedMemoryArtifact(t, storeRoot, "claude-code", "# x\n")
	_, err := runTagCmd(t, "add", id, "work", "--store", storeRoot)
	require.NoError(t, err)

	out, err := runTagCmd(t, "describe", "work",
		"--description", "Anything related to my job",
		"--color", "#3aa1ff",
		"--scope", "personal",
		"--store", storeRoot,
	)
	require.NoError(t, err, "describe:\n%s", out)
	require.Contains(t, out, "Anything related to my job")

	out, err = runTagCmd(t, "describe", "work", "--store", storeRoot)
	require.NoError(t, err)
	require.Contains(t, out, "Anything related to my job")
	require.Contains(t, out, "#3aa1ff")
	require.Contains(t, out, "personal")
}
