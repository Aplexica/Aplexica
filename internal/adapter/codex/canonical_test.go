package codex

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
	"github.com/stretchr/testify/require"
)

func TestEncodeCanonical_TinyFixture(t *testing.T) {
	jsonl, err := os.ReadFile(filepath.Join("testdata", "session-tiny.jsonl"))
	require.NoError(t, err)

	events, err := EncodeCanonical(jsonl)
	require.NoError(t, err)

	// Mapping:
	//   session_meta              → DROP
	//   message/user "hello"      → turn
	//   message/assistant "hi…"   → turn
	//   function_call shell ls    → tool_call
	//   function_call_output      → tool_result
	//   reasoning (encrypted)     → DROP (lossy)
	//   event_msg task_started    → DROP
	// Expected canonical events: 4.
	require.Len(t, events, 4)

	require.Equal(t, acf.EventTypeTurn, events[0].Type)
	require.Equal(t, "user", events[0].Role)
	require.Equal(t, "hello", events[0].Content[0].Text)

	require.Equal(t, acf.EventTypeTurn, events[1].Type)
	require.Equal(t, "assistant", events[1].Role)
	require.Equal(t, "hi! let me list files", events[1].Content[0].Text)

	require.Equal(t, acf.EventTypeToolCall, events[2].Type)
	require.Equal(t, "call_1", events[2].CallID)
	require.Equal(t, "shell", events[2].ToolName)
	require.JSONEq(t, `{"cmd":"ls"}`, string(events[2].Input))

	require.Equal(t, acf.EventTypeToolResult, events[3].Type)
	require.Equal(t, "call_1", events[3].CallID)
	require.Equal(t, "file1\nfile2", events[3].Content[0].Text)
	require.False(t, events[3].IsError)
}

func TestEncodeCanonical_CustomToolMapping(t *testing.T) {
	// Real Codex custom_tool_call carries its invocation under "input" (a JSON
	// string — e.g. apply_patch's patch text), NOT "arguments". The input MUST
	// be preserved or every custom-tool invocation body is silently lost.
	jsonl := []byte(`{"timestamp":"2026-05-21T10:00:00Z","type":"response_item","payload":{"type":"custom_tool_call","name":"apply_patch","input":"*** Begin Patch\n+hi","call_id":"c2"}}
{"timestamp":"2026-05-21T10:00:01Z","type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"c2","output":"OK"}}
`)
	events, err := EncodeCanonical(jsonl)
	require.NoError(t, err)
	require.Len(t, events, 2)
	require.Equal(t, acf.EventTypeToolCall, events[0].Type)
	require.Equal(t, "apply_patch", events[0].ToolName)
	require.Equal(t, "c2", events[0].CallID)
	require.JSONEq(t, `"*** Begin Patch\n+hi"`, string(events[0].Input),
		"custom_tool_call input (patch text) must round-trip into canonical Input")
	require.Equal(t, acf.EventTypeToolResult, events[1].Type)
	require.Equal(t, "c2", events[1].CallID)
	require.Equal(t, "OK", events[1].Content[0].Text)
}

func TestEncodeCanonical_SkipsLocalCommandRows(t *testing.T) {
	jsonl := []byte(`{"timestamp":"2026-07-01T19:00:00Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<command-name>/model</command-name>"}]}}
{"timestamp":"2026-07-01T19:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<local-command-stdout>Set model to Opus 4.8</local-command-stdout>"}]}}
{"timestamp":"2026-07-01T19:00:02Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"What is the distance to Sun?"}]}}
{"timestamp":"2026-07-01T19:00:03Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"About 149.6 million kilometers."}]}}
`)
	events, err := EncodeCanonical(jsonl)
	require.NoError(t, err)
	require.Len(t, events, 2)
	require.Equal(t, "user", events[0].Role)
	require.Equal(t, "What is the distance to Sun?", events[0].Content[0].Text)
	require.Equal(t, "assistant", events[1].Role)
}

func TestEncodeCanonical_SkipsDesktopSyntheticNoResponse(t *testing.T) {
	jsonl := []byte(`{"timestamp":"2026-07-16T22:40:15Z","type":"session_meta","payload":{"cli_version":"0.135.0","aplexica_thread_id":"thread-from-older-aplexica"}}
{"timestamp":"2026-07-16T22:40:16Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"how many planets in solar system?"}]}}
{"timestamp":"2026-07-16T22:40:17Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"No response requested."}]}}
{"timestamp":"2026-07-16T22:40:20Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"There are eight planets."}]}}
`)
	events, err := EncodeCanonical(jsonl)
	require.NoError(t, err)
	require.Len(t, events, 2)
	require.Equal(t, "user", events[0].Role)
	require.Equal(t, "assistant", events[1].Role)
	require.Equal(t, "There are eight planets.", events[1].Content[0].Text)
	require.True(t, generatedCodexSession(jsonl),
		"the durable Aplexica thread marker must recognize legacy generated rollouts")

	native, err := EncodeCanonical([]byte(`{"timestamp":"2026-07-16T22:40:17Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"No response requested."}]}}`))
	require.NoError(t, err)
	require.Len(t, native, 1, "an identical real native reply must not be removed")
}

