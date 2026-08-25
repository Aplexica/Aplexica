package retention

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/blobstore"
	"github.com/stretchr/testify/require"
)

// newConvStore stands up a v2 store + a blob store rooted at its blobs dir.
func newConvStore(t *testing.T) (*acf.Store, *blobstore.Store) {
	t.Helper()
	store := &acf.Store{Root: t.TempDir()}
	require.NoError(t, store.Init())
	return store, &blobstore.Store{Root: store.BlobsDir()}
}

func writeConvArtifact(t *testing.T, store *acf.Store, id string, created time.Time) {
	t.Helper()
	require.NoError(t, store.WriteArtifact(acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion,
		ArtifactID:       id,
		Kind:             acf.KindConversation,
		Name:             "c",
		CreatedAt:        created,
		UpdatedAt:        created,
	}))
}

func attachmentPayload(t *testing.T, atts ...acf.Attachment) json.RawMessage {
	t.Helper()
	p, err := acf.EncodePayload(acf.ConversationPayload{
		Format:      acf.ConversationFormatV1,
		Attachments: atts,
	})
	require.NoError(t, err)
	return p
}

// TestEvictAttachments_PreservesChainIntegrity is the eviction centerpiece: an
// old attachment-bearing event is evicted and the hash chain MUST still
// verify afterward (the fix), with a proper evicted marker and no bytes on
// the wire, while a recent attachment is left untouched.
func TestEvictAttachments_PreservesChainIntegrity(t *testing.T) {
	store, blobs := newConvStore(t)
	id := acf.NewID()
	now := time.Now().UTC()
	writeConvArtifact(t, store, id, now.Add(-50*24*time.Hour))

	// Old blob (~40d) and a recent blob (~1d), both content-addressed.
	oldRaw := []byte("old-attachment-bytes")
	oldHash, err := blobs.Put(oldRaw)
	require.NoError(t, err)
	recentRaw := []byte("recent-attachment-bytes")
	recentHash, err := blobs.Put(recentRaw)
	require.NoError(t, err)

	oldAtt := acf.Attachment{
		Kind: "image", MimeType: "image/png",
		ContentHash: oldHash, Bytes: int64(len(oldRaw)),
	}
	recentAtt := acf.Attachment{
		Kind: "image", MimeType: "image/png",
		ContentHash: recentHash, Bytes: int64(len(recentRaw)),
	}

	// Genesis create event, 40 days old.
	require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
		EventID:    acf.NewID(),
		ArtifactID: id,
		Type:       acf.EventTypeCreate,
		Timestamp:  now.Add(-40 * 24 * time.Hour),
		Payload:    attachmentPayload(t, oldAtt),
	}))
	head, err := store.HeadHash(acf.KindConversation, id)
	require.NoError(t, err)
	require.NotEmpty(t, head)

	// Recent update event (1 day old), chained.
	require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
		EventID:    acf.NewID(),
		ArtifactID: id,
		Type:       acf.EventTypeUpdate,
		Timestamp:  now.Add(-1 * 24 * time.Hour),
		Payload:    attachmentPayload(t, recentAtt),
		ParentHash: head,
	}))

	// PRE-eviction: the chain verifies.
	pre, err := store.ReadEvents(acf.KindConversation, id)
	require.NoError(t, err)
	require.NoError(t, acf.VerifyChain(pre), "chain must verify before eviction")

	// Evict everything older than 30 days -> only the genesis event.
	n, err := EvictAttachments(context.Background(), store, blobs, acf.KindConversation, id, EvictOpts{
		MinAge: 30 * 24 * time.Hour,
		Reason: "age",
	})
	require.NoError(t, err)
	require.Equal(t, 1, n, "exactly one attachment evicted")

	post, err := store.ReadEvents(acf.KindConversation, id)
	require.NoError(t, err)

	// (a) Chain still verifies post-eviction.
	require.NoError(t, acf.VerifyChain(post), "chain must STILL verify after eviction")

	// Eviction is append-only: a new event was added, originals untouched.
	require.Len(t, post, 3, "an eviction event was appended, none removed")
	require.Equal(t, pre[0], post[0], "genesis event byte-for-byte unchanged")
	require.Equal(t, pre[1], post[1], "recent event byte-for-byte unchanged")

	// (b) The newest event re-asserts the old slot, now evicted, no bytes.
	var evictedPayload acf.ConversationPayload
	require.NoError(t, json.Unmarshal(post[2].Payload, &evictedPayload))
	require.Len(t, evictedPayload.Attachments, 1)
	ev := evictedPayload.Attachments[0]
	require.True(t, ev.IsEvicted(), "attachment must be evicted")
	require.NotNil(t, ev.Evicted)
	require.Equal(t, "age", ev.Evicted.Reason)
	require.Equal(t, int64(len(oldRaw)), ev.Evicted.OriginalSize)
	require.Equal(t, oldHash, ev.Evicted.ContentHash)
	require.NotContains(t, string(post[2].Payload), "\"data\"", "no Data on the wire")

	// (c) The recent attachment is untouched.
	var recentPayload acf.ConversationPayload
	require.NoError(t, json.Unmarshal(post[1].Payload, &recentPayload))
	require.Len(t, recentPayload.Attachments, 1)
	require.False(t, recentPayload.Attachments[0].IsEvicted(), "recent attachment untouched")
	require.Equal(t, recentHash, recentPayload.Attachments[0].ContentHash)
}

