package syncd

import (
	"context"
	"sort"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
)

// Continuous convergence.
//
// Sync was a pipeline — import, publish, receive, materialize — in which each
// stage could drop work permanently. Desired state (canonical store x eligible
// agents) is knowable at all times, but nothing continuously compared it to
// what is actually on disk, so a write lost to a closed gate, an exhausted
// retry budget or a missed watcher event stayed lost until a human noticed.
//
// This sweep closes that loop by returning budget-exhausted writes to the
// existing retry queue, a couple at a time. It deliberately does NOT
// materialize anything itself: the drain loop already has backoff,
// withheld-vs-charged accounting and quarantine awareness, and a second
// materialization engine would fight it.
//
// Two approaches were tried and rejected, both for reasons worth keeping:
//
//   - reconcileDeferredMaterializationTarget is fail-fast: it aborts on the
//     first per-artifact error, and a legitimately withheld artifact (a rule
//     that does not route it, an unresolved conflict, an unregistered project)
//     produces exactly such an error. Since artifacts are ordered by
//     CreatedAt, one denied artifact aborted every sweep at the same position
//     while reporting success. Under selective sync that is the normal state,
//     so coverage was near zero — silently.
//   - RefanOutAll re-fans out the whole store. With pre-existing failures it can
//     trip the quarantine breaker for every adapter during the first sweep.
//     Quarantine blocks all materialization including live sync, so the device
//     would have cycled between quarantined and briefly healthy forever. The
//     self-heal made the device measurably worse than no self-heal at all.
//
// The lesson both share: a repair pass must be SMALLER than the failure
// budget of the systems it drives, or it becomes an outage generator.
//
// R5 (resource-bounded) is met by never sweeping a quiescent device. The tick
// is cheap and, when nothing has changed and nothing is owed, does no work at
// all — see convergenceWorthSweeping. That predicate is the whole cost story:
// the sweep itself is the expensive part and it is skipped by default.

const (
	// convergenceTickInterval is how often the daemon CHECKS whether a sweep
	// is warranted. The check is a metadata-only fingerprint, not a sweep.
	convergenceTickInterval = 5 * time.Minute

	// convergenceSweepMinInterval floors the gap between two actual sweeps
	// even while drift keeps being found, so a persistently broken target
	// cannot turn convergence into a hot loop. This is the R2 guarantee at
	// the sweep layer: bounded work per unit time, always.
	convergenceSweepMinInterval = 15 * time.Minute

	// convergenceSweepMaxInterval caps the backoff on a converged device, so
	// drift introduced by something the daemon never observed (an external
	// edit, a restore, a partial disk failure) is still found within a bounded
	// window rather than never.
	convergenceSweepMaxInterval = 6 * time.Hour
)

// convergenceFingerprint is the cheap "has anything changed?" summary of the
// canonical store. It reads artifact metadata only and never opens an event
// log or a native file.
type convergenceFingerprint struct {
	artifacts int
	latest    time.Time
}

func (f convergenceFingerprint) equal(other convergenceFingerprint) bool {
	return f.artifacts == other.artifacts && f.latest.Equal(other.latest)
}

// convergenceState is the sweep scheduler's memory. Guarded by o.mu.
type convergenceState struct {
	lastFingerprint convergenceFingerprint
	lastSweepAt     time.Time
	nextInterval    time.Duration
	everSwept       bool
	// originSessionVerdicts memoizes, per artifact, the exact (source-file
	// fingerprint, canonical head hash) the last no-enqueue origin-session
	// inspection judged. While both stand, re-inspecting could only repeat the
	// verdict, so the candidate is skipped without charging the sweep's parse
	// budget — that skip is what lets the fixed per-sweep budget cover a
	// moving window instead of the same top-16 forever (see the budget
	// arithmetic at queueForkedOriginSessions). Memory-only by design: a
	// restart merely re-inspects, which fails toward spending IO, never
	// toward missing a fork.
	originSessionVerdicts map[string]originSessionInspectMemo
}

