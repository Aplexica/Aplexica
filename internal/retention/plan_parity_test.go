package retention

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/stretchr/testify/require"
)

// TestPrunePlanApplyParity asserts PlanPruneArtifact returns EXACTLY the
// mutation set that PruneArtifact then applies — same moved events, same
// compacted-delete decision — across the four retention-relevant fixtures:
// a normal artifact, a pinned (skipped) artifact, a forked (branch-ancestor
// protected) artifact, and one whose compacted file is grace-deletable.
func TestPrunePlanApplyParity(t *testing.T) {
	ctx := context.Background()

	t.Run("normal artifact", func(t *testing.T) {
		store := &acf.Store{Root: t.TempDir()}
		require.NoError(t, store.Init())
		id := acf.NewID()
		now := time.Now().UTC()
		require.NoError(t, store.WriteArtifact(acf.Artifact{
			AcfSchemaVersion: acf.SchemaVersion, ArtifactID: id, Kind: acf.KindMemory,
			Name: "t", CreatedAt: now, UpdatedAt: now,
		}))
		buildChainedArtifact(t, store, id, now, 4)
		_, err := CreateSnapshot(ctx, store, acf.KindMemory, id)
		require.NoError(t, err)

		grace := now.Add(-1 * time.Hour) // freshly-written file is newer -> no delete
		plan, err := PlanPruneArtifact(ctx, store, acf.KindMemory, id, grace)
		require.NoError(t, err)
		require.Len(t, plan.ToMove, 4)
		require.False(t, plan.DeleteCompacted)

		planMoved := hashSet(plan.ToMove)
		moved, deleted, err := PruneArtifact(ctx, store, acf.KindMemory, id, grace)
		require.NoError(t, err)
		require.Equal(t, len(plan.ToMove), moved, "apply moves exactly the plan's count")
		require.Equal(t, 0, deleted)
		require.Equal(t, planMoved, movedHashSet(t, store, id),
			"the events apply actually moved equal the plan's ToMove set")
	})

	t.Run("pinned artifact is skipped", func(t *testing.T) {
		store := &acf.Store{Root: t.TempDir()}
		require.NoError(t, store.Init())
		id := acf.NewID()
		now := time.Now().UTC()
		require.NoError(t, store.WriteArtifact(acf.Artifact{
			AcfSchemaVersion: acf.SchemaVersion, ArtifactID: id, Kind: acf.KindMemory,
			Name: "t", CreatedAt: now, UpdatedAt: now, Tags: []string{"pinned"},
		}))
		buildChainedArtifact(t, store, id, now, 3)
		_, err := CreateSnapshot(ctx, store, acf.KindMemory, id)
		require.NoError(t, err)

		plan, err := PlanPruneArtifact(ctx, store, acf.KindMemory, id, now)
		require.NoError(t, err)
		require.Empty(t, plan.ToMove, "pinned -> empty plan")
		require.False(t, plan.DeleteCompacted)

		moved, deleted, err := PruneArtifact(ctx, store, acf.KindMemory, id, now)
		require.NoError(t, err)
		require.Equal(t, 0, moved)
		require.Equal(t, 0, deleted)
		require.Empty(t, movedHashSet(t, store, id))
	})

	t.Run("forked artifact protects branch ancestors", func(t *testing.T) {
		store := &acf.Store{Root: t.TempDir()}
		require.NoError(t, store.Init())
		id := acf.NewID()
		now := time.Now().UTC()
		require.NoError(t, store.WriteArtifact(acf.Artifact{
			AcfSchemaVersion: acf.SchemaVersion, ArtifactID: id, Kind: acf.KindMemory,
			Name: "t", CreatedAt: now, UpdatedAt: now,
		}))
		heads := buildChainedArtifact(t, store, id, now, 5)
		e1Hash := heads[1]
		require.NoError(t, store.AppendEvent(acf.KindMemory, acf.Event{
			EventID: acf.NewID(), ArtifactID: id, Type: acf.EventTypeForkOuter,
			Timestamp: now.Add(10 * time.Second), ParentHash: e1Hash,
			Branch: "alt", ForkSourceBranch: acf.MainBranch, ForkOriginAgent: "codex",
		}))
		forkHash, err := store.HeadHashByBranch(acf.KindMemory, id, "alt")
		require.NoError(t, err)
		altPayload, _ := acf.EncodePayload(acf.MemoryPayload{Format: "markdown", Content: "alt-1"})
		require.NoError(t, store.AppendEvent(acf.KindMemory, acf.Event{
			EventID: acf.NewID(), ArtifactID: id, Type: acf.EventTypeUpdate,
			Timestamp: now.Add(11 * time.Second), Branch: "alt",
			ParentHash: forkHash, Payload: altPayload,
		}))
		_, err = CreateSnapshot(ctx, store, acf.KindMemory, id)
		require.NoError(t, err)

		grace := now.Add(-1 * time.Hour)
		plan, err := PlanPruneArtifact(ctx, store, acf.KindMemory, id, grace)
		require.NoError(t, err)
		planMoved := hashSet(plan.ToMove)
		// Branch-ancestor protection: the fork-point ancestor e1 and e0 are
		// NOT in the plan.
		require.False(t, planMoved[e1Hash], "fork-point ancestor must not be planned for move")
		require.False(t, planMoved[heads[0]], "e0 (ancestor of e1) must not be planned for move")

		moved, _, err := PruneArtifact(ctx, store, acf.KindMemory, id, grace)
		require.NoError(t, err)
		require.Equal(t, len(plan.ToMove), moved)
		require.Equal(t, planMoved, movedHashSet(t, store, id),
			"plan ToMove == events apply moved, with branch protection honored")
	})

	t.Run("grace-deletable compacted file", func(t *testing.T) {
		store := &acf.Store{Root: t.TempDir()}
		require.NoError(t, store.Init())
		id := acf.NewID()
		now := time.Now().UTC()
		require.NoError(t, store.WriteArtifact(acf.Artifact{
			AcfSchemaVersion: acf.SchemaVersion, ArtifactID: id, Kind: acf.KindMemory,
			Name: "t", CreatedAt: now, UpdatedAt: now,
		}))
		buildChainedArtifact(t, store, id, now, 3)
		_, err := CreateSnapshot(ctx, store, acf.KindMemory, id)
		require.NoError(t, err)

		// A graceDeadline in the FUTURE makes the freshly-written compacted
		// file older than the deadline -> apply grace-deletes it. The plan
		// must predict the same.
		grace := time.Now().Add(1 * time.Hour)
		plan, err := PlanPruneArtifact(ctx, store, acf.KindMemory, id, grace)
		require.NoError(t, err)
		require.NotEmpty(t, plan.ToMove)
		require.True(t, plan.DeleteCompacted, "plan predicts a grace-delete")

		moved, deleted, err := PruneArtifact(ctx, store, acf.KindMemory, id, grace)
		require.NoError(t, err)
		require.Equal(t, len(plan.ToMove), moved)
		require.Equal(t, 1, deleted, "apply grace-deletes exactly as the plan predicted")
	})
}

