package adapter

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
)

// ConversationPayloadEncoder builds an acf.conversation.v1 payload from
// canonical events.
type ConversationPayloadEncoder func(events []acf.ConversationEvent) (json.RawMessage, error)

// EncodeCanonicalConversationPayload is the standard acf.conversation.v1 encoder
// used by the thread merge.
func EncodeCanonicalConversationPayload(events []acf.ConversationEvent) (json.RawMessage, error) {
	return acf.EncodePayload(acf.ConversationPayload{Format: acf.ConversationFormatV1, Events: events})
}

type ThreadRef struct {
	ArtifactID            string
	BranchID              string
	MaterializedTurnsHash string
	// MaterializedTurnCount is the number of visible turns in the generated
	// snapshot the agent resumed. Rows appended by the agent are not included.
	// Together with MaterializedTurnsHash this identifies a trustworthy stale
	// base, so a continuation written from that base can be unioned with turns
	// that arrived meanwhile without accepting an arbitrary divergent rewrite.
	MaterializedTurnCount int
	GeneratedSnapshot     bool
	// AuthenticatedGeneratedPath is set only by an adapter after correlating
	// the imported native file with the deterministic materialization path and
	// native session id for ArtifactID+BranchID. Marker text alone is forgeable;
	// any content-removing generated-session repair must require this additional
	// path identity proof.
	AuthenticatedGeneratedPath bool
	// SanitizedSyntheticTurn is set only by an adapter that found and removed
	// its own exact bookkeeping reply from an Aplexica-generated native
	// session. It permits one corrective, prefix-preserving shrink below while
	// keeping the normal stale-copy anti-revert guard intact.
	SanitizedSyntheticTurn bool
	// SanitizedPortableProjection is set only when an adapter-authenticated
	// Aplexica-generated native session has been projected down to portable
	// user/assistant text. It permits a proven legacy projection to replace a
	// canonical head that still contains injected system/tool/commentary rows.
	SanitizedPortableProjection bool
	// SanitizedLegacyTurns is the visible user/assistant projection the same
	// authenticated native bytes would have produced before commentary phases
	// were filtered. It is repair evidence, not merge input: an existing head
	// must match this sequence (or its prefix) before unequal visible turns may
	// be removed. That prevents a stale generated session from erasing a real
	// remote continuation merely because portable sanitation is enabled.
	SanitizedLegacyTurns []acf.TextTurn
}

// ConversationTurnsHash fingerprints only the role+text representation that
// cross-agent native materializers preserve. Materialized session files stamp
// this value so their watcher echo can be rejected without replaying the
// canonical artifact's potentially multi-gigabyte event log. A real user
// continuation changes the extracted turns and therefore cannot hit the
// shortcut.
func ConversationTurnsHash(turns []acf.TextTurn) string {
	encoded, _ := json.Marshal(turns)
	return fmt.Sprintf("%x", sha256.Sum256(encoded))
}

func NormalizeThreadRef(ref ThreadRef) (ThreadRef, error) {
	if ref.ArtifactID == "" {
		return ThreadRef{}, fmt.Errorf("adapter: empty conversation artifact id")
	}
	if ref.BranchID == "" {
		ref.BranchID = acf.MainBranch
	}
	branch, err := acf.NormalizeBranchName(ref.BranchID)
	if err != nil {
		return ThreadRef{}, err
	}
	ref.BranchID = branch
	return ref, nil
}

// MergeConversationByThread is the heart of loop-safe bidirectional conversation
// sync. A session file that Aplexica materialized carries the canonical THREAD
// id (the conversation artifact id). When such a file is imported:
//
//   - threadID empty or unknown  → handled=false; the caller does a normal
//     path-keyed import (a native session the user started in this agent).
//   - threadID is a known artifact AND its clean text turns are UNCHANGED →
//     handled=true, no ids: a no-op. This is the loop break — Aplexica's own
//     re-materializations reproduce the same turns and stop here.
//   - threadID is known AND the text turns CHANGED (the user continued the
//     conversation in this agent) → append an update event to the canonical
//     thread and return its id, so fan-out re-materializes to every other
//     agent. This is what carries a continuation back across agents.
//
// Comparison is on acf.ExtractTextTurns (role+text only), the representation
// that round-trips across native formats — so tool/format differences never
// register as spurious changes.
func MergeConversationByThread(
	ctx context.Context,
	store *acf.Store,
	params OpaqueParams,
	threadID string,
	newEvents []acf.ConversationEvent,
	encode ConversationPayloadEncoder,
) (ids []string, handled bool, err error) {
	if threadID == "" {
		return nil, false, nil
	}
	return MergeConversationByThreadRef(ctx, store, params, ThreadRef{ArtifactID: threadID, BranchID: acf.MainBranch}, newEvents, encode)
}

