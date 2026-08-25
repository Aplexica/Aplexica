package kilo

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/project"
	"github.com/aplexica/aplexica/internal/secrets"
	"github.com/stretchr/testify/require"
)

func TestImportTool_WritesArtifactAndStoresSecrets(t *testing.T) {
	tmp := t.TempDir()
	store := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, store.Init())
	ss := &secrets.Store{Root: filepath.Join(tmp, "secrets")}
	require.NoError(t, ss.Init())

	src := filepath.Join(tmp, "kilo.jsonc")
	require.NoError(t, os.WriteFile(src,
		[]byte(`{"mcp":{"gh":{"type":"local","command":["uvx","mcp-server-github"],"environment":{"TOKEN":"shh"}}}}`),
		0o644))

	a := &Adapter{HomeDir: tmp, DeviceID: "dev", SecretsStore: ss}
	ids, err := a.ImportTool(context.Background(), store, src)
	require.NoError(t, err)
	require.Len(t, ids, 1)

	got, err := store.ReadArtifact(acf.KindTool, ids[0])
	require.NoError(t, err)
	require.Equal(t, acf.KindTool, got.Kind)
	require.Equal(t, "kilo.jsonc", got.Name)

	events, err := store.ReadEvents(acf.KindTool, ids[0])
	require.NoError(t, err)
	require.Len(t, events, 1)

	payload, err := acf.DecodeToolPayload(events[0])
	require.NoError(t, err)
	require.Equal(t, "acf.mcp.v1", payload.Format,
		"Kilo tool artifacts use the canonical Format string (same as claudecode and codex)")
	require.NotContains(t, payload.Content, "shh",
		"canonical store MUST NOT contain raw secret values (ADR-0024)")
	require.Contains(t, payload.Content, "${secret:gh.TOKEN}",
		"canonical store MUST contain the redaction reference")

	val, err := ss.Get(ids[0], "gh.TOKEN")
	require.NoError(t, err)
	require.Equal(t, "shh", val)
}

// TestImportTool_GlobalConfig_NotRecapturedByParentProject mirrors the shared
// ImportOpaqueContent guard for kilo's separate tool-import path. A kilo MCP
// config under kilo's user-level config root (~/.config/kilo → ScopeGlobal by
// inferScope) must stay global even when $HOME is a registered local project
// (the daemon's implicit --dir registration). Otherwise the global tool config
// is recaptured into the home project and stranded in `pending` on any device
// that hasn't linked that home project.
func TestImportTool_GlobalConfig_NotRecapturedByParentProject(t *testing.T) {
	home := t.TempDir()
	store := &acf.Store{Root: filepath.Join(home, "store")}
	require.NoError(t, store.Init())
	ss := &secrets.Store{Root: filepath.Join(home, "secrets")}
	require.NoError(t, ss.Init())

	// Register $HOME as an implicit local project, mirroring daemon startup.
	reg, err := project.NewRegistry(filepath.Join(t.TempDir(), "projects.json"))
	require.NoError(t, err)
	require.NoError(t, reg.AddOrUpdate(project.Entry{
		ID: "local:home", Path: home, VCS: "none", Scope: "local",
	}))

	// A kilo MCP config under kilo's user-level config root (global by inferScope).
	cfgDir := filepath.Join(home, ".config", "kilo")
	require.NoError(t, os.MkdirAll(cfgDir, 0o755))
	src := filepath.Join(cfgDir, "kilo.jsonc")
	require.NoError(t, os.WriteFile(src,
		[]byte(`{"mcp":{"gh":{"type":"local","command":["uvx","x"]}}}`), 0o644))

	a := &Adapter{HomeDir: home, DeviceID: "dev", SecretsStore: ss, Registry: reg}
	ids, err := a.ImportTool(context.Background(), store, src)
	require.NoError(t, err)
	require.Len(t, ids, 1)

	got, err := store.ReadArtifact(acf.KindTool, ids[0])
	require.NoError(t, err)
	require.Equal(t, acf.ScopeGlobal, got.Scope,
		"a kilo global-config tool must stay global even under a registered $HOME project")
	require.Nil(t, got.Project,
		"global tool must not be attributed to the parent (home) project")
}

func TestExportTool_ReassemblesSecretsToKiloFormat(t *testing.T) {
	tmp := t.TempDir()
	store := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, store.Init())
	ss := &secrets.Store{Root: filepath.Join(tmp, "secrets")}
	require.NoError(t, ss.Init())

	src := filepath.Join(tmp, "kilo.jsonc")
	original := `{"mcp":{"gh":{"type":"local","command":["uvx"],"environment":{"TOKEN":"shh"}}}}`
	require.NoError(t, os.WriteFile(src, []byte(original), 0o644))

	a := &Adapter{HomeDir: tmp, DeviceID: "dev", SecretsStore: ss}
	ids, err := a.ImportTool(context.Background(), store, src)
	require.NoError(t, err)

	dest := filepath.Join(tmp, "out", "kilo.jsonc")
	require.NoError(t, a.ExportTool(context.Background(), store, ids[0], dest))

	gotBytes, err := os.ReadFile(dest)
	require.NoError(t, err)

	var inObj, outObj map[string]any
	require.NoError(t, json.Unmarshal([]byte(original), &inObj))
	require.NoError(t, json.Unmarshal(gotBytes, &outObj))

	mcp := outObj["mcp"].(map[string]any)
	gh := mcp["gh"].(map[string]any)
	env := gh["environment"].(map[string]any)
	require.Equal(t, "shh", env["TOKEN"], "export must expand the secret back to its real value")
	require.Equal(t, "local", gh["type"], "export must use Kilo's type names")
}