func hashSet(events []acf.Event) map[string]bool {
	m := map[string]bool{}
	for _, e := range events {
		m[e.Hash] = true
	}
	return m
}

// TestEvictPlanApplyParity asserts the eviction plan equals what
// EvictAttachments appends: same attachments marked, same bytesReclaimable,
// including pin-skip and the min-age boundary.
func TestEvictPlanApplyParity(t *testing.T) {
	ctx := context.Background()

	t.Run("plan set equals applied evictions", func(t *testing.T) {
		store, blobs := newConvStore(t)
		id := acf.NewID()
		now := time.Now().UTC()
		writeConvArtifact(t, store, id, now.Add(-50*24*time.Hour))

		oldRaw := []byte("old-attachment-bytes")
		oldHash, err := blobs.Put(oldRaw)
		require.NoError(t, err)
		recentRaw := []byte("recent-attachment-bytes")
		recentHash, err := blobs.Put(recentRaw)
		require.NoError(t, err)

		require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
			EventID: acf.NewID(), ArtifactID: id, Type: acf.EventTypeCreate,
			Timestamp: now.Add(-40 * 24 * time.Hour),
			Payload:   attachmentPayload(t, acf.Attachment{Kind: "image", ContentHash: oldHash, Bytes: int64(len(oldRaw))}),
		}))
		head, err := store.HeadHash(acf.KindConversation, id)
		require.NoError(t, err)
		require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
			EventID: acf.NewID(), ArtifactID: id, Type: acf.EventTypeUpdate,
			Timestamp: now.Add(-1 * 24 * time.Hour), ParentHash: head,
			Payload: attachmentPayload(t, acf.Attachment{Kind: "image", ContentHash: recentHash, Bytes: int64(len(recentRaw))}),
		}))

		opts := EvictOpts{MinAge: 30 * 24 * time.Hour, Reason: "age"}
		plan, err := PlanEvictAttachments(ctx, store, acf.KindConversation, id, opts)
		require.NoError(t, err)
		require.Equal(t, 1, plan.AttachmentsEvicted, "only the old attachment is past min-age")
		require.Equal(t, int64(len(oldRaw)), plan.BytesReclaimable)
		require.Len(t, plan.Events, 1)

		n, err := EvictAttachments(ctx, store, blobs, acf.KindConversation, id, opts)
		require.NoError(t, err)
		require.Equal(t, plan.AttachmentsEvicted, n,
			"apply evicts exactly the plan's attachment count")
	})

	t.Run("pin-skip", func(t *testing.T) {
		store, blobs := newConvStore(t)
		id := acf.NewID()
		now := time.Now().UTC()
		require.NoError(t, store.WriteArtifact(acf.Artifact{
			AcfSchemaVersion: acf.SchemaVersion, ArtifactID: id, Kind: acf.KindConversation,
			Name: "c", CreatedAt: now.Add(-50 * 24 * time.Hour), UpdatedAt: now,
			Tags: []string{"pinned"},
		}))
		h, err := blobs.Put([]byte("pinned-blob"))
		require.NoError(t, err)
		require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
			EventID: acf.NewID(), ArtifactID: id, Type: acf.EventTypeCreate,
			Timestamp: now.Add(-40 * 24 * time.Hour),
			Payload:   attachmentPayload(t, acf.Attachment{Kind: "file", ContentHash: h, Bytes: 11}),
		}))

		opts := EvictOpts{MinAge: 30 * 24 * time.Hour, Reason: "age"}
		plan, err := PlanEvictAttachments(ctx, store, acf.KindConversation, id, opts)
		require.NoError(t, err)
		require.Empty(t, plan.Events, "pinned -> empty plan")
		require.Equal(t, 0, plan.AttachmentsEvicted)

		n, err := EvictAttachments(ctx, store, blobs, acf.KindConversation, id, opts)
		require.NoError(t, err)
		require.Equal(t, 0, n)
	})

	t.Run("min-age boundary keeps fresh attachment", func(t *testing.T) {
		store, blobs := newConvStore(t)
		id := acf.NewID()
		now := time.Now().UTC()
		writeConvArtifact(t, store, id, now.Add(-2*time.Hour))
		h, err := blobs.Put([]byte("fresh-blob"))
		require.NoError(t, err)
		// Event is only ~1h old; MinAge is 30 days -> not past cutoff.
		require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
			EventID: acf.NewID(), ArtifactID: id, Type: acf.EventTypeCreate,
			Timestamp: now.Add(-1 * time.Hour),
			Payload:   attachmentPayload(t, acf.Attachment{Kind: "file", ContentHash: h, Bytes: 10}),
		}))
		opts := EvictOpts{MinAge: 30 * 24 * time.Hour, Reason: "age"}
		plan, err := PlanEvictAttachments(ctx, store, acf.KindConversation, id, opts)
		require.NoError(t, err)
		require.Empty(t, plan.Events, "attachment newer than the min-age cutoff is kept")

		n, err := EvictAttachments(ctx, store, blobs, acf.KindConversation, id, opts)
		require.NoError(t, err)
		require.Equal(t, 0, n)
	})
}

