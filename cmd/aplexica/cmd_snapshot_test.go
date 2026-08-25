package main

import (
	"bytes"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/stretchr/testify/require"
)

// runSnapshotCmd invokes `aplexica snapshot …` via rootCmd and returns the
// combined output. The --store flag is injected so the real store is never
// touched.
func runSnapshotCmd(t *testing.T, store string, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&out)
	full := append([]string{"snapshot", "--store", store}, args...)
	rootCmd.SetArgs(full)
	t.Cleanup(func() { snapshotStoreRoot = "" })
	err := rootCmd.Execute()
	return out.String(), err
}

// seedSnapshotStore builds a memory artifact WITHOUT a snapshot event, so a
// caller can either create one (positional form) or list whatever exists.
func seedSnapshotStore(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	store := &acf.Store{Root: root}
	require.NoError(t, store.Init())

	id := acf.NewID()
	now := time.Now().UTC()
	require.NoError(t, store.WriteArtifact(acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion, ArtifactID: id, Kind: acf.KindMemory,
		Name: "m", CreatedAt: now, UpdatedAt: now,
	}))
	p0, _ := acf.EncodePayload(acf.MemoryPayload{Format: "markdown", Content: "v0"})
	require.NoError(t, store.AppendEvent(acf.KindMemory, acf.Event{
		EventID: acf.NewID(), ArtifactID: id, Type: acf.EventTypeCreate,
		Timestamp: now, Payload: p0,
	}))
	for i := 1; i < 3; i++ {
		p, _ := acf.EncodePayload(acf.MemoryPayload{Format: "markdown", Content: "v"})
		head, _ := store.HeadHash(acf.KindMemory, id)
		require.NoError(t, store.AppendEvent(acf.KindMemory, acf.Event{
			EventID: acf.NewID(), ArtifactID: id, Type: acf.EventTypeUpdate,
			Timestamp: now.Add(time.Duration(i) * time.Second), Payload: p, ParentHash: head,
		}))
	}
	return root, id
}

// TestSnapshotCreate_PositionalStillWorks is the backward-compat guard: the
// bare `aplexica snapshot <id>` form MUST still create a snapshot event.
func TestSnapshotCreate_PositionalStillWorks(t *testing.T) {
	root, id := seedSnapshotStore(t)

	out, err := runSnapshotCmd(t, root, id)
	require.NoError(t, err)
	require.Contains(t, out, "created snapshot")

	store := &acf.Store{Root: root}
	require.NoError(t, store.Init())
	events, err := store.ReadEvents(acf.KindMemory, id)
	require.NoError(t, err)
	found := false
	for _, e := range events {
		if e.Type == acf.EventTypeSnapshot {
			found = true
		}
	}
	require.True(t, found, "positional snapshot must append a snapshot event")
}

// TestSnapshotList: `aplexica snapshot list <id>` prints the artifact's
// snapshot events.
func TestSnapshotList(t *testing.T) {
	root, id := seedSnapshotStore(t)

	// No snapshot yet → list reports none.
	out, err := runSnapshotCmd(t, root, "list", id)
	require.NoError(t, err)
	require.Contains(t, out, "no snapshot events")

	// Create one via the positional form, then list it.
	_, err = runSnapshotCmd(t, root, id)
	require.NoError(t, err)

	out, err = runSnapshotCmd(t, root, "list", id)
	require.NoError(t, err)
	require.Contains(t, out, "EVENT")   // header
	require.Contains(t, out, "sha256:") // snapshot state
}