// originSessionInspectMemo is one memoized no-enqueue verdict. The fingerprint
// is captured BEFORE the inspection reads the file, so bytes appended mid-read
// leave a memo that no longer matches the file and force a re-inspection —
// the failure mode is a wasted parse, never a trusted stale verdict.
type originSessionInspectMemo struct {
	fp   scanFP
	head string
}

// originSessionVerdictUnchanged reports whether the artifact's last memoized
// no-enqueue verdict still vouches for exactly this (file, head) state.
func (o *Orchestrator) originSessionVerdictUnchanged(artifactID string, fp scanFP, headHash string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	memo, ok := o.convergence.originSessionVerdicts[artifactID]
	return ok && memo.fp == fp && memo.head == headHash
}

// recordOriginSessionVerdict memoizes a no-enqueue verdict for the (file,
// head) state it judged. Enqueued candidates are deliberately never recorded:
// their state is owned by the queue entry until the write retires it, and the
// repair rewrites the file anyway, which retracts any older memo by moving
// the fingerprint.
func (o *Orchestrator) recordOriginSessionVerdict(artifactID string, fp scanFP, headHash string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.convergence.originSessionVerdicts == nil {
		o.convergence.originSessionVerdicts = map[string]originSessionInspectMemo{}
	}
	o.convergence.originSessionVerdicts[artifactID] = originSessionInspectMemo{fp: fp, head: headHash}
}

// storeFingerprint summarizes the canonical store without reading event logs.
// An error on any kind returns ok=false, and the caller then declines to make
// a "nothing changed" claim it cannot support — failing toward sweeping
// rather than toward silence.
func (o *Orchestrator) storeFingerprint() (convergenceFingerprint, bool) {
	if o.cfg.Store == nil {
		return convergenceFingerprint{}, false
	}
	var fp convergenceFingerprint
	for _, kind := range []acf.Kind{acf.KindMemory, acf.KindSkill, acf.KindTool, acf.KindConversation} {
		arts, err := o.cfg.Store.ListArtifacts(kind)
		if err != nil {
			return convergenceFingerprint{}, false
		}
		fp.artifacts += len(arts)
		for _, art := range arts {
			if art.UpdatedAt.After(fp.latest) {
				fp.latest = art.UpdatedAt
			}
		}
	}
	return fp, true
}

// convergenceOwedWork reports whether any target has writes it still owes —
// entries mid-retry, coalesced overflow reconciliations, or writes that
// exhausted their budget and are waiting for something to drive them again.
// The last group is the one that matters most: those never self-heal on their
// own, and before this sweep existed nothing revisited them.
func (o *Orchestrator) convergenceOwedWork() bool {
	o.deferredMaterializeMu.Lock()
	defer o.deferredMaterializeMu.Unlock()
	for _, queue := range o.deferredMaterialize {
		if queue == nil {
			continue
		}
		if len(queue.entries) > 0 || queue.overflow || len(queue.abandoned) > 0 {
			return true
		}
	}
	return false
}

// convergenceWorthSweeping decides whether to spend a sweep. This is where R5
// is enforced: a device whose store has not changed and which owes no writes
// does nothing at all, no matter how long the daemon runs.
//
// It returns true when the store changed since the last sweep, when some
// target still owes work, or when the backoff ceiling has elapsed (so drift
// the daemon never observed is still eventually found).
func (o *Orchestrator) convergenceWorthSweeping(now time.Time) (bool, convergenceFingerprint, bool) {
	fp, fpOK := o.storeFingerprint()

	o.mu.Lock()
	state := o.convergence
	o.mu.Unlock()

	// Never swept: the daemon's own startup reconciliation covers first boot,
	// so record the baseline and wait rather than duplicating that work.
	if !state.everSwept {
		return false, fp, fpOK
	}
	if !state.lastSweepAt.IsZero() && now.Sub(state.lastSweepAt) < convergenceSweepMinInterval {
		return false, fp, fpOK
	}
	if o.convergenceOwedWork() {
		return true, fp, fpOK
	}
	if !fpOK || !fp.equal(state.lastFingerprint) {
		// Store changed (or could not be summarized): sweep.
		return true, fp, fpOK
	}
	interval := state.nextInterval
	if interval <= 0 {
		interval = convergenceSweepMinInterval
	}
	return now.Sub(state.lastSweepAt) >= interval, fp, fpOK
}