func MergeConversationByThreadRef(
	ctx context.Context,
	store *acf.Store,
	params OpaqueParams,
	ref ThreadRef,
	newEvents []acf.ConversationEvent,
	encode ConversationPayloadEncoder,
) (ids []string, handled bool, err error) {
	origRef := ref
	normalizedRef, nerr := NormalizeThreadRef(ref)
	if nerr != nil {
		if origRef.ArtifactID == "" {
			return nil, false, nil
		}
		return nil, true, nerr
	}
	ref = normalizedRef
	existing, rerr := store.ReadArtifact(acf.KindConversation, ref.ArtifactID)
	if rerr != nil {
		return nil, false, nil // not a thread we own → fall back to native import
	}
	newTurns := acf.ExtractTextTurns(newEvents)
	completeGeneratedSnapshot := ref.GeneratedSnapshot && ref.MaterializedTurnCount == len(newTurns) &&
		ref.MaterializedTurnsHash != "" && ref.MaterializedTurnsHash == ConversationTurnsHash(newTurns)
	// A turns-hash match is the cheapest loop break. Legacy generated snapshots
	// without the hash continue to the text comparison below: that lets a clean
	// native copy repair a canonical head polluted by older re-timestamped
	// materializations while unchanged mirrors still no-op.
	// A complete untouched generated snapshot also continues: it may be the
	// clean authority for the narrowly proven legacy edge-echo repair below.
	if !ref.SanitizedPortableProjection && !completeGeneratedSnapshot && ref.MaterializedTurnsHash != "" &&
		ref.MaterializedTurnsHash == ConversationTurnsHash(newTurns) {
		return nil, true, nil
	}
	if ref.GeneratedSnapshot {
		// Desktop catalog metadata may surface a generated thread shell before
		// its canonical event log exists. It is still our own mirror and must
		// not fall through to a path-keyed import. Generated snapshots with a
		// real head continue below so they can repair legacy pollution.
		if _, ok, lerr := store.LastEvent(acf.KindConversation, ref.ArtifactID); lerr == nil && !ok {
			return nil, true, nil
		}
	}
	var (
		cur     acf.ConversationPayload
		curHead acf.Event
		ok      bool
		derr    error
	)
	if ref.BranchID == acf.MainBranch {
		// Generated Codex/Claude continuations overwhelmingly target main. Read
		// its current projection from the newest self-contained anchor (or the
		// exact head cache) once; ProjectEventsForBranch followed by
		// ProjectConversationPayloadForBranch decoded the complete JSONL twice
		// before every prompt and answer append.
		cur, curHead, ok, derr = store.MaterializedConversationHeadFromStore(ref.ArtifactID)
	} else {
		cur, ok, derr = currentConversationPayloadForBranch(store, ref.ArtifactID, ref.BranchID)
	}
	if derr != nil {
		if errors.Is(derr, acf.ErrBranchNotFound) {
			return nil, true, derr
		}
		return nil, true, derr
	}
	if !ok {
		return nil, false, nil
	}
	existingTurns := conversationTextTurns(cur)
	legacyProjectionMatch := len(ref.SanitizedLegacyTurns) > 0 &&
		(acf.TextTurnsEqual(existingTurns, ref.SanitizedLegacyTurns) ||
			textTurnsPrefix(existingTurns, ref.SanitizedLegacyTurns))
	legacyEdgeEchoRepair := ref.BranchID == acf.MainBranch &&
		provenGeneratedLegacyEdgeEchoes(params.SourceAgent, ref, cur.Events, newEvents, existingTurns, newTurns)
	legacyAdjacentEchoRepair := ref.BranchID == acf.MainBranch &&
		provenGeneratedLegacyAdjacentAssistantEcho(params.SourceAgent, ref, cur.Events, newEvents, existingTurns, newTurns)
	generatedEchoOnlyRepair := ref.BranchID == acf.MainBranch &&
		provenGeneratedEchoOnlyProjectionRepair(params.SourceAgent, ref, cur.Events, newEvents, existingTurns, newTurns)
	unprovenAdjacentEcho := cur.Format == acf.ConversationFormatV1 &&
		acf.IsLegacyAdjacentAssistantEchoRepairCleanup(newEvents, cur.Events) &&
		!legacyAdjacentEchoRepair
	portableRepair := (ref.SanitizedPortableProjection &&
		(params.SourceAgent != "codex" || ref.AuthenticatedGeneratedPath) &&
		cur.Format == acf.ConversationFormatV1 &&
		portableConversationEvents(newEvents) &&
		(acf.TextTurnsEqual(existingTurns, newTurns) || legacyProjectionMatch) &&
		(!portableConversationEvents(cur.Events) || !acf.TextTurnsEqual(existingTurns, newTurns))) ||
		legacyEdgeEchoRepair || legacyAdjacentEchoRepair || generatedEchoOnlyRepair
	if acf.TextTurnsEqual(existingTurns, newTurns) && !portableRepair {
		return nil, true, nil // unchanged → LOOP BREAK (no event, no fan-out)
	}
	legacyRepair := false
	if cur.Format == acf.ConversationFormatV1 && !portableRepair {
		legacyEdgeEchoShrink := acf.IsLegacyAssistantEchoCleanup(newEvents, cur.Events)
		if repaired, ok := acf.RepairLegacyRetimestampedConversation(cur.Events, newEvents); ok {
			legacyRepair = true
			newEvents = repaired
			newTurns = acf.ExtractTextTurns(repaired)
			// Remote peer reconciliation has no native marker, so the structural
			// legacy repair itself is sufficient to make clean-vs-dirty unions
			// converge. A local native import has stronger anti-revert duties: it
			// may shrink only with the exact untouched generated-snapshot proof.
			if len(newTurns) < len(existingTurns) && legacyEdgeEchoShrink && !legacyEdgeEchoRepair {
				return nil, true, nil
			}
			if acf.TextTurnsEqual(existingTurns, newTurns) {
				return nil, true, nil
			}
		}
	}
	correctsSyntheticTurn := ref.SanitizedSyntheticTurn &&
		(params.SourceAgent != "codex" || ref.AuthenticatedGeneratedPath) &&
		len(existingTurns) == len(newTurns)+1 &&
		acf.TextTurnsEqual(newTurns, existingTurns[:len(newTurns)])
	// The exact U,A,A,U,A shape is still ambiguous without native-session
	// provenance: two byte-identical adjacent assistant answers can be real.
	// Do not let the stale-continuation fallback reinterpret the clean-looking
	// input as a suffix and append U2/A2 a second time.
	if unprovenAdjacentEcho {
		return nil, true, nil
	}
	// Anti-revert: local/materialized sessions are append-only from Aplexica's
	// point of view. A generated session can legitimately be continued while it
	// is behind the canonical head. In that case the adapter supplies the exact
	// stamped base length+hash: keep the current canonical suffix and append only
	// the native rows written after that base. An unproven divergent rewrite is
	// still suppressed. Shrink exceptions are limited to adapter-authenticated
	// removal of one synthetic terminal bookkeeping turn and the exact legacy
	// generated-edge echo proven above.
	if !textTurnsPrefix(existingTurns, newTurns) && !correctsSyntheticTurn && !legacyRepair && !portableRepair {
		merged, mergeOK := mergeStaleMaterializedContinuation(cur.Events, newEvents, ref)
		if !mergeOK {
			return nil, true, nil
		}
		newEvents = merged
		newTurns = acf.ExtractTextTurns(merged)
	} else if !correctsSyntheticTurn && !legacyRepair && !portableRepair &&
		textTurnsOnlyReplayCurrent(existingTurns, newTurns) {
		// The third door onto the same hole. Reaching here means the incoming
		// copy IS a strict append of the head, which the prefix test reads as a
		// legitimate continuation -- but when everything it adds is turns the
		// head already holds, it is a pre-repair copy re-asserting what the
		// repair removed, not a continuation. Deferral, not deletion: one
		// genuinely new turn in the tail releases it.
		return nil, true, nil
	}
	// Repairs are authorized against the exact materialized projection read
	// above. Any replacement which is not an append of that projection can
	// remove content, so it must never be silently rebased onto a newer main
	// head. Ordinary append/union paths remain free to adopt the latest head.
	contentRemovingRepair := ref.BranchID == acf.MainBranch &&
		cur.Format == acf.ConversationFormatV1 &&
		(portableRepair || correctsSyntheticTurn || legacyRepair) &&
		!conversationEventsPrefix(cur.Events, newEvents)

	// A real continuation: append a delta when the parsed canonical event list
	// is an append of the current canonical thread; otherwise fall back to a
	// full replacement payload to preserve correctness for rewrites/divergence.
	payload, eerr := encode(newEvents)
	fullReplacement := true
	// committedEvents is what this device's EVENT LOG will project after the
	// append. It equals newEvents only for a full replacement; a delta commits
	// cur.Events ++ tail.
	committedEvents := newEvents
	if cur.Format == acf.ConversationFormatV1 {
		if deltaPayload, tail, ok, derr := canonicalConversationAppendPayloadWithTail(cur.Events, newEvents); derr != nil {
			eerr = derr
		} else if ok && deltaPayload != nil {
			payload = deltaPayload
			fullReplacement = false
			committedEvents = append(append([]acf.ConversationEvent(nil), cur.Events...), tail...)
		}
	}
	if eerr != nil {
		return nil, true, eerr
	}
	// Native session encoders carry events but not canonical attachment
	// metadata. A full corrective/replacement payload supersedes the prior
	// state, so explicitly retain its attachments; append deltas inherit them
	// through normal materialization.
	if fullReplacement && cur.Format == acf.ConversationFormatV1 && len(cur.Attachments) > 0 {
		payload, eerr = acf.EncodePayload(acf.ConversationPayload{
			Format:      acf.ConversationFormatV1,
			Events:      newEvents,
			Attachments: append([]acf.Attachment(nil), cur.Attachments...),
		})
		if eerr != nil {
			return nil, true, eerr
		}
	}
	now := time.Now().UTC()
	parentHash, herr := parentHashForThreadRef(store, &existing, ref)
	if herr != nil {
		return nil, true, herr
	}
	if contentRemovingRepair {
		// The deletion proof above is bound to cur. Do not silently rebase that
		// proof onto a continuation which arrived after cur was inspected: doing
		// so would let the full corrective payload erase the new suffix while
		// still passing AppendEvent's normal head check. Compare the current
		// bookkeeping head with the exact effective head that produced cur; a
		// race after this comparison is still rejected by AppendEvent's CAS.
		provenParent := curHead.Hash
		if curHead.Type == acf.EventTypeBaseline && curHead.AlignedHead != "" {
			provenParent = curHead.AlignedHead
		}
		if parentHash != provenParent {
			return nil, true, fmt.Errorf(
				"%w: conversation repair inspected head %q, current head is %q",
				acf.ErrHeadMismatch, provenParent, parentHash,
			)
		}
	}
	ev := acf.Event{
		EventID:    acf.NewID(),
		ArtifactID: ref.ArtifactID,
		Type:       acf.EventTypeUpdate,
		Branch:     ref.BranchID,
		Timestamp:  now,
		Provenance: acf.Provenance{
			DeviceID:       params.DeviceID,
			SourceAgent:    params.SourceAgent,
			AgentVersion:   acf.UnknownAgentVersion,
			AdapterVersion: params.AdapterVersion,
			CausedBy:       CausedByFromContext(ctx),
		},
		Payload:    payload,
		ParentHash: parentHash,
	}
	if legacyAdjacentEchoRepair {
		ev.EventTags = []string{acf.LegacyAdjacentAssistantEchoRepairEventTag}
	}
	// The materialized-branch marker must be written from the fresh artifact
	// state protected by the same append lock as this event. Writing the stale
	// `existing` shell here before AppendEvent could reset a head appended after
	// parentHashForThreadRef, making the stale event appear valid and corrupting
	// the branch chain.
	if aerr := store.AppendEventWithMaterializedBranch(
		acf.KindConversation, ev, params.SourceAgent, ref.BranchID,
	); aerr != nil {
		return nil, true, aerr
	}
	if ref.BranchID == acf.MainBranch {
		// Generated Codex/Claude continuations arrive through this merge path,
		// not the path-keyed native importer. Prime the exact full projection we
		// just COMMITTED so immediate local/remote fan-out can export the prompt
		// (and then the final answer) without replaying a very large historical
		// conversation log. Side-branch projections must not seed the main-head
		// cache.
		//
		// It must be the committed projection, NOT the raw parse: a delta chosen
		// by the TEXT-TURN prefix match keeps the CANONICAL base events, so a
		// materializer's re-stamped base rows are never committed. Caching the
		// raw parse instead published a lane=retained full-state baseline whose
		// base turns carried timestamps that exist in no device's event log; a
		// receiver reconciling it classified convDiverged (conversationEventKey
		// binds the timestamp) and union-merged both copies into a block
		// duplicate.
		store.PrimeMaterializedConversationAtHeadEvent(ref.ArtifactID, ev.EventID, acf.ConversationPayload{
			Format:      acf.ConversationFormatV1,
			Events:      append([]acf.ConversationEvent(nil), committedEvents...),
			Attachments: append([]acf.Attachment(nil), cur.Attachments...),
		})
	}
	return []string{ref.ArtifactID}, true, nil
}

