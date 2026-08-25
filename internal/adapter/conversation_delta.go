package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/atomicfile"
)

// ConversationEventParser parses native session bytes into the full canonical
// event list represented by that file.
type ConversationEventParser func(content []byte) ([]acf.ConversationEvent, error)

// ConversationCanonicalDecoder renders a canonical event list into an adapter's
// native session bytes.
type ConversationCanonicalDecoder func(events []acf.ConversationEvent) ([]byte, error)

// ErrConversationImportNoop is returned internally when a canonical
// conversation import is a stale or equivalent snapshot and must not append a
// replacement event. ImportCanonicalConversation converts it back into a
// successful identity-preserving no-op.
var ErrConversationImportNoop = errors.New("adapter: conversation import has no newer turns")

// ErrConversationImportDiverged marks the ONE case a native import is refused
// without either side being repairable by the other: canonical and the native
// file each hold a turn the other lacks, and conversationDivergentNativeTail
// could not absorb the difference.
//
// It WRAPS ErrConversationImportNoop, so every existing errors.Is caller is
// unaffected, while the condition stops being indistinguishable from an
// ordinary stale re-read. That distinction is the point: this is the state in
// which the native file holds turns canonical will never learn about by
// importing this file again, and both import entry points still convert it into
// a successful identity-preserving no-op (returning a real error there would
// make the caller read the file as not-committed and re-probe it forever). The
// operator-visible surface for it is the materialize side, which independently
// reports SessionDeclineDiverged for the same pair and escalates it.
var ErrConversationImportDiverged = fmt.Errorf(
	"%w: native and canonical each hold turns the other lacks", ErrConversationImportNoop)

// EncodeCanonicalConversationDeltaPayload stores only newly appended canonical
// conversation events. Readers compose it by replaying the artifact event log.
func EncodeCanonicalConversationDeltaPayload(events []acf.ConversationEvent) (json.RawMessage, error) {
	return acf.EncodePayload(acf.ConversationPayload{Format: acf.ConversationDeltaFormatV1, Events: events})
}

// ImportCanonicalConversation imports a canonical-mode session while avoiding
// full-thread update payloads for append-only growth. Creates still carry a full
// acf.conversation.v1 payload; updates carry acf.conversation.delta.v1 when the
// new parsed event list is an append of the current materialized thread.
func ImportCanonicalConversation(
	ctx context.Context,
	store *acf.Store,
	params OpaqueParams,
	nativePath string,
	parse ConversationEventParser,
) ([]string, error) {
	abs, err := filepath.Abs(nativePath)
	if err != nil {
		return nil, fmt.Errorf("adapter: resolve path: %w", err)
	}
	var parsedEvents []acf.ConversationEvent
	var primePayload acf.ConversationPayload
	var primeOK bool
	encoder := func(content []byte) (json.RawMessage, error) {
		var parseErr error
		parsedEvents, parseErr = parse(content)
		if parseErr != nil {
			return nil, parseErr
		}
		payload, payloadErr := canonicalConversationPayloadForEvents(store, abs, parsedEvents, params.SourceAgent)
		if payloadErr == nil {
			primePayload, primeOK = materializedConversationProjectionAfterImport(store, abs, payload)
		}
		return payload, payloadErr
	}
	ids, headEventID, err := ImportOpaqueWithHeadEvent(ctx, store, acf.KindConversation, params, nativePath, encoder)
	if err == nil && len(ids) == 1 && parsedEvents != nil && primeOK {
		store.PrimeMaterializedConversationAtHeadEvent(
			ids[0], headEventID, primePayload,
		)
	}
	if err != nil && errors.Is(err, ErrConversationImportNoop) {
		existing, found, ferr := store.FindBySourcePath(acf.KindConversation, abs)
		if ferr != nil {
			return nil, ferr
		}
		if found {
			return []string{existing.ArtifactID}, nil
		}
	}
	return ids, err
}

// ConversationFileParser parses a native conversation directly from its path.
// Unlike ConversationEventParser it may keep a per-path byte offset and read
// only an append-only file's newly-written tail. This is important for active
// Codex/Claude transcripts that can grow to hundreds of megabytes: routing the
// import through os.ReadFile on every watcher settle turns a tiny append into a
// full-file read and parse.
type ConversationFileParser func(path string) ([]acf.ConversationEvent, error)

