package claudecode

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/retention"
	"github.com/aplexica/aplexica/internal/secrets"
	"github.com/stretchr/testify/require"
)

func TestImportTool_WritesArtifactAndStoresSecrets(t *testing.T) {
	tmp := t.TempDir()
	store := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, store.Init())
	secretsStore := &secrets.Store{Root: filepath.Join(tmp, "secrets")}
	require.NoError(t, secretsStore.Init())

	src := filepath.Join(tmp, ".mcp.json")
	content := `{"mcpServers":{"github":{"type":"stdio","command":"uvx","env":{"TOKEN":"secret-value"}}}}`
	writeFile(t, src, content)

	a := &Adapter{HomeDir: tmp, DeviceID: "dev", SecretsStore: secretsStore}
	ids, err := a.ImportTool(context.Background(), store, src)
	require.NoError(t, err)
	require.Len(t, ids, 1)

	got, err := store.ReadArtifact(acf.KindTool, ids[0])
	require.NoError(t, err)
	require.Equal(t, acf.KindTool, got.Kind)
	require.Equal(t, ".mcp.json", got.Name)

	events, err := store.ReadEvents(acf.KindTool, ids[0])
	require.NoError(t, err)
	require.Len(t, events, 1)
	payload, err := acf.DecodeToolPayload(events[0])
	require.NoError(t, err)
	require.Equal(t, "acf.mcp.v1", payload.Format,
		"v0.3.0: tool Format is the canonical schema marker")
	require.NotContains(t, payload.Content, "secret-value",
		"canonical store MUST NOT contain raw secret values (ADR-0027)")
	require.Contains(t, payload.Content, "${secret:github.TOKEN}",
		"canonical store MUST contain the redaction reference")

	secVal, err := secretsStore.Get(ids[0], "github.TOKEN")
	require.NoError(t, err)
	require.Equal(t, "secret-value", secVal)
}

// The secret sidecar's usedByTools must list the importing tool artifact so
// `aplexica secret list` can report which tools reference a secret. Regression
// for the review finding "usedByTools sidecar field is never populated by the
// adapter import path".
func TestImportTool_RecordsUsedByTool(t *testing.T) {
	tmp := t.TempDir()
	store := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, store.Init())
	secretsStore := &secrets.Store{Root: filepath.Join(tmp, "secrets")}
	require.NoError(t, secretsStore.Init())

	src := filepath.Join(tmp, ".mcp.json")
	writeFile(t, src, `{"mcpServers":{"github":{"type":"stdio","command":"uvx","env":{"TOKEN":"secret-value"}}}}`)

	a := &Adapter{HomeDir: tmp, DeviceID: "dev", SecretsStore: secretsStore}
	ids, err := a.ImportTool(context.Background(), store, src)
	require.NoError(t, err)
	require.Len(t, ids, 1)

	meta, err := secretsStore.ReadMeta("github.TOKEN")
	require.NoError(t, err, "import must create the secret's sidecar")
	require.Equal(t, []string{ids[0]}, meta.UsedByTools,
		"the importing tool artifact must be recorded in usedByTools")
}

// FR-02.28: every event's createdBy.agentVersion must be populated — "unknown"
// when the source agent does not expose a version — never left empty.
func TestImportTool_SetsAgentVersionProvenance(t *testing.T) {
	tmp := t.TempDir()
	store := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, store.Init())
	secretsStore := &secrets.Store{Root: filepath.Join(tmp, "secrets")}
	require.NoError(t, secretsStore.Init())

	src := filepath.Join(tmp, ".mcp.json")
	writeFile(t, src, `{"mcpServers":{"github":{"type":"stdio","command":"uvx"}}}`)

	a := &Adapter{HomeDir: tmp, DeviceID: "dev", SecretsStore: secretsStore}
	ids, err := a.ImportTool(context.Background(), store, src)
	require.NoError(t, err)
	require.Len(t, ids, 1)

	events, err := store.ReadEvents(acf.KindTool, ids[0])
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.NotEmpty(t, events[0].Provenance.AgentVersion,
		"FR-02.28: agentVersion must never be empty")
	require.Equal(t, acf.UnknownAgentVersion, events[0].Provenance.AgentVersion)
}

func TestImportTool_NoEnv_NoSecrets(t *testing.T) {
	tmp := t.TempDir()
	store := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, store.Init())
	secretsStore := &secrets.Store{Root: filepath.Join(tmp, "secrets")}
	require.NoError(t, secretsStore.Init())

	src := filepath.Join(tmp, ".mcp.json")
	writeFile(t, src, `{"mcpServers":{"cf":{"type":"http","url":"https://x"}}}`)

	a := &Adapter{HomeDir: tmp, DeviceID: "dev", SecretsStore: secretsStore}
	ids, err := a.ImportTool(context.Background(), store, src)
	require.NoError(t, err)

	keys, err := secretsStore.ListForArtifact(ids[0])
	require.NoError(t, err)
	require.Empty(t, keys, "no env block = no secrets")
}

