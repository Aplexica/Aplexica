package acf

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

const materializedConversationCacheMaxEntries = 8

type conversationCacheEntry struct {
	headHash string
	head     Event
	payload  ConversationPayload
	tick     uint64
}

// PrimeMaterializedConversation records a full main-branch projection that an
// adapter just parsed from native storage and successfully committed. It is a
// performance hint only: a later artifact-head mismatch invalidates it and the
// normal event-log reconstruction remains the source of truth.
func (s *Store) PrimeMaterializedConversation(artifactID string, payload ConversationPayload) {
	s.primeMaterializedConversationAtHeadEvent(artifactID, "", payload)
}

// PrimeMaterializedConversationAtHeadEvent records payload only when the
// current persisted main-branch tail is still expectedEventID. Importers call
// this after committing a projection parsed before AppendEvent: if another
// prompt or answer appends between commit and priming, the stale projection is
// rejected instead of being cached under that newer head.
func (s *Store) PrimeMaterializedConversationAtHeadEvent(
	artifactID, expectedEventID string,
	payload ConversationPayload,
) {
	if expectedEventID == "" {
		return
	}
	s.primeMaterializedConversationAtHeadEvent(artifactID, expectedEventID, payload)
}

func (s *Store) primeMaterializedConversationAtHeadEvent(
	artifactID, expectedEventID string,
	payload ConversationPayload,
) {
	if artifactID == "" || payload.Format != ConversationFormatV1 {
		return
	}
	art, err := s.ReadArtifact(KindConversation, artifactID)
	if err != nil || art.HeadEventHash == "" {
		return
	}
	head, ok, err := s.LastEvent(KindConversation, artifactID)
	headMatches := ok && head.Hash == art.HeadEventHash
	if ok && head.Type == EventTypeBaseline && head.AlignedHead == art.HeadEventHash {
		// Baseline adoption intentionally points artifact bookkeeping at the
		// origin's aligned head instead of the local wrapper event hash. The
		// caller already holds the decoded full payload, so it is still a valid
		// cache seed for the aligned head.
		headMatches = true
	}
	if err != nil || !ok || normalizeBranch(head.Branch) != MainBranch || !headMatches ||
		(expectedEventID != "" && head.EventID != expectedEventID) {
		return
	}
	s.cacheMaterializedConversation(artifactID, art.HeadEventHash, head, payload)
}

func (s *Store) cachedMaterializedConversation(artifactID, headHash string) (ConversationPayload, Event, bool) {
	s.conversationCacheMu.Lock()
	defer s.conversationCacheMu.Unlock()
	entry, ok := s.conversationCache[artifactID]
	if !ok || entry.headHash != headHash {
		return ConversationPayload{}, Event{}, false
	}
	s.conversationCacheClock++
	entry.tick = s.conversationCacheClock
	s.conversationCache[artifactID] = entry
	return cloneConversationPayload(entry.payload), entry.head, true
}

// ValidatedCachedMaterializedConversationPayload returns a native-import
// projection only when it is still bound to the artifact's exact main-branch
// head and that newest persisted event verifies against its recorded hash.
//
// The cache is populated immediately after an adapter has parsed the complete
// native conversation and AppendEvent has committed the corresponding event.
// Reusing that in-process projection avoids rereading and hashing every older
// event during immediate fan-out. A cache miss, a side-branch tail, or any head
// mismatch deliberately returns ok=false so callers retain their full
// event-log verification path. A malformed or hash-mismatched newest event is
// an integrity error, not a cache miss.
func (s *Store) ValidatedCachedMaterializedConversationPayload(artifactID string) (ConversationPayload, bool, error) {
	art, err := s.ReadArtifact(KindConversation, artifactID)
	if err != nil {
		return ConversationPayload{}, false, err
	}
	if art.HeadEventHash == "" {
		return ConversationPayload{}, false, nil
	}
	payload, cachedHead, ok := s.cachedMaterializedConversation(artifactID, art.HeadEventHash)
	if !ok {
		return ConversationPayload{}, false, nil
	}
	latest, found, err := s.LastEvent(KindConversation, artifactID)
	if err != nil {
		return ConversationPayload{}, false, err
	}
	headMatches := found && latest.Hash == art.HeadEventHash
	if found && latest.Type == EventTypeBaseline && latest.AlignedHead == art.HeadEventHash {
		// AdoptBaseline deliberately points main-head bookkeeping at the
		// authenticated origin head while the local log tail is a baseline
		// wrapper. PrimeMaterializedConversation accepts this shape; validation
		// must apply the same rule or the first live answer delta immediately
		// misses the cache seeded by the retained prompt baseline.
		headMatches = true
	}
	if !found || normalizeBranch(latest.Branch) != MainBranch ||
		!headMatches || cachedHead.Hash != latest.Hash {
		return ConversationPayload{}, false, nil
	}
	want, err := ComputeHash(latest)
	if err != nil {
		return ConversationPayload{}, false, err
	}
	if latest.Hash != want {
		return ConversationPayload{}, false, fmt.Errorf(
			"acf: cached conversation head %s hash %q does not match recomputed %q",
			latest.EventID, latest.Hash, want,
		)
	}
	return payload, true, nil
}

