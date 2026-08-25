package hermes

import (
	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
)

// Capabilities — see BRD-02 §4.5.
func (a *Adapter) Capabilities() adapter.Capabilities {
	return adapter.Capabilities{
		Name:     a.Name(),
		Surfaces: []adapter.Surface{adapter.SurfaceCLI},
		Artifacts: adapter.ArtifactSupport{
			Memory:       true, // MEMORY.md / USER.md / AGENTS.md
			Skill:        true, // SKILL.md
			Tool:         true, // config.yaml / hermes.yaml
			Conversation: true, // *.db (SQLite session store)
		},
		Tools: []adapter.ToolKind{
			adapter.ToolKindMCPServer, // mcp_servers section of config.yaml
			// Hook / subagent / slash-command / plugin are M2+ for hermes: the
			// tool importer reads only the mcp_servers section of config.yaml
			// (the hooks: block is not yet round-tripped), so we do not
			// advertise ToolKindHook until an import path exists.
		},
		NativeBasenames: []string{
			"MEMORY.md", "USER.md", "AGENTS.md", "SOUL.md", "SKILL.md",
			"config.yaml", "hermes.yaml", "hermes.yml",
		},
		BasenameToKind: map[string]acf.Kind{
			"MEMORY.md":   acf.KindMemory,
			"USER.md":     acf.KindMemory,
			"AGENTS.md":   acf.KindMemory,
			"SOUL.md":     acf.KindMemory, // ~/.hermes/SOUL.md — agent identity memory
			"SKILL.md":    acf.KindSkill,
			"config.yaml": acf.KindTool,
			"hermes.yaml": acf.KindTool,
			"hermes.yml":  acf.KindTool,
		},
		NotesURL: "docs/adapters/hermes.md",
	}
}
