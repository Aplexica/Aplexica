package hermes

import (
	"runtime"
	"testing"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
	"github.com/aplexica/aplexica/internal/hermesdb"
	"github.com/stretchr/testify/require"
)

// Hermes stores multimodal content as a NUL-sentinel-prefixed JSON parts list
// ("\x00json:[{...}]", per hermes_state.py _encode_content). The canonical
// encoder must decode it into typed blocks, not emit a garbled text blob.
func TestEncodeBundleAsCanonical_MultimodalContentDecoded(t *testing.T) {
	content := "\x00json:" + `[{"type":"text","text":"hi"},{"type":"image_url","image_url":{"url":"https://x.png"}}]`
	b := hermesdb.SessionBundle{
		Messages: []hermesdb.MessageRow{
			{Role: "user", Content: &content, Timestamp: 100},
		},
	}
	events := EncodeBundleAsCanonical(b)
	require.Len(t, events, 1)
	require.Equal(t, acf.EventTypeTurn, events[0].Type)
	require.Len(t, events[0].Content, 2)
	require.Equal(t, "text", events[0].Content[0].Type)
	require.Equal(t, "hi", events[0].Content[0].Text)
	require.Equal(t, "image", events[0].Content[1].Type)
	require.Equal(t, "https://x.png", events[0].Content[1].Data)
}

// Assistant reasoning fields must survive a canonical encode→decode round trip
// (carried in ConversationEvent.NativeExtras — per-adapter fidelity).
func TestReasoning_RoundTripsViaNativeExtras(t *testing.T) {
	reasoning := "let me think step by step"
	content := "the answer"
	b := hermesdb.SessionBundle{
		Messages: []hermesdb.MessageRow{
			{Role: "assistant", Content: &content, Reasoning: &reasoning, Timestamp: 100},
		},
	}
	events := EncodeBundleAsCanonical(b)
	require.GreaterOrEqual(t, len(events), 1)
	require.Equal(t, acf.EventTypeTurn, events[0].Type)
	require.NotEmpty(t, events[0].NativeExtras, "reasoning must be carried in NativeExtras")

	out := DecodeBundleFromCanonical("sess-x", "", events)
	require.Len(t, out.Messages, 1)
	require.NotNil(t, out.Messages[0].Reasoning, "reasoning must be restored on decode")
	require.Equal(t, reasoning, *out.Messages[0].Reasoning)
}

func TestNativePath_SoulMd_RoutesToHermesRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("hardcodes a unix home path; NativePath uses OS separators on Windows")
	}
	a := &Adapter{HomeDir: "/home/u"}
	art := acf.Artifact{Kind: acf.KindMemory, Name: "SOUL.md", Scope: acf.ScopeGlobal}
	p, ok, err := a.NativePath(art, "")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "/home/u/.hermes/SOUL.md", p, "SOUL.md lives at ~/.hermes/SOUL.md, not under memories/")
}

func TestCapabilities_HookNotAdvertised_SoulCovered(t *testing.T) {
	caps := New().Capabilities()
	for _, k := range caps.Tools {
		require.NotEqual(t, adapter.ToolKindHook, k, "hook must not be advertised (no import path)")
	}
	require.Contains(t, caps.NativeBasenames, "SOUL.md")
	require.Equal(t, acf.KindMemory, caps.BasenameToKind["SOUL.md"])
}
