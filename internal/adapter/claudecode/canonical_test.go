package claudecode

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/stretchr/testify/require"
)

func TestEncodeCanonical_TinyFixture(t *testing.T) {
	jsonl, err := os.ReadFile(filepath.Join("testdata", "session-tiny.jsonl"))
	require.NoError(t, err)

	events, err := EncodeCanonical(jsonl)
	require.NoError(t, err)

	// Fixture has 6 native rows. Mapping:
	//   user "hello"                                 → turn
	//   assistant text + tool_use                    → turn + tool_call
	//   user tool_result                             → tool_result (no turn)
	//   assistant text-only                          → turn
	//   system                                       → system_note
	//   queue-operation                              → SKIPPED (lossy)
	// Expected canonical events: 6 (5 content events + 1 system_note).
	require.Len(t, events, 6)

	require.Equal(t, acf.EventTypeTurn, events[0].Type)
	require.Equal(t, "user", events[0].Role)
	require.Equal(t, "hello", events[0].Content[0].Text)

	require.Equal(t, acf.EventTypeTurn, events[1].Type)
	require.Equal(t, "assistant", events[1].Role)
	// Assistant turn should contain ONLY the text block, NOT the tool_use
	// (tool_use becomes the next event).
	require.Len(t, events[1].Content, 1)
	require.Equal(t, "hi! let me list files", events[1].Content[0].Text)

	require.Equal(t, acf.EventTypeToolCall, events[2].Type)
	require.Equal(t, "call_1", events[2].CallID)
	require.Equal(t, "bash", events[2].ToolName)
	require.JSONEq(t, `{"command":"ls"}`, string(events[2].Input))

	require.Equal(t, acf.EventTypeToolResult, events[3].Type)
	require.Equal(t, "call_1", events[3].CallID)
	require.False(t, events[3].IsError)

	require.Equal(t, acf.EventTypeTurn, events[4].Type)
	require.Equal(t, "assistant", events[4].Role)
	require.Equal(t, "there are two files", events[4].Content[0].Text)

	// system row → system_note. queue-operation is dropped.
	require.Equal(t, acf.EventTypeSystemNote, events[5].Type)
	require.Equal(t, "context compaction", events[5].Content[0].Text)
}

func TestDecodeCanonical_RoundTripsTurns(t *testing.T) {
	events := []acf.ConversationEvent{
		{Type: acf.EventTypeTurn, Role: "user", Content: []acf.ContentBlock{{Type: "text", Text: "hi"}}},
		{Type: acf.EventTypeTurn, Role: "assistant", Content: []acf.ContentBlock{{Type: "text", Text: "hello back"}}},
	}
	jsonl, err := DecodeCanonical(events)
	require.NoError(t, err)
	// Must produce 2 lines, each a valid JSON object with "type" = user/assistant.
	require.NotEmpty(t, jsonl)
	require.Contains(t, string(jsonl), `"type":"user"`)
	require.Contains(t, string(jsonl), `"type":"assistant"`)
}

func TestEncodeCanonical_RealClaudeCodeMessageWrappedShape(t *testing.T) {
	// Real Claude Code .jsonl files wrap user/assistant content under a
	// .message object; the parser must accept that shape too. Smoke test:
	// one user row in real shape + one assistant row in real shape.
	jsonl := []byte(`{"type":"user","timestamp":"2026-05-21T10:00:00.000Z","message":{"role":"user","content":"hi from real shape"}}
{"type":"assistant","timestamp":"2026-05-21T10:00:01.000Z","message":{"role":"assistant","content":[{"type":"text","text":"hello"}]}}
`)
	events, err := EncodeCanonical(jsonl)
	require.NoError(t, err)
	require.Len(t, events, 2)
	require.Equal(t, "user", events[0].Role)
	require.Equal(t, "hi from real shape", events[0].Content[0].Text)
	require.Equal(t, "assistant", events[1].Role)
	require.Equal(t, "hello", events[1].Content[0].Text)
}

func TestEncodeCanonical_SkipsLocalCommandRows(t *testing.T) {
	jsonl := []byte(`{"type":"user","timestamp":"2026-07-01T19:00:00.000Z","message":{"role":"user","content":"<command-name>/model</command-name>"}}
{"type":"user","timestamp":"2026-07-01T19:00:00.001Z","message":{"role":"user","content":"<command-message>model</command-message>"}}
{"type":"user","timestamp":"2026-07-01T19:00:00.002Z","message":{"role":"user","content":"<local-command-stdout>Set model to Opus 4.8</local-command-stdout>"}}
{"type":"user","timestamp":"2026-07-01T19:00:11.000Z","message":{"role":"user","content":"What is the distance to Sun?"}}
{"type":"assistant","timestamp":"2026-07-01T19:00:20.000Z","message":{"role":"assistant","content":[{"type":"text","text":"About 149.6 million kilometers."}]}}
`)
	events, err := EncodeCanonical(jsonl)
	require.NoError(t, err)
	require.Len(t, events, 2)
	require.Equal(t, "user", events[0].Role)
	require.Equal(t, "What is the distance to Sun?", events[0].Content[0].Text)
	require.Equal(t, "assistant", events[1].Role)
}