// portableConversationEvents reports whether every stored event belongs in a
// cross-agent transcript. Empty metadata-only slices are not considered a
// usable projection; generated session adapters always provide at least one
// normalized text turn before setting SanitizedPortableProjection.
func portableConversationEvents(events []acf.ConversationEvent) bool {
	if len(events) == 0 {
		return false
	}
	for _, event := range events {
		if event.Type != acf.EventTypeTurn || (event.Role != "user" && event.Role != "assistant") {
			return false
		}
		if _, ok := acf.NormalizeTextTurn(event.Role, conversationEventText(event)); !ok {
			return false
		}
	}
	return true
}

func conversationEventText(event acf.ConversationEvent) string {
	var text string
	for _, block := range event.Content {
		if block.Type != "text" || block.Text == "" {
			continue
		}
		if text != "" {
			text += "\n\n"
		}
		text += block.Text
	}
	return text
}

// provenGeneratedLegacyEdgeEchoes recognizes one exact corruption shape left
// by the pre-v1.0.39 generated-session materialization/reconciliation race:
//
//	clean:   [U1 A1 U2 A2]
//	polluted:[A1 U1 A1 U2 A2 A2]
//
// The first answer was re-timestamped ahead of its prompt and the final answer
// was appended twice. A plain subsequence repair must preserve the longer
// snapshot because it cannot know whether either extra assistant turn is real.
// This merge path has stronger proof: the untouched Aplexica-generated native
// snapshot stamps the complete clean turn count+hash. Codex additionally proves
// that its old and new encoders agree on the clean projection. Only then may
// the two exact edge echoes be removed. Any continuation, different content,
// non-portable row, untrusted native file, or partial/stale base fails closed.
func provenGeneratedLegacyEdgeEchoes(
	sourceAgent string,
	ref ThreadRef,
	currentEvents, incomingEvents []acf.ConversationEvent,
	currentTurns, incomingTurns []acf.TextTurn,
) bool {
	if !ref.GeneratedSnapshot ||
		ref.MaterializedTurnCount != len(incomingTurns) || len(incomingTurns) < 2 ||
		ref.MaterializedTurnsHash == "" ||
		ref.MaterializedTurnsHash != ConversationTurnsHash(incomingTurns) ||
		!portableConversationEvents(currentEvents) || !portableConversationEvents(incomingEvents) ||
		len(currentTurns) != len(incomingTurns)+2 ||
		!alternatingCompletedConversation(incomingTurns) {
		return false
	}
	switch sourceAgent {
	case "codex":
		// Codex's portable encoder is lossy. Require the adapter-authenticated
		// old projection to agree exactly, proving no commentary/tool row is
		// being hidden by the proposed clean snapshot.
		if !ref.AuthenticatedGeneratedPath ||
			!ref.SanitizedPortableProjection || len(ref.SanitizedLegacyTurns) == 0 ||
			!acf.TextTurnsEqual(ref.SanitizedLegacyTurns, incomingTurns) {
			return false
		}
	case "claude-code":
		// Claude's GeneratedSnapshot means every native row carries the same
		// Aplexica thread marker. The full count+hash checks above authenticate
		// the complete portable projection without a separate sanitizer flag.
	default:
		return false
	}
	return acf.IsLegacyAssistantEchoCleanup(incomingEvents, currentEvents)
}

