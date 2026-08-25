package openclaw

import (
	"testing"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
	"github.com/stretchr/testify/require"
)

// OpenClaw rewrites its transcript wholesale, so it has no snapshot race or
// divergence relation to report: every non-error decline is the permanent
// opt-out, and both the writer and the path planner must say so.
func TestOpenClawConversationSessionReason_OptsOut(t *testing.T) {
	a := New()
	a.HomeDir = t.TempDir()
	payload, err := acf.EncodePayload(acf.ConversationPayload{Format: "vendor.unknown/v1", Content: "{}"})
	require.NoError(t, err)
	art := acf.Artifact{ArtifactID: acf.NewID(), Kind: acf.KindConversation, Scope: acf.ScopeGlobal}
	head := acf.Event{ArtifactID: art.ArtifactID, Type: acf.EventTypeUpdate, Payload: payload}

	path, ok, reason, err := a.MaterializeConversationSessionReason(art, head, "codex")
	require.NoError(t, err)
	require.False(t, ok)
	require.Empty(t, path)
	require.Equal(t, adapter.SessionDeclineOptOut, reason)

	path, ok, reason, err = a.ConversationSessionPathReason(art, head, "codex")
	require.NoError(t, err)
	require.False(t, ok)
	require.Empty(t, path)
	require.Equal(t, adapter.SessionDeclineOptOut, reason)

	var _ adapter.ConversationSessionDeclineReporter = a
	var _ adapter.ConversationSessionPathDeclineReporter = a
}
