package codex

import (
	"os"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
	"github.com/stretchr/testify/require"
)

func codexDeclineFixture(t *testing.T) (*Adapter, acf.Artifact, func(...acf.TextTurn) acf.Event) {
	t.Helper()
	home := t.TempDir()
	artifactID := acf.NewID()
	baseTime := time.Date(2026, 7, 30, 11, 0, 0, 0, time.UTC)
	art := acf.Artifact{
		ArtifactID: artifactID, Kind: acf.KindConversation, Scope: acf.ScopeGlobal,
		Name: "thread.jsonl", CreatedAt: baseTime, UpdatedAt: baseTime,
	}
	makeHead := func(turns ...acf.TextTurn) acf.Event {
		events := make([]acf.ConversationEvent, 0, len(turns))
		for i, turn := range turns {
			events = append(events, acf.ConversationEvent{
				Type: acf.EventTypeTurn, Role: turn.Role,
				Timestamp: baseTime.Add(time.Duration(i) * time.Second),
				Content:   []acf.ContentBlock{{Type: "text", Text: turn.Text}},
			})
		}
		payload, err := acf.EncodePayload(acf.ConversationPayload{Format: acf.ConversationFormatV1, Events: events})
		require.NoError(t, err)
		return acf.Event{ArtifactID: artifactID, Type: acf.EventTypeUpdate, Timestamp: baseTime, Payload: payload}
	}
	return &Adapter{HomeDir: home}, art, makeHead
}

// The livelock is bidirectional: codex declines claude-code-origin heads too,
// and a lost append race must not be classified like a permanent divergence.
func TestCodexMaterializeConversationSessionReason_RaceVersusDivergence(t *testing.T) {
	a, art, makeHead := codexDeclineFixture(t)
	base := []acf.TextTurn{
		{Role: "user", Text: "capital?"},
		{Role: "assistant", Text: "Warsaw."},
		{Role: "user", Text: "population?"},
	}
	primary, ok, reason, err := a.MaterializeConversationSessionReason(art, makeHead(base...), "claude-code")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, adapter.SessionDeclineUnspecified, reason)

	// Codex wins the optimistic append between snapshot and commit.
	writer, err := os.OpenFile(primary, os.O_WRONLY|os.O_APPEND, 0)
	require.NoError(t, err)
	t.Cleanup(func() { _ = writer.Close() })
	complete := append(append([]acf.TextTurn(nil), base...),
		acf.TextTurn{Role: "assistant", Text: "About 1.87 million."})
	raced, ok, reason, err := a.materializeConversationSession(art, makeHead(complete...), "claude-code",
		func(string) error {
			_, writeErr := writer.WriteString(codexConvLine("user", "and the metro area?"))
			return writeErr
		})
	require.NoError(t, err)
	require.False(t, ok)
	require.Equal(t, primary, raced)
	require.Equal(t, adapter.SessionDeclineRace, reason)

	// The raced turn is now on disk and absent from the canonical plan, while
	// the plan holds an answer the rollout lacks: neither is a prefix of the
	// other, and no append will ever converge them.
	diverged, ok, reason, err := a.MaterializeConversationSessionReason(art, makeHead(complete...), "claude-code")
	require.NoError(t, err)
	require.False(t, ok)
	require.Equal(t, primary, diverged)
	require.Equal(t, adapter.SessionDeclineMirrorDiverged, reason,
		"this destination is an Aplexica-owned mirror, so the MIRROR is the side "+
			"holding a turn the other lacks; the canonical-dedupe remedy does not apply")

	// The compatibility entry point keeps its original three-value contract.
	legacyPath, legacyOK, legacyErr := a.MaterializeConversationSession(art, makeHead(complete...), "claude-code")
	require.NoError(t, legacyErr)
	require.False(t, legacyOK)
	require.Equal(t, primary, legacyPath)
}

// A payload this adapter can never transcode is a permanent opt-out, not a
// failure that should ever be retried or escalated.
func TestCodexConversationSessionReason_OptsOutOnUnsupportedPayload(t *testing.T) {
	a, art, _ := codexDeclineFixture(t)
	payload, err := acf.EncodePayload(acf.ConversationPayload{Format: "vendor.unknown/v1", Content: "{}"})
	require.NoError(t, err)
	head := acf.Event{ArtifactID: art.ArtifactID, Type: acf.EventTypeUpdate, Payload: payload}

	path, ok, reason, err := a.MaterializeConversationSessionReason(art, head, "claude-code")
	require.NoError(t, err)
	require.False(t, ok)
	require.Empty(t, path)
	require.Equal(t, adapter.SessionDeclineOptOut, reason)

	path, ok, reason, err = a.ConversationSessionPathReason(art, head, "claude-code")
	require.NoError(t, err)
	require.False(t, ok)
	require.Empty(t, path)
	require.Equal(t, adapter.SessionDeclineOptOut, reason)
}
