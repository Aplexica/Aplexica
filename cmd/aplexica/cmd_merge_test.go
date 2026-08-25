package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/stretchr/testify/require"
)

func runMergeCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	t.Cleanup(func() {
		mergeStoreRoot = ""
		mergeFrom = ""
		mergeInto = ""
		mergeStrategy = ""
		mergeNotes = ""
		mergeAccept = ""
		mergeJSON = false
		mergeNonInter = false
	})
	return runRoot(t, append([]string{"merge"}, args...)...)
}

func TestMerge_TheirsStrategy(t *testing.T) {
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
		"--branch", "alt",
		"--store", storeRoot,
	)
	require.NoError(t, err)

	out, err := runMergeCmd(t, id,
		"--from", "alt",
		"--into", "main",
		"--strategy", "theirs",
		"--store", storeRoot,
	)
	require.NoError(t, err, "merge output:\n%s", out)
	require.Contains(t, out, "strategy: theirs")

	// After merge there must be a merge event on main.
	final, err := store.ReadEvents(acf.KindMemory, id)
	require.NoError(t, err)
	var mergeEvt *acf.Event
	for i := range final {
		if final[i].Type == acf.EventTypeMergeOuter {
			mergeEvt = &final[i]
		}
	}
	require.NotNil(t, mergeEvt, "expected a merge event in the log")
	require.Equal(t, "alt", mergeEvt.MergeFromBranch)
	require.Equal(t, "theirs", mergeEvt.MergeStrategy)
}

func TestMerge_RejectsNonexistentDestination(t *testing.T) {
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
		"--branch", "alt",
		"--store", storeRoot,
	)
	require.NoError(t, err)

	// Merging into a branch that does not exist must error, not fabricate a
	// ghost branch rooted at an orphan (empty-parent) merge event.
	out, err := runMergeCmd(t, id,
		"--from", "alt",
		"--into", "ghost-typo",
		"--strategy", "theirs",
		"--store", storeRoot,
	)
	require.Error(t, err, "merge into a nonexistent destination must fail; output:\n%s", out)

	// Nothing should have been written: no merge event, no ghost branch.
	final, err := store.ReadEvents(acf.KindMemory, id)
	require.NoError(t, err)
	for _, e := range final {
		require.NotEqual(t, acf.EventTypeMergeOuter, e.Type, "no merge event on a rejected merge")
		require.NotEqual(t, "ghost-typo", e.Branch, "no ghost branch event must be written")
	}
}

func TestMerge_ManualWithAccept(t *testing.T) {
	tmp := t.TempDir()
	storeRoot := filepath.Join(tmp, "store")
	id := seedMemoryArtifact(t, storeRoot, "claude-code", "# v1\n")
	store := &acf.Store{Root: storeRoot}
	events, _ := store.ReadEvents(acf.KindMemory, id)

	_, err := runRoot(t,
		"fork", id,
		"--from", events[0].EventID,
		"--to-agent", "codex",
		"--branch", "alt",
		"--store", storeRoot,
	)
	require.NoError(t, err)

	out, err := runMergeCmd(t, id,
		"--from", "alt", "--into", "main",
		"--strategy", "manual",
		"--accept", "from",
		"--store", storeRoot,
	)
	require.NoError(t, err, "manual merge output:\n%s", out)
	require.Contains(t, out, "manual")
}

func TestMerge_FastForwardRefusesDivergent(t *testing.T) {
	tmp := t.TempDir()
	storeRoot := filepath.Join(tmp, "store")
	id := seedMemoryArtifact(t, storeRoot, "claude-code", "# v1\n")
	store := &acf.Store{Root: storeRoot}
	events, _ := store.ReadEvents(acf.KindMemory, id)

	_, err := runRoot(t,
		"fork", id,
		"--from", events[0].EventID,
		"--to-agent", "codex",
		"--branch", "alt",
		"--store", storeRoot,
	)
	require.NoError(t, err)

	out, err := runMergeCmd(t, id,
		"--from", "alt", "--into", "main",
		"--strategy", "fast-forward",
		"--store", storeRoot,
	)
	// alt is NOT a strict prefix of main (main has e0, alt has e0+fork-event).
	// So fast-forward should be refused.
	require.Error(t, err, "fast-forward should be refused; out:\n%s", out)
	require.True(t,
		strings.Contains(out, "fast-forward merge refused") || strings.Contains(err.Error(), "fast-forward merge refused"),
		"want fast-forward error; got %q / %q", out, err)
}

func TestMerge_NWayDest_PicksMainByDefault(t *testing.T) {
	tmp := t.TempDir()
	storeRoot := filepath.Join(tmp, "store")
	id := seedMemoryArtifact(t, storeRoot, "claude-code", "# v1\n")
	store := &acf.Store{Root: storeRoot}
	events, _ := store.ReadEvents(acf.KindMemory, id)

	for _, branchName := range []string{"a", "b", "c"} {
		_, err := runRoot(t,
			"fork", id,
			"--from", events[0].EventID,
			"--to-agent", "agent-"+branchName,
			"--branch", branchName,
			"--store", storeRoot,
		)
		require.NoError(t, err, "fork %s failed", branchName)
	}

	out, err := runMergeCmd(t, id,
		"--store", storeRoot,
		"--non-interactive",
		"--strategy", "theirs",
	)
	require.NoError(t, err, "n-way merge output:\n%s", out)
	require.Contains(t, out, "merged 3 branch(es) into")
	require.Contains(t, out, "main")
}
