package hermes

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/hermesdb"
	"github.com/stretchr/testify/require"
)

func TestDispatch_MemoryMd_RoutesToMemory(t *testing.T) {
	tmp := t.TempDir()
	store := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, store.Init())

	for _, name := range []string{"MEMORY.md", "USER.md"} {
		src := filepath.Join(tmp, name)
		require.NoError(t, os.WriteFile(src, []byte("# mem\n"), 0o644))

		a := New()
		ids, err := a.Import(context.Background(), store, src)
		require.NoError(t, err)
		require.Len(t, ids, 1)

		got, err := store.ReadArtifact(acf.KindMemory, ids[0])
		require.NoError(t, err)
		require.Equal(t, acf.KindMemory, got.Kind)
		require.Equal(t, name, got.Name)
	}
}

func TestDispatch_SkillMd_RoutesToSkill(t *testing.T) {
	tmp := t.TempDir()
	store := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, store.Init())

	src := filepath.Join(tmp, "SKILL.md")
	require.NoError(t, os.WriteFile(src, []byte("---\nname: x\n---\n# s\n"), 0o644))

	a := New()
	ids, err := a.Import(context.Background(), store, src)
	require.NoError(t, err)
	require.Len(t, ids, 1)

	got, err := store.ReadArtifact(acf.KindSkill, ids[0])
	require.NoError(t, err)
	require.Equal(t, acf.KindSkill, got.Kind)
}

func TestDispatch_StateDB_RoutesToConversationImport(t *testing.T) {
	tmp := t.TempDir()
	store := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, store.Init())

	dbPath := filepath.Join(tmp, "state.db")
	db, err := hermesdb.InitTestDB(dbPath)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO sessions (id, source, started_at, title) VALUES ('disp1','cli',100.0,'dispatch')`)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	a := New()
	ids, err := a.Import(context.Background(), store, dbPath)
	require.NoError(t, err)
	require.Len(t, ids, 1, "*.db filename should route to ImportConversationsFromDB")

	got, err := store.ReadArtifact(acf.KindConversation, ids[0])
	require.NoError(t, err)
	require.Equal(t, acf.KindConversation, got.Kind)
}

func TestDispatch_UnrecognizedFilename_IsError(t *testing.T) {
	tmp := t.TempDir()
	store := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, store.Init())

	src := filepath.Join(tmp, "RANDOM.txt")
	require.NoError(t, os.WriteFile(src, []byte("x"), 0o644))

	a := New()
	_, err := a.Import(context.Background(), store, src)
	require.Error(t, err)
}

// Project-scoped memory routes to the CENTRAL memories file — hermes never
// reads project folders, so contextDir-based dests were dead letters (the
// project context never reached the agent). ExportMemory upserts it there
// as a delimited "## Project:" section.
func TestNativePath_Memory_ProjectScopeRoutesToCentral(t *testing.T) {
	a := New()
	art := acf.Artifact{Kind: acf.KindMemory, Scope: acf.ScopeProject, Name: "MEMORY.md"}
	p, supports, err := a.NativePath(art, "/proj")
	require.NoError(t, err)
	require.True(t, supports)
	require.Equal(t, filepath.Join(a.HomeDir, ".hermes", "memories", "MEMORY.md"), p)
}

func TestNativePath_Memory_USERmd(t *testing.T) {
	a := New()
	art := acf.Artifact{Kind: acf.KindMemory, Scope: acf.ScopeProject, Name: "USER.md"}
	p, supports, err := a.NativePath(art, "/proj")
	require.NoError(t, err)
	require.True(t, supports)
	require.Equal(t, filepath.Join(a.HomeDir, ".hermes", "memories", "MEMORY.md"), p,
		"project scope routes to the central file regardless of name")
}

func TestNativePath_Memory_UnknownNameDefaultsToMEMORY(t *testing.T) {
	// Cross-adapter scenario: artifact came from claudecode (Name="CLAUDE.md").
	// Fan-out to hermes should default to MEMORY.md.
	a := New()
	art := acf.Artifact{Kind: acf.KindMemory, Scope: acf.ScopeProject, Name: "CLAUDE.md"}
	p, supports, err := a.NativePath(art, "/proj")
	require.NoError(t, err)
	require.True(t, supports)
	require.Equal(t, filepath.Join(a.HomeDir, ".hermes", "memories", "MEMORY.md"), p,
		"project scope routes to the central file regardless of name")
}

func TestNativePath_Skill(t *testing.T) {
	a := New()
	art := acf.Artifact{Kind: acf.KindSkill, Scope: acf.ScopeProject}
	p, supports, err := a.NativePath(art, "/proj")
	require.NoError(t, err)
	require.True(t, supports)
	require.Equal(t, filepath.Join("/proj", "SKILL.md"), p)
}

func TestNativePath_Tool(t *testing.T) {
	a := New()
	art := acf.Artifact{Kind: acf.KindTool, Scope: acf.ScopeProject}
	p, supports, err := a.NativePath(art, "/proj")
	require.NoError(t, err)
	require.True(t, supports)
	require.Equal(t, filepath.Join("/proj", "config.yaml"), p)
}

func TestNativePath_Conversation_RoutesToStateDB(t *testing.T) {
	// With HomeDir set, conversation artifacts fan out to ~/.hermes/state.db
	// regardless of scope or contextDir (sessions only live in the global DB).
	a := &Adapter{HomeDir: "/home/u"}
	art := acf.Artifact{Kind: acf.KindConversation, Scope: acf.ScopeGlobal}
	p, supports, err := a.NativePath(art, "")
	require.NoError(t, err)
	require.True(t, supports,
		"hermes conversation fan-out should target ~/.hermes/state.db when HomeDir is set")
	require.Equal(t, filepath.Join("/home/u", ".hermes", "state.db"), p)
}

func TestNativePath_Conversation_NoHomeDir_NotSupported(t *testing.T) {
	// Without HomeDir we cannot point at ~/.hermes/state.db, so report
	// supports=false rather than guess at a path.
	a := &Adapter{}
	art := acf.Artifact{Kind: acf.KindConversation, Scope: acf.ScopeProject}
	_, supports, err := a.NativePath(art, "/proj")
	require.NoError(t, err)
	require.False(t, supports,
		"hermes conversation fan-out requires HomeDir to know where state.db lives")
}

func TestNativePath_GlobalMemory(t *testing.T) {
	a := &Adapter{HomeDir: "/home/u"}
	art := acf.Artifact{Kind: acf.KindMemory, Scope: acf.ScopeGlobal, Name: "MEMORY.md"}
	p, supports, err := a.NativePath(art, "")
	require.NoError(t, err)
	require.True(t, supports)
	require.Equal(t, filepath.Join("/home/u", ".hermes", "memories", "MEMORY.md"), p)
}
