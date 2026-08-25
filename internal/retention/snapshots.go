// Package retention implements BRD-03 §4.8 retention policy: snapshot
// creation (§4.8.1), on-snapshot pruning (§4.8.2), and integrity-preserving
// attachment eviction (§4.8.3). Pre-snapshot events are moved to
// <store>/events/.compacted/<kind>/<id>.jsonl.gz and deleted outright after
// the grace period.
//
// Attachment eviction is append-only: attachment bytes live
// content-addressed in internal/blobstore (never in the hashed event
// payload), EvictAttachments appends an EventTypeUpdate that sets each
// attachment's Evicted marker, and GCBlobs reclaims the bytes once no live
// event references them. acf.VerifyChain stays green across eviction.
//
// Out of scope here: the daemon disk-pressure trigger path and config/flag
// wiring (a later task), and bundle-time blob rehydration.
package retention

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/blobstore"
)

// AutomaticConversationSnapshotByteLimit bounds unattended full-state
// conversation snapshots. A snapshot must carry a complete materialized
// payload for post-prune recovery, so snapshotting a very large live agent
// transcript necessarily duplicates and re-encodes that entire transcript.
// Above this limit, cadence and time-based snapshotters skip the checkpoint;
// an operator can still request an explicit snapshot or use pressure GC.
const AutomaticConversationSnapshotByteLimit int64 = 8 * 1024 * 1024

// AutomaticSnapshotAllowed performs only bounded metadata checks. It avoids
// loading the event log or materializing conversation payloads. Non-
// conversation artifacts retain their existing automatic snapshot behavior.
func AutomaticSnapshotAllowed(store *acf.Store, artifact acf.Artifact) bool {
	if artifact.Kind != acf.KindConversation {
		return true
	}
	if artifact.SourcePath != "" {
		if info, err := os.Stat(artifact.SourcePath); err == nil && info.Mode().IsRegular() && info.Size() > AutomaticConversationSnapshotByteLimit {
			return false
		}
	}
	logBytes, err := store.EventLogSize(artifact.Kind, artifact.ArtifactID)
	return err == nil && logBytes <= AutomaticConversationSnapshotByteLimit
}