func TestEncodeCanonical_SkipsDesktopSyntheticAssistantReply(t *testing.T) {
	jsonl := []byte(`{"type":"user","timestamp":"2026-07-16T22:40:19Z","message":{"role":"user","content":"how many planets in solar system?"}}
{"type":"assistant","timestamp":"2026-07-16T22:40:49Z","message":{"role":"assistant","model":"<synthetic>","content":[{"type":"text","text":"No response requested."}]}}
{"type":"assistant","timestamp":"2026-07-16T22:40:50Z","message":{"role":"assistant","model":"claude-opus-4-8","content":[{"type":"text","text":"There are eight planets."}]}}
`)
	events, err := EncodeCanonical(jsonl)
	require.NoError(t, err)
	require.Len(t, events, 2)
	require.Equal(t, "user", events[0].Role)
	require.Equal(t, "assistant", events[1].Role)
	require.Equal(t, "There are eight planets.", events[1].Content[0].Text)
}

func TestRoundTrip_FixedPointAfterTwoPasses(t *testing.T) {
	// Spec claim: round-trip is semantically stable, not byte-identical.
	// Encode → Decode → Encode must produce the SAME canonical events.
	jsonl, err := os.ReadFile(filepath.Join("testdata", "session-tiny.jsonl"))
	require.NoError(t, err)
	// Append thinking-only and image-only probe rows so the fixed point is
	// exercised over non-text blocks too (the shared fixture has none).
	jsonl = append(jsonl, []byte(
		`{"type":"assistant","timestamp":"2026-05-21T10:00:06.000Z","content":[{"type":"thinking","thinking":"silent reasoning"}]}`+"\n"+
			`{"type":"user","timestamp":"2026-05-21T10:00:07.000Z","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}]}`+"\n",
	)...)

	pass1, err := EncodeCanonical(jsonl)
	require.NoError(t, err)

	emitted, err := DecodeCanonical(pass1)
	require.NoError(t, err)

	pass2, err := EncodeCanonical(emitted)
	require.NoError(t, err)

	require.Len(t, pass2, len(pass1),
		"round-trip must be a fixed point: encode→decode→encode same length")
	for i := range pass1 {
		require.Equal(t, pass1[i].Type, pass2[i].Type, "event %d type mismatch", i)
		require.Equal(t, pass1[i].Role, pass2[i].Role, "event %d role mismatch", i)
		require.Equal(t, pass1[i].CallID, pass2[i].CallID, "event %d call_id mismatch", i)
		require.Equal(t, pass1[i].ToolName, pass2[i].ToolName, "event %d tool_name mismatch", i)
		require.Equal(t, pass1[i].Content, pass2[i].Content, "event %d content mismatch", i)
	}
}

// DecodeCanonical must preserve EVERY content block on a turn, not just the
// first text block. A thinking-only or image-only turn would otherwise decode
// to an empty row and vanish on the next encode — breaking the documented
// "thinking text / image data are preserved" round-trip claim.
func TestDecodeCanonical_PreservesThinkingAndImageBlocks(t *testing.T) {
	events := []acf.ConversationEvent{
		// Thinking-only assistant turn.
		{Type: acf.EventTypeTurn, Role: "assistant", Content: []acf.ContentBlock{
			{Type: "thinking", Text: "silent reasoning"},
		}},
		// Image-only user turn.
		{Type: acf.EventTypeTurn, Role: "user", Content: []acf.ContentBlock{
			{Type: "image", Data: "AAAA"},
		}},
		// Mixed thinking + text assistant turn (order must survive).
		{Type: acf.EventTypeTurn, Role: "assistant", Content: []acf.ContentBlock{
			{Type: "thinking", Text: "let me reason"},
			{Type: "text", Text: "the answer"},
		}},
		// Mixed text + image user turn.
		{Type: acf.EventTypeTurn, Role: "user", Content: []acf.ContentBlock{
			{Type: "text", Text: "see this"},
			{Type: "image", Data: "BBBB"},
		}},
	}

	jsonl, err := DecodeCanonical(events)
	require.NoError(t, err)

	got, err := EncodeCanonical(jsonl)
	require.NoError(t, err)

	require.Len(t, got, len(events), "every turn must survive decode→encode")
	for i := range events {
		require.Equal(t, events[i].Type, got[i].Type, "event %d type mismatch", i)
		require.Equal(t, events[i].Role, got[i].Role, "event %d role mismatch", i)
		require.Equal(t, events[i].Content, got[i].Content, "event %d content mismatch", i)
	}
}

// TestImportConversation_CanonicalMode lives in canonical_wire_test.go —
// added by the Task-3 wiring commit since it references the
// CanonicalConversations adapter field.
