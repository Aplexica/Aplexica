package claudecode

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/secrets"
	"github.com/stretchr/testify/require"
)

func userCfgAdapter(t *testing.T, home string) *Adapter {
	t.Helper()
	ss := &secrets.Store{Root: filepath.Join(home, ".aplexica", "secrets")}
	require.NoError(t, ss.Init())
	return &Adapter{HomeDir: home, DeviceID: "dev", SecretsStore: ss}
}

// `claude mcp add -s user` writes the mcpServers key into ~/.claude.json —
// the ONLY user-scope MCP location Claude Code reads. The regression found this
// file was neither watched nor recognized, so agent-native MCP adds were
// invisible to sync (and exports went to ~/.claude/.mcp.json, which Claude
// Code never loads).
func TestImportTool_UserConfig_GlobalScope(t *testing.T) {
	home := t.TempDir()
	store := &acf.Store{Root: filepath.Join(home, "store")}
	require.NoError(t, store.Init())
	a := userCfgAdapter(t, home)

	cfg := filepath.Join(home, ".claude.json")
	require.NoError(t, os.WriteFile(cfg, []byte(`{
		"numStartups": 42,
		"mcpServers": {"gh": {"type": "stdio", "command": "uvx", "args": ["mcp-server-github"]}},
		"projects": {"/Users/u/x": {"history": []}}
	}`), 0o600))

	ids, err := a.ImportTool(context.Background(), store, cfg)
	require.NoError(t, err)
	require.Len(t, ids, 1)
	art, err := store.ReadArtifact(acf.KindTool, ids[0])
	require.NoError(t, err)
	require.Equal(t, acf.ScopeGlobal, art.Scope,
		"~/.claude.json is the user-scope config: global, despite living outside ~/.claude/")
}

func TestNativePath_Tool_Global_UserConfig(t *testing.T) {
	a := &Adapter{HomeDir: "/home/u"}
	p, supports, err := a.NativePath(acf.Artifact{Kind: acf.KindTool, Scope: acf.ScopeGlobal}, "")
	require.NoError(t, err)
	require.True(t, supports)
	require.Equal(t, filepath.Join("/home/u", ".claude.json"), p,
		"global MCP must land in ~/.claude.json (the file `claude mcp list` reads), not ~/.claude/.mcp.json")
}

// ExportTool to ~/.claude.json must MERGE the mcpServers key, preserving
// every other key of Claude Code's user config byte-for-byte.
func TestExportTool_UserConfig_MergesPreservingOtherKeys(t *testing.T) {
	home := t.TempDir()
	store := &acf.Store{Root: filepath.Join(home, "store")}
	require.NoError(t, store.Init())
	a := userCfgAdapter(t, home)

	// Source: a codex-side server list arrives as a canonical tool artifact.
	src := filepath.Join(home, "proj", ".mcp.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(src), 0o755))
	require.NoError(t, os.WriteFile(src, []byte(`{"mcpServers":{"newsrv":{"type":"stdio","command":"npx","args":["x"]}}}`), 0o644))
	ids, err := a.ImportTool(context.Background(), store, src)
	require.NoError(t, err)

	dest := filepath.Join(home, ".claude.json")
	require.NoError(t, os.WriteFile(dest, []byte(`{
		"numStartups": 7,
		"oauthAccount": {"emailAddress": "u@example.com"},
		"mcpServers": {"old": {"type": "stdio", "command": "stale"}}
	}`), 0o600))

	require.NoError(t, a.ExportTool(context.Background(), store, ids[0], dest))

	var got map[string]json.RawMessage
	b, err := os.ReadFile(dest)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(b, &got))
	require.Contains(t, string(got["numStartups"]), "7", "unrelated keys must survive")
	require.Contains(t, string(got["oauthAccount"]), "u@example.com", "unrelated keys must survive")
	var servers map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(got["mcpServers"], &servers))
	require.Contains(t, servers, "newsrv", "mcpServers replaced with the synced set")
	require.NotContains(t, servers, "old")

	// An unparseable user config must abort, not be clobbered.
	require.NoError(t, os.WriteFile(dest, []byte("{not json"), 0o600))
	require.Error(t, a.ExportTool(context.Background(), store, ids[0], dest))
	b, _ = os.ReadFile(dest)
	require.Equal(t, "{not json", string(b), "refusal must leave the file untouched")
}

func TestDiscover_WatchesUserConfigFile(t *testing.T) {
	home := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".claude"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(home, ".claude.json"), []byte("{}"), 0o600))
	a := &Adapter{HomeDir: home}
	d, err := a.Discover()
	require.NoError(t, err)
	require.Contains(t, d.WatchFiles, filepath.Join(home, ".claude.json"))
}
