package claudecode

import (
	"testing"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/stretchr/testify/require"
)

// Real Claude Code tool_result blocks carry the payload under `content`
// (string OR array of blocks), NOT `text`. It must be preserved.
func TestEncodeCanonical_ToolResultContent_String(t *testing.T) {
	jsonl := []byte(`{"type":"user","timestamp":"2026-05-21T10:00:00Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu1","content":"file1\nfile2","is_error":false}]}}
`)
	events, err := EncodeCanonical(jsonl)
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.Equal(t, acf.EventTypeToolResult, events[0].Type)
	require.Equal(t, "tu1", events[0].CallID)
	require.Equal(t, "file1\nfile2", events[0].Content[0].Text, "tool_result content must be preserved")
}

func TestEncodeCanonical_ToolResultContent_Array(t *testing.T) {
	jsonl := []byte(`{"type":"user","timestamp":"2026-05-21T10:00:00Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu2","content":[{"type":"text","text":"part1"},{"type":"text","text":"part2"}],"is_error":true}]}}
`)
	events, err := EncodeCanonical(jsonl)
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.Equal(t, acf.EventTypeToolResult, events[0].Type)
	require.Equal(t, "tu2", events[0].CallID)
	require.True(t, events[0].IsError)
	require.Equal(t, "part1part2", events[0].Content[0].Text, "array-shaped tool_result content must be concatenated")
}

// Assistant `thinking` blocks must be preserved (currently dropped, so
// thinking-only turns vanished entirely from the canonical log).
func TestEncodeCanonical_ThinkingPreserved(t *testing.T) {
	jsonl := []byte(`{"type":"assistant","timestamp":"2026-05-21T10:00:00Z","message":{"role":"assistant","content":[{"type":"thinking","thinking":"let me reason","signature":"sig"},{"type":"text","text":"the answer"}]}}
`)
	events, err := EncodeCanonical(jsonl)
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.Equal(t, acf.EventTypeTurn, events[0].Type)
	require.Len(t, events[0].Content, 2)
	require.Equal(t, "thinking", events[0].Content[0].Type)
	require.Equal(t, "let me reason", events[0].Content[0].Text)
	require.Equal(t, "text", events[0].Content[1].Type)
	require.Equal(t, "the answer", events[0].Content[1].Text)
}

func TestEncodeCanonical_ThinkingOnlyTurnEmits(t *testing.T) {
	jsonl := []byte(`{"type":"assistant","timestamp":"2026-05-21T10:00:00Z","message":{"role":"assistant","content":[{"type":"thinking","thinking":"silent reasoning"}]}}
`)
	events, err := EncodeCanonical(jsonl)
	require.NoError(t, err)
	require.Len(t, events, 1, "a thinking-only assistant turn must still emit an event")
	require.Equal(t, "thinking", events[0].Content[0].Type)
	require.Equal(t, "silent reasoning", events[0].Content[0].Text)
}

// User-pasted image blocks must be preserved (ContentBlock.Data), not dropped.
func TestEncodeCanonical_UserImagePreserved(t *testing.T) {
	jsonl := []byte(`{"type":"user","timestamp":"2026-05-21T10:00:00Z","message":{"role":"user","content":[{"type":"text","text":"see this"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}]}}
`)
	events, err := EncodeCanonical(jsonl)
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.Equal(t, acf.EventTypeTurn, events[0].Type)
	require.Len(t, events[0].Content, 2)
	require.Equal(t, "text", events[0].Content[0].Type)
	require.Equal(t, "image", events[0].Content[1].Type)
	require.Equal(t, "AAAA", events[0].Content[1].Data)
}
