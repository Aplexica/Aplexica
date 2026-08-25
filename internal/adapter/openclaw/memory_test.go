package openclaw

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/stretchr/testify/require"
)

func TestImportMemory_RoundTrip(t *testing.T) {
	tmp := t.TempDir()
	storeRoot := filepath.Join(tmp, "store")
	store := &acf.Store{Root: storeRoot}
	require.NoError(t, store.Init())

	memPath := filepath.Join(tmp, "MEMORY.md")
	content := "# OpenClaw memory\n\n- pref: lobster\n"
	require.NoError(t, os.WriteFile(memPath, []byte(content), 0o644))

	a := &Adapter{HomeDir: tmp, DeviceID: "dev"}
	ids, err := a.ImportMemory(context.Background(), store, memPath)
	require.NoError(t, err)
	require.Len(t, ids, 1)

	outPath := filepath.Join(tmp, "out.md")
	require.NoError(t, a.ExportMemory(context.Background(), store, ids[0], outPath))

	got, err := os.ReadFile(outPath)
	require.NoError(t, err)
	require.Equal(t, content, string(got), "byte-identical round-trip required")
}

func TestImportMemory_AcceptsAgentsCLAUDEDreams(t *testing.T) {
	for _, name := range []string{"AGENTS.md", "CLAUDE.md", "DREAMS.md"} {
		t.Run(name, func(t *testing.T) {
			tmp := t.TempDir()
			storeRoot := filepath.Join(tmp, "store")
			store := &acf.Store{Root: storeRoot}
			require.NoError(t, store.Init())

			p := filepath.Join(tmp, name)
			require.NoError(t, os.WriteFile(p, []byte("body for "+name), 0o644))

			a := &Adapter{HomeDir: tmp, DeviceID: "dev"}
			ids, err := a.ImportMemory(context.Background(), store, p)
			require.NoError(t, err)
			require.Len(t, ids, 1)
		})
	}
}

func TestImportMemory_AcceptsDailyNote(t *testing.T) {
	tmp := t.TempDir()
	storeRoot := filepath.Join(tmp, "store")
	store := &acf.Store{Root: storeRoot}
	require.NoError(t, store.Init())

	// memory/YYYY-MM-DD.md routed via the dispatch's daily-note matcher.
	memDir := filepath.Join(tmp, "memory")
	require.NoError(t, os.MkdirAll(memDir, 0o755))
	noteName := "2026-05-21.md"
	notePath := filepath.Join(memDir, noteName)
	require.NoError(t, os.WriteFile(notePath, []byte("# 2026-05-21\nworked on aplexica\n"), 0o644))

	a := &Adapter{HomeDir: tmp, DeviceID: "dev"}
	ids, err := a.Import(context.Background(), store, notePath)
	require.NoError(t, err)
	require.Len(t, ids, 1)

	art, err := store.ReadArtifact(acf.KindMemory, ids[0])
	require.NoError(t, err)
	require.Equal(t, noteName, art.Name, "daily-note artifact Name should be the filename")
}

func TestImportMemory_AcceptsDailyNoteWithSlug(t *testing.T) {
	tmp := t.TempDir()
	storeRoot := filepath.Join(tmp, "store")
	store := &acf.Store{Root: storeRoot}
	require.NoError(t, store.Init())

	memDir := filepath.Join(tmp, "memory")
	require.NoError(t, os.MkdirAll(memDir, 0o755))
	noteName := "2026-05-21-reset.md"
	notePath := filepath.Join(memDir, noteName)
	require.NoError(t, os.WriteFile(notePath, []byte("# 2026-05-21 (after /reset)\n"), 0o644))

	a := &Adapter{HomeDir: tmp, DeviceID: "dev"}
	ids, err := a.Import(context.Background(), store, notePath)
	require.NoError(t, err)
	require.Len(t, ids, 1)
}

func TestNativePath_MemoryDailyNote_RoutesToMemorySubdir(t *testing.T) {
	a := &Adapter{HomeDir: "/home/u", DeviceID: "dev"}
	for _, name := range []string{"2026-05-21.md", "2026-05-21-slug.md", "2026-12-31-after-reset.md"} {
		dest, supports, err := a.NativePath(acf.Artifact{Kind: acf.KindMemory, Scope: acf.ScopeProject, Name: name}, "/tmp/proj")
		require.NoError(t, err)
		require.True(t, supports)
		require.Equal(t, filepath.Join("/home/u", ".openclaw", "workspace", "MEMORY.md"), dest,
			"project-scoped memory (incl. daily notes) routes to the central file openclaw actually reads")
	}
}
