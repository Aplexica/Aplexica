package codex

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/stretchr/testify/require"
)

func TestImport_GeneratedContinuationStreamsPromptThenFinalAnswer(t *testing.T) {
	root := t.TempDir()
	store := &acf.Store{Root: filepath.Join(root, "store")}
	require.NoError(t, store.Init())
	a := New()
	a.HomeDir = root
	a.CanonicalConversations = true

	original := filepath.Join(root, ".codex", "sessions", "native.jsonl")
	baseNative := codexConvLine("user", "what is capital of Poland") +
		codexConvLine("assistant", "Warsaw.")
	writeFile(t, original, baseNative)
	ids, err := a.ImportConversation(context.Background(), store, original)
	require.NoError(t, err)
	require.Len(t, ids, 1)
	artifactID := ids[0]

	baseTurns := []acf.TextTurn{
		{Role: "user", Text: "what is capital of Poland"},
		{Role: "assistant", Text: "Warsaw."},
	}
	rollout := transcodeToCodexRollout(baseTurns, artifactID, artifactID, acf.MainBranch,
		root, "claude-code", time.Date(2026, 7, 18, 20, 10, 54, 0, time.UTC))
	pending := rollout + strings.Join([]string{
		`{"timestamp":"2026-07-18T20:11:58Z","type":"event_msg","payload":{"type":"task_started","turn_id":"turn-2"}}`,
		`{"timestamp":"2026-07-18T20:11:58Z","type":"response_item","payload":{"type":"message","role":"developer","content":[{"type":"input_text","text":"<permissions instructions>private harness</permissions instructions>"}]}}`,
		`{"timestamp":"2026-07-18T20:11:59Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"how many people live in warsaw?"}]}}`,
		`{"timestamp":"2026-07-18T20:12:02Z","type":"response_item","payload":{"type":"web_search_call","call_id":"search-1","action":{"type":"search","query":"Warsaw population"}}}`,
		`{"timestamp":"2026-07-18T20:12:03Z","type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"search-1","output":"large tool output"}}`,
		`{"timestamp":"2026-07-18T20:12:04Z","type":"response_item","payload":{"type":"message","role":"assistant","phase":"commentary","content":[{"type":"output_text","text":"Still working."}]}}`,
	}, "\n") + "\n"
	generated := filepath.Join(root, ".codex", "sessions", "2026", "07", "18", "rollout-thread.jsonl")
	writeFile(t, generated, pending)

	gotIDs, err := a.Import(context.Background(), store, generated)
	require.NoError(t, err)
	require.Equal(t, []string{artifactID}, gotIDs,
		"the watcher/import boundary must commit the completed user prompt before the answer")
	events, err := store.ReadEvents(acf.KindConversation, artifactID)
	require.NoError(t, err)
	require.Len(t, events, 2)
	payload, ok, err := acf.MaterializedConversationPayload(events)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, append(baseTurns, acf.TextTurn{Role: "user", Text: "how many people live in warsaw?"}),
		acf.ExtractTextTurns(payload.Events))
	require.Len(t, payload.Events, 3, "harness, commentary, and tool traffic must not enter the portable update")

	complete := pending + strings.Join([]string{
		`{"timestamp":"2026-07-18T20:12:06Z","type":"response_item","payload":{"type":"message","role":"assistant","phase":"final_answer","content":[{"type":"output_text","text":"Warsaw has about 1.87 million residents."}]}}`,
		`{"timestamp":"2026-07-18T20:12:07Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"turn-2"}}`,
	}, "\n") + "\n"
	require.NoError(t, os.WriteFile(generated, []byte(complete), 0o644))

	gotIDs, err = a.Import(context.Background(), store, generated)
	require.NoError(t, err)
	require.Equal(t, []string{artifactID}, gotIDs)
	events, err = store.ReadEvents(acf.KindConversation, artifactID)
	require.NoError(t, err)
	require.Len(t, events, 3)
	payload, ok, err = acf.MaterializedConversationPayload(events)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, []acf.TextTurn{
		{Role: "user", Text: "what is capital of Poland"},
		{Role: "assistant", Text: "Warsaw."},
		{Role: "user", Text: "how many people live in warsaw?"},
		{Role: "assistant", Text: "Warsaw has about 1.87 million residents."},
	}, acf.ExtractTextTurns(payload.Events))
}

