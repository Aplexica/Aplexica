package codex

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/stretchr/testify/require"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/secrets"
)

func TestImportTool_WritesArtifactAndStoresSecrets(t *testing.T) {
	tmp := t.TempDir()
	store := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, store.Init())
	ss := &secrets.Store{Root: filepath.Join(tmp, "secrets")}
	require.NoError(t, ss.Init())

	src := filepath.Join(tmp, "config.toml")
	content := `model = "gpt-5"

[mcp_servers.github]
command = "uvx"

[mcp_servers.github.env]
TOKEN = "secret-value"
`
	writeFile(t, src, content)

	a := &Adapter{HomeDir: tmp, DeviceID: "dev", SecretsStore: ss}
	ids, err := a.ImportTool(context.Background(), store, src)
	require.NoError(t, err)
	require.Len(t, ids, 1)

	got, err := store.ReadArtifact(acf.KindTool, ids[0])
	require.NoError(t, err)
	require.Equal(t, acf.KindTool, got.Kind)
	require.Equal(t, "config.toml", got.Name)

	events, err := store.ReadEvents(acf.KindTool, ids[0])
	require.NoError(t, err)
	require.Len(t, events, 1)
	payload, err := acf.DecodeToolPayload(events[0])
	require.NoError(t, err)
	require.NotContains(t, payload.Content, "secret-value",
		"canonical store MUST NOT contain raw secret values (ADR-0027)")
	require.Contains(t, payload.Content, "${secret:github.TOKEN}",
		"canonical store MUST contain the redaction reference")
	require.Equal(t, "acf.mcp.v1", payload.Format,
		"v0.3.0: tool Format is the canonical schema marker")

	secVal, err := ss.Get(ids[0], "github.TOKEN")
	require.NoError(t, err)
	require.Equal(t, "secret-value", secVal)
}

func TestImportTool_NoEnv_NoSecrets(t *testing.T) {
	tmp := t.TempDir()
	store := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, store.Init())
	ss := &secrets.Store{Root: filepath.Join(tmp, "secrets")}
	require.NoError(t, ss.Init())

	src := filepath.Join(tmp, "config.toml")
	writeFile(t, src, `[mcp_servers.cf]
type = "http"
url = "https://x"
`)

	a := &Adapter{HomeDir: tmp, DeviceID: "dev", SecretsStore: ss}
	ids, err := a.ImportTool(context.Background(), store, src)
	require.NoError(t, err)

	keys, err := ss.ListForArtifact(ids[0])
	require.NoError(t, err)
	require.Empty(t, keys)
}

func TestExportTool_ReassemblesSecretsFromStore(t *testing.T) {
	tmp := t.TempDir()
	store := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, store.Init())
	ss := &secrets.Store{Root: filepath.Join(tmp, "secrets")}
	require.NoError(t, ss.Init())

	src := filepath.Join(tmp, "config.toml")
	original := `[mcp_servers.github]
command = "uvx"

[mcp_servers.github.env]
TOKEN = "shh"
`
	writeFile(t, src, original)

	a := &Adapter{HomeDir: tmp, DeviceID: "dev", SecretsStore: ss}
	ids, err := a.ImportTool(context.Background(), store, src)
	require.NoError(t, err)

	dest := filepath.Join(tmp, "out", "config.toml")
	require.NoError(t, a.ExportTool(context.Background(), store, ids[0], dest))

	got, err := os.ReadFile(dest)
	require.NoError(t, err)

	// SEMANTIC equivalence: parse both, compare mcp_servers structures.
	var inObj, outObj map[string]any
	require.NoError(t, toml.Unmarshal([]byte(original), &inObj))
	require.NoError(t, toml.Unmarshal(got, &outObj))
	require.Equal(t, inObj["mcp_servers"], outObj["mcp_servers"],
		"tool export must be semantically equivalent on mcp_servers after secret expansion")
}

func TestExportTool_MissingSecretIsError(t *testing.T) {
	tmp := t.TempDir()
	store := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, store.Init())
	ss := &secrets.Store{Root: filepath.Join(tmp, "secrets")}
	require.NoError(t, ss.Init())

	src := filepath.Join(tmp, "config.toml")
	writeFile(t, src, `[mcp_servers.x.env]
K = "v"
`)

	a := &Adapter{HomeDir: tmp, DeviceID: "dev", SecretsStore: ss}
	ids, err := a.ImportTool(context.Background(), store, src)
	require.NoError(t, err)

	require.NoError(t, os.RemoveAll(filepath.Join(ss.Root, ids[0])))

	err = a.ExportTool(context.Background(), store, ids[0], filepath.Join(tmp, "out.toml"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "x.K")
}