func TestExportTool_ReassemblesSecretsFromStore(t *testing.T) {
	tmp := t.TempDir()
	store := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, store.Init())
	secretsStore := &secrets.Store{Root: filepath.Join(tmp, "secrets")}
	require.NoError(t, secretsStore.Init())

	src := filepath.Join(tmp, ".mcp.json")
	original := `{"mcpServers":{"github":{"type":"stdio","env":{"TOKEN":"shh"}}}}`
	writeFile(t, src, original)

	a := &Adapter{HomeDir: tmp, DeviceID: "dev", SecretsStore: secretsStore}
	ids, err := a.ImportTool(context.Background(), store, src)
	require.NoError(t, err)

	dest := filepath.Join(tmp, "out", ".mcp.json")
	require.NoError(t, a.ExportTool(context.Background(), store, ids[0], dest))

	got, err := os.ReadFile(dest)
	require.NoError(t, err)

	// Tool round-trip is SEMANTIC equivalence, not byte-identical, because
	// JSON gets parsed and re-serialized.
	var inObj, outObj any
	require.NoError(t, json.Unmarshal([]byte(original), &inObj))
	require.NoError(t, json.Unmarshal(got, &outObj))
	require.Equal(t, inObj, outObj,
		"tool export must be semantically equivalent to import after secret expansion")
}

// TestExportTool_AfterSnapshotAndPrune covers the export-after-prune P1 for the
// TOOL kind: ExportTool replays through the shared adapter.ReplayToolPayload
// helper, which (like the opaque path) decodes a payload-bearing snapshot or
// falls back to the compacted layer. Before the fix the inline tool replay ran
// acf.VerifyChain on the snapshot-only active log and failed across the prune
// boundary with "event log is invalid". The secret is reassembled from the
// secrets store (which the prune does not touch), so the round-trip is whole.
func TestExportTool_AfterSnapshotAndPrune(t *testing.T) {
	tmp := t.TempDir()
	store := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, store.Init())
	secretsStore := &secrets.Store{Root: filepath.Join(tmp, "secrets")}
	require.NoError(t, secretsStore.Init())

	src := filepath.Join(tmp, ".mcp.json")
	original := `{"mcpServers":{"github":{"type":"stdio","env":{"TOKEN":"shh"}}}}`
	writeFile(t, src, original)

	a := &Adapter{HomeDir: tmp, DeviceID: "dev", SecretsStore: secretsStore}
	ids, err := a.ImportTool(context.Background(), store, src)
	require.NoError(t, err)
	require.Len(t, ids, 1)
	id := ids[0]

	// Snapshot then prune: the create event moves to .compacted and the active
	// log is snapshot-only.
	_, err = retention.CreateSnapshot(context.Background(), store, acf.KindTool, id)
	require.NoError(t, err)
	moved, _, err := retention.PruneArtifact(context.Background(), store, acf.KindTool, id, time.Now().Add(-time.Hour))
	require.NoError(t, err)
	require.Equal(t, 1, moved, "the single pre-snapshot create event must move to .compacted")

	active, err := store.ReadEvents(acf.KindTool, id)
	require.NoError(t, err)
	require.Len(t, active, 1, "active log is snapshot-only after prune")
	require.Equal(t, acf.EventType(acf.EventTypeSnapshot), active[0].Type)

	dest := filepath.Join(tmp, "out", ".mcp.json")
	require.NoError(t, a.ExportTool(context.Background(), store, id, dest),
		"tool export must still materialize after snapshot+prune (no 'event log is invalid')")

	got, err := os.ReadFile(dest)
	require.NoError(t, err)
	var inObj, outObj any
	require.NoError(t, json.Unmarshal([]byte(original), &inObj))
	require.NoError(t, json.Unmarshal(got, &outObj))
	require.Equal(t, inObj, outObj,
		"pruned tool export must be semantically equivalent to import after secret expansion")
}

func TestExportTool_MissingSecretIsError(t *testing.T) {
	tmp := t.TempDir()
	store := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, store.Init())
	secretsStore := &secrets.Store{Root: filepath.Join(tmp, "secrets")}
	require.NoError(t, secretsStore.Init())

	src := filepath.Join(tmp, ".mcp.json")
	writeFile(t, src, `{"mcpServers":{"x":{"env":{"K":"v"}}}}`)

	a := &Adapter{HomeDir: tmp, DeviceID: "dev", SecretsStore: secretsStore}
	ids, err := a.ImportTool(context.Background(), store, src)
	require.NoError(t, err)

	// Delete the secret directory to simulate the secrets store going missing.
	require.NoError(t, os.RemoveAll(filepath.Join(secretsStore.Root, ids[0])))

	err = a.ExportTool(context.Background(), store, ids[0], filepath.Join(tmp, "out.json"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "x.K")
}

