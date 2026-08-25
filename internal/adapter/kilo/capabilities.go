package kilo

import (
	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
)

// Capabilities — see BRD-02 §4.5.
//
// Kilo's narrower native surface is reflected in Artifacts.Conversation =
// false. The adapter can import Kilo's SQLite session DB into canonical
// conversation artifacts, but Kilo conversation export/fan-out remains
// unsupported until there is a stable native write-back path.
func (a *Adapter) Capabilities() adapter.Capabilities {
	return adapter.Capabilities{
		Name:     a.Name(),
		Surfaces: []adapter.Surface{adapter.SurfaceCLI},
		Artifacts: adapter.ArtifactSupport{
			Memory:       true,  // AGENTS.md / AGENT.md
			Skill:        true,  // SKILL.md
			Tool:         true,  // kilo.jsonc
			Conversation: false, // read-only DB import exists; no export/fan-out yet
		},
		Tools: []adapter.ToolKind{
			adapter.ToolKindMCPServer, // kilo.jsonc mcpServers
			// Subagent / hook / slash-command / plugin: kilo's "modes"
			// + custom-instructions model doesn't map 1:1 to these
			// kinds yet; full mapping is M2+.
		},
		NativeBasenames: []string{
			"AGENTS.md", "AGENT.md", "SKILL.md", "kilo.jsonc", "mcp.json",
		},
		BasenameToKind: map[string]acf.Kind{
			"AGENTS.md":  acf.KindMemory,
			"AGENT.md":   acf.KindMemory,
			"SKILL.md":   acf.KindSkill,
			"kilo.jsonc": acf.KindTool,
			"mcp.json":   acf.KindTool,
		},
		NotesURL: "docs/adapters/kilo.md",
	}
}