// ImportCanonicalConversationFile is the path-streaming counterpart to
// ImportCanonicalConversation. Identity, event creation, delta encoding, and
// rollback semantics stay in ImportOpaqueContent; only the native parse is
// delegated to a parser that can read incrementally from disk.
func ImportCanonicalConversationFile(
	ctx context.Context,
	store *acf.Store,
	params OpaqueParams,
	nativePath string,
	parseFile ConversationFileParser,
) ([]string, error) {
	abs, err := filepath.Abs(nativePath)
	if err != nil {
		return nil, fmt.Errorf("adapter: resolve path: %w", err)
	}
	var parsedEvents []acf.ConversationEvent
	var primePayload acf.ConversationPayload
	var primeOK bool
	encoder := func(_ []byte) (json.RawMessage, error) {
		newEvents, perr := parseFile(abs)
		if perr != nil {
			return nil, perr
		}
		parsedEvents = newEvents
		payload, payloadErr := canonicalConversationPayloadForEvents(store, abs, newEvents, params.SourceAgent)
		if payloadErr == nil {
			primePayload, primeOK = materializedConversationProjectionAfterImport(store, abs, payload)
		}
		return payload, payloadErr
	}
	ids, headEventID, err := ImportOpaqueContentWithHeadEvent(ctx, store, acf.KindConversation, params, abs, nil, encoder)
	if err == nil && len(ids) == 1 && parsedEvents != nil && primeOK {
		store.PrimeMaterializedConversationAtHeadEvent(
			ids[0], headEventID, primePayload,
		)
	}
	if err != nil && errors.Is(err, ErrConversationImportNoop) {
		existing, found, ferr := store.FindBySourcePath(acf.KindConversation, abs)
		if ferr != nil {
			return nil, ferr
		}
		if found {
			return []string{existing.ArtifactID}, nil
		}
	}
	return ids, err
}

// materializedConversationProjectionAfterImport composes the exact full state
// that payload will produce if ImportOpaqueContent appends it to the current
// source-path artifact. Native parsers do not carry canonical attachments, so
// priming the cache from parsed events alone would make immediate fan-out see a
// projection with every previously synced attachment missing. Delta imports
// retain the current attachments and any portable prefix that is absent from a
// native suffix-overlap snapshot. Full payloads remain self-contained.
//
// This is only a cache hint. Any lookup/materialization failure returns false
// and leaves the persisted event log as the sole source of truth.
func materializedConversationProjectionAfterImport(
	store *acf.Store,
	abs string,
	payload json.RawMessage,
) (acf.ConversationPayload, bool) {
	var next acf.ConversationPayload
	if err := json.Unmarshal(payload, &next); err != nil {
		return acf.ConversationPayload{}, false
	}
	switch next.Format {
	case acf.ConversationFormatV1:
		return acf.ConversationPayload{
			Format:      acf.ConversationFormatV1,
			Events:      append([]acf.ConversationEvent(nil), next.Events...),
			Attachments: append([]acf.Attachment(nil), next.Attachments...),
		}, true
	case acf.ConversationDeltaFormatV1:
	default:
		return acf.ConversationPayload{}, false
	}

	art, found, err := store.FindBySourcePath(acf.KindConversation, abs)
	if err != nil || !found {
		return acf.ConversationPayload{}, false
	}
	current, ok, err := store.MaterializedConversationPayloadFromStore(art.ArtifactID)
	if err != nil || !ok || current.Format != acf.ConversationFormatV1 {
		return acf.ConversationPayload{}, false
	}
	// canonicalConversationPayloadForEvents reuses the current head payload for
	// an exact/no-new-turn import. ImportOpaqueContent then performs a no-op, so
	// applying that same delta a second time here would duplicate its events.
	head, hasHead, err := store.LastEvent(acf.KindConversation, art.ArtifactID)
	if err != nil {
		return acf.ConversationPayload{}, false
	}
	if hasHead && bytes.Equal(head.Payload, payload) {
		return current, true
	}
	current.Events = append(current.Events, next.Events...)
	current.Attachments = append(current.Attachments, next.Attachments...)
	current.Format = acf.ConversationFormatV1
	return current, true
}

// RepairCanonicalConversationProjection replaces a legacy full conversation
// projection only when the current canonical event sequence is an exact prefix
// of the legacy sequence reconstructed from the same native source bytes. This
// is an upgrade-only escape hatch for an adapter that used to import rows which
// are now known to be execution-local (for example Codex developer/system and
// commentary messages). The prefix proof prevents a stale native file from
// erasing a continuation that arrived from another device.
//
// repaired is false when no source-path artifact exists, the artifact is not a
// canonical v1 conversation, or the proof does not match. In those cases the
// caller must continue through its normal import path.
func RepairCanonicalConversationProjection(
	ctx context.Context,
	store *acf.Store,
	params OpaqueParams,
	nativePath string,
	legacyEvents, cleanEvents []acf.ConversationEvent,
) (ids []string, repaired bool, err error) {
	if err := ctx.Err(); err != nil {
		return nil, false, fmt.Errorf("adapter: repair conversation projection cancelled: %w", err)
	}
	abs, err := filepath.Abs(nativePath)
	if err != nil {
		return nil, false, fmt.Errorf("adapter: resolve repair path: %w", err)
	}
	art, found, err := store.FindBySourcePath(acf.KindConversation, abs)
	if err != nil || !found {
		return nil, false, err
	}
	current, provenHead, ok, err := store.MaterializedConversationHeadFromStore(art.ArtifactID)
	if err != nil || !ok || current.Format != acf.ConversationFormatV1 {
		return nil, false, err
	}
	replacement, proven := SanitizedConversationProjection(current.Events, legacyEvents, cleanEvents)
	if !proven {
		return nil, false, nil
	}
	if conversationEventsEqual(current.Events, replacement) {
		return []string{art.ArtifactID}, true, nil
	}
	ids, err = ReplaceCanonicalConversationProjectionAtHead(
		ctx, store, params, art.ArtifactID, provenHead, replacement, current.Attachments,
	)
	return ids, true, err
}

