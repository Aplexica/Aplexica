package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/conflicts"
)

func TestAnalyzeConflict_ConversationHighlightsFirstVisibleTurnDifference(t *testing.T) {
	headA := conversationEvent(t, "event-a",
		acf.ConversationEvent{Type: acf.EventTypeTurn, Role: "user", Content: textBlocks("what is my name?")},
		acf.ConversationEvent{Type: acf.EventTypeTurn, Role: "assistant", Content: textBlocks("Your name is Example User.")},
	)
	headB := conversationEvent(t, "event-b",
		acf.ConversationEvent{Type: acf.EventTypeTurn, Role: "user", Content: textBlocks("what is my dog's name?")},
		acf.ConversationEvent{Type: acf.EventTypeTurn, Role: "assistant", Content: textBlocks("Comet.")},
	)

	got, err := AnalyzeConflict(conversationConflict("artifact-1"), lookupEvents(headA, headB))
	if err != nil {
		t.Fatalf("AnalyzeConflict: %v", err)
	}
	if got == nil {
		t.Fatal("analysis is nil")
	}
	if !strings.Contains(got.Summary, "turn 1") {
		t.Fatalf("summary = %q, want first turn called out", got.Summary)
	}
	if len(got.Differences) == 0 {
		t.Fatal("expected at least one difference")
	}
	diff := got.Differences[0]
	if diff.Status != "changed" || diff.Label != "Turn 1" {
		t.Fatalf("diff = %+v", diff)
	}
	if !strings.Contains(diff.HeadA, "what is my name?") ||
		!strings.Contains(diff.HeadB, "dog") {
		t.Fatalf("diff = %+v", diff)
	}
}

func TestAnalyzeConflict_ConversationIgnoresInjectedContext(t *testing.T) {
	headA := conversationEvent(t, "event-a",
		acf.ConversationEvent{Type: acf.EventTypeTurn, Role: "user", Content: textBlocks("what is my name?")},
		acf.ConversationEvent{Type: acf.EventTypeTurn, Role: "assistant", Content: textBlocks("Your name is Example User.")},
	)
	headB := conversationEvent(t, "event-b",
		acf.ConversationEvent{Type: acf.EventTypeTurn, Role: "user", Content: textBlocks("<permissions instructions>\nFilesystem sandboxing defines...")},
		acf.ConversationEvent{Type: acf.EventTypeTurn, Role: "user", Content: textBlocks("what is my name?")},
		acf.ConversationEvent{Type: acf.EventTypeTurn, Role: "assistant", Content: textBlocks("Your name is Example User.")},
	)

	got, err := AnalyzeConflict(conversationConflict("artifact-1"), lookupEvents(headA, headB))
	if err != nil {
		t.Fatalf("AnalyzeConflict: %v", err)
	}
	if got == nil {
		t.Fatal("analysis is nil")
	}
	if len(got.Differences) != 0 {
		t.Fatalf("differences = %+v, want none", got.Differences)
	}
	if !strings.Contains(got.Summary, "visible conversation turns match") {
		t.Fatalf("summary = %q", got.Summary)
	}
	if !got.AutoResolvable {
		t.Fatalf("AutoResolvable = false, want true")
	}
	if got.PreferredHead != "B" {
		t.Fatalf("PreferredHead = %q, want newer head B", got.PreferredHead)
	}
	analysisHeadB := got.Heads[1]
	if !strings.Contains(analysisHeadB.PayloadJSON, "\n  \"format\": \"acf.conversation.v1\"") {
		t.Fatalf("PayloadJSON is not pretty JSON: %q", analysisHeadB.PayloadJSON)
	}
	if !strings.Contains(analysisHeadB.PayloadJSON, "<permissions instructions>") {
		t.Fatalf("PayloadJSON missing full raw payload: %q", analysisHeadB.PayloadJSON)
	}
}

func TestAnalyzeConflict_HermesConversationIgnoresHiddenMetadata(t *testing.T) {
	headA := hermesConversationEvent(t, "event-a", `{"session":{"id":"s1","system_prompt":"one"},"messages":[
		{"role":"user","content":"What is the capital of France?"},
		{"role":"assistant","content":"Paris."}
	]}`)
	headB := hermesConversationEvent(t, "event-b", `{"session":{"id":"s1","system_prompt":"two","cost_status":"changed"},"messages":[
		{"role":"user","content":"What is the capital of France?"},
		{"role":"assistant","content":"Paris."}
	]}`)

	got, err := AnalyzeConflict(conversationConflict("artifact-1"), lookupEvents(headA, headB))
	if err != nil {
		t.Fatalf("AnalyzeConflict: %v", err)
	}
	if got == nil {
		t.Fatal("analysis is nil")
	}
	if !got.AutoResolvable {
		t.Fatalf("AutoResolvable = false, want true")
	}
	if !strings.Contains(got.Summary, "Both heads have 2 visible turns") {
		t.Fatalf("summary = %q", got.Summary)
	}
	if len(got.Differences) != 0 {
		t.Fatalf("differences = %+v, want none", got.Differences)
	}
}

func TestAnalyzeConflict_MemoryHighlightsFirstLineDifference(t *testing.T) {
	headA := memoryEvent(t, "event-a", "alpha\nbravo\ncharlie")
	headB := memoryEvent(t, "event-b", "alpha\nbeta\ncharlie")

	got, err := AnalyzeConflict(conflicts.Conflict{
		ArtifactID: "memory-1",
		Kind:       acf.KindMemory,
		Heads: []conflicts.Head{
			{SourceAgent: "claude-code", EventID: "event-a"},
			{SourceAgent: "codex", EventID: "event-b"},
		},
	}, lookupEvents(headA, headB))
	if err != nil {
		t.Fatalf("AnalyzeConflict: %v", err)
	}
	if len(got.Differences) == 0 {
		t.Fatal("expected line difference")
	}
	diff := got.Differences[0]
	if diff.Label != "Line 2" || diff.HeadA != "bravo" || diff.HeadB != "beta" {
		t.Fatalf("diff = %+v", diff)
	}
}