// TestEvictAttachments_PinExempt mirrors PruneArtifact's pin exemption:
// a "pinned" or "keep-forever" artifact is never evicted.
func TestEvictAttachments_PinExempt(t *testing.T) {
	store, blobs := newConvStore(t)
	id := acf.NewID()
	now := time.Now().UTC()
	require.NoError(t, store.WriteArtifact(acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion,
		ArtifactID:       id,
		Kind:             acf.KindConversation,
		Name:             "c",
		CreatedAt:        now.Add(-50 * 24 * time.Hour),
		UpdatedAt:        now,
		Tags:             []string{"pinned"},
	}))
	h, err := blobs.Put([]byte("pinned-blob"))
	require.NoError(t, err)
	require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
		EventID: acf.NewID(), ArtifactID: id, Type: acf.EventTypeCreate,
		Timestamp: now.Add(-40 * 24 * time.Hour),
		Payload:   attachmentPayload(t, acf.Attachment{Kind: "file", ContentHash: h, Bytes: 11}),
	}))

	n, err := EvictAttachments(context.Background(), store, blobs, acf.KindConversation, id, EvictOpts{
		MinAge: 30 * 24 * time.Hour, Reason: "age",
	})
	require.NoError(t, err)
	require.Equal(t, 0, n, "pinned artifact is exempt")
	events, err := store.ReadEvents(acf.KindConversation, id)
	require.NoError(t, err)
	require.Len(t, events, 1, "no eviction event appended for a pinned artifact")
}

