package codex

import (
	"testing"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/stretchr/testify/require"
)

// web_search_call is a first-class response_item type in current Codex
// (codex-rs/protocol/src/models.rs). It must not be silently dropped.
func TestEncodeCanonical_WebSearchCall(t *testing.T) {
	jsonl := []byte(`{"timestamp":"2026-05-25T10:00:00Z","type":"response_item","payload":{"type":"web_search_call","status":"completed","call_id":"ws1","action":{"type":"search","queries":["golang generics"]}}}
`)
	events, err := EncodeCanonical(jsonl)
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.Equal(t, acf.EventTypeToolCall, events[0].Type)
	require.Equal(t, "web_search", events[0].ToolName)
	require.JSONEq(t, `{"type":"search","queries":["golang generics"]}`, string(events[0].Input))
}

// input_image content blocks must be preserved (ContentBlock.Data is the
// documented home for non-text blocks), not dropped to an empty/text-only turn.
func TestEncodeCanonical_InputImagePreserved(t *testing.T) {
	jsonl := []byte(`{"timestamp":"2026-05-25T10:00:00Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"look at this"},{"type":"input_image","image_url":"data:image/png;base64,AAAA"}]}}
`)
	events, err := EncodeCanonical(jsonl)
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.Equal(t, acf.EventTypeTurn, events[0].Type)
	require.Len(t, events[0].Content, 2)
	require.Equal(t, "text", events[0].Content[0].Type)
	require.Equal(t, "look at this", events[0].Content[0].Text)
	require.Equal(t, "image", events[0].Content[1].Type)
	require.Equal(t, "data:image/png;base64,AAAA", events[0].Content[1].Data)
}

// An image-only user message must still emit a turn (previously dropped because
// the text was empty).
func TestEncodeCanonical_ImageOnlyMessageEmitsTurn(t *testing.T) {
	jsonl := []byte(`{"timestamp":"2026-05-25T10:00:00Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_image","image_url":"https://x/y.png"}]}}
`)
	events, err := EncodeCanonical(jsonl)
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.Equal(t, "image", events[0].Content[0].Type)
	require.Equal(t, "https://x/y.png", events[0].Content[0].Data)
}
