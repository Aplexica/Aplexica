package acf_test

import (
	"testing"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/stretchr/testify/require"
)

func TestReplayBranches_MainOnly(t *testing.T) {
	events := []acf.ConversationEvent{
		{Type: acf.EventTypeTurn, Role: "user"},
		{Type: acf.EventTypeTurn, Role: "assistant"},
	}
	branches := acf.ReplayBranches(events)
	require.Len(t, branches, 1)
	require.Equal(t, "main", branches[0].ID)
	require.Empty(t, branches[0].ParentBranchID)
}

func TestReplayBranches_ForkAddsBranch(t *testing.T) {
	events := []acf.ConversationEvent{
		{Type: acf.EventTypeTurn, Role: "user"},
		{Type: acf.EventTypeFork, BranchID: "experiment-1", SourceEventID: "evt-1"},
		{Type: acf.EventTypeTurn, Role: "assistant", BranchID: "experiment-1"},
	}
	branches := acf.ReplayBranches(events)
	require.Len(t, branches, 2)
	ids := []string{branches[0].ID, branches[1].ID}
	require.Contains(t, ids, "main")
	require.Contains(t, ids, "experiment-1")
	// experiment-1 parent should be main
	for _, b := range branches {
		if b.ID == "experiment-1" {
			require.Equal(t, "main", b.ParentBranchID)
			require.Equal(t, "evt-1", b.ForkedFromEventID)
		}
	}
}

func TestReplayBranches_MergeMarksBranchClosed(t *testing.T) {
	events := []acf.ConversationEvent{
		{Type: acf.EventTypeFork, BranchID: "experiment-1", SourceEventID: "evt-1"},
		{Type: acf.EventTypeTurn, BranchID: "experiment-1"},
		{Type: acf.EventTypeMerge, BranchID: "main", MergedBranchIDs: []string{"experiment-1"}},
	}
	branches := acf.ReplayBranches(events)
	for _, b := range branches {
		if b.ID == "experiment-1" {
			require.True(t, b.MergedIntoMain, "experiment-1 should be marked as merged into main")
		}
	}
}
