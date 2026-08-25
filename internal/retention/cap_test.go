package retention

import (
	"context"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/stretchr/testify/require"
)

// snapshotCount returns how many snapshot events are reachable in the active
// log of an artifact.
func snapshotCount(t *testing.T, store *acf.Store, kind acf.Kind, id string) int {
	t.Helper()
	events, err := store.ReadEvents(kind, id)
	require.NoError(t, err)
	n := 0
	for _, e := range events {
		if e.Type == acf.EventTypeSnapshot {
			n++
		}
	}
	return n
}

// seedSnapshotChain appends `snaps` snapshots to a fresh memory artifact, each
// preceded by one update event (so every snapshot has distinct pre-snapshot
// history). It returns the ordered list of snapshot event hashes (oldest
// first). The artifact starts with a single create event before the first
// update/snapshot pair.
func seedSnapshotChain(t *testing.T, store *acf.Store, id string, now time.Time, snaps int) []string {
	t.Helper()
	p0, _ := acf.EncodePayload(acf.MemoryPayload{Format: "markdown", Content: "v0"})
	require.NoError(t, store.AppendEvent(acf.KindMemory, acf.Event{
		EventID: acf.NewID(), ArtifactID: id, Type: acf.EventTypeCreate,
		Timestamp: now, Payload: p0,
	}))
	snapHashes := make([]string, 0, snaps)
	for i := 0; i < snaps; i++ {
		p, _ := acf.EncodePayload(acf.MemoryPayload{Format: "markdown", Content: "v"})
		head, herr := store.HeadHash(acf.KindMemory, id)
		require.NoError(t, herr)
		require.NoError(t, store.AppendEvent(acf.KindMemory, acf.Event{
			EventID: acf.NewID(), ArtifactID: id, Type: acf.EventTypeUpdate,
			Timestamp: now.Add(time.Duration(2*i+1) * time.Second), Payload: p,
			ParentHash: head,
		}))
		_, err := CreateSnapshot(context.Background(), store, acf.KindMemory, id)
		require.NoError(t, err)
		// CreateSnapshot returns the event it built before the store computes
		// its content hash, so read the freshly-appended snapshot's hash back
		// from the head (the snapshot is the current tip).
		snapHash, herr := store.HeadHash(acf.KindMemory, id)
		require.NoError(t, herr)
		require.NotEmpty(t, snapHash)
		snapHashes = append(snapHashes, snapHash)
	}
	return snapHashes
}

// TestEnforceSnapshotCap_KeepsLastN is the FR-03.25 keystone: with more than N
// snapshots accumulated, the cap compacts pre-anchor history so that only the
// most-recent N snapshots remain reachable in the active log, anchoring at the
// Nth-most-recent snapshot. VerifyChain must stay green across the boundary.
func TestEnforceSnapshotCap_KeepsLastN(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store := &acf.Store{Root: root}
	require.NoError(t, store.Init())

	id := acf.NewID()
	now := time.Now().UTC()
	require.NoError(t, store.WriteArtifact(acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion, ArtifactID: id, Kind: acf.KindMemory,
		Name: "t", CreatedAt: now, UpdatedAt: now,
	}))

	// 5 snapshots, each with one preceding update event.
	const totalSnaps = 5
	const keepN = 2
	snapHashes := seedSnapshotChain(t, store, id, now, totalSnaps)
	require.Equal(t, totalSnaps, snapshotCount(t, store, acf.KindMemory, id),
		"sanity: all 5 snapshots present before cap enforcement")

	// The Nth-most-recent snapshot (N=2) is snapHashes[totalSnaps-keepN] =
	// snapHashes[3] — the anchor. Everything strictly before it must compact;
	// it and everything after must stay.
	anchorHash := snapHashes[totalSnaps-keepN]

	moved, _, err := EnforceSnapshotCap(ctx, store, acf.KindMemory, id, keepN, now.Add(-1*time.Hour))
	require.NoError(t, err)
	require.Positive(t, moved, "older pre-anchor events must be moved to .compacted")

	// Exactly N snapshots remain reachable in the active log.
	require.Equal(t, keepN, snapshotCount(t, store, acf.KindMemory, id),
		"only the most-recent N snapshots remain after cap enforcement")

	// The active log's first event is the anchor snapshot.
	active, err := store.ReadEvents(acf.KindMemory, id)
	require.NoError(t, err)
	require.NotEmpty(t, active)
	require.Equal(t, anchorHash, active[0].Hash, "active log must begin at the Nth-most-recent snapshot anchor")

	// The two oldest snapshots were compacted out of the active log.
	movedSet := movedHashSet(t, store, id)
	require.True(t, movedSet[snapHashes[0]], "oldest snapshot must be compacted")
	require.True(t, movedSet[snapHashes[1]], "second-oldest snapshot must be compacted")
	require.False(t, movedSet[anchorHash], "anchor snapshot must remain in the active log")

	// VerifyChain still passes over the merged active+compacted log walked
	// from the head back to genesis.
	all, err := store.ReadEventsIncludingCompacted(acf.KindMemory, id)
	require.NoError(t, err)
	byHash := indexByHash(all)
	tip, err := store.HeadHashByBranch(acf.KindMemory, id, acf.MainBranch)
	require.NoError(t, err)
	require.NoError(t, acf.VerifyChain(chainTo(byHash, tip)),
		"chain must verify after snapshot-cap compaction")
}