// ReplaceCanonicalConversationProjectionAtHead appends a self-contained full
// replacement only on the exact main-branch head the caller inspected. The
// store's ParentHash check is the compare-and-swap: an intervening local or
// remote continuation makes the append fail with acf.ErrHeadMismatch rather
// than being erased by the replacement.
func ReplaceCanonicalConversationProjectionAtHead(
	ctx context.Context,
	store *acf.Store,
	params OpaqueParams,
	artifactID string,
	provenHead acf.Event,
	replacement []acf.ConversationEvent,
	attachments []acf.Attachment,
) ([]string, error) {
	payload, err := acf.EncodePayload(acf.ConversationPayload{
		Format:      acf.ConversationFormatV1,
		Events:      replacement,
		Attachments: append([]acf.Attachment(nil), attachments...),
	})
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	parentHash := provenHead.Hash
	if provenHead.Type == acf.EventTypeBaseline && provenHead.AlignedHead != "" {
		parentHash = provenHead.AlignedHead
	}
	event := acf.Event{
		EventID:    acf.NewID(),
		ArtifactID: artifactID,
		Type:       acf.EventTypeUpdate,
		Branch:     acf.MainBranch,
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
	if err := store.AppendEvent(acf.KindConversation, event); err != nil {
		return nil, err
	}
	store.PrimeMaterializedConversationAtHeadEvent(artifactID, event.EventID, acf.ConversationPayload{
		Format:      acf.ConversationFormatV1,
		Events:      replacement,
		Attachments: append([]acf.Attachment(nil), attachments...),
	})
	return []string{artifactID}, nil
}

// SanitizedConversationProjection proves and constructs the two historical
// layouts produced when a native source was merged into a portable thread:
//
//   - current ends with a prefix of legacy (an older source snapshot, possibly
//     preceded by remote/import metadata); or
//   - current contains the complete legacy sequence (possibly surrounded by
//     remote events).
//
// Only the exact source-derived segment is replaced. Unknown prefix/suffix
// events are retained byte-for-byte. clean must itself be a strict ordered
// subsequence of legacy, proving this is sanitation rather than an arbitrary
// rewrite.
func SanitizedConversationProjection(current, legacy, clean []acf.ConversationEvent) ([]acf.ConversationEvent, bool) {
	if len(current) == 0 || len(legacy) == 0 || len(clean) >= len(legacy) ||
		!conversationEventsSubsequence(clean, legacy) {
		return nil, false
	}
	if start := conversationEventSequenceIndex(current, legacy); start >= 0 {
		out := make([]acf.ConversationEvent, 0, len(current)-len(legacy)+len(clean))
		out = append(out, current[:start]...)
		out = append(out, clean...)
		out = append(out, current[start+len(legacy):]...)
		return out, true
	}
	// Find a current suffix that is an exact legacy prefix. Candidate starts
	// are narrowed by the first event, avoiding an O(n²) decrementing search on
	// multi-thousand-event transcripts.
	for start := 0; start < len(current); start++ {
		overlap := len(current) - start
		if overlap > len(legacy) || !conversationEventEquivalent(current[start], legacy[0]) {
			continue
		}
		if !conversationEventSequencesEqual(current[start:], legacy[:overlap]) {
			continue
		}
		out := make([]acf.ConversationEvent, 0, start+len(clean))
		out = append(out, current[:start]...)
		out = append(out, clean...)
		return out, true
	}
	return nil, false
}

func conversationEventsSubsequence(subsequence, full []acf.ConversationEvent) bool {
	if len(subsequence) > len(full) {
		return false
	}
	next := 0
	for i := range full {
		if next < len(subsequence) && conversationEventEquivalent(subsequence[next], full[i]) {
			next++
		}
	}
	return next == len(subsequence)
}

func conversationEventSequenceIndex(full, sequence []acf.ConversationEvent) int {
	if len(sequence) == 0 || len(sequence) > len(full) {
		return -1
	}
	for start := 0; start+len(sequence) <= len(full); start++ {
		if !conversationEventEquivalent(full[start], sequence[0]) ||
			!conversationEventEquivalent(full[start+len(sequence)-1], sequence[len(sequence)-1]) {
			continue
		}
		if conversationEventSequencesEqual(full[start:start+len(sequence)], sequence) {
			return start
		}
	}
	return -1
}

func canonicalConversationPayloadForEvents(
	store *acf.Store,
	abs string,
	newEvents []acf.ConversationEvent,
	sourceAgent string,
) (json.RawMessage, error) {
	// Full-session encoding can itself be hundreds of megabytes. Keep it lazy:
	// the normal update path proves continuity from the current head delta and
	// emits only the appended tail, so allocating/serializing the whole thread
	// first would erase most of the incremental parser's CPU/memory benefit.
	var fullPayload json.RawMessage
	full := func() (json.RawMessage, error) {
		if fullPayload != nil {
			return fullPayload, nil
		}
		var err error
		fullPayload, err = EncodeCanonicalConversationPayload(newEvents)
		return fullPayload, err
	}
	existing, found, err := store.FindBySourcePath(acf.KindConversation, abs)
	if err != nil || !found {
		if err != nil {
			return nil, err
		}
		return full()
	}
	if head, hasHead, herr := lastConversationEvent(store, existing.ArtifactID); herr == nil && hasHead && acf.HasPayload(head.Payload) {
		current, derr := acf.DecodeConversationPayload(head)
		if derr == nil {
			switch current.Format {
			case acf.ConversationFormatV1:
				if payload, ok, perr := canonicalConversationAppendPayload(current.Events, newEvents); ok || perr != nil {
					if ok && perr == nil && payload == nil {
						return nil, ErrConversationImportNoop
					}
					return payload, perr
				}
			case acf.ConversationDeltaFormatV1:
				// The common hot-session path ends in a delta written by this
				// same adapter. Locate that exact delta in the freshly parsed
				// native session and encode only what follows it. This proves
				// the source includes the current head without replaying the
				// artifact's potentially multi-gigabyte historical JSONL log.
				if sourceAgent != "" && head.Provenance.SourceAgent == sourceAgent {
					if tail, anchored := conversationTailAfterEventSequence(current.Events, newEvents); anchored {
						if len(tail) == 0 {
							return nil, ErrConversationImportNoop
						}
						return EncodeCanonicalConversationDeltaPayload(tail)
					}
				}
			}
		}
	}
	current, ok, err := store.MaterializedConversationPayloadFromStore(existing.ArtifactID)
	if err != nil {
		return full()
	}
	if !ok || current.Format != acf.ConversationFormatV1 {
		return full()
	}
	if payload, ok, perr := canonicalConversationAppendPayload(current.Events, newEvents); ok || perr != nil {
		if ok && payload == nil {
			return nil, ErrConversationImportNoop
		}
		return payload, perr
	}
	if conversationImportWouldRegress(current.Events, newEvents) {
		// The anti-regression guard is unchanged and still runs FIRST. Only after
		// it has already refused does the append-only absorb get a chance: it
		// preserves canonical's events verbatim and adds nothing canonical
		// already holds, so it can revert or reorder nothing — which is exactly
		// what this guard exists to prevent. Without it a native file holding
		// turns canonical lacks is refused on every pass forever and those turns
		// are never learned.
		if tail, ok := conversationDivergentNativeTail(current.Events, newEvents); ok {
			return EncodeCanonicalConversationDeltaPayload(tail)
		}
		return nil, fmt.Errorf("%w: current has %d visible turns/%d events; import has %d visible turns/%d events",
			ErrConversationImportDiverged,
			len(acf.ExtractTextTurns(current.Events)),
			len(current.Events),
			len(acf.ExtractTextTurns(newEvents)),
			len(newEvents),
		)
	}
	return full()
}

func lastConversationEvent(store *acf.Store, artifactID string) (acf.Event, bool, error) {
	return store.LastEvent(acf.KindConversation, artifactID)
}

// conversationTailAfterEventSequence returns the events following the LAST
// exact occurrence of anchor in full. Choosing the last match handles repeated
// tool/status events conservatively. An empty anchor is never evidence of a
// shared head and therefore does not match.
func conversationTailAfterEventSequence(anchor, full []acf.ConversationEvent) ([]acf.ConversationEvent, bool) {
	if len(anchor) == 0 || len(anchor) > len(full) {
		return nil, false
	}
	for start := len(full) - len(anchor); start >= 0; start-- {
		// Most canonical events have a timestamp/call-id/type identity. Check
		// those scalar fields before marshaling a candidate so a large active
		// transcript remains an O(events) scan with only the rare plausible
		// match paying the JSON cost.
		if !conversationEventIdentityEqual(anchor[0], full[start]) ||
			!conversationEventIdentityEqual(anchor[len(anchor)-1], full[start+len(anchor)-1]) {
			continue
		}
		if conversationEventSequencesEqual(anchor, full[start:start+len(anchor)]) {
			return full[start+len(anchor):], true
		}
	}
	return nil, false
}

// conversationEventIdentityEqual is a cheap candidate filter, not the final
// equality decision. Content, tags, and opaque native fields are deliberately
// excluded here and compared by their canonical JSON wire form above.
func conversationEventIdentityEqual(a, b acf.ConversationEvent) bool {
	return a.Type == b.Type &&
		a.Timestamp.Equal(b.Timestamp) &&
		a.Role == b.Role &&
		a.CallID == b.CallID &&
		a.ToolName == b.ToolName &&
		a.IsError == b.IsError &&
		a.BranchID == b.BranchID &&
		a.SourceEventID == b.SourceEventID &&
		a.SnapshotState == b.SnapshotState &&
		len(a.Content) == len(b.Content) &&
		len(a.Tags) == len(b.Tags) &&
		len(a.MergedBranchIDs) == len(b.MergedBranchIDs)
}

func conversationEventSequencesEqual(a, b []acf.ConversationEvent) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !conversationEventEquivalent(a[i], b[i]) {
			return false
		}
	}
	return true
}