// CreateSnapshot reads all events for an artifact, computes a SHA-256 over
// the latest materialized payload (the most recent create or update event),
// and appends an EventTypeSnapshot event that CARRIES that materialized
// payload and sets SnapshotState to "sha256:<hex>" of it. Returns the event
// written.
//
// FR-02.32: the snapshot event encodes the full materialized state so a
// reader can reconstruct from snapshot + later events alone. This matters
// for the on-snapshot prune (§4.8.2): PruneArtifact moves every pre-snapshot
// create/update event into .compacted, leaving the snapshot as the first
// (and possibly only) event in the active log. An export/replay reader that
// walks only the active log would otherwise find no materialized payload and
// silently fail to re-materialize the artifact. Carrying the payload makes
// the snapshot a self-contained checkpoint the backward replay can decode and
// stop on.
//
// The SnapshotState hash still bounds replay cost — a future reader can
// replay events forward from the most recent snapshot and trust the
// snapshot's state matches the content hash of the carried payload.
//
// Hash/format note: snapshot events are immutable once written. A NEW
// snapshot carrying a payload simply hashes that payload into ITS OWN content
// (Event.Payload feeds ComputeHash verbatim); it does not retroactively
// change any existing event's hash, and the serialization of non-snapshot
// events is untouched (Event.Payload keeps its `json:"payload"` tag — no
// global omitempty change). VerifyChain remains green across
// import->snapshot->prune->export because ParentHash still chains the
// snapshot to the (now-compacted) pre-snapshot head, re-merged via
// Store.ReadEventsIncludingCompacted.
//
// Per ADR-0024 (Merkle hash chain), the snapshot event's ParentHash is set
// to the artifact's current HeadEventHash before AppendEvent extends the
// chain. After a subsequent PruneArtifact moves pre-snapshot events into
// .compacted, the snapshot remains the first event in the active log; its
// ParentHash still references the last pre-snapshot event's content hash
// (which lives in .compacted). Verification across the prune boundary uses
// Store.ReadEventsIncludingCompacted to re-merge the two layers.
func CreateSnapshot(ctx context.Context, store *acf.Store, kind acf.Kind, artifactID string) (acf.Event, error) {
	if err := ctx.Err(); err != nil {
		return acf.Event{}, err
	}
	art, err := store.ReadArtifact(kind, artifactID)
	if err != nil {
		return acf.Event{}, fmt.Errorf("retention: read artifact %s: %w", artifactID, err)
	}
	events, err := store.ReadEvents(kind, artifactID)
	if err != nil {
		return acf.Event{}, fmt.Errorf("retention: read events: %w", err)
	}
	if len(events) == 0 {
		return acf.Event{}, fmt.Errorf("retention: no events to snapshot for %s", artifactID)
	}

	// Find the latest materialized payload — that's the state at this point.
	// A payload-bearing snapshot is itself a valid checkpoint (FR-02.32), so
	// it counts as a materialized-payload source identically to create/update/
	// resolution: re-snapshotting a snapshot-only active log (the post-prune
	// case) carries the same materialized state forward rather than failing.
	// acf.LatestPayloadEvent performs that backward walk; the nil-payload guard
	// preserves the original behavior (a create/update/resolution with no payload
	// bytes is "nothing to snapshot", same as finding no such event at all).
	var latestPayload json.RawMessage
	if kind == acf.KindConversation {
		materialized, ok, perr := store.MaterializedConversationPayloadFromStore(artifactID)
		if perr != nil {
			return acf.Event{}, fmt.Errorf("retention: materialize conversation payload: %w", perr)
		}
		if !ok {
			return acf.Event{}, fmt.Errorf("retention: no create/update event found in %s log — nothing to snapshot", artifactID)
		}
		payload, eerr := acf.EncodePayload(materialized)
		if eerr != nil {
			return acf.Event{}, fmt.Errorf("retention: encode conversation snapshot payload: %w", eerr)
		}
		latestPayload = payload
	} else {
		src, ok := acf.LatestPayloadEvent(events)
		if !ok || src.Payload == nil {
			return acf.Event{}, fmt.Errorf("retention: no create/update event found in %s log — nothing to snapshot", artifactID)
		}
		latestPayload = src.Payload
	}
	sum := sha256.Sum256(latestPayload)
	stateHash := "sha256:" + hex.EncodeToString(sum[:])

	now := time.Now().UTC()
	snap := acf.Event{
		EventID:    acf.NewID(),
		ArtifactID: artifactID,
		Type:       acf.EventTypeSnapshot,
		// FR-02.32: carry the materialized payload so the snapshot is a
		// self-contained checkpoint a replay reader can reconstruct from
		// without the (possibly pruned) pre-snapshot events.
		Payload:       latestPayload,
		ParentHash:    art.HeadEventHash,
		SnapshotState: stateHash,
		Timestamp:     now,
	}
	if err := store.AppendEvent(kind, snap); err != nil {
		return acf.Event{}, fmt.Errorf("retention: append snapshot event: %w", err)
	}
	return snap, nil
}

// EvictOpts configures EvictAttachments.
type EvictOpts struct {
	// MinAge is how old an attachment slot must be before it is eligible for
	// eviction: both the source event naming it AND the slot's newest
	// assertion (latest-wins per ContentHash — see PlanEvictAttachments)
	// must be older than now-MinAge. Anything newer is untouched.
	MinAge time.Duration
	// Reason is recorded in each EvictedInfo marker (FR-03.20). Free-text;
	// e.g. "age" or "disk-pressure".
	Reason string
}