// TestAnalyzeConflict_RemoteHeadFromFullPayload is the B3 fix on the analysis
// path: a remote inbound conflict head is never appended to the local log, so
// the canonical EventID lookup returns ok=false for it. The analysis must fall
// back to the head's preserved FullPayload so the side-by-side diff renders the
// full remote content instead of degrading to the preview-only summary.
func TestAnalyzeConflict_RemoteHeadFromFullPayload(t *testing.T) {
	headA := memoryEvent(t, "event-a", "alpha\nbravo\ncharlie")

	remotePayload, err := acf.EncodePayload(acf.MemoryPayload{Format: "markdown", Content: "alpha\nbeta\ncharlie"})
	if err != nil {
		t.Fatalf("encode remote: %v", err)
	}

	got, err := AnalyzeConflict(conflicts.Conflict{
		ArtifactID: "memory-1",
		Kind:       acf.KindMemory,
		Heads: []conflicts.Head{
			{SourceAgent: "claude-code", EventID: "event-a"},
			// event-b is absent from the local store (remote inbound head) but
			// carries its full payload in the conflict sidecar.
			{SourceAgent: "codex", EventID: "event-b", FullPayload: json.RawMessage(remotePayload)},
		},
	}, lookupEvents(headA)) // only headA resolves; headB lookup returns ok=false
	if err != nil {
		t.Fatalf("AnalyzeConflict: %v", err)
	}
	if len(got.Differences) == 0 {
		t.Fatal("expected a line difference from the recovered remote payload, got preview-only fallback")
	}
	diff := got.Differences[0]
	if diff.Label != "Line 2" || diff.HeadA != "bravo" || diff.HeadB != "beta" {
		t.Fatalf("diff = %+v", diff)
	}
	if len(got.Heads) != 2 || got.Heads[1].PayloadJSON == "" {
		t.Fatalf("expected the remote head to render its full payload JSON, got %+v", got.Heads)
	}
}

func TestAnalyzeConflict_TruncatesLargePayloadJSON(t *testing.T) {
	event := memoryEvent(t, "event-a", strings.Repeat("large-payload ", 1000))

	got := withPayloadJSON(ConflictHeadAnalysis{Label: "A"}, event)

	if got.PayloadJSON == "" {
		t.Fatal("expected payload JSON preview")
	}
	if len([]rune(got.PayloadJSON)) > maxConflictPayloadRunes+len("\n… truncated raw payload preview …") {
		t.Fatalf("payload preview was not capped: %d runes", len([]rune(got.PayloadJSON)))
	}
	if !strings.Contains(got.PayloadJSON, "truncated raw payload preview") {
		t.Fatalf("payload preview missing truncation marker: %q", got.PayloadJSON)
	}
}

func conversationConflict(id string) conflicts.Conflict {
	return conflicts.Conflict{
		ArtifactID: id,
		Kind:       acf.KindConversation,
		Heads: []conflicts.Head{
			{SourceAgent: "claude-code", EventID: "event-a", AbsTimestamp: 100},
			{SourceAgent: "codex", EventID: "event-b", AbsTimestamp: 103},
		},
	}
}

func lookupEvents(events ...acf.Event) ConflictEventLookup {
	byID := make(map[string]acf.Event, len(events))
	for _, e := range events {
		byID[e.EventID] = e
	}
	return func(_ context.Context, _ acf.Kind, _ string, eventID string) (acf.Event, bool, error) {
		e, ok := byID[eventID]
		return e, ok, nil
	}
}

func conversationEvent(t *testing.T, id string, events ...acf.ConversationEvent) acf.Event {
	t.Helper()
	payload, err := acf.EncodePayload(acf.ConversationPayload{
		Format: acf.ConversationFormatV1,
		Events: events,
	})
	if err != nil {
		t.Fatalf("encode conversation: %v", err)
	}
	return acf.Event{
		EventID:   id,
		Type:      acf.EventTypeUpdate,
		Timestamp: time.Now().UTC(),
		Payload:   payload,
	}
}

func hermesConversationEvent(t *testing.T, id, content string) acf.Event {
	t.Helper()
	payload, err := acf.EncodePayload(acf.ConversationPayload{
		Format:  acf.ConversationFormatHermesBundle,
		Content: content,
	})
	if err != nil {
		t.Fatalf("encode hermes conversation: %v", err)
	}
	return acf.Event{
		EventID:   id,
		Type:      acf.EventTypeUpdate,
		Timestamp: time.Now().UTC(),
		Payload:   payload,
	}
}

func memoryEvent(t *testing.T, id, content string) acf.Event {
	t.Helper()
	payload, err := acf.EncodePayload(acf.MemoryPayload{Format: "markdown", Content: content})
	if err != nil {
		t.Fatalf("encode memory: %v", err)
	}
	return acf.Event{
		EventID:   id,
		Type:      acf.EventTypeUpdate,
		Timestamp: time.Now().UTC(),
		Payload:   payload,
	}
}

func textBlocks(text string) []acf.ContentBlock {
	return []acf.ContentBlock{{Type: "text", Text: text}}
}
