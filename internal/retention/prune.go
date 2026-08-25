package retention

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/atomicfile"
)

// PruneArtifact implements the BRD-03 §4.8.2 on-snapshot pruning policy
// for a single artifact:
//
//  1. Read all events.
//  2. If the artifact has tag "pinned" or "keep-forever" → skip (return 0,0,nil).
//  3. If there is no snapshot event (or the snapshot is the first event) → skip.
//  4. Move all events STRICTLY BEFORE the most recent snapshot into
//     <store>/events/.compacted/<kind>/<id>.jsonl.gz (appended, gzipped).
//  5. Atomically rewrite the active events file with toKeep (snapshot + tail).
//  6. If the compacted file's mtime is older than graceDeadline, delete it.
//
// Returns (movedThisCall, deletedThisCall, err).
//
// Hash chain note (per ADR-0024): the snapshot event is the first entry in
// the rewritten active log. Its ParentHash still references the last
// pre-snapshot event's content hash — and that event now lives in
// .compacted. Verification across the boundary uses
// Store.ReadEventsIncludingCompacted to re-merge the two layers in
// timestamp order before walking the chain.
//
// Branch-ancestor protection (§4.8.2 rule 1, FR-03.17): before slicing the
// pre-snapshot events to move, PruneArtifact builds a PROTECTED set covering
// the full ancestry of every live branch head and excludes those events from
// the move. Without this, a side branch forked off a pre-snapshot event would
// be orphaned — its fork-point ancestor would be compacted away and the
// branch could no longer VerifyChain or replay. See collectProtectedAncestors.
//
// PruneArtifact is now a thin Plan + apply (FR-03.22/23): it computes a
// PrunePlan (no writes) via PlanPruneArtifact, then performs the existing
// move / active-rewrite / grace-delete writes. The split lets a later
// gc --dry-run report what would change without mutating. Apply behavior is
// unchanged: it honors exactly the plan PlanPruneArtifact returns.
func PruneArtifact(ctx context.Context, store *acf.Store, kind acf.Kind, artifactID string, graceDeadline time.Time) (movedCount, deletedCount int, err error) {
	plan, err := PlanPruneArtifact(ctx, store, kind, artifactID, graceDeadline)
	if err != nil {
		return 0, 0, err
	}
	return applyPrunePlan(store, kind, artifactID, plan)
}

// PrunePlan is the no-write result of PlanPruneArtifact: the exact set of
// pre-snapshot events PruneArtifact would move into the .compacted layer, and
// whether the resulting compacted file would be grace-deleted in the same
// pass. It is the source of truth the apply path executes verbatim, so plan
// and apply can never diverge.
type PrunePlan struct {
	// ToMove is the ordered set of events that would be moved out of the
	// active log into <store>/events/.compacted/<kind>/<id>.jsonl.gz,
	// preserving append order. Empty when nothing would move (no snapshot,
	// pinned, or everything before the snapshot is branch-protected).
	ToMove []acf.Event
	// ToKeep is the active log after the move (snapshot + protected tail),
	// preserving append order. Only meaningful when ToMove is non-empty.
	ToKeep []acf.Event
	// MovedBytes is the total JSON-encoded byte size of ToMove — the bytes
	// that would move to the compacted layer.
	MovedBytes int64
	// DeleteCompacted reports whether the compacted file would be deleted
	// outright after the move because its mtime would be older than
	// graceDeadline. The apply path obeys this decision verbatim.
	DeleteCompacted bool
	// CompactedBytes is the on-disk size of the compacted file that would be
	// grace-deleted (only set when DeleteCompacted is true).
	CompactedBytes int64
}