// TestEvictAttachments_CheckpointCarriedAttachment_Evictable: after an
// on-snapshot prune the active log can be checkpoint-only (the snapshot — or,
// on an adopting device, a baseline — is the only payload-bearing event), so
// the checkpoint payload is the only place the attachment is asserted.
// Age-based eviction must reach those attachments; otherwise, since the
// live-set GC (correctly) protects checkpoint-referenced blobs, the bytes
// become permanently unreclaimable.
func TestEvictAttachments_CheckpointCarriedAttachment_Evictable(t *testing.T) {
	now := time.Now().UTC()

	appendCheckpoint := map[string]func(t *testing.T, store *acf.Store, id string, payload json.RawMessage){
		"snapshot": func(t *testing.T, store *acf.Store, id string, payload json.RawMessage) {
			t.Helper()
			require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
				EventID:       acf.NewID(),
				ArtifactID:    id,
				Type:          acf.EventTypeSnapshot,
				Timestamp:     now.Add(-40 * 24 * time.Hour),
				SnapshotState: "sha256:test",
				Payload:       payload,
			}))
		},
		"baseline": func(t *testing.T, store *acf.Store, id string, payload json.RawMessage) {
			t.Helper()
			require.NoError(t, store.AdoptBaseline(acf.KindConversation, acf.Event{
				EventID:        acf.NewID(),
				ArtifactID:     id,
				Type:           acf.EventTypeBaseline,
				Timestamp:      now.Add(-40 * 24 * time.Hour),
				Payload:        payload,
				AlignedHead:    "origin-head-hash",
				AlignedEventID: "origin-event-id",
			}))
		},
	}

	for name, appendFn := range appendCheckpoint {
		t.Run(name, func(t *testing.T) {
			store, blobs := newConvStore(t)
			raw := []byte("checkpoint-carried attachment bytes: " + name)
			hash, err := blobs.Put(raw)
			require.NoError(t, err)

			id := acf.NewID()
			writeConvArtifact(t, store, id, now.Add(-50*24*time.Hour))
			appendFn(t, store, id, attachmentPayload(t, acf.Attachment{
				Kind: "file", ContentHash: hash, Bytes: int64(len(raw)),
			}))

			ctx := context.Background()
			n, err := EvictAttachments(ctx, store, blobs, acf.KindConversation, id, EvictOpts{
				MinAge: 30 * 24 * time.Hour,
				Reason: "age",
			})
			require.NoError(t, err)
			require.Equal(t, 1, n, "the checkpoint-carried attachment must be evictable")

			// The appended eviction event re-asserts the slot evicted.
			events, err := store.ReadEvents(acf.KindConversation, id)
			require.NoError(t, err)
			last := events[len(events)-1]
			require.Equal(t, acf.EventType(acf.EventTypeUpdate), last.Type)
			var p acf.ConversationPayload
			require.NoError(t, json.Unmarshal(last.Payload, &p))
			require.Len(t, p.Attachments, 1)
			require.True(t, p.Attachments[0].IsEvicted())
			require.Equal(t, "age", p.Attachments[0].Evicted.Reason)

			// And the blob is now collectible once past grace.
			old := now.Add(-2 * blobGCGrace)
			require.NoError(t, os.Chtimes(blobs.Path(hash), old, old))
			deleted, err := GCBlobs(ctx, store, blobs)
			require.NoError(t, err)
			require.Equal(t, 1, deleted, "evicted checkpoint-carried blob is reclaimable")
			require.False(t, blobs.Has(hash))
		})
	}
}

// TestEvictAttachments_FreshCheckpointRespectsMinAge: a checkpoint YOUNGER
// than MinAge is not an eviction source — the min-age gate applies to
// checkpoint events exactly as to create/update/resolution.
func TestEvictAttachments_FreshCheckpointRespectsMinAge(t *testing.T) {
	store, blobs := newConvStore(t)
	now := time.Now().UTC()
	raw := []byte("fresh checkpoint attachment")
	hash, err := blobs.Put(raw)
	require.NoError(t, err)

	id := acf.NewID()
	writeConvArtifact(t, store, id, now.Add(-50*24*time.Hour))
	require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
		EventID:       acf.NewID(),
		ArtifactID:    id,
		Type:          acf.EventTypeSnapshot,
		Timestamp:     now.Add(-1 * 24 * time.Hour),
		SnapshotState: "sha256:test",
		Payload: attachmentPayload(t, acf.Attachment{
			Kind: "file", ContentHash: hash, Bytes: int64(len(raw)),
		}),
	}))

	n, err := EvictAttachments(context.Background(), store, blobs, acf.KindConversation, id, EvictOpts{
		MinAge: 30 * 24 * time.Hour,
		Reason: "age",
	})
	require.NoError(t, err)
	require.Equal(t, 0, n, "a fresh checkpoint's attachments are protected by MinAge")
}

