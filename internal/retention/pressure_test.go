package retention

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/stretchr/testify/require"
)

// TestCheckPressure_UnderWatermark verifies the typical happy-path: a
// store smaller than the configured watermark reports exceeded=false and
// the correct byte total.
func TestCheckPressure_UnderWatermark(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "small.txt"), []byte("hi"), 0o644))

	size, exceeded, err := CheckPressure(root, 1024)
	require.NoError(t, err)
	require.False(t, exceeded, "2-byte store must not exceed 1024 byte watermark")
	require.Equal(t, int64(2), size.Bytes)
}

// TestCheckPressure_OverWatermark verifies the trigger fires when the
// store crosses the watermark. Uses a 2 KiB file vs a 1 KiB watermark.
func TestCheckPressure_OverWatermark(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "big.bin"), make([]byte, 2048), 0o644))

	size, exceeded, err := CheckPressure(root, 1024)
	require.NoError(t, err)
	require.True(t, exceeded, "2 KiB store must exceed 1 KiB watermark")
	require.GreaterOrEqual(t, size.Bytes, int64(2048))
}

// TestCheckPressure_ClassifiesStoreAreas verifies the honest split: one
// walk classifies every byte into event-log / compacted / blob / other, the
// areas sum to the total, and only the .compacted layer counts as reclaimable.
func TestCheckPressure_ClassifiesStoreAreas(t *testing.T) {
	root := t.TempDir()
	write := func(rel string, n int) {
		path := filepath.Join(root, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, make([]byte, n), 0o644))
	}
	write("events/conversations/a.jsonl", 1000)
	write("events/memories/b.jsonl", 100)
	write("events/.compacted/conversations/a.jsonl.gz", 300)
	write("blobs/ab/abcdef0123", 500)
	write("acf/conversations/a.json", 150)
	write("version", 50)

	size, exceeded, err := CheckPressure(root, 1024)
	require.NoError(t, err)
	require.True(t, exceeded)
	require.Equal(t, int64(1100), size.EventLogBytes, "active .jsonl logs across kinds")
	require.Equal(t, int64(300), size.CompactedBytes)
	require.Equal(t, int64(500), size.BlobBytes)
	require.Equal(t, int64(200), size.OtherBytes, "acf metadata + version file")
	require.Equal(t, int64(2100), size.Bytes)
	require.Equal(t, size.Bytes, size.EventLogBytes+size.CompactedBytes+size.BlobBytes+size.OtherBytes,
		"classified areas must sum to the total")
	require.Equal(t, int64(300), size.ReclaimableBytes(), "only .compacted segments are reclaimable")
	require.Equal(t, int64(1800), size.PinnedBytes())
}

// TestForceSnapshotsAll seeds three memory artifacts and verifies that
// ForceSnapshotsAll snapshots all of them in one pass. The post-condition
// is that every artifact's log ends with an EventTypeSnapshot event.
func TestForceSnapshotsAll(t *testing.T) {
	root := t.TempDir()
	store := &acf.Store{Root: root}
	require.NoError(t, store.Init())

	ids := make([]string, 0, 3)
	for i := 0; i < 3; i++ {
		id := acf.NewID()
		now := time.Now().UTC()
		require.NoError(t, store.WriteArtifact(acf.Artifact{
			AcfSchemaVersion: acf.SchemaVersion,
			ArtifactID:       id,
			Kind:             acf.KindMemory,
			Name:             "x",
			CreatedAt:        now,
			UpdatedAt:        now,
		}))
		p, err := acf.EncodePayload(acf.MemoryPayload{Format: "markdown", Content: "v"})
		require.NoError(t, err)
		require.NoError(t, store.AppendEvent(acf.KindMemory, acf.Event{
			EventID:    acf.NewID(),
			ArtifactID: id,
			Type:       acf.EventTypeCreate,
			Timestamp:  now,
			Payload:    p,
		}))
		ids = append(ids, id)
	}

	n, err := ForceSnapshotsAll(context.Background(), store)
	require.NoError(t, err)
	require.Equal(t, 3, n, "all 3 artifacts should be snapshotted")

	// Confirm each artifact's tail is now a snapshot event.
	for _, id := range ids {
		events, err := store.ReadEvents(acf.KindMemory, id)
		require.NoError(t, err)
		require.GreaterOrEqual(t, len(events), 2, "create + snapshot")
		require.Equal(t, acf.EventType(acf.EventTypeSnapshot), events[len(events)-1].Type)
	}
}

// TestForceSnapshotsAll_SkipsAlreadySnapshotted is the redundant-snapshot
// regression guard: ForceSnapshotsAll must NOT append a second snapshot to an
// artifact whose head event is already an EventTypeSnapshot (mirrors the
// time-loop guard at TickTimeBasedSnapshots). A repeat pass over an idle,
// already-snapshotted store is a no-op — it returns 0 and grows no log.
func TestForceSnapshotsAll_SkipsAlreadySnapshotted(t *testing.T) {
	root := t.TempDir()
	store := &acf.Store{Root: root}
	require.NoError(t, store.Init())

	id := acf.NewID()
	now := time.Now().UTC()
	require.NoError(t, store.WriteArtifact(acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion,
		ArtifactID:       id,
		Kind:             acf.KindMemory,
		Name:             "x",
		CreatedAt:        now,
		UpdatedAt:        now,
	}))
	p, err := acf.EncodePayload(acf.MemoryPayload{Format: "markdown", Content: "v"})
	require.NoError(t, err)
	require.NoError(t, store.AppendEvent(acf.KindMemory, acf.Event{
		EventID:    acf.NewID(),
		ArtifactID: id,
		Type:       acf.EventTypeCreate,
		Timestamp:  now,
		Payload:    p,
	}))

	// First pass snapshots the artifact.
	n1, err := ForceSnapshotsAll(context.Background(), store)
	require.NoError(t, err)
	require.Equal(t, 1, n1, "first pass snapshots the create-only artifact")

	eventsAfter1, err := store.ReadEvents(acf.KindMemory, id)
	require.NoError(t, err)
	require.Len(t, eventsAfter1, 2, "create + snapshot")

	// Second pass: head is already a snapshot — must be a no-op.
	n2, err := ForceSnapshotsAll(context.Background(), store)
	require.NoError(t, err)
	require.Zero(t, n2, "already-snapshotted artifact must not be re-snapshotted")

	eventsAfter2, err := store.ReadEvents(acf.KindMemory, id)
	require.NoError(t, err)
	require.Len(t, eventsAfter2, 2, "no redundant snapshot appended on the second pass")
}
