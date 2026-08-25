package retention

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/stretchr/testify/require"
)

// TestGCBlobs_SnapshotOnlyReference_BlobStaysLive: a blob whose ONLY
// reference lives in a payload-bearing snapshot checkpoint (FR-02.32) must be
// counted live by LiveBlobSet and survive GCBlobs. This is the post-prune
// shape: the pre-snapshot create/update events were compacted and the
// compacted segment already grace-deleted, so the snapshot is the only event
// naming the ContentHash. Collecting the blob would leave the checkpoint's
// non-evicted attachment dangling — a corrupted checkpoint.
func TestGCBlobs_SnapshotOnlyReference_BlobStaysLive(t *testing.T) {
	store, blobs := newConvStore(t)
	now := time.Now().UTC()

	raw := []byte("blob referenced only by a snapshot checkpoint")
	hash, err := blobs.Put(raw)
	require.NoError(t, err)

	id := acf.NewID()
	writeConvArtifact(t, store, id, now.Add(-50*24*time.Hour))
	require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
		EventID:       acf.NewID(),
		ArtifactID:    id,
		Type:          acf.EventTypeSnapshot,
		Timestamp:     now.Add(-40 * 24 * time.Hour),
		SnapshotState: "sha256:test",
		Payload: attachmentPayload(t, acf.Attachment{
			Kind: "file", ContentHash: hash, Bytes: int64(len(raw)),
		}),
	}))
	// A legacy payload-LESS snapshot on the same log asserts nothing and
	// must neither crash the scan nor affect liveness.
	head, err := store.HeadHash(acf.KindConversation, id)
	require.NoError(t, err)
	require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
		EventID:       acf.NewID(),
		ArtifactID:    id,
		Type:          acf.EventTypeSnapshot,
		Timestamp:     now.Add(-39 * 24 * time.Hour),
		SnapshotState: "sha256:legacy",
		ParentHash:    head,
	}))

	// Defeat the grace mask so liveness is the only thing keeping the blob.
	old := now.Add(-2 * blobGCGrace)
	require.NoError(t, os.Chtimes(blobs.Path(hash), old, old))

	ctx := context.Background()
	live, err := LiveBlobSet(ctx, store)
	require.NoError(t, err)
	require.True(t, live[hash],
		"a blob referenced only by a payload-bearing snapshot checkpoint must be live")

	deleted, err := GCBlobs(ctx, store, blobs)
	require.NoError(t, err)
	require.Equal(t, 0, deleted, "GC must not delete a checkpoint-referenced blob")
	require.True(t, blobs.Has(hash), "snapshot-referenced blob kept")
}

// TestGCBlobs_BaselineOnlyReference_BlobStaysLive is the aligned-chains
// flavor: on an adopting device the baseline event carries the full
// materialized origin state — including its attachment list — and the origin
// history never existed locally, so the baseline is the ONLY event naming the
// ContentHash. GC must keep the blob.
func TestGCBlobs_BaselineOnlyReference_BlobStaysLive(t *testing.T) {
	store, blobs := newConvStore(t)
	now := time.Now().UTC()

	raw := []byte("blob referenced only by an adopted baseline")
	hash, err := blobs.Put(raw)
	require.NoError(t, err)

	id := acf.NewID()
	writeConvArtifact(t, store, id, now.Add(-50*24*time.Hour))
	require.NoError(t, store.AdoptBaseline(acf.KindConversation, acf.Event{
		EventID:    acf.NewID(),
		ArtifactID: id,
		Type:       acf.EventTypeBaseline,
		Timestamp:  now.Add(-40 * 24 * time.Hour),
		Payload: attachmentPayload(t, acf.Attachment{
			Kind: "file", ContentHash: hash, Bytes: int64(len(raw)),
		}),
		AlignedHead:    "origin-head-hash",
		AlignedEventID: "origin-event-id",
	}))

	old := now.Add(-2 * blobGCGrace)
	require.NoError(t, os.Chtimes(blobs.Path(hash), old, old))

	ctx := context.Background()
	live, err := LiveBlobSet(ctx, store)
	require.NoError(t, err)
	require.True(t, live[hash],
		"a blob referenced only by a payload-bearing baseline must be live")

	deleted, err := GCBlobs(ctx, store, blobs)
	require.NoError(t, err)
	require.Equal(t, 0, deleted, "GC must not delete a baseline-referenced blob")
	require.True(t, blobs.Has(hash), "baseline-referenced blob kept")
}

// TestGCBlobs_CheckpointThenEviction_LatestWins guards the other direction:
// counting checkpoint payloads must NOT over-protect. A later eviction append
// re-asserts the slot evicted, and that newer assertion wins over the
// snapshot's non-evicted one, so the blob is collectible.
func TestGCBlobs_CheckpointThenEviction_LatestWins(t *testing.T) {
	store, blobs := newConvStore(t)
	now := time.Now().UTC()

	raw := []byte("snapshot-then-evicted blob")
	hash, err := blobs.Put(raw)
	require.NoError(t, err)

	id := acf.NewID()
	writeConvArtifact(t, store, id, now.Add(-50*24*time.Hour))
	require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
		EventID:       acf.NewID(),
		ArtifactID:    id,
		Type:          acf.EventTypeSnapshot,
		Timestamp:     now.Add(-40 * 24 * time.Hour),
		SnapshotState: "sha256:test",
		Payload: attachmentPayload(t, acf.Attachment{
			Kind: "file", ContentHash: hash, Bytes: int64(len(raw)),
		}),
	}))
	head, err := store.HeadHash(acf.KindConversation, id)
	require.NoError(t, err)
	require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
		EventID:    acf.NewID(),
		ArtifactID: id,
		Type:       acf.EventTypeUpdate,
		Timestamp:  now.Add(-1 * 24 * time.Hour),
		ParentHash: head,
		Payload: attachmentPayload(t, acf.Attachment{
			Kind: "file", ContentHash: hash, Bytes: int64(len(raw)),
			Evicted: &acf.EvictedInfo{
				At: now, Reason: "age", OriginalSize: int64(len(raw)), ContentHash: hash,
			},
		}),
	}))

	old := now.Add(-2 * blobGCGrace)
	require.NoError(t, os.Chtimes(blobs.Path(hash), old, old))

	ctx := context.Background()
	live, err := LiveBlobSet(ctx, store)
	require.NoError(t, err)
	require.False(t, live[hash],
		"an eviction appended after the snapshot wins latest-wins; the blob is not live")

	deleted, err := GCBlobs(ctx, store, blobs)
	require.NoError(t, err)
	require.Equal(t, 1, deleted, "evicted-after-checkpoint blob is collectible")
	require.False(t, blobs.Has(hash))
}
