package acf_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/stretchr/testify/require"
)

func TestConversationEvent_TurnRoundTrip(t *testing.T) {
	ts := time.Date(2026, 5, 21, 10, 0, 0, 0, time.UTC)
	e := acf.ConversationEvent{
		Type:      acf.EventTypeTurn,
		Timestamp: ts,
		Role:      "user",
		Content:   []acf.ContentBlock{{Type: "text", Text: "hello"}},
	}
	data, err := json.Marshal(e)
	require.NoError(t, err)

	var got acf.ConversationEvent
	require.NoError(t, json.Unmarshal(data, &got))
	require.Equal(t, e.Type, got.Type)
	require.Equal(t, e.Role, got.Role)
	require.Len(t, got.Content, 1)
	require.Equal(t, "text", got.Content[0].Type)
	require.Equal(t, "hello", got.Content[0].Text)
	require.True(t, e.Timestamp.Equal(got.Timestamp))
}

func TestConversationEvent_ToolCallRoundTrip(t *testing.T) {
	ts := time.Date(2026, 5, 21, 10, 0, 0, 0, time.UTC)
	e := acf.ConversationEvent{
		Type:      acf.EventTypeToolCall,
		Timestamp: ts,
		CallID:    "call_abc",
		ToolName:  "bash",
		Input:     json.RawMessage(`{"command":"ls"}`),
	}
	data, err := json.Marshal(e)
	require.NoError(t, err)
	var got acf.ConversationEvent
	require.NoError(t, json.Unmarshal(data, &got))
	require.Equal(t, "call_abc", got.CallID)
	require.Equal(t, "bash", got.ToolName)
	require.JSONEq(t, `{"command":"ls"}`, string(got.Input))
}

func TestConversationEvent_ToolResultRoundTrip(t *testing.T) {
	e := acf.ConversationEvent{
		Type:    acf.EventTypeToolResult,
		CallID:  "call_abc",
		Content: []acf.ContentBlock{{Type: "text", Text: "file1\nfile2"}},
		IsError: false,
	}
	data, err := json.Marshal(e)
	require.NoError(t, err)
	var got acf.ConversationEvent
	require.NoError(t, json.Unmarshal(data, &got))
	require.Equal(t, "call_abc", got.CallID)
	require.Equal(t, false, got.IsError)
	require.Len(t, got.Content, 1)
}

func TestConversationEvent_SystemNoteRoundTrip(t *testing.T) {
	e := acf.ConversationEvent{
		Type:    acf.EventTypeSystemNote,
		Content: []acf.ContentBlock{{Type: "text", Text: "compaction"}},
		Tags:    []string{"aplexica:compact"},
	}
	data, err := json.Marshal(e)
	require.NoError(t, err)
	var got acf.ConversationEvent
	require.NoError(t, json.Unmarshal(data, &got))
	require.Equal(t, acf.EventTypeSystemNote, got.Type)
	require.Equal(t, []string{"aplexica:compact"}, got.Tags)
}

func TestConversationPayload_StructuredMode(t *testing.T) {
	p := acf.ConversationPayload{
		Format: acf.ConversationFormatV1,
		Events: []acf.ConversationEvent{
			{Type: acf.EventTypeTurn, Role: "user", Content: []acf.ContentBlock{{Type: "text", Text: "hi"}}},
		},
	}
	data, err := json.Marshal(p)
	require.NoError(t, err)
	var got acf.ConversationPayload
	require.NoError(t, json.Unmarshal(data, &got))
	require.Equal(t, acf.ConversationFormatV1, got.Format)
	require.Empty(t, got.Content, "Content empty when in structured mode")
	require.Len(t, got.Events, 1)
}

