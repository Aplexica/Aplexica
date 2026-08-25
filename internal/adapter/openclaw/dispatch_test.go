package openclaw

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/secrets"
	"github.com/stretchr/testify/require"
)

func TestDispatch_Import_RoutesByFilename(t *testing.T) {
	tmp := t.TempDir()
	storeRoot := filepath.Join(tmp, "store")
	store := &acf.Store{Root: storeRoot}
	require.NoError(t, store.Init())
	a := &Adapter{HomeDir: tmp, DeviceID: "dev"}

	for _, tc := range []struct {
		filename string
	}{
		{"MEMORY.md"}, {"AGENTS.md"}, {"CLAUDE.md"}, {"DREAMS.md"}, {"SKILL.md"},
	} {
		t.Run(tc.filename, func(t *testing.T) {
			p := filepath.Join(tmp, tc.filename)
			require.NoError(t, os.WriteFile(p, []byte("body"), 0o644))
			ids, err := a.Import(context.Background(), store, p)
			require.NoError(t, err)
			require.Len(t, ids, 1)
		})
	}
}

func TestDispatch_Import_RoutesOpenClawJSONToTool(t *testing.T) {
	tmp := t.TempDir()
	storeRoot := filepath.Join(tmp, "store")
	store := &acf.Store{Root: storeRoot}
	require.NoError(t, store.Init())
	ss := &secrets.Store{Root: filepath.Join(tmp, "sec")}
	require.NoError(t, ss.Init())
	a := &Adapter{HomeDir: tmp, DeviceID: "dev", SecretsStore: ss}

	p := filepath.Join(tmp, "openclaw.json")
	require.NoError(t, os.WriteFile(p, []byte(`{"mcp":{"servers":{}}}`), 0o644))
	ids, err := a.Import(context.Background(), store, p)
	require.NoError(t, err)
	require.Len(t, ids, 1)
}

func TestDispatch_Import_RoutesJSONLToConversation(t *testing.T) {
	tmp := t.TempDir()
	storeRoot := filepath.Join(tmp, "store")
	store := &acf.Store{Root: storeRoot}
	require.NoError(t, store.Init())
	a := &Adapter{HomeDir: tmp, DeviceID: "dev"}

	p := filepath.Join(tmp, "session.jsonl")
	require.NoError(t, os.WriteFile(p, []byte(`{"type":"user","content":"x"}`+"\n"), 0o644))
	ids, err := a.Import(context.Background(), store, p)
	require.NoError(t, err)
	require.Len(t, ids, 1)
}

func TestDispatch_Import_UnknownFilename(t *testing.T) {
	tmp := t.TempDir()
	storeRoot := filepath.Join(tmp, "store")
	store := &acf.Store{Root: storeRoot}
	require.NoError(t, store.Init())
	a := &Adapter{HomeDir: tmp, DeviceID: "dev"}

	p := filepath.Join(tmp, "random.txt")
	require.NoError(t, os.WriteFile(p, []byte("x"), 0o644))
	_, err := a.Import(context.Background(), store, p)
	require.Error(t, err)
	require.Contains(t, err.Error(), "unrecognized filename")
}

func TestNativePath_Memory_ProjectScope(t *testing.T) {
	a := &Adapter{HomeDir: "/home/u", DeviceID: "dev"}
	dest, supports, err := a.NativePath(acf.Artifact{Kind: acf.KindMemory, Scope: acf.ScopeProject, Name: "MEMORY.md"}, "/tmp/proj")
	require.NoError(t, err)
	require.True(t, supports)
	require.Equal(t, filepath.Join("/home/u", ".openclaw", "workspace", "MEMORY.md"), dest,
		"project scope routes to the central workspace file — openclaw never reads project folders")
}

