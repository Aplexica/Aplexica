package hermes

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/secrets"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestImportTool_WritesArtifactAndStoresSecrets(t *testing.T) {
	tmp := t.TempDir()
	store := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, store.Init())
	ss := &secrets.Store{Root: filepath.Join(tmp, "secrets")}
	require.NoError(t, ss.Init())

	src := filepath.Join(tmp, "config.yaml")
	yamlContent := `
mcp_servers:
  gh:
    command: uvx
    args: [mcp-server-github]
    env:
      TOKEN: shh
`
	require.NoError(t, os.WriteFile(src, []byte(yamlContent), 0o644))

	a := &Adapter{HomeDir: tmp, DeviceID: "dev", SecretsStore: ss}
	ids, err := a.ImportTool(context.Background(), store, src)
	require.NoError(t, err)
	require.Len(t, ids, 1)

	events, err := store.ReadEvents(acf.KindTool, ids[0])
	require.NoError(t, err)
	payload, err := acf.DecodeToolPayload(events[0])
	require.NoError(t, err)
	require.Equal(t, "acf.mcp.v1", payload.Format)
	require.NotContains(t, payload.Content, "shh", "no raw secret in canonical payload")
	require.Contains(t, payload.Content, "${secret:gh.TOKEN}")

	val, err := ss.Get(ids[0], "gh.TOKEN")
	require.NoError(t, err)
	require.Equal(t, "shh", val)
}

func TestExportTool_ReassemblesSecretsToHermesYAML(t *testing.T) {
	tmp := t.TempDir()
	store := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, store.Init())
	ss := &secrets.Store{Root: filepath.Join(tmp, "secrets")}
	require.NoError(t, ss.Init())

	src := filepath.Join(tmp, "config.yaml")
	require.NoError(t, os.WriteFile(src, []byte(`
mcp_servers:
  gh:
    command: uvx
    env:
      TOKEN: shh
`), 0o644))

	a := &Adapter{HomeDir: tmp, DeviceID: "dev", SecretsStore: ss}
	ids, err := a.ImportTool(context.Background(), store, src)
	require.NoError(t, err)

	dest := filepath.Join(tmp, "out", "config.yaml")
	require.NoError(t, a.ExportTool(context.Background(), store, ids[0], dest))

	gotBytes, err := os.ReadFile(dest)
	require.NoError(t, err)

	var got map[string]any
	require.NoError(t, yaml.Unmarshal(gotBytes, &got))
	mcp := got["mcp_servers"].(map[string]any)
	gh := mcp["gh"].(map[string]any)
	env := gh["env"].(map[string]any)
	require.Equal(t, "shh", env["TOKEN"], "secret expanded in export")
}

// TestImportTool_ReImportUnchanged_NoNewEvent: re-importing an identical
// config.yaml (same redacted payload AND same secret) must reuse the id and
// append no event. A secret rotation (the test below) still appends.
func TestImportTool_ReImportUnchanged_NoNewEvent(t *testing.T) {
	tmp := t.TempDir()
	store := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, store.Init())
	ss := &secrets.Store{Root: filepath.Join(tmp, "secrets")}
	require.NoError(t, ss.Init())

	src := filepath.Join(tmp, "config.yaml")
	cfg := []byte("mcp_servers:\n  gh:\n    env:\n      TOKEN: v1\n")
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

	src := filepath.Join(tmp, "config.yaml")
	require.NoError(t, os.WriteFile(src, []byte(`mcp_servers:
  gh:
    env:
      TOKEN: v1
`), 0o644))

	a := &Adapter{HomeDir: tmp, DeviceID: "dev", SecretsStore: ss}
	ids1, err := a.ImportTool(context.Background(), store, src)
	require.NoError(t, err)

	require.NoError(t, os.WriteFile(src, []byte(`mcp_servers:
  gh:
    env:
      TOKEN: v2
`), 0o644))
	ids2, err := a.ImportTool(context.Background(), store, src)
	require.NoError(t, err)

	require.Equal(t, ids1[0], ids2[0], "re-import same config.yaml must reuse ArtifactID")

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

	src := filepath.Join(tmp, "config.yaml")
	require.NoError(t, os.WriteFile(src, []byte("memory_enabled: true\nplugins: []\n"), 0o644))

	a := &Adapter{HomeDir: tmp, DeviceID: "dev", SecretsStore: ss}
	ids, err := a.ImportTool(context.Background(), store, src)
	require.NoError(t, err)

	keys, err := ss.ListForArtifact(ids[0])
	require.NoError(t, err)
	require.Empty(t, keys, "no mcp_servers section = no secrets")
}

func TestDispatch_ConfigYaml_RoutesToTool(t *testing.T) {
	// Now that ImportTool works, the dispatch routing to it can be tested.
	tmp := t.TempDir()
	store := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, store.Init())
	ss := &secrets.Store{Root: filepath.Join(tmp, "secrets")}
	require.NoError(t, ss.Init())

	src := filepath.Join(tmp, "config.yaml")
	require.NoError(t, os.WriteFile(src, []byte("mcp_servers:\n  x:\n    command: c\n"), 0o644))

	a := &Adapter{HomeDir: tmp, DeviceID: "dev", SecretsStore: ss}
	ids, err := a.Import(context.Background(), store, src)
	require.NoError(t, err)
	require.Len(t, ids, 1)

	got, err := store.ReadArtifact(acf.KindTool, ids[0])
	require.NoError(t, err)
	require.Equal(t, acf.KindTool, got.Kind)
}