// provenGeneratedLegacyAdjacentAssistantEcho authenticates the one-row
// residual left after the older two-edge cleanup had already removed the
// trailing echo but retained an adjacent copy of the first answer:
//
//	clean:   [U1 A1 U2 A2]
//	polluted:[U1 A1 A1 U2 A2]
//
// Identical adjacent assistant text is not sufficient proof: repeated answers
// are legitimate. Codex must supply the portable projection reconstructed from
// the exact Aplexica-marked rollout bytes, a deterministic generated path and
// session id, plus a valid stamped materialized base. No other local adapter is
// allowed to mint the peer-portable deletion tag. The structural helper then
// compares the complete event bodies, ignoring only the re-authored Timestamp.
func provenGeneratedLegacyAdjacentAssistantEcho(
	sourceAgent string,
	ref ThreadRef,
	currentEvents, incomingEvents []acf.ConversationEvent,
	currentTurns, incomingTurns []acf.TextTurn,
) bool {
	if !portableConversationEvents(currentEvents) || !portableConversationEvents(incomingEvents) ||
		!alternatingCompletedConversation(incomingTurns) ||
		!acf.IsLegacyAdjacentAssistantEchoRepairCleanup(incomingEvents, currentEvents) {
		return false
	}
	if sourceAgent != "codex" {
		return false
	}
	baseCount := ref.MaterializedTurnCount
	if !ref.AuthenticatedGeneratedPath ||
		!ref.SanitizedPortableProjection ||
		len(ref.SanitizedLegacyTurns) == 0 ||
		!acf.TextTurnsEqual(ref.SanitizedLegacyTurns, incomingTurns) ||
		baseCount <= 0 || baseCount > len(incomingTurns) ||
		ref.MaterializedTurnsHash == "" ||
		ref.MaterializedTurnsHash != ConversationTurnsHash(incomingTurns[:baseCount]) {
		return false
	}
	return true
}

