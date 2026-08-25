package hermes

import (
	"path/filepath"
	"testing"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/stretchr/testify/require"
)

// Global skills must land in ~/.hermes/skills/aplexica/<name>/SKILL.md —
// hermes discovers skills from <category>/<name>/ dirs under skills/; synced
// skills use an "aplexica" category mirroring that layout. A bare
// ~/.hermes/SKILL.md is never loaded.
func TestNativePath_Skill_Global_AplexicaCategory(t *testing.T) {
	a := &Adapter{HomeDir: "/home/u"}
	art := acf.Artifact{
		Kind:       acf.KindSkill,
		Scope:      acf.ScopeGlobal,
		Name:       "SKILL.md",
		SourcePath: "/home/u/.claude/skills/deploy-helper/SKILL.md",
	}
	p, supports, err := a.NativePath(art, "")
	require.NoError(t, err)
	require.True(t, supports)
	require.Equal(t, filepath.Join("/home/u", ".hermes", "skills", "aplexica", "deploy-helper", "SKILL.md"), p)
}
