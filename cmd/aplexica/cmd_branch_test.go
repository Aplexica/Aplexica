package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/stretchr/testify/require"
)

// runRoot executes the cobra root command with the supplied args using
// the shared rootCmd. It captures stdout+stderr into a single string
// for assertions and resets the cobra package globals on cleanup so
// state doesn't leak across tests.
func runRoot(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&out)
	rootCmd.SetArgs(args)
	t.Cleanup(func() {
		forkStoreRoot = ""
		forkFromEvent = ""
		forkToAgent = ""
		forkBranch = ""
		forkRationale = ""
		forkNoMaterialize = false
		branchStoreRoot = ""
		branchListJSON = false
		branchListIncludeArch = false
		branchCreateFrom = ""
		checkoutStoreRoot = ""
		checkoutBranch = ""
		checkoutAgent = ""
		checkoutNoMaterialize = false
	})
	err := rootCmd.Execute()
	return out.String(), err
}

func TestForkBranchCheckout_RoundTrip(t *testing.T) {
	tmp := t.TempDir()
	storeRoot := filepath.Join(tmp, "store")
	id := seedMemoryArtifact(t, storeRoot, "claude-code", "# original\n")

	store := &acf.Store{Root: storeRoot}
	events, err := store.ReadEvents(acf.KindMemory, id)
	require.NoError(t, err)
	require.NotEmpty(t, events)
	parent := events[0]

	out, err := runRoot(t,
		"fork", id,
		"--from", parent.EventID,
		"--to-agent", "codex",
		"--branch", "experiment",
		"--store", storeRoot,
	)
	require.NoError(t, err, "fork output:\n%s", out)
	require.Contains(t, out, "experiment")

	listOut, err := runRoot(t, "branch", "list", id, "--store", storeRoot)
	require.NoError(t, err, "branch list output:\n%s", listOut)
	require.Contains(t, listOut, "main")
	require.Contains(t, listOut, "experiment")
	require.Contains(t, listOut, "MATERIALIZED IN")
	require.Contains(t, listOut, "codex")
	branches, err := store.ListBranches(acf.KindMemory, id, true)
	require.NoError(t, err)
	var experiment acf.BranchInfo
	foundExperiment := false
	for _, branch := range branches {
		if branch.Name == "experiment" {
			experiment = branch
			foundExperiment = true
			break
		}
	}
	require.True(t, foundExperiment)
	require.Equal(t, "claude-code", experiment.OriginAgent)

	checkoutOut, err := runRoot(t,
		"checkout", id,
		"--branch", "experiment",
		"--agent", "codex",
		"--store", storeRoot,
	)
	require.NoError(t, err, "checkout output:\n%s", checkoutOut)
	require.Contains(t, checkoutOut, `materializes branch "experiment"`)

	// Reload artifact and confirm pointer.
	updated, err := store.ReadArtifact(acf.KindMemory, id)
	require.NoError(t, err)
	require.Equal(t, "experiment", updated.MaterializedBranchByAgent["codex"])
}

func TestBranchArchiveUnarchive(t *testing.T) {
	tmp := t.TempDir()
	storeRoot := filepath.Join(tmp, "store")
	id := seedMemoryArtifact(t, storeRoot, "claude-code", "# original\n")
	store := &acf.Store{Root: storeRoot}
	events, err := store.ReadEvents(acf.KindMemory, id)
	require.NoError(t, err)

	_, err = runRoot(t,
		"branch", "create", id, "sandbox",
		"--from", events[0].EventID,
		"--store", storeRoot,
	)
	require.NoError(t, err)

	_, err = runRoot(t, "branch", "archive", id, "sandbox", "--store", storeRoot)
	require.NoError(t, err)

	// Default list hides archived.
	out, err := runRoot(t, "branch", "list", id, "--store", storeRoot)
	require.NoError(t, err)
	require.NotContains(t, out, "sandbox")

	out2, err := runRoot(t, "branch", "list", id, "--include-archived", "--store", storeRoot)
	require.NoError(t, err)
	require.Contains(t, out2, "sandbox")
	require.Contains(t, out2, "archived")

	// Unarchive.
	_, err = runRoot(t, "branch", "unarchive", id, "sandbox", "--store", storeRoot)
	require.NoError(t, err)

	out3, err := runRoot(t, "branch", "list", id, "--store", storeRoot)
	require.NoError(t, err)
	require.Contains(t, out3, "sandbox")
}

func TestBranchDeleteRequiresArchive(t *testing.T) {
	tmp := t.TempDir()
	storeRoot := filepath.Join(tmp, "store")
	id := seedMemoryArtifact(t, storeRoot, "claude-code", "# original\n")
	store := &acf.Store{Root: storeRoot}
	events, _ := store.ReadEvents(acf.KindMemory, id)

	_, err := runRoot(t,
		"branch", "create", id, "drop-me",
		"--from", events[0].EventID,
		"--store", storeRoot,
	)
	require.NoError(t, err)

	out, err := runRoot(t, "branch", "delete", id, "drop-me", "--store", storeRoot)
	require.Error(t, err, "delete without archive should fail; output:\n%s", out)
	require.True(t, strings.Contains(out, "archived") || strings.Contains(err.Error(), "archived"),
		"expected error mentioning archive requirement; got %q / %q", out, err)
}

func TestBranchRename_DurableAcrossRefresh(t *testing.T) {
	tmp := t.TempDir()
	storeRoot := filepath.Join(tmp, "store")
	id := seedMemoryArtifact(t, storeRoot, "claude-code", "# original\n")
	store := &acf.Store{Root: storeRoot}
	events, _ := store.ReadEvents(acf.KindMemory, id)

	_, err := runRoot(t, "branch", "create", id, "sandbox",
		"--from", events[0].EventID, "--store", storeRoot)
	require.NoError(t, err)

	_, err = runRoot(t, "branch", "rename", id, "sandbox", "renamed-box", "--store", storeRoot)
	require.NoError(t, err)

	// `branch list` rebuilds the index off the event log; the rename must
	// survive that rebuild rather than reverting to the old name.
	out, err := runRoot(t, "branch", "list", id, "--store", storeRoot)
	require.NoError(t, err, "branch list output:\n%s", out)
	require.Contains(t, out, "renamed-box")
	require.NotContains(t, out, "sandbox")
}

func TestBranchRename_PreservesArchivedMetadata(t *testing.T) {
	tmp := t.TempDir()
	storeRoot := filepath.Join(tmp, "store")
	id := seedMemoryArtifact(t, storeRoot, "claude-code", "# original\n")
	store := &acf.Store{Root: storeRoot}
	events, _ := store.ReadEvents(acf.KindMemory, id)

	_, err := runRoot(t, "branch", "create", id, "sandbox",
		"--from", events[0].EventID, "--store", storeRoot)
	require.NoError(t, err)
	_, err = runRoot(t, "branch", "archive", id, "sandbox", "--store", storeRoot)
	require.NoError(t, err)

	// Renaming an archived branch must keep it archived, not silently revive it.
	_, err = runRoot(t, "branch", "rename", id, "sandbox", "renamed-box", "--store", storeRoot)
	require.NoError(t, err)

	out, err := runRoot(t, "branch", "list", id, "--include-archived", "--store", storeRoot)
	require.NoError(t, err, "branch list output:\n%s", out)
	require.Contains(t, out, "renamed-box")
	require.Contains(t, out, "archived")
	require.NotContains(t, out, "sandbox")
}
