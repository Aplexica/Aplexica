package openclaw

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
			Memory:       true, // MEMORY.md / AGENTS.md / CLAUDE.md / DREAMS.md / daily notes
			Skill:        true, // SKILL.md
			Tool:         true, // openclaw.json / openclaw.jsonc / openclaw.json5
			Conversation: true, // *.jsonl
		},
		Tools: []adapter.ToolKind{
			adapter.ToolKindMCPServer, // mcp.servers section of openclaw.json
			// Slash-command / subagent / hook / plugin are M2+ for openclaw
			// (no native import path yet — slash commands are skill-based).
		},
		NativeBasenames: []string{
			"MEMORY.md", "AGENTS.md", "CLAUDE.md", "DREAMS.md", "SKILL.md",
			"openclaw.json", "openclaw.jsonc", "openclaw.json5",
		},
		BasenameToKind: map[string]acf.Kind{
			"MEMORY.md":      acf.KindMemory,
			"AGENTS.md":      acf.KindMemory,
			"CLAUDE.md":      acf.KindMemory,
			"DREAMS.md":      acf.KindMemory,
			"SKILL.md":       acf.KindSkill,
			"openclaw.json":  acf.KindTool,
			"openclaw.jsonc": acf.KindTool,
			"openclaw.json5": acf.KindTool,
		},
		NotesURL: "docs/adapters/openclaw.md",
	}
}
