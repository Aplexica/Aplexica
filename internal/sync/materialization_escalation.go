package syncd

import (
	"fmt"
	"strings"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
)

// Escalation policy for the deferred-materialization queue: when a queued
// native write stops being retried and is raised for a human instead.
//
// v1.0.63 escalated on attempt count alone, and that budget turned out to be
// structurally unreachable for a whole population: a write that a policy gate
// turns back never reaches the adapter and therefore charges no
// attempt, while startNativeStartupSafety re-arms that gate for every agent on
// every daemon start and resumeDeferredMaterializationAfterUnblock zeroes the
// pacing counter each time it clears. A counter that resets on a
// frequently-firing path can never be the sole basis of a terminal decision.
//
// Escalation is therefore driven by wall-clock age plus QUIESCENCE: an entry is
// raised only once nothing it depends on has moved for a sustained window —
// same canonical head, same destination bytes. Quiescence is what keeps the age
// trigger honest. A conversation someone leaves open in a TUI all day keeps
// moving its own session file, so it never becomes quiet and can never be
// false-terminaled on elapsed time.
const (
	// deferredMaterializationEscalateAge is how long an entry must have been
	// queued before elapsed time may raise it. Deliberately well under the
	// 24h attempt-budget age: the population this trigger exists for cannot
	// reach that budget at all.
	deferredMaterializationEscalateAge = 6 * time.Hour

	// deferredMaterializationQuietFor is how long the entry's observable
	// inputs must have been unchanged. It is an order of magnitude above the
	// 15-minute retry ceiling, so a genuinely live destination resets the
	// clock many times over before it could ever expire.
	deferredMaterializationQuietFor = 2 * time.Hour
)

// Escalation rate budget (per device, rolling window).
//
// A substantial retained population can hold turns canonical lacks and would
// classify as escalation-worthy, while convergenceReadmitPerSweep bounds
// RE-admission only, never initial
// classification. One `aplexica daemon reload` fans out the whole store, so
// without a cap the first sweep would raise hundreds of needs_attention rows
// inside half an hour — a wall of alarms is the same failure as silence.
const (
	deferredEscalationsPerWindow = 3
	deferredEscalationWindow     = 24 * time.Hour

	// deferredEscalationHeldRetry paces an entry the cap turned back. It is
	// deliberately longer than the 15-minute retry ceiling: the entry is known
	// to be going nowhere, and the only thing still worth doing is noticing if
	// its inputs finally move.
	deferredEscalationHeldRetry = time.Hour

	// deferredEscalationHeldAdmit paces the held POPULATION, not the individual
	// entry: at most one held retry per target per interval. Per-entry pacing
	// alone makes the aggregate rate proportional to how many entries are held,
	// which is exactly how a retained population becomes an outage generator.
	// See deferredMaterializationQueue.heldRetryDueLocked for the arithmetic.
	deferredEscalationHeldAdmit = 15 * time.Minute
)

// Repair-pass budget (design rule 3). The quarantine breaker is 3 adapter
// failures per 10 minutes and blocks ALL materialization including live sync;
// an unbounded whole-store repair pass can cause an outage that way. The
// arithmetic for everything in this file:
//
//   - escalation and the rate budget make ZERO adapter calls. They read
//     timestamps already on the entry and, at most, os.Stat a path the previous
//     attempt already named. Worst case: 0 failures / 10 min, however many
//     entries qualify at once.
//   - the unchanged-inputs short circuit strictly REMOVES adapter calls — it
//     returns before fan-out — so it can only lower the observed rate.
//   - per-entry pacing is untouched: the backoff still doubles to a 15-minute
//     ceiling, so one entry contributes at most 1 failure per 15 min once
//     ramped.
//
// 0 < 3, and nothing here widens the existing drain's fan-out. Asserted by
// TestDeferredMaterializationEscalation_CapsNewAttentionRowsPerDay, which
// qualifies 200 entries at once and requires the adapter to be untouched.

// EscalationsPerDay is the per-device cap the status surface reports against.
// Exported so the CLI states the limit it is measuring rather than keeping a
// second copy of the number that could drift from this one.
const EscalationsPerDay = deferredEscalationsPerWindow

