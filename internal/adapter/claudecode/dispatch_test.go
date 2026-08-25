package claudecode

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/secrets"
	"github.com/stretchr/testify/require"
)

func TestDispatchImport_DetectsMemoryByFilename(t *testing.T) {
	tmp := t.TempDir()
	s := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, s.Init())

	src := filepath.Join(tmp, "CLAUDE.md")
	writeFile(t, src, "# Memory\n")

	a := New()
	ids, err := a.Import(context.Background(), s, src)
	require.NoError(t, err)
	got, _ := s.ReadArtifact(acf.KindMemory, ids[0])
	require.Equal(t, acf.KindMemory, got.Kind)
}

func TestDispatchImport_DetectsSkillByFilename(t *testing.T) {
	tmp := t.TempDir()
	s := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, s.Init())

	src := filepath.Join(tmp, "my-skill", "SKILL.md")
	writeFile(t, src, "---\nname: x\n---\n# Skill\n")

	a := New()
	ids, err := a.Import(context.Background(), s, src)
	require.NoError(t, err)
	got, _ := s.ReadArtifact(acf.KindSkill, ids[0])
	require.Equal(t, acf.KindSkill, got.Kind)
}

func TestDispatchImport_RejectsUnknownFilename(t *testing.T) {
	tmp := t.TempDir()
	s := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, s.Init())

	src := filepath.Join(tmp, "weird.md")
	writeFile(t, src, "x")

	a := New()
	_, err := a.Import(context.Background(), s, src)
	require.Error(t, err)
	require.Contains(t, err.Error(), "unrecognized")
}

func TestDispatchExport_ResolvesMemoryAndSkillByKind(t *testing.T) {
	tmp := t.TempDir()
	s := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, s.Init())

	a := New()

	// Memory artifact
	mSrc := filepath.Join(tmp, "CLAUDE.md")
	writeFile(t, mSrc, "# M\n")
	mIDs, err := a.Import(context.Background(), s, mSrc)
	require.NoError(t, err)

	// Skill artifact
	sSrc := filepath.Join(tmp, "x", "SKILL.md")
	writeFile(t, sSrc, "---\nname: x\n---\n")
	sIDs, err := a.Import(context.Background(), s, sSrc)
	require.NoError(t, err)

	// Export memory — should write to CLAUDE.md output
	mOut := filepath.Join(tmp, "out", "CLAUDE.md")
	require.NoError(t, a.Export(context.Background(), s, mIDs[0], mOut))

	// Export skill — should write to SKILL.md output
	sOut := filepath.Join(tmp, "out", "SKILL.md")
	require.NoError(t, a.Export(context.Background(), s, sIDs[0], sOut))
}

func TestDispatchExport_RejectsUnknownID(t *testing.T) {
	tmp := t.TempDir()
	s := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, s.Init())

	a := New()
	err := a.Export(context.Background(), s, "01956a39-aaaa-aaaa-aaaa-aaaaaaaaaaaa", filepath.Join(tmp, "out.md"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "not found")
}

func TestDispatchExport_PropagatesNonNotFoundError(t *testing.T) {
	tmp := t.TempDir()
	s := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, s.Init())

	src := filepath.Join(tmp, "CLAUDE.md")
	writeFile(t, src, "# M\n")
	ids, err := New().Import(context.Background(), s, src)
	require.NoError(t, err)

	// Corrupt the artifact JSON to make ReadArtifact fail with a non-os.ErrNotExist error.
	artifactPath := filepath.Join(tmp, "store", "acf", "memories", ids[0]+".json")
	require.NoError(t, os.WriteFile(artifactPath, []byte("not valid json"), 0o644))

	err = New().Export(context.Background(), s, ids[0], filepath.Join(tmp, "out.md"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "read memory artifact",
		"non-ErrNotExist errors from ReadArtifact must propagate, not be silently swallowed")
}

func TestDispatchImport_DetectsConversationByExtension(t *testing.T) {
	tmp := t.TempDir()
	s := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, s.Init())

	src := filepath.Join(tmp, "projects", "x", "session-uuid.jsonl")
	writeFile(t, src, `{"type":"summary"}`+"\n")

	a := New()
	ids, err := a.Import(context.Background(), s, src)
	require.NoError(t, err)
	got, _ := s.ReadArtifact(acf.KindConversation, ids[0])
	require.Equal(t, acf.KindConversation, got.Kind)
}

