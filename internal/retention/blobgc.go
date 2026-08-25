package retention

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/blobstore"
)

// blobGCGrace is how long an unreferenced blob is protected from GC after
// it was written. The window guards a blob that was Put between the
// live-set scan and the GC pass (e.g. a fresh attachment racing a
// concurrent eviction sweep): collecting it immediately could drop a blob
// a not-yet-scanned event references. One hour is generous for a
// single-process daemon while still bounding orphan lifetime.
const blobGCGrace = time.Hour

// LiveBlobSet scans EVERY conversation artifact's events — active AND
// compacted (Store.ReadEventsIncludingCompacted) — and returns the set of
// ContentHashes that are still live.
//
// A blob is live when, in ANY artifact that references it, that artifact's
// MOST RECENT assertion (the latest create/update/resolution or
// payload-bearing checkpoint event — snapshot/baseline, see isCheckpointEvent
// — in append order) of the ContentHash is NOT evicted. Liveness is resolved PER
// ARTIFACT and then OR'd across artifacts: because blobs are content-addressed
// and deduped, a single blob can be shared by several conversations, and it
// must survive as long as even one of them still references it non-evicted —
// independent of artifact iteration order. (Resolving globally would let
// whichever artifact is scanned LAST decide a shared blob's fate and silently
// drop a blob a live event in another artifact still names.)
//
// Within one artifact this is what makes the append-only eviction model
// GC-correct: an old create event references the blob non-evicted, but a later
// appended eviction event re-asserts the same slot with the Evicted marker set
// — that newer assertion wins, so the blob drops out of that artifact's live
// set even though the original create still names it. A blob re-introduced
// non-evicted by a still-newer event stays live.
//
// The returned set is keyed by hex content hash (the blobstore filename),
// suitable to pass straight to blobstore.Store.GC.
func LiveBlobSet(ctx context.Context, store *acf.Store) (map[string]bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	arts, err := store.ListArtifacts(acf.KindConversation)
	if err != nil {
		return nil, fmt.Errorf("retention: list conversations: %w", err)
	}
	live := map[string]bool{}
	for _, art := range arts {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		events, eerr := store.ReadEventsIncludingCompacted(acf.KindConversation, art.ArtifactID)
		if eerr != nil {
			return nil, fmt.Errorf("retention: read events (incl compacted) %s: %w", art.ArtifactID, eerr)
		}
		// Resolve latest-wins WITHIN this artifact: the last event (append
		// order) that asserts a ContentHash decides whether THIS artifact still
		// references it. true => evicted, false => non-evicted.
		perArtifact := map[string]bool{}
		for _, e := range events {
			// Checkpoint payloads (snapshot/baseline) count too: after an
			// on-snapshot prune whose compacted segment was grace-deleted, or
			// a baseline adoption, the checkpoint can be the ONLY event
			// naming a ContentHash — skipping it would let GC collect a blob
			// the checkpoint still references, corrupting the checkpoint.
			if !assertsAttachmentSlots(e) {
				continue
			}
			var p acf.ConversationPayload
			if jerr := json.Unmarshal(e.Payload, &p); jerr != nil {
				continue
			}
			for _, att := range p.Attachments {
				if att.ContentHash == "" {
					continue
				}
				perArtifact[att.ContentHash] = att.IsEvicted()
			}
		}
		// OR across artifacts: live if THIS artifact's latest assertion is
		// non-evicted. A shared blob another artifact already evicted stays
		// live as long as this one still references it.
		for hash, evicted := range perArtifact {
			if !evicted {
				live[hash] = true
			}
		}
	}
	return live, nil
}

// PlanGCBlobs builds the live ContentHash set across all conversation events
// (active + compacted) and returns — WITHOUT deleting — the set of blobs
// GCBlobs would delete: those neither live nor within the grace window. The
// selection (LiveBlobSet plus the blobGCGrace window) is identical to GCBlobs,
// so plan and apply never diverge on which blobs are collectible.
//
// This is the no-write half of the deletion path (FR-03.22/23): it lets a
// later gc --dry-run report exactly which blobs would be reclaimed, and how
// many bytes, before any deletion.
func PlanGCBlobs(ctx context.Context, store *acf.Store, blobs *blobstore.Store) ([]blobstore.PlanGCEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	live, err := LiveBlobSet(ctx, store)
	if err != nil {
		return nil, err
	}
	graceCutoff := time.Now().Add(-blobGCGrace)
	entries, err := blobs.PlanGC(live, graceCutoff)
	if err != nil {
		return entries, fmt.Errorf("retention: plan gc blobs: %w", err)
	}
	return entries, nil
}

// GCBlobs builds the live ContentHash set across all conversation events
// (active + compacted) and deletes every blob that is neither live nor
// within the grace window. Returns the number of blobs deleted.
//
// This is the deletion half: EvictAttachments only appends evicted
// markers (keeping the chain append-only); the actual byte reclamation
// happens here, once no live event references a blob and the grace window
// has elapsed.
func GCBlobs(ctx context.Context, store *acf.Store, blobs *blobstore.Store) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	live, err := LiveBlobSet(ctx, store)
	if err != nil {
		return 0, err
	}
	graceCutoff := time.Now().Add(-blobGCGrace)
	deleted, err := blobs.GC(live, graceCutoff)
	if err != nil {
		return deleted, fmt.Errorf("retention: gc blobs: %w", err)
	}
	return deleted, nil
}