// deferredMaterializationEscalates reports whether an entry should stop being
// retried automatically and be raised for a human.
//
// Two independent triggers, because the queue holds two structurally different
// populations:
//
//   - the attempt budget: the entry really reached the adapter and was refused
//     deferredMaterializationMaxAttempts times over at least
//     deferredMaterializationMaxAge. Unchanged from v1.0.63.
//   - age plus quiescence: the entry has been queued at least
//     deferredMaterializationEscalateAge and nothing it depends on has changed
//     for at least deferredMaterializationQuietFor. This is the only trigger
//     that can ever fire for a write no gate ever let through.
func deferredMaterializationEscalates(entry deferredMaterializationEntry, now time.Time) bool {
	if deferredMaterializationExhausted(entry.attempts, entry.firstDeferred, now) {
		return true
	}
	return deferredMaterializationQuiescent(entry, now)
}

// deferredMaterializationQuiescent is the age+quiescence half of the rule. An
// entry whose inputs were never observed has no quiescence evidence at all and
// is never judged quiet — silence about a fact is not the fact.
func deferredMaterializationQuiescent(entry deferredMaterializationEntry, now time.Time) bool {
	if entry.firstDeferred.IsZero() || entry.quietSince.IsZero() {
		return false
	}
	if now.Sub(entry.firstDeferred) < deferredMaterializationEscalateAge {
		return false
	}
	return now.Sub(entry.quietSince) >= deferredMaterializationQuietFor
}

// escalationsInWindow counts the needs_attention rows this device has raised
// inside the rolling window.
//
// It is derived from persisted state rather than from a live counter on
// purpose. A daemon restart happens often enough to re-arm the startup safety
// gate for every adapter, and a counter that a restart zeroes would hand a
// restart loop a fresh allowance every time — the same defect class as the
// attempt budget this escalation path replaces.
//
// Cost is one pass over each target's give-up records and pending entries, and
// it is only consulted when an entry has already qualified to escalate — which
// takes at least deferredMaterializationEscalateAge to happen at all. The
// caller holds deferredMaterializeMu.
func escalationsInWindow(byTarget map[string]*deferredMaterializationQueue, now time.Time) int {
	count := 0
	within := func(at time.Time) bool {
		return !at.IsZero() && now.Sub(at) < deferredEscalationWindow
	}
	for _, queue := range byTarget {
		if queue == nil {
			continue
		}
		raised := make(map[string]struct{}, len(queue.abandoned))
		for _, record := range queue.abandoned {
			if record.artifactID != "" {
				raised[record.artifactID] = struct{}{}
			}
			if within(record.abandonedAt) {
				count++
			}
		}
		// A re-admitted entry carries its escalation timestamp forward, so a
		// write the sweep is retrying still counts against the allowance it
		// already spent. It is counted here only when no give-up record for the
		// same artifact survives — records are now retained across re-admission,
		// and counting both would charge one escalation twice.
		for artifactID, entry := range queue.entries {
			if _, alreadyCounted := raised[artifactID]; alreadyCounted {
				continue
			}
			if within(entry.lastEscalatedAt) {
				count++
			}
		}
	}
	return count
}

// deferredMaterializationHeadWitness is the canonical half of the quiescence
// observation: the hash of the head this write would materialize. It is read
// from artifact metadata rather than by replaying the event log, so observing
// quiescence costs nothing even for a multi-gigabyte conversation.
func deferredMaterializationHeadWitness(art acf.Artifact, agent string) string {
	if art.Kind == acf.KindConversation {
		if head := art.BranchHeads[selectedBranchForAgent(art, agent)]; head != "" {
			return head
		}
	}
	return art.HeadEventHash
}

