package syncd

import (
	"errors"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
)

// The diverged-import nudge: how an already-stuck conversation converges with
// no user action.
//
// THE DEADLOCK IT BREAKS. After a native session declines as `diverged`,
// nothing ever re-imports that file. handleEventWithDisposition short-circuits
// on scanCache.unchanged, which is true for a byte-stable transcript, so the
// import half never runs again; and the drain's own short circuit
// (observeDeferredMaterializationInputs) sees the same canonical head and the
// same destination bytes, so the materialize half never runs either. Each half
// waits for the other and the artifact stays frozen indefinitely — which is
// exactly the state this recovery path addresses.
//
// THE TRIGGER IS EVIDENCE, NOT A TIMER. On a SessionDeclineDiverged the adapter
// has just told us, with a real pathname, that this specific file holds turns
// canonical lacks. That is a fact about those bytes, not a guess, and the
// remedy it justifies is exactly one import-only re-read of exactly that file.
const (
	// nativeReimportNudgesPerWindow and nativeReimportNudgeWindow are a
	// DEVICE-WIDE IO bound, not a breaker bound. An import parses a whole
	// transcript and some can be large, so the pass is
	// rate-limited even though it cannot fail anything.
	//
	// The budget is spent only by a NEW (artifact, destination bytes) pair — a
	// repeat for an unchanged file returns at the witness check without touching
	// it — so a backlog of N stuck artifacts costs N units in total, not N per
	// pass. The fixed budget lets an existing backlog converge gradually without
	// user action.
	//
	// REPAIR-PASS BUDGET (design rule 3 — a repair pass must stay under the
	// failure budget of the systems it drives). The quarantine breaker is 3
	// failures / 10 minutes PER ADAPTER and blocks ALL materialization including
	// live sync; a whole-store repair pass must not consume that headroom.
	//
	//   - Failures charged by this pass: ZERO, STRUCTURALLY.
	//     QuarantineTracker.RecordFailure has exactly one call site in the whole
	//     tree — the `if err := p.ad.Export(...)` arm of fanOut. importOnly
	//     returns before fanOut, so this pass cannot reach that arm however it
	//     fails. 0 < 3.
	//   - It therefore consumes NONE of the headroom the convergence sweep
	//     already claimed. Export-capable repair actions per adapter per
	//     10-minute window stay at convergenceReadmitPerSweep = 2 (the
	//     15-minute sweep floor admits at most one sweep per 10-minute window),
	//     exactly as before this change. 2 < 3. Had the nudge fanned out, the
	//     worst case would have been 2 + 1 = 3, which TRIPS the breaker — that is
	//     precisely why eventHandlingOptions.importOnly exists and why it must
	//     not be "simplified" away later.
	//   - Per-entry pacing is the DRAIN's, not this cap's. The entry's backoff
	//     doubles to a 15-minute ceiling and only a real materialization attempt
	//     can produce the decline that offers a nudge, so one artifact
	//     contributes at most one nudge per drain pass of that artifact. The cap
	//     below bounds the device, not the entry — and when it turns a nudge
	//     back it does NOT throw the evidence away — see reofferDivergedNativeNudge.
	//   - It is a FIXED POINT. A successful absorb makes the native turns an
	//     in-order subsequence of canonical, so the next import is a no-op AND
	//     the diverged decline that triggers this pass stops occurring. A repair
	//     that removes its own trigger cannot loop.
	nativeReimportNudgesPerWindow = 4
	nativeReimportNudgeWindow     = 10 * time.Minute

	// nativeReimportSeenMax bounds what the nudge remembers for the lifetime of
	// the process. The population is artifacts that produced a diverged decline,
	// which the deferral queue's own limit already keeps small; this exists so a
	// daemon running for months cannot accumulate one entry per conversation
	// forever. Clearing wholesale rather than evicting one key is deliberate:
	// the map is a duplicate-work optimization and the rate cap above is what
	// actually bounds the work, so losing it costs at most one extra re-read per
	// artifact.
	nativeReimportSeenMax = 1024
)

// divergedNativeDest returns the unredacted destination a target just refused
// as SessionDeclineDiverged, and "" for every other cause.
//
// The reason restriction is the whole safety argument for re-reading a file
// nothing asked us to re-read. `diverged` is the ONE decline that means "both
// sides hold something the other lacks", so it is the only one for which a
// fresh import can move canonical at all. Every other decline either resolves
// itself (race, native_ahead), or names a file whose bytes canonical has
// already fully consumed (mirror_diverged, forked_mirror, chain_unspanned), or
// names a file that could not be authenticated at all (graph_malformed).
func divergedNativeDest(cause error) string {
	var decline *ConversationDeclineError
	if errors.As(cause, &decline) && decline.Reason == adapter.SessionDeclineDiverged {
		return decline.dest
	}
	return ""
}

