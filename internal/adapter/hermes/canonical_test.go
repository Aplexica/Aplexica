package hermes

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/hermesdb"
	"github.com/stretchr/testify/require"
)

func ptrString(s string) *string { return &s }

func TestEncodeBundleAsCanonical_BasicConversation(t *testing.T) {
	b := hermesdb.SessionBundle{
		Session: hermesdb.SessionRow{
			ID:        "sX",
			Source:    "cli",
			Title:     ptrString("Test"),
			StartedAt: 100.0,
		},
		Messages: []hermesdb.MessageRow{
			{Role: "user", Content: ptrString("hello"), Timestamp: 101.0},
			{Role: "assistant", Content: ptrString("hi back"), Timestamp: 102.0},
		},
	}
	events := EncodeBundleAsCanonical(b)
	require.Len(t, events, 2)
	require.Equal(t, acf.EventTypeTurn, events[0].Type)
	require.Equal(t, "user", events[0].Role)
	require.Equal(t, "hello", events[0].Content[0].Text)
	require.Equal(t, "assistant", events[1].Role)
	require.Equal(t, "hi back", events[1].Content[0].Text)
}

func TestEncodeBundleAsCanonical_SkipsLocalCommandRows(t *testing.T) {
	b := hermesdb.SessionBundle{
		Session: hermesdb.SessionRow{ID: "cmd-only", Source: canonicalImportSource, StartedAt: 100.0},
		Messages: []hermesdb.MessageRow{
			{Role: "user", Content: ptrString("<command-name>/model</command-name>"), Timestamp: 101.0},
			{Role: "user", Content: ptrString("<local-command-stdout>Set model to Opus 4.8</local-command-stdout>"), Timestamp: 102.0},
			{Role: "user", Content: ptrString("What is the distance to Sun?"), Timestamp: 103.0},
		},
	}
	events := EncodeBundleAsCanonical(b)
	require.Len(t, events, 1)
	require.Equal(t, "user", events[0].Role)
	require.Equal(t, "What is the distance to Sun?", events[0].Content[0].Text)
}

func TestEncodeBundleAsCanonical_ToolCallsExpandedAfterTurn(t *testing.T) {
	toolCallsJSON := `[{"id":"call_1","type":"function","function":{"name":"bash","arguments":"{\"cmd\":\"ls\"}"}}]`
	b := hermesdb.SessionBundle{
		Session: hermesdb.SessionRow{ID: "sY", Source: "cli", StartedAt: 100.0},
		Messages: []hermesdb.MessageRow{
			{Role: "assistant", Content: ptrString("let me list files"), ToolCalls: ptrString(toolCallsJSON), Timestamp: 101.0},
			{Role: "tool", Content: ptrString("file1\nfile2"), ToolCallID: ptrString("call_1"), ToolName: ptrString("bash"), Timestamp: 102.0},
		},
	}
	events := EncodeBundleAsCanonical(b)
	require.Len(t, events, 3, "assistant message → turn + 1 tool_call (text first); tool → tool_result")
	require.Equal(t, acf.EventTypeTurn, events[0].Type)
	require.Equal(t, "assistant", events[0].Role)
	require.Equal(t, "let me list files", events[0].Content[0].Text)

	require.Equal(t, acf.EventTypeToolCall, events[1].Type)
	require.Equal(t, "call_1", events[1].CallID)
	require.Equal(t, "bash", events[1].ToolName)
	require.JSONEq(t, `{"cmd":"ls"}`, string(events[1].Input))

	require.Equal(t, acf.EventTypeToolResult, events[2].Type)
	require.Equal(t, "call_1", events[2].CallID)
	require.Equal(t, "file1\nfile2", events[2].Content[0].Text)
}

