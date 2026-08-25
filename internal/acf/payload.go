package acf

import (
	"encoding/json"
	"fmt"
)

// jsonNullLiteral is the four bytes json.Marshal emits for a nil
// json.RawMessage payload. Because Event.Payload has no `omitempty` tag, a
// payload-less event (e.g. a pre-FR-02.32 snapshot written with Payload:nil)
// serializes to `"payload":null` and reads BACK as this literal — len 4, not
// nil. HasPayload uses it to distinguish "no payload" from a real body.
const jsonNullLiteral = "null"

// HasPayload reports whether p carries a real, decodable body. It returns
// false for three indistinguishable-after-round-trip "empty" shapes: a nil
// slice, a zero-length slice, and the literal JSON `null` (the on-disk form of
// a nil payload, since Event.Payload is serialized without `omitempty`).
//
// Callers walking an event log MUST use this rather than len(p) > 0: a
// payload-less event read back from JSONL has len 4 ("null"), so a bare
// length check would mistake it for a body and then fail to json.Unmarshal it
// into a typed payload.
func HasPayload(p json.RawMessage) bool {
	if len(p) == 0 {
		return false
	}
	return string(p) != jsonNullLiteral
}

// EncodePayload marshals a typed payload (MemoryPayload, SkillPayload, ...)
// into json.RawMessage for storage in Event.Payload. The wire format is
// exactly what json.Marshal would produce on the input — no extra wrapping.
func EncodePayload(p any) (json.RawMessage, error) {
	b, err := json.Marshal(p)
	if err != nil {
		return nil, fmt.Errorf("acf: encode payload: %w", err)
	}
	return json.RawMessage(b), nil
}

// DecodeMemoryPayload extracts a MemoryPayload from e.Payload.
// Caller is responsible for verifying that e came from an artifact with
// Kind == KindMemory.
//
// TODO(v0.2): if a use case emerges that needs to decode without a full Event
// (e.g. migration tools operating on raw JSONL), refactor to accept
// (payload json.RawMessage, eventID string) instead.
func DecodeMemoryPayload(e Event) (MemoryPayload, error) {
	var p MemoryPayload
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		return p, fmt.Errorf("acf: decode memory payload (event %s): %w", e.EventID, err)
	}
	return p, nil
}

// DecodeSkillPayload extracts a SkillPayload from e.Payload.
//
// TODO(v0.2): if a use case emerges that needs to decode without a full Event
// (e.g. migration tools operating on raw JSONL), refactor to accept
// (payload json.RawMessage, eventID string) instead.
func DecodeSkillPayload(e Event) (SkillPayload, error) {
	var p SkillPayload
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		return p, fmt.Errorf("acf: decode skill payload (event %s): %w", e.EventID, err)
	}
	return p, nil
}

// DecodeConversationPayload extracts a ConversationPayload from e.Payload.
// Caller is responsible for verifying that e came from an artifact with
// Kind == KindConversation.
//
// TODO(v0.2): if a use case emerges that needs to decode without a full Event
// (e.g. migration tools operating on raw JSONL), refactor to accept
// (payload json.RawMessage, eventID string) instead.
func DecodeConversationPayload(e Event) (ConversationPayload, error) {
	if e.MaterializedConversation != nil {
		return cloneConversationPayload(*e.MaterializedConversation), nil
	}
	var p ConversationPayload
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		return p, fmt.Errorf("acf: decode conversation payload (event %s): %w", e.EventID, err)
	}
	return p, nil
}