// EvictAttachments marks attachment blobs as evicted in a conversation
// artifact WITHOUT breaking the append-only hash chain (BRD-03 §4.8).
//
// The earlier implementation rewrote historical event payloads in place,
// which by construction broke acf.VerifyChain (a payload edit changes the
// event hash). The current model keeps attachment bytes out of the
// hashed payload — they are content-addressed in the blob store, and the
// event carries only Attachment.ContentHash. Eviction therefore never
// needs to touch a historical event. Instead, for each older event still
// holding non-evicted attachments, we APPEND a NEW EventTypeUpdate that
// re-asserts the same payload with each attachment's Evicted marker set
// (and its transient Data cleared). Every historical event hash — and thus
// VerifyChain over the whole log — is preserved.
//
// Algorithm:
//  1. Pin exemption: artifacts tagged "pinned"/"keep-forever" are skipped
//     (mirrors PruneArtifact), returning 0.
//  2. cutoff = now - opts.MinAge.
//  3. For each slot-asserting event (create/update/resolution or
//     payload-bearing checkpoint — see assertsAttachmentSlots) OLDER than
//     cutoff that holds at least one attachment whose CURRENT latest-wins
//     state is non-evicted and older than cutoff, build an updated payload
//     with each such attachment's Evicted set (At/Reason/OriginalSize=Bytes/
//     ContentHash) and Data cleared. Slots already evicted keep (or are
//     re-stamped with) their existing marker — see PlanEvictAttachments.
//  4. Append that payload as a NEW EventTypeUpdate via store.AppendEvent
//     (chained off the current head). No existing event is mutated.
//
// Blob deletion is NOT done here. A blob may still be referenced by a more
// recent event, so deletion is delegated to the live-set GC (GCBlobs),
// which only removes blobs no live event references and that are older than
// the grace window.
//
// Returns the number of attachments marked evicted (across all qualifying
// events).
//
// EvictAttachments is now a thin Plan + apply (FR-03.22/23): PlanEvictAttachments
// computes the eviction set and reclaimable bytes WITHOUT appending the marker
// event, and EvictAttachments appends one EventTypeUpdate per affected source
// event exactly as before. The split lets a later gc --dry-run report what
// would be evicted without mutating. Apply behavior is unchanged.
func EvictAttachments(ctx context.Context, store *acf.Store, blobs *blobstore.Store, kind acf.Kind, artifactID string, opts EvictOpts) (int, error) {
	plan, err := PlanEvictAttachments(ctx, store, kind, artifactID, opts)
	if err != nil {
		return 0, err
	}

	evicted := 0
	for _, ev := range plan.Events {
		art, herr := store.ReadArtifact(kind, artifactID)
		if herr != nil {
			return evicted, fmt.Errorf("retention: read artifact head: %w", herr)
		}
		head := art.HeadEventHash
		if art.BranchHeads != nil && art.BranchHeads[acf.MainBranch] != "" {
			head = art.BranchHeads[acf.MainBranch]
		}
		appendEvt := acf.Event{
			EventID:    acf.NewID(),
			ArtifactID: artifactID,
			Type:       acf.EventTypeUpdate,
			Timestamp:  ev.Timestamp,
			Payload:    ev.Payload,
			ParentHash: head,
		}
		if aerr := store.AppendEvent(kind, appendEvt); aerr != nil {
			return evicted, fmt.Errorf("retention: append eviction event: %w", aerr)
		}
		evicted += ev.AttachmentsEvicted
	}

	// blobs is accepted for symmetry with the GC path and future
	// inline-deletion policies; blob removal itself is delegated to
	// GCBlobs (live-set + grace window), so it is intentionally unused
	// here.
	_ = blobs
	return evicted, nil
}

