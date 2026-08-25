// Disk-pressure retention sweep (FR-03.20). When the canonical store
// approaches store_high_watermark the daemon runs ONE ordered sweep instead
// of only force-snapshotting. The sweep is the OSS-default attachments_only
// path: it evicts old attachment bytes first (cheap, lossless for text and
// the hash chain), reclaims those bytes, snapshots so prune has fresh
// checkpoints, and only as a last resort compacts history.
//
// Each phase re-checks an overWatermark() closure and returns early the
// moment pressure is relieved, so a momentary spike never escalates all the
// way to history compaction.
package retention

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/blobstore"
)

// sweepNoGraceDelete is the graceDeadline the sweep passes to PruneArtifact:
// the zero time, which makes PruneArtifact's grace-delete prediction
// (now.Before(graceDeadline)) always false. The sweep never grace-deletes the
// freshly-written compacted file in the same pass — pressure relief comes from
// moving events OUT of the active log; a later prune past the real grace
// window reclaims the compacted file itself.
var sweepNoGraceDelete = time.Time{}

// OpSnapshot marks a force-snapshot taken by the pressure sweep so it shows up
// in the GCReport action stream alongside the eviction / blob-GC / prune ops.
// It does not free bytes directly (a snapshot only enables a later prune), so
// its BytesSaved is always 0.
const OpSnapshot = "snapshot"

// pressureEvictReason is the EvictedInfo.Reason stamped on attachments the
// pressure sweep evicts (distinct from the time-loop's "age" reason so an
// operator can tell which path freed a blob).
const pressureEvictReason = "pressure"

// RunPressureSweep runs the ordered FR-03.20 retention sweep against a store
// that is at/over the high watermark, building a GCReport as it goes. The
// phases run in a FIXED order, each followed by an overWatermark() re-check
// that short-circuits the sweep the instant pressure is relieved:
//
//	(a) attachments (OSS default, cfg.AttachmentsOnly):
//	    evict OLD attachment blobs (MinAge=cfg.AttachmentMinAge, reason
//	    "pressure") across every conversation artifact — preserving text,
//	    metadata, and the hash chain — then GCBlobs to reclaim the bytes.
//	    Re-check; if relieved, return.
//	(b) snapshot: ForceSnapshotsAll so prune has fresh checkpoints. Skipped
//	    when keep_last_n_snapshots="all", because that policy disables the
//	    consuming prune and a full-payload snapshot would only grow the store.
//	    Re-check; if relieved, return.
//	(c) prune (ONLY if STILL over): PruneArtifact each artifact
//	    (branch-ancestor-safe, pin-exempt). Gated behind the still-over
//	    re-check so a momentary spike does not aggressively compact history,
//	    and skipped entirely when keep_last_n_snapshots == "all" (-1).
//
// Best-effort per artifact: a per-artifact error is logged and the sweep
// continues (mirroring ForceSnapshotsAll). ctx cancellation is honored
// between artifacts and phases. The returned GCReport is an apply report
// (DryRun=false) and its BytesSaved is the bytes actually reclaimed.
func RunPressureSweep(ctx context.Context, store *acf.Store, blobs *blobstore.Store, cfg Config, overWatermark func() bool) (GCReport, error) {
	report := GCReport{}

	// (a) attachments_only: evict old attachment bytes, then reclaim.
	if cfg.AttachmentsOnly {
		hasBlobs, berr := blobStoreHasFiles(blobs)
		if berr != nil {
			return report, berr
		}
		// An empty blob store has no attachment bytes to reclaim. Avoid
		// replaying every conversation log twice just to rediscover that fact;
		// on real stores with multi-gigabyte histories this no-op phase was the
		// dominant pressure-sweep CPU cost.
		if hasBlobs {
			if err := sweepEvictAttachments(ctx, store, blobs, cfg, &report); err != nil {
				return report, err
			}
			if err := sweepGCBlobs(ctx, store, blobs, &report); err != nil {
				return report, err
			}
			if err := ctx.Err(); err != nil {
				return report, err
			}
			if !overWatermark() {
				return report, nil
			}
		}
	}

	// (b) snapshot: give prune fresh checkpoints to compact behind. With the
	// keep-all policy the prune phase below is forbidden, so snapshotting would
	// be pure write amplification (multi-gigabyte conversations were repeatedly
	// copied here while pressure could never be relieved).
	if cfg.KeepLastNSnapshots == keepAll {
		return report, nil
	}
	if err := sweepSnapshots(ctx, store, &report); err != nil {
		return report, err
	}
	if err := ctx.Err(); err != nil {
		return report, err
	}
	if !overWatermark() {
		return report, nil
	}

	// (c) prune: last resort, gated behind the still-over re-check above and
	// disabled when snapshots are kept forever (returned before phase b).
	if err := sweepPrune(ctx, store, &report); err != nil {
		return report, err
	}
	return report, nil
}