// observeDeferredMaterializationInputs folds one attempt's observable inputs
// into the entry's quiescence clock and reports whether they are identical to
// what the previous attempt in THIS process saw.
//
// The two inputs are the canonical head this write would materialize and the
// destination the target last refused. Together they are exactly what decides
// the outcome, so leaving both unchanged means the next attempt is guaranteed
// to reach the same conclusion.
//
// A true return is the drain's unchanged-inputs short circuit, and it requires
// POSITIVE evidence that these two inputs are what decided the last outcome:
// declineObserved, which only a typed adapter decline naming its own
// destination sets, and which every other outcome clears. Without that rule the
// short circuit would also swallow retries it has no business swallowing —
// a transient adapter error names no destination, so "the canonical head did
// not move" would wrongly read as "retrying is pointless", and a policy gate
// that later OPENS is a change these two inputs cannot see at all.
//
// The quiescence clock is folded regardless, because a write nothing ever let
// through is precisely the population that must still be able to escalate.
func (o *Orchestrator) observeDeferredMaterializationInputs(agent string, art acf.Artifact, now time.Time) bool {
	head := deferredMaterializationHeadWitness(art, agent)

	o.deferredMaterializeMu.Lock()
	defer o.deferredMaterializeMu.Unlock()
	queue := o.deferredMaterialize[agent]
	if queue == nil || queue.overflow {
		// Overflow is a whole-target reconciliation with no per-artifact
		// accounting, and it is FAIL-FAST — it returns on the first error, so a
		// short circuit fired mid-scan would abort the pass at the same
		// artifact every time while reporting itself withheld.
		return false
	}
	entry, ok := queue.entries[art.ArtifactID]
	if !ok {
		return false
	}
	// The destination comparison is BASELINED, not assumed. destPath and
	// destWitness are memory-only (destPath is a real user path and is never
	// persisted), so after a restart the first passes have no destination to
	// witness at all. Comparing a zero witness against the first real one would
	// read as "the destination changed" and restart the quiescence clock — on a
	// path that fires on every daemon start, against a clock whose whole purpose
	// is to be immune to restarts. So the first real observation in this process
	// establishes the baseline and asserts no change.
	destUnchanged := true
	observedDest := false
	dest := sessionWriteWitness{}
	if entry.destPath != "" {
		dest = witnessSessionFile(entry.destPath)
		observedDest = true
		if entry.destObserved {
			destUnchanged = entry.destWitness.same(dest)
		}
	}
	unchanged := entry.lastHeadHash == head && destUnchanged
	if !unchanged || entry.quietSince.IsZero() {
		entry.quietSince = now
	}
	if !unchanged {
		// New evidence: whatever the rate budget decided last time was decided
		// about a situation that no longer holds.
		entry.escalationHeld = false
	}
	entry.lastHeadHash = head
	if observedDest {
		entry.destWitness = dest
		entry.destObserved = true
	}
	queue.entries[art.ArtifactID] = entry
	return unchanged && entry.declineObserved
}

// errDeferredMaterializationUnchanged marks the short circuit above. It carries
// the withheld marker because a pass that never reached the adapter must be
// paced, never charged — the pre-fix design charged it, which let a queue spend
// its whole give-up budget on attempts it deliberately did not make.
var errDeferredMaterializationUnchanged = fmt.Errorf(
	"nothing changed since the previous attempt (%w)", errDeferredMaterializationWithheld)

// materializationSurface carries the per-device facts a needs_attention row's
// wording depends on. It exists so the surface can never claim "no shipped
// command repairs that" about a class this very build ships a repair for — it
// just happens to be switched off.
type materializationSurface struct {
	// mirrorRepairSupported is whether the target adapter SHIPS the rebuild at
	// all. It is a separate axis from mirrorRepairEnabled because collapsing the
	// two makes "off" indistinguishable from "does not exist", and only
	// claudecode implements adapter.ConversationMirrorRepairReporter while codex
	// reports the same `diverged` reason. Offering a codex divergence a flag
	// codex never reads is a remedy the operator can follow to completion and
	// still be exactly where they started.
	mirrorRepairSupported bool

	// mirrorRepairEnabled is whether that adapter is currently authorized to run
	// it (sync.repairForkedMirrors). Meaningful only when supported.
	mirrorRepairEnabled bool
}

// offersMirrorRepair reports that naming the rebuild flag would actually change
// this target's outcome: the target has the repair and it is switched off.
func (s materializationSurface) offersMirrorRepair() bool {
	return s.mirrorRepairSupported && !s.mirrorRepairEnabled
}

// mirrorRepairRefused reports that the rebuild ran for this target and its loss
// proof declined, which is the only state in which "nothing you can type
// changes this" is an honest thing to say about a repairable class.
func (s materializationSurface) mirrorRepairRefused() bool {
	return s.mirrorRepairSupported && s.mirrorRepairEnabled
}