func TestNativePath_Memory_GlobalScope(t *testing.T) {
	a := &Adapter{HomeDir: "/home/u", DeviceID: "dev"}
	dest, supports, err := a.NativePath(acf.Artifact{Kind: acf.KindMemory, Scope: acf.ScopeGlobal, Name: "MEMORY.md"}, "")
	require.NoError(t, err)
	require.True(t, supports)
	require.Equal(t, filepath.Join("/home/u", ".openclaw", "workspace", "MEMORY.md"), dest)
}

func TestNativePath_Memory_PreservesAlternateNames(t *testing.T) {
	a := &Adapter{HomeDir: "/home/u", DeviceID: "dev"}
	for _, name := range []string{"AGENTS.md", "CLAUDE.md", "DREAMS.md"} {
		// GLOBAL scope preserves alternate names in the workspace;
		// project scope routes to the central MEMORY.md regardless.
		dest, supports, err := a.NativePath(acf.Artifact{Kind: acf.KindMemory, Scope: acf.ScopeGlobal, Name: name}, "")
		require.NoError(t, err)
		require.True(t, supports)
		require.Equal(t, filepath.Join("/home/u", ".openclaw", "workspace", name), dest)
	}
}

func TestNativePath_Skill(t *testing.T) {
	a := &Adapter{HomeDir: "/home/u", DeviceID: "dev"}
	dest, supports, err := a.NativePath(acf.Artifact{Kind: acf.KindSkill, Scope: acf.ScopeProject, Name: "SKILL.md"}, "/tmp/proj")
	require.NoError(t, err)
	require.True(t, supports)
	require.Equal(t, filepath.Join("/tmp/proj", "SKILL.md"), dest)
}

func TestNativePath_Tool(t *testing.T) {
	a := &Adapter{HomeDir: "/home/u", DeviceID: "dev"}
	dest, supports, err := a.NativePath(acf.Artifact{Kind: acf.KindTool, Scope: acf.ScopeProject, Name: "openclaw.json"}, "/tmp/proj")
	require.NoError(t, err)
	require.True(t, supports)
	require.Equal(t, filepath.Join("/tmp/proj", "openclaw.json"), dest)
}

func TestNativePath_Conversation(t *testing.T) {
	a := &Adapter{HomeDir: "/home/u", DeviceID: "dev"}
	dest, supports, err := a.NativePath(acf.Artifact{Kind: acf.KindConversation, Scope: acf.ScopeProject, Name: "session.jsonl"}, "/tmp/proj")
	require.NoError(t, err)
	require.True(t, supports)
	require.Equal(t, filepath.Join("/tmp/proj", "session.jsonl"), dest)
}

func TestNativePath_Conversation_DefaultsToSessionJSONL(t *testing.T) {
	a := &Adapter{HomeDir: "/home/u", DeviceID: "dev"}
	dest, supports, err := a.NativePath(acf.Artifact{Kind: acf.KindConversation, Scope: acf.ScopeProject, Name: ""}, "/tmp/proj")
	require.NoError(t, err)
	require.True(t, supports)
	require.Equal(t, filepath.Join("/tmp/proj", "session.jsonl"), dest)
}

func TestHandlesFormat_AllFourKinds(t *testing.T) {
	a := &Adapter{HomeDir: "/home/u", DeviceID: "dev"}
	require.True(t, a.HandlesFormat(acf.KindMemory, "markdown"))
	require.True(t, a.HandlesFormat(acf.KindSkill, "skill.md"))
	require.True(t, a.HandlesFormat(acf.KindTool, "acf.mcp.v1"))
	require.True(t, a.HandlesFormat(acf.KindConversation, "openclaw.session.jsonl"))
	require.True(t, a.HandlesFormat(acf.KindConversation, "acf.conversation.v1"))
	require.True(t, a.HandlesFormat(acf.KindConversation, "acf.conversation.delta.v1"))
	require.False(t, a.HandlesFormat(acf.KindMemory, "claude-code.session.jsonl"))
	require.False(t, a.HandlesFormat(acf.KindConversation, "claude-code.session.jsonl"))
}