// TestRoundTrip_ToolNamePreserved locks the lossless-fidelity contract for
// messages.tool_name (a real Hermes column): a tool-role result row carrying a
// tool_name must survive hermes→canonical→hermes in canonical mode. Without
// carrying tool_name through NativeExtras the canonical model drops it on
// tool_result/assistant rows, so the round-tripped tool row comes back with a
// nil ToolName.
func TestRoundTrip_ToolNamePreserved(t *testing.T) {
	original := hermesdb.SessionBundle{
		Session: hermesdb.SessionRow{ID: "tn-rt", Source: "cli", StartedAt: 100.0},
		Messages: []hermesdb.MessageRow{
			{Role: "user", Content: ptrString("list files"), Timestamp: 100.0},
			{Role: "assistant", Content: ptrString("let me list files"), Timestamp: 101.0},
			{Role: "tool", Content: ptrString("file1\nfile2"), ToolCallID: ptrString("call_1"), ToolName: ptrString("bash"), Timestamp: 102.0},
		},
	}
	events := EncodeBundleAsCanonical(original)
	roundTripped := DecodeBundleFromCanonical("tn-rt", "", events)

	var toolRow *hermesdb.MessageRow
	for i := range roundTripped.Messages {
		if roundTripped.Messages[i].Role == "tool" {
			toolRow = &roundTripped.Messages[i]
			break
		}
	}
	require.NotNil(t, toolRow, "round-tripped bundle must contain the tool-role result row")
	require.NotNil(t, toolRow.ToolName, "tool_name must survive the canonical round trip")
	require.Equal(t, "bash", *toolRow.ToolName)
}

// TestEncodeBundleAsCanonical_MalformedToolCallsPreserved locks the
// lossless-replication contract for assistant tool_calls that fail to parse:
// a non-nil ToolCalls string that does NOT unmarshal into []herToolCall must
// not be silently dropped. Since it can't become canonical tool_call events,
// it rides verbatim in NativeExtras (extras != nil) so the assistant turn is
// still emitted and the raw string survives hermes→canonical→hermes.
func TestEncodeBundleAsCanonical_MalformedToolCallsPreserved(t *testing.T) {
	malformed := `{not valid tool_calls json`
	b := hermesdb.SessionBundle{
		Session: hermesdb.SessionRow{ID: "tc-bad", Source: "cli", StartedAt: 100.0},
		Messages: []hermesdb.MessageRow{
			{Role: "assistant", Content: ptrString("here goes"), ToolCalls: ptrString(malformed), Timestamp: 101.0},
		},
	}
	events := EncodeBundleAsCanonical(b)

	// No tool_call event is produced (the string doesn't parse), but the
	// assistant turn must still be emitted and carry the raw tool_calls in
	// NativeExtras — not silently discarded.
	require.GreaterOrEqual(t, len(events), 1, "assistant turn must still be emitted")
	require.Equal(t, acf.EventTypeTurn, events[0].Type)
	require.Equal(t, "assistant", events[0].Role)
	require.NotEmpty(t, events[0].NativeExtras, "malformed tool_calls must be carried in NativeExtras, not dropped")
	for _, e := range events {
		require.NotEqual(t, acf.EventTypeToolCall, e.Type, "malformed tool_calls must not yield a tool_call event")
	}

	// Round-trips back verbatim onto the assistant MessageRow.
	out := DecodeBundleFromCanonical("tc-bad", "", events)
	var asst *hermesdb.MessageRow
	for i := range out.Messages {
		if out.Messages[i].Role == "assistant" {
			asst = &out.Messages[i]
			break
		}
	}
	require.NotNil(t, asst, "round-tripped bundle must contain the assistant row")
	require.NotNil(t, asst.ToolCalls, "malformed tool_calls must survive the round trip")
	require.Equal(t, malformed, *asst.ToolCalls, "tool_calls must round-trip verbatim")
}

func TestEncodeBundleAsCanonical_SystemMessage(t *testing.T) {
	b := hermesdb.SessionBundle{
		Session: hermesdb.SessionRow{ID: "sZ", Source: "cli", StartedAt: 100.0},
		Messages: []hermesdb.MessageRow{
			{Role: "system", Content: ptrString("you are helpful"), Timestamp: 100.0},
		},
	}
	events := EncodeBundleAsCanonical(b)
	require.Len(t, events, 1)
	require.Equal(t, acf.EventTypeSystemNote, events[0].Type)
	require.Equal(t, "you are helpful", events[0].Content[0].Text)
}

