package main

import (
	"bytes"
	"io"
	"path/filepath"
	"testing"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/conflicts"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

// seedTwoHeadConflict stages an artifact with a recorded two-head
// conflict and returns its id. Head 0 is the local head; head 1 is a
// second local (codex) head with a distinct payload.
func seedTwoHeadConflict(t *testing.T, storeRoot, conflictsDir string) (string, string, string) {
	t.Helper()
	id := seedMemoryArtifact(t, storeRoot, "claude-code", "# head zero\n")
	store := &acf.Store{Root: storeRoot}
	ev0, err := store.ReadEvents(acf.KindMemory, id)
	require.NoError(t, err)
	require.Len(t, ev0, 1)
	head0 := ev0[0]

	// A second, divergent local head appended on a side branch so its
	// EventID is present in the local log.
	altContent := "# head one\n"
	altPayload, err := acf.EncodePayload(acf.MemoryPayload{Format: "markdown", Content: altContent})
	require.NoError(t, err)
	alt := acf.Event{
		EventID:    acf.NewID(),
		ArtifactID: id,
		Type:       acf.EventTypeCreate,
		Branch:     "alt",
		Provenance: acf.Provenance{SourceAgent: "codex"},
		Payload:    altPayload,
		ParentHash: "",
	}
	require.NoError(t, store.AppendEvent(acf.KindMemory, alt))

	cs := &conflicts.Store{Root: conflictsDir}
	require.NoError(t, cs.Init())
	require.NoError(t, cs.Record(conflicts.Conflict{
		ArtifactID: id,
		Kind:       acf.KindMemory,
		Heads: []conflicts.Head{
			{SourceAgent: head0.Provenance.SourceAgent, EventID: head0.EventID, ContentSHA256: head0.Hash},
			{SourceAgent: "codex", EventID: alt.EventID, ContentSHA256: alt.Hash},
		},
	}))
	return id, head0.EventID, alt.EventID
}

// TestPromptForHeadIndex_ReadsSelection exercises the interactive head
// picker the top-level `aplexica resolve` uses when --pick is omitted.
// A 1-based selection of "2" must map to 0-based index 1; an empty line
// must default to 0.
func TestPromptForHeadIndex_ReadsSelection(t *testing.T) {
	heads := []conflicts.Head{
		{SourceAgent: "claude-code", EventID: "e0"},
		{SourceAgent: "codex", EventID: "e1"},
	}

	idx, err := promptForHeadIndex(newResolveTestCmd("2\n"), heads)
	require.NoError(t, err)
	require.Equal(t, 1, idx, "selection '2' must map to 0-based index 1")

	idx, err = promptForHeadIndex(newResolveTestCmd("\n"), heads)
	require.NoError(t, err)
	require.Equal(t, 0, idx, "empty selection must default to head 0")
}

// TestResolveTop_HonorsExplicitPick confirms the --pick override still
// resolves to the requested head non-interactively.
func TestResolveTop_HonorsExplicitPick(t *testing.T) {
	tmp := t.TempDir()
	storeRoot := filepath.Join(tmp, "store")
	conflictsDir := filepath.Join(tmp, "conflicts")
	id, _, altEventID := seedTwoHeadConflict(t, storeRoot, conflictsDir)

	t.Cleanup(func() {
		resolveTopStoreRoot = ""
		resolveTopConflictsRoot = ""
		resolveTopPick = 0
		conflictsRoot = ""
		resolveStoreRoot = ""
		resolvePickIdx = 0
	})
	out, err := runRoot(t,
		"resolve", id,
		"--pick", "1",
		"--store", storeRoot,
		"--conflicts-root", conflictsDir,
	)
	require.NoError(t, err, "out:\n%s", out)

	store := &acf.Store{Root: storeRoot}
	final, err := store.ReadEvents(acf.KindMemory, id)
	require.NoError(t, err)
	var resolution *acf.Event
	for i := range final {
		if final[i].Type == acf.EventTypeResolution {
			resolution = &final[i]
		}
	}
	require.NotNil(t, resolution)
	// Head 1's payload ("# head one") must have won.
	require.Contains(t, string(resolution.Payload), "head one")
	_ = altEventID
}

// newResolveTestCmd returns a cobra command whose stdin yields the given
// string, for driving the picker helper in tests.
func newResolveTestCmd(stdin string) *cobra.Command {
	c := &cobra.Command{}
	c.SetIn(io.NopCloser(bytes.NewBufferString(stdin)))
	c.SetOut(&bytes.Buffer{})
	return c
}
