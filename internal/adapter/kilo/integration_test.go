package kilo

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
	"github.com/aplexica/aplexica/internal/adapter/claudecode"
	"github.com/aplexica/aplexica/internal/adapter/codex"
	"github.com/aplexica/aplexica/internal/secrets"
	"github.com/stretchr/testify/require"
)

func TestCrossAdapter_Memory_KiloImport_CodexExport(t *testing.T) {
	tmp := t.TempDir()
	store := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, store.Init())

	src := filepath.Join(tmp, "AGENTS.md")
	content := "# Shared memory\n\nPara.\n"
	require.NoError(t, os.WriteFile(src, []byte(content), 0o644))

	k := New()
	ids, err := k.ImportMemory(context.Background(), store, src)
	require.NoError(t, err)
	require.Len(t, ids, 1)

	cx := codex.New()
	out := filepath.Join(tmp, "out", "AGENTS.md")
	require.NoError(t, cx.ExportMemory(context.Background(), store, ids[0], out))

	got, err := os.ReadFile(out)
	require.NoError(t, err)
	require.Equal(t, content, string(got),
		"kilo→codex memory round-trip must be byte-identical (both use Format: markdown)")
}

func TestCrossAdapter_Skill_KiloImport_ClaudeCodeExport(t *testing.T) {
	tmp := t.TempDir()
	store := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, store.Init())

	src := filepath.Join(tmp, "SKILL.md")
	content := "---\nname: shared\n---\n# Shared skill body\n"
	require.NoError(t, os.WriteFile(src, []byte(content), 0o644))

	k := New()
	ids, err := k.ImportSkill(context.Background(), store, src)
	require.NoError(t, err)
	require.Len(t, ids, 1)

	cc := claudecode.New()
	out := filepath.Join(tmp, "out", "SKILL.md")
	require.NoError(t, cc.ExportSkill(context.Background(), store, ids[0], out))

	// Cross-AGENT materialization carries the provenance marker (the echo
	// guard that keeps watched fan-out copies from minting duplicate
	// artifacts); stripping it recovers the original bytes exactly.
	got, err := os.ReadFile(out)
	require.NoError(t, err)
	id, stripped, found := adapter.ParseSkillMarker(got)
	require.True(t, found, "cross-agent skill copy must carry the provenance marker")
	require.Equal(t, ids[0], id)
	require.Equal(t, content, string(stripped),
		"kilo→claudecode skill content must be byte-identical above the marker (both use Format: skill.md)")
}

func TestCrossAdapter_Memory_CodexImport_KiloExport(t *testing.T) {
	tmp := t.TempDir()
	store := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, store.Init())

	src := filepath.Join(tmp, "AGENTS.md")
	content := "# Origin codex\n"
	require.NoError(t, os.WriteFile(src, []byte(content), 0o644))

	cx := codex.New()
	ids, err := cx.ImportMemory(context.Background(), store, src)
	require.NoError(t, err)
	require.Len(t, ids, 1)

	k := New()
	out := filepath.Join(tmp, "out", "AGENTS.md")
	require.NoError(t, k.ExportMemory(context.Background(), store, ids[0], out))

	got, err := os.ReadFile(out)
	require.NoError(t, err)
	require.Equal(t, content, string(got),
		"codex→kilo memory round-trip must be byte-identical")
}