// LatestEventFormat scans events backward and returns the Format field of
// the most recent create/update event's payload. Returns ("", false) when
// no create/update events exist or the latest payload can't be parsed.
//
// Used by the sync orchestrator to gate fan-out: only adapters that
// HandlesFormat(kind, format) for the artifact's current payload format
// participate in fan-out export.
//
// All four payload types (MemoryPayload, SkillPayload, ToolPayload,
// ConversationPayload) declare Format as the first field with json tag
// "format", so a small anonymous struct can peek at it without knowing
// the artifact's kind up front.
func LatestEventFormat(events []Event) (string, bool) {
	for i := len(events) - 1; i >= 0; i-- {
		e := events[i]
		switch e.Type {
		case EventTypeCreate, EventTypeUpdate, EventTypeResolution:
			// EventTypeResolution (v0.34.0) carries a full payload like
			// create/update and counts as a format-bearing event.
		case EventTypeSnapshot, EventTypeBaseline:
			// FR-02.32: a payload-bearing snapshot is a self-contained
			// checkpoint carrying the materialized payload (and thus its
			// format). After an on-snapshot prune the active log can be
			// snapshot-only, so the fan-out gate must read the format from it.
			// A baseline (aligned-chains adoption) carries the full
			// materialized origin state and is format-bearing the same way.
			// A payload-LESS snapshot (legacy) has no format — skip it.
			if !HasPayload(e.Payload) {
				continue
			}
		default:
			continue
		}
		var generic struct {
			Format string `json:"format"`
		}
		if err := json.Unmarshal(e.Payload, &generic); err != nil {
			return "", false
		}
		if generic.Format == "" {
			return "", false
		}
		return generic.Format, true
	}
	return "", false
}

// LatestPayloadEvent walks events backward and returns the newest event that
// carries a materializable payload: a create/update/resolution event, a
// payload-bearing snapshot (FR-02.32), or a payload-bearing baseline
// (aligned-chains adoption — a full-state checkpoint of the origin device).
// A payload-LESS snapshot (the legacy, pre-FR-02.32 shape) and every other
// event type — fork, merge, amendment, redaction — are skipped. Returns
// (Event{}, false) when none exists.
//
// This is the shared backward "walk to the latest payload-bearing event" used
// by both the sync orchestrator's conversation-head selection
// (syncd.latestPayloadBearingEvent) and the hermes exporter
// (exportableBundleFromActiveLog). It mirrors LatestEventFormat's event-type
// switch and snapshot handling exactly — that sibling reads the format off the
// same event this one returns.
//
// It is deliberately policy-FREE about redaction: a redaction is treated like
// any other non-payload event and the walk continues past it. Callers that must
// treat a redaction as an authoritative content removal — stopping the walk so a
// pre-redaction payload is NOT resurrected — own that policy themselves (both
// callers bound their search to events newer than the latest redaction before
// delegating here). Keeping acf free of redaction/fallback policy is deliberate.
func LatestPayloadEvent(events []Event) (Event, bool) {
	for i := len(events) - 1; i >= 0; i-- {
		e := events[i]
		switch e.Type {
		case EventTypeCreate, EventTypeUpdate, EventTypeResolution:
			// EventTypeResolution (v0.34.0) carries a full payload like
			// create/update.
			return e, true
		case EventTypeSnapshot, EventTypeBaseline:
			// FR-02.32: a payload-bearing snapshot is a self-contained
			// checkpoint. A baseline (aligned-chains adoption) carries the
			// full materialized origin state and is a checkpoint the same
			// way. A payload-less snapshot (legacy) has no body — skip it.
			if HasPayload(e.Payload) {
				return e, true
			}
		}
		// Any other event type (fork, merge, redaction, amendment, …): skip.
	}
	return Event{}, false
}

// DecodeToolPayload extracts a ToolPayload from e.Payload.
// Caller is responsible for verifying that e came from an artifact with
// Kind == KindTool.
//
// TODO(v0.2): if a use case emerges that needs to decode without a full Event
// (e.g. migration tools operating on raw JSONL), refactor to accept
// (payload json.RawMessage, eventID string) instead.
func DecodeToolPayload(e Event) (ToolPayload, error) {
	var p ToolPayload
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		return p, fmt.Errorf("acf: decode tool payload (event %s): %w", e.EventID, err)
	}
	return p, nil
}