func conversationEventEquivalent(a, b acf.ConversationEvent) bool {
	if !conversationEventIdentityEqual(a, b) {
		return false
	}
	for i := range a.Content {
		if a.Content[i] != b.Content[i] {
			return false
		}
	}
	for i := range a.Tags {
		if a.Tags[i] != b.Tags[i] {
			return false
		}
	}
	for i := range a.MergedBranchIDs {
		if a.MergedBranchIDs[i] != b.MergedBranchIDs[i] {
			return false
		}
	}
	return rawJSONEquivalent(a.Input, b.Input) && rawJSONEquivalent(a.NativeExtras, b.NativeExtras)
}

func rawJSONEquivalent(a, b json.RawMessage) bool {
	if bytes.Equal(a, b) {
		return true
	}
	if len(a) == 0 || len(b) == 0 || !json.Valid(a) || !json.Valid(b) {
		return false
	}
	var decodedA, decodedB any
	decoderA := json.NewDecoder(bytes.NewReader(a))
	decoderA.UseNumber()
	decoderB := json.NewDecoder(bytes.NewReader(b))
	decoderB.UseNumber()
	if decoderA.Decode(&decodedA) != nil || decoderB.Decode(&decodedB) != nil {
		return false
	}
	return reflect.DeepEqual(decodedA, decodedB)
}