func (s *Store) cacheMaterializedConversation(artifactID, headHash string, head Event, payload ConversationPayload) {
	if artifactID == "" || headHash == "" {
		return
	}
	s.conversationCacheMu.Lock()
	defer s.conversationCacheMu.Unlock()
	if s.conversationCache == nil {
		s.conversationCache = make(map[string]conversationCacheEntry)
	}
	s.conversationCacheClock++
	s.conversationCache[artifactID] = conversationCacheEntry{
		headHash: headHash,
		head:     head,
		payload:  cloneConversationPayload(payload),
		tick:     s.conversationCacheClock,
	}
	for len(s.conversationCache) > materializedConversationCacheMaxEntries {
		var victimID string
		var victimTick uint64
		first := true
		for id, entry := range s.conversationCache {
			if first || entry.tick < victimTick {
				victimID, victimTick, first = id, entry.tick, false
			}
		}
		delete(s.conversationCache, victimID)
	}
}

func cloneConversationPayload(payload ConversationPayload) ConversationPayload {
	payload.Events = append([]ConversationEvent(nil), payload.Events...)
	payload.Attachments = append([]Attachment(nil), payload.Attachments...)
	return payload
}

// MaterializedConversationPayload forward-replays a conversation artifact log
// into the current materialized ConversationPayload.
//
// Full canonical payloads and legacy opaque payloads replace the current state.
// ConversationDeltaFormatV1 payloads append their Events to the current
// canonical state and return a full ConversationFormatV1 payload. Redaction
// clears the current state and acts as a barrier: a log ending at redaction
// returns ok=false.
//
// A payload-bearing baseline (EventTypeBaseline, aligned-chains adoption)
// replays like a payload-bearing snapshot: it carries the full materialized
// origin state, superseding whatever local state preceded it — which is
// exactly the adoption semantics (post-baseline deltas then extend the
// ORIGIN's thread, not the pre-adoption local one).
func MaterializedConversationPayload(events []Event) (ConversationPayload, bool, error) {
	var current ConversationPayload
	found := false
	for _, e := range events {
		switch e.Type {
		case EventTypeCreate, EventTypeUpdate, EventTypeResolution:
		case EventTypeSnapshot, EventTypeBaseline:
			if !HasPayload(e.Payload) {
				continue
			}
		case EventTypeRedaction:
			current = ConversationPayload{}
			found = false
			continue
		default:
			continue
		}

		p, err := DecodeConversationPayload(e)
		if err != nil {
			return ConversationPayload{}, false, err
		}
		switch p.Format {
		case ConversationFormatV1:
			current = ConversationPayload{
				Format:      ConversationFormatV1,
				Events:      append([]ConversationEvent(nil), p.Events...),
				Attachments: append([]Attachment(nil), p.Attachments...),
			}
		case ConversationDeltaFormatV1:
			if current.Format != ConversationFormatV1 {
				current = ConversationPayload{Format: ConversationFormatV1}
			}
			current.Events = append(current.Events, p.Events...)
			current.Attachments = append(current.Attachments, p.Attachments...)
		default:
			current = p
			current.Events = append([]ConversationEvent(nil), p.Events...)
			current.Attachments = append([]Attachment(nil), p.Attachments...)
		}
		found = true
	}
	if !found {
		return ConversationPayload{}, false, nil
	}
	return current, true, nil
}

// MaterializedConversationPayloadBytes is MaterializedConversationPayload
// encoded back into an event payload. Delta updates are folded into a full
// ConversationFormatV1 payload so snapshots and materializers carry complete
// thread state.
func MaterializedConversationPayloadBytes(events []Event) (json.RawMessage, bool, error) {
	p, ok, err := MaterializedConversationPayload(events)
	if err != nil || !ok {
		return nil, ok, err
	}
	payload, err := EncodePayload(p)
	if err != nil {
		return nil, false, fmt.Errorf("acf: encode materialized conversation payload: %w", err)
	}
	return payload, true, nil
}