func TestDecodeBundleFromCanonical_RoundTrip(t *testing.T) {
	original := hermesdb.SessionBundle{
		Session: hermesdb.SessionRow{ID: "rt", Source: "cli", StartedAt: 100.0},
		Messages: []hermesdb.MessageRow{
			{Role: "user", Content: ptrString("q"), Timestamp: 101.0},
			{Role: "assistant", Content: ptrString("a"), Timestamp: 102.0},
		},
	}
	events := EncodeBundleAsCanonical(original)
	roundTripped := DecodeBundleFromCanonical("rt", "", events)
	require.Equal(t, "rt", roundTripped.Session.ID)
	require.Len(t, roundTripped.Messages, 2)
	require.Equal(t, "user", roundTripped.Messages[0].Role)
	require.Equal(t, "q", *roundTripped.Messages[0].Content)
	require.Equal(t, "assistant", roundTripped.Messages[1].Role)
	require.Equal(t, "a", *roundTripped.Messages[1].Content)
}

func TestDecodeBundleFromCanonical_ToolCallReconstruction(t *testing.T) {
	events := []acf.ConversationEvent{
		{Type: acf.EventTypeTurn, Timestamp: time.Unix(100, 0).UTC(), Role: "assistant", Content: []acf.ContentBlock{{Type: "text", Text: "calling bash"}}},
		{Type: acf.EventTypeToolCall, Timestamp: time.Unix(100, 0).UTC(), CallID: "c1", ToolName: "bash", Input: json.RawMessage(`{"cmd":"pwd"}`)},
		{Type: acf.EventTypeToolResult, Timestamp: time.Unix(101, 0).UTC(), CallID: "c1", Content: []acf.ContentBlock{{Type: "text", Text: "/tmp"}}},
	}
	bundle := DecodeBundleFromCanonical("tool-rt", "", events)
	require.GreaterOrEqual(t, len(bundle.Messages), 2)
	// First message is the assistant text turn.
	require.Equal(t, "assistant", bundle.Messages[0].Role)
	require.Equal(t, "calling bash", *bundle.Messages[0].Content)
	// One of the subsequent messages carries tool_calls (search; placement varies by impl).
	hasToolCall := false
	for _, m := range bundle.Messages {
		if m.Role == "assistant" && m.ToolCalls != nil && *m.ToolCalls != "" {
			tc := *m.ToolCalls
			require.Contains(t, tc, "c1")
			require.Contains(t, tc, "bash")
			hasToolCall = true
		}
	}
	require.True(t, hasToolCall, "at least one assistant message must carry tool_calls JSON")
	// Tool result.
	hasResult := false
	for _, m := range bundle.Messages {
		if m.Role == "tool" {
			require.Equal(t, "c1", *m.ToolCallID)
			require.Equal(t, "/tmp", *m.Content)
			hasResult = true
		}
	}
	require.True(t, hasResult)
}

func TestDecodeBundleFromCanonical_SkipsLocalCommandRows(t *testing.T) {
	events := []acf.ConversationEvent{
		{Type: acf.EventTypeTurn, Timestamp: time.Unix(100, 0).UTC(), Role: "user", Content: []acf.ContentBlock{{Type: "text", Text: "<command-name>/model</command-name>"}}},
		{Type: acf.EventTypeTurn, Timestamp: time.Unix(101, 0).UTC(), Role: "user", Content: []acf.ContentBlock{{Type: "text", Text: "<local-command-stdout>Set model to Opus 4.8</local-command-stdout>"}}},
		{Type: acf.EventTypeTurn, Timestamp: time.Unix(102, 0).UTC(), Role: "user", Content: []acf.ContentBlock{{Type: "text", Text: "What is the distance to Sun?"}}},
		{Type: acf.EventTypeTurn, Timestamp: time.Unix(103, 0).UTC(), Role: "assistant", Content: []acf.ContentBlock{{Type: "text", Text: "About 149.6 million kilometers."}}},
	}
	bundle := DecodeBundleFromCanonical("cmd-filter", "claude-code", events)
	require.Len(t, bundle.Messages, 2)
	require.Equal(t, "What is the distance to Sun?", *bundle.Messages[0].Content)
	require.NotNil(t, bundle.Session.Title)
	require.Equal(t, "↪ Claude-code: What is the distance to Sun?", *bundle.Session.Title)
}