func TestEncodeCanonical_DeveloperInstructionsAreNotVisibleUserTurns(t *testing.T) {
	jsonl := []byte(`{"timestamp":"2026-07-15T12:00:00Z","type":"response_item","payload":{"type":"message","role":"developer","content":[{"type":"input_text","text":"You are /root, the primary agent."}]}}
{"timestamp":"2026-07-15T12:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Fix the synchronized subjects."}]}}
`)
	events, err := EncodeCanonical(jsonl)
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.Equal(t, []acf.TextTurn{{Role: "user", Text: "Fix the synchronized subjects."}}, acf.ExtractTextTurns(events))
}

func TestEncodeCanonical_GeneratedContinuationDropsHarnessToolsAndCommentary(t *testing.T) {
	jsonl := []byte(`{"timestamp":"2026-07-18T21:47:30Z","type":"session_meta","payload":{"id":"thread","aplexica_thread_id":"thread"}}
{"timestamp":"2026-07-18T21:47:31Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"what is capital of France?"}]}}
{"timestamp":"2026-07-18T21:47:32Z","type":"response_item","payload":{"type":"message","role":"assistant","phase":"final_answer","content":[{"type":"output_text","text":"Paris."}]}}
{"timestamp":"2026-07-18T21:48:29Z","type":"response_item","payload":{"type":"message","role":"developer","content":[{"type":"input_text","text":"<permissions instructions>local execution policy</permissions instructions>"}]}}
{"timestamp":"2026-07-18T21:48:30Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"# AGENTS.md instructions for /tmp/project\nprivate harness"}]}}
{"timestamp":"2026-07-18T21:48:31Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"how many people live in paris?"}]}}
{"timestamp":"2026-07-18T21:48:32Z","type":"response_item","payload":{"type":"message","role":"assistant","phase":"commentary","content":[{"type":"output_text","text":"Searching official figures."}]}}
{"timestamp":"2026-07-18T21:48:33Z","type":"response_item","payload":{"type":"function_call","name":"exec","arguments":"{\"cmd\":\"search\"}","call_id":"call-1"}}
{"timestamp":"2026-07-18T21:48:34Z","type":"response_item","payload":{"type":"function_call_output","call_id":"call-1","output":"large unrelated tool output"}}
{"timestamp":"2026-07-18T21:48:43Z","type":"response_item","payload":{"type":"message","role":"assistant","phase":"final_answer","content":[{"type":"output_text","text":"About 2.1 million."}]}}
`)

	events, err := EncodeCanonical(jsonl)
	require.NoError(t, err)
	require.Equal(t, []acf.TextTurn{
		{Role: "user", Text: "what is capital of France?"},
		{Role: "assistant", Text: "Paris."},
		{Role: "user", Text: "how many people live in paris?"},
		{Role: "assistant", Text: "About 2.1 million."},
	}, acf.ExtractTextTurns(events))
	require.Len(t, events, 4, "generated continuation must contain only portable visible turns")
	for _, event := range events {
		require.Equal(t, acf.EventTypeTurn, event.Type)
		require.Contains(t, []string{"user", "assistant"}, event.Role)
	}
}

func TestSanitizeGeneratedMaterializedEchoes_RemovesPromptReplayAndAnswerRace(t *testing.T) {
	t0 := time.Date(2026, 7, 23, 12, 20, 16, 0, time.UTC)
	events := []acf.ConversationEvent{
		{Type: acf.EventTypeTurn, Role: "user", Timestamp: t0, Content: []acf.ContentBlock{{Type: "text", Text: "q1"}}},
		{Type: acf.EventTypeTurn, Role: "user", Timestamp: t0.Add(time.Second), Content: []acf.ContentBlock{{Type: "text", Text: "q1"}}},
		{Type: acf.EventTypeTurn, Role: "assistant", Timestamp: t0.Add(2 * time.Second), Content: []acf.ContentBlock{{Type: "text", Text: "a1"}}},
		{Type: acf.EventTypeTurn, Role: "assistant", Timestamp: t0.Add(3 * time.Second), Content: []acf.ContentBlock{{Type: "text", Text: "a1"}}},
		{Type: acf.EventTypeTurn, Role: "user", Timestamp: t0.Add(4 * time.Second), Content: []acf.ContentBlock{{Type: "text", Text: "q2"}}},
		{Type: acf.EventTypeTurn, Role: "assistant", Timestamp: t0.Add(5 * time.Second), Content: []acf.ContentBlock{{Type: "text", Text: "a2"}}},
	}
	base := []acf.TextTurn{{Role: "user", Text: "q1"}}
	ref := adapter.ThreadRef{
		MaterializedTurnCount: 1,
		MaterializedTurnsHash: adapter.ConversationTurnsHash(base),
	}

	clean, changed := sanitizeGeneratedMaterializedEchoes(ref, events)
	require.True(t, changed)
	require.Equal(t, []acf.TextTurn{
		{Role: "user", Text: "q1"},
		{Role: "assistant", Text: "a1"},
		{Role: "user", Text: "q2"},
		{Role: "assistant", Text: "a2"},
	}, acf.ExtractTextTurns(clean))
}