func TestImportTool_Codex_RollsBackOrphanArtifactAndSecretsOnAppendFailure(t *testing.T) {
	tmp := t.TempDir()
	store := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, store.Init())
	ss := &secrets.Store{Root: filepath.Join(tmp, "secrets")}
	require.NoError(t, ss.Init())

	src := filepath.Join(tmp, "config.toml")
	writeFile(t, src, "[mcp_servers.gh]\ncommand = \"uvx\"\n[mcp_servers.gh.env]\nTOKEN = \"shh\"\n")

	eventsKindDir := filepath.Join(store.Root, "events", "tools")
	if _, err := os.Stat(eventsKindDir); err == nil {
		require.NoError(t, os.RemoveAll(eventsKindDir))
	}
	require.NoError(t, os.WriteFile(eventsKindDir, []byte("not a dir"), 0o644))

	a := &Adapter{HomeDir: tmp, DeviceID: "dev", SecretsStore: ss}
	_, err := a.ImportTool(context.Background(), store, src)
	require.Error(t, err)

	tools, err := store.ListArtifacts(acf.KindTool)
	require.NoError(t, err)
	require.Empty(t, tools, "failed codex tool import must roll back the artifact write")

	entries, err := os.ReadDir(ss.Root)
	require.NoError(t, err)
	for _, e := range entries {
		require.False(t, e.IsDir(),
			"failed codex tool import must roll back its secrets — found dir %s", e.Name())
	}
}

// TestImportTool_Codex_ReImportUnchanged_NoNewEvent: re-importing an identical
// config.toml (same redacted payload AND same secret) must reuse the id and
// append no event. A secret rotation (the test below) still appends.
func TestImportTool_Codex_ReImportUnchanged_NoNewEvent(t *testing.T) {
	tmp := t.TempDir()
	store := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, store.Init())
	ss := &secrets.Store{Root: filepath.Join(tmp, "secrets")}
	require.NoError(t, ss.Init())

	src := filepath.Join(tmp, "config.toml")
	writeFile(t, src, "[mcp_servers.gh]\ncommand = \"uvx\"\n[mcp_servers.gh.env]\nTOKEN = \"v1\"\n")

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

func TestImportTool_Codex_ReImportSameFile_ReusesArtifactID(t *testing.T) {
	tmp := t.TempDir()
	store := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, store.Init())
	ss := &secrets.Store{Root: filepath.Join(tmp, "secrets")}
	require.NoError(t, ss.Init())

	src := filepath.Join(tmp, "config.toml")
	writeFile(t, src, "[mcp_servers.gh]\ncommand = \"uvx\"\n[mcp_servers.gh.env]\nTOKEN = \"v1\"\n")

	a := &Adapter{HomeDir: tmp, DeviceID: "dev", SecretsStore: ss}
	ids1, err := a.ImportTool(context.Background(), store, src)
	require.NoError(t, err)
	require.Len(t, ids1, 1)

	writeFile(t, src, "[mcp_servers.gh]\ncommand = \"uvx\"\n[mcp_servers.gh.env]\nTOKEN = \"v2\"\n")
	ids2, err := a.ImportTool(context.Background(), store, src)
	require.NoError(t, err)
	require.Len(t, ids2, 1)

	require.Equal(t, ids1[0], ids2[0], "re-importing the same config.toml must reuse the artifact ID")

	events, err := store.ReadEvents(acf.KindTool, ids1[0])
	require.NoError(t, err)
	require.Len(t, events, 2)
	require.Equal(t, acf.EventTypeCreate, events[0].Type)
	require.Equal(t, acf.EventTypeUpdate, events[1].Type)
	require.NoError(t, acf.VerifyChain(events))

	got, err := ss.Get(ids1[0], "gh.TOKEN")
	require.NoError(t, err)
	require.Equal(t, "v2", got, "re-import must overwrite secrets with latest values")

	payload, err := acf.DecodeToolPayload(events[1])
	require.NoError(t, err)
	require.NotContains(t, payload.Content, "v2")
	require.NotContains(t, payload.Content, "v1")
}