func alternatingCompletedConversation(turns []acf.TextTurn) bool {
	if len(turns) < 2 || len(turns)%2 != 0 {
		return false
	}
	for i, turn := range turns {
		want := "user"
		if i%2 == 1 {
			want = "assistant"
		}
		if turn.Role != want {
			return false
		}
	}
	return true
}

// provenGeneratedEchoOnlyProjectionRepair recognizes a conversation polluted
// exclusively by feedback copies from one authenticated generated Codex
// rollout. The clean native projection must:
//
//   - come from the deterministic generated path for this artifact;
//   - retain the exact stamped materialized base;
//   - be a complete alternating user/assistant transcript;
//   - be an ordered subsequence of the current projection; and
//   - account for every current turn by exact role+text.
//
// This is deliberately narrower than generic duplicate removal. A distinct
// prompt, answer, attachment, tool row, or unrecognized event fails closed.
// It repairs the observed U1,U1,A1,A1,U2,... feedback expansion without
// treating legitimate repeated text in arbitrary native conversations as an
// echo.
func provenGeneratedEchoOnlyProjectionRepair(
	sourceAgent string,
	ref ThreadRef,
	currentEvents, incomingEvents []acf.ConversationEvent,
	currentTurns, incomingTurns []acf.TextTurn,
) bool {
	if sourceAgent != "codex" ||
		!ref.AuthenticatedGeneratedPath ||
		!ref.SanitizedPortableProjection ||
		ref.MaterializedTurnCount <= 0 ||
		ref.MaterializedTurnCount > len(incomingTurns) ||
		ref.MaterializedTurnsHash == "" ||
		ref.MaterializedTurnsHash != ConversationTurnsHash(incomingTurns[:ref.MaterializedTurnCount]) ||
		len(ref.SanitizedLegacyTurns) <= len(incomingTurns) ||
		len(currentTurns) <= len(incomingTurns) ||
		!alternatingCompletedConversation(incomingTurns) ||
		!plainPortableTextEvents(currentEvents) ||
		!plainPortableTextEvents(incomingEvents) ||
		!textTurnSequenceSubsequence(incomingTurns, currentTurns) ||
		!textTurnSequenceSubsequence(ref.SanitizedLegacyTurns, currentTurns) {
		return false
	}
	allowed := make(map[string]struct{}, len(incomingTurns))
	for _, turn := range incomingTurns {
		allowed[textTurnEchoKey(turn)] = struct{}{}
	}
	for _, turn := range currentTurns {
		if _, ok := allowed[textTurnEchoKey(turn)]; !ok {
			return false
		}
	}
	return true
}

