package claudecode

import (
	"testing"

	"github.com/aplexica/aplexica/internal/adapter"
	"github.com/stretchr/testify/require"
)

// The claude-code adapter only has an import path for MCP servers (.mcp.json).
// Subagents (.claude/agents/*.md), hooks (settings.json), and slash commands
// (.claude/commands/*.md) are not yet parsed, so Capabilities() must not
// advertise them (BRD-02 §5.4 #7: declared capability must match behavior).
func TestCapabilities_OnlyAdvertisesImplementedTools(t *testing.T) {
	caps := New().Capabilities()
	require.Equal(t, []adapter.ToolKind{adapter.ToolKindMCPServer}, caps.Tools)
	require.Equal(t, []adapter.Surface{adapter.SurfaceCLI, adapter.SurfaceDesktop}, caps.Surfaces)
}