func TestDispatchImport_SkipsConversationUnderSubagentsDirectory(t *testing.T) {
	tmp := t.TempDir()
	s := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, s.Init())

	src := filepath.Join(tmp, "projects", "x", "session", "subagents", "agent-1.jsonl")
	writeFile(t, src, `{"type":"user","message":{"role":"user","content":"review this"}}`+"\n")

	ids, err := New().Import(context.Background(), s, src)
	require.NoError(t, err)
	require.Empty(t, ids)
	artifacts, err := s.ListArtifacts(acf.KindConversation)
	require.NoError(t, err)
	require.Empty(t, artifacts)
}

func TestDispatchImport_SkipsSidechainConversationByMetadata(t *testing.T) {
	tmp := t.TempDir()
	s := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, s.Init())

	src := filepath.Join(tmp, "projects", "x", "agent-1.jsonl")
	writeFile(t, src, `{"isSidechain":true,"agentId":"agent-1","type":"user","message":{"role":"user","content":"review this"}}`+"\n")

	ids, err := New().Import(context.Background(), s, src)
	require.NoError(t, err)
	require.Empty(t, ids)
}

func TestDispatchImport_DesktopCatalogChangeReimportsMatchingTranscript(t *testing.T) {
	home := t.TempDir()
	store := &acf.Store{Root: filepath.Join(home, "store")}
	require.NoError(t, store.Init())

	cliSessionID := "a5b71172-2a33-4ff3-abb2-22c32758d73d"
	cwd := filepath.Join(home, "project", ".claude", "worktrees", "desktop-one")
	transcript := filepath.Join(home, ".claude", "projects", encodeProjectDir(cwd), cliSessionID+".jsonl")
	writeFile(t, transcript, `{"type":"user","sessionId":"`+cliSessionID+`","cwd":`+quotedClaudeJSON(cwd)+`,"message":{"role":"user","content":"hello"}}`+"\n")

	catalog := filepath.Join(home, "desktop-catalog")
	recordPath := filepath.Join(catalog, "project", "local_record.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(recordPath), 0o755))
	require.NoError(t, os.WriteFile(recordPath, []byte(`{"sessionId":"local_record","cliSessionId":"`+cliSessionID+`","cwd":`+quotedClaudeJSON(cwd)+`,"title":"Testing greeting"}`), 0o600))

	a := New()
	a.HomeDir = home
	a.DesktopSessionRoots = []string{catalog}
	ids, err := a.Import(context.Background(), store, recordPath)
	require.NoError(t, err)
	require.Len(t, ids, 1)
	art, err := store.ReadArtifact(acf.KindConversation, ids[0])
	require.NoError(t, err)
	require.Equal(t, "Testing greeting", art.Name)
	require.Equal(t, transcript, art.SourcePath, "the catalog is a trigger, never the artifact source")
}

func TestDispatchImport_DesktopCatalogChangeDoesNotDuplicateGeneratedThread(t *testing.T) {
	home := t.TempDir()
	store := &acf.Store{Root: filepath.Join(home, "store")}
	require.NoError(t, store.Init())

	artifactID := acf.NewID()
	created := time.Date(2026, 7, 15, 16, 0, 0, 0, time.UTC)
	require.NoError(t, store.WriteArtifact(acf.Artifact{
		ArtifactID: artifactID,
		Kind:       acf.KindConversation,
		Scope:      acf.ScopeGlobal,
		Name:       "Original Codex subject",
		CreatedAt:  created,
		UpdatedAt:  created,
	}))

	cwd := home
	transcript := filepath.Join(home, ".claude", "projects", encodeProjectDir(cwd), artifactID+".jsonl")
	writeFile(t, transcript, `{"type":"user","sessionId":"`+artifactID+`","cwd":`+quotedClaudeJSON(cwd)+`,"aplexicaThreadId":"`+artifactID+`","aplexicaBranchId":"main","aplexicaTurnsHash":"snapshot","message":{"role":"user","content":"hello"}}`+"\n")

	catalog := filepath.Join(home, "desktop-catalog")
	recordPath := filepath.Join(catalog, "account", "workspace", "local_"+artifactID+".json")
	require.NoError(t, os.MkdirAll(filepath.Dir(recordPath), 0o755))
	require.NoError(t, os.WriteFile(recordPath, []byte(`{"sessionId":"local_`+artifactID+`","cliSessionId":"`+artifactID+`","cwd":`+quotedClaudeJSON(cwd)+`,"title":"Original Codex subject"}`), 0o600))

	a := New()
	a.HomeDir = home
	a.DesktopSessionRoots = []string{catalog}
	ids, err := a.Import(context.Background(), store, recordPath)
	require.NoError(t, err)
	require.Empty(t, ids, "an unchanged generated snapshot must stop at the thread merge guard")
	artifacts, err := store.ListArtifacts(acf.KindConversation)
	require.NoError(t, err)
	require.Len(t, artifacts, 1, "a Desktop metadata save must not mint a path-keyed echo conversation")
	require.Equal(t, artifactID, artifacts[0].ArtifactID)
}

func TestDispatchExport_ResolvesConversation(t *testing.T) {
	tmp := t.TempDir()
	s := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, s.Init())

	src := filepath.Join(tmp, "session.jsonl")
	writeFile(t, src, `{"type":"summary"}`+"\n")

	a := New()
	ids, err := a.Import(context.Background(), s, src)
	require.NoError(t, err)

	out := filepath.Join(tmp, "out", "session.jsonl")
	require.NoError(t, a.Export(context.Background(), s, ids[0], out))

	got, err := os.ReadFile(out)
	require.NoError(t, err)
	require.Equal(t, `{"type":"summary"}`+"\n", string(got))
}

