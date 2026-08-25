// Per-artifact snapshot-count cap (BRD-03 §4.8.4, FR-03.25). When an artifact
// accumulates more than keep_last_n_snapshots snapshots, the older snapshot
// history is compacted out of the hash-chained active log, anchored at the
// Nth-most-recent snapshot, so only the most recent N snapshots' worth of
// history remains reachable.
//
// This is the routine cap enforcement, distinct from the on-snapshot prune
// (PruneArtifact, anchor = most-recent snapshot) and from the emergency
// pressure-sweep's aggressive single-snapshot compaction. It reuses exactly
// the same move-to-.compacted mechanism (planPruneAtAnchor + applyPrunePlan),
// so branch-ancestor protection, pin exemption, and acf.VerifyChain integrity
// across the prune boundary are preserved.
package retention

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
)

// minKeepLastN is the floor for the snapshot-count cap (FR-03.25): the cap
// never retains fewer than two snapshots, so even a misconfigured or
// below-floor keepN is clamped up to this value before any compaction. Two is
// the minimum that still leaves a prior checkpoint to fall back to.
const minKeepLastN = 2

// PlanPruneArtifactKeepingSnapshots computes — WITHOUT writing — the
// snapshot-cap compaction for one artifact (FR-03.25): the branch-ancestor-safe
// set of events strictly before the Nth-most-recent snapshot to move into the
// .compacted layer. keepN is clamped up to minKeepLastN first, so the cap never
// retains fewer than two snapshots.
//
// When the artifact has keepN or fewer snapshots there is no Nth-most-recent
// anchor and the plan is empty (nothing to compact). Pin-tag exemption,
// branch-ancestor protection, byte accounting, and the grace-delete prediction
// are all computed by the shared planPruneAtAnchor, identically to the
// on-snapshot prune path.
func PlanPruneArtifactKeepingSnapshots(ctx context.Context, store *acf.Store, kind acf.Kind, artifactID string, keepN int, graceDeadline time.Time) (PrunePlan, error) {
	if cerr := ctx.Err(); cerr != nil {
		return PrunePlan{}, cerr
	}
	if keepN < minKeepLastN {
		keepN = minKeepLastN
	}

	art, err := store.ReadArtifact(kind, artifactID)
	if err != nil {
		return PrunePlan{}, fmt.Errorf("retention: read artifact: %w", err)
	}
	for _, tag := range art.Tags {
		if tag == "pinned" || tag == "keep-forever" {
			return PrunePlan{}, nil
		}
	}

	events, err := store.ReadEvents(kind, artifactID)
	if err != nil {
		return PrunePlan{}, fmt.Errorf("retention: read events: %w", err)
	}

	// FR-03.25 triggers only when the artifact has MORE THAN keepN snapshots.
	// At or under the cap there is nothing to shed: anchoring at the oldest
	// retained snapshot would still compact the genesis prefix before it, so
	// gate on the count first to keep an at-cap artifact a strict no-op.
	if snapshotCountIn(events) <= keepN {
		return PrunePlan{}, nil
	}

	// Anchor at the Nth-most-recent snapshot. Keeping the most recent N
	// snapshots' worth of history means compacting everything strictly before
	// that snapshot; the snapshot itself and all later events (including the
	// other N-1 retained snapshots) stay in the active log.
	anchorIdx := nthMostRecentSnapshotIndex(events, keepN)
	return planPruneAtAnchor(store, kind, artifactID, events, anchorIdx, graceDeadline)
}

// snapshotCountIn returns how many checkpoint events (EventTypeSnapshot or
// EventTypeBaseline — see isCheckpointEvent) are in events. Counting the same
// event set the anchor walk (nthMostRecentSnapshotIndex) selects from keeps
// the cap's count gate and its anchor consistent.
func snapshotCountIn(events []acf.Event) int {
	n := 0
	for _, e := range events {
		if isCheckpointEvent(e) {
			n++
		}
	}
	return n
}

// EnforceSnapshotCapAll applies the FR-03.25 snapshot-count cap across every
// artifact of all four kinds, recording one OpPruneEvents action per artifact
// whose older snapshot history it compacts. It is a strict no-op when
// cfg.KeepLastNSnapshots is the "all" sentinel (keepAll) — snapshots are then
// kept forever — so callers can invoke it unconditionally. Per-artifact errors
// are logged and skipped (best-effort, matching the other store-wide retention
// passes); ctx cancellation is honored between artifacts.
//
// It reuses EnforceSnapshotCap (hence the shared prune machinery), so branch
// protection, pin exemption, and VerifyChain integrity hold identically. The
// configured N is floored at minKeepLastN inside EnforceSnapshotCap.
func EnforceSnapshotCapAll(ctx context.Context, store *acf.Store, cfg Config, graceDeadline time.Time, report *GCReport) error {
	if cfg.KeepLastNSnapshots == keepAll {
		return nil
	}
	for _, k := range []acf.Kind{acf.KindMemory, acf.KindSkill, acf.KindTool, acf.KindConversation} {
		arts, err := store.ListArtifacts(k)
		if err != nil {
			return err
		}
		for _, art := range arts {
			if cerr := ctx.Err(); cerr != nil {
				return cerr
			}
			moved, _, aerr := EnforceSnapshotCap(ctx, store, k, art.ArtifactID, cfg.KeepLastNSnapshots, graceDeadline)
			if aerr != nil {
				slog.Warn("retention: snapshot-cap enforcement failed", "kind", k, "artifact", art.ArtifactID, "err", aerr)
				continue
			}
			if moved > 0 && report != nil {
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

// EnforceSnapshotCap applies the FR-03.25 snapshot-count cap to one artifact:
// if it has more than keepN snapshots, compact the pre-anchor history so only
// the most recent N snapshots remain reachable in the active log. keepN is
// floored at minKeepLastN. It is the apply half of the cap (Plan + apply),
// reusing applyPrunePlan so the compaction is byte-identical to every other
// prune path. Returns (movedThisCall, deletedThisCall, err); a no-op (at/under
// the cap, pinned, or fully branch-protected) returns (0, 0, nil).
func EnforceSnapshotCap(ctx context.Context, store *acf.Store, kind acf.Kind, artifactID string, keepN int, graceDeadline time.Time) (movedCount, deletedCount int, err error) {
	plan, err := PlanPruneArtifactKeepingSnapshots(ctx, store, kind, artifactID, keepN, graceDeadline)
	if err != nil {
		return 0, 0, err
	}
	return applyPrunePlan(store, kind, artifactID, plan)
}
