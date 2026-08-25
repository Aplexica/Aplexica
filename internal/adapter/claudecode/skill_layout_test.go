package claudecode

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/stretchr/testify/require"
)

// Global skills must land in ~/.claude/skills/<name>/SKILL.md — the only
// location Claude Code discovers personal skills from. A bare
// ~/.claude/SKILL.md is never loaded, and a single shared path made every
// global skill overwrite every other.
func TestNativePath_Skill_Global_PerNameDir(t *testing.T) {
	a := &Adapter{HomeDir: "/home/u"}
	art := acf.Artifact{
		Kind:       acf.KindSkill,
		Scope:      acf.ScopeGlobal,
		Name:       "SKILL.md",
		SourcePath: "/home/u/.codex/skills/deploy-helper/SKILL.md",
	}
	p, supports, err := a.NativePath(art, "")
	require.NoError(t, err)
	require.True(t, supports)
	require.Equal(t, filepath.Join("/home/u", ".claude", "skills", "deploy-helper", "SKILL.md"), p)
}

// ~/.claude/skills/<name>/SKILL.md is nested below the flat root watch, so
// it must be advertised as a recursive root — otherwise agent-native skill
// creation never imports (skills were silently import-blind).
func TestDiscover_WatchesSkillsRecursively(t *testing.T) {
	home := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".claude", "skills"), 0o755))
	a := &Adapter{HomeDir: home}
	d, err := a.Discover()
	require.NoError(t, err)
	require.True(t, d.Installed)
	require.Contains(t, d.RecursiveRoots, filepath.Join(home, ".claude", "skills"))
}