// enableMirrorRepairRemedy names the ONE config key that turns the rebuild on.
//
// It names a FILE edit rather than `aplexica config set` on purpose: that
// command writes the layered ~/.aplexica/config.toml, while this flag is read
// from the daemon's <state-dir>/config.json. Naming the command would be the
// same defect as the remedy it replaced — typeable, and inert.
const enableMirrorRepairRemedy = `set "sync": {"repairForkedMirrors": true} in ` +
	"<state-dir>/config.json, then run: aplexica daemon restart"

// escalatedMaterializationExplain says, in the operator's terms, what a
// needs_attention row means for THIS class.
//
// The pre-fix text was one sentence for every class and named no cause at all,
// which made accumulated entries difficult to diagnose.
func escalatedMaterializationExplain(
	reason adapter.SessionDeclineReason, gate SuppressionReason, surface materializationSurface,
) string {
	switch reason {
	case adapter.SessionDeclineDiverged:
		diverged := "This agent's own session has diverged from the canonical conversation — " +
			"each side holds turns the other does not, so neither can be written over the other."
		if surface.offersMirrorRepair() {
			return diverged + " The automatic rebuild that repairs this is switched off on this device."
		}
		if surface.mirrorRepairRefused() {
			return diverged + " The automatic rebuild is enabled and still refused, which means " +
				"the file holds a row the canonical conversation cannot reproduce."
		}
		return diverged
	case adapter.SessionDeclineMirrorDiverged:
		// Mirrors with a canonical log can hold a turn canonical never saw. The
		// canonical head is
		// not the problem here, so saying "diverged" and pointing at the
		// canonical repair sends the operator somewhere that cannot help.
		return "The copy Aplexica maintains for this agent holds a turn the canonical " +
			"conversation has not imported, so rewriting it would delete that turn. " +
			"It clears on its own once that turn is imported."
	case adapter.SessionDeclineForkedMirror:
		unreachable := "This agent's session file holds rows its own resume walk cannot reach, " +
			"so Aplexica cannot append to it without stranding them."
		if surface.mirrorRepairRefused() {
			return unreachable + " The automatic rebuild is enabled and still refused, which " +
				"means the file holds a row the canonical conversation cannot reproduce."
		}
		if surface.offersMirrorRepair() {
			return unreachable + " The automatic rebuild that repairs this is switched off on this device."
		}
		return unreachable
	case adapter.SessionDeclineChainUnspanned:
		// This class represents a session holding several independent
		// conversation roots — a
		// compacted or summarized transcript whose pre-compaction rows sit under a
		// root of their own. Nothing branched.
		//
		// It gets the fork's TWO-STATE treatment even so, because the repair
		// ROUTER — rebuildDivergedClaudeMirror and repairDivergedNativeSession
		// alike — keys on "the walk did not span the file", not on the fork
		// measurement. This build therefore repairs a containment-provable
		// chain_unspanned exactly as it repairs a fork, and telling the operator
		// "a shape Aplexica has not seen" at the moment the fix is one config key
		// away is the precise failure this surface exists to prevent. Only the
		// SHAPE description differs, so the operator is not sent looking for a
		// branch that is not there.
		unreached := "This agent's session file holds a conversational row its own resume walk " +
			"did not reach, and the file is not forked — several independent conversation roots " +
			"share the one file."
		if surface.mirrorRepairRefused() {
			return unreached + " The automatic rebuild is enabled and still refused, which means " +
				"the file holds a row the canonical conversation cannot reproduce."
		}
		if surface.offersMirrorRepair() {
			return unreached + " The automatic rebuild that repairs this is switched off on this device."
		}
		return unreached + " The daemon log records the file's structural fault."
	case adapter.SessionDeclineGraphMalformed:
		return "The session file on disk could not be authenticated as this conversation, " +
			"so re-reading it reaches the same conclusion every time."
	case adapter.SessionDeclineNativeAhead:
		return "This agent's own session is ahead of the canonical conversation and its turns " +
			"have not been imported yet. Closing the conversation in the agent lets the import finish."
	case adapter.SessionDeclineRace:
		return "The destination was being written on every attempt, so Aplexica never had a " +
			"settled file to update."
	case adapter.SessionDeclineOptOut:
		return "This agent cannot store this payload as a native session, so this write will never land."
	}
	if gate != "" {
		return gate.Explain() + " Aplexica stopped retrying the write while that held."
	}
	return "Aplexica stopped retrying this write and the agent reported no reason for refusing it. " +
		"The daemon log records the last error."
}