// noteConvergenceSweep records the outcome and sets the next backoff. Finding
// drift tightens the interval to the floor; a clean sweep doubles it toward
// the ceiling, so a healthy device converges toward doing almost nothing.
func (o *Orchestrator) noteConvergenceSweep(now time.Time, fp convergenceFingerprint, fpOK, foundDrift bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.convergence.lastSweepAt = now
	o.convergence.everSwept = true
	if fpOK {
		o.convergence.lastFingerprint = fp
	}
	if foundDrift {
		o.convergence.nextInterval = convergenceSweepMinInterval
		return
	}
	next := o.convergence.nextInterval * 2
	if next < convergenceSweepMinInterval {
		next = convergenceSweepMinInterval
	}
	if next > convergenceSweepMaxInterval {
		next = convergenceSweepMaxInterval
	}
	o.convergence.nextInterval = next
}

// markConvergenceBaseline records the store fingerprint after the daemon's own
// startup reconciliation, so the first periodic tick does not immediately
// repeat work that just ran.
func (o *Orchestrator) markConvergenceBaseline(now time.Time) {
	fp, fpOK := o.storeFingerprint()
	o.mu.Lock()
	defer o.mu.Unlock()
	o.convergence.everSwept = true
	o.convergence.lastSweepAt = now
	o.convergence.nextInterval = convergenceSweepMinInterval
	if fpOK {
		o.convergence.lastFingerprint = fp
	}
}

// RunConvergence is the daemon's periodic self-heal loop. It returns when ctx
// is done or the orchestrator closes.
//
// It registers with beginBackground so Close() joins it: a sweep writes native
// agent files, and Close promises that no orchestrator goroutine is still
// touching watched roots once it returns. It also selects on bgDone, because
// Close can happen while the caller's context is still live — without that the
// loop would keep walking the store every tick against a closed orchestrator.
func (o *Orchestrator) RunConvergence(ctx context.Context) {
	if o == nil || o.cfg.Store == nil {
		return
	}
	if !o.beginBackground() {
		return
	}
	defer o.endBackground()

	o.markConvergenceBaseline(time.Now().UTC())
	ticker := time.NewTicker(convergenceTickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-o.bgDone:
			return
		case <-ticker.C:
		}
		o.convergenceSweepOnce(ctx, time.Now().UTC())
	}
}

// convergenceReadmitPerSweep bounds how many budget-exhausted writes one sweep
// returns to the retry queue.
//
// This bound is load-bearing, not cosmetic. The first version of this sweep
// drove RefanOutAll — a whole-store fan-out — and with pre-existing failures it
// could re-hit those failures fast enough to trip the quarantine breaker for
// every adapter. Quarantine blocks all materialization, including live sync;
// with a periodic sweep the device
// would have cycled between quarantined and briefly healthy indefinitely. The
// self-heal made the device worse.
//
// Re-admitting a small batch keeps each sweep's failure count under the
// breaker's threshold, so a genuinely broken write is retried steadily instead
// of taking every adapter down with it.
const convergenceReadmitPerSweep = 2

// convergenceReadmitMinDwell is how long a give-up record must have stood
// before this sweep will drive it again.
//
// It exists because the sweep and the escalation surface were fighting each
// other. Deletion capacity here is 2 per sweep on a 15-minute floor — 192 a day
// — against a creation cap of 3 per rolling 24h, a 64x mismatch, so a
// needs_attention row was auto-retired long before any human could act on one.
// Requirement (a) asks for a raised flag; raising one and having a sibling
// subsystem lower it, with the condition unresolved, is not raising it.
//
// A day of dwell also makes the sweep's job the one it was designed for: drive
// writes that nothing else will, rather than churn the ones the live fan-out
// already re-defers on every commit.
const convergenceReadmitMinDwell = 24 * time.Hour