// TestGCBlobsPlanApplyParity asserts the planned deletable-blob set equals
// what GCBlobs deletes, including the shared-blob live-if-any-non-evicted rule
// and the grace window.
func TestGCBlobsPlanApplyParity(t *testing.T) {
	store, blobs := newConvStore(t)
	now := time.Now().UTC()
	ctx := context.Background()

	sharedRaw := []byte("shared-blob")
	sharedHash, err := blobs.Put(sharedRaw)
	require.NoError(t, err)

	mkConv := func() string {
		id := acf.NewID()
		writeConvArtifact(t, store, id, now.Add(-50*24*time.Hour))
		require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
			EventID: acf.NewID(), ArtifactID: id, Type: acf.EventTypeCreate,
			Timestamp: now.Add(-40 * 24 * time.Hour),
			Payload:   attachmentPayload(t, acf.Attachment{Kind: "file", ContentHash: sharedHash, Bytes: int64(len(sharedRaw))}),
		}))
		return id
	}
	idA := mkConv()
	idB := mkConv()
	freshHash, err := blobs.Put([]byte("fresh-orphan"))
	require.NoError(t, err)

	// Evict only A: shared blob still live via B; fresh orphan within grace.
	_, err = EvictAttachments(ctx, store, blobs, acf.KindConversation, idA, EvictOpts{MinAge: 30 * 24 * time.Hour, Reason: "age"})
	require.NoError(t, err)
	plan, err := PlanGCBlobs(ctx, store, blobs)
	require.NoError(t, err)
	require.Empty(t, plan, "nothing collectible: shared blob live via B, fresh orphan within grace")

	// Evict B too, backdate the shared blob past grace.
	_, err = EvictAttachments(ctx, store, blobs, acf.KindConversation, idB, EvictOpts{MinAge: 30 * 24 * time.Hour, Reason: "age"})
	require.NoError(t, err)
	old := now.Add(-2 * blobGCGrace)
	require.NoError(t, os.Chtimes(blobs.Path(sharedHash), old, old))

	plan, err = PlanGCBlobs(ctx, store, blobs)
	require.NoError(t, err)
	require.Len(t, plan, 1, "only the shared blob is now collectible (fresh orphan still within grace)")
	require.Equal(t, sharedHash, plan[0].Hash)
	require.Equal(t, int64(len(sharedRaw)), plan[0].Bytes)

	planned := map[string]bool{}
	for _, e := range plan {
		planned[e.Hash] = true
	}

	deleted, err := GCBlobs(ctx, store, blobs)
	require.NoError(t, err)
	require.Equal(t, len(plan), deleted, "apply deletes exactly the planned blob count")
	require.False(t, blobs.Has(sharedHash), "planned blob deleted")
	require.True(t, blobs.Has(freshHash), "blob NOT in the plan is kept")
}