// TestEvictAttachments_SecondPassAppendsNothing: eviction planning must key
// off the CURRENT (latest-wins) slot state, not just the immutable source
// payloads. The source event keeps naming the slot non-evicted forever
// (append-only log), so a planner that only reads source payloads re-appends
// a duplicate eviction event on EVERY periodic sweep, growing the event log
// without bound.
func TestEvictAttachments_SecondPassAppendsNothing(t *testing.T) {
	store, blobs := newConvStore(t)
	now := time.Now().UTC()
	raw := []byte("evict-once blob")
	hash, err := blobs.Put(raw)
	require.NoError(t, err)

	id := acf.NewID()
	writeConvArtifact(t, store, id, now.Add(-50*24*time.Hour))
	require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
		EventID: acf.NewID(), ArtifactID: id, Type: acf.EventTypeCreate,
		Timestamp: now.Add(-40 * 24 * time.Hour),
		Payload:   attachmentPayload(t, acf.Attachment{Kind: "file", ContentHash: hash, Bytes: int64(len(raw))}),
	}))

	ctx := context.Background()
	opts := EvictOpts{MinAge: 30 * 24 * time.Hour, Reason: "age"}
	n1, err := EvictAttachments(ctx, store, blobs, acf.KindConversation, id, opts)
	require.NoError(t, err)
	require.Equal(t, 1, n1, "first pass evicts the attachment")

	n2, err := EvictAttachments(ctx, store, blobs, acf.KindConversation, id, opts)
	require.NoError(t, err)
	require.Equal(t, 0, n2, "second pass has nothing left to evict")

	events, err := store.ReadEvents(acf.KindConversation, id)
	require.NoError(t, err)
	require.Len(t, events, 2, "create + exactly ONE eviction event — a repeat pass must not append duplicates")
}

// TestEvictAttachments_RecentReAssertionProtectsSlot: a slot re-asserted
// non-evicted by a RECENT event is protected by MinAge even though an OLD
// source event also names it. Evicting via the old source would mark the slot
// evicted at the head — latest-wins — and let GC collect a blob the recent
// event still references non-evicted.
func TestEvictAttachments_RecentReAssertionProtectsSlot(t *testing.T) {
	store, blobs := newConvStore(t)
	now := time.Now().UTC()
	raw := []byte("recently re-asserted blob")
	hash, err := blobs.Put(raw)
	require.NoError(t, err)

	id := acf.NewID()
	writeConvArtifact(t, store, id, now.Add(-50*24*time.Hour))
	att := acf.Attachment{Kind: "file", ContentHash: hash, Bytes: int64(len(raw))}
	require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
		EventID: acf.NewID(), ArtifactID: id, Type: acf.EventTypeCreate,
		Timestamp: now.Add(-40 * 24 * time.Hour),
		Payload:   attachmentPayload(t, att),
	}))
	head, err := store.HeadHash(acf.KindConversation, id)
	require.NoError(t, err)
	require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
		EventID: acf.NewID(), ArtifactID: id, Type: acf.EventTypeUpdate,
		Timestamp:  now.Add(-1 * 24 * time.Hour),
		Payload:    attachmentPayload(t, att),
		ParentHash: head,
	}))

	ctx := context.Background()
	n, err := EvictAttachments(ctx, store, blobs, acf.KindConversation, id, EvictOpts{
		MinAge: 30 * 24 * time.Hour, Reason: "age",
	})
	require.NoError(t, err)
	require.Equal(t, 0, n, "the slot's newest assertion is recent — MinAge protects it")

	live, err := LiveBlobSet(ctx, store)
	require.NoError(t, err)
	require.True(t, live[hash], "the recently re-asserted blob stays live")
}