// nudgeDivergedNativeImport re-imports the ONE native file a target just
// refused as diverged, so the import half of the deadlock can absorb the turns
// canonical is missing.
//
// It is deliberately import-ONLY (see eventHandlingOptions.importOnly), fires
// at most once per (artifact, destination bytes) so an unchanged file is never
// re-parsed twice for the same reason, and is capped device-wide.
//
// refusedByBudget reports that the device-wide cap turned this nudge back. That
// is NOT the same as "nothing to do", and the caller must not treat it as one:
// the decline that offered the nudge is produced by a real materialization
// attempt, and the drain's own short circuit
// (observeDeferredMaterializationInputs) suppresses every later attempt once a
// decline has been observed for unchanged inputs. So an evidence-bearing nudge
// that is simply dropped removes its own trigger forever — on a backlog the
// first artifact would take the slot and the rest would never produce another
// diverged decline at all.
func (o *Orchestrator) nudgeDivergedNativeImport(agent, artifactID, destPath string) (refusedByBudget bool) {
	if o == nil || artifactID == "" || destPath == "" || o.closingNow() {
		return false
	}
	// Witness the bytes we are about to re-read. Recording the witness rather
	// than a bare "already nudged" flag is what makes this retryable: when the
	// agent writes to the file again the witness moves, the pair is new, and the
	// nudge is available once more.
	witness := witnessSessionFile(destPath)
	if !witness.exists {
		return false
	}
	now := time.Now()
	o.nativeReimportMu.Lock()
	if seen, ok := o.nativeReimportSeen[artifactID]; ok && seen.same(witness) {
		// Already re-read at exactly these bytes. There is nothing new to learn
		// from the file, so this is a real "nothing to do" rather than a
		// deferral.
		o.nativeReimportMu.Unlock()
		return false
	}
	kept := o.nativeReimportAt[:0]
	for _, at := range o.nativeReimportAt {
		if now.Sub(at) < nativeReimportNudgeWindow {
			kept = append(kept, at)
		}
	}
	o.nativeReimportAt = kept
	if len(o.nativeReimportAt) >= nativeReimportNudgesPerWindow {
		// The witness is deliberately NOT recorded here, so the pair stays new
		// and the next paced pass can spend a later window's budget on it.
		o.nativeReimportMu.Unlock()
		return true
	}
	o.nativeReimportAt = append(o.nativeReimportAt, now)
	if o.nativeReimportSeen == nil || len(o.nativeReimportSeen) >= nativeReimportSeenMax {
		o.nativeReimportSeen = make(map[string]sessionWriteWitness, 1)
	}
	o.nativeReimportSeen[artifactID] = witness
	o.nativeReimportMu.Unlock()

	// The scan cache's premise — byte-stable means already consumed — is exactly
	// what the divergence refutes, so drop this one path's entry before asking.
	o.scanCache.invalidate(destPath)
	o.handleEventWithDisposition(destPath, eventHandlingOptions{importOnly: true})
	if o.cfg.Logger != nil {
		o.cfg.Logger.Info("re-read a diverged native session so canonical can absorb its turns",
			"agent", agent, "artifact_id", artifactID, "path", redactPaths(destPath))
	}
	return false
}

// deferAbsorbedConversationFanOut hands every other conversation target the
// canonical commit an import-only pass just made, through the deferral queue.
//
// It exists because eventHandlingOptions.importOnly returns BEFORE fanOut, and
// fanOut is the only thing in the tree that derives "target X is behind on
// artifact A". Without this the turns the nudge worked to absorb reach canonical
// and no other local agent — and nothing revisits them, because the same pass
// has already recorded the file's fingerprint in the scan cache and a later pass
// would filter the artifact out as not freshly committed anyway.
//
// It enqueues rather than fanning out inline, which is the whole reason
// importOnly exists: the quarantine breaker is fed from fanOut's Export arm, and
// an inline fan-out here would put this pass's failures on the same 3-per-10-
// minute budget the convergence sweep already claims 2 of. Enqueuing charges
// nothing and lets the drain — which is backoff-paced, quarantine-aware and
// re-evaluates every gate on each retry — perform the write.
func (o *Orchestrator) deferAbsorbedConversationFanOut(ids []string) {
	if o == nil || len(ids) == 0 || o.cfg.Store == nil {
		return
	}
	for _, id := range ids {
		art, found := o.findArtifact(id)
		if !found || art.Kind != acf.KindConversation {
			continue
		}
		for _, ad := range o.cfg.Adapters {
			target := ad.Name()
			// Only the targets fanOut would route a conversation to. Anything
			// else would queue a write its adapter can never perform.
			if _, session := ad.(adapter.ConversationSessionTarget); !session {
				if _, doc := ad.(adapter.ConversationDocTarget); !doc {
					continue
				}
			}
			head, ok, err := conversationHeadForBranch(
				o.cfg.Store, art.ArtifactID, selectedBranchForAgent(art, target))
			if err != nil || !ok {
				continue
			}
			origin := head.Provenance.SourceAgent
			if origin == "" || origin == target {
				// The agent that wrote the head needs no copy of it. Its own
				// queued entry — the one whose decline triggered the nudge — was
				// already reopened by reopenDeferredMaterializationForDest.
				continue
			}
			o.deferMaterialization(target, art.ArtifactID, origin, false, false, true)
		}
	}
}

// reofferDivergedNativeNudge clears ONE entry's decline short circuit so its
// next PACED pass reproduces the decline and re-offers the nudge the device-wide
// budget just turned back.
//
// It adds no unpaced work: the entry still waits out its own backoff, still
// costs one materialization attempt, and that attempt is the same one the drain
// was always going to make. What it prevents is the trigger being deleted —
// which is what turned a device-wide cap into a device-wide limit of one healed
// artifact per daemon lifetime.
func (o *Orchestrator) reofferDivergedNativeNudge(agent, artifactID string) {
	if o == nil || agent == "" || artifactID == "" {
		return
	}
	o.deferredMaterializeMu.Lock()
	defer o.deferredMaterializeMu.Unlock()
	queue := o.deferredMaterialize[agent]
	if queue == nil {
		return
	}
	entry, ok := queue.entries[artifactID]
	if !ok || !entry.declineObserved {
		return
	}
	entry.declineObserved = false
	queue.entries[artifactID] = entry
}