func TestCrossAdapter_Tool_KiloImport_ClaudeCodeExport(t *testing.T) {
	tmp := t.TempDir()
	store := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, store.Init())
	ss := &secrets.Store{Root: filepath.Join(tmp, "secrets")}
	require.NoError(t, ss.Init())

	in := filepath.Join(tmp, "kilo.jsonc")
	require.NoError(t, os.MkdirAll(filepath.Dir(in), 0o755))
	require.NoError(t, os.WriteFile(in,
		[]byte(`{"mcp":{"gh":{"type":"local","command":["uvx","mcp-server-github"],"environment":{"TOKEN":"shh"}}}}`),
		0o644))

	k := &Adapter{HomeDir: tmp, DeviceID: "dev", SecretsStore: ss}
	ids, err := k.ImportTool(context.Background(), store, in)
	require.NoError(t, err)
	require.Len(t, ids, 1)

	cc := claudecode.New()
	cc.SecretsStore = ss
	out := filepath.Join(tmp, "out", ".mcp.json")
	require.NoError(t, cc.ExportTool(context.Background(), store, ids[0], out))

	gotBytes, err := os.ReadFile(out)
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal(gotBytes, &got))

	servers, ok := got["mcpServers"].(map[string]any)
	require.True(t, ok, "claudecode export must have mcpServers wrapper")
	gh, ok := servers["gh"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "stdio", gh["type"], "kilo 'local' → canonical 'stdio' → claudecode 'stdio'")
	require.Equal(t, "uvx", gh["command"], "kilo command-array → canonical command (string) → claudecode command")
	require.Equal(t, []any{"mcp-server-github"}, gh["args"])
	env := gh["env"].(map[string]any)
	require.Equal(t, "shh", env["TOKEN"], "secret expanded via shared secrets store")
}

func TestCrossAdapter_Tool_KiloImport_CodexExport(t *testing.T) {
	tmp := t.TempDir()
	store := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, store.Init())
	ss := &secrets.Store{Root: filepath.Join(tmp, "secrets")}
	require.NoError(t, ss.Init())

	in := filepath.Join(tmp, "kilo.jsonc")
	require.NoError(t, os.MkdirAll(filepath.Dir(in), 0o755))
	require.NoError(t, os.WriteFile(in,
		[]byte(`{"mcp":{"gh":{"type":"local","command":["uvx"],"environment":{"TOKEN":"shh"}}}}`),
		0o644))

	k := &Adapter{HomeDir: tmp, DeviceID: "dev", SecretsStore: ss}
	ids, err := k.ImportTool(context.Background(), store, in)
	require.NoError(t, err)
	require.Len(t, ids, 1)

	cx := codex.New()
	cx.SecretsStore = ss
	out := filepath.Join(tmp, "out", "config.toml")
	require.NoError(t, cx.ExportTool(context.Background(), store, ids[0], out))

	gotBytes, err := os.ReadFile(out)
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, toml.Unmarshal(gotBytes, &got))

	servers := got["mcp_servers"].(map[string]any)
	gh := servers["gh"].(map[string]any)
	require.Equal(t, "stdio", gh["type"], "kilo→canonical→codex preserves stdio")
	require.Equal(t, "uvx", gh["command"])
	env := gh["env"].(map[string]any)
	require.Equal(t, "shh", env["TOKEN"])
}

func TestCrossAdapter_Tool_ClaudeCodeImport_KiloExport(t *testing.T) {
	tmp := t.TempDir()
	store := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, store.Init())
	ss := &secrets.Store{Root: filepath.Join(tmp, "secrets")}
	require.NoError(t, ss.Init())

	in := filepath.Join(tmp, ".mcp.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(in), 0o755))
	require.NoError(t, os.WriteFile(in,
		[]byte(`{"mcpServers":{"gh":{"type":"stdio","command":"uvx","args":["mcp-server-github"],"env":{"TOKEN":"shh"}}}}`),
		0o644))

	cc := claudecode.New()
	cc.SecretsStore = ss
	cc.HomeDir = tmp
	ids, err := cc.ImportTool(context.Background(), store, in)
	require.NoError(t, err)
	require.Len(t, ids, 1)

	k := &Adapter{HomeDir: tmp, DeviceID: "dev", SecretsStore: ss}
	out := filepath.Join(tmp, "out", "kilo.jsonc")
	require.NoError(t, k.ExportTool(context.Background(), store, ids[0], out))

	gotBytes, err := os.ReadFile(out)
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal(gotBytes, &got))

	mcp := got["mcp"].(map[string]any)
	gh := mcp["gh"].(map[string]any)
	require.Equal(t, "local", gh["type"], "canonical 'stdio' → kilo 'local'")
	cmd := gh["command"].([]any)
	require.Equal(t, []any{"uvx", "mcp-server-github"}, cmd,
		"canonical command+args → kilo single command array")
	env := gh["environment"].(map[string]any)
	require.Equal(t, "shh", env["TOKEN"])
}
