package acf

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestHasPayload(t *testing.T) {
	require.False(t, HasPayload(nil), "nil payload has no body")
	require.False(t, HasPayload(json.RawMessage{}), "empty payload has no body")
	require.False(t, HasPayload(json.RawMessage("null")), "literal JSON null is the on-disk form of a nil payload")
	require.True(t, HasPayload(json.RawMessage(`{"format":"markdown"}`)), "a real object is a body")
	require.True(t, HasPayload(json.RawMessage(`"null-ish but quoted"`)), "a quoted string that merely contains null is a body")

	// Round-trip proof: a nil payload marshals to `"payload":null` and reads
	// back as the 4-byte literal, which HasPayload must still call empty.
	b, err := json.Marshal(Event{Payload: nil})
	require.NoError(t, err)
	var e Event
	require.NoError(t, json.Unmarshal(b, &e))
	require.Equal(t, "null", string(e.Payload))
	require.False(t, HasPayload(e.Payload), "round-tripped nil payload is still empty")
}

// TestLatestEventFormat_PayloadBearingSnapshot proves LatestEventFormat treats
// an FR-02.32 payload-bearing snapshot as a format-bearing checkpoint. After an
// on-snapshot prune the active log can be snapshot-only; the fan-out gate
// (orchestrator) reads the format from the active log, so a snapshot that
// carries the materialized payload must report that payload's format — else a
// pruned conversation silently drops out of fan-out. A payload-LESS snapshot is
// still skipped (legacy).
func TestLatestEventFormat_PayloadBearingSnapshot(t *testing.T) {
	conv, err := EncodePayload(ConversationPayload{Format: ConversationFormatV1})
	require.NoError(t, err)

	// Payload-bearing snapshot alone (the post-prune, snapshot-only log).
	got, ok := LatestEventFormat([]Event{
		{Type: EventTypeSnapshot, Payload: conv, SnapshotState: "sha256:abc"},
	})
	require.True(t, ok, "a payload-bearing snapshot is format-bearing")
	require.Equal(t, ConversationFormatV1, got)

	// Payload-less snapshot is skipped; falls through to the create below it.
	mem, err := EncodePayload(MemoryPayload{Format: "markdown", Content: "x"})
	require.NoError(t, err)
	got2, ok2 := LatestEventFormat([]Event{
		{Type: EventTypeCreate, Payload: mem},
		{Type: EventTypeSnapshot, Payload: json.RawMessage("null"), SnapshotState: "sha256:def"},
	})
	require.True(t, ok2)
	require.Equal(t, "markdown", got2, "a payload-less snapshot is skipped; the create's format wins")
}

// TestLatestPayloadEvent_* exercise the shared payload-walk helper that both
// syncd (latestPayloadBearingEvent) and hermes (exportableBundleFromActiveLog)
// delegate to: walk backward, return the newest create/update/resolution event
// or payload-bearing snapshot, skipping everything else. It is policy-FREE
// about redaction (treats it as a skip) — the redaction-as-barrier and
// compacted-fallback policy lives in the callers, not in acf.

func TestLatestPayloadEvent_ReturnsNewestMutatingEvent(t *testing.T) {
	for _, typ := range []EventType{EventTypeCreate, EventTypeUpdate, EventTypeResolution} {
		t.Run(string(typ), func(t *testing.T) {
			got, ok := LatestPayloadEvent([]Event{
				{EventID: "older", Type: EventTypeCreate, Payload: json.RawMessage(`{}`)},
				{EventID: "newer", Type: typ, Payload: json.RawMessage(`{}`)},
			})
			require.True(t, ok)
			require.Equal(t, "newer", got.EventID, "the newest payload-bearing event wins (last write)")
		})
	}
}

func TestLatestPayloadEvent_PayloadBearingSnapshotIsACheckpoint(t *testing.T) {
	conv, err := EncodePayload(ConversationPayload{Format: ConversationFormatV1})
	require.NoError(t, err)
	got, ok := LatestPayloadEvent([]Event{
		{EventID: "snap", Type: EventTypeSnapshot, Payload: conv, SnapshotState: "sha256:abc"},
	})
	require.True(t, ok, "a payload-bearing snapshot is a self-contained checkpoint (FR-02.32)")
	require.Equal(t, "snap", got.EventID)
}

