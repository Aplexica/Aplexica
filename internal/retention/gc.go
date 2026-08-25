// Manual garbage-collection pass (FR-03.22/23) plus the
// --force-local-only prune gate (FR-03.24 fallback).
//
// RunGC is the operator-invoked full retention pass behind `aplexica gc`. It
// is NOT watermark-gated like RunPressureSweep — it always attempts the full
// ordered pass (evict attachments -> GC blobs -> snapshot eligible artifacts
// -> prune each artifact). The snapshot and prune phases share the same policy
// and peer-ACK gate: a full-state snapshot is not appended when the consuming
// prune is disabled or unauthorized. Otherwise a non-forced GC could duplicate
// a very large conversation and reclaim nothing.
//
// PeerAckGate is a seam, not a working implementation. True per-device ACK
// coordination (FR-03.24) is blocked on relay support: there is no
// ACK-cursor API in internal/plugin/proto today, so the only shipped gate
// (NoPeerAck) always reports "not acked". Until that API lands, an operator
// who knows their store is single-device (or who accepts the trade-off) uses
// --force-local-only to compact history.
package retention

import (
	"context"
	"log/slog"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/blobstore"
)

// OpPruneBlocked marks a history-compaction prune that RunGC SKIPPED because
// the prune was not authorized: neither --force-local-only was set nor did the
// PeerAckGate report every peer acked the artifact. It frees no bytes (it is a
// no-op the operator can unblock with --force-local-only). Emitted by RunGC.
const OpPruneBlocked = "prune-blocked"

// forceLocalOnlyHint is the Detail stamped on an OpPruneBlocked action so the
// rendered report tells the operator exactly how to authorize the prune.
const forceLocalOnlyHint = "needs --force-local-only"

// gcGraceAlways is the graceDeadline RunGC passes to the prune path: the zero
// time, which makes the grace-delete prediction (now.Before(graceDeadline))
// always false. Like the pressure sweep, RunGC never grace-deletes the freshly
// written compacted file in the same pass — relief comes from moving events
// out of the active log; a later prune past the real grace window reclaims the
// compacted file itself.
var gcGraceAlways = sweepNoGraceDelete

// PeerAckGate decides whether a history-compaction prune of one artifact is
// safe with respect to OTHER devices: AllPeersAcked reports whether every peer
// that should receive this artifact has already acknowledged (replicated) the
// events the prune would move out of the active log.
//
// FR-03.24 is blocked: no transport ACK-cursor API exists in
// internal/plugin/proto yet, so the only shipped implementation is NoPeerAck.
type PeerAckGate interface {
	// AllPeersAcked reports whether every peer has acknowledged the artifact's
	// pre-snapshot events. A true result authorizes a history-compaction prune
	// without --force-local-only.
	AllPeersAcked(kind acf.Kind, artifactID string) bool
}

// NoPeerAck is the default, always-deny PeerAckGate. It returns false for
// every artifact because there is no transport ACK-cursor API yet (FR-03.24
// blocked on relay work): with no way to learn what peers have replicated, the
// safe default is to assume nothing is acked, so history compaction requires an
// explicit --force-local-only.
type NoPeerAck struct{}

// AllPeersAcked always returns false — see NoPeerAck.
func (NoPeerAck) AllPeersAcked(_ acf.Kind, _ string) bool { return false }

// GCOptions configures a RunGC pass.
type GCOptions struct {
	// DryRun reports what RunGC WOULD do without performing any write.
	DryRun bool
	// ForceLocalOnly authorizes history-compaction prunes outright, bypassing
	// the PeerAckGate. The operator asserts the store is single-device (or
	// accepts that un-replicated peers will lose the compacted events).
	ForceLocalOnly bool
	// AckGate decides per-artifact whether a prune may proceed without
	// --force-local-only. A nil gate is treated as NoPeerAck (deny-all).
	AckGate PeerAckGate
}

// gcEvictOpts builds the attachment-eviction options for a RunGC pass from the
// retention config (same fields the pressure sweep uses), tagging the eviction
// reason "gc" so an operator can tell the manual pass apart from the pressure
// and time-loop paths.
func gcEvictOpts(cfg Config) EvictOpts {
	return EvictOpts{MinAge: cfg.AttachmentMinAge, Reason: gcEvictReason}
}

