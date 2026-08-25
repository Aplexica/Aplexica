package hermes

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
	"github.com/aplexica/aplexica/internal/adapter/claudecode"
	"github.com/aplexica/aplexica/internal/adapter/codex"
	"github.com/aplexica/aplexica/internal/hermesdb"
	"github.com/aplexica/aplexica/internal/secrets"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestCrossAdapter_Memory_HermesImport_CodexExport(t *testing.T) {
	tmp := t.TempDir()
	store := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, store.Init())

	src := filepath.Join(tmp, "MEMORY.md")
	content := "# Shared world knowledge\n"
	require.NoError(t, os.WriteFile(src, []byte(content), 0o644))

	h := New()
	ids, err := h.ImportMemory(context.Background(), store, src)
	require.NoError(t, err)

	cx := codex.New()
	out := filepath.Join(tmp, "out", "AGENTS.md")
	require.NoError(t, cx.ExportMemory(context.Background(), store, ids[0], out))

	got, err := os.ReadFile(out)
	require.NoError(t, err)
	require.Equal(t, content, string(got),
		"hermes→codex memory round-trip must be byte-identical")
}

func TestCrossAdapter_Skill_HermesImport_ClaudeCodeExport(t *testing.T) {
	tmp := t.TempDir()
	store := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, store.Init())

	src := filepath.Join(tmp, "SKILL.md")
	content := "---\nname: shared\n---\n# Body\n"
	require.NoError(t, os.WriteFile(src, []byte(content), 0o644))

	h := New()
	ids, err := h.ImportSkill(context.Background(), store, src)
	require.NoError(t, err)

	cc := claudecode.New()
	out := filepath.Join(tmp, "out", "SKILL.md")
	require.NoError(t, cc.ExportSkill(context.Background(), store, ids[0], out))

	// Cross-AGENT materialization carries the provenance marker (the echo
	// guard that keeps watched fan-out copies from minting duplicate
	// artifacts); the content above it is byte-identical, and stripping the
	// marker recovers the original exactly (loop-inert).
	got, err := os.ReadFile(out)
	require.NoError(t, err)
	id, stripped, found := adapter.ParseSkillMarker(got)
	require.True(t, found, "cross-agent skill copy must carry the provenance marker")
	require.Equal(t, ids[0], id)
	require.Equal(t, content, string(stripped))
}

func TestCrossAdapter_Tool_HermesImport_ClaudeCodeExport(t *testing.T) {
	tmp := t.TempDir()
	store := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, store.Init())
	ss := &secrets.Store{Root: filepath.Join(tmp, "secrets")}
	require.NoError(t, ss.Init())

	src := filepath.Join(tmp, "config.yaml")
	require.NoError(t, os.WriteFile(src, []byte(`mcp_servers:
  gh:
    command: uvx
    args: [mcp-server-github]
    env:
      TOKEN: shh
`), 0o644))

	h := &Adapter{HomeDir: tmp, DeviceID: "dev", SecretsStore: ss}
	ids, err := h.ImportTool(context.Background(), store, src)
	require.NoError(t, err)

	cc := claudecode.New()
	cc.SecretsStore = ss
	out := filepath.Join(tmp, "out", ".mcp.json")
	require.NoError(t, cc.ExportTool(context.Background(), store, ids[0], out))

	gotBytes, err := os.ReadFile(out)
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal(gotBytes, &got))
	servers := got["mcpServers"].(map[string]any)
	gh := servers["gh"].(map[string]any)
	require.Equal(t, "uvx", gh["command"])
	env := gh["env"].(map[string]any)
	require.Equal(t, "shh", env["TOKEN"])
}