func canonicalConversationAppendPayload(currentEvents, newEvents []acf.ConversationEvent) (json.RawMessage, bool, error) {
	payload, _, ok, err := canonicalConversationAppendPayloadWithTail(currentEvents, newEvents)
	return payload, ok, err
}

// canonicalConversationAppendPayloadWithTail additionally returns the events the
// returned delta actually commits. Callers that cache a post-append projection
// MUST compose currentEvents ++ tail: when the delta was selected by a
// TEXT-TURN prefix match (role+text only), newEvents may differ from the
// canonical base in fields the text comparison ignores — notably the
// per-row timestamps a materializer re-stamps — so newEvents is NOT what the
// event log projects after the append. tail is nil when no delta was produced.
// A nil payload with ok=true means "nothing new to write". It must never be
// signalled by handing back the CURRENT HEAD'S OWN BYTES.
//
// That is what this used to do, and it was a time-of-check/time-of-use race
// with a very bad failure mode. The caller resolved "no change" by comparing
// those bytes against the head it re-read later (ImportOpaqueContent), and
// nothing bound the two reads. In the regression scenario a Claude Code scan
// read a one-turn-delta head and returned that payload as "unchanged" while a
// peer answer arrived inside the parse window; the byte comparison then ran
// against the newer head, saw a difference, and committed the stale sentinel as a real
// update. Because a hot conversation's head is a one-turn DELTA, appending it
// REPLAYS that turn -- canonical timestamp and all -- so the thread grew a
// duplicated question with no answer, which then replicated to every device.
//
// Returning nil is unambiguous under any interleaving: the callers convert it
// to ErrConversationImportNoop, which both import entry points already unwrap
// into a successful identity-preserving no-op.
func canonicalConversationAppendPayloadWithTail(
	currentEvents, newEvents []acf.ConversationEvent,
) (json.RawMessage, []acf.ConversationEvent, bool, error) {
	if conversationEventsEqual(currentEvents, newEvents) {
		return nil, nil, true, nil
	}
	if conversationEventsPrefix(currentEvents, newEvents) {
		tail := newEvents[len(currentEvents):]
		payload, err := EncodeCanonicalConversationDeltaPayload(tail)
		return payload, tail, true, err
	}
	if tail, ok := conversationTailAfterTextTurnPrefix(currentEvents, newEvents); ok {
		payload, err := EncodeCanonicalConversationDeltaPayload(tail)
		return payload, tail, true, err
	}
	if tail, ok := conversationTailAfterTextTurnSuffixOverlap(currentEvents, newEvents); ok {
		if len(tail) == 0 {
			return nil, nil, true, nil
		}
		payload, err := EncodeCanonicalConversationDeltaPayload(tail)
		return payload, tail, true, err
	}
	currentTurns := acf.ExtractTextTurns(currentEvents)
	newTurns := acf.ExtractTextTurns(newEvents)
	// A stale or equal COPY of a thread that already has turns is not a change.
	// The old gate for this was "unchangedPayload != nil", which only ever meant
	// "the caller found a head to quote"; requiring the current side to hold
	// turns states the actual precondition and keeps a brand-new artifact
	// falling through to a full create.
	if len(currentTurns) > 0 && (acf.TextTurnsEqual(newTurns, currentTurns) || textTurnsPrefix(newTurns, currentTurns)) {
		return nil, nil, true, nil
	}
	return nil, nil, false, nil
}