// TestEvictAttachments_ReAssertDoesNotResurrectEvictedSlot: when an eviction
// event IS appended for one slot, other slots in the same re-asserted source
// payload must not be resurrected. Here slot B was already evicted by a newer
// event; the old source payload still names it non-evicted. The appended
// eviction event (for slot A) must re-stamp B's evicted marker — re-asserting
// B non-evicted at the head would flip it live under latest-wins while its
// blob may already be GC'd.
func TestEvictAttachments_ReAssertDoesNotResurrectEvictedSlot(t *testing.T) {
	store, blobs := newConvStore(t)
	now := time.Now().UTC()
	rawA := []byte("slot A blob")
	hashA, err := blobs.Put(rawA)
	require.NoError(t, err)
	rawB := []byte("slot B blob")
	hashB, err := blobs.Put(rawB)
	require.NoError(t, err)

	id := acf.NewID()
	writeConvArtifact(t, store, id, now.Add(-50*24*time.Hour))
	attA := acf.Attachment{Kind: "file", ContentHash: hashA, Bytes: int64(len(rawA))}
	attB := acf.Attachment{Kind: "file", ContentHash: hashB, Bytes: int64(len(rawB))}
	require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
		EventID: acf.NewID(), ArtifactID: id, Type: acf.EventTypeCreate,
		Timestamp: now.Add(-40 * 24 * time.Hour),
		Payload:   attachmentPayload(t, attA, attB),
	}))
	// A newer event asserts B evicted (e.g. a manual eviction or one synced
	// in from another device).
	head, err := store.HeadHash(acf.KindConversation, id)
	require.NoError(t, err)
	attBEvicted := attB
	attBEvicted.Evicted = &acf.EvictedInfo{
		At: now.Add(-2 * 24 * time.Hour), Reason: "manual",
		OriginalSize: attB.Bytes, ContentHash: hashB,
	}
	require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
		EventID: acf.NewID(), ArtifactID: id, Type: acf.EventTypeUpdate,
		Timestamp:  now.Add(-2 * 24 * time.Hour),
		Payload:    attachmentPayload(t, attBEvicted),
		ParentHash: head,
	}))

	ctx := context.Background()
	n, err := EvictAttachments(ctx, store, blobs, acf.KindConversation, id, EvictOpts{
		MinAge: 30 * 24 * time.Hour, Reason: "age",
	})
	require.NoError(t, err)
	require.Equal(t, 1, n, "only slot A is newly evicted; B is already evicted")

	events, err := store.ReadEvents(acf.KindConversation, id)
	require.NoError(t, err)
	var p acf.ConversationPayload
	require.NoError(t, json.Unmarshal(events[len(events)-1].Payload, &p))
	require.Len(t, p.Attachments, 2)
	for _, got := range p.Attachments {
		switch got.ContentHash {
		case hashA:
			require.True(t, got.IsEvicted(), "slot A newly evicted")
			require.Equal(t, "age", got.Evicted.Reason)
		case hashB:
			require.True(t, got.IsEvicted(), "slot B must stay evicted in the re-asserted payload")
			require.Equal(t, "manual", got.Evicted.Reason, "B keeps its original marker")
		default:
			t.Fatalf("unexpected attachment %q", got.ContentHash)
		}
	}

	live, err := LiveBlobSet(ctx, store)
	require.NoError(t, err)
	require.False(t, live[hashB], "B must not be resurrected by the re-assert")
	require.False(t, live[hashA], "A is evicted")
}

