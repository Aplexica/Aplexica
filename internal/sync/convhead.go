package syncd

import "github.com/aplexica/aplexica/internal/acf"

// conversationHead selects the event whose payload materializes a conversation
// for fan-out — the markdown transcript (renderConversationMarkdown) and the
// native session (ConversationSessionTarget.MaterializeConversationSession).
//
// It mirrors hermes' exportableBundleFromActiveLog walk (internal/adapter/
// hermes/conversation.go): walk the active log BACKWARD to the latest
// payload-bearing mutating event — create/update/resolution, or a
// payload-bearing snapshot (FR-02.32). A payload-LESS snapshot (the
// pre-FR-02.32 shape) carries no transcript and is skipped.
//
// After retention's on-snapshot prune compacts a main-only conversation, the
// active log can be a single payload-less snapshot. Selecting events[len-1]
// then decodes the literal JSON `null` into a zero-value ConversationPayload
// (Format=="") — the transcript renders as an empty code block and the native
// session materializer silently skips. To recover, when the active walk finds
// no payload-bearing event (and is not blocked by a redaction), fall back to
// Store.ReadEventsIncludingCompacted, which re-merges the .compacted layer so
// the create/update payload comes back into view — exactly what hermes
// ExportConversationsToDB does.
//
// `events` is the artifact's active log, already read by the caller. Returns
// ok=false when nothing is materializable: the latest mutating event is a
// redaction (authoritative — content was removed, so it must NOT be resurrected
// from a pre-redaction payload), or no payload-bearing event exists in either
// layer. err is non-nil only when the compacted fallback read fails.
func conversationHead(store *acf.Store, artifactID string, events []acf.Event) (acf.Event, bool, error) {
	return conversationHeadForBranch(store, artifactID, acf.MainBranch)
}

func conversationHeadForBranch(store *acf.Store, artifactID, branchID string) (acf.Event, bool, error) {
	head, ok, err := projectedConversationHeadForBranch(store, artifactID, branchID, acf.BranchProjectionOpts{})
	if err != nil || ok {
		return head, ok, err
	}
	return projectedConversationHeadForBranch(store, artifactID, branchID, acf.BranchProjectionOpts{IncludeCompacted: true})
}

func conversationHeadNeedsMaterialization(head acf.Event) bool {
	p, err := acf.DecodeConversationPayload(head)
	return err == nil && p.Format == acf.ConversationDeltaFormatV1
}

func materializedConversationHead(store *acf.Store, artifactID string) (acf.Event, bool, error) {
	return conversationHeadForBranch(store, artifactID, acf.MainBranch)
}

func projectedConversationHeadForBranch(
	store *acf.Store,
	artifactID string,
	branchID string,
	opts acf.BranchProjectionOpts,
) (acf.Event, bool, error) {
	if branchID == "" {
		branchID = acf.MainBranch
	}
	normalizedBranch, nerr := acf.NormalizeBranchName(branchID)
	if nerr != nil {
		return acf.Event{}, false, nerr
	}
	// The main branch has no fork graph to project. Reconstruct it from the
	// newest full payload/redaction barrier instead of decoding the entire
	// append log. Long-running sessions can contain gigabytes of superseded
	// full-history events; replaying that whole log once per native target made
	// one changed transcript consume multiple CPU cores for tens of minutes.
	// IncludeCompacted retains the explicit full-history recovery path below.
	if normalizedBranch == acf.MainBranch && !opts.IncludeCompacted {
		payload, head, ok, err := store.MaterializedConversationHeadFromStore(artifactID)
		if err != nil || !ok {
			return acf.Event{}, ok, err
		}
		// This Event is a transient fan-out projection, not an append candidate.
		// Keep the stored head's compact payload and attach the already-typed full
		// state out-of-band. Native targets call DecodeConversationPayload, which
		// consumes the typed projection without allocating a second 100+ MB JSON
		// document. Retained cloud publishing builds its independently hashed full
		// baseline in retainedConversationEvent and is unaffected.
		head.MaterializedConversation = &payload
		head.Branch = normalizedBranch
		return head, true, nil
	}
	projected, err := store.ProjectEventsForBranch(acf.KindConversation, artifactID, branchID, opts)
	if err != nil {
		return acf.Event{}, false, err
	}
	if len(projected) == 0 {
		return acf.Event{}, false, nil
	}
	if _, ok, redacted := latestPayloadBearingEvent(projected); redacted && !ok {
		return acf.Event{}, false, nil
	}
	materialized, ok, err := acf.MaterializedConversationPayload(projected)
	if err != nil || !ok {
		return acf.Event{}, ok, err
	}
	head := projected[len(projected)-1]
	head.MaterializedConversation = &materialized
	head.Branch = normalizedBranch
	return head, true, nil
}

// latestPayloadBearingEvent walks events backward to the newest event carrying
// a materializable conversation payload, delegating the payload walk to
// acf.LatestPayloadEvent and layering the redaction-as-barrier policy on top
// (acf stays policy-free about redaction/fallback).
//
//   - ok==true: a create/update/resolution event, or a payload-bearing snapshot
//     (FR-02.32), was found — head is that event.
//   - redacted==true: the newest mutating event is a redaction; the caller must
//     NOT fall back to the compacted layer (the redaction is authoritative).
//   - both false: no payload-bearing event in this slice — the caller may retry
//     against a merged (active + compacted) log.
//
// Redaction handling: a redaction authoritatively removes content, so a payload
// at or before it must NOT be resurrected. We bound the payload search to events
// NEWER than the latest redaction; acf.LatestPayloadEvent is policy-free and
// would otherwise walk straight past the redaction to a pre-redaction payload.
func latestPayloadBearingEvent(events []acf.Event) (head acf.Event, ok, redacted bool) {
	window := events
	hasRedaction := false
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type == acf.EventTypeRedaction {
			window = events[i+1:]
			hasRedaction = true
			break
		}
	}
	head, ok = acf.LatestPayloadEvent(window)
	// A redaction that shadows every payload below it (nothing newer carries
	// one) is authoritative — signal redacted so the caller skips the fallback.
	return head, ok, hasRedaction && !ok
}