// readmitStuckMaterializations returns budget-exhausted writes to the retry
// queue, a few at a time, and reports how many were re-admitted.
//
// This is the whole repair action. It deliberately does NOT re-materialize
// anything itself: it hands the work back to the existing drain loop, which
// already has exponential backoff, withheld-vs-charged accounting and
// quarantine awareness. Reusing that path is what keeps the sweep from
// becoming a second, competing materialization engine.
//
// It also deliberately does NOT retire the give-up record. Re-admission means
// "we are going to try this again", not "this is fixed"; the record is retired
// by a SUCCESSFUL write (see runDeferredMaterializationDrain) or by an
// operator. deferMaterialization carries the record's escalation stamp onto the
// fresh entry, so a re-admitted write that declines for the same reason returns
// to the standing set without spending a new escalation.
func (o *Orchestrator) readmitStuckMaterializations(now time.Time) int {
	type readmission struct {
		agent      string
		artifactID string
		origin     string
		// includePrimary and mirrorsOnly restore the WRITE the record stands
		// for. The origin-session population (origin == agent) exists at all
		// only through includePrimary — re-admitting it without the flag would
		// plan zero writes, report success, and retire the record vacuously.
		includePrimary bool
		mirrorsOnly    bool
	}
	var picks []readmission

	o.deferredMaterializeMu.Lock()
	for agent, queue := range o.deferredMaterialize {
		if queue == nil {
			continue
		}
		for _, record := range queue.abandoned {
			// Whole-target give-up records carry no artifact id; they are
			// re-driven by the overflow path, not per-artifact re-admission.
			if record.artifactID == "" {
				continue
			}
			// Skip anything already back in the queue: re-admitting a pair
			// that is mid-retry would reset its budget and could keep it
			// retrying forever, which is exactly what must not happen.
			if _, queued := queue.entries[record.artifactID]; queued {
				continue
			}
			if !record.abandonedAt.IsZero() && now.Sub(record.abandonedAt) < convergenceReadmitMinDwell {
				continue
			}
			picks = append(picks, readmission{
				agent: agent, artifactID: record.artifactID, origin: record.originAgent,
				includePrimary: record.includePrimary, mirrorsOnly: record.mirrorsOnly,
			})
			if len(picks) >= convergenceReadmitPerSweep {
				break
			}
		}
		if len(picks) >= convergenceReadmitPerSweep {
			break
		}
	}
	o.deferredMaterializeMu.Unlock()

	for _, pick := range picks {
		o.deferMaterialization(pick.agent, pick.artifactID, pick.origin,
			pick.includePrimary, pick.mirrorsOnly, false)
	}
	return len(picks)
}