func plainPortableTextEvents(events []acf.ConversationEvent) bool {
	if len(events) == 0 {
		return false
	}
	for _, event := range events {
		if event.Type != acf.EventTypeTurn ||
			(event.Role != "user" && event.Role != "assistant") ||
			len(event.Content) != 1 ||
			event.Content[0].Type != "text" ||
			event.Content[0].Text == "" ||
			len(event.NativeExtras) != 0 ||
			len(event.Tags) != 0 {
			return false
		}
	}
	return true
}

func textTurnSequenceSubsequence(subsequence, full []acf.TextTurn) bool {
	if len(subsequence) > len(full) {
		return false
	}
	next := 0
	for _, turn := range full {
		if next < len(subsequence) &&
			subsequence[next].Role == turn.Role &&
			subsequence[next].Text == turn.Text {
			next++
		}
	}
	return next == len(subsequence)
}

func textTurnEchoKey(turn acf.TextTurn) string {
	return turn.Role + "\x00" + turn.Text
}

// mergeStaleMaterializedContinuation linearizes a continuation authored from a
// provably older generated snapshot. Example:
//
//	canonical: [A B C D]
//	generated base: [A B]
//	native after user continuation: [A B X Y]
//	result: [A B C D X Y]
//
// The stamped base must be a strict prefix of the current canonical state and
// its hash must still match. This deliberately does not merge arbitrary edits
// inside old turns: without that proof, suppressing the write is safer than
// duplicating or replacing history.
//
// The stamped base can also be older than the FILE, not just older than
// canonical: a native continuation this device already published still counts
// as an unstamped row, so re-importing the unchanged file after canonical has
// moved on would otherwise append that continuation a second time. The result
// must therefore be idempotent under repeated re-import, which the scan cache
// makes routine (a schema bump re-imports every generated mirror at startup,
// and the hot scanners bypass the recursion guard). Everything the current
// projection already holds is stripped off the front of the native suffix
// before anything is appended; an entirely echoed suffix is no continuation at
// all and suppresses the write.
func mergeStaleMaterializedContinuation(
	currentEvents, incomingEvents []acf.ConversationEvent,
	ref ThreadRef,
) ([]acf.ConversationEvent, bool) {
	baseCount := ref.MaterializedTurnCount
	if baseCount <= 0 {
		return nil, false
	}
	currentTurns := acf.ExtractTextTurns(currentEvents)
	incomingTurns := acf.ExtractTextTurns(incomingEvents)
	if baseCount >= len(incomingTurns) || baseCount >= len(currentTurns) {
		return nil, false // no native suffix, or the base is not actually stale
	}
	baseTurns := incomingTurns[:baseCount]
	if ref.MaterializedTurnsHash == "" ||
		ref.MaterializedTurnsHash != ConversationTurnsHash(baseTurns) ||
		!acf.TextTurnsEqual(baseTurns, currentTurns[:baseCount]) {
		return nil, false
	}
	tail, ok := conversationEventTailAfterVisibleTurns(
		incomingEvents, echoedContinuationBase(currentTurns, incomingTurns, baseCount))
	if !ok {
		return nil, false
	}
	tail = dropReplayedLeadingEvents(currentEvents, tail)
	if len(acf.ExtractTextTurns(tail)) == 0 {
		return nil, false // the whole native suffix is already canonical
	}
	merged := make([]acf.ConversationEvent, 0, len(currentEvents)+len(tail))
	merged = append(merged, currentEvents...)
	merged = append(merged, tail...)
	return merged, true
}

