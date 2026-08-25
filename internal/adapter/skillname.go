package adapter

import (
	"path/filepath"
	"strings"

	"github.com/aplexica/aplexica/internal/acf"
)

// SkillDirName derives the per-skill directory name for materializing a
// skill artifact into an agent's skills tree (<skills-root>/<name>/SKILL.md
// — the layout claude-code, codex, kilo, hermes, and openclaw all discover
// skills from; a bare <root>/SKILL.md is found by NONE of them).
//
// The name comes from the artifact's SourcePath parent directory (skills are
// imported as <name>/SKILL.md, so the parent IS the skill name). Dot-dirs
// and the literal "skills" parent are rejected — a source path like
// ~/.claude/skills/SKILL.md must not produce a dir named ".claude" or
// "skills" (the dot-dir case shipped as kilo bug 9e22ecd). Falls back to the
// artifact name, then "skill".
//
// Promoted from kilo's kiloSkillDirName when the per-name layout was
// extended to the other four adapters (global skills previously fanned out
// to <agent-root>/SKILL.md, where no agent looked and every skill collided
// on one path).
func SkillDirName(artifact acf.Artifact) string {
	if artifact.SourcePath != "" {
		parent := filepath.Base(filepath.Dir(artifact.SourcePath))
		if parent != "" && parent != "." && parent != string(filepath.Separator) &&
			parent != "skills" && !strings.HasPrefix(parent, ".") {
			return parent
		}
	}
	if n := strings.TrimSuffix(artifact.Name, ".md"); n != "" && n != "SKILL" {
		return n
	}
	return "skill"
}
