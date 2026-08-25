package codex

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter/adaptertest"
	"github.com/stretchr/testify/require"
)

// Global skills must land in ~/.agents/skills/<name>/SKILL.md — the current
// location shared by Codex CLI and Desktop. A bare SKILL.md is never loaded
// and made every global skill collide on one path.
func TestNativePath_Skill_Global_PerNameDir(t *testing.T) {
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
	require.Equal(t, filepath.Join("/home/u", ".agents", "skills", "deploy-helper", "SKILL.md"), p)
}

func TestDiscover_WatchesSkillsRecursively(t *testing.T) {
	codexPath := adaptertest.WithCommand(t, "codex")
	home := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".codex"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".agents", "skills"), 0o755))
	a := &Adapter{HomeDir: home, CLIExecutablePaths: []string{codexPath}}
	d, err := a.Discover()
	require.NoError(t, err)
	require.True(t, d.Installed)
	require.Contains(t, d.RecursiveRoots, filepath.Join(home, ".agents", "skills"))
}

func TestDiscover_KeepsLegacyCodexSkillsAsCompatibilityInput(t *testing.T) {
	codexPath := adaptertest.WithCommand(t, "codex")
	home := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".codex", "skills"), 0o755))
	d, err := (&Adapter{HomeDir: home, CLIExecutablePaths: []string{codexPath}}).Discover()
	require.NoError(t, err)
	require.Contains(t, d.RecursiveRoots, filepath.Join(home, ".codex", "skills"))
}

func TestInferScope_UserAgentSkillsAreGlobal(t *testing.T) {
	home := t.TempDir()
	a := &Adapter{HomeDir: home}
	require.Equal(t, acf.ScopeGlobal, a.inferScope(filepath.Join(home, ".agents", "skills", "deploy", "SKILL.md")))
}