// echoedContinuationBase advances the stamped base past the leading native
// turns the canonical thread already holds in the same position.
//
// A materialized session file is `canonical-as-of-materialization ++ user
// continuation`. When the stamp is older than the file — the file grew but the
// materializer could not rewrite it, so the base stamp was never refreshed —
// the turns between the stamped base and the real continuation are turns this
// device already published. They line up positionally with the canonical suffix
// because that is where they came from, so a role+text prefix match identifies
// them without needing timestamps to have survived the round trip (the Claude
// materializer re-authors every row timestamp from device-local bookkeeping).
//
// Positional matching is also why this cannot swallow a genuine continuation:
// the first native turn that diverges from canonical stops the scan, and
// everything from there on is appended.
func echoedContinuationBase(currentTurns, incomingTurns []acf.TextTurn, baseCount int) int {
	base := baseCount
	for base < len(currentTurns) && base < len(incomingTurns) && currentTurns[base] == incomingTurns[base] {
		base++
	}
	return base
}

// dropReplayedLeadingEvents removes leading events the current projection
// already contains verbatim.
//
// This is the position-independent companion to echoedContinuationBase: it
// still recognizes an echo whose canonical neighbours have since been
// reordered, which is what an already-duplicated thread looks like. Identity is
// the COMPLETE canonical body including Timestamp, so two events can only match
// when they are literally the same logical event replayed — a distinct turn
// would have to carry byte-identical text at the same millisecond.
//
// Only the front is trimmed. A repeat further inside the suffix is separated
// from the base by content this device has not seen, which is not an echo of a
// stale base and is not this function's to judge.
func dropReplayedLeadingEvents(currentEvents, tail []acf.ConversationEvent) []acf.ConversationEvent {
	if len(tail) == 0 || len(currentEvents) == 0 {
		return tail
	}
	present := make(map[string]struct{}, len(currentEvents))
	for _, ev := range currentEvents {
		if identity, ok := conversationEventIdentity(ev); ok {
			present[identity] = struct{}{}
		}
	}
	for i, ev := range tail {
		identity, ok := conversationEventIdentity(ev)
		if !ok {
			return tail[i:]
		}
		if _, replayed := present[identity]; !replayed {
			return tail[i:]
		}
	}
	return nil
}

// conversationEventIdentity fingerprints the complete canonical body of one
// conversation event, Timestamp included. Encoding failure reports ok=false so
// callers fail closed rather than treating an unencodable event as equal to
// anything.
//
// It is deliberately NOT syncd's conversationEventKey: that key hashes only
// role, type and content, so two distinct parallel tool calls sharing their
// assistant message's timestamp collide. Anything that removes content needs
// the whole body.
func conversationEventIdentity(ev acf.ConversationEvent) (string, bool) {
	encoded, err := json.Marshal(ev)
	if err != nil {
		return "", false
	}
	return string(encoded), true
}

func conversationEventTailAfterVisibleTurns(events []acf.ConversationEvent, count int) ([]acf.ConversationEvent, bool) {
	if count <= 0 {
		return events, true
	}
	seen := 0
	for i := range events {
		seen += len(acf.ExtractTextTurns(events[i : i+1]))
		if seen == count {
			return events[i+1:], true
		}
		if seen > count {
			return nil, false
		}
	}
	return nil, false
}

// latestContentEvent scans events backward and returns the most recent
// content-bearing event — a create, update, resolution, or FR-02.32
// payload-bearing snapshot — i.e. the one whose payload holds the materialized
// conversation turns. A payload-LESS snapshot (legacy, Payload null) carries no
// turns and is skipped, as are redaction/fork/merge heads. Returns ok=false
// when no content event exists.
//
// The merge path deliberately wants the policy-free walk: a redaction head is
// walked PAST to decode the current turns for the loop-break comparison (this is
// dedup, not content removal). That is exactly acf.LatestPayloadEvent's contract,
// so this delegates straight to it (no redaction barrier).
//
// Recognizing payload-bearing snapshots matters after an on-snapshot prune: the
// active log can be a snapshot ALONE (the create/update was compacted away), and
// that snapshot now carries the materialized payload. Without this the loop-break
// comparison falls back to a duplicate native import for every pruned thread.
func latestContentEvent(events []acf.Event) (acf.Event, bool) {
	return acf.LatestPayloadEvent(events)
}

