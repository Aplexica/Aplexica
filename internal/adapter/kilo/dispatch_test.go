package kilo

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/stretchr/testify/require"
)

func TestDispatch_AgentsMd_RoutesToMemory(t *testing.T) {
	tmp := t.TempDir()
	store := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, store.Init())

	src := filepath.Join(tmp, "AGENTS.md")
	require.NoError(t, os.WriteFile(src, []byte("# mem\n"), 0o644))

	a := New()
	ids, err := a.Import(context.Background(), store, src)
	require.NoError(t, err)
	require.Len(t, ids, 1)

	got, err := store.ReadArtifact(acf.KindMemory, ids[0])
	require.NoError(t, err)
	require.Equal(t, acf.KindMemory, got.Kind)
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

func TestDispatch_UnrecognizedFilename_IsError(t *testing.T) {
	tmp := t.TempDir()
	store := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, store.Init())

	src := filepath.Join(tmp, "RANDOM.txt")
	require.NoError(t, os.WriteFile(src, []byte("x"), 0o644))

	a := New()
	_, err := a.Import(context.Background(), store, src)
	require.Error(t, err)
	require.Contains(t, err.Error(), "kilo")
	require.Contains(t, err.Error(), "RANDOM.txt")
}

func TestDispatch_Export_UnknownID(t *testing.T) {
	tmp := t.TempDir()
	store := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, store.Init())

	a := New()
	err := a.Export(context.Background(), store, "01956a39-aaaa-aaaa-aaaa-aaaaaaaaaaaa", filepath.Join(tmp, "out"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "not found")
}

func TestDispatch_AgentMd_FallbackRoutesToMemory(t *testing.T) {
	tmp := t.TempDir()
	store := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, store.Init())

	src := filepath.Join(tmp, "AGENT.md")
	require.NoError(t, os.WriteFile(src, []byte("# mem\n"), 0o644))

	a := New()
	ids, err := a.Import(context.Background(), store, src)
	require.NoError(t, err)
	require.Len(t, ids, 1)

	got, err := store.ReadArtifact(acf.KindMemory, ids[0])
	require.NoError(t, err)
	require.Equal(t, acf.KindMemory, got.Kind)
}

func TestDispatch_RuleMarkdown_RoutesToMemory(t *testing.T) {
	tmp := t.TempDir()
	store := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, store.Init())

	src := filepath.Join(tmp, ".kilo", "rules", "formatting.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(src), 0o755))
	require.NoError(t, os.WriteFile(src, []byte("# formatting\n"), 0o644))

	a := New()
	ids, err := a.Import(context.Background(), store, src)
	require.NoError(t, err)
	require.Len(t, ids, 1)

	got, err := store.ReadArtifact(acf.KindMemory, ids[0])
	require.NoError(t, err)
	require.Equal(t, acf.KindMemory, got.Kind)
	require.Equal(t, "formatting.md", got.Name)
}

func TestDispatch_KiloJsonc_RoutesToTool(t *testing.T) {
	tmp := t.TempDir()
	store := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, store.Init())

	src := filepath.Join(tmp, "kilo.jsonc")
	require.NoError(t, os.WriteFile(src,
		[]byte(`{"mcp":{"x":{"type":"local","command":["c"]}}}`), 0o644))

	a := New()
	ids, err := a.Import(context.Background(), store, src)
	require.NoError(t, err)
	require.Len(t, ids, 1)

	got, err := store.ReadArtifact(acf.KindTool, ids[0])
	require.NoError(t, err)
	require.Equal(t, acf.KindTool, got.Kind)
}

func TestDispatch_JSONLFile_RejectedAsNotApplicable(t *testing.T) {
	// Kilo uses a SQLite session DB; a *.jsonl import attempt must error with a
	// clear "not applicable" message rather than "not yet supported".
	tmp := t.TempDir()
	store := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, store.Init())

	src := filepath.Join(tmp, "session.jsonl")
	require.NoError(t, os.WriteFile(src, []byte(`{"x":1}`+"\n"), 0o644))

	a := New()
	_, err := a.Import(context.Background(), store, src)
	require.Error(t, err)
	require.Contains(t, err.Error(), "not applicable",
		"v0.4.1 must say conversation is 'not applicable in V1' (vs v0.4.0's 'not yet supported')")
	require.Contains(t, err.Error(), "SQLite DB")
}