func conversationImportWouldRegress(currentEvents, newEvents []acf.ConversationEvent) bool {
	currentTurns := acf.ExtractTextTurns(currentEvents)
	newTurns := acf.ExtractTextTurns(newEvents)
	if len(currentTurns) == 0 {
		return false
	}
	if !textTurnsPrefixOrEqual(currentTurns, newTurns) {
		return true
	}
	if acf.TextTurnsEqual(newTurns, currentTurns) && len(newEvents) < len(currentEvents) {
		return true
	}
	// Same hole as the hermes door (see WouldRevertThreadRef): a prefix relation
	// is an append-only test, and a native copy taken BEFORE a repair is a
	// strict superset of the repaired head, so it passes as "a continuation" and
	// re-asserts the very turns the repair removed immediately after it commits.
	return textTurnsOnlyReplayCurrent(currentTurns, newTurns)
}

// conversationDivergentNativeTail returns the native events that follow the two
// projections' longest common turn prefix, and reports whether appending them to
// canonical is SOUND.
//
// It exists for the one shape that otherwise loses turns permanently and
// silently: a conversation that started in one agent, was continued elsewhere,
// and then continued again in the original agent WITHOUT resuming. Canonical
// then holds the foreign turns, the native file holds its own later turns, and
// neither is a prefix of the other — so the ordinary append route cannot fire
// and conversationImportWouldRegress correctly refuses. Refusing is right;
// refusing forever and never learning the native turns is not.
//
// APPEND-ONLY BY CONSTRUCTION. Canonical's events are preserved verbatim and
// only turns canonical does not already hold are added, so this can neither
// revert nor reorder a turn. Preconditions, all required:
//
//   - both sides hold turns, and they agree on at least their first turn, which
//     is what proves the two projections are the same conversation rather than
//     two threads that happen to share a source path;
//   - neither side is a prefix of the other — the ordinary routes own those, and
//     a native side that is merely BEHIND must never have its own past appended;
//   - canonical does not already hold every native turn in order. THIS IS THE
//     FIXED POINT and it is load-bearing: the tail is appended contiguously at
//     the end, so afterwards the native turns ARE an in-order subsequence of
//     canonical — the inverse of this precondition — and a second pass over the
//     same unchanged file absorbs nothing. Without it the same turns would be
//     re-appended on every import forever and the artifact would grow without
//     bound;
//   - the native side is not a pre-repair copy re-asserting turns a repair
//     removed (textTurnsOnlyReplayCurrent, which prevents a stale copy from
//     undoing a completed repair);
//   - every TEXT TURN in the tail is absent from canonical. This is what keeps
//     the proof correct against a thread that already holds duplicates — the
//     reproduction thread for this defect holds one — because absorbing a tail
//     containing a turn canonical already has would re-assert a duplicate a
//     repair may have deliberately removed;
//   - every turn in the tail is stamped STRICTLY LATER than every turn canonical
//     already holds. This is the clause that separates the two shapes the turn
//     relations cannot tell apart: a native file continued in its own agent
//     since canonical last saw it (tail is newer — the case this exists for),
//     and a stale re-read of a snapshot the file has since moved past (tail is
//     older, and absorbing it would re-assert content the file no longer has).
//     A synthetic regression fixture verifies that native-only turns are newer
//     than the latest canonical turn.
//     Cross-device clock skew can make a genuine continuation look old; that
//     defers the absorb rather than misapplying it, which is the right way for
//     this to fail;
//   - the tail is non-empty.
func conversationDivergentNativeTail(
	currentEvents, newEvents []acf.ConversationEvent,
) ([]acf.ConversationEvent, bool) {
	currentTurns := acf.ExtractTextTurns(currentEvents)
	newTurns := acf.ExtractTextTurns(newEvents)
	if len(currentTurns) == 0 || len(newTurns) == 0 {
		return nil, false
	}
	if textTurnsPrefixOrEqual(currentTurns, newTurns) || textTurnsPrefixOrEqual(newTurns, currentTurns) {
		return nil, false
	}
	if textTurnsSubsequence(newTurns, currentTurns) {
		return nil, false
	}
	if textTurnsOnlyReplayCurrent(currentTurns, newTurns) {
		return nil, false
	}
	common := commonTextTurnPrefixLen(currentTurns, newTurns)
	if common == 0 {
		return nil, false
	}
	held := make(map[acf.TextTurn]struct{}, len(currentTurns))
	for _, turn := range currentTurns {
		held[turn] = struct{}{}
	}
	for _, turn := range newTurns[common:] {
		if _, seen := held[turn]; seen {
			return nil, false
		}
	}
	idx, ok := eventIndexAfterTextTurns(newEvents, common)
	if !ok || idx >= len(newEvents) {
		return nil, false
	}
	tail := newEvents[idx:]
	newestCurrent, haveCurrent := newestConversationTurnTime(currentEvents)
	oldestTail, haveTail := oldestConversationTurnTime(tail)
	if !haveCurrent || !haveTail || !oldestTail.After(newestCurrent) {
		return nil, false
	}
	return tail, true
}

