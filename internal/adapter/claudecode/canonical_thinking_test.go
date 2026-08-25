package claudecode

import (
	"testing"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/stretchr/testify/require"
)

// Claude Code ≥2.1.204 writes one assistant API message as multiple JSONL
// records sharing message.id — an interleaved-thinking record whose thinking
// text is EMPTY (signature only, which we deliberately do not carry), then
// the text record. The empty record must not become a canonical turn: it has
// zero renderable content, and once materialized on another device it shows
// up as a phantom assistant message.
func TestEncodeCanonical_EmptyThinkingRecordEmitsNoTurn(t *testing.T) {
	raw := []byte(`{"type":"user","uuid":"u-1","timestamp":"2026-07-08T17:58:21.962Z","message":{"role":"user","content":"How many moons Mercury has?"}}
{"type":"assistant","uuid":"a-1","parentUuid":"u-1","timestamp":"2026-07-08T17:58:25.068Z","message":{"id":"msg_01","role":"assistant","content":[{"type":"thinking","thinking":"","signature":"EpAECokBCA8"}]}}
{"type":"assistant","uuid":"a-2","parentUuid":"a-1","timestamp":"2026-07-08T17:58:26.608Z","message":{"id":"msg_01","role":"assistant","content":[{"type":"text","text":"Mercury has zero moons."}]}}
`)
	events, err := EncodeCanonical(raw)
	require.NoError(t, err)

	var turns []acf.ConversationEvent
	for _, ev := range events {
		if ev.Type == acf.EventTypeTurn {
			turns = append(turns, ev)
		}
	}
	require.Len(t, turns, 2, "one user turn + one assistant turn; the empty-thinking record must vanish")
	require.Equal(t, "user", turns[0].Role)
	require.Equal(t, "assistant", turns[1].Role)
	require.Len(t, turns[1].Content, 1)
	require.Equal(t, "text", turns[1].Content[0].Type)
	require.Equal(t, "Mercury has zero moons.", turns[1].Content[0].Text)
}

// Non-empty extended thinking is real content and must keep round-tripping
// as a typed block (the fixed-point contract documented in canonical.go).
func TestEncodeCanonical_NonEmptyThinkingIsPreserved(t *testing.T) {
	raw := []byte(`{"type":"assistant","uuid":"a-1","timestamp":"2026-07-08T17:58:25.068Z","message":{"id":"msg_01","role":"assistant","content":[{"type":"thinking","thinking":"let me count the moons","signature":"sig"}]}}
`)
	events, err := EncodeCanonical(raw)
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.Equal(t, acf.EventTypeTurn, events[0].Type)
	require.Equal(t, "assistant", events[0].Role)
	require.Len(t, events[0].Content, 1)
	require.Equal(t, "thinking", events[0].Content[0].Type)
	require.Equal(t, "let me count the moons", events[0].Content[0].Text)
}