func TestImport_GeneratedContinuationRepairsLegacyCanonicalInternals(t *testing.T) {
	root := t.TempDir()
	store := &acf.Store{Root: filepath.Join(root, "store")}
	require.NoError(t, store.Init())
	a := New()
	a.HomeDir = root
	a.CanonicalConversations = true

	original := filepath.Join(root, ".codex", "sessions", "native.jsonl")
	baseTurns := []acf.TextTurn{
		{Role: "user", Text: "what is capital of France?"},
		{Role: "assistant", Text: "Paris."},
	}
	writeFile(t, original, codexConvLine("user", baseTurns[0].Text)+codexConvLine("assistant", baseTurns[1].Text))
	ids, err := a.ImportConversation(context.Background(), store, original)
	require.NoError(t, err)
	require.Len(t, ids, 1)
	artifactID := ids[0]

	allTurns := append(append([]acf.TextTurn(nil), baseTurns...),
		acf.TextTurn{Role: "user", Text: "how many people live in Paris?"},
		acf.TextTurn{Role: "assistant", Text: "About 2.1 million."},
	)
	polluted := []acf.ConversationEvent{
		textTurnEvent(baseTurns[0]), textTurnEvent(baseTurns[1]),
		{Type: acf.EventTypeSystemNote, Content: []acf.ContentBlock{{Type: "text", Text: "<permissions instructions>private harness"}}},
		textTurnEvent(allTurns[2]),
		textTurnEvent(acf.TextTurn{Role: "assistant", Text: "Searching the web"}),
		{Type: acf.EventTypeToolCall, CallID: "call-1", ToolName: "exec"},
		{Type: acf.EventTypeToolResult, CallID: "call-1", Content: []acf.ContentBlock{{Type: "text", Text: "private output"}}},
		textTurnEvent(allTurns[3]),
	}
	payload, err := acf.EncodePayload(acf.ConversationPayload{Format: acf.ConversationFormatV1, Events: polluted})
	require.NoError(t, err)
	parent, ok, err := store.LastEvent(acf.KindConversation, artifactID)
	require.NoError(t, err)
	require.True(t, ok)
	require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
		EventID: acf.NewID(), ArtifactID: artifactID, Type: acf.EventTypeUpdate,
		Timestamp: time.Now().UTC(), ParentHash: parent.Hash, Payload: payload,
	}))

	art, err := store.ReadArtifact(acf.KindConversation, artifactID)
	require.NoError(t, err)
	materialized, materializedHead, ok, err := store.MaterializedConversationHeadFromStore(artifactID)
	require.NoError(t, err)
	require.True(t, ok)
	materializedHead.MaterializedConversation = &materialized
	plan, ok, err := a.conversationSessionPlan(art, materializedHead)
	require.NoError(t, err)
	require.True(t, ok)
	generated := plan.dest
	generatedRaw := transcodeToCodexRollout(
		baseTurns, plan.sessionID, artifactID, plan.branchID, root, "claude-code", plan.sessionTime,
	) + strings.Join([]string{
		`{"timestamp":"2026-07-18T20:12:07Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"how many people live in Paris?"}]}}`,
		`{"timestamp":"2026-07-18T20:12:08Z","type":"response_item","payload":{"type":"message","role":"developer","content":[{"type":"input_text","text":"<permissions instructions>private harness"}]}}`,
		`{"timestamp":"2026-07-18T20:12:08.5Z","type":"response_item","payload":{"type":"message","role":"assistant","phase":"commentary","content":[{"type":"output_text","text":"Searching the web"}]}}`,
		`{"timestamp":"2026-07-18T20:12:09Z","type":"response_item","payload":{"type":"function_call","call_id":"call-1","name":"exec","arguments":"{}"}}`,
		`{"timestamp":"2026-07-18T20:12:10Z","type":"response_item","payload":{"type":"function_call_output","call_id":"call-1","output":"private output"}}`,
		`{"timestamp":"2026-07-18T20:12:11Z","type":"response_item","payload":{"type":"message","role":"assistant","phase":"final_answer","content":[{"type":"output_text","text":"About 2.1 million."}]}}`,
	}, "\n") + "\n"
	writeFile(t, generated, generatedRaw)
	gotIDs, err := a.Import(context.Background(), store, generated)
	require.NoError(t, err)
	require.Equal(t, []string{artifactID}, gotIDs)

	events, err := store.ReadEvents(acf.KindConversation, artifactID)
	require.NoError(t, err)
	require.Len(t, events, 3)
	head, err := acf.DecodeConversationPayload(events[2])
	require.NoError(t, err)
	require.Equal(t, acf.ConversationFormatV1, head.Format)
	require.Len(t, head.Events, len(allTurns))
	require.Equal(t, allTurns, acf.ExtractTextTurns(head.Events))
	for _, event := range head.Events {
		require.Equal(t, acf.EventTypeTurn, event.Type)
		require.Contains(t, []string{"user", "assistant"}, event.Role)
	}
}