// gcEvictReason is the EvictedInfo.Reason stamped on attachments the manual GC
// pass evicts (distinct from the pressure sweep's "pressure" and the time
// loop's "age" so the provenance of a freed blob is auditable).
const gcEvictReason = "gc"

// RunGC performs a MANUAL full retention pass and returns a GCReport. The pass
// runs in a fixed order:
//
//	(a) attachments (only when cfg.AttachmentsOnly): evict old attachment
//	    bytes across every conversation artifact, then GC unreferenced blobs.
//	(b) snapshot: snapshot every prune-authorized, non-pinned artifact so prune
//	    has fresh checkpoints. Skip the phase when snapshots are kept forever.
//	(c) prune: for each artifact across all four kinds, compact pre-snapshot
//	    history — but ONLY when the prune is authorized (opts.ForceLocalOnly,
//	    or the gate reports all peers acked). An unauthorized prune that WOULD
//	    move events is SKIPPED and recorded as OpPruneBlocked.
//
// Unlike RunPressureSweep this is not watermark-gated: every phase always
// runs. When opts.DryRun is set, RunGC performs ZERO writes — it builds the
// report from the PR4 Plan* paths — and still reflects the gate (a gated-out
// prune is reported as OpPruneBlocked, an authorized one as OpPruneEvents).
//
// Best-effort per artifact: a per-artifact error in the prune phase is logged
// and the pass continues. ctx cancellation is honored between artifacts.
func RunGC(ctx context.Context, store *acf.Store, blobs *blobstore.Store, cfg Config, opts GCOptions) (GCReport, error) {
	gate := opts.AckGate
	if gate == nil {
		gate = NoPeerAck{}
	}
	if opts.DryRun {
		return runGCDryRun(ctx, store, blobs, cfg, opts, gate)
	}
	return runGCApply(ctx, store, blobs, cfg, opts, gate)
}

// runGCApply executes the writing RunGC pass. Attachment eviction and blob GC
// reuse the pressure-sweep helpers. The manual snapshot phase is deliberately
// coupled to the same policy and peer-ACK gate as the following prune: writing
// a full-state checkpoint without permission to consume it only grows the
// store, and can be especially expensive for active conversations.
func runGCApply(ctx context.Context, store *acf.Store, blobs *blobstore.Store, cfg Config, opts GCOptions, gate PeerAckGate) (GCReport, error) {
	report := GCReport{}

	if cfg.AttachmentsOnly {
		if err := runGCEvictAttachments(ctx, store, blobs, cfg, &report); err != nil {
			return report, err
		}
		if err := sweepGCBlobs(ctx, store, blobs, &report); err != nil {
			return report, err
		}
		if err := ctx.Err(); err != nil {
			return report, err
		}
	}

	if err := runGCSnapshots(ctx, store, cfg, opts, gate, &report); err != nil {
		return report, err
	}
	if err := ctx.Err(); err != nil {
		return report, err
	}

	if err := runGCPrune(ctx, store, cfg, opts, gate, &report); err != nil {
		return report, err
	}

	// Snapshot-count cap (FR-03.25): after on-snapshot pruning, enforce the
	// per-artifact keep_last_n_snapshots cap so any artifact still holding more
	// than N snapshots (e.g. one whose on-snapshot prune was gated out) is
	// compacted down to the most-recent N. A no-op when keep_last_n is "all" or
	// every artifact is already at/under the cap. Gated identically to the
	// on-snapshot prune: an unauthorized cap-compaction is recorded
	// OpPruneBlocked rather than applied.
	if err := runGCSnapshotCap(ctx, store, cfg, opts, gate, &report); err != nil {
		return report, err
	}
	return report, nil
}