func TestSanitizeGeneratedMaterializedEchoes_PreservesDistinctRepeatedPrompt(t *testing.T) {
	t0 := time.Date(2026, 7, 23, 12, 20, 16, 0, time.UTC)
	events := []acf.ConversationEvent{
		{Type: acf.EventTypeTurn, Role: "user", Timestamp: t0, Content: []acf.ContentBlock{{Type: "text", Text: "q1"}}},
		{Type: acf.EventTypeTurn, Role: "assistant", Timestamp: t0.Add(time.Second), Content: []acf.ContentBlock{{Type: "text", Text: "a1"}}},
		{Type: acf.EventTypeTurn, Role: "user", Timestamp: t0.Add(2 * time.Second), Content: []acf.ContentBlock{{Type: "text", Text: "q1"}}},
	}
	base := acf.ExtractTextTurns(events[:2])
	ref := adapter.ThreadRef{
		MaterializedTurnCount: len(base),
		MaterializedTurnsHash: adapter.ConversationTurnsHash(base),
	}

	clean, changed := sanitizeGeneratedMaterializedEchoes(ref, events)
	require.False(t, changed, "a completed base ending in an assistant must not consume a later repeated user prompt")
	require.Equal(t, events, clean)
}

func TestEncodeCanonical_NonJSONArguments(t *testing.T) {
	// Codex stores function_call `arguments` as a JSON-encoded string. Most of
	// the time the string's VALUE is itself a JSON object ("{\"cmd\":\"ls\"}"),
	// but a defensive unwrap must not assume that — a function_call whose
	// arguments is a JSON string holding a NON-JSON value ("ls -la") must still
	// (a) encode without error and (b) produce an Input that survives
	// acf.EncodePayload (which json.Marshals the RawMessage and so validates it).
	jsonl := []byte(`{"timestamp":"2026-05-21T10:00:00Z","type":"response_item","payload":{"type":"function_call","name":"shell","arguments":"ls -la","call_id":"c3"}}
`)
	events, err := EncodeCanonical(jsonl)
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.Equal(t, acf.EventTypeToolCall, events[0].Type)
	require.Equal(t, "shell", events[0].ToolName)
	require.Equal(t, "c3", events[0].CallID)

	// The Input must be a valid JSON value; the bare bytes "ls -la" are not.
	require.True(t, json.Valid(events[0].Input),
		"normalizeCodexArguments must not emit invalid-JSON Input (got %q)", string(events[0].Input))

	// And the full payload must survive the encode path used by the daemon's
	// canonical conversation import (conversation.go conversationEncode →
	// acf.EncodePayload), which is where the bad RawMessage aborts the whole
	// conversation import today.
	_, err = acf.EncodePayload(acf.ConversationPayload{
		Format: acf.ConversationFormatV1,
		Events: events,
	})
	require.NoError(t, err, "canonical payload with non-JSON arguments must survive acf.EncodePayload")
}

func TestDecodeCanonical_RoundTrip(t *testing.T) {
	events := []acf.ConversationEvent{
		{Type: acf.EventTypeTurn, Role: "user", Content: []acf.ContentBlock{{Type: "text", Text: "hi"}}},
		{Type: acf.EventTypeTurn, Role: "assistant", Content: []acf.ContentBlock{{Type: "text", Text: "yo"}}},
		{Type: acf.EventTypeToolCall, CallID: "c1", ToolName: "shell", Input: []byte(`{"cmd":"pwd"}`)},
		{Type: acf.EventTypeToolResult, CallID: "c1", Content: []acf.ContentBlock{{Type: "text", Text: "/tmp"}}},
	}
	jsonl, err := DecodeCanonical(events)
	require.NoError(t, err)
	require.NotEmpty(t, jsonl)

	// Re-encode and confirm we recover the same events (semantic fixed point).
	events2, err := EncodeCanonical(jsonl)
	require.NoError(t, err)
	require.Len(t, events2, 4)
	require.Equal(t, events[0].Role, events2[0].Role)
	require.Equal(t, events[0].Content[0].Text, events2[0].Content[0].Text)
	require.Equal(t, events[2].ToolName, events2[2].ToolName)
	require.Equal(t, events[2].CallID, events2[2].CallID)
	require.Equal(t, events[3].Content[0].Text, events2[3].Content[0].Text)
}
