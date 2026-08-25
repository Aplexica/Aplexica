package adapter

import (
	"fmt"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/stretchr/testify/require"
)

// fakeSecretStore is a map-backed SecretsReader for exercising SecretsUnchanged
// in isolation — including the key-added/removed branches that the real tool
// import can't reach (changing the secret SET also changes the redacted
// payload, so ToolImportUnchanged short-circuits on the payload check first).
type fakeSecretStore struct {
	m        map[string]map[string]string // artifactID -> name -> value
	deleted  []string
	unlinked []string
}

func (f fakeSecretStore) ListForArtifact(id string) ([]string, error) {
	var ks []string
	for k := range f.m[id] {
		ks = append(ks, k)
	}
	return ks, nil
}

func (f fakeSecretStore) Get(id, name string) (string, error) {
	v, ok := f.m[id][name]
	if !ok {
		return "", fmt.Errorf("secret %s/%s not found", id, name)
	}
	return v, nil
}

func (f *fakeSecretStore) Delete(id, name string) error {
	delete(f.m[id], name)
	f.deleted = append(f.deleted, name)
	return nil
}

func (f *fakeSecretStore) UnlinkToolSecret(name, artifactID string) error {
	f.unlinked = append(f.unlinked, name)
	return nil
}

func TestSecretsUnchanged(t *testing.T) {
	store := fakeSecretStore{m: map[string]map[string]string{
		"A":     {"x": "1", "y": "2"},
		"empty": {},
	}}

	cases := []struct {
		name string
		id   string
		want map[string]string
		exp  bool
	}{
		{"identical set", "A", map[string]string{"x": "1", "y": "2"}, true},
		{"changed value (rotation)", "A", map[string]string{"x": "1", "y": "ROTATED"}, false},
		{"added key", "A", map[string]string{"x": "1", "y": "2", "z": "3"}, false},
		{"removed key", "A", map[string]string{"x": "1"}, false},
		{"same count, different key", "A", map[string]string{"x": "1", "z": "2"}, false},
		{"no secrets either side", "empty", map[string]string{}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := SecretsUnchanged(store, tc.id, tc.want)
			require.NoError(t, err)
			require.Equal(t, tc.exp, got)
		})
	}
}

func TestToolImportUnchanged_PrunesStaleExtraSecrets(t *testing.T) {
	store := &acf.Store{Root: t.TempDir()}
	require.NoError(t, store.Init())
	id := "tool-1"
	payload, err := acf.EncodePayload(acf.ToolPayload{Format: "acf.mcp.v1", Content: `{"servers":{}}`})
	require.NoError(t, err)
	now := time.Now().UTC()
	require.NoError(t, store.WriteArtifact(acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion,
		ArtifactID:       id,
		Kind:             acf.KindTool,
		Scope:            acf.ScopeGlobal,
		Name:             "config.toml",
		CreatedAt:        now,
		UpdatedAt:        now,
	}))
	require.NoError(t, store.AppendEvent(acf.KindTool, acf.Event{
		EventID:    "event-1",
		ArtifactID: id,
		Type:       acf.EventTypeUpdate,
		Timestamp:  now,
		Payload:    payload,
	}))

	sec := &fakeSecretStore{m: map[string]map[string]string{
		id: {"node_repl.CODEX_HOME": "/tmp/codex"},
	}}
	unchanged, err := ToolImportUnchanged(store, sec, id, payload, map[string]string{})
	require.NoError(t, err)
	require.True(t, unchanged, "stale extra secrets are cleanup work, not a new tool event")
	require.Empty(t, sec.m[id])
	require.Equal(t, []string{"node_repl.CODEX_HOME"}, sec.deleted)
	require.Equal(t, []string{"node_repl.CODEX_HOME"}, sec.unlinked)
}

func TestToolImportUnchanged_SecretRotationStillChanges(t *testing.T) {
	store := &acf.Store{Root: t.TempDir()}
	require.NoError(t, store.Init())
	id := "tool-1"
	payload, err := acf.EncodePayload(acf.ToolPayload{Format: "acf.mcp.v1", Content: `{"servers":{"github":{"env":{"TOKEN":"${secret:github.TOKEN}"}}}}`})
	require.NoError(t, err)
	now := time.Now().UTC()
	require.NoError(t, store.WriteArtifact(acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion,
		ArtifactID:       id,
		Kind:             acf.KindTool,
		Scope:            acf.ScopeGlobal,
		Name:             "config.toml",
		CreatedAt:        now,
		UpdatedAt:        now,
	}))
	require.NoError(t, store.AppendEvent(acf.KindTool, acf.Event{
		EventID:    "event-1",
		ArtifactID: id,
		Type:       acf.EventTypeUpdate,
		Timestamp:  now,
		Payload:    payload,
	}))

	sec := &fakeSecretStore{m: map[string]map[string]string{
		id: {"github.TOKEN": "old"},
	}}
	unchanged, err := ToolImportUnchanged(store, sec, id, payload, map[string]string{"github.TOKEN": "new"})
	require.NoError(t, err)
	require.False(t, unchanged)
	require.Empty(t, sec.deleted, "secret rotations must append an event, not prune the changed key")
	require.Empty(t, sec.unlinked)
}