func TestLatestPayloadEvent_PayloadlessSnapshotSkipped(t *testing.T) {
	got, ok := LatestPayloadEvent([]Event{
		{EventID: "create", Type: EventTypeCreate, Payload: json.RawMessage(`{}`)},
		{EventID: "snap", Type: EventTypeSnapshot, Payload: json.RawMessage("null"), SnapshotState: "sha256:def"},
	})
	require.True(t, ok)
	require.Equal(t, "create", got.EventID, "a payload-less snapshot is skipped; the create below it wins")
}

func TestLatestPayloadEvent_PolicyFreeWalksPastRedaction(t *testing.T) {
	// acf stays policy-free: it keeps walking past a redaction to the create
	// below it. Treating a redaction as an authoritative barrier is CALLER
	// policy (syncd's latestPayloadBearingEvent, hermes' exportableBundleFromActiveLog).
	got, ok := LatestPayloadEvent([]Event{
		{EventID: "create", Type: EventTypeCreate, Payload: json.RawMessage(`{}`)},
		{EventID: "redaction", Type: EventTypeRedaction},
	})
	require.True(t, ok, "acf must NOT stop at a redaction — that is caller policy")
	require.Equal(t, "create", got.EventID)
}

func TestLatestPayloadEvent_NoneFound(t *testing.T) {
	_, ok := LatestPayloadEvent(nil)
	require.False(t, ok, "empty slice has no payload event")

	_, ok = LatestPayloadEvent([]Event{
		{Type: EventTypeAmendment},
		{Type: EventTypeForkOuter},
		{Type: EventTypeRedaction},
		{Type: EventTypeSnapshot, Payload: json.RawMessage("null")},
	})
	require.False(t, ok, "no create/update/resolution or payload-bearing snapshot → not found")
}

func TestEncodePayload_MemoryRoundTrip(t *testing.T) {
	original := MemoryPayload{Format: "markdown", Content: "# Hi\n"}
	raw, err := EncodePayload(original)
	require.NoError(t, err)

	decoded, err := DecodeMemoryPayload(Event{Payload: raw})
	require.NoError(t, err)
	require.Equal(t, original, decoded)
}

func TestEncodePayload_SkillRoundTrip(t *testing.T) {
	original := SkillPayload{Format: "skill.md", Content: "---\nname: x\n---\n# Skill\n"}
	raw, err := EncodePayload(original)
	require.NoError(t, err)

	decoded, err := DecodeSkillPayload(Event{Payload: raw})
	require.NoError(t, err)
	require.Equal(t, original, decoded)
}

func TestEncodePayload_WireFormatMatchesDirectMarshal(t *testing.T) {
	cases := []struct {
		name    string
		payload any
	}{
		{"memory-plain", MemoryPayload{Format: "markdown", Content: "x"}},
		{"memory-empty", MemoryPayload{}},
		{"memory-unicode", MemoryPayload{Format: "markdown", Content: "héllo — 🎉\n"}},
		{"skill-plain", SkillPayload{Format: "skill.md", Content: "---\nname: x\n---\n"}},
		{"skill-empty", SkillPayload{}},
		{"conversation-plain", ConversationPayload{Format: "claude-code.session.jsonl", Content: "line1\nline2\n"}},
		{"conversation-empty", ConversationPayload{}},
		{"tool-plain", ToolPayload{Format: "claude-code.mcp.json", Content: `{"mcpServers":{}}`}},
		{"tool-empty", ToolPayload{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			direct, err := json.Marshal(tc.payload)
			require.NoError(t, err)
			via, err := EncodePayload(tc.payload)
			require.NoError(t, err)
			require.Equal(t, string(direct), string(via),
				"EncodePayload MUST produce JSON byte-identical to json.Marshal — the hash chain depends on this")
		})
	}
}

func TestDecodeMemoryPayload_RejectsMalformed(t *testing.T) {
	bad := Event{Payload: json.RawMessage(`{`)}
	_, err := DecodeMemoryPayload(bad)
	require.Error(t, err)
}

func TestDecodeSkillPayload_RejectsMalformed(t *testing.T) {
	bad := Event{Payload: json.RawMessage(`{`)}
	_, err := DecodeSkillPayload(bad)
	require.Error(t, err)
}

