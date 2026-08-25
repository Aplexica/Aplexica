package main

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/retention"
	"github.com/stretchr/testify/require"
)

// runPinCmd invokes `aplexica pin|unpin …` via rootCmd and returns the
// combined output. The --store flag is injected so the real store is
// never touched.
func runPinCmd(t *testing.T, verb, store string, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&out)
	full := append([]string{verb, "--store", store}, args...)
	rootCmd.SetArgs(full)
	t.Cleanup(func() { pinStoreRoot = "" })
	err := rootCmd.Execute()
	return out.String(), err
}

// seedPinStore stands up a memory artifact whose pre-snapshot history would
// be compacted by PruneArtifact unless the artifact is pinned.
func seedPinStore(t *testing.T) (string, string) {
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
	for i := 1; i < 4; i++ {
		p, _ := acf.EncodePayload(acf.MemoryPayload{Format: "markdown", Content: "v"})
		head, _ := store.HeadHash(acf.KindMemory, id)
		require.NoError(t, store.AppendEvent(acf.KindMemory, acf.Event{
			EventID: acf.NewID(), ArtifactID: id, Type: acf.EventTypeUpdate,
			Timestamp: now.Add(time.Duration(i) * time.Second), Payload: p, ParentHash: head,
		}))
	}
	_, err := retention.CreateSnapshot(context.Background(), store, acf.KindMemory, id)
	require.NoError(t, err)
	return root, id
}

// TestPinUnpin: pin adds the pin tag (so PruneArtifact then skips the
// artifact); unpin removes it (so prune moves pre-snapshot events again).
// Both operations are idempotent.
func TestPinUnpin(t *testing.T) {
	root, id := seedPinStore(t)
	store := &acf.Store{Root: root}
	require.NoError(t, store.Init())

	// pin
	out, err := runPinCmd(t, "pin", root, id)
	require.NoError(t, err)
	require.Contains(t, out, "pinned")
	art, err := store.ReadArtifact(acf.KindMemory, id)
	require.NoError(t, err)
	require.Contains(t, art.Tags, "pinned")

	// idempotent pin (no duplicate tag)
	_, err = runPinCmd(t, "pin", root, id)
	require.NoError(t, err)
	art, err = store.ReadArtifact(acf.KindMemory, id)
	require.NoError(t, err)
	count := 0
	for _, tag := range art.Tags {
		if tag == "pinned" {
			count++
		}
	}
	require.Equal(t, 1, count, "pin must dedupe")

	// PruneArtifact must SKIP a pinned artifact.
	deadline := time.Now().Add(-time.Hour)
	moved, _, err := retention.PruneArtifact(context.Background(), store, acf.KindMemory, id, deadline)
	require.NoError(t, err)
	require.Equal(t, 0, moved, "pinned artifact must be exempt from prune")

	// unpin
	_, err = runPinCmd(t, "unpin", root, id)
	require.NoError(t, err)
	art, err = store.ReadArtifact(acf.KindMemory, id)
	require.NoError(t, err)
	require.NotContains(t, art.Tags, "pinned")

	// idempotent unpin (no error on already-absent)
	_, err = runPinCmd(t, "unpin", root, id)
	require.NoError(t, err)

	// Now prune moves the pre-snapshot events.
	moved, _, err = retention.PruneArtifact(context.Background(), store, acf.KindMemory, id, deadline)
	require.NoError(t, err)
	require.Greater(t, moved, 0, "unpinned artifact must be prunable again")
}
