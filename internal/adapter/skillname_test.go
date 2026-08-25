package adapter

import (
	"testing"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/stretchr/testify/require"
)

func TestSkillDirName(t *testing.T) {
	cases := []struct {
		name string
		art  acf.Artifact
		want string
	}{
		{"source parent dir wins", acf.Artifact{SourcePath: "/h/.claude/skills/pdf-tools/SKILL.md", Name: "SKILL.md"}, "pdf-tools"},
		{"literal skills parent rejected", acf.Artifact{SourcePath: "/h/.kilo/skills/SKILL.md", Name: "SKILL.md"}, "skill"},
		{"dot-dir parent rejected (config-root import)", acf.Artifact{SourcePath: "/h/.claude/SKILL.md", Name: "SKILL.md"}, "skill"},
		{"name fallback strips .md", acf.Artifact{Name: "deploy-helper.md"}, "deploy-helper"},
		{"bare SKILL name falls back", acf.Artifact{Name: "SKILL.md"}, "skill"},
		{"empty everything falls back", acf.Artifact{}, "skill"},
	}
	for _, c := range cases {
		require.Equal(t, c.want, SkillDirName(c.art), c.name)
	}
}