func blobStoreHasFiles(blobs *blobstore.Store) (bool, error) {
	if blobs == nil || blobs.Root == "" {
		return false, nil
	}
	found := false
	err := filepath.WalkDir(blobs.Root, func(_ string, de fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if de.Type().IsRegular() {
			found = true
			return fs.SkipAll
		}
		return nil
	})
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return found, err
}

// sweepEvictAttachments evicts old attachments across every conversation
// artifact, recording one OpEvictAttachment action per evicting append.
// Per-artifact errors are logged and skipped.
func sweepEvictAttachments(ctx context.Context, store *acf.Store, blobs *blobstore.Store, cfg Config, report *GCReport) error {
	arts, err := store.ListArtifacts(acf.KindConversation)
	if err != nil {
		return err
	}
	opts := EvictOpts{MinAge: cfg.AttachmentMinAge, Reason: pressureEvictReason}
	for _, art := range arts {
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		// Plan first so the report carries the reclaimable byte total per
		// affected event; then apply (which re-derives the same plan). The
		// double-read is negligible against the disk-pressure cadence.
		plan, perr := PlanEvictAttachments(ctx, store, acf.KindConversation, art.ArtifactID, opts)
		if perr != nil {
			slog.Warn("pressure sweep: plan attachment eviction failed", "artifact", art.ArtifactID, "err", perr)
			continue
		}
		if _, aerr := EvictAttachments(ctx, store, blobs, acf.KindConversation, art.ArtifactID, opts); aerr != nil {
			slog.Warn("pressure sweep: evict attachments failed", "artifact", art.ArtifactID, "err", aerr)
			continue
		}
		for _, ev := range plan.Events {
			report.AddAction(GCAction{
				Kind:       acf.KindConversation,
				ArtifactID: art.ArtifactID,
				Op:         OpEvictAttachment,
				BytesSaved: ev.BytesReclaimable,
			})
		}
	}
	return nil
}

// sweepGCBlobs reclaims unreferenced blob bytes after eviction, recording one
// OpGCBlob action carrying the total bytes freed.
func sweepGCBlobs(ctx context.Context, store *acf.Store, blobs *blobstore.Store, report *GCReport) error {
	entries, perr := PlanGCBlobs(ctx, store, blobs)
	if perr != nil {
		return perr
	}
	deleted, gerr := GCBlobs(ctx, store, blobs)
	if gerr != nil {
		return gerr
	}
	if deleted == 0 {
		return nil
	}
	var freed int64
	for _, e := range entries {
		freed += e.Bytes
	}
	report.AddAction(GCAction{
		Kind:       acf.KindConversation,
		Op:         OpGCBlob,
		BytesSaved: freed,
	})
	return nil
}

// sweepSnapshots force-snapshots every artifact, recording one OpSnapshot
// action carrying the count. Mirrors ForceSnapshotsAll's best-effort
// semantics (per-artifact errors swallowed there).
func sweepSnapshots(ctx context.Context, store *acf.Store, report *GCReport) error {
	n, err := ForceSnapshotsAll(ctx, store)
	if err != nil {
		return err
	}
	if n > 0 {
		report.AddAction(GCAction{
			Kind: acf.KindConversation,
			Op:   OpSnapshot,
			// BytesSaved intentionally 0 — a snapshot enables a later prune
			// but frees nothing on its own.
		})
	}
	return nil
}

// sweepPrune compacts pre-snapshot history for every artifact across all four
// kinds, recording one OpPruneEvents action per artifact that moved events.
// Per-artifact errors are logged and skipped. Branch-ancestor safety and pin
// exemption are enforced inside PruneArtifact.
func sweepPrune(ctx context.Context, store *acf.Store, report *GCReport) error {
	for _, k := range []acf.Kind{acf.KindMemory, acf.KindSkill, acf.KindTool, acf.KindConversation} {
		arts, err := store.ListArtifacts(k)
		if err != nil {
			return err
		}
		for _, art := range arts {
			if cerr := ctx.Err(); cerr != nil {
				return cerr
			}
			// graceDeadline = zero time: never grace-delete the freshly-written
			// compacted file in the same pass (a future prune past the grace
			// window deletes it). Pressure relief comes from moving events out
			// of the active log, not from same-pass deletion.
			moved, _, perr := PruneArtifact(ctx, store, k, art.ArtifactID, sweepNoGraceDelete)
			if perr != nil {
				slog.Warn("pressure sweep: prune failed", "kind", k, "artifact", art.ArtifactID, "err", perr)
				continue
			}
			if moved > 0 {
				report.AddAction(GCAction{
					Kind:       k,
					ArtifactID: art.ArtifactID,
					Op:         OpPruneEvents,
				})
			}
		}
	}
	return nil
}
