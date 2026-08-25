package syncd

import (
	"path/filepath"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
)

// The origin-session repair trigger.
//
// THE HOLE IT CLOSES. When a conversation's own agent appends at a stale
// in-memory leaf (the still-open TUI case), the file's parentUuid graph forks.
// The agent's own import commits the new turns fine — canonical converges —
// but fan-out same-source-excludes the origin adapter, so no materialize
// attempt is ever made toward the forked file. No attempt means no decline, no
// queue entry, no diverged-nudge, and no flag-gated repair: every repair stage
// downstream exists and is tested, and nothing ever started it. The transcript
// stayed frozen at the fork's own branch until an unrelated FOREIGN turn
// happened to arrive — silent, and demanding user action, which violates both
// owner requirements at once.
//
// THE TRIGGER IS EVIDENCE, NOT A TIMER. Immediately after an import of the
// artifact's own source file, the source adapter is asked — read-only, via
// adapter.ConversationSessionSourceInspector — whether that file still
// presents the post-import canonical head on its own resume walk. A file that
// cannot (forked, chain-unspanned, or turn-order diverged) is handed to the
// EXISTING deferral queue with includePrimary=true, which is the one flag that
// bypasses fan-out's same-source exclusion. Everything after that is the
// machinery that already exists: the drain's paced attempt, the typed decline,
// the flag-gated repair, escalation, and the rate caps. Nothing here builds a
// parallel path.
//
// Repair-pass budget (design rule 3): this trigger performs ZERO adapter
// writes and charges NOTHING to the quarantine breaker. It reads one plan and
// one file — bytes the import has just paid to read — and at most enqueues.
// The drain it wakes was always allowed to run; a conversation-session decline
// is a typed refusal, not an Export failure, so it feeds the breaker nothing
// either (QuarantineTracker.RecordFailure's only call site is fanOut's Export
// arm, which conversation session plans never reach).

// originSessionRepairTriggerReason names the inspector classifications the
// trigger acts on. All three mean "the file cannot come to present the
// canonical conversation through the ordinary routes", which is exactly the
// population the flag-gated rebuild addresses and the escalation surface must
// name when the flag is off.
//
// Deliberately NOT here: race is a writer mid-append and resolves itself;
// native_ahead is transient and the file's own pending import is the
// authority; graph_malformed means the file could not be authenticated as this
// conversation at all, so queueing a write toward it would retry forever
// against a file Aplexica may not touch.
func originSessionRepairTriggerReason(reason adapter.SessionDeclineReason) bool {
	switch reason {
	case adapter.SessionDeclineDiverged,
		adapter.SessionDeclineForkedMirror,
		adapter.SessionDeclineChainUnspanned:
		return true
	default:
		return false
	}
}

// originSessionQueueState reports whether (agent, artifact) is already queued,
// whether a give-up (needs_attention) record stands for it, and whether the
// queued request already has the exact origin-repair routing semantics.
func (o *Orchestrator) originSessionQueueState(agent, artifactID string) (queued, givenUp, repairReady bool) {
	o.deferredMaterializeMu.Lock()
	defer o.deferredMaterializeMu.Unlock()
	queue := o.deferredMaterialize[agent]
	if queue == nil {
		return false, false, false
	}
	entry, queued := queue.entries[artifactID]
	repairReady = queued && entry.includePrimary && !entry.mirrorsOnly && entry.originAgent == agent
	for _, record := range queue.abandoned {
		if record.artifactID == artifactID {
			return queued, true, repairReady
		}
	}
	return queued, false, repairReady
}

// queueOriginSessionRepairs is the post-import origin-session check. It runs
// after handleEventWithDisposition has finished importing a changed file —
// including a disposition no-op, because the fork's later imports commit
// nothing new while the file stays broken — and never on the
// scanCache-unchanged short circuit, so an idle daemon pays nothing.
//
// A standing give-up record suppresses the trigger: re-admitting an escalated
// write is the convergence sweep's job, under its dwell rules, and an import
// hook that resurrected it on every edit would refill the escalation budget
// the record exists to spend once.
func (o *Orchestrator) queueOriginSessionRepairs(primary adapter.Adapter, path string, ids []string) {
	if o == nil || primary == nil || path == "" || len(ids) == 0 || o.cfg.Store == nil {
		return
	}
	inspector, ok := primary.(adapter.ConversationSessionSourceInspector)
	if !ok {
		return
	}
	agent := primary.Name()
	cleanPath := filepath.Clean(path)
	for _, id := range ids {
		art, found := o.findArtifact(id)
		if !found || art.Kind != acf.KindConversation || art.SourcePath == "" {
			continue
		}
		// Only the artifact whose OWN source file this import consumed: that
		// is the file fan-out's same-source exclusion shields from every other
		// trigger. Remote shells never have a local origin session.
		if art.RemoteOriginDeviceID != "" || filepath.Clean(art.SourcePath) != cleanPath {
			continue
		}
		// A queued origin-repair entry already owns the retry lifecycle, so the
		// trigger has nothing to add and must not spend the inspection. A queued
		// FOREIGN write is different: it may have includePrimary=false and the
		// wrong origin, so it cannot repair this source session. Inspect that case
		// once and let deferMaterialization widen the existing request below.
		// The fresh-request
		// semantics a re-defer would provide are already provided upstream:
		// reopenDeferredMaterializationForDest runs on every import of this
		// destination, just before this hook, and retracts the drain's
		// unchanged-inputs short circuit. Re-deferring here would ALSO retract
		// it — by resetting declineObserved — which turned every native edit of
		// a flag-off fork into a full second inspection here plus a forced real
		// adapter attempt at the drain's next paced slot, for as long as the
		// fork lived. Sustained, yield-free burn the short circuit exists to
		// avoid. And a standing give-up record suppresses the trigger too (see
		// the function comment).
		if _, givenUp, repairReady := o.originSessionQueueState(agent, art.ArtifactID); givenUp || repairReady {
			continue
		}
		head, hasHead, err := conversationHeadForBranch(
			o.cfg.Store, art.ArtifactID, selectedBranchForAgent(art, agent))
		if err != nil || !hasHead {
			continue
		}
		reusable, applicable, reason, ierr := inspector.InspectConversationSessionSource(art, head)
		if ierr != nil || !applicable || reusable || !originSessionRepairTriggerReason(reason) {
			continue
		}
		if o.cfg.Logger != nil {
			o.cfg.Logger.Info("origin session cannot present the canonical conversation; queued its repair",
				"agent", agent, "artifact_id", art.ArtifactID,
				"reason", string(reason), "path", redactPaths(path))
		}
		// includePrimary=true is the whole point: it is the one flag that lets
		// the drain's fan-out reach the origin adapter past the same-source
		// exclusion. The origin is the agent itself — this is its own file.
		o.deferMaterialization(agent, art.ArtifactID, agent, true, false, true)
	}
}