// TestEnforceSnapshotCap_FloorTwo guards the floor: even when called with
// keepN below 2, enforcement behaves as if keepN == 2 (never compacts below
// two retained snapshots).
func TestEnforceSnapshotCap_FloorTwo(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store := &acf.Store{Root: root}
	require.NoError(t, store.Init())

	id := acf.NewID()
	now := time.Now().UTC()
	require.NoError(t, store.WriteArtifact(acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion, ArtifactID: id, Kind: acf.KindMemory,
		Name: "t", CreatedAt: now, UpdatedAt: now,
	}))

	const totalSnaps = 4
	seedSnapshotChain(t, store, id, now, totalSnaps)

	// keepN=1 is below the floor; enforcement must keep 2 snapshots, not 1.
	_, _, err := EnforceSnapshotCap(ctx, store, acf.KindMemory, id, 1, now.Add(-1*time.Hour))
	require.NoError(t, err)
	require.Equal(t, 2, snapshotCount(t, store, acf.KindMemory, id),
		"floor N=2: enforcement never compacts below two retained snapshots")
}

// TestEnforceSnapshotCap_UnderCapIsNoOp: with N or fewer snapshots there is no
// Nth-most-recent anchor to compact behind, so nothing moves.
func TestEnforceSnapshotCap_UnderCapIsNoOp(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store := &acf.Store{Root: root}
	require.NoError(t, store.Init())

	id := acf.NewID()
	now := time.Now().UTC()
	require.NoError(t, store.WriteArtifact(acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion, ArtifactID: id, Kind: acf.KindMemory,
		Name: "t", CreatedAt: now, UpdatedAt: now,
	}))

	// Exactly 3 snapshots, keepN=3 -> no compaction.
	seedSnapshotChain(t, store, id, now, 3)
	moved, _, err := EnforceSnapshotCap(ctx, store, acf.KindMemory, id, 3, now.Add(-1*time.Hour))
	require.NoError(t, err)
	require.Zero(t, moved, "at or under the cap, nothing is compacted")
	require.Equal(t, 3, snapshotCount(t, store, acf.KindMemory, id))
}

// TestEnforceSnapshotCapAll_StoreWide: the store-wide pass compacts a
// multi-snapshot artifact down to the configured N and records the action;
// "all" is a strict no-op.
func TestEnforceSnapshotCapAll_StoreWide(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store := &acf.Store{Root: root}
	require.NoError(t, store.Init())

	id := acf.NewID()
	now := time.Now().UTC()
	require.NoError(t, store.WriteArtifact(acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion, ArtifactID: id, Kind: acf.KindMemory,
		Name: "t", CreatedAt: now, UpdatedAt: now,
	}))
	seedSnapshotChain(t, store, id, now, 5)

	// "all" sentinel: no-op.
	allCfg := DefaultConfig() // KeepLastNSnapshots == keepAll
	var noopReport GCReport
	require.NoError(t, EnforceSnapshotCapAll(ctx, store, allCfg, now.Add(-1*time.Hour), &noopReport))
	require.Empty(t, noopReport.Actions, `"all" keeps every snapshot; nothing compacted`)
	require.Equal(t, 5, snapshotCount(t, store, acf.KindMemory, id))

	// Numeric cap N=2: compacts to 2 snapshots and records the prune action.
	capCfg := DefaultConfig()
	capCfg.KeepLastNSnapshots = 2
	var report GCReport
	require.NoError(t, EnforceSnapshotCapAll(ctx, store, capCfg, now.Add(-1*time.Hour), &report))
	require.Equal(t, 2, snapshotCount(t, store, acf.KindMemory, id),
		"store-wide cap keeps the most-recent N snapshots")
	require.Positive(t, countActions(report, OpPruneEvents), "the cap compaction is reported")
}

// TestEnforceSnapshotCap_PinExempt: a pinned artifact is never compacted.
func TestEnforceSnapshotCap_PinExempt(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store := &acf.Store{Root: root}
	require.NoError(t, store.Init())

	id := acf.NewID()
	now := time.Now().UTC()
	require.NoError(t, store.WriteArtifact(acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion, ArtifactID: id, Kind: acf.KindMemory,
		Name: "t", CreatedAt: now, UpdatedAt: now,
		Tags: []string{"pinned"},
	}))
	seedSnapshotChain(t, store, id, now, 5)
	moved, _, err := EnforceSnapshotCap(ctx, store, acf.KindMemory, id, 2, now.Add(-1*time.Hour))
	require.NoError(t, err)
	require.Zero(t, moved, "pinned artifact is exempt from snapshot-cap compaction")
	require.Equal(t, 5, snapshotCount(t, store, acf.KindMemory, id))
}
