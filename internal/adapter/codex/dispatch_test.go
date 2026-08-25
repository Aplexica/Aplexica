package codex

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/secrets"
	"github.com/stretchr/testify/require"
)

func TestDispatchImport_DetectsAGENTSmd(t *testing.T) {
	tmp := t.TempDir()
	s := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, s.Init())

	src := filepath.Join(tmp, "myproj", "AGENTS.md")
	writeFile(t, src, "# X\n")

	a := New()
	ids, err := a.Import(context.Background(), s, src)
	require.NoError(t, err)
	got, _ := s.ReadArtifact(acf.KindMemory, ids[0])
	require.Equal(t, acf.KindMemory, got.Kind)
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

func TestDispatchExport_ResolvesMemory(t *testing.T) {
	tmp := t.TempDir()
	s := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, s.Init())

	src := filepath.Join(tmp, "AGENTS.md")
	writeFile(t, src, "# X\n")

	a := New()
	ids, err := a.Import(context.Background(), s, src)
	require.NoError(t, err)

	out := filepath.Join(tmp, "out", "AGENTS.md")
	require.NoError(t, a.Export(context.Background(), s, ids[0], out))
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

func TestDispatchImport_DetectsSkillByFilename(t *testing.T) {
	tmp := t.TempDir()
	s := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, s.Init())

	src := filepath.Join(tmp, "skills", "x", "SKILL.md")
	writeFile(t, src, "---\nname: x\n---\n")

	a := New()
	ids, err := a.Import(context.Background(), s, src)
	require.NoError(t, err)
	got, _ := s.ReadArtifact(acf.KindSkill, ids[0])
	require.Equal(t, acf.KindSkill, got.Kind)
}

func TestDispatchImport_DetectsConversationByExtension(t *testing.T) {
	tmp := t.TempDir()
	s := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, s.Init())

	src := filepath.Join(tmp, "sessions", "rollout-test.jsonl")
	writeFile(t, src, `{"type":"summary"}`+"\n")

	a := New()
	ids, err := a.Import(context.Background(), s, src)
	require.NoError(t, err)
	got, _ := s.ReadArtifact(acf.KindConversation, ids[0])
	require.Equal(t, acf.KindConversation, got.Kind)
}

func TestDispatchImport_SkipsSubagentConversationByThreadSource(t *testing.T) {
	tmp := t.TempDir()
	s := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, s.Init())

	src := filepath.Join(tmp, "sessions", "rollout-subagent.jsonl")
	writeFile(t, src, `{"type":"session_meta","payload":{"thread_source":"subagent","source":{"subagent":{"thread_spawn":{"parent_thread_id":"parent"}}}}}`+"\n")

	ids, err := New().Import(context.Background(), s, src)
	require.NoError(t, err)
	require.Empty(t, ids)
	artifacts, err := s.ListArtifacts(acf.KindConversation)
	require.NoError(t, err)
	require.Empty(t, artifacts)
}

func TestDispatchImport_SkipsSubagentConversationBySourceObject(t *testing.T) {
	tmp := t.TempDir()
	s := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, s.Init())

	src := filepath.Join(tmp, "sessions", "rollout-subagent.jsonl")
	writeFile(t, src, `{"type":"session_meta","payload":{"source":{"subagent":{"agent_path":"reviewer"}}}}`+"\n")

	ids, err := New().Import(context.Background(), s, src)
	require.NoError(t, err)
	require.Empty(t, ids)
}

func TestDispatchExport_ResolvesSkill(t *testing.T) {
	tmp := t.TempDir()
	s := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, s.Init())

	src := filepath.Join(tmp, "SKILL.md")
	writeFile(t, src, "x")
	a := New()
	ids, _ := a.Import(context.Background(), s, src)

	out := filepath.Join(tmp, "out", "SKILL.md")
	require.NoError(t, a.Export(context.Background(), s, ids[0], out))
}

func TestDispatchExport_ResolvesConversation(t *testing.T) {
	tmp := t.TempDir()
	s := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, s.Init())

	src := filepath.Join(tmp, "session.jsonl")
	writeFile(t, src, `{"x":1}`+"\n")
	a := New()
	ids, _ := a.Import(context.Background(), s, src)

	out := filepath.Join(tmp, "out", "session.jsonl")
	require.NoError(t, a.Export(context.Background(), s, ids[0], out))
}

func TestDispatchImport_DetectsToolByTOMLExtension(t *testing.T) {
	tmp := t.TempDir()
	s := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, s.Init())
	ss := &secrets.Store{Root: filepath.Join(tmp, "secrets")}
	require.NoError(t, ss.Init())

	src := filepath.Join(tmp, "config.toml")
	writeFile(t, src, `[mcp_servers.x]
type = "http"
url = "https://x"
`)

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

	src := filepath.Join(tmp, "config.toml")
	writeFile(t, src, `[mcp_servers.x]
type = "http"
url = "https://x"
`)

	a := &Adapter{HomeDir: tmp, DeviceID: "dev", SecretsStore: ss}
	ids, err := a.Import(context.Background(), s, src)
	require.NoError(t, err)

	out := filepath.Join(tmp, "out", "config.toml")
	require.NoError(t, a.Export(context.Background(), s, ids[0], out))
}

func TestNativePath_Memory_Project(t *testing.T) {
	a := New()
	art := acf.Artifact{Kind: acf.KindMemory, Scope: acf.ScopeProject}
	p, supports, err := a.NativePath(art, "/proj/foo")
	require.NoError(t, err)
	require.True(t, supports)
	require.Equal(t, filepath.Join("/proj/foo", "AGENTS.md"), p)
}

func TestNativePath_Memory_Global(t *testing.T) {
	a := &Adapter{HomeDir: "/home/u"}
	art := acf.Artifact{Kind: acf.KindMemory, Scope: acf.ScopeGlobal}
	p, supports, err := a.NativePath(art, "")
	require.NoError(t, err)
	require.True(t, supports)
	require.Equal(t, filepath.Join("/home/u", ".codex", "AGENTS.md"), p)
}

func TestNativePath_Skill_Project(t *testing.T) {
	a := New()
	art := acf.Artifact{Kind: acf.KindSkill, Scope: acf.ScopeProject, Name: "deploy.md"}
	p, supports, err := a.NativePath(art, "/proj")
	require.NoError(t, err)
	require.True(t, supports)
	require.Equal(t, filepath.Join("/proj", ".agents", "skills", "deploy", "SKILL.md"), p)
}

func TestNativePath_Tool_Project(t *testing.T) {
	a := New()
	art := acf.Artifact{Kind: acf.KindTool, Scope: acf.ScopeProject}
	p, supports, err := a.NativePath(art, "/proj")
	require.NoError(t, err)
	require.True(t, supports)
	require.Equal(t, filepath.Join("/proj", ".codex", "config.toml"), p)
}

func TestNativePath_Tool_Global(t *testing.T) {
	a := &Adapter{HomeDir: "/home/u"}
	art := acf.Artifact{Kind: acf.KindTool, Scope: acf.ScopeGlobal}
	p, supports, err := a.NativePath(art, "")
	require.NoError(t, err)
	require.True(t, supports)
	require.Equal(t, filepath.Join("/home/u", ".codex", "config.toml"), p)
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
