package openclaw

import (
	"path/filepath"
	"testing"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/stretchr/testify/require"
)

// Global skills must land in ~/.openclaw/workspace/skills/<name>/SKILL.md —
// verified live: OpenClaw lists such a skill as "✓ ready" with source
// "openclaw-workspace". A bare workspace SKILL.md is never loaded as a skill
// and would pollute the workspace root OpenClaw ingests as context (E2E
// SM-1 finding).
// Global MCP must land in the ROOT config ~/.openclaw/openclaw.json — the
// file `openclaw mcp list` reads; workspace/openclaw.json is never consulted
// (regression coverage).
func TestNativePath_Tool_Global_RootConfig(t *testing.T) {
	a := &Adapter{HomeDir: "/home/u"}
	p, supports, err := a.NativePath(acf.Artifact{Kind: acf.KindTool, Scope: acf.ScopeGlobal}, "")
	require.NoError(t, err)
	require.True(t, supports)
	require.Equal(t, filepath.Join("/home/u", ".openclaw", "openclaw.json"), p)
}

func TestNativePath_Skill_Global_WorkspaceSkillsDir(t *testing.T) {
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
	require.Equal(t, filepath.Join("/home/u", ".openclaw", "workspace", "skills", "deploy-helper", "SKILL.md"), p)
}
