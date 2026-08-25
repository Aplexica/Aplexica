package kilo

import (
	"encoding/json"
	"testing"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/stretchr/testify/require"
)

func TestEncodeKiloBundleAsCanonical_MapsTextToolAndInternalCheckpoints(t *testing.T) {
	b := kiloSessionBundle{
		Session: kiloSession{
			ID:          "s1",
			ProjectID:   "proj",
			Directory:   "/tmp/project",
			Title:       "Project Chat",
			TimeCreated: 1000,
			TimeUpdated: 2000,
		},
		Messages: []kiloMessageBundle{
			{
				Message: kiloMessage{ID: "m1", SessionID: "s1", Role: "user", TimeCreated: 1100, Data: rawJSON(`{"role":"user","time":{"created":1100}}`)},
				Parts: []kiloPart{
					{ID: "p1", MessageID: "m1", SessionID: "s1", Type: "text", TimeCreated: 1101, Data: rawJSON(`{"type":"text","text":"hello","time":{"start":1101,"end":1102}}`)},
				},
			},
			{
				Message: kiloMessage{ID: "m2", SessionID: "s1", Role: "assistant", TimeCreated: 1200, Data: rawJSON(`{"role":"assistant","time":{"created":1200,"completed":1300},"parentID":"m1","modelID":"kilo-auto/frontier","providerID":"kilo","path":{"cwd":"/tmp/project","root":"/tmp/project"},"agent":"code","mode":"code","cost":0.01,"tokens":{"input":10,"output":3,"reasoning":1,"cache":{"read":0,"write":0}}}`)},
				Parts: []kiloPart{
					{ID: "p2", MessageID: "m2", SessionID: "s1", Type: "step-start", TimeCreated: 1201, Data: rawJSON(`{"type":"step-start","snapshot":"snap-1"}`)},
					{ID: "p3", MessageID: "m2", SessionID: "s1", Type: "reasoning", TimeCreated: 1202, Data: rawJSON(`{"type":"reasoning","text":"thinking","time":{"start":1202,"end":1203}}`)},
					{ID: "p4", MessageID: "m2", SessionID: "s1", Type: "text", TimeCreated: 1204, Data: rawJSON(`{"type":"text","text":"I will read it","time":{"start":1204,"end":1205}}`)},
					{ID: "p5", MessageID: "m2", SessionID: "s1", Type: "tool", TimeCreated: 1206, Data: rawJSON(`{"type":"tool","callID":"call-1","tool":"read","state":{"status":"completed","input":{"filePath":"README.md"},"output":"file text","title":"Read README","metadata":{},"time":{"start":1206,"end":1207}}}`)},
					{ID: "p6", MessageID: "m2", SessionID: "s1", Type: "step-finish", TimeCreated: 1208, Data: rawJSON(`{"type":"step-finish","reason":"stop","snapshot":"snap-1","cost":0.01,"tokens":{"input":10,"output":3,"reasoning":1,"cache":{"read":0,"write":0},"total":14}}`)},
				},
			},
		},
	}

	events, err := encodeKiloBundleAsCanonical(b)
	require.NoError(t, err)
	require.Len(t, events, 7)
	require.Equal(t, []string{
		acf.EventTypeTurn,
		acf.EventTypeSystemNote,
		acf.EventTypeSystemNote,
		acf.EventTypeTurn,
		acf.EventTypeToolCall,
		acf.EventTypeToolResult,
		acf.EventTypeSystemNote,
	}, kiloEventTypes(events))

	require.Equal(t, "user", events[0].Role)
	require.Equal(t, "hello", events[0].Content[0].Text)
	require.ElementsMatch(t, []string{"kilo:step-start", "internal-checkpoint"}, events[1].Tags)
	require.ElementsMatch(t, []string{"kilo:reasoning", "internal-checkpoint"}, events[2].Tags)
	require.Equal(t, "thinking", events[2].Content[0].Text)
	require.Equal(t, "assistant", events[3].Role)
	require.Equal(t, "I will read it", events[3].Content[0].Text)
	require.Equal(t, "call-1", events[4].CallID)
	require.Equal(t, "read", events[4].ToolName)
	require.JSONEq(t, `{"filePath":"README.md"}`, string(events[4].Input))
	require.Equal(t, "call-1", events[5].CallID)
	require.False(t, events[5].IsError)
	require.Equal(t, "file text", events[5].Content[0].Text)
	require.ElementsMatch(t, []string{"kilo:step-finish", "internal-checkpoint"}, events[6].Tags)
	require.Contains(t, string(events[6].NativeExtras), `"tokens"`)
	require.Contains(t, string(events[0].NativeExtras), `"session_id":"s1"`)
}