// newestConversationTurnTime and oldestConversationTurnTime bracket the two
// sides in time. Both scan for the extreme rather than trusting log order, so an
// out-of-order event cannot defeat the ordering proof, and both report false on
// an unstamped turn — an absorb may never be authorized by a timestamp that was
// never written.
func newestConversationTurnTime(events []acf.ConversationEvent) (time.Time, bool) {
	var newest time.Time
	found := false
	for _, ev := range events {
		if len(acf.ExtractTextTurns([]acf.ConversationEvent{ev})) == 0 {
			continue
		}
		if ev.Timestamp.IsZero() {
			return time.Time{}, false
		}
		if !found || ev.Timestamp.After(newest) {
			newest = ev.Timestamp
			found = true
		}
	}
	return newest, found
}

func oldestConversationTurnTime(events []acf.ConversationEvent) (time.Time, bool) {
	var oldest time.Time
	found := false
	for _, ev := range events {
		if len(acf.ExtractTextTurns([]acf.ConversationEvent{ev})) == 0 {
			continue
		}
		if ev.Timestamp.IsZero() {
			return time.Time{}, false
		}
		if !found || ev.Timestamp.Before(oldest) {
			oldest = ev.Timestamp
			found = true
		}
	}
	return oldest, found
}

func commonTextTurnPrefixLen(a, b []acf.TextTurn) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}

// textTurnsSubsequence reports whether every turn of sub appears in full, in
// order. It is the "canonical already holds this" test, and it must stay an
// ORDERED containment rather than a set test so a thread holding duplicates is
// measured honestly.
func textTurnsSubsequence(sub, full []acf.TextTurn) bool {
	if len(sub) > len(full) {
		return false
	}
	next := 0
	for _, turn := range full {
		if next < len(sub) && sub[next] == turn {
			next++
		}
	}
	return next == len(sub)
}

func textTurnsPrefixOrEqual(prefix, full []acf.TextTurn) bool {
	if len(prefix) > len(full) {
		return false
	}
	return acf.TextTurnsEqual(prefix, full[:len(prefix)])
}

func conversationEventsEqual(a, b []acf.ConversationEvent) bool {
	// Compare event-by-event rather than marshaling both complete transcripts.
	// A json.RawMessage decoded from the event log may still differ bytewise
	// (`<` versus `\u003c`), so opaque fields receive semantic JSON comparison;
	// ordinary fields stay allocation-free. This keeps prefix validation O(n)
	// without creating two additional 100+ MB JSON documents.
	return conversationEventSequencesEqual(a, b)
}

func conversationEventsPrefix(prefix, full []acf.ConversationEvent) bool {
	if len(prefix) > len(full) {
		return false
	}
	return conversationEventsEqual(prefix, full[:len(prefix)])
}

func conversationTailAfterTextTurnPrefix(currentEvents, newEvents []acf.ConversationEvent) ([]acf.ConversationEvent, bool) {
	currentTurns := acf.ExtractTextTurns(currentEvents)
	newTurns := acf.ExtractTextTurns(newEvents)
	if len(currentTurns) > len(newTurns) || !acf.TextTurnsEqual(currentTurns, newTurns[:len(currentTurns)]) {
		return nil, false
	}
	idx, ok := eventIndexAfterTextTurns(newEvents, len(currentTurns))
	if !ok || idx >= len(newEvents) {
		return nil, false
	}
	return newEvents[idx:], true
}

func conversationTailAfterTextTurnSuffixOverlap(currentEvents, newEvents []acf.ConversationEvent) ([]acf.ConversationEvent, bool) {
	currentTurns := acf.ExtractTextTurns(currentEvents)
	newTurns := acf.ExtractTextTurns(newEvents)
	overlap := largestTextTurnSuffixPrefixOverlap(currentTurns, newTurns)
	if overlap < 2 {
		return nil, false
	}
	idx, ok := eventIndexAfterTextTurns(newEvents, overlap)
	if !ok {
		return nil, false
	}
	return newEvents[idx:], true
}

func largestTextTurnSuffixPrefixOverlap(currentTurns, newTurns []acf.TextTurn) int {
	max := len(currentTurns)
	if len(newTurns) < max {
		max = len(newTurns)
	}
	for n := max; n > 0; n-- {
		if acf.TextTurnsEqual(currentTurns[len(currentTurns)-n:], newTurns[:n]) {
			return n
		}
	}
	return 0
}

func eventIndexAfterTextTurns(events []acf.ConversationEvent, turns int) (int, bool) {
	if turns == 0 {
		return 0, true
	}
	seen := 0
	for i, ev := range events {
		if len(acf.ExtractTextTurns([]acf.ConversationEvent{ev})) == 0 {
			continue
		}
		seen++
		if seen == turns {
			return i + 1, true
		}
	}
	return len(events), seen == turns
}