func TestImportTool_RollsBackOrphanArtifactAndSecretsOnAppendFailure(t *testing.T) {
	tmp := t.TempDir()
	store := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, store.Init())
	ss := &secrets.Store{Root: filepath.Join(tmp, "secrets")}
	require.NoError(t, ss.Init())

	src := filepath.Join(tmp, ".mcp.json")
	writeFile(t, src, `{"mcpServers":{"gh":{"type":"stdio","env":{"TOKEN":"shh"}}}}`)

	// Corrupt the events kind dir so AppendEvent fails.
	eventsKindDir := filepath.Join(store.Root, "events", "tools")
	if _, err := os.Stat(eventsKindDir); err == nil {
		require.NoError(t, os.RemoveAll(eventsKindDir))
	}
	require.NoError(t, os.WriteFile(eventsKindDir, []byte("not a dir"), 0o644))

	a := &Adapter{HomeDir: tmp, DeviceID: "dev", SecretsStore: ss}
	_, err := a.ImportTool(context.Background(), store, src)
	require.Error(t, err, "AppendEvent should fail because the events tools dir is a file")

	tools, err := store.ListArtifacts(acf.KindTool)
	require.NoError(t, err)
	require.Empty(t, tools, "failed tool import must roll back the artifact write")

	entries, err := os.ReadDir(ss.Root)
	require.NoError(t, err)
	for _, e := range entries {
		require.False(t, e.IsDir(),
			"failed tool import must roll back the secrets it wrote — found dir %s", e.Name())
	}
}

func TestImportTool_DoesNotRollBackSecretsOnUpdateFailure(t *testing.T) {
	tmp := t.TempDir()
	store := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, store.Init())
	ss := &secrets.Store{Root: filepath.Join(tmp, "secrets")}
	require.NoError(t, ss.Init())

	src := filepath.Join(tmp, ".mcp.json")
	writeFile(t, src, `{"mcpServers":{"gh":{"type":"stdio","env":{"TOKEN":"v1"}}}}`)

	a := &Adapter{HomeDir: tmp, DeviceID: "dev", SecretsStore: ss}
	ids, err := a.ImportTool(context.Background(), store, src)
	require.NoError(t, err)
	require.Len(t, ids, 1)

	eventsKindDir := filepath.Join(store.Root, "events", "tools")
	existingEvents := filepath.Join(eventsKindDir, ids[0]+".jsonl")
	preservedEvents, rerr := os.ReadFile(existingEvents)
	require.NoError(t, rerr)
	require.NoError(t, os.RemoveAll(eventsKindDir))
	require.NoError(t, os.WriteFile(eventsKindDir, []byte("not a dir"), 0o644))

	writeFile(t, src, `{"mcpServers":{"gh":{"type":"stdio","env":{"TOKEN":"v2"}}}}`)
	_, err = a.ImportTool(context.Background(), store, src)
	require.Error(t, err)

	_, rerr = store.ReadArtifact(acf.KindTool, ids[0])
	require.NoError(t, rerr, "existing tool artifact must survive a failed re-import")

	keys, lerr := ss.ListForArtifact(ids[0])
	require.NoError(t, lerr)
	require.NotEmpty(t, keys, "secrets dir must survive a failed update — we did not write it in this call")

	// Best-effort restore so the test env exits clean.
	_ = os.Remove(eventsKindDir)
	_ = os.MkdirAll(eventsKindDir, 0o755)
	_ = os.WriteFile(existingEvents, preservedEvents, 0o644)
}

// TestImportTool_ReImportUnchanged_NoNewEvent guards the restart skip path:
// re-importing a byte-identical .mcp.json (same redacted payload AND same
// secret value) must NOT append a redundant event — it reuses the id, like the
// opaque path. Contrast with the rotation test below, where the secret changes
// and an event MUST still be appended.
func TestImportTool_ReImportUnchanged_NoNewEvent(t *testing.T) {
	tmp := t.TempDir()
	store := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, store.Init())
	ss := &secrets.Store{Root: filepath.Join(tmp, "secrets")}
	require.NoError(t, ss.Init())

	src := filepath.Join(tmp, ".mcp.json")
	writeFile(t, src, `{"mcpServers":{"gh":{"type":"stdio","env":{"TOKEN":"v1"}}}}`)

	a := &Adapter{HomeDir: tmp, DeviceID: "dev", SecretsStore: ss}
	ids1, err := a.ImportTool(context.Background(), store, src)
	require.NoError(t, err)
	require.Len(t, ids1, 1)

	// Re-import the SAME bytes.
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

	src := filepath.Join(tmp, ".mcp.json")
	writeFile(t, src, `{"mcpServers":{"gh":{"type":"stdio","env":{"TOKEN":"v1"}}}}`)

	a := &Adapter{HomeDir: tmp, DeviceID: "dev", SecretsStore: ss}
	ids1, err := a.ImportTool(context.Background(), store, src)
	require.NoError(t, err)
	require.Len(t, ids1, 1)

	writeFile(t, src, `{"mcpServers":{"gh":{"type":"stdio","env":{"TOKEN":"v2"}}}}`)
	ids2, err := a.ImportTool(context.Background(), store, src)
	require.NoError(t, err)
	require.Len(t, ids2, 1)

	require.Equal(t, ids1[0], ids2[0], "re-importing the same .mcp.json must reuse the artifact ID")

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
	require.NotContains(t, payload.Content, "v2",
		"re-import canonical payload must NOT contain raw secret values")
	require.NotContains(t, payload.Content, "v1",
		"re-import canonical payload must NOT contain prior raw secret values either")
}
