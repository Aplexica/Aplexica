package openclaw

import (
	"testing"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
	"github.com/stretchr/testify/require"
)

// Real OpenClaw (pi-coding-agent) transcripts wrap turns as
// {"type":"message","message":{"role","content"}} — not Claude Code's
// top-level {"type":"user"|"assistant"}. The canonical encoder must read the
// wrapped shape instead of silently dropping every turn.
func TestEncodeCanonical_OpenClawMessageWrapper(t *testing.T) {
	jsonl := []byte(`{"type":"session-meta","agentId":"a"}
{"type":"message","message":{"role":"user","content":"Hello world"}}
{"type":"message","message":{"role":"assistant","content":"Hi there"}}
{"type":"custom","customType":"openclaw.cache-ttl","data":{}}
`)
	events, err := EncodeCanonical(jsonl)
	require.NoError(t, err)
	require.Len(t, events, 2, "two message rows → two turns; session-meta/custom dropped")
	require.Equal(t, acf.EventTypeTurn, events[0].Type)
	require.Equal(t, "user", events[0].Role)
	require.Equal(t, "Hello world", events[0].Content[0].Text)
	require.Equal(t, "assistant", events[1].Role)
	require.Equal(t, "Hi there", events[1].Content[0].Text)
}

// slash-command is not implemented by the openclaw adapter (no dispatch path),
// so Capabilities() must not advertise it.
func TestCapabilities_OmitsUnimplementedSlashCommand(t *testing.T) {
	caps := New().Capabilities()
	for _, k := range caps.Tools {
		require.NotEqual(t, adapter.ToolKindSlashCommand, k,
			"slash-command must not be advertised until an import path exists")
	}
}