// Origin-session fork pickup: the sweep half of the origin-session repair
// trigger (see origin_session_repair.go for the import half and the shared
// predicate). T1 fires on IMPORT of a forked file, so a file forked before the
// trigger shipped — or whose fork import raced a crash or a wiped journal —
// would heal only on its next native edit. Each sweep therefore examines
// recently-updated conversations whose SourcePath owner is a
// conversation-session target and queues the ones whose origin session can no
// longer present the canonical head, bounded, newest first.
//
// Budget arithmetic (design rule 3 — a repair pass must stay SMALLER than the
// failure budget of the systems it drives; the quarantine breaker is 3
// failures / 10 minutes per adapter and blocks ALL materialization):
//
//   - Enqueues per sweep ≤ convergenceOriginSessionQueuePerSweep = 2 —
//     consistent with convergenceReadmitPerSweep = 2 and under the breaker's 3
//     on its own. And these writes cannot charge the breaker AT ALL, however
//     often they decline: a queued origin-session write is a conversation
//     session plan, whose typed decline is carried outside fanOut's Export arm
//     — QuarantineTracker.RecordFailure's single call site — so its failure
//     count against the breaker is structurally zero.
//   - Plan evaluations per sweep ≤ convergenceOriginSessionParsePerSweep = 16.
//     An inspection reads and walks a whole transcript, so this is the IO
//     bound that keeps one sweep O(1) whatever the backlog holds. What makes
//     the bound a rotation rather than a starvation is the verdict memo, not
//     the ordering: a no-enqueue verdict is memoized against the (source
//     fingerprint, canonical head hash) it judged, and an unchanged candidate
//     is skipped WITHOUT charging the budget — so each sweep spends its 16
//     only on candidates not yet judged at their current state, and the scan
//     reaches strictly deeper until every in-window candidate has been
//     judged. Newest-first alone could not do that: sixteen healthy hot
//     conversations would be re-inspected identically every sweep and an
//     older fork below them would never be reached — then age out of the
//     window entirely. A candidate can still be deferred only while sixteen
//     or more others keep CHANGING between every consecutive pair of sweeps,
//     and a changing file is exactly the population the T1 import hook
//     already inspects on every import.
const (
	// convergenceOriginSessionWindow bounds candidacy to conversations still
	// in active use. A fork in a conversation nobody has touched for a week
	// keeps its needs-driven path — the T1 hook on its next edit — rather than
	// spending sweep IO forever on cold history.
	convergenceOriginSessionWindow        = 7 * 24 * time.Hour
	convergenceOriginSessionParsePerSweep = 16
	convergenceOriginSessionQueuePerSweep = 2
)

// queueForkedOriginSessions performs one bounded pickup pass and reports how
// many origin-session writes it queued. Like readmitStuckMaterializations it
// repairs nothing itself: the existing drain owns backoff, gate re-evaluation
// and the flag-gated rebuild.
func (o *Orchestrator) queueForkedOriginSessions(now time.Time) int {
	if o == nil || o.cfg.Store == nil {
		return 0
	}
	conversations, err := o.cfg.Store.ListArtifacts(acf.KindConversation)
	if err != nil {
		return 0
	}
	sort.SliceStable(conversations, func(i, j int) bool {
		return conversations[i].UpdatedAt.After(conversations[j].UpdatedAt)
	})
	parsed, queued := 0, 0
	for _, art := range conversations {
		if queued >= convergenceOriginSessionQueuePerSweep ||
			parsed >= convergenceOriginSessionParsePerSweep {
			break
		}
		if now.Sub(art.UpdatedAt) > convergenceOriginSessionWindow {
			// Sorted newest first: everything after this is older still.
			break
		}
		// Only a local artifact whose source file exactly one configured
		// session adapter owns can have an origin session at all.
		if art.SourcePath == "" || art.RemoteOriginDeviceID != "" {
			continue
		}
		owners := o.pathOwners(art.SourcePath)
		if len(owners) != 1 {
			continue
		}
		targetName := ""
		for name := range owners {
			targetName = name
		}
		var inspector adapter.ConversationSessionSourceInspector
		for _, ad := range o.cfg.Adapters {
			if ad.Name() == targetName {
				inspector, _ = ad.(adapter.ConversationSessionSourceInspector)
				break
			}
		}
		if inspector == nil {
			continue
		}
		// An origin-repair entry already mid-retry must not have its budget or
		// flags disturbed, and a standing give-up record is re-admitted only by
		// the dwell-gated readmit pass above — never re-triggered here. A queued
		// foreign write is not sufficient: inspect it and widen its routing if the
		// source session is forked. Checked before inspection so genuinely
		// suppressed candidates cost no parse budget.
		if _, givenUp, repairReady := o.originSessionQueueState(targetName, art.ArtifactID); givenUp || repairReady {
			continue
		}
		head, hasHead, headErr := conversationHeadForBranch(
			o.cfg.Store, art.ArtifactID, selectedBranchForAgent(art, targetName))
		if headErr != nil || !hasHead {
			continue
		}
		// The verdict memo: a candidate already judged at exactly this (file,
		// head) state is skipped for an lstat — WITHOUT charging the parse
		// budget — so the budget only ever buys verdicts it does not have yet.
		// The fingerprint is captured before the inspection reads the file;
		// see originSessionInspectMemo for why that direction is the safe one.
		fp, fpOK := fingerprintPath(art.SourcePath)
		if fpOK && o.originSessionVerdictUnchanged(art.ArtifactID, fp, head.Hash) {
			continue
		}
		parsed++
		reusable, applicable, reason, ierr := inspector.InspectConversationSessionSource(art, head)
		if ierr != nil || !applicable || reusable || !originSessionRepairTriggerReason(reason) {
			// Every deterministic no-enqueue verdict is memoizable: reusable,
			// not applicable, and the non-trigger reasons (race, native_ahead,
			// graph_malformed) are all functions of the bytes and the head. An
			// inspection ERROR is not — it proves nothing about either — so it
			// stays unmemoized and is simply retried next sweep.
			if ierr == nil && fpOK {
				o.recordOriginSessionVerdict(art.ArtifactID, fp, head.Hash)
			}
			continue
		}
		if o.cfg.Logger != nil {
			o.cfg.Logger.Info("convergence sweep queued a forked origin session",
				"agent", targetName, "artifact_id", art.ArtifactID, "reason", string(reason))
		}
		queued++
		o.deferMaterialization(targetName, art.ArtifactID, targetName, true, false, true)
	}
	return queued
}