// EvictEvent is one would-be eviction append: the EventTypeUpdate payload
// (re-asserting a source event's attachments with their Evicted markers set)
// that EvictAttachments would append, plus the count of attachments it marks
// and the bytes those attachments would reclaim.
type EvictEvent struct {
	// Timestamp is the timestamp the appended eviction event would carry
	// (the plan's reference instant). apply uses it verbatim.
	Timestamp time.Time
	// Payload is the pre-encoded ConversationPayload for the appended
	// EventTypeUpdate. apply chains it off the current head unchanged.
	Payload json.RawMessage
	// AttachmentsEvicted is how many attachments this event marks evicted.
	AttachmentsEvicted int
	// BytesReclaimable is the sum of the OriginalSize of the attachments
	// this event marks evicted.
	BytesReclaimable int64
}

// EvictPlan is the no-write result of PlanEvictAttachments: the ordered set of
// eviction-update events EvictAttachments would append, the total attachments
// marked, and the total bytes reclaimable. It is the source of truth the apply
// path executes verbatim.
type EvictPlan struct {
	// Events is the ordered set of eviction appends, one per source event
	// that holds at least one not-yet-evicted attachment past the min-age
	// cutoff. Empty when nothing would be evicted (pinned, or no qualifying
	// attachments).
	Events []EvictEvent
	// AttachmentsEvicted is the total attachments marked across all events.
	AttachmentsEvicted int
	// BytesReclaimable is the total bytes the eviction would make collectible
	// (summed attachment OriginalSize).
	BytesReclaimable int64
}

