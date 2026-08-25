package main

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	apiweb "github.com/aplexica/aplexica/internal/web/api"
	"github.com/stretchr/testify/require"
)

func TestConversationBranchesWebAccessor_ListIncludesMaterializedAgents(t *testing.T) {
	store, artifactID, _ := seedConversationForWebBranching(t)
	art, err := store.ReadArtifact(acf.KindConversation, artifactID)
	require.NoError(t, err)
	art.MaterializedBranchByAgent = map[string]string{"codex": acf.MainBranch}
	require.NoError(t, store.WriteArtifact(art))

	acc := &conversationBranchesWebAccessor{deps: &webAPIDeps{store: store}}
	got, ok, err := acc.ListConversationBranches(artifactID)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, artifactID, got.ArtifactID)
	require.Len(t, got.Branches, 1)
	require.Equal(t, "main", got.Branches[0].Name)
	require.Equal(t, []string{"codex"}, got.Branches[0].MaterializedAgents)
}

func TestConversationBranchesWebAccessor_ListDefaultsMaterializedAgents(t *testing.T) {
	store, artifactID, _ := seedConversationForWebBranching(t)
	acc := &conversationBranchesWebAccessor{deps: &webAPIDeps{store: store}}

	got, ok, err := acc.ListConversationBranches(artifactID)
	require.NoError(t, err)
	require.True(t, ok)
	require.Len(t, got.Branches, 1)
	require.NotNil(t, got.Branches[0].MaterializedAgents)
	require.Empty(t, got.Branches[0].MaterializedAgents)
}

func TestConversationBranchesWebAccessor_ForkCreatesBranchAndPointer(t *testing.T) {
	store, artifactID, events := seedConversationForWebBranching(t)
	acc := &conversationBranchesWebAccessor{deps: &webAPIDeps{store: store}}

	got, err := acc.ForkConversation(artifactID, apiweb.ConversationForkRequest{
		FromEventID: events[0].EventID,
		TargetAgent: "codex",
		Branch:      "Review branch",
		Rationale:   "try an alternate answer",
	})
	require.NoError(t, err)
	require.Equal(t, "review-branch", got.Branch)
	require.Equal(t, "codex", got.Agent)
	require.True(t, got.CreatedBranch)
	require.Contains(t, got.Warning, "materializer")

	branches, err := store.ListBranches(acf.KindConversation, artifactID, true)
	require.NoError(t, err)
	require.Len(t, branches, 2)
	require.Equal(t, "review-branch", branches[1].Name)
	require.Equal(t, acf.MainBranch, branches[1].ForkedFrom)
	require.Equal(t, "claude-code", branches[1].OriginAgent)
	require.Equal(t, "try an alternate answer", branches[1].Rationale)

	art, err := store.ReadArtifact(acf.KindConversation, artifactID)
	require.NoError(t, err)
	require.NotEmpty(t, art.BranchHeads["review-branch"])
	require.Equal(t, "review-branch", art.MaterializedBranchByAgent["codex"])
}

func TestConversationBranchesWebAccessor_CheckoutUpdatesPointer(t *testing.T) {
	store, artifactID, events := seedConversationForWebBranching(t)
	acc := &conversationBranchesWebAccessor{deps: &webAPIDeps{store: store}}
	_, err := acc.ForkConversation(artifactID, apiweb.ConversationForkRequest{
		FromEventID: events[0].Hash,
		TargetAgent: "codex",
		Branch:      "alternate",
	})
	require.NoError(t, err)

	got, err := acc.CheckoutConversation(artifactID, apiweb.ConversationCheckoutRequest{
		Agent:  "codex",
		Branch: "main",
	})
	require.NoError(t, err)
	require.Equal(t, "checkout", got.Operation)
	require.Equal(t, acf.MainBranch, got.Branch)

	art, err := store.ReadArtifact(acf.KindConversation, artifactID)
	require.NoError(t, err)
	require.Equal(t, acf.MainBranch, art.MaterializedBranchByAgent["codex"])
}

func seedConversationForWebBranching(t *testing.T) (*acf.Store, string, []acf.Event) {
	t.Helper()
	store := &acf.Store{Root: filepath.Join(t.TempDir(), "store")}
	require.NoError(t, store.Init())
	artifactID := acf.NewID()
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	require.NoError(t, store.WriteArtifact(acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion,
		ArtifactID:       artifactID,
		Kind:             acf.KindConversation,
		Scope:            acf.ScopeGlobal,
		Name:             "session.jsonl",
		CreatedAt:        now,
		UpdatedAt:        now,
	}))
	payload, err := acf.EncodePayload(acf.ConversationPayload{
		Format: acf.ConversationFormatV1,
		Events: []acf.ConversationEvent{{
			Type:      acf.EventTypeTurn,
			Timestamp: now,
			Role:      "user",
			Content:   []acf.ContentBlock{{Type: "text", Text: "hello"}},
		}},
	})
	require.NoError(t, err)
	first := acf.Event{
		EventID:    acf.NewID(),
		ArtifactID: artifactID,
		Type:       acf.EventTypeCreate,
		Timestamp:  now,
		Provenance: acf.Provenance{SourceAgent: "claude-code"},
		Payload:    payload,
	}
	require.NoError(t, store.AppendEvent(acf.KindConversation, first))
	events, err := store.ReadEvents(acf.KindConversation, artifactID)
	require.NoError(t, err)
	return store, artifactID, events
}