// ExportCanonicalConversation materializes a conversation log that may contain
// canonical delta updates and writes the adapter-native session bytes.
func ExportCanonicalConversation(
	ctx context.Context,
	store *acf.Store,
	artifactID string,
	destPath string,
	decodeCanonical ConversationCanonicalDecoder,
	decodeLegacy OpaqueDecoder,
) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("adapter: export cancelled: %w", err)
	}
	current, tombstoned, err := ReplayCanonicalConversationContent(store, artifactID, decodeCanonical, decodeLegacy)
	if err != nil {
		return err
	}
	if tombstoned {
		return ErrArtifactTombstoned
	}
	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		return fmt.Errorf("adapter: mkdir dest: %w", err)
	}
	return atomicfile.WriteFile(destPath, []byte(current), 0o644)
}

// ReplayCanonicalConversationContent replays a conversation artifact's event
// log, composing acf.conversation.delta.v1 updates into a complete canonical
// payload before decoding to native session content.
func ReplayCanonicalConversationContent(
	store *acf.Store,
	artifactID string,
	decodeCanonical ConversationCanonicalDecoder,
	decodeLegacy OpaqueDecoder,
) (content string, tombstoned bool, err error) {
	art, aerr := store.ReadArtifact(acf.KindConversation, artifactID)
	if aerr != nil {
		return "", false, fmt.Errorf("adapter: read artifact: %w", aerr)
	}
	if art.Tombstoned {
		return "", true, nil
	}
	// Native import has already parsed the complete source conversation and
	// primed a projection for the exact event AppendEvent just committed. Reuse
	// it for immediate fan-out after validating the persisted head event. On a
	// miss, remote/startup materialization retains the full verified-log path.
	if cached, ok, cerr := store.ValidatedCachedMaterializedConversationPayload(artifactID); cerr != nil {
		return "", false, fmt.Errorf("adapter: validate cached conversation projection: %w", cerr)
	} else if ok {
		current := decodeMaterializedConversationPayload(cached, decodeCanonical, decodeLegacy)
		if current.err != nil {
			return "", false, current.err
		}
		return current.content, false, nil
	}
	events, err := store.ReadEvents(acf.KindConversation, artifactID)
	if err != nil {
		return "", false, fmt.Errorf("adapter: read events: %w", err)
	}
	if len(events) == 0 {
		return "", false, fmt.Errorf("adapter: no events for artifact %s", artifactID)
	}

	current, found := replayCanonicalConversationFromLog(events, decodeCanonical, decodeLegacy)
	if current.err != nil {
		return "", false, current.err
	}
	if found {
		return current.content, false, nil
	}

	merged, merr := store.ReadEventsIncludingCompacted(acf.KindConversation, artifactID)
	if merr != nil {
		return "", false, fmt.Errorf("adapter: read events including compacted: %w", merr)
	}
	if verr := acf.VerifyChain(merged); verr != nil {
		return "", false, fmt.Errorf("adapter: event log is invalid: %w", verr)
	}
	current, _ = replayCanonicalConversationFromLog(merged, decodeCanonical, decodeLegacy)
	if current.err != nil {
		return "", false, current.err
	}
	return current.content, false, nil
}

func replayCanonicalConversationFromLog(
	events []acf.Event,
	decodeCanonical ConversationCanonicalDecoder,
	decodeLegacy OpaqueDecoder,
) (replayOpaqueResult, bool) {
	if err := acf.VerifyChain(events); err != nil {
		return replayOpaqueResult{}, false
	}
	p, ok, err := acf.MaterializedConversationPayload(events)
	if err != nil {
		return replayOpaqueResult{err: fmt.Errorf("adapter: %w", err)}, false
	}
	if !ok {
		return replayOpaqueResult{}, false
	}
	return decodeMaterializedConversationPayload(p, decodeCanonical, decodeLegacy), true
}

func decodeMaterializedConversationPayload(
	p acf.ConversationPayload,
	decodeCanonical ConversationCanonicalDecoder,
	decodeLegacy OpaqueDecoder,
) replayOpaqueResult {
	if p.Format == acf.ConversationFormatV1 || p.Format == acf.ConversationDeltaFormatV1 {
		decoded, derr := decodeCanonical(p.Events)
		if derr != nil {
			return replayOpaqueResult{err: fmt.Errorf("adapter: %w", derr)}
		}
		return replayOpaqueResult{content: string(decoded)}
	}
	payload, perr := acf.EncodePayload(p)
	if perr != nil {
		return replayOpaqueResult{err: fmt.Errorf("adapter: %w", perr)}
	}
	decoded, derr := decodeLegacy(acf.Event{Payload: payload})
	if derr != nil {
		return replayOpaqueResult{err: fmt.Errorf("adapter: %w", derr)}
	}
	return replayOpaqueResult{content: decoded}
}