func TestDispatchImport_DetectsToolByFilename(t *testing.T) {
	tmp := t.TempDir()
	s := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, s.Init())
	ss := &secrets.Store{Root: filepath.Join(tmp, "secrets")}
	require.NoError(t, ss.Init())

	src := filepath.Join(tmp, ".mcp.json")
	writeFile(t, src, `{"mcpServers":{"x":{"type":"http","url":"https://x"}}}`)

	a := &Adapter{HomeDir: tmp, DeviceID: "dev", SecretsStore: ss}
	ids, err := a.Import(context.Background(), s, src)
	require.NoError(t, err)
	got, _ := s.ReadArtifact(acf.KindTool, ids[0])
	require.Equal(t, acf.KindTool, got.Kind)
}

func TestDispatchExport_ResolvesTool(t *testing.T) {
	tmp := t.TempDir()
	s := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, s.Init())
	ss := &secrets.Store{Root: filepath.Join(tmp, "secrets")}
	require.NoError(t, ss.Init())

	src := filepath.Join(tmp, ".mcp.json")
	writeFile(t, src, `{"mcpServers":{"x":{"type":"http","url":"https://x"}}}`)

	a := &Adapter{HomeDir: tmp, DeviceID: "dev", SecretsStore: ss}
	ids, err := a.Import(context.Background(), s, src)
	require.NoError(t, err)

	out := filepath.Join(tmp, "out", ".mcp.json")
	require.NoError(t, a.Export(context.Background(), s, ids[0], out))

	_, err = os.Stat(out)
	require.NoError(t, err)
}

func TestNativePath_Memory_Project(t *testing.T) {
	a := New()
	art := acf.Artifact{Kind: acf.KindMemory, Scope: acf.ScopeProject}
	p, supports, err := a.NativePath(art, "/proj/foo")
	require.NoError(t, err)
	require.True(t, supports)
	require.Equal(t, filepath.Join("/proj/foo", "CLAUDE.md"), p)
}

func TestNativePath_Memory_Global(t *testing.T) {
	a := &Adapter{HomeDir: "/home/u"}
	art := acf.Artifact{Kind: acf.KindMemory, Scope: acf.ScopeGlobal}
	p, supports, err := a.NativePath(art, "")
	require.NoError(t, err)
	require.True(t, supports)
	require.Equal(t, filepath.Join("/home/u", ".claude", "CLAUDE.md"), p)
}

func TestNativePath_Skill_Project(t *testing.T) {
	a := New()
	art := acf.Artifact{Kind: acf.KindSkill, Scope: acf.ScopeProject, Name: "review.md"}
	p, supports, err := a.NativePath(art, "/proj")
	require.NoError(t, err)
	require.True(t, supports)
	require.Equal(t, filepath.Join("/proj", ".claude", "skills", "review", "SKILL.md"), p)
}

func TestNativePath_Tool_Project(t *testing.T) {
	a := New()
	art := acf.Artifact{Kind: acf.KindTool, Scope: acf.ScopeProject}
	p, supports, err := a.NativePath(art, "/proj")
	require.NoError(t, err)
	require.True(t, supports)
	require.Equal(t, filepath.Join("/proj", ".mcp.json"), p)
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
