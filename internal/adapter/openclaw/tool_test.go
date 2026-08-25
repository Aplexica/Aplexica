package openclaw

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/secrets"
	"github.com/stretchr/testify/require"
)

func TestImportTool_RoundTrip(t *testing.T) {
	tmp := t.TempDir()
	storeRoot := filepath.Join(tmp, "store")
	store := &acf.Store{Root: storeRoot}
	require.NoError(t, store.Init())

	ss := &secrets.Store{Root: filepath.Join(tmp, "sec")}
	require.NoError(t, ss.Init())

	a := &Adapter{HomeDir: tmp, DeviceID: "dev", SecretsStore: ss}

	configPath := filepath.Join(tmp, "openclaw.json")
	cfg := `{
		"mcp": {
			"servers": {
				"github": {
					"transport": "stdio",
					"command": "node",
					"args": ["mcp-github.js"],
					"env": {"GITHUB_TOKEN": "secret-value"}
				}
			}
		}
	}`
	require.NoError(t, os.WriteFile(configPath, []byte(cfg), 0o644))

	ids, err := a.ImportTool(context.Background(), store, configPath)
	require.NoError(t, err)
	require.Len(t, ids, 1)

	// Verify the secret made it to the secrets store, NOT into the canonical store.
	// mcp.ExtractSecrets keys secrets by "<serverName>.<envKey>".
	val, err := ss.Get(ids[0], "github.GITHUB_TOKEN")
	require.NoError(t, err)
	require.Equal(t, "secret-value", val)

	// ADR-0027 invariant: canonical payload must NOT contain the raw secret value.
	events, err := store.ReadEvents(acf.KindTool, ids[0])
	require.NoError(t, err)
	require.Len(t, events, 1)
	var p acf.ToolPayload
	require.NoError(t, json.Unmarshal(events[0].Payload, &p))
	require.NotContains(t, p.Content, "secret-value", "ADR-0027: raw secret values must never appear in canonical payloads")

	// Round-trip back to disk; secrets must be expanded.
	outPath := filepath.Join(tmp, "out-openclaw.json")
	require.NoError(t, a.ExportTool(context.Background(), store, ids[0], outPath))
	got, err := os.ReadFile(outPath)
	require.NoError(t, err)
	require.Contains(t, string(got), `"github"`)
	require.Contains(t, string(got), "secret-value", "secrets should be expanded into the exported file")
	require.True(t, strings.Contains(string(got), `"mcp"`) && strings.Contains(string(got), `"servers"`),
		"exported openclaw.json must use the nested mcp.servers shape")
}

func TestExportTool_PreservesNonMCPKeysInExistingOpenclawJSON(t *testing.T) {
	tmp := t.TempDir()
	storeRoot := filepath.Join(tmp, "store")
	store := &acf.Store{Root: storeRoot}
	require.NoError(t, store.Init())
	ss := &secrets.Store{Root: filepath.Join(tmp, "sec")}
	require.NoError(t, ss.Init())
	a := &Adapter{HomeDir: tmp, DeviceID: "dev", SecretsStore: ss}

	src := filepath.Join(tmp, "openclaw.json")
	require.NoError(t, os.WriteFile(src, []byte(`{
		"mcp": {"servers": {"alpha": {"command": "node", "args": ["a.js"]}}}
	}`), 0o644))
	ids, err := a.ImportTool(context.Background(), store, src)
	require.NoError(t, err)
	require.Len(t, ids, 1)

	// Seed the destination with non-MCP top-level keys.
	dst := filepath.Join(tmp, "openclaw-out.json")
	require.NoError(t, os.WriteFile(dst, []byte(`{
		"channels": {"discord": {"enabled": true}},
		"agents": {"defaults": {"workspace": "/x"}}
	}`), 0o644))

	require.NoError(t, a.ExportTool(context.Background(), store, ids[0], dst))

	out, err := os.ReadFile(dst)
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal(out, &got))
	require.NotNil(t, got["channels"], "channels block must survive export")
	require.NotNil(t, got["agents"], "agents block must survive export")
	require.NotNil(t, got["mcp"])
}

// TestImportTool_ReImportUnchanged_NoNewEvent: re-importing an identical
// openclaw.json (same redacted payload AND same secret) must reuse the id and
// append no event.
func TestImportTool_ReImportUnchanged_NoNewEvent(t *testing.T) {
	tmp := t.TempDir()
	store := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, store.Init())
	ss := &secrets.Store{Root: filepath.Join(tmp, "sec")}
	require.NoError(t, ss.Init())

	src := filepath.Join(tmp, "openclaw.json")
	cfg := []byte(`{"mcp":{"servers":{"github":{"command":"node","env":{"GITHUB_TOKEN":"v1"}}}}}`)
	require.NoError(t, os.WriteFile(src, cfg, 0o644))

	a := &Adapter{HomeDir: tmp, DeviceID: "dev", SecretsStore: ss}
	ids1, err := a.ImportTool(context.Background(), store, src)
	require.NoError(t, err)
	require.Len(t, ids1, 1)

	ids2, err := a.ImportTool(context.Background(), store, src)
	require.NoError(t, err)
	require.Equal(t, ids1, ids2, "unchanged re-import must reuse the artifact ID")

	events, err := store.ReadEvents(acf.KindTool, ids1[0])
	require.NoError(t, err)
	require.Len(t, events, 1, "unchanged re-import must NOT append a redundant event")

	got, err := ss.Get(ids1[0], "github.GITHUB_TOKEN")
	require.NoError(t, err)
	require.Equal(t, "v1", got, "the stored secret is left intact")
}

// TestImportTool_RotatedSecret_AppendsEvent: rotating a secret leaves the
// redacted payload byte-identical but MUST still append an update event so the
// new secret value fans out to other agents.
func TestImportTool_RotatedSecret_AppendsEvent(t *testing.T) {
	tmp := t.TempDir()
	store := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, store.Init())
	ss := &secrets.Store{Root: filepath.Join(tmp, "sec")}
	require.NoError(t, ss.Init())

	src := filepath.Join(tmp, "openclaw.json")
	a := &Adapter{HomeDir: tmp, DeviceID: "dev", SecretsStore: ss}

	require.NoError(t, os.WriteFile(src,
		[]byte(`{"mcp":{"servers":{"github":{"command":"node","env":{"GITHUB_TOKEN":"v1"}}}}}`), 0o644))
	ids1, err := a.ImportTool(context.Background(), store, src)
	require.NoError(t, err)
	require.Len(t, ids1, 1)

	require.NoError(t, os.WriteFile(src,
		[]byte(`{"mcp":{"servers":{"github":{"command":"node","env":{"GITHUB_TOKEN":"v2"}}}}}`), 0o644))
	ids2, err := a.ImportTool(context.Background(), store, src)
	require.NoError(t, err)
	require.Equal(t, ids1, ids2, "rotation reuses the artifact ID")

	events, err := store.ReadEvents(acf.KindTool, ids1[0])
	require.NoError(t, err)
	require.Len(t, events, 2, "a secret rotation must append an update event so it fans out")
	require.Equal(t, acf.EventTypeUpdate, events[1].Type)

	got, err := ss.Get(ids1[0], "github.GITHUB_TOKEN")
	require.NoError(t, err)
	require.Equal(t, "v2", got, "rotation overwrites the stored secret")
}