func TestImportTool_StripsCommentsFromJSONC(t *testing.T) {
	tmp := t.TempDir()
	store := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, store.Init())
	ss := &secrets.Store{Root: filepath.Join(tmp, "secrets")}
	require.NoError(t, ss.Init())

	src := filepath.Join(tmp, "kilo.jsonc")
	jsonc := `{
		// my mcp servers
		"mcp": {
			"gh": {
				"type": "local", /* stdio */
				"command": ["uvx"],
				"environment": {"TOKEN": "shh"}
			}
		}
	}`
	require.NoError(t, os.WriteFile(src, []byte(jsonc), 0o644))

	a := &Adapter{HomeDir: tmp, DeviceID: "dev", SecretsStore: ss}
	ids, err := a.ImportTool(context.Background(), store, src)
	require.NoError(t, err)
	require.Len(t, ids, 1)

	events, err := store.ReadEvents(acf.KindTool, ids[0])
	require.NoError(t, err)
	payload, err := acf.DecodeToolPayload(events[0])
	require.NoError(t, err)
	require.NotContains(t, payload.Content, "//", "comments must not appear in canonical content")
}

// TestImportTool_ReImportUnchanged_NoNewEvent: re-importing an identical
// kilo.jsonc (same redacted payload AND same secret) must reuse the id and
// append no event. A secret rotation (the test below) still appends.
func TestImportTool_ReImportUnchanged_NoNewEvent(t *testing.T) {
	tmp := t.TempDir()
	store := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, store.Init())
	ss := &secrets.Store{Root: filepath.Join(tmp, "secrets")}
	require.NoError(t, ss.Init())

	src := filepath.Join(tmp, "kilo.jsonc")
	cfg := []byte(`{"mcp":{"gh":{"type":"local","command":["uvx"],"environment":{"TOKEN":"v1"}}}}`)
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

	got, err := ss.Get(ids1[0], "gh.TOKEN")
	require.NoError(t, err)
	require.Equal(t, "v1", got, "the stored secret is left intact")
}

func TestImportTool_ReImportSameFile_ReusesArtifactID(t *testing.T) {
	tmp := t.TempDir()
	store := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, store.Init())
	ss := &secrets.Store{Root: filepath.Join(tmp, "secrets")}
	require.NoError(t, ss.Init())

	src := filepath.Join(tmp, "kilo.jsonc")
	require.NoError(t, os.WriteFile(src,
		[]byte(`{"mcp":{"gh":{"type":"local","command":["uvx"],"environment":{"TOKEN":"v1"}}}}`),
		0o644))

	a := &Adapter{HomeDir: tmp, DeviceID: "dev", SecretsStore: ss}
	ids1, err := a.ImportTool(context.Background(), store, src)
	require.NoError(t, err)

	require.NoError(t, os.WriteFile(src,
		[]byte(`{"mcp":{"gh":{"type":"local","command":["uvx"],"environment":{"TOKEN":"v2"}}}}`),
		0o644))
	ids2, err := a.ImportTool(context.Background(), store, src)
	require.NoError(t, err)

	require.Equal(t, ids1[0], ids2[0],
		"re-import of same kilo.jsonc must reuse artifact ID (v0.2.0 reconciliation)")

	got, err := ss.Get(ids1[0], "gh.TOKEN")
	require.NoError(t, err)
	require.Equal(t, "v2", got)
}

func TestImportTool_NoMCPSection_NoSecrets(t *testing.T) {
	tmp := t.TempDir()
	store := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, store.Init())
	ss := &secrets.Store{Root: filepath.Join(tmp, "secrets")}
	require.NoError(t, ss.Init())

	src := filepath.Join(tmp, "kilo.jsonc")
	require.NoError(t, os.WriteFile(src, []byte(`{"someOtherKey": 42}`), 0o644))

	a := &Adapter{HomeDir: tmp, DeviceID: "dev", SecretsStore: ss}
	ids, err := a.ImportTool(context.Background(), store, src)
	require.NoError(t, err)
	require.Len(t, ids, 1)

	keys, err := ss.ListForArtifact(ids[0])
	require.NoError(t, err)
	require.Empty(t, keys, "no mcp section = no secrets to store")
}

func TestImportTool_RejectsURLWithDoubleSlash_FromJSONCStripper(t *testing.T) {
	tmp := t.TempDir()
	store := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, store.Init())
	ss := &secrets.Store{Root: filepath.Join(tmp, "secrets")}
	require.NoError(t, ss.Init())

	src := filepath.Join(tmp, "kilo.jsonc")
	jsonc := `{"mcp": {"cf": {"type": "streamable-http", "url": "https://mcp.cloudflare.com/v1"}}}`
	require.NoError(t, os.WriteFile(src, []byte(jsonc), 0o644))

	a := &Adapter{HomeDir: tmp, DeviceID: "dev", SecretsStore: ss}
	ids, err := a.ImportTool(context.Background(), store, src)
	require.NoError(t, err)

	dest := filepath.Join(tmp, "out", "kilo.jsonc")
	require.NoError(t, a.ExportTool(context.Background(), store, ids[0], dest))

	gotBytes, err := os.ReadFile(dest)
	require.NoError(t, err)
	require.True(t, strings.Contains(string(gotBytes), "https://mcp.cloudflare.com/v1"),
		"the URL must survive import → export verbatim")
}