func TestEncodePayload_ConversationRoundTrip(t *testing.T) {
	original := ConversationPayload{
		Format:  "claude-code.session.jsonl",
		Content: `{"type":"summary","leafUuid":"abc","sessionId":"xyz"}` + "\n",
	}
	raw, err := EncodePayload(original)
	require.NoError(t, err)

	decoded, err := DecodeConversationPayload(Event{Payload: raw})
	require.NoError(t, err)
	require.Equal(t, original, decoded)
}

func TestDecodeConversationPayload_RejectsMalformed(t *testing.T) {
	bad := Event{Payload: json.RawMessage(`{`)}
	_, err := DecodeConversationPayload(bad)
	require.Error(t, err)
}

func TestDecodeConversationPayload_UsesTransientMaterializedProjection(t *testing.T) {
	want := ConversationPayload{
		Format: ConversationFormatV1,
		Events: []ConversationEvent{{Type: EventTypeTurn, Role: "user"}},
	}
	event := Event{
		Payload:                  json.RawMessage(`{}`),
		MaterializedConversation: &want,
	}
	got, err := DecodeConversationPayload(event)
	require.NoError(t, err)
	require.Equal(t, want, got)

	wire, err := json.Marshal(event)
	require.NoError(t, err)
	require.NotContains(t, string(wire), "materializedConversation")
	require.NotContains(t, string(wire), ConversationFormatV1)
}

func TestEncodePayload_ToolRoundTrip(t *testing.T) {
	original := ToolPayload{
		Format:  "claude-code.mcp.json",
		Content: `{"mcpServers":{"foo":{"type":"http","url":"https://x.example"}}}`,
	}
	raw, err := EncodePayload(original)
	require.NoError(t, err)

	decoded, err := DecodeToolPayload(Event{Payload: raw})
	require.NoError(t, err)
	require.Equal(t, original, decoded)
}

func TestDecodeToolPayload_RejectsMalformed(t *testing.T) {
	bad := Event{Payload: json.RawMessage(`{`)}
	_, err := DecodeToolPayload(bad)
	require.Error(t, err)
}

func TestLatestEventFormat_ConversationPayload(t *testing.T) {
	payload, err := EncodePayload(ConversationPayload{
		Format: "claude-code.session.jsonl", Content: "x",
	})
	require.NoError(t, err)
	events := []Event{
		{Type: "create", Payload: payload, ArtifactID: "a"},
	}
	got, ok := LatestEventFormat(events)
	require.True(t, ok)
	require.Equal(t, "claude-code.session.jsonl", got)
}

func TestLatestEventFormat_MemorySkillTool(t *testing.T) {
	for _, tc := range []struct {
		name     string
		payload  any
		expected string
	}{
		{"memory", MemoryPayload{Format: "markdown", Content: "x"}, "markdown"},
		{"skill", SkillPayload{Format: "skill.md", Content: "x"}, "skill.md"},
		{"tool", ToolPayload{Format: "acf.mcp.v1", Content: `{}`}, "acf.mcp.v1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := EncodePayload(tc.payload)
			require.NoError(t, err)
			got, ok := LatestEventFormat([]Event{{Type: "create", Payload: p}})
			require.True(t, ok)
			require.Equal(t, tc.expected, got)
		})
	}
}

func TestLatestEventFormat_PrefersLatestCreateOrUpdate(t *testing.T) {
	old, err := EncodePayload(MemoryPayload{Format: "markdown", Content: "v1"})
	require.NoError(t, err)
	new1, err := EncodePayload(MemoryPayload{Format: "markdown", Content: "v2"})
	require.NoError(t, err)
	events := []Event{
		{Type: "create", Payload: old},
		{Type: "amendment", Payload: nil},
		{Type: "update", Payload: new1},
	}
	got, ok := LatestEventFormat(events)
	require.True(t, ok)
	require.Equal(t, "markdown", got)
}

func TestLatestEventFormat_EmptyEvents(t *testing.T) {
	_, ok := LatestEventFormat(nil)
	require.False(t, ok)
}

func TestLatestEventFormat_NoCreateOrUpdate(t *testing.T) {
	_, ok := LatestEventFormat([]Event{
		{Type: "amendment", Payload: nil},
		{Type: "redaction", Payload: nil},
	})
	require.False(t, ok)
}