func TestDispatch_DBFile_RoutesToConversationImport(t *testing.T) {
	tmp := t.TempDir()
	store := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, store.Init())

	dbPath := initKiloConversationTestDB(t)
	insertKiloSession(t, dbPath, kiloTestSession{
		ID:          "s1",
		ProjectID:   "proj-s1",
		Directory:   tmp,
		Title:       "Kilo DB Chat",
		TimeCreated: 1000,
		TimeUpdated: 2000,
	})
	insertKiloMessage(t, dbPath, "m1", "s1", 1100, `{"role":"user","time":{"created":1100}}`)
	insertKiloPart(t, dbPath, "p1", "m1", "s1", 1101, `{"type":"text","text":"hello","time":{"start":1101,"end":1102}}`)

	a := New()
	ids, err := a.Import(context.Background(), store, dbPath)
	require.NoError(t, err)
	require.Len(t, ids, 1)

	got, err := store.ReadArtifact(acf.KindConversation, ids[0])
	require.NoError(t, err)
	require.Equal(t, acf.KindConversation, got.Kind)
	require.Equal(t, "Kilo DB Chat", got.Name)
}

func TestNativePath_Memory_Project(t *testing.T) {
	a := New()
	art := acf.Artifact{Kind: acf.KindMemory, Scope: acf.ScopeProject}
	p, supports, err := a.NativePath(art, "/proj/foo")
	require.NoError(t, err)
	require.True(t, supports)
	require.Equal(t, filepath.Join("/proj/foo", "AGENTS.md"), p)
}

func TestNativePath_Global_ErrorsBecauseKiloIsProjectScoped(t *testing.T) {
	a := &Adapter{HomeDir: "/home/u"}
	art := acf.Artifact{Kind: acf.KindMemory, Scope: acf.ScopeGlobal}
	p, supports, err := a.NativePath(art, "")
	require.NoError(t, err)
	require.True(t, supports)
	require.Equal(t, filepath.Join("/home/u", ".config", "kilo", "AGENTS.md"), p)
}

func TestNativePath_Skill_Project(t *testing.T) {
	a := New()
	art := acf.Artifact{Kind: acf.KindSkill, Scope: acf.ScopeProject}
	p, supports, err := a.NativePath(art, "/proj")
	require.NoError(t, err)
	require.True(t, supports)
	require.Equal(t, filepath.Join("/proj", ".kilo", "skills", "skill", "SKILL.md"), p,
		"Kilo discovers skills under .kilo/skills/<name>/SKILL.md, not at the project root")
}

func TestNativePath_Tool_Project(t *testing.T) {
	a := New()
	art := acf.Artifact{Kind: acf.KindTool, Scope: acf.ScopeProject}
	p, supports, err := a.NativePath(art, "/proj")
	require.NoError(t, err)
	require.True(t, supports)
	require.Equal(t, filepath.Join("/proj", "kilo.jsonc"), p)
}

func TestNativePath_RuleMemory_PreservesRuleLocation(t *testing.T) {
	a := New()
	art := acf.Artifact{
		Kind:       acf.KindMemory,
		Scope:      acf.ScopeProject,
		Name:       "formatting.md",
		SourcePath: filepath.Join("/other", ".kilo", "rules", "formatting.md"),
	}
	p, supports, err := a.NativePath(art, "/proj")
	require.NoError(t, err)
	require.True(t, supports)
	require.Equal(t, filepath.Join("/proj", ".kilo", "rules", "formatting.md"), p)
}

func TestNativePath_Conversation_NotSupportedForFanout(t *testing.T) {
	a := New()
	art := acf.Artifact{Kind: acf.KindConversation, Scope: acf.ScopeProject}
	_, supports, err := a.NativePath(art, "/proj")
	require.NoError(t, err)
	require.False(t, supports,
		"conversation fan-out is intentionally unsupported in v0.7.0 (Format string differs per agent)")
}

func TestNativePath_NonGlobalRequiresContextDir(t *testing.T) {
	a := New()
	art := acf.Artifact{Kind: acf.KindMemory, Scope: acf.ScopeProject}
	_, _, err := a.NativePath(art, "")
	require.Error(t, err)
}

// Regression for E2E F3: a global skill imported from a single-file root
// (~/.claude/SKILL.md) exported to kilo as skills/.claude/SKILL.md — the
// dir-name heuristic took the source's PARENT DIRECTORY (".claude", an
// agent config root) as the skill name. Hidden/dot parents are config
// roots, never skill names.
func TestKiloSkillDirName_RejectsDotDirParents(t *testing.T) {
	got := kiloSkillDirName(acf.Artifact{
		Name:       "SKILL.md",
		SourcePath: "/Users/u/.claude/SKILL.md",
	})
	if got == ".claude" || strings.HasPrefix(got, ".") {
		t.Fatalf("dot-dir parent must not become the skill dir name; got %q", got)
	}
}

func TestKiloSkillDirName_KeepsRealSkillDirParent(t *testing.T) {
	got := kiloSkillDirName(acf.Artifact{
		Name:       "SKILL.md",
		SourcePath: "/Users/u/.claude/skills/deploy-helper/SKILL.md",
	})
	if got != "deploy-helper" {
		t.Fatalf("want deploy-helper, got %q", got)
	}
}
