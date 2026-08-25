package kilo

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/stretchr/testify/require"
)

// Kilo skills live at .kilo/skills/<name>/SKILL.md, NOT at the project root.
// NativePath must produce the nested path or exported skills are invisible to
// Kilo. The <name> is recovered from the source skill directory.
func TestNativePath_SkillNestsUnderKiloSkills(t *testing.T) {
	a := New()
	art := acf.Artifact{
		Kind:       acf.KindSkill,
		Name:       "SKILL.md",
		Scope:      acf.ScopeProject,
		SourcePath: "/some/proj/.kilo/skills/myskill/SKILL.md",
	}
	p, ok, err := a.NativePath(art, "/proj")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, filepath.Join("/proj", ".kilo", "skills", "myskill", "SKILL.md"), p)
}

// Legacy projects keep their MCP config at .kilocode/mcp.json (flat
// {"mcpServers":{...}} shape). Importing that file must succeed, not error
// with "unrecognized filename".
func TestImport_LegacyKilocodeMcpJSON(t *testing.T) {
	a := New()
	a.HomeDir = t.TempDir()

	dir := t.TempDir()
	path := filepath.Join(dir, "mcp.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"mcpServers":{"gh":{"command":"node","args":["x.js"]}}}`), 0o644))

	store := &acf.Store{Root: filepath.Join(t.TempDir(), "store")}
	require.NoError(t, store.Init())

	ids, err := a.Import(context.Background(), store, path)
	require.NoError(t, err, "legacy .kilocode/mcp.json must be importable")
	require.Len(t, ids, 1)
}
