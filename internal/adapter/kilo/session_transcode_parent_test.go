package kilo

import (
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/stretchr/testify/require"
)

// TestBuildKiloExport_AssistantFirstThreadHasValidParents: when the visible
// thread begins with an assistant turn (its leading user turn was injected
// context and filtered out), the assistant must still chain to a valid parent
// rather than parentID="" — which risks Kilo dropping the leading turn. All
// turns must be preserved (lossless fan-out).
func TestBuildKiloExport_AssistantFirstThreadHasValidParents(t *testing.T) {
	art := acf.Artifact{
		ArtifactID: "00000000-0000-0000-0000-000000000abc",
		UpdatedAt:  time.Unix(1700000000, 0).UTC(),
	}
	turns := []acf.TextTurn{
		{Role: "assistant", Text: "leading assistant reply"},
		{Role: "user", Text: "a question"},
		{Role: "assistant", Text: "second reply"},
	}

	f := buildKiloExport(art, turns, "claude-code", "/home/u")

	texts := map[string]bool{}
	for _, m := range f.Messages {
		if role, _ := m.Info["role"].(string); role == "assistant" {
			pid, _ := m.Info["parentID"].(string)
			require.NotEmpty(t, pid,
				"assistant messages must chain to a valid parent, not parentID=\"\"")
		}
		for _, p := range m.Parts {
			texts[p.Text] = true
		}
	}
	require.True(t, texts["leading assistant reply"], "the leading assistant turn must be preserved")
	require.True(t, texts["a question"])
	require.True(t, texts["second reply"])
}