// PlanEvictAttachments computes — WITHOUT writing — exactly what
// EvictAttachments would append for one artifact: for each create/update/
// resolution or payload-bearing checkpoint (snapshot/baseline) event older
// than now-MinAge that still holds a non-evicted attachment, the re-asserted
// payload with each such attachment's Evicted marker set and its transient
// Data cleared. Pin-tag exemption and the min-age cutoff are computed
// identically to apply.
//
// No event is appended; the returned EvictPlan.Events carries the pre-encoded
// payloads apply will chain onto the head in order, so plan and apply can
// never diverge on which attachments are marked or how many bytes are freed.
func PlanEvictAttachments(ctx context.Context, store *acf.Store, kind acf.Kind, artifactID string, opts EvictOpts) (EvictPlan, error) {
	if err := ctx.Err(); err != nil {
		return EvictPlan{}, err
	}
	art, err := store.ReadArtifact(kind, artifactID)
	if err != nil {
		return EvictPlan{}, fmt.Errorf("retention: read artifact: %w", err)
	}
	for _, tag := range art.Tags {
		if tag == "pinned" || tag == "keep-forever" {
			return EvictPlan{}, nil
		}
	}

	events, err := store.ReadEvents(kind, artifactID)
	if err != nil {
		return EvictPlan{}, fmt.Errorf("retention: read events: %w", err)
	}
	cutoff := time.Now().UTC().Add(-opts.MinAge)
	now := time.Now().UTC()

	// Resolve the CURRENT state of every attachment slot first — latest-wins
	// per ContentHash within this artifact. NOTE the resolution ORDER differs
	// from LiveBlobSet's: this planner walks ReadEvents (append order) while
	// LiveBlobSet walks ReadEventsIncludingCompacted (timestamp order), so under
	// non-monotonic clocks the two can disagree on which assertion of a
	// ContentHash is "newest". That is GC-SAFE in the only direction that
	// matters: an eviction event is stamped `now` (below), so it is BOTH the
	// append-last and the timestamp-last assertion of its slot — a slot this
	// planner considers evicted, LiveBlobSet also considers evicted, so the
	// planner→GC handoff can never delete a blob a live event still references
	// (the reverse, brief over-retention, is harmless). The eviction decision
	// keys off this resolved state, NOT off each immutable source payload alone:
	//
	//   - a slot whose newest assertion is ALREADY evicted (e.g. by a prior
	//     sweep's eviction append) must not be re-planned — the append-only
	//     source payload keeps naming it non-evicted forever, so re-planning
	//     it would append a duplicate eviction event on EVERY periodic sweep,
	//     growing the event log without bound;
	//   - a slot re-asserted non-evicted by a RECENT event is protected by
	//     MinAge even when an OLD source event also names it — marking it via
	//     the old source would flip the slot evicted at the head and let GC
	//     collect a blob the recent event still references.
	//
	// The newest assertion's own EvictedInfo is kept so a re-asserted payload
	// can re-stamp (never resurrect) slots evicted elsewhere — see below.
	type slotState struct {
		evicted *acf.EvictedInfo // non-nil when the newest assertion is evicted
		ts      time.Time        // timestamp of the newest asserting event
	}
	currentSlot := map[string]slotState{}
	for _, e := range events {
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
			currentSlot[att.ContentHash] = slotState{evicted: att.Evicted, ts: e.Timestamp}
		}
	}
	// Slots whose newest assertion is evicted — on disk, or marked earlier in
	// THIS plan. Re-asserted payloads must re-stamp these markers rather than
	// re-assert the slot non-evicted (which would resurrect, under
	// latest-wins, a slot whose blob may already be GC'd).
	alreadyEvicted := map[string]*acf.EvictedInfo{}
	for hash, s := range currentSlot {
		if s.evicted != nil {
			alreadyEvicted[hash] = s.evicted
		}
	}

	var plan EvictPlan
	for _, e := range events {
		// Checkpoint payloads (snapshot/baseline) are eviction sources too:
		// after an on-snapshot prune (or a baseline adoption) the checkpoint
		// can be the ONLY event asserting a ContentHash, and the live-set GC
		// keeps checkpoint-referenced blobs alive — skipping checkpoints here
		// would make those attachment bytes permanently unreclaimable.
		if !assertsAttachmentSlots(e) {
			continue
		}
		if !e.Timestamp.Before(cutoff) {
			continue // recent, keep
		}
		var p acf.ConversationPayload
		if jerr := json.Unmarshal(e.Payload, &p); jerr != nil {
			continue // non-conversation or malformed payload — leave alone
		}
		if len(p.Attachments) == 0 {
			continue
		}
		changed := 0
		var bytesReclaimable int64
		for i := range p.Attachments {
			if p.Attachments[i].IsEvicted() {
				continue
			}
			hash := p.Attachments[i].ContentHash
			if hash == "" {
				// No blob slot to evict (ContentHash is contractually always
				// populated; tolerate a malformed payload rather than marking
				// a slot that names no blob).
				continue
			}
			if info := alreadyEvicted[hash]; info != nil {
				// Newest assertion already evicted: re-stamp the existing
				// marker (don't count it again) so this re-asserted payload
				// cannot resurrect the slot.
				p.Attachments[i].Evicted = info
				p.Attachments[i].Data = nil
				continue
			}
			if cur, ok := currentSlot[hash]; !ok || !cur.ts.Before(cutoff) {
				continue // newest assertion is recent — protected by MinAge
			}
			att := p.Attachments[i]
			info := &acf.EvictedInfo{
				At:           now,
				Reason:       opts.Reason,
				OriginalSize: att.Bytes,
				ContentHash:  att.ContentHash,
			}
			p.Attachments[i].Evicted = info
			p.Attachments[i].Data = nil
			alreadyEvicted[hash] = info
			changed++
			bytesReclaimable += att.Bytes
		}
		if changed == 0 {
			continue
		}
		newPayload, encErr := acf.EncodePayload(p)
		if encErr != nil {
			return EvictPlan{}, fmt.Errorf("retention: encode evicted payload: %w", encErr)
		}
		plan.Events = append(plan.Events, EvictEvent{
			Timestamp:          now,
			Payload:            newPayload,
			AttachmentsEvicted: changed,
			BytesReclaimable:   bytesReclaimable,
		})
		plan.AttachmentsEvicted += changed
		plan.BytesReclaimable += bytesReclaimable
	}
	return plan, nil
}
