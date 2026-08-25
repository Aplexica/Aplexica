package codex

import (
	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
)

// Capabilities — see BRD-02 §4.5.
func (a *Adapter) Capabilities() adapter.Capabilities {
	return adapter.Capabilities{
		Name:     a.Name(),
		Surfaces: []adapter.Surface{adapter.SurfaceCLI, adapter.SurfaceDesktop},
		Artifacts: adapter.ArtifactSupport{
			Memory:       true, // AGENTS.md
			Skill:        true, // SKILL.md
			Tool:         true, // *.toml
			Conversation: true, // *.jsonl session logs
		},
		Tools: []adapter.ToolKind{
			adapter.ToolKindMCPServer, // mcp_servers TOML
			// Subagents, slash commands, hooks, and plugins have no adapter
			// import/export mapping yet, so they are not advertised.
		},
		NativeBasenames: []string{
			"AGENTS.md", "SKILL.md", "config.toml",
		},
		BasenameToKind: map[string]acf.Kind{
			"AGENTS.md":   acf.KindMemory,
			"SKILL.md":    acf.KindSkill,
			"config.toml": acf.KindTool,
		},
		NotesURL: "docs/adapters/codex.md",
	}
}
