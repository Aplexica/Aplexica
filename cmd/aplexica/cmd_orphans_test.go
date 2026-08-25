package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/stretchr/testify/require"
)

func runOrphansCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	t.Cleanup(func() {
		orphansStoreRoot = ""
		orphansJSON = false
	})
	return runRoot(t, append([]string{"orphans"}, args...)...)
}

func TestOrphans_ListAndClean(t *testing.T) {
	tmp := t.TempDir()
	storeRoot := filepath.Join(tmp, "store")
	id := seedMemoryArtifact(t, storeRoot, "claude-code", "# x\n")

	store := &acf.Store{Root: storeRoot}
	require.NoError(t, store.MarkOrphan(acf.KindMemory, id, "codex", ""))

	out, err := runOrphansCmd(t, "list", "--store", storeRoot)
	require.NoError(t, err, "list:\n%s", out)
	require.Contains(t, out, id)
	require.Contains(t, out, "codex")

	out, err = runOrphansCmd(t, "clean", "codex", "--store", storeRoot)
	require.NoError(t, err)
	require.Contains(t, out, "cleared 1 orphan")

	out, err = runOrphansCmd(t, "list", "--store", storeRoot)
	require.NoError(t, err)
	require.Contains(t, out, "no orphans")
}

func TestRules_ApplyRetroactiveTagAssigning(t *testing.T) {
	tmp := t.TempDir()
	storeRoot := filepath.Join(tmp, "store")
	userRules := filepath.Join(tmp, "rules.toml")
	id := seedMemoryArtifact(t, storeRoot, "claude-code", "# x\n")

	require.NoError(t, os.WriteFile(userRules, []byte(`
[[sync.rules]]
name = "auto-tag-work"
match.agentSource = ["claude-code"]
match.type = ["memory"]
assign.tags = ["work"]
`), 0o644))

	// Dry run first — must NOT modify.
	out, err := runRulesCmd(t, "apply", "--retroactive", "--dry-run",
		"--rules-file", userRules, "--store", storeRoot)
	require.NoError(t, err, "dry-run:\n%s", out)
	require.Contains(t, out, "would apply")

	store := &acf.Store{Root: storeRoot}
	a, err := store.ReadArtifact(acf.KindMemory, id)
	require.NoError(t, err)
	require.NotContains(t, a.Tags, "work", "dry-run must not mutate")

	// Real run — should add the tag.
	out, err = runRulesCmd(t, "apply", "--retroactive",
		"--rules-file", userRules, "--store", storeRoot)
	require.NoError(t, err)
	require.Contains(t, out, "applied changes")

	a, err = store.ReadArtifact(acf.KindMemory, id)
	require.NoError(t, err)
	require.Contains(t, a.Tags, "work")
}