// TestDecodeBundleFromCanonical_SessionMetadata locks the export-fidelity
// contract found in E2E follow-up: sessions exported into hermes' DB had
// NO title, message_count=0, and no ended_at — hermes' /resume rendered
// them as "—" rows nobody could recognize as their synced conversations.
func TestDecodeBundleFromCanonical_SessionMetadata(t *testing.T) {
	events := []acf.ConversationEvent{
		{Type: acf.EventTypeTurn, Timestamp: time.Unix(100, 0).UTC(), Role: "system", Content: []acf.ContentBlock{{Type: "text", Text: "<permissions instructions> Filesystem"}}},
		{Type: acf.EventTypeTurn, Timestamp: time.Unix(101, 0).UTC(), Role: "user", Content: []acf.ContentBlock{{Type: "text", Text: "what is my name?"}}},
		{Type: acf.EventTypeTurn, Timestamp: time.Unix(102, 0).UTC(), Role: "assistant", Content: []acf.ContentBlock{{Type: "text", Text: "Your name is Example User."}}},
	}
	b := DecodeBundleFromCanonical("meta-rt", "codex", events)

	require.NotNil(t, b.Session.Title, "exported sessions must carry a title")
	require.Equal(t, "↪ Codex: what is my name?", *b.Session.Title,
		"title = origin agent + first USER message (system preamble skipped), mirroring the claude-side convention")
	require.Equal(t, int64(3), b.Session.MessageCount)
	require.NotNil(t, b.Session.EndedAt)
	require.InDelta(t, 102.0, *b.Session.EndedAt, 0.01, "ended_at = last message timestamp")
}

// Transcripts from claude-code/codex carry harness meta as USER-role
// messages ("<permissions instructions>…", "<command-name>/clear…");
// titles must come from the first HUMAN-looking message instead.
func TestSyncedSessionTitle_SkipsTagLikeMeta(t *testing.T) {
	meta := "<permissions instructions> Filesystem access is restricted"
	q := "what is my name?"
	msgs := []hermesdb.MessageRow{
		{Role: "user", Content: &meta},
		{Role: "user", Content: &q},
	}
	require.Equal(t, "↪ Codex: what is my name?", syncedSessionTitle("codex", msgs))
}

func TestSyncedSessionTitle_EmptyWhenOnlyMeta(t *testing.T) {
	meta := "<command-name>/clear</command-name>"
	msgs := []hermesdb.MessageRow{{Role: "user", Content: &meta}}
	got := syncedSessionTitle("claude-code", msgs)
	require.Empty(t, got, "meta-only sessions must not become visible synced-session titles")
}

// Codex transcripts inject the project AGENTS.md as the FIRST user message;
// the title must come from the actual prompt, matching ExtractTextTurns'
// injected-context filter.
func TestSyncedSessionTitle_SkipsInjectedAgentsPreamble(t *testing.T) {
	pre := "# AGENTS.md instructions for /Users/testuser/aplexica-test\n\n- rules…"
	q := "Conversation re-test. Reply with exactly: ok"
	msgs := []hermesdb.MessageRow{
		{Role: "user", Content: &pre},
		{Role: "user", Content: &q},
	}
	require.Equal(t, "↪ Codex: Conversation re-test. Reply with exactly: ok", syncedSessionTitle("codex", msgs))
}
