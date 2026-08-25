// Package sse provides a Server-Sent Events stream for the local
// web UI. It exposes:
//
//   - Bus      — a fan-out pub/sub for in-process Event values.
//   - Stream   — an http.Handler that subscribes a connected client
//     to the bus and emits events as SSE frames.
//
// The bus deliberately decouples publishers (orchestrator, conflict
// detector, rule engine) from the HTTP transport. V1 wires a single
// process-wide Bus into the daemon; future tiered/buffered backends
// can swap in without touching the publishers or the HTTP layer.
package sse

import (
	"encoding/json"
	"sync"
	"time"
)

// EventKind enumerates the canonical event types the SPA subscribes
// to. Strings are stable wire identifiers shared with the portal's
// Zod schemas (aplexica-portal/src/shared/schemas/events.ts).
type EventKind string

const (
	KindDaemonState        EventKind = "daemon.state"
	KindAgentActivity      EventKind = "agent.activity"
	KindArtifactImported   EventKind = "artifact.imported"
	KindArtifactSynced     EventKind = "artifact.synced"
	KindArtifactCheckpoint EventKind = "artifact.checkpoint"
	KindArtifactRefused    EventKind = "artifact.refused"
	KindConflictCreated    EventKind = "conflict.created"
	KindConflictResolved   EventKind = "conflict.resolved"
	KindPendingAdded       EventKind = "pending.added"
	KindPendingLinked      EventKind = "pending.linked"
	KindRuleFired          EventKind = "rule.fired"
)

// Event is a single SSE frame's payload. Body is left as a raw json
// blob so publishers can pass whatever shape the kind warrants
// without the bus knowing the schema.
type Event struct {
	// Seq is a monotonically increasing per-bus sequence number. The
	// SSE handler emits it as the SSE `id:` field so clients can
	// reconnect with Last-Event-ID and replay missed frames (V2;
	// V1's bus has no replay buffer so the field is informational).
	Seq uint64 `json:"seq"`

	// Kind classifies the event for SPA-side routing/filtering.
	Kind EventKind `json:"kind"`

	// Timestamp is the publication time in UTC. Set by the bus on
	// Publish; publishers don't need to fill it.
	Timestamp time.Time `json:"ts"`

	// Body is the kind-specific payload. The shape contract lives
	// with the publisher (e.g. "artifact.synced" carries
	// {artifactId, kind, source, target}; "rule.fired" carries
	// {ruleId, artifactId, decision}). Bus serializes it via
	// json.RawMessage so we don't have to enumerate every shape
	// here.
	Body json.RawMessage `json:"body,omitempty"`
}

// subscriberChanBuffer is the per-subscriber send channel depth.
// Big enough to absorb a burst (e.g. an orchestrator tick that
// publishes a dozen events back-to-back) without blocking the
// publisher; small enough that slow clients drop excess events
// rather than backing up the bus.
const subscriberChanBuffer = 64

// Bus is a one-to-many fan-out for Event values.
//
// Concurrency: Subscribe / Unsubscribe / Publish are all safe to call
// from any goroutine. Publish is non-blocking against slow
// subscribers — when a subscriber's channel is full, the event is
// dropped for THAT subscriber and the next Publish proceeds. This
// matters because the orchestrator publishes synchronously inside
// hot paths (file imports, rule firings) and we don't want a
// disconnected browser to stall daemon throughput.
type Bus struct {
	mu      sync.RWMutex
	nextID  uint64
	seq     uint64
	clients map[uint64]chan<- Event
	// dropped tracks the cumulative number of per-subscriber drop
	// occurrences: it is bumped once per (event, full-channel subscriber)
	// pair, so a single Publish to N saturated subscribers raises it by up
	// to N. Exposed via DroppedCount for tests and `aplexica status`-style
	// diagnostics.
	dropped uint64
}

// NewBus returns an empty Bus ready for Subscribe/Publish.
func NewBus() *Bus {
	return &Bus{clients: map[uint64]chan<- Event{}}
}

// Subscribe registers ch to receive future Publishes. Returns an
// opaque token to pass to Unsubscribe when the subscriber goes away.
//
// Callers should pass a buffered channel of size ≥ subscriberChanBuffer.
// An unbuffered channel will be served best-effort: a publish that
// finds the channel busy is dropped (counted in DroppedCount).
func (b *Bus) Subscribe(ch chan<- Event) uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	id := b.nextID
	b.nextID++
	b.clients[id] = ch
	return id
}

// Unsubscribe removes the subscriber identified by id. Idempotent.
func (b *Bus) Unsubscribe(id uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.clients, id)
}

// Publish fans the event out to every current subscriber. Sets
// Timestamp and Seq if the caller left them zero (typical for the
// daemon's call sites). Returns the assigned Seq.
func (b *Bus) Publish(e Event) uint64 {
	b.mu.Lock()
	b.seq++
	e.Seq = b.seq
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now().UTC()
	}
	// Snapshot the subscriber list under the write lock so concurrent
	// Subscribe/Unsubscribe calls don't race with the fan-out send.
	subs := make([]chan<- Event, 0, len(b.clients))
	for _, ch := range b.clients {
		subs = append(subs, ch)
	}
	seq := e.Seq
	b.mu.Unlock()

	for _, ch := range subs {
		select {
		case ch <- e:
		default:
			// Slow subscriber — drop and bump the counter under a
			// short lock window so DroppedCount stays accurate.
			b.mu.Lock()
			b.dropped++
			b.mu.Unlock()
		}
	}
	return seq
}

// SubscriberCount returns the current number of subscribers.
func (b *Bus) SubscriberCount() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.clients)
}

// DroppedCount returns the cumulative number of per-subscriber drop
// occurrences due to full subscriber channels (see the dropped field:
// counted once per slow subscriber per Publish, not once per event).
func (b *Bus) DroppedCount() uint64 {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.dropped
}

// PublishKind is a convenience helper for the common
// "publish event of kind K with marshalable body B" pattern.
//
//	bus.PublishKind(sse.KindArtifactSynced, syncedPayload{...})
//
// If json.Marshal fails (programmer error in the payload) the body
// is replaced with `{"error":"<marshal err>"}` so the event still
// reaches subscribers — silent drops here would mask bugs.
func (b *Bus) PublishKind(kind EventKind, body any) uint64 {
	raw, err := json.Marshal(body)
	if err != nil {
		// Marshal the fallback so err.Error() is escaped; json.Marshal of a
		// map[string]string cannot itself fail, so the data line is always
		// valid JSON the SPA can parse.
		raw, _ = json.Marshal(map[string]string{"error": "sse: marshal body: " + err.Error()})
	}
	return b.Publish(Event{Kind: kind, Body: raw})
}