func TestCrossAdapter_Tool_ClaudeCodeImport_HermesExport(t *testing.T) {
	tmp := t.TempDir()
	store := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, store.Init())
	ss := &secrets.Store{Root: filepath.Join(tmp, "secrets")}
	require.NoError(t, ss.Init())

	src := filepath.Join(tmp, ".mcp.json")
	require.NoError(t, os.WriteFile(src,
		[]byte(`{"mcpServers":{"gh":{"command":"uvx","args":["x"],"env":{"TOKEN":"shh"}}}}`),
		0o644))

	cc := claudecode.New()
	cc.HomeDir = tmp
	cc.SecretsStore = ss
	ids, err := cc.ImportTool(context.Background(), store, src)
	require.NoError(t, err)

	h := &Adapter{HomeDir: tmp, DeviceID: "dev", SecretsStore: ss}
	out := filepath.Join(tmp, "out", "config.yaml")
	require.NoError(t, h.ExportTool(context.Background(), store, ids[0], out))

	gotBytes, err := os.ReadFile(out)
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, yaml.Unmarshal(gotBytes, &got))
	mcp := got["mcp_servers"].(map[string]any)
	gh := mcp["gh"].(map[string]any)
	env := gh["env"].(map[string]any)
	require.Equal(t, "shh", env["TOKEN"])
}

// TestHermesAdapter_AllFourKinds is the v0.11.0 coverage assertion: the
// hermes adapter now supports all 4 ACF kinds (memory, skill, tool,
// conversation). Runs each kind's Import end-to-end against a single store
// and confirms the resulting artifact lands with the expected kind.
func TestHermesAdapter_AllFourKinds(t *testing.T) {
	tmp := t.TempDir()
	store := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, store.Init())
	ss := &secrets.Store{Root: filepath.Join(tmp, "secrets")}
	require.NoError(t, ss.Init())

	a := &Adapter{HomeDir: tmp, DeviceID: "test-dev", SecretsStore: ss}
	ctx := context.Background()

	// 1. Memory
	memPath := filepath.Join(tmp, "MEMORY.md")
	require.NoError(t, os.WriteFile(memPath, []byte("# memory\n"), 0o644))
	memIDs, err := a.ImportMemory(ctx, store, memPath)
	require.NoError(t, err)
	require.Len(t, memIDs, 1)
	memArt, err := store.ReadArtifact(acf.KindMemory, memIDs[0])
	require.NoError(t, err)
	require.Equal(t, acf.KindMemory, memArt.Kind)

	// 2. Skill
	skillPath := filepath.Join(tmp, "SKILL.md")
	require.NoError(t, os.WriteFile(skillPath, []byte("---\nname: x\n---\n# body\n"), 0o644))
	skillIDs, err := a.ImportSkill(ctx, store, skillPath)
	require.NoError(t, err)
	require.Len(t, skillIDs, 1)
	skillArt, err := store.ReadArtifact(acf.KindSkill, skillIDs[0])
	require.NoError(t, err)
	require.Equal(t, acf.KindSkill, skillArt.Kind)

	// 3. Tool
	toolPath := filepath.Join(tmp, "config.yaml")
	require.NoError(t, os.WriteFile(toolPath, []byte(`mcp_servers:
  gh:
    command: uvx
    args: [mcp-server-github]
`), 0o644))
	toolIDs, err := a.ImportTool(ctx, store, toolPath)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(toolIDs), 1)
	toolArt, err := store.ReadArtifact(acf.KindTool, toolIDs[0])
	require.NoError(t, err)
	require.Equal(t, acf.KindTool, toolArt.Kind)

	// 4. Conversation (new in v0.11.0)
	dbPath := filepath.Join(tmp, "state.db")
	db, err := hermesdb.InitTestDB(dbPath)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO sessions (id, source, started_at, title) VALUES ('s1','cli',100.0,'first')`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO messages (session_id, role, content, timestamp) VALUES ('s1','user','hi',101.0)`)
	require.NoError(t, err)
	db.Close()

	convIDs, err := a.ImportConversationsFromDB(ctx, store, dbPath, 0)
	require.NoError(t, err)
	require.Len(t, convIDs, 1)
	convArt, err := store.ReadArtifact(acf.KindConversation, convIDs[0])
	require.NoError(t, err)
	require.Equal(t, acf.KindConversation, convArt.Kind)
	require.Equal(t, acf.ScopeGlobal, convArt.Scope)
}