func TestEncodeKiloBundleAsCanonical_MapsErrorToolAndCompaction(t *testing.T) {
	b := kiloSessionBundle{
		Session: kiloSession{ID: "s2", Directory: "/tmp/project", Title: "Errors", TimeCreated: 1000, TimeUpdated: 2000},
		Messages: []kiloMessageBundle{
			{
				Message: kiloMessage{ID: "m1", SessionID: "s2", Role: "assistant", TimeCreated: 1000, Data: rawJSON(`{"role":"assistant","error":{"name":"UnknownError","data":{"message":"model stopped"}}}`)},
				Parts: []kiloPart{
					{ID: "p1", MessageID: "m1", SessionID: "s2", Type: "tool", TimeCreated: 1001, Data: rawJSON(`{"type":"tool","callID":"call-error","tool":"read","state":{"status":"error","input":{"filePath":"missing.txt"},"error":"missing file","time":{"start":1001,"end":1002}}}`)},
					{ID: "p2", MessageID: "m1", SessionID: "s2", Type: "compaction", TimeCreated: 1003, Data: rawJSON(`{"type":"compaction","auto":true,"tail_start_id":"m-tail"}`)},
				},
			},
		},
	}

	events, err := encodeKiloBundleAsCanonical(b)
	require.NoError(t, err)
	require.Len(t, events, 3)
	require.Equal(t, acf.EventTypeToolCall, events[0].Type)
	require.Equal(t, acf.EventTypeToolResult, events[1].Type)
	require.True(t, events[1].IsError)
	require.Equal(t, "missing file", events[1].Content[0].Text)
	require.Equal(t, acf.EventTypeSystemNote, events[2].Type)
	require.ElementsMatch(t, []string{"kilo:compaction", "internal-checkpoint"}, events[2].Tags)
	require.Contains(t, string(events[2].NativeExtras), `"tail_start_id":"m-tail"`)
}

func TestEncodeKiloBundleAsCanonical_SkipsEmptyReasoningPart(t *testing.T) {
	b := kiloSessionBundle{
		Session: kiloSession{ID: "s1", Directory: "/tmp/project", Title: "Chat", TimeCreated: 1000, TimeUpdated: 2000},
		Messages: []kiloMessageBundle{
			{
				Message: kiloMessage{ID: "m1", SessionID: "s1", Role: "assistant", TimeCreated: 1200, Data: rawJSON(`{"role":"assistant"}`)},
				Parts: []kiloPart{
					// Metadata-only / redacted reasoning part: no text at all.
					{ID: "p1", MessageID: "m1", SessionID: "s1", Type: "reasoning", TimeCreated: 1201, Data: rawJSON(`{"type":"reasoning"}`)},
				},
			},
		},
	}

	events, err := encodeKiloBundleAsCanonical(b)
	require.NoError(t, err)
	require.Empty(t, events, "a reasoning part with no text must not emit an empty system_note")
}

func kiloEventTypes(events []acf.ConversationEvent) []string {
	out := make([]string, 0, len(events))
	for _, e := range events {
		out = append(out, e.Type)
	}
	return out
}

func rawJSON(s string) json.RawMessage {
	return json.RawMessage(s)
}
