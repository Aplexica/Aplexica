package syncd

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/stretchr/testify/require"
)

// TestPrimaryImport_PathOwnershipBeatsBasename is the regression guard for the
// "received memory from hermes" bug: Claude Code's auto-memory file
// ~/.claude/projects/<cwd>/memory/MEMORY.md lives INSIDE claude-code's own
// watched root, but its basename collides with hermes' native memory filename
// (MEMORY.md). primaryImport must attribute it to the OWNING adapter
// (claude-code), never to hermes/openclaw which merely share the basename and
// do not own the path.
func TestPrimaryImport_PathOwnershipBeatsBasename(t *testing.T) {
	root := realTempDir(t)
	require.NoError(t, os.MkdirAll(filepath.Join(root, "proj"), 0o755))
	adapters, store, _ := buildAllFiveAdapters(t, root)

	claudeRoot := filepath.Join(root, ".claude")
	require.NoError(t, os.MkdirAll(claudeRoot, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(claudeRoot, "CLAUDE.md"), []byte("# claude global\n"), 0o644))

	memDir := filepath.Join(claudeRoot, "projects", "-tmp-proj", "memory")
	require.NoError(t, os.MkdirAll(memDir, 0o755))
	memPath := filepath.Join(memDir, "MEMORY.md")
	require.NoError(t, os.WriteFile(memPath, []byte("# Memory index\n- [Dogs](dogs.md)\n"), 0o644))

	orch, err := NewOrchestrator(Config{
		Dir:      filepath.Join(root, "proj"),
		Adapters: adapters,
		Store:    store,
		RootsByAdapter: map[string][]string{
			"claude-code": {claudeRoot, filepath.Join(claudeRoot, "projects")},
			"codex":       {filepath.Join(root, ".codex")},
			"hermes":      {filepath.Join(root, ".hermes")},
			"openclaw":    {filepath.Join(root, ".openclaw")},
		},
	})
	require.NoError(t, err)
	defer orch.Close()

	ad, _, ok := orch.primaryImport(context.Background(), memPath)
	if ok {
		require.Equal(t, "claude-code", ad.Name(),
			"a MEMORY.md inside claude-code's own root must be imported by claude-code, never a basename-collision adapter")
	}

	// The hard invariant, independent of whether claude-code yet knows how to
	// import the file: NO memory artifact may be attributed to hermes.
	mems, err := store.ListArtifacts(acf.KindMemory)
	require.NoError(t, err)
	for _, m := range mems {
		evs, err := store.ReadEvents(acf.KindMemory, m.ArtifactID)
		require.NoError(t, err)
		for _, e := range evs {
			require.NotEqual(t, "hermes", e.Provenance.SourceAgent,
				"claude-code's auto-memory (under ~/.claude) must not be mis-attributed to hermes on a basename collision")
		}
	}
}