// runGCSnapshots appends only checkpoints that the same RunGC invocation is
// authorized to consume. Automatic cadence/time snapshotters have their own
// large-conversation guard, while the manual GC path may need a large
// checkpoint to compact a large log; the invariant here is therefore policy
// and authorization, not a byte limit.
//
// keep_last_n_snapshots="all" disables the consuming prune by definition, so
// this phase is a no-op even under --force-local-only. Pinned artifacts are
// also skipped because PlanPruneArtifact will never move their history.
func runGCSnapshots(
	ctx context.Context,
	store *acf.Store,
	cfg Config,
	opts GCOptions,
	gate PeerAckGate,
	report *GCReport,
) error {
	if cfg.KeepLastNSnapshots == keepAll {
		return nil
	}
	var snapped int
	for _, kind := range []acf.Kind{acf.KindMemory, acf.KindSkill, acf.KindTool, acf.KindConversation} {
		arts, err := store.ListArtifacts(kind)
		if err != nil {
			return err
		}
		for _, art := range arts {
			if err := ctx.Err(); err != nil {
				return err
			}
			if gcPruneExempt(art) || !prunePermitted(opts, gate, kind, art.ArtifactID) {
				continue
			}
			latest, found, err := store.LastEvent(kind, art.ArtifactID)
			if err != nil || !found || isCheckpointEvent(latest) {
				continue
			}
			if _, err := CreateSnapshot(ctx, store, kind, art.ArtifactID); err == nil {
				snapped++
			}
		}
	}
	if snapped > 0 {
		report.AddAction(GCAction{Kind: acf.KindConversation, Op: OpSnapshot})
	}
	return nil
}

func gcPruneExempt(art acf.Artifact) bool {
	for _, tag := range art.Tags {
		if tag == "pinned" || tag == "keep-forever" {
			return true
		}
	}
	return false
}