func currentConversationPayload(store *acf.Store, artifactID string, events []acf.Event) (acf.ConversationPayload, bool, error) {
	cur, ok, err := acf.MaterializedConversationPayload(events)
	if err != nil || ok {
		return cur, ok, err
	}
	merged, merr := store.ReadEventsIncludingCompacted(acf.KindConversation, artifactID)
	if merr != nil {
		return acf.ConversationPayload{}, false, merr
	}
	return acf.MaterializedConversationPayload(merged)
}

func currentConversationPayloadForBranch(store *acf.Store, artifactID, branchID string) (acf.ConversationPayload, bool, error) {
	payload, _, ok, err := store.ProjectConversationPayloadForBranch(artifactID, branchID, acf.BranchProjectionOpts{})
	return payload, ok, err
}

func parentHashForThreadRef(store *acf.Store, art *acf.Artifact, ref ThreadRef) (string, error) {
	if ref.BranchID == acf.MainBranch {
		return RefreshMainBranchHead(store, acf.KindConversation, art)
	}
	head, ok, err := store.BranchHeadEvent(acf.KindConversation, ref.ArtifactID, ref.BranchID)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("%w: %s", acf.ErrBranchNotFound, ref.BranchID)
	}
	return head.Hash, nil
}

func conversationTextTurns(p acf.ConversationPayload) []acf.TextTurn {
	turns, _ := acf.ConversationTextTurns(p)
	return turns
}

// textTurnsPrefix reports whether a is a STRICT prefix of b: shorter than b, with
// every turn of a equal to the corresponding leading turn of b. The anti-revert
// guard uses it to recognize a stale, shorter conversation copy.
func textTurnsPrefix(a, b []acf.TextTurn) bool {
	if len(a) >= len(b) {
		return false
	}
	return acf.TextTurnsEqual(a, b[:len(a)])
}

// WouldRevertThread reports whether appending newTurns to a conversation
// artifact would REVERT it — i.e. the artifact's current visible turns are not
// a prefix of the incoming visible turns. Adapters that reconcile conversations
// by source-path (the hermes/kilo SQLite imports) call this before appending an
// update so a stale or divergent re-import can't overwrite a newer continuation
// that arrived from another agent. Returns false when the artifact is missing
// or its head can't be decoded, so the normal import path still runs.
func WouldRevertThread(store *acf.Store, artifactID string, newTurns []acf.TextTurn) bool {
	return WouldRevertThreadRef(store, ThreadRef{ArtifactID: artifactID, BranchID: acf.MainBranch}, newTurns)
}

func WouldRevertThreadRef(store *acf.Store, ref ThreadRef, newTurns []acf.TextTurn) bool {
	ref, err := NormalizeThreadRef(ref)
	if err != nil {
		return false
	}
	if _, err := store.ReadArtifact(acf.KindConversation, ref.ArtifactID); err != nil {
		return false
	}
	cur, ok, derr := currentConversationPayloadForBranch(store, ref.ArtifactID, ref.BranchID)
	if derr != nil || !ok {
		return false
	}
	currentTurns := conversationTextTurns(cur)
	if len(currentTurns) == 0 {
		return false
	}
	if !textTurnsPrefixOrEqual(currentTurns, newTurns) {
		return true
	}
	// A prefix relation alone used to mean "this is a legitimate continuation".
	// It is an append-only test, and it waves through the one shape that undoes
	// a repair: a stale native copy is a strict SUPERSET of the repaired head,
	// so it passes the prefix test and re-asserts the very duplicates the head
	// deliberately no longer holds. That is how a hermes re-import silently
	// reverted a repaired conversation on 2026-07-27.
	//
	// Reject when the incoming side's extra turns are ENTIRELY turns the
	// current head already contains — a replay, not a continuation. Suppression
	// here is deferral, not deletion: one genuinely new turn in the tail makes
	// this false again and the import proceeds normally.
	return textTurnsOnlyReplayCurrent(currentTurns, newTurns)
}

// textTurnsOnlyReplayCurrent reports whether newTurns extends currentTurns by
// nothing but turns currentTurns already holds.
func textTurnsOnlyReplayCurrent(currentTurns, newTurns []acf.TextTurn) bool {
	if len(newTurns) <= len(currentTurns) {
		return false
	}
	present := make(map[acf.TextTurn]struct{}, len(currentTurns))
	for _, turn := range currentTurns {
		present[turn] = struct{}{}
	}
	for _, turn := range newTurns[len(currentTurns):] {
		if _, seen := present[turn]; !seen {
			return false
		}
	}
	return true
}