// TestGCBlobs_SharedBlobAndGrace exercises the S4 live-set GC:
//   - two events share one blob -> evicting one keeps the blob;
//   - evicting both (so no live event references it) past grace deletes it;
//   - a fresh orphan within grace is kept.
func TestGCBlobs_SharedBlobAndGrace(t *testing.T) {
	store, blobs := newConvStore(t)
	now := time.Now().UTC()

	sharedRaw := []byte("shared-blob")
	sharedHash, err := blobs.Put(sharedRaw)
	require.NoError(t, err)

	// Two distinct old conversations both reference the shared blob.
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

	// A fresh orphan blob, never referenced, just written (within grace).
	freshHash, err := blobs.Put([]byte("fresh-orphan"))
	require.NoError(t, err)

	ctx := context.Background()

	// Evict A only -> B still references the shared blob -> it stays live.
	_, err = EvictAttachments(ctx, store, blobs, acf.KindConversation, idA, EvictOpts{MinAge: 30 * 24 * time.Hour, Reason: "age"})
	require.NoError(t, err)
	deleted, err := GCBlobs(ctx, store, blobs)
	require.NoError(t, err)
	require.Equal(t, 0, deleted, "shared blob still referenced by B; fresh orphan within grace")
	require.True(t, blobs.Has(sharedHash), "shared blob kept while B references it")
	require.True(t, blobs.Has(freshHash), "fresh orphan kept (within grace)")

	// Evict B too -> no live reference remains. Backdate the blob mtime so
	// it is past the grace window, then GC must collect it.
	_, err = EvictAttachments(ctx, store, blobs, acf.KindConversation, idB, EvictOpts{MinAge: 30 * 24 * time.Hour, Reason: "age"})
	require.NoError(t, err)

	old := now.Add(-2 * blobGCGrace)
	require.NoError(t, os.Chtimes(blobs.Path(sharedHash), old, old))

	deleted, err = GCBlobs(ctx, store, blobs)
	require.NoError(t, err)
	require.Equal(t, 1, deleted, "shared blob collected once unreferenced and past grace")
	require.False(t, blobs.Has(sharedHash), "shared blob deleted")
	require.True(t, blobs.Has(freshHash), "fresh orphan still within grace, kept")
}

// TestGCBlobs_SharedBlob_LiveIfAnyArtifactNonEvicted is the cross-artifact
// regression: liveness of a shared (deduped) blob must be OR'd across artifacts,
// not decided by whichever artifact ListArtifacts iterates last. The LATER-
// created conversation (iterated last) evicts the blob while the EARLIER one
// still references it non-evicted; with the blob mtime backdated past the grace
// window (so grace can't mask the bug), GC MUST keep it. The previous global
// latest-wins resolution failed this (the later eviction overwrote the earlier
// live reference), silently deleting a blob a live event still names.
func TestGCBlobs_SharedBlob_LiveIfAnyArtifactNonEvicted(t *testing.T) {
	store, blobs := newConvStore(t)
	now := time.Now().UTC()

	sharedRaw := []byte("shared-across-convs")
	sharedHash, err := blobs.Put(sharedRaw)
	require.NoError(t, err)

	mkConv := func(created time.Time) string {
		id := acf.NewID()
		writeConvArtifact(t, store, id, created)
		require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
			EventID: acf.NewID(), ArtifactID: id, Type: acf.EventTypeCreate,
			Timestamp: created,
			Payload:   attachmentPayload(t, acf.Attachment{Kind: "file", ContentHash: sharedHash, Bytes: int64(len(sharedRaw))}),
		}))
		return id
	}
	// Earlier conv keeps the blob; later conv (iterated last by CreatedAt asc) evicts it.
	mkConv(now.Add(-50 * 24 * time.Hour))
	later := mkConv(now.Add(-10 * 24 * time.Hour))

	ctx := context.Background()
	_, err = EvictAttachments(ctx, store, blobs, acf.KindConversation, later, EvictOpts{MinAge: 24 * time.Hour, Reason: "age"})
	require.NoError(t, err)

	// Defeat the grace mask so liveness is the only thing keeping the blob.
	old := now.Add(-2 * blobGCGrace)
	require.NoError(t, os.Chtimes(blobs.Path(sharedHash), old, old))

	live, err := LiveBlobSet(ctx, store)
	require.NoError(t, err)
	require.True(t, live[sharedHash],
		"a blob still referenced non-evicted by the earlier conv must be live even though the later conv evicted it")

	deleted, err := GCBlobs(ctx, store, blobs)
	require.NoError(t, err)
	require.Equal(t, 0, deleted, "GC must not delete a blob an earlier conv still references non-evicted")
	require.True(t, blobs.Has(sharedHash), "shared blob kept")
}