func TestImport_GeneratedContinuationRepairsCleanNativeEchoAndPollutedHead(t *testing.T) {
	root := t.TempDir()
	store := &acf.Store{Root: filepath.Join(root, "store")}
	require.NoError(t, store.Init())
	a := New()
	a.HomeDir = root
	a.CanonicalConversations = true

	base := []acf.TextTurn{
		{Role: "user", Text: "what is capital of Poland?"},
		{Role: "assistant", Text: "Warsaw."},
		{Role: "user", Text: "how many people live in Warsaw?"},
	}
	original := filepath.Join(root, ".codex", "sessions", "native.jsonl")
	writeFile(t, original, codexConvLine("user", base[0].Text)+codexConvLine("assistant", base[1].Text))
	ids, err := a.ImportConversation(context.Background(), store, original)
	require.NoError(t, err)
	require.Len(t, ids, 1)
	artifactID := ids[0]
	answer := acf.TextTurn{Role: "assistant", Text: "Warsaw has about 1.87 million residents."}
	polluted := []acf.ConversationEvent{
		textTurnEvent(base[0]), textTurnEvent(base[1]),
		{Type: acf.EventTypeSystemNote, Content: []acf.ContentBlock{{Type: "text", Text: "<permissions instructions>private harness</permissions instructions>"}}},
		textTurnEvent(base[2]), textTurnEvent(answer), textTurnEvent(answer),
	}
	payload, err := acf.EncodePayload(acf.ConversationPayload{Format: acf.ConversationFormatV1, Events: polluted})
	require.NoError(t, err)
	parent, ok, err := store.LastEvent(acf.KindConversation, artifactID)
	require.NoError(t, err)
	require.True(t, ok)
	require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
		EventID: acf.NewID(), ArtifactID: artifactID, Type: acf.EventTypeUpdate,
		Timestamp: time.Now().UTC(), ParentHash: parent.Hash,
		Provenance: acf.Provenance{SourceAgent: "codex", AdapterVersion: "0.9.2"}, Payload: payload,
	}))

	// The native rollout is already free of the old harness, but contains the
	// legacy duplicate final-answer echo after the exact three-turn stamped
	// materialized base. The durable marker must still trigger canonical repair.
	art, err := store.ReadArtifact(acf.KindConversation, artifactID)
	require.NoError(t, err)
	_, materializedHead, ok, err := store.MaterializedConversationHeadFromStore(artifactID)
	require.NoError(t, err)
	require.True(t, ok)
	plan, ok, err := a.conversationSessionPlan(art, materializedHead)
	require.NoError(t, err)
	require.True(t, ok)
	generatedRaw := transcodeToCodexRollout(
		base, plan.sessionID, artifactID, plan.branchID, root, "claude-code", plan.sessionTime,
	) + strings.Join([]string{
		`{"timestamp":"2026-07-18T20:12:11Z","type":"response_item","payload":{"type":"message","role":"assistant","phase":"final_answer","content":[{"type":"output_text","text":"Warsaw has about 1.87 million residents."}]}}`,
		`{"timestamp":"2026-07-18T20:12:12Z","type":"response_item","payload":{"type":"message","role":"assistant","phase":"final_answer","content":[{"type":"output_text","text":"Warsaw has about 1.87 million residents."}]}}`,
		`{"timestamp":"2026-07-18T20:12:13Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"turn-2"}}`,
	}, "\n") + "\n"
	generated := plan.dest
	writeFile(t, generated, generatedRaw)

	gotIDs, err := a.Import(context.Background(), store, generated)
	require.NoError(t, err)
	require.Equal(t, []string{artifactID}, gotIDs)
	events, err := store.ReadEvents(acf.KindConversation, artifactID)
	require.NoError(t, err)
	head, err := acf.DecodeConversationPayload(events[len(events)-1])
	require.NoError(t, err)
	require.Equal(t, acf.ConversationFormatV1, head.Format)
	require.Equal(t, []acf.TextTurn{base[0], base[1], base[2], answer}, acf.ExtractTextTurns(head.Events))
	require.Len(t, head.Events, 4)
	for _, event := range head.Events {
		require.Equal(t, acf.EventTypeTurn, event.Type)
	}
}

func textTurnEvent(turn acf.TextTurn) acf.ConversationEvent {
	return acf.ConversationEvent{
		Type: acf.EventTypeTurn, Role: turn.Role,
		Content: []acf.ContentBlock{{Type: "text", Text: turn.Text}},
	}
}