// runGCSnapshotCap enforces the FR-03.25 snapshot-count cap across every
// artifact, but only where prunePermitted authorizes it (same gate as
// runGCPrune). An authorized cap-compaction that moves events records
// OpPruneEvents; an unauthorized one that WOULD move events records
// OpPruneBlocked and performs no write. Skipped entirely when snapshots are
// kept forever. Per-artifact errors are logged and skipped.
func runGCSnapshotCap(ctx context.Context, store *acf.Store, cfg Config, opts GCOptions, gate PeerAckGate, report *GCReport) error {
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
			plan, perr := PlanPruneArtifactKeepingSnapshots(ctx, store, k, art.ArtifactID, cfg.KeepLastNSnapshots, gcGraceAlways)
			if perr != nil {
				slog.Warn("gc: plan snapshot-cap failed", "kind", k, "artifact", art.ArtifactID, "err", perr)
				continue
			}
			if len(plan.ToMove) == 0 {
				continue // at/under the cap, pinned, or all protected
			}
			if !prunePermitted(opts, gate, k, art.ArtifactID) {
				report.AddAction(GCAction{
					Kind:       k,
					ArtifactID: art.ArtifactID,
					Op:         OpPruneBlocked,
					Detail:     forceLocalOnlyHint,
				})
				continue
			}
			moved, _, aerr := applyPrunePlan(store, k, art.ArtifactID, plan)
			if aerr != nil {
				slog.Warn("gc: apply snapshot-cap failed", "kind", k, "artifact", art.ArtifactID, "err", aerr)
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

// runGCEvictAttachments mirrors sweepEvictAttachments but stamps the "gc"
// reason. Per-artifact errors are logged and skipped.
func runGCEvictAttachments(ctx context.Context, store *acf.Store, blobs *blobstore.Store, cfg Config, report *GCReport) error {
	arts, err := store.ListArtifacts(acf.KindConversation)
	if err != nil {
		return err
	}
	opts := gcEvictOpts(cfg)
	for _, art := range arts {
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		plan, perr := PlanEvictAttachments(ctx, store, acf.KindConversation, art.ArtifactID, opts)
		if perr != nil {
			slog.Warn("gc: plan attachment eviction failed", "artifact", art.ArtifactID, "err", perr)
			continue
		}
		if _, aerr := EvictAttachments(ctx, store, blobs, acf.KindConversation, art.ArtifactID, opts); aerr != nil {
			slog.Warn("gc: evict attachments failed", "artifact", art.ArtifactID, "err", aerr)
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

// runGCPrune compacts pre-snapshot history for every artifact across all four
// kinds, but only where prunePermitted authorizes it. An authorized prune that
// moves events records OpPruneEvents; an unauthorized prune that WOULD move
// events records OpPruneBlocked instead and performs no write. Per-artifact
// errors are logged and skipped. Pruning is skipped entirely when snapshots
// are kept forever (keep_last_n_snapshots == "all").
func runGCPrune(ctx context.Context, store *acf.Store, cfg Config, opts GCOptions, gate PeerAckGate, report *GCReport) error {
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
			plan, perr := PlanPruneArtifact(ctx, store, k, art.ArtifactID, gcGraceAlways)
			if perr != nil {
				slog.Warn("gc: plan prune failed", "kind", k, "artifact", art.ArtifactID, "err", perr)
				continue
			}
			if len(plan.ToMove) == 0 {
				continue // nothing to compact (no snapshot, pinned, all protected)
			}
			if !prunePermitted(opts, gate, k, art.ArtifactID) {
				report.AddAction(GCAction{
					Kind:       k,
					ArtifactID: art.ArtifactID,
					Op:         OpPruneBlocked,
					Detail:     forceLocalOnlyHint,
				})
				continue
			}
			moved, _, aerr := applyPrunePlan(store, k, art.ArtifactID, plan)
			if aerr != nil {
				slog.Warn("gc: apply prune failed", "kind", k, "artifact", art.ArtifactID, "err", aerr)
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

// prunePermitted reports whether a history-compaction prune of one artifact is
// authorized: --force-local-only bypasses the gate outright; otherwise the gate
// must report every peer has acked the artifact.
func prunePermitted(opts GCOptions, gate PeerAckGate, kind acf.Kind, artifactID string) bool {
	if opts.ForceLocalOnly {
		return true
	}
	return gate.AllPeersAcked(kind, artifactID)
}

// runGCDryRun builds the GCReport for a RunGC pass WITHOUT any write, using the
// PR4 Plan* paths. It mirrors the apply phase order — eviction + blob GC, then
// the snapshot phase, then prune — and reflects the prune gate exactly: an
// authorized history-compaction prune is reported as OpPruneEvents, an
// unauthorized one as OpPruneBlocked.
func runGCDryRun(ctx context.Context, store *acf.Store, blobs *blobstore.Store, cfg Config, opts GCOptions, gate PeerAckGate) (GCReport, error) {
	report := GCReport{DryRun: true}

	if cfg.AttachmentsOnly {
		if err := dryRunEvictAttachments(ctx, store, cfg, &report); err != nil {
			return report, err
		}
		blobEntries, err := PlanGCBlobs(ctx, store, blobs)
		if err != nil {
			return report, err
		}
		for _, b := range blobEntries {
			report.AddAction(GCAction{
				Kind:       acf.KindConversation,
				Op:         OpGCBlob,
				Detail:     b.Hash,
				BytesSaved: b.Bytes,
			})
		}
		if err := ctx.Err(); err != nil {
			return report, err
		}
	}

	// Snapshot phase: project exactly the policy- and ACK-authorized candidates
	// that runGCSnapshots would write.
	if err := dryRunGCSnapshots(ctx, store, cfg, opts, gate, &report); err != nil {
		return report, err
	}
	if err := ctx.Err(); err != nil {
		return report, err
	}

	if err := dryRunPrune(ctx, store, cfg, opts, gate, &report); err != nil {
		return report, err
	}

	// Project the snapshot-cap phase (FR-03.25) the apply pass performs after
	// on-snapshot pruning. Like dryRunPrune it plans against the CURRENT
	// on-disk snapshots only (no chaining behind the just-projected snapshot
	// phase), so the projection is a pure no-write read of present state.
	if err := dryRunSnapshotCap(ctx, store, cfg, opts, gate, &report); err != nil {
		return report, err
	}
	return report, nil
}

// dryRunSnapshotCap records the would-be snapshot-cap compaction (FR-03.25) for
// every artifact, reflecting the gate exactly as runGCSnapshotCap does: an
// authorized cap-compaction as OpPruneEvents (one per moved event), an
// unauthorized one as OpPruneBlocked. No write is performed. Skipped entirely
// when snapshots are kept forever.
func dryRunSnapshotCap(ctx context.Context, store *acf.Store, cfg Config, opts GCOptions, gate PeerAckGate, report *GCReport) error {
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
			plan, perr := PlanPruneArtifactKeepingSnapshots(ctx, store, k, art.ArtifactID, cfg.KeepLastNSnapshots, gcGraceAlways)
			if perr != nil {
				return perr
			}
			if len(plan.ToMove) == 0 {
				continue
			}
			if !prunePermitted(opts, gate, k, art.ArtifactID) {
				report.AddAction(GCAction{
					Kind:       k,
					ArtifactID: art.ArtifactID,
					Op:         OpPruneBlocked,
					Detail:     forceLocalOnlyHint,
				})
				continue
			}
			for _, e := range plan.ToMove {
				report.AddAction(GCAction{
					Kind:       k,
					ArtifactID: art.ArtifactID,
					Op:         OpPruneEvents,
					Detail:     e.Hash,
				})
			}
		}
	}
	return nil
}

// dryRunGCSnapshots projects runGCSnapshots WITHOUT writing. Eligibility is
// intentionally identical so dry-run never advertises a checkpoint that apply
// would suppress (or hides one apply would append).
func dryRunGCSnapshots(
	ctx context.Context,
	store *acf.Store,
	cfg Config,
	opts GCOptions,
	gate PeerAckGate,
	report *GCReport,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if cfg.KeepLastNSnapshots == keepAll {
		return nil
	}
	var wouldSnapshot int
	for _, k := range []acf.Kind{acf.KindMemory, acf.KindSkill, acf.KindTool, acf.KindConversation} {
		arts, err := store.ListArtifacts(k)
		if err != nil {
			return err
		}
		for _, art := range arts {
			if cerr := ctx.Err(); cerr != nil {
				return cerr
			}
			if gcPruneExempt(art) || !prunePermitted(opts, gate, k, art.ArtifactID) {
				continue
			}
			latest, found, rerr := store.LastEvent(k, art.ArtifactID)
			if rerr != nil || !found {
				continue // unreadable or empty — apply skips it too
			}
			if isCheckpointEvent(latest) {
				continue // head already a checkpoint (snapshot/baseline) — apply skips it
			}
			wouldSnapshot++
		}
	}
	if wouldSnapshot > 0 {
		report.AddAction(GCAction{
			Kind: acf.KindConversation,
			Op:   OpSnapshot,
			// BytesSaved 0 — a snapshot enables a later prune but frees
			// nothing on its own (mirrors sweepSnapshots).
		})
	}
	return nil
}

// dryRunEvictAttachments records the would-be attachment evictions across every
// conversation artifact without appending any marker event.
func dryRunEvictAttachments(ctx context.Context, store *acf.Store, cfg Config, report *GCReport) error {
	arts, err := store.ListArtifacts(acf.KindConversation)
	if err != nil {
		return err
	}
	opts := gcEvictOpts(cfg)
	for _, art := range arts {
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		plan, perr := PlanEvictAttachments(ctx, store, acf.KindConversation, art.ArtifactID, opts)
		if perr != nil {
			return perr
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

// dryRunPrune records the would-be prune for every artifact, reflecting the
// gate: an authorized history-compaction prune as OpPruneEvents, an
// unauthorized one as OpPruneBlocked. No write is performed. Pruning is skipped
// entirely when snapshots are kept forever.
//
// NOTE: the snapshot phase itself is projected separately by
// dryRunGCSnapshots (an OpSnapshot action), but the PRUNE projection here does
// NOT chain behind a just-projected snapshot: it plans against CURRENT on-disk
// checkpoints only. An artifact with no checkpoint can therefore report the
// authorized snapshot but not the prune that apply will perform immediately
// afterward. This is the existing conservative, no-write projection contract.
func dryRunPrune(ctx context.Context, store *acf.Store, cfg Config, opts GCOptions, gate PeerAckGate, report *GCReport) error {
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
			plan, perr := PlanPruneArtifact(ctx, store, k, art.ArtifactID, gcGraceAlways)
			if perr != nil {
				return perr
			}
			if len(plan.ToMove) == 0 {
				continue
			}
			if !prunePermitted(opts, gate, k, art.ArtifactID) {
				report.AddAction(GCAction{
					Kind:       k,
					ArtifactID: art.ArtifactID,
					Op:         OpPruneBlocked,
					Detail:     forceLocalOnlyHint,
				})
				continue
			}
			for _, e := range plan.ToMove {
				report.AddAction(GCAction{
					Kind:       k,
					ArtifactID: art.ArtifactID,
					Op:         OpPruneEvents,
					Detail:     e.Hash,
				})
			}
		}
	}
	return nil
}
