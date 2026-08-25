package claudecode

import (
	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
)

// Capabilities reports the claude-code adapter's BRD-02 §4.5
// compatibility matrix. Conformance test §5.4 #7 verifies these
// values match the adapter's actual Import/Export behavior.
func (a *Adapter) Capabilities() adapter.Capabilities {
	return adapter.Capabilities{
		Name:     a.Name(),
		Surfaces: []adapter.Surface{adapter.SurfaceCLI, adapter.SurfaceDesktop},
		Artifacts: adapter.ArtifactSupport{
			Memory:       true, // CLAUDE.md / AGENTS.md
			Skill:        true, // SKILL.md
			Tool:         true, // .mcp.json
			Conversation: true, // *.jsonl
		},
		Tools: []adapter.ToolKind{
			adapter.ToolKindMCPServer, // .mcp.json
			// Subagent (.claude/agents/*.md), hook (settings.json), and
			// slash-command (.claude/commands/*.md) have no import path yet,
			// and plugins are M2+ — none are advertised until implemented so
			// Capabilities() matches actual Import/Export behavior (§5.4 #7).
		},
		NativeBasenames: []string{
			"CLAUDE.md", "AGENTS.md", "SKILL.md", ".mcp.json", ".claude.json",
		},
		BasenameToKind: map[string]acf.Kind{
			"CLAUDE.md":    acf.KindMemory,
			"AGENTS.md":    acf.KindMemory,
			"SKILL.md":     acf.KindSkill,
			".mcp.json":    acf.KindTool,
			".claude.json": acf.KindTool, // user-scope MCP servers (`claude mcp add -s user`)
		},
		NotesURL: "docs/adapters/claude-code.md",
	}
}
