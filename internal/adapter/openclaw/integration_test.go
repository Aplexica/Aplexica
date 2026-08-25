package openclaw

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
	"github.com/aplexica/aplexica/internal/adapter/claudecode"
	"github.com/aplexica/aplexica/internal/secrets"
	"github.com/stretchr/testify/require"
)

// TestOpenClawAdapter_SatisfiesAdapterInterface ensures *Adapter implements
// adapter.Adapter — including the v0.14.0 HandlesFormat method.
func TestOpenClawAdapter_SatisfiesAdapterInterface(t *testing.T) {
	var a adapter.Adapter = New()
	require.Equal(t, "openclaw", a.Name())
	require.NotEmpty(t, a.Version())
}

// TestCrossAdapter_ClaudeCodeMemoryToOpenClaw proves the shared "markdown"
// memory format makes claudecode CLAUDE.md ↔ openclaw MEMORY.md interop
// byte-identical — a v0.23.0 milestone-completing assertion.
func TestCrossAdapter_ClaudeCodeMemoryToOpenClaw(t *testing.T) {
	tmp := t.TempDir()
	storeRoot := filepath.Join(tmp, "store")
	store := &acf.Store{Root: storeRoot}
	require.NoError(t, store.Init())

	// Import via claudecode (CLAUDE.md).
	cc := claudecode.New()
	cc.HomeDir = tmp
	cc.SecretsStore = &secrets.Store{Root: filepath.Join(tmp, "sec")}
	require.NoError(t, cc.SecretsStore.Init())

	claudePath := filepath.Join(tmp, "CLAUDE.md")
	body := "# memory body via claudecode\n"
	require.NoError(t, os.WriteFile(claudePath, []byte(body), 0o644))

	ids, err := cc.ImportMemory(context.Background(), store, claudePath)
	require.NoError(t, err)
	require.Len(t, ids, 1)

	// Export via openclaw (MEMORY.md).
	oc := New()
	oc.HomeDir = tmp
	oc.SecretsStore = cc.SecretsStore

	outDir := filepath.Join(tmp, "openclaw-out")
	require.NoError(t, os.MkdirAll(outDir, 0o755))
	outPath := filepath.Join(outDir, "MEMORY.md")
	require.NoError(t, oc.ExportMemory(context.Background(), store, ids[0], outPath))

	got, err := os.ReadFile(outPath)
	require.NoError(t, err)
	require.Equal(t, body, string(got), "claudecode → openclaw memory round-trip must be byte-identical (shared markdown format)")
}

// TestCrossAdapter_ClaudeCodeMCPToOpenClawTool proves the shared canonical
// mcp.v1 tool format makes claudecode .mcp.json ↔ openclaw openclaw.json
// interop work end-to-end with secrets expanded correctly into the OpenClaw
// nested mcp.servers shape.
func TestCrossAdapter_ClaudeCodeMCPToOpenClawTool(t *testing.T) {
	tmp := t.TempDir()
	storeRoot := filepath.Join(tmp, "store")
	store := &acf.Store{Root: storeRoot}
	require.NoError(t, store.Init())
	ss := &secrets.Store{Root: filepath.Join(tmp, "sec")}
	require.NoError(t, ss.Init())

	// Import via claudecode (.mcp.json shape).
	cc := claudecode.New()
	cc.HomeDir = tmp
	cc.SecretsStore = ss

	claudeMCP := filepath.Join(tmp, ".mcp.json")
	require.NoError(t, os.WriteFile(claudeMCP, []byte(`{
		"mcpServers": {
			"github": {"command": "node", "args": ["mcp-github.js"], "env": {"GITHUB_TOKEN": "shh"}}
		}
	}`), 0o644))

	ids, err := cc.ImportTool(context.Background(), store, claudeMCP)
	require.NoError(t, err)
	require.Len(t, ids, 1)

	// Export via openclaw (openclaw.json shape).
	oc := New()
	oc.HomeDir = tmp
	oc.SecretsStore = ss

	outPath := filepath.Join(tmp, "openclaw-out.json")
	require.NoError(t, oc.ExportTool(context.Background(), store, ids[0], outPath))

	got, err := os.ReadFile(outPath)
	require.NoError(t, err)
	require.Contains(t, string(got), `"mcp"`)
	require.Contains(t, string(got), `"servers"`)
	require.Contains(t, string(got), `"github"`)
	require.Contains(t, string(got), "shh", "secrets must be expanded into the openclaw fragment")
}
