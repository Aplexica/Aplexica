package syncd

import (
	"sync"
	"testing"
	"time"
)

// stubRemotePublisher captures every PublishOutbound call so tests
// can assert the orchestrator wires the hook correctly.
type stubRemotePublisher struct {
	mu            sync.Mutex
	events        []OutboundEvent
	largeRetained bool
}

func (s *stubRemotePublisher) SupportsLargeRetainedCheckpoint(OutboundEvent) bool {
	return s.largeRetained
}

func (s *stubRemotePublisher) PublishOutbound(e OutboundEvent) {
	s.mu.Lock()
	s.events = append(s.events, e)
	s.mu.Unlock()
}

func (s *stubRemotePublisher) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.events)
}

func TestPublishOutbound_NilPublisherIsNoOp(t *testing.T) {
	// An orchestrator with RemoteEventPublisher=nil must accept a
	// publishOutbound call without panicking.
	o := &Orchestrator{cfg: Config{}}
	o.publishOutbound(OutboundEvent{
		NamespaceID: "ns",
		EventID:     "e1",
		Timestamp:   time.Now().UTC(),
	})
	// If we got here, the nil-guard worked.
}

func TestPublishOutbound_ForwardsToPublisher(t *testing.T) {
	pub := &stubRemotePublisher{}
	o := &Orchestrator{cfg: Config{RemoteEventPublisher: pub}}

	o.publishOutbound(OutboundEvent{
		NamespaceID: "ns-a",
		BranchID:    "branch-main",
		ArtifactID:  "art-1",
		EventID:     "evt-1",
		ParentHash:  "evt-0",
		Kind:        "memory",
		Type:        "update",
		Timestamp:   time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC),
		Bytes:       []byte(`{"opaque":"canonical bytes"}`),
		Sequence:    42,
		Origin:      "dev-test",
		SourceAgent: "codex",
	})

	if got := pub.Count(); got != 1 {
		t.Fatalf("Count = %d, want 1", got)
	}
	pub.mu.Lock()
	defer pub.mu.Unlock()
	got := pub.events[0]
	if got.NamespaceID != "ns-a" || got.EventID != "evt-1" || got.Sequence != 42 {
		t.Errorf("unexpected event: %+v", got)
	}
	if got.SourceAgent != "codex" {
		t.Errorf("SourceAgent = %q, want codex", got.SourceAgent)
	}
}