// escalatedMaterializationRemedy names a command that can actually resolve this
// class, or nothing at all.
//
// Earlier needs_attention rows offered `aplexica repair materialization
// --agent <x>`, which only lists and drops —
// and --drop explicitly forfeits the write. Handing the operator a command that
// repairs nothing is worse than handing them none, because it costs them the
// time to try it before they learn that.
func escalatedMaterializationRemedy(
	agent, artifactID string, reason adapter.SessionDeclineReason, gate SuppressionReason,
	surface materializationSurface,
) string {
	switch reason {
	case adapter.SessionDeclineDiverged:
		if surface.offersMirrorRepair() {
			// A native divergence — canonical and the user's own transcript each
			// holding a turn the other lacks — is repairable by the SAME
			// containment-proven rebuild the forked mirror uses, now that it is
			// allowed to run against a native-origin session. It is one config key
			// away, so naming that key is the minimum-involvement remedy for the
			// one class the machine can otherwise finish by itself.
			//
			// It supersedes `aplexica repair conversation <id>`, which was named
			// here on the theory that duplicated canonical turns cause the
			// divergence. That command only PRINTS a proposed collapse, and it
			// cannot resolve a divergence at all when the two sides simply hold
			// different turns — which is the shape this class actually is.
			//
			// GATED ON SUPPORT, not merely on the flag being off. codex reports
			// this same reason for a native-origin rollout and cannot read this
			// flag at all, so an unconditional !enabled test sent a codex
			// divergence around a loop it could never leave — while having talked
			// the operator into enabling destructive rewriting of their Claude
			// transcripts on the strength of an unrelated problem.
			return enableMirrorRepairRemedy
		}
		// Either the rebuild ran and its loss proof refused, or this target has
		// no rebuild. Neither is resolved by a config key. The one remaining
		// lever is the canonical head, so keep naming the read-only inspection
		// command rather than nothing.
		if artifactID != "" {
			return "aplexica repair conversation " + artifactID
		}
		return ""
	case adapter.SessionDeclineOptOut:
		// Nothing was ever going to land here, so clearing the record is the
		// whole resolution rather than a forfeit.
		if artifactID != "" {
			return "aplexica repair materialization --drop --artifact " + artifactID
		}
		return ""
	case adapter.SessionDeclineForkedMirror, adapter.SessionDeclineChainUnspanned:
		// BOTH classes, because the repair router keys on "the walk did not span
		// the file" rather than on the fork measurement, so this build repairs a
		// containment-provable chain_unspanned exactly as it repairs a fork.
		// Listing chain_unspanned under "no shipped command changes this" while
		// shipping a repair for it was the same defect this surface was built to
		// prevent, one class over.
		if surface.offersMirrorRepair() {
			// This build ships the repair; it is one config key away. Saying
			// "no shipped command repairs that" at the exact moment the operator
			// is looking is the maximum-involvement outcome for the one class
			// that has an automatic fix.
			return enableMirrorRepairRemedy
		}
		// Enabled and still refusing, or a target with no rebuild at all: the
		// loss proof found a row the canonical conversation cannot reproduce, or
		// there is nothing to switch on. Nothing the operator can type changes
		// that, and the honest explain already says so.
		return ""
	case adapter.SessionDeclineMirrorDiverged,
		adapter.SessionDeclineGraphMalformed,
		adapter.SessionDeclineNativeAhead,
		adapter.SessionDeclineRace:
		// No shipped command changes any of these. Say so.
		return ""
	}
	if gate != "" {
		// The write never reached the target; the gate is the problem, and the
		// suppression catalog already describes how to open it. Substituting the
		// agent makes the catalog's placeholder form copy-pasteable, which is the
		// difference between a remedy and a hint.
		return strings.ReplaceAll(gate.Remedy(), "<agent>", agent)
	}
	return ""
}
