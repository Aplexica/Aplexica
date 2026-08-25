package proxy

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
)

func mustEncodeMemoryPayload(t *testing.T, content string) json.RawMessage {
	t.Helper()
	b, err := acf.EncodePayload(acf.MemoryPayload{Format: "markdown", Content: content})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return b
}

func TestReplayCurrentPayloadSingleCreate(t *testing.T) {
	events := []acf.Event{
		{Type: acf.EventTypeCreate, Timestamp: time.Now(), Payload: mustEncodeMemoryPayload(t, "hello")},
	}
	got, err := ReplayCurrentPayload(events)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	var p acf.MemoryPayload
	if err := json.Unmarshal(got, &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if p.Content != "hello" {
		t.Errorf("got %q", p.Content)
	}
}

func TestReplayCurrentPayloadCreateThenUpdate(t *testing.T) {
	events := []acf.Event{
		{Type: acf.EventTypeCreate, Timestamp: time.Now(), Payload: mustEncodeMemoryPayload(t, "old")},
		{Type: acf.EventTypeUpdate, Timestamp: time.Now(), Payload: mustEncodeMemoryPayload(t, "new")},
	}
	got, err := ReplayCurrentPayload(events)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	var p acf.MemoryPayload
	json.Unmarshal(got, &p)
	if p.Content != "new" {
		t.Errorf("got %q, want 'new'", p.Content)
	}
}

func TestReplayCurrentPayloadNoEvents(t *testing.T) {
	_, err := ReplayCurrentPayload(nil)
	if err == nil {
		t.Error("expected error for empty events")
	}
}

func TestReplayCurrentPayloadOnlyRedaction(t *testing.T) {
	events := []acf.Event{
		{Type: acf.EventTypeRedaction, Timestamp: time.Now()},
	}
	_, err := ReplayCurrentPayload(events)
	if err == nil {
		t.Error("expected error: redaction without a prior create")
	}
}

func TestReplayCurrentPayloadResolutionAfterUpdate(t *testing.T) {
	events := []acf.Event{
		{Type: acf.EventTypeCreate, Timestamp: time.Now(), Payload: mustEncodeMemoryPayload(t, "v1")},
		{Type: acf.EventTypeUpdate, Timestamp: time.Now(), Payload: mustEncodeMemoryPayload(t, "v2")},
		{Type: acf.EventTypeResolution, Timestamp: time.Now(), Payload: mustEncodeMemoryPayload(t, "resolved")},
	}
	got, err := ReplayCurrentPayload(events)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	var p acf.MemoryPayload
	json.Unmarshal(got, &p)
	if p.Content != "resolved" {
		t.Errorf("got %q, want 'resolved' — resolution event must be treated as payload-bearing", p.Content)
	}
}