// MaterializedConversationPayloadFromStore reconstructs the active main-log
// conversation from the newest full payload/redaction barrier forward. It
// reads event lines backward and therefore avoids replaying gigabytes of
// superseded full-history updates. If the active log was pruned without a
// self-contained anchor, it falls back to the compacted+active union.
func (s *Store) MaterializedConversationPayloadFromStore(artifactID string) (ConversationPayload, bool, error) {
	payload, _, ok, err := s.MaterializedConversationHeadFromStore(artifactID)
	return payload, ok, err
}

// MaterializedConversationHeadFromStore returns the materialized main-branch
// payload together with the newest main-branch event that produced it. Side
// branches share the physical JSONL file but must never influence either
// result. Returning both values from the same backward scan also avoids a
// second whole-log search merely to recover the event metadata.
func (s *Store) MaterializedConversationHeadFromStore(artifactID string) (ConversationPayload, Event, bool, error) {
	art, artErr := s.ReadArtifact(KindConversation, artifactID)
	if artErr == nil && art.HeadEventHash != "" {
		if payload, head, ok := s.cachedMaterializedConversation(artifactID, art.HeadEventHash); ok {
			return payload, head, true, nil
		}
	}
	events, complete, err := s.conversationMaterializationWindow(artifactID)
	if err != nil {
		return ConversationPayload{}, Event{}, false, err
	}
	if complete {
		payload, ok, err := MaterializedConversationPayload(events)
		if err != nil || !ok {
			return ConversationPayload{}, Event{}, ok, err
		}
		head := events[len(events)-1]
		if artErr == nil && art.HeadEventHash == head.Hash {
			s.cacheMaterializedConversation(artifactID, art.HeadEventHash, head, payload)
		}
		return payload, head, true, nil
	}
	merged, err := s.ReadEventsIncludingCompacted(KindConversation, artifactID)
	if err != nil {
		return ConversationPayload{}, Event{}, false, err
	}
	mainEvents := merged[:0]
	for _, event := range merged {
		if normalizeBranch(event.Branch) == MainBranch {
			mainEvents = append(mainEvents, event)
		}
	}
	payload, ok, err := MaterializedConversationPayload(mainEvents)
	if err != nil || !ok {
		return ConversationPayload{}, Event{}, ok, err
	}
	head := mainEvents[len(mainEvents)-1]
	if artErr == nil && art.HeadEventHash == head.Hash {
		s.cacheMaterializedConversation(artifactID, art.HeadEventHash, head, payload)
	}
	return payload, head, true, nil
}

func (s *Store) conversationMaterializationWindow(artifactID string) ([]Event, bool, error) {
	f, err := s.openRead(eventsRel(KindConversation, artifactID))
	if errors.Is(err, os.ErrNotExist) {
		return nil, true, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("acf: open conversation events: %w", err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, false, fmt.Errorf("acf: stat conversation events: %w", err)
	}
	end := st.Size()
	var reversed []Event
	complete := false
	for end > 0 {
		line, next, ok, rerr := readPreviousNonEmptyLine(f, end)
		if rerr != nil {
			return nil, false, fmt.Errorf("acf: read conversation event window: %w", rerr)
		}
		if !ok {
			break
		}
		end = next
		var event Event
		if err := json.Unmarshal(line, &event); err != nil {
			return nil, false, fmt.Errorf("acf: parse conversation event window: %w", err)
		}
		if normalizeBranch(event.Branch) != MainBranch {
			continue
		}
		reversed = append(reversed, event)
		if event.Type == EventTypeRedaction {
			complete = true
			break
		}
		switch event.Type {
		case EventTypeCreate, EventTypeUpdate, EventTypeResolution, EventTypeSnapshot, EventTypeBaseline:
			if !HasPayload(event.Payload) {
				continue
			}
			payload, derr := DecodeConversationPayload(event)
			if derr != nil {
				return nil, false, derr
			}
			if payload.Format != ConversationDeltaFormatV1 {
				complete = true
			}
		}
		if complete {
			break
		}
	}
	for i, j := 0, len(reversed)-1; i < j; i, j = i+1, j-1 {
		reversed[i], reversed[j] = reversed[j], reversed[i]
	}
	return reversed, complete, nil
}
