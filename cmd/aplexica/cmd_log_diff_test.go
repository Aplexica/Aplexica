package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/stretchr/testify/require"
)

// runRoot is shared with cmd_branch_test.go.

func runLogCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	t.Cleanup(func() {
		logStoreRoot = ""
		logIncludeCompacted = false
		logGraph = false
		logBranchFilter = ""
		logEventTagFilter = ""
		logFormat = ""
	})
	return runRoot(t, append([]string{"log"}, args...)...)
}

func runDiffCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	t.Cleanup(func() {
		diffStoreRoot = ""
		diffBranchA = ""
		diffEventA = ""
		diffTo = ""
		diffJSON = false
	})
	return runRoot(t, append([]string{"diff"}, args...)...)
}

func TestLog_GraphRendersBranches(t *testing.T) {
	tmp := t.TempDir()
	storeRoot := filepath.Join(tmp, "store")
	id := seedMemoryArtifact(t, storeRoot, "claude-code", "# original\n")

	store := &acf.Store{Root: storeRoot}
	events, err := store.ReadEvents(acf.KindMemory, id)
	require.NoError(t, err)
	parent := events[0]

	// Fork into "alt".
	_, err = runRoot(t,
		"fork", id,
		"--from", parent.EventID,
		"--to-agent", "codex",
		"--branch", "alt",
		"--store", storeRoot,
	)
	require.NoError(t, err)

	out, err := runLogCmd(t, id, "--graph", "--store", storeRoot)
	require.NoError(t, err, "log --graph output:\n%s", out)
	require.Contains(t, out, "main")
	require.Contains(t, out, "alt")
	require.Contains(t, out, "[fork]")
}

func TestLog_BranchFilter(t *testing.T) {
	tmp := t.TempDir()
	storeRoot := filepath.Join(tmp, "store")
	id := seedMemoryArtifact(t, storeRoot, "claude-code", "# original\n")

	store := &acf.Store{Root: storeRoot}
	events, err := store.ReadEvents(acf.KindMemory, id)
	require.NoError(t, err)

	_, err = runRoot(t,
		"fork", id,
		"--from", events[0].EventID,
		"--to-agent", "codex",
		"--branch", "topic",
		"--store", storeRoot,
	)
	require.NoError(t, err)

	out, err := runLogCmd(t, id, "--branch", "main", "--store", storeRoot)
	require.NoError(t, err)
	require.Contains(t, out, events[0].Hash)
	// Should NOT contain "topic" branch fork event
	require.NotContains(t, out, "\"branch\":\"topic\"")

	outAlt, err := runLogCmd(t, id, "--branch", "topic", "--store", storeRoot)
	require.NoError(t, err)
	require.Contains(t, outAlt, events[0].Hash, "branch projection must include inherited source prefix")
	require.Contains(t, outAlt, "\"branch\":\"topic\"")
}

func TestDiff_BranchMode(t *testing.T) {
	tmp := t.TempDir()
	storeRoot := filepath.Join(tmp, "store")
	id := seedMemoryArtifact(t, storeRoot, "claude-code", "# v1\n")

	store := &acf.Store{Root: storeRoot}
	events, err := store.ReadEvents(acf.KindMemory, id)
	require.NoError(t, err)

	_, err = runRoot(t,
		"fork", id,
		"--from", events[0].EventID,
		"--to-agent", "codex",
		"--branch", "experiment",
		"--store", storeRoot,
	)
	require.NoError(t, err)

	out, err := runDiffCmd(t, id, "--branch", "main", "--to", "experiment", "--store", storeRoot)
	require.NoError(t, err, "diff output:\n%s", out)
	require.Contains(t, out, "1 events in common", "branch diff must compare projected histories")
	require.True(t, strings.Contains(out, "only in A") || strings.Contains(out, "only in B"),
		"expected diff summary; got %q", out)
}