// convergenceSweepOnce performs at most one repair pass. Split out so tests
// can drive it deterministically without a timer.
//
// It drives RefanOutAll, NOT reconcileDeferredMaterializationTarget. That
// distinction is the whole correctness of this loop and was got wrong first
// time: reconcileDeferredMaterializationTarget is FAIL-FAST — it returns on
// the first per-artifact error, and a legitimately withheld artifact (a rule
// that does not route it, an unresolved conflict, a project not registered
// locally) produces exactly such an error. Since ListArtifacts is ordered by
// CreatedAt, a single denied artifact aborts the pass at the same position on
// every sweep, repairing everything before it and nothing after — silently,
// and while reporting itself clean. With selective sync, per-artifact denial
// is the NORMAL state, so effective coverage collapsed to roughly zero.
//
// RefanOutAll is the pass that actually repaired a whole device's backlog in
// one shot when an operator ran `aplexica daemon reload`; it fans out per
// artifact without short-circuiting, so one denied artifact costs exactly that
// artifact. It also takes nativeRestoreGate.RLock internally, so it cannot run
// during a native restore.
func (o *Orchestrator) convergenceSweepOnce(ctx context.Context, now time.Time) {
	worth, fp, fpOK := o.convergenceWorthSweeping(now)
	if !worth {
		return
	}
	if ctx.Err() != nil || o.closingNow() {
		return
	}
	readmitted := o.readmitStuckMaterializations(now)
	forkedQueued := o.queueForkedOriginSessions(now)
	foundDrift := readmitted > 0 || forkedQueued > 0
	if readmitted > 0 && o.cfg.Logger != nil {
		o.cfg.Logger.Info("convergence sweep re-admitted stuck writes",
			"writes", readmitted)
	}
	if forkedQueued > 0 && o.cfg.Logger != nil {
		o.cfg.Logger.Info("convergence sweep queued forked origin sessions",
			"writes", forkedQueued)
	}
	// Writes still owed (mid-retry, coalesced, or budget-exhausted) mean the
	// device has not converged, so hold the interval at the floor rather than
	// backing off toward the ceiling.
	if o.convergenceOwedWork() {
		foundDrift = true
	}
	o.noteConvergenceSweep(now, fp, fpOK, foundDrift)
}
