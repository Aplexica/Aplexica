package adapter

import (
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/stretchr/testify/require"
)

func TestRefreshMainBranchHead_PreservesAlignedBaselineHead(t *testing.T) {
	store := &acf.Store{Root: t.TempDir()}
	require.NoError(t, store.Init())
	id := acf.NewID()
	now := time.Now().UTC()
	require.NoError(t, store.WriteArtifact(acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion,
		ArtifactID:       id,
		Kind:             acf.KindConversation,
		Scope:            acf.ScopeGlobal,
		CreatedAt:        now,
		UpdatedAt:        now,
	}))
	require.NoError(t, store.AdoptBaseline(acf.KindConversation, acf.Event{
		EventID:        acf.NewID(),
		ArtifactID:     id,
		Type:           acf.EventTypeBaseline,
		Timestamp:      now,
		Payload:        []byte(`{"format":"acf.conversation.v1","events":[]}`),
		AlignedHead:    "origin-aligned-head",
		AlignedEventID: acf.NewID(),
	}))

	art, err := store.ReadArtifact(acf.KindConversation, id)
	require.NoError(t, err)
	last, ok, err := store.LastEvent(acf.KindConversation, id)
	require.NoError(t, err)
	require.True(t, ok)
	require.NotEqual(t, last.Hash, art.HeadEventHash)

	head, err := RefreshMainBranchHead(store, acf.KindConversation, &art)
	require.NoError(t, err)
	require.Equal(t, "origin-aligned-head", head)
	require.Equal(t, "origin-aligned-head", art.HeadEventHash)
	require.Equal(t, "origin-aligned-head", art.BranchHeads[acf.MainBranch])
}