// PlanPruneArtifact computes — WITHOUT writing — exactly what PruneArtifact
// would do for one artifact: the branch-ancestor-safe set of pre-snapshot
// events to move, the bytes that would move, and whether the resulting
// compacted file would be grace-deleted. Pin-tag exemption, the most-recent-
// snapshot boundary, and branch-ancestor protection are computed identically
// to apply.
//
// The grace-delete prediction mirrors apply precisely: apply writes the
// compacted file fresh (mtime ≈ now) and then deletes it iff its mtime is
// before graceDeadline. PlanPruneArtifact captures the same reference instant
// and predicts deletion iff that instant is before graceDeadline, so the plan
// and the apply stat-check agree on every call.
func PlanPruneArtifact(ctx context.Context, store *acf.Store, kind acf.Kind, artifactID string, graceDeadline time.Time) (PrunePlan, error) {
	if cerr := ctx.Err(); cerr != nil {
		return PrunePlan{}, cerr
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

	// Anchor at the most recent snapshot: everything strictly before it is
	// superseded by the checkpoint for main-branch replay and is eligible to
	// compact. This is the on-snapshot pruning boundary; the FR-03.25
	// snapshot-cap path anchors farther back (the Nth-most-recent snapshot)
	// via planPruneAtAnchor.
	anchorIdx := nthMostRecentSnapshotIndex(events, 1)
	return planPruneAtAnchor(store, kind, artifactID, events, anchorIdx, graceDeadline)
}

// isCheckpointEvent reports whether e is a self-contained full-state
// checkpoint for retention purposes: an EventTypeSnapshot (FR-02.32), or an
// aligned-chains EventTypeBaseline — a baseline carries the full materialized
// origin state (acf.AdoptBaseline requires a payload), so everything strictly
// before it is superseded for main-branch replay exactly as with a
// payload-bearing snapshot. Shared by the prune anchor, the snapshot-count
// cap, and the head-already-a-checkpoint guards so the sites can't drift.
func isCheckpointEvent(e acf.Event) bool {
	return e.Type == acf.EventTypeSnapshot || e.Type == acf.EventTypeBaseline
}

// assertsAttachmentSlots reports whether e is an event whose payload asserts
// attachment slots: a payload-bearing content event (create/update/
// resolution), or a payload-bearing checkpoint (snapshot/baseline — see
// isCheckpointEvent), which carries the full materialized state INCLUDING
// the attachment list. After an on-snapshot prune whose compacted segment
// was grace-deleted — or a baseline adoption, where the origin history never
// existed locally — a checkpoint can be the ONLY event naming a ContentHash.
// A legacy payload-LESS snapshot asserts nothing.
//
// Shared by the blob-GC live set (LiveBlobSet) and the eviction planner
// (PlanEvictAttachments) so the two can never disagree on which events
// assert a slot. acf's collectBundleBlobHashes mirrors the same filter
// (retention is unimportable there — cycle).
func assertsAttachmentSlots(e acf.Event) bool {
	switch {
	case e.Type == acf.EventTypeCreate,
		e.Type == acf.EventTypeUpdate,
		e.Type == acf.EventTypeResolution:
		return true
	case isCheckpointEvent(e):
		return acf.HasPayload(e.Payload)
	}
	return false
}

// nthMostRecentSnapshotIndex returns the index in events (append order) of the
// Nth-most-recent checkpoint event (EventTypeSnapshot or EventTypeBaseline —
// see isCheckpointEvent), where n==1 is the most recent. It returns -1 when
// there are fewer than n checkpoints. The returned index is the compaction
// anchor: events strictly before it are eligible to move out of the active
// log, and the checkpoint at the index plus everything after it stays.
func nthMostRecentSnapshotIndex(events []acf.Event, n int) int {
	if n < 1 {
		return -1
	}
	seen := 0
	for i := len(events) - 1; i >= 0; i-- {
		if !isCheckpointEvent(events[i]) {
			continue
		}
		seen++
		if seen == n {
			return i
		}
	}
	return -1
}

// planPruneAtAnchor builds the branch-ancestor-safe PrunePlan that compacts
// every event strictly before anchorIdx. It is the shared core of both the
// on-snapshot prune (anchor = most-recent snapshot) and the FR-03.25
// snapshot-cap prune (anchor = Nth-most-recent snapshot): given the chosen
// anchor it applies identical branch protection, byte accounting, and
// grace-delete prediction, so the two paths can never diverge on how the move
// set is sliced or how reclaim is predicted.
//
// anchorIdx <= 0 (no qualifying snapshot, or the anchor is already the first
// event) yields an empty plan — there is nothing before the anchor to compact.
func planPruneAtAnchor(store *acf.Store, kind acf.Kind, artifactID string, events []acf.Event, anchorIdx int, graceDeadline time.Time) (PrunePlan, error) {
	if anchorIdx <= 0 {
		return PrunePlan{}, nil // no snapshot anchor, or anchor is already the first event
	}

	// Branch-ancestor protection (FR-03.17): mark every event in the
	// ancestry of every live branch head as protected, then exclude those
	// from the move. toKeep is the original active log minus the moved set,
	// preserving append order.
	protected, perr := collectProtectedAncestors(store, kind, artifactID)
	if perr != nil {
		return PrunePlan{}, fmt.Errorf("retention: collect protected ancestors: %w", perr)
	}

	toMove := make([]acf.Event, 0, anchorIdx)
	toKeep := make([]acf.Event, 0, len(events)-anchorIdx)
	var movedBytes int64
	for i, e := range events {
		if i < anchorIdx && !protected[e.Hash] {
			toMove = append(toMove, e)
			if jsonBytes, mErr := json.Marshal(e); mErr == nil {
				movedBytes += int64(len(jsonBytes)) + 1 // +1 for the trailing newline
			}
			continue
		}
		toKeep = append(toKeep, e)
	}
	if len(toMove) == 0 {
		return PrunePlan{}, nil
	}

	// Predict the grace-delete decision against the same instant apply's
	// fresh write would stamp on the compacted file. apply deletes iff the
	// (freshly-written) compacted mtime is before graceDeadline; capturing
	// the reference instant here keeps plan and apply in lockstep.
	now := time.Now()
	deleteCompacted := now.Before(graceDeadline)
	var compactedBytes int64
	if deleteCompacted {
		compactedBytes = predictCompactedBytes(store, kind, artifactID, toMove)
	}

	return PrunePlan{
		ToMove:          toMove,
		ToKeep:          toKeep,
		MovedBytes:      movedBytes,
		DeleteCompacted: deleteCompacted,
		CompactedBytes:  compactedBytes,
	}, nil
}

// compactedScanInitBuf / compactedScanMaxBuf size the bufio.Scanner used to
// decompress the compacted gz log: a 64 KiB initial buffer that grows to a
// 256 MiB cap. A single event line can approach the max-artifact-size (default
// 64 MiB), so — mirroring internal/acf.scanBufMax — the cap must exceed it or a
// large-history artifact's compacted log aborts with "bufio.Scanner: token too
// long". The scanner only grows the buffer as a line demands. Shared by every
// reader (apply + the dry-run estimate) so the buffering is defined once.
const (
	compactedScanInitBuf = 64 * 1024
	compactedScanMaxBuf  = 256 * 1024 * 1024
)

// readExistingCompactedLines decompresses the compacted gz file at
// compactedPath and returns its events as newline-terminated JSON bytes (the
// concatenation that a re-write appends onto). A missing file yields (nil,
// nil) — the common first-prune case. Any non-ErrNotExist open error is
// returned so callers can decide whether to fail hard (apply) or fall back to
// an estimate (dry-run). gzip-decode errors leave whatever lines were read so
// far, matching the pre-existing best-effort behavior.
func readExistingCompactedLines(compactedPath string) ([]byte, error) {
	f, oerr := os.Open(compactedPath)
	if oerr != nil {
		if errors.Is(oerr, os.ErrNotExist) {
			return nil, nil
		}
		return nil, oerr
	}
	defer f.Close()

	gz, gerr := gzip.NewReader(f)
	if gerr != nil {
		return nil, nil
	}
	defer gz.Close()

	var lines []byte
	scanBuf := bufio.NewScanner(gz)
	scanBuf.Buffer(make([]byte, 0, compactedScanInitBuf), compactedScanMaxBuf)
	for scanBuf.Scan() {
		lines = append(lines, scanBuf.Bytes()...)
		lines = append(lines, '\n')
	}
	return lines, nil
}

// marshalEventLines encodes events as JSON, one per line with a trailing
// newline — the exact wire form the active log and the compacted log both
// use. Shared by apply and the dry-run estimate so their encodings can't
// drift.
func marshalEventLines(events []acf.Event) ([]byte, error) {
	var lines []byte
	for _, e := range events {
		jsonBytes, mErr := json.Marshal(e)
		if mErr != nil {
			return nil, mErr
		}
		lines = append(lines, jsonBytes...)
		lines = append(lines, '\n')
	}
	return lines, nil
}

// predictCompactedBytes returns the on-disk size the compacted gz file would
// have AFTER toMove is appended — i.e. the bytes a same-pass grace-delete
// would actually reclaim. It mirrors applyPrunePlan's encoding exactly:
// decompress the existing compacted file (if any), append each toMove event as
// json.Marshal + '\n', gzip the concatenation, and report the gzipped length.
//
// Mirroring apply matters: apply APPENDS toMove to any pre-existing compacted
// file and only THEN grace-deletes it, so the file removed is larger than the
// pre-append size. Returning the existing file's bare os.Stat size (the prior
// behavior) under-counted the reclaim by the newly-appended events' size. This
// figure feeds the dry-run report only (FR-03.23) and never gates the move.
//
// On any read/encode error the estimate degrades to the raw JSON-line byte
// total of toMove (a documented lower bound) rather than returning zero.
func predictCompactedBytes(store *acf.Store, kind acf.Kind, artifactID string, toMove []acf.Event) int64 {
	compactedPath := filepath.Join(store.Root, "events", ".compacted", kindDirName(kind), artifactID+".jsonl.gz")

	rawLowerBound := func() int64 {
		var raw int64
		for _, e := range toMove {
			if jsonBytes, mErr := json.Marshal(e); mErr == nil {
				raw += int64(len(jsonBytes)) + 1
			}
		}
		return raw
	}

	// Decompress the existing compacted file (if present); mirrors apply. A
	// non-ErrNotExist read error degrades to the lower-bound estimate.
	existingBytes, rerr := readExistingCompactedLines(compactedPath)
	if rerr != nil {
		return rawLowerBound()
	}

	newLines, merr := marshalEventLines(toMove)
	if merr != nil {
		return rawLowerBound()
	}
	all := append(existingBytes, newLines...)

	// Gzip the concatenation and report the on-disk size, matching the
	// atomic write applyPrunePlan performs.
	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	if _, werr := gzw.Write(all); werr != nil {
		_ = gzw.Close()
		return rawLowerBound()
	}
	if cerr := gzw.Close(); cerr != nil {
		return rawLowerBound()
	}
	return int64(buf.Len())
}

// applyPrunePlan performs the writes a PrunePlan describes: append ToMove to
// the compacted gz layer, atomically rewrite the active log with ToKeep, and
// grace-delete the compacted file when the plan says so. It is the apply half
// of the Plan + apply split; the behavior matches the pre-refactor
// PruneArtifact exactly.
func applyPrunePlan(store *acf.Store, kind acf.Kind, artifactID string, plan PrunePlan) (movedCount, deletedCount int, err error) {
	if len(plan.ToMove) == 0 {
		return 0, 0, nil
	}
	toMove := plan.ToMove
	toKeep := plan.ToKeep

	// Append toMove to the compacted gz file.
	compactedDir := filepath.Join(store.Root, "events", ".compacted", kindDirName(kind))
	if mkErr := os.MkdirAll(compactedDir, 0o755); mkErr != nil {
		return 0, 0, fmt.Errorf("retention: mkdir compacted: %w", mkErr)
	}
	compactedPath := filepath.Join(compactedDir, artifactID+".jsonl.gz")

	// Read existing compacted (if any) so we can re-write the whole file
	// (gzip streams don't support trivial append-without-rewrite). Shared with
	// the dry-run estimate (predictCompactedBytes) so the bytes apply writes
	// and the bytes the report predicts come from one encoding.
	existingBytes, oerr := readExistingCompactedLines(compactedPath)
	if oerr != nil {
		return 0, 0, fmt.Errorf("retention: open compacted for append: %w", oerr)
	}

	newLines, mErr := marshalEventLines(toMove)
	if mErr != nil {
		return 0, 0, fmt.Errorf("retention: marshal event for compacted: %w", mErr)
	}
	all := append(existingBytes, newLines...)

	// Gzip + atomic write via tmp + rename.
	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	if _, werr := gzw.Write(all); werr != nil {
		_ = gzw.Close()
		return 0, 0, fmt.Errorf("retention: gzip write: %w", werr)
	}
	if cerr := gzw.Close(); cerr != nil {
		return 0, 0, fmt.Errorf("retention: gzip close: %w", cerr)
	}
	if werr := atomicfile.WriteFile(compactedPath, buf.Bytes(), 0o644); werr != nil {
		return 0, 0, fmt.Errorf("retention: write compacted: %w", werr)
	}

	// Rewrite the active events file with toKeep.
	activeBytes, mErr := marshalEventLines(toKeep)
	if mErr != nil {
		return 0, 0, fmt.Errorf("retention: marshal event for active rewrite: %w", mErr)
	}
	activePath := filepath.Join(store.Root, "events", kindDirName(kind), artifactID+".jsonl")
	if werr := atomicfile.WriteFile(activePath, activeBytes, 0o644); werr != nil {
		return 0, 0, fmt.Errorf("retention: rewrite active events: %w", werr)
	}

	moved := len(toMove)

	// Grace delete: the plan already decided whether the freshly-written
	// compacted file would be older than graceDeadline (PlanPruneArtifact
	// captured the same reference instant apply stamps on the write). Obeying
	// the plan's decision verbatim — rather than re-stat'ing — guarantees
	// plan/apply parity. The original logic deleted iff the just-written
	// file's mtime was before graceDeadline, which this reproduces. The
	// just-written file is fresh, so DeleteCompacted is true only when the
	// caller passes a deadline in the future (as the grace-boundary tests do),
	// or in production once a later prune sees the file past its grace window.
	if plan.DeleteCompacted {
		rerr := os.Remove(compactedPath)
		if rerr == nil {
			return moved, 1, nil
		}
		// A failed grace-delete is NOT a hard error (the move already
		// succeeded and the active log was rewritten), but a swallowed
		// failure means a promised reclaim silently didn't happen. Surface it
		// like the FR-03.13 EACCES/EBUSY path so an unremovable compacted file
		// is observable instead of disappearing into a deletedCount=0 with no
		// signal.
		slog.Warn("retention: grace-delete of compacted file failed",
			"kind", kind, "artifact", artifactID, "path", compactedPath, "err", rerr)
	}
	return moved, 0, nil
}

// maxAncestorWalk bounds each per-branch ParentHash walk. It is a cycle /
// runaway guard only; a real artifact's chain can never exceed the number of
// events in its merged (active + compacted) log, which is the loop bound we
// actually rely on. The constant is a defensive ceiling far above any
// plausible per-branch depth.
const maxAncestorWalk = 1 << 30

// collectProtectedAncestors returns the set of event hashes (per FR-03.17)
// that must NOT be moved out of the active log because they sit in the
// ancestry of a LIVE SIDE-BRANCH head.
//
// Only non-main branch tips seed the walk. The main branch's own pre-snapshot
// events are precisely what pruning is meant to compact — the snapshot is the
// checkpoint that supersedes them for main-branch replay, so protecting main's
// head ancestry would defeat pruning entirely (and regress the no-fork case).
// A side branch is different: it forked off a (possibly pre-snapshot) trunk
// event and has no snapshot of its own, so its entire ancestry — including the
// shared trunk events from the fork point back to genesis — must survive.
//
// Side-branch tips come primarily from acf.Artifact.BranchHeads (authoritative;
// AppendEvent maintains it on every append). The branch index is consulted via
// the READ-ONLY LoadBranchIndex (never RefreshBranchIndex, which would persist
// the index) so this path — and the dry-run/Plan path that calls it — performs
// NO writes; it contributes each non-main branch's Head and its ForkedFromHash
// (the shared trunk ancestor at the fork point). A stale or absent cache only
// ever over-protects, which is safe — BranchHeads is the authoritative source.
//
// For each tip the ParentHash chain is walked backward through the MERGED
// active+compacted event set until an already-protected event or genesis is
// reached. A one-time hash→event index makes parent lookups O(1); missing
// parents (already compacted-and-deleted, or genesis) and cycles terminate
// the walk safely.
func collectProtectedAncestors(store *acf.Store, kind acf.Kind, artifactID string) (map[string]bool, error) {
	merged, err := store.ReadEventsIncludingCompacted(kind, artifactID)
	if err != nil {
		return nil, fmt.Errorf("read merged events: %w", err)
	}
	byHash := make(map[string]acf.Event, len(merged))
	for _, e := range merged {
		byHash[e.Hash] = e
	}

	tips := map[string]struct{}{}
	addTip := func(h string) {
		if h != "" {
			tips[h] = struct{}{}
		}
	}

	art, err := store.ReadArtifact(kind, artifactID)
	if err != nil {
		return nil, fmt.Errorf("read artifact: %w", err)
	}
	for branch, head := range art.BranchHeads {
		if branch == acf.MainBranch {
			continue
		}
		addTip(head)
	}

	// Corroborate with the branch index: each non-main branch's Head plus
	// its ForkedFromHash (the trunk ancestor at the fork point).
	if bi, berr := store.LoadBranchIndex(kind, artifactID); berr == nil {
		for name, info := range bi.Branches {
			if info == nil || name == acf.MainBranch {
				continue
			}
			addTip(info.Head)
			addTip(info.ForkedFromHash)
		}
	}

	protected := map[string]bool{}
	for tip := range tips {
		h := tip
		for steps := 0; h != "" && steps < maxAncestorWalk; steps++ {
			if protected[h] {
				break // joined an already-walked ancestry
			}
			e, ok := byHash[h]
			if !ok {
				break // parent already compacted-and-deleted, or genesis
			}
			protected[h] = true
			h = e.ParentHash
		}
	}
	return protected, nil
}

// kindDirName mirrors acf.kindDir (which is unexported). The mapping is
// kept local so retention doesn't depend on an unexported helper across
// packages. The two implementations must stay in sync.
func kindDirName(k acf.Kind) string {
	switch k {
	case acf.KindMemory:
		return "memories"
	case acf.KindSkill:
		return "skills"
	case acf.KindTool:
		return "tools"
	case acf.KindConversation:
		return "conversations"
	}
	return string(k) + "s"
}