func TestMaterializedConversationPayload_ComposesDelta(t *testing.T) {
	baseEvents := []acf.ConversationEvent{
		{Type: acf.EventTypeTurn, Role: "user", Content: []acf.ContentBlock{{Type: "text", Text: "hello"}}},
		{Type: acf.EventTypeTurn, Role: "assistant", Content: []acf.ContentBlock{{Type: "text", Text: "hi"}}},
	}
	deltaEvents := []acf.ConversationEvent{
		{Type: acf.EventTypeTurn, Role: "user", Content: []acf.ContentBlock{{Type: "text", Text: "next"}}},
		{Type: acf.EventTypeTurn, Role: "assistant", Content: []acf.ContentBlock{{Type: "text", Text: "ok"}}},
	}
	base, err := acf.EncodePayload(acf.ConversationPayload{Format: acf.ConversationFormatV1, Events: baseEvents})
	require.NoError(t, err)
	delta, err := acf.EncodePayload(acf.ConversationPayload{Format: acf.ConversationDeltaFormatV1, Events: deltaEvents})
	require.NoError(t, err)

	got, ok, err := acf.MaterializedConversationPayload([]acf.Event{
		{Type: acf.EventTypeCreate, Payload: base},
		{Type: acf.EventTypeUpdate, Payload: delta},
	})
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, acf.ConversationFormatV1, got.Format)
	require.Len(t, got.Events, 4)
	require.Equal(t, "next", got.Events[2].Content[0].Text)

	encoded, ok, err := acf.MaterializedConversationPayloadBytes([]acf.Event{
		{Type: acf.EventTypeCreate, Payload: base},
		{Type: acf.EventTypeUpdate, Payload: delta},
	})
	require.NoError(t, err)
	require.True(t, ok)
	var payload acf.ConversationPayload
	require.NoError(t, json.Unmarshal(encoded, &payload))
	require.Equal(t, acf.ConversationFormatV1, payload.Format)
	require.Len(t, payload.Events, 4)
}

func TestConversationEvent_ForkRoundTrip(t *testing.T) {
	e := acf.ConversationEvent{
		Type:          acf.EventTypeFork,
		Timestamp:     time.Date(2026, 5, 21, 10, 0, 0, 0, time.UTC),
		BranchID:      "experiment-1",
		SourceEventID: "evt-abc",
		Tags:          []string{"experiment-start"},
	}
	data, err := json.Marshal(e)
	require.NoError(t, err)
	require.Contains(t, string(data), `"type":"fork"`)
	require.Contains(t, string(data), `"branch_id":"experiment-1"`)
	require.Contains(t, string(data), `"source_event_id":"evt-abc"`)

	var got acf.ConversationEvent
	require.NoError(t, json.Unmarshal(data, &got))
	require.Equal(t, acf.EventTypeFork, got.Type)
	require.Equal(t, "experiment-1", got.BranchID)
	require.Equal(t, "evt-abc", got.SourceEventID)
}

func TestConversationEvent_MergeRoundTrip(t *testing.T) {
	e := acf.ConversationEvent{
		Type:            acf.EventTypeMerge,
		BranchID:        "main",
		MergedBranchIDs: []string{"experiment-1", "experiment-2"},
	}
	data, err := json.Marshal(e)
	require.NoError(t, err)
	var got acf.ConversationEvent
	require.NoError(t, json.Unmarshal(data, &got))
	require.Equal(t, acf.EventTypeMerge, got.Type)
	require.Equal(t, []string{"experiment-1", "experiment-2"}, got.MergedBranchIDs)
}

func TestConversationEvent_SnapshotRoundTrip(t *testing.T) {
	e := acf.ConversationEvent{
		Type:          acf.EventTypeSnapshot,
		BranchID:      "main",
		SnapshotState: "sha256:abc123",
	}
	data, err := json.Marshal(e)
	require.NoError(t, err)
	var got acf.ConversationEvent
	require.NoError(t, json.Unmarshal(data, &got))
	require.Equal(t, acf.EventTypeSnapshot, got.Type)
	require.Equal(t, "sha256:abc123", got.SnapshotState)
}

func TestConversationEvent_OmitemptyHoldsForBranchFields(t *testing.T) {
	e := acf.ConversationEvent{
		Type: acf.EventTypeTurn, Role: "user",
		Content: []acf.ContentBlock{{Type: "text", Text: "hi"}},
	}
	data, err := json.Marshal(e)
	require.NoError(t, err)
	require.NotContains(t, string(data), "branch_id")
	require.NotContains(t, string(data), "source_event_id")
	require.NotContains(t, string(data), "merged_branch_ids")
	require.NotContains(t, string(data), "snapshot_state")
}

func TestConversationPayload_LegacyOpaqueModeStillWorks(t *testing.T) {
	p := acf.ConversationPayload{
		Format:  "claude-code.session.jsonl",
		Content: `{"type":"user","content":"hi"}`,
	}
	data, err := json.Marshal(p)
	require.NoError(t, err)
	var got acf.ConversationPayload
	require.NoError(t, json.Unmarshal(data, &got))
	require.Equal(t, "claude-code.session.jsonl", got.Format)
	require.Equal(t, p.Content, got.Content)
	require.Empty(t, got.Events, "Events empty when in legacy opaque mode")
}
