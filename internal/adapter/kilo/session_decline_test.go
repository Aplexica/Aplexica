package kilo

import (
	"testing"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
	"github.com/stretchr/testify/require"
)

// Kilo delegates every write to `kilo import`, so its only non-error decline is
// the permanent opt-out. Reporting it explicitly keeps the orchestrator from
// treating an unmaterializable payload as an unclassified failure worth
// retrying forever.
func TestKiloMaterializeConversationSessionReason_OptsOut(t *testing.T) {
	a := New()
	a.HomeDir = t.TempDir()
	payload, err := acf.EncodePayload(acf.ConversationPayload{Format: "vendor.unknown/v1", Content: "{}"})
	require.NoError(t, err)
	art := acf.Artifact{ArtifactID: acf.NewID(), Kind: acf.KindConversation, Scope: acf.ScopeGlobal}
	head := acf.Event{ArtifactID: art.ArtifactID, Type: acf.EventTypeUpdate, Payload: payload}

	path, ok, reason, err := a.MaterializeConversationSessionReason(art, head, "claude-code")
	require.NoError(t, err)
	require.False(t, ok)
	require.Empty(t, path)
	require.Equal(t, adapter.SessionDeclineOptOut, reason)

	var _ adapter.ConversationSessionDeclineReporter = a
}
