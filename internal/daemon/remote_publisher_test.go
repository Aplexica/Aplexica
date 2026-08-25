package daemon

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/aplexica/aplexica/internal/plugin/proto"
	syncd "github.com/aplexica/aplexica/internal/sync"
)

// TestToRemoteEvent_Translation verifies the OutboundEvent -> RemoteEvent
// field mapping is faithful, including the opaque Bytes passthrough.
func TestToRemoteEvent_Translation(t *testing.T) {
	ts := time.Date(2026, 6, 14, 10, 0, 0, 0, time.UTC)
	out := syncd.OutboundEvent{
		NamespaceID:             "ns-1",
		BranchID:                "main",
		ArtifactID:              "art-1",
		EventID:                 "evt-1",
		ParentHash:              "evt-0",
		CheckpointAlignmentHash: "aligned-head-1",
		Kind:                    "memory",
		Type:                    "update",
		Timestamp:               ts,
		Bytes:                   []byte(`{"opaque":true}`),
		Sequence:                7,
		Origin:                  "dev-a",
		SourceAgent:             "claude-code",
	}
	re := toRemoteEvent(out)
	if re.NamespaceID != "ns-1" || re.BranchID != "main" || re.ArtifactID != "art-1" {
		t.Fatalf("routing fields mismatch: %+v", re)
	}
	if re.EventID != "evt-1" || re.ParentHash != "evt-0" || re.CheckpointAlignmentHash != "aligned-head-1" || re.Sequence != 7 {
		t.Fatalf("chain fields mismatch: %+v", re)
	}
	if re.Kind != "memory" || re.Type != "update" || re.Origin != "dev-a" {
		t.Fatalf("kind/type/origin mismatch: %+v", re)
	}
	if re.SourceAgent != "claude-code" {
		t.Fatalf("source agent mismatch: %+v", re)
	}
	if !re.Timestamp.Equal(ts) {
		t.Fatalf("timestamp mismatch: %v", re.Timestamp)
	}
	if string(re.Bytes) != `{"opaque":true}` {
		t.Fatalf("opaque bytes not passed through: %s", string(re.Bytes))
	}
	// Confirm the wire shape round-trips through JSON with the expected tags.
	b, err := json.Marshal(re)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(b, &probe); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"namespace_id", "branch_id", "artifact_id", "event_id", "parent_hash", "checkpoint_alignment_hash", "kind", "event_type", "ts", "bytes", "seq", "origin"} {
		if _, ok := probe[key]; !ok {
			t.Errorf("wire JSON missing key %q", key)
		}
	}
}

func TestRemoteEventApproxBytesIncludesCheckpointAlignment(t *testing.T) {
	base := proto.RemoteEvent{EventID: "checkpoint", Lane: syncd.LaneRetained, Bytes: json.RawMessage(`{"sealed":true}`)}
	withAlignment := base
	withAlignment.CheckpointAlignmentHash = strings.Repeat("a", 64)
	require.Equal(t, 64, remoteEventApproxBytes(withAlignment)-remoteEventApproxBytes(base))
}

// TestPublishOutbound_NonBlockingNoLossWhenFull verifies the adapter never
// blocks the caller AND never loses an event when the in-memory queue is
// saturated. With the durable outbox (B1) a full queue is no longer a drop
// point: PublishOutbound persists the event to the outbox first (so it is
// recoverable), then attempts a non-blocking enqueue. We construct the adapter
// directly (no pump) with an outbox so the queue stays full and the overflow
// path is exercised without a network call.
func TestPublishOutbound_NonBlockingNoLossWhenFull(t *testing.T) {
	ob := &Outbox{Root: t.TempDir()}
	if err := ob.Init(); err != nil {
		t.Fatalf("outbox init: %v", err)
	}
	a := &RemotePublishAdapter{
		client:  &RemoteRunner{Executable: "/bin/true"},
		queue:   make(chan proto.RemoteEvent, remotePublishQueueDepth),
		retries: map[string]int{},
		outbox:  ob,
	}
	// Saturate the in-memory queue so every PublishOutbound below hits the
	// select-default (full) branch.
	for i := 0; i < remotePublishQueueDepth; i++ {
		a.queue <- proto.RemoteEvent{EventID: "filler"}
	}

	const overflow = 20
	done := make(chan struct{})
	go func() {
		for i := 0; i < overflow; i++ {
			a.PublishOutbound(syncd.OutboundEvent{EventID: "ov-" + strconv.Itoa(i), ArtifactID: "a"})
		}
		close(done)
	}()

	select {
	case <-done:
		// Good: all calls returned without blocking on the full queue.
	case <-time.After(10 * time.Second):
		t.Fatal("PublishOutbound blocked: the non-blocking contract is violated")
	}

	// None of the overflow events were lost: each persists durably in the outbox.
	list, err := ob.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	got := map[string]bool{}
	for _, e := range list {
		got[e.Event.EventID] = true
	}
	for i := 0; i < overflow; i++ {
		id := "ov-" + strconv.Itoa(i)
		if !got[id] {
			t.Fatalf("overflow event %q lost; durable outbox must retain it", id)
		}
	}
}

func TestCoalesceCapsRemotePublishBatch(t *testing.T) {
	a := &RemotePublishAdapter{
		queue:   make(chan proto.RemoteEvent, remotePublishQueueDepth),
		retries: map[string]int{},
	}
	for i := 0; i < remotePublishMaxBatch+5; i++ {
		a.queue <- proto.RemoteEvent{EventID: "queued-" + strconv.Itoa(i)}
	}

	start := time.Now()
	batch := a.coalesce(context.Background(), proto.RemoteEvent{EventID: "first"})
	if len(batch) != remotePublishMaxBatch {
		t.Fatalf("batch len = %d, want cap %d", len(batch), remotePublishMaxBatch)
	}
	if elapsed := time.Since(start); elapsed >= publishBatchWindow {
		t.Fatalf("coalesce waited for window despite hitting batch cap: %v", elapsed)
	}
	if batch[0].EventID != "first" {
		t.Fatalf("first event was not preserved at batch head: %+v", batch[0])
	}
}

func TestCoalesceSendsLargeFirstEventAlone(t *testing.T) {
	a := &RemotePublishAdapter{
		queue:   make(chan proto.RemoteEvent, remotePublishQueueDepth),
		retries: map[string]int{},
	}
	a.queue <- proto.RemoteEvent{EventID: "queued", Bytes: []byte("small")}

	start := time.Now()
	batch := a.coalesce(context.Background(), proto.RemoteEvent{
		EventID: "first-large",
		Bytes:   make([]byte, remotePublishLargeEventBytes),
	})
	if len(batch) != 1 {
		t.Fatalf("batch len = %d, want one large event alone", len(batch))
	}
	if elapsed := time.Since(start); elapsed >= publishBatchWindow {
		t.Fatalf("coalesce waited despite large first event: %v", elapsed)
	}
	if got := len(a.queue); got != 1 {
		t.Fatalf("queue len = %d, want queued event left for next batch", got)
	}
}

func TestCoalesceReturnsWhenByteBudgetReached(t *testing.T) {
	a := &RemotePublishAdapter{
		queue:   make(chan proto.RemoteEvent, remotePublishQueueDepth),
		retries: map[string]int{},
	}
	chunk := remotePublishMaxBatchBytes / 3
	a.queue <- proto.RemoteEvent{EventID: "queued-1", Bytes: make([]byte, chunk)}
	a.queue <- proto.RemoteEvent{EventID: "queued-2", Bytes: make([]byte, chunk)}
	a.queue <- proto.RemoteEvent{EventID: "queued-3", Bytes: []byte("after-budget")}

	start := time.Now()
	batch := a.coalesce(context.Background(), proto.RemoteEvent{
		EventID: "first",
		Bytes:   make([]byte, chunk),
	})
	if len(batch) != 3 {
		t.Fatalf("batch len = %d, want byte-budget batch of 3", len(batch))
	}
	if elapsed := time.Since(start); elapsed >= publishBatchWindow {
		t.Fatalf("coalesce waited for window despite hitting byte budget: %v", elapsed)
	}
	if got := len(a.queue); got != 1 {
		t.Fatalf("queue len = %d, want one event left for next batch", got)
	}
}

// TestPublishOutbound_NilProxyTolerated verifies a live pump tolerates a
// runner whose plugin isn't connected (Publish -> ErrRemoteReconnecting): the
// call must not panic and the pump must keep running.
func TestPublishOutbound_NilProxyTolerated(t *testing.T) {
	r := &RemoteRunner{Executable: "/bin/true"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := NewRemotePublishAdapter(ctx, r, t.TempDir(), nil)

	a.PublishOutbound(syncd.OutboundEvent{EventID: "e1", ArtifactID: "a1", Kind: "memory"})
	// Give the pump a moment to drain + attempt the (failing) publish.
	time.Sleep(250 * time.Millisecond)
	// If we reach here without a panic, the nil-proxy path is tolerated.
}

// ---------------------------------------------------------------------------
// Transport lanes for aligned-chains delta sync.
// ---------------------------------------------------------------------------

// TestToRemoteEvent_CopiesLane pins the OutboundEvent.Lane -> RemoteEvent.Lane
// translation and the wire tag: without it the plugin cannot route live vs
// retained topics. Legacy laneless events keep the key off the wire
// (omitempty) so an old persisted outbox entry replays byte-identically.
func TestToRemoteEvent_CopiesLane(t *testing.T) {
	re := toRemoteEvent(syncd.OutboundEvent{EventID: "evt-1", Lane: syncd.LaneRetained})
	require.Equal(t, syncd.LaneRetained, re.Lane)

	b, err := json.Marshal(re)
	require.NoError(t, err)
	require.Contains(t, string(b), `"lane":"retained"`)

	b, err = json.Marshal(toRemoteEvent(syncd.OutboundEvent{EventID: "evt-2"}))
	require.NoError(t, err)
	require.NotContains(t, string(b), `"lane"`)
}

// TestToRemoteEvent_CopiesClear pins the retained-slot CLEAR plumbing
// (redaction propagation): OutboundEvent.Clear crosses the daemon->plugin
// boundary as proto.RemoteEvent.Clear (`json:"clear,omitempty"`), absent on
// ordinary events.
func TestToRemoteEvent_CopiesClear(t *testing.T) {
	re := toRemoteEvent(syncd.OutboundEvent{EventID: "evt-1-r", Lane: syncd.LaneRetained, Clear: true})
	require.True(t, re.Clear)

	b, err := json.Marshal(re)
	require.NoError(t, err)
	require.Contains(t, string(b), `"clear":true`)

	b, err = json.Marshal(toRemoteEvent(syncd.OutboundEvent{EventID: "evt-2"}))
	require.NoError(t, err)
	require.NotContains(t, string(b), `"clear"`)
}

// TestOutbox_RoundTripsClear: a retained-slot CLEAR (empty Bytes) must survive
// Append -> List so a post-crash resume still clears the broker's retained
// slot instead of leaving it serving pre-redaction state.
func TestOutbox_RoundTripsClear(t *testing.T) {
	ob := &Outbox{Root: t.TempDir()}
	require.NoError(t, ob.Init())
	require.NoError(t, ob.Append(proto.RemoteEvent{
		EventID: "evt-red-r", ArtifactID: "art-1",
		Lane: syncd.LaneRetained, Clear: true,
	}))

	entries, err := ob.List()
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.True(t, entries[0].Event.Clear)
	// A nil RawMessage encodes as JSON null; either shape means "no body".
	body := string(entries[0].Event.Bytes)
	require.True(t, body == "" || body == "null",
		"a clear must round-trip with no usable body, got %q", body)
}

// TestDeadletterIfOversize_LaneAwareCaps pins the per-lane byte caps (design
// rule 6): live, retained, and legacy laneless events keep the practical
// realtime per-message cap until retained baselines are chunked. Oversized live
// work is parked for checkpoint recovery; retained redundancy still uses the
// bounded dead-letter path.
func TestDeadletterIfOversize_LaneAwareCaps(t *testing.T) {
	a := &RemotePublishAdapter{retries: map[string]int{}}
	notified := make(chan map[string]any, 4)
	a.SetEventNotifier(func(kind string, body map[string]any) {
		require.Equal(t, "remote.outbound_oversized", kind)
		notified <- body
	})

	// Live keeps the 4 MB cap.
	live := proto.RemoteEvent{EventID: "e-live", ArtifactID: "art-live",
		Lane: syncd.LaneLive, Bytes: make(json.RawMessage, remotePublishMaxEventBytes+1)}
	require.True(t, a.deadletterIfOversize(live, "test"), "lane=live must keep the 4 MB cap")
	require.Equal(t, remotePublishMaxEventBytes, (<-notified)["limit"])

	// Legacy laneless events predate the split and keep the live cap too.
	legacy := proto.RemoteEvent{EventID: "e-legacy", ArtifactID: "art-legacy",
		Bytes: make(json.RawMessage, remotePublishMaxEventBytes+1)}
	require.True(t, a.deadletterIfOversize(legacy, "test"), "legacy laneless events must keep the 4 MB cap")
	require.Equal(t, remotePublishMaxEventBytes, (<-notified)["limit"])

	// Retained admits baselines up to the practical retained cap.
	retained := proto.RemoteEvent{EventID: "e-retained", ArtifactID: "art-retained",
		Lane: syncd.LaneRetained, Bytes: make(json.RawMessage, remotePublishMaxRetainedEventBytes-1024)}
	require.False(t, a.deadletterIfOversize(retained, "test"),
		"a retained event under the retained cap must be accepted")

	// ...but only up to the practical retained cap.
	overRetained := proto.RemoteEvent{EventID: "e-over-retained", ArtifactID: "art-over-retained",
		Lane: syncd.LaneRetained, Bytes: make(json.RawMessage, remotePublishMaxRetainedEventBytes+1)}
	require.True(t, a.deadletterIfOversize(overRetained, "test"),
		"a retained event above remotePublishMaxRetainedEventBytes must dead-letter")
	require.Equal(t, remotePublishMaxRetainedEventBytes, (<-notified)["limit"])

	select {
	case body := <-notified:
		t.Fatalf("unexpected extra oversized notify: %v", body)
	default:
	}
}

// TestCoalesceFrom_RetainedShipsAloneAndPreservesFIFO pins the
// remotePublishMaxBatch bypass: a retained event NEVER shares a
// remote.publish batch. A retained first returns immediately (no batching
// window); a retained event pulled mid-window ends the current batch and is
// parked so the pump's next iteration ships it alone, ahead of later queue
// entries (queue order preserved).
func TestCoalesceFrom_RetainedShipsAloneAndPreservesFIFO(t *testing.T) {
	ctx := context.Background()
	a := newTestPublishAdapter(&fakePublishClient{})

	a.liveQueue <- proto.RemoteEvent{EventID: "live-1", Lane: syncd.LaneLive}
	start := time.Now()
	batch := a.coalesceFrom(ctx, proto.RemoteEvent{EventID: "retained-1", Lane: syncd.LaneRetained}, remotePublishQueueLive)
	require.Equal(t, []string{"retained-1"}, idsOf(batch), "a retained first must ship as a solo batch")
	require.Less(t, time.Since(start), publishBatchWindow, "a retained first must not wait out the batching window")
	require.Len(t, a.liveQueue, 1, "queued live work must stay for its own later RPC")

	a.liveQueue <- proto.RemoteEvent{EventID: "retained-2", Lane: syncd.LaneRetained}
	a.liveQueue <- proto.RemoteEvent{EventID: "live-2", Lane: syncd.LaneLive}

	first, src, ok := a.nextEventWithSource(ctx)
	require.True(t, ok)
	require.Equal(t, "live-1", first.EventID)
	require.Equal(t, []string{"live-1"}, idsOf(a.coalesceFrom(ctx, first, src)),
		"a retained event pulled mid-window must end the live batch, not join it")

	next, src, ok := a.nextEventWithSource(ctx)
	require.True(t, ok)
	require.Equal(t, "retained-2", next.EventID, "the parked retained event must go out before later queue entries")
	require.Equal(t, []string{"retained-2"}, idsOf(a.coalesceFrom(ctx, next, src)))

	last, _, ok := a.nextEventWithSource(ctx)
	require.True(t, ok)
	require.Equal(t, "live-2", last.EventID)
}

// laneRecordingClient records, per Publish call, the batch composition and the
// remaining ctx deadline budget so lane-aware batching/timeout tests can
// assert on them. Accepts everything.
type laneRecordingClient struct {
	mu    sync.Mutex
	calls []laneRecordedCall
}

type laneRecordedCall struct {
	ids    []string
	budget time.Duration // time.Until(ctx deadline) at call time; 0 if none
}

func (f *laneRecordingClient) Publish(ctx context.Context, events []proto.RemoteEvent) (proto.RemotePublishResult, error) {
	call := laneRecordedCall{ids: idsOf(events)}
	if d, ok := ctx.Deadline(); ok {
		call.budget = time.Until(d)
	}
	f.mu.Lock()
	f.calls = append(f.calls, call)
	f.mu.Unlock()
	return acceptAll(events), nil
}

func (f *laneRecordingClient) snapshot() []laneRecordedCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]laneRecordedCall{}, f.calls...)
}

// TestPublish_RetainedUnderCapShipsSoloWithLongTimeout drives the real pump
// end to end: a lane=retained baseline under the retained cap must reach the
// plugin (not the dead-letter path), as a SOLO batch, with the long
// publishRetainedCallTimeout budget, while live events keep the short
// publishCallTimeout.
func TestPublish_RetainedUnderCapShipsSoloWithLongTimeout(t *testing.T) {
	fake := &laneRecordingClient{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := newTestPublishAdapter(fake)
	go a.pump(ctx)

	a.PublishOutbound(syncd.OutboundEvent{
		EventID: "big-retained", ArtifactID: "art-1", Kind: "conversation",
		Lane: syncd.LaneRetained, Bytes: make([]byte, remotePublishMaxRetainedEventBytes-1024),
	})
	a.PublishOutbound(syncd.OutboundEvent{
		EventID: "small-live", ArtifactID: "art-1", Kind: "conversation",
		Lane: syncd.LaneLive, Bytes: []byte(`{"delta":true}`),
	})

	var calls []laneRecordedCall
	require.Eventually(t, func() bool {
		calls = fake.snapshot()
		seen := map[string]bool{}
		for _, c := range calls {
			for _, id := range c.ids {
				seen[id] = true
			}
		}
		return seen["big-retained"] && seen["small-live"]
	}, 5*time.Second, 10*time.Millisecond, "the retained event under the cap must be published, not dead-lettered")

	for _, c := range calls {
		for _, id := range c.ids {
			switch id {
			case "big-retained":
				require.Equal(t, []string{"big-retained"}, c.ids, "a retained event must ship as a solo batch")
				require.Greater(t, c.budget, publishCallTimeout, "a retained batch must use the long call timeout")
			case "small-live":
				require.Positive(t, c.budget)
				require.LessOrEqual(t, c.budget, publishCallTimeout, "a live batch must keep the short call timeout")
			}
		}
	}
}

// TestOutbox_LiveAndRetainedLanesOfOneCommitPersistDistinctFiles pins the
// EventID-collision fix: the two lanes of ONE conversation commit carry
// DISTINCT wire/outbox EventIDs (retained = the origin-scoped head EventID +
// "-r-<dev8>", syncd.RetainedWireEventID), so the durable outbox — whose
// Append dedupes by EventID — persists BOTH lanes. With a shared id the
// retained lane's persist-before-publish (B1) append silently no-opped
// against the live lane's file, leaving the recovery lane with no durable
// backing.
func TestOutbox_LiveAndRetainedLanesOfOneCommitPersistDistinctFiles(t *testing.T) {
	a := newTestPublishAdapterWithOutbox(t, &fakePublishClient{})
	const id = "evt-1"
	const origin = "origin-device"
	a.PublishOutbound(syncd.OutboundEvent{
		EventID: id, ArtifactID: "art-1", Kind: "conversation",
		Lane: syncd.LaneLive, Bytes: []byte(`{"delta":true}`),
	})
	a.PublishOutbound(syncd.OutboundEvent{
		EventID: syncd.RetainedWireEventID(id, origin), ArtifactID: "art-1", Kind: "conversation",
		Lane: syncd.LaneRetained, Bytes: []byte(`{"full":true}`),
	})

	entries, err := a.outbox.List()
	require.NoError(t, err)
	ids := make([]string, 0, len(entries))
	for _, e := range entries {
		ids = append(ids, e.Event.EventID)
	}
	require.Equal(t, []string{id, syncd.RetainedWireEventID(id, origin)}, ids,
		"each lane needs its OWN durable outbox file (persist-before-publish)")
}

// TestDeadletterIfOversize_RetainedLeavesLiveOutboxFileIntact pins the second
// half of the EventID-collision fix: dead-lettering an oversized retained
// sealed) lane=retained event must move the RETAINED lane's own durable file
// into dead/ — never the live delta's file, and never block future appends
// for the live EventID (Append refuses ids with a dead/ entry).
func TestDeadletterIfOversize_RetainedLeavesLiveOutboxFileIntact(t *testing.T) {
	a := newTestPublishAdapterWithOutbox(t, &fakePublishClient{})
	const id = "evt-1"
	const origin = "origin-device"
	a.PublishOutbound(syncd.OutboundEvent{
		EventID: id, ArtifactID: "art-1", Kind: "conversation",
		Lane: syncd.LaneLive, Bytes: []byte(`{"delta":true}`),
	})
	// Valid-JSON payload just over the retained cap (the outbox marshals
	// Bytes as json.RawMessage, so it must be well-formed JSON).
	a.PublishOutbound(syncd.OutboundEvent{
		EventID: syncd.RetainedWireEventID(id, origin), ArtifactID: "art-1", Kind: "conversation",
		Lane: syncd.LaneRetained, Bytes: []byte(`"` + strings.Repeat("x", remotePublishMaxRetainedEventBytes) + `"`),
	})

	entries, err := a.outbox.List()
	require.NoError(t, err)
	require.Len(t, entries, 1, "the live delta's durable file must survive the retained dead-letter")
	require.Equal(t, id, entries[0].Event.EventID)

	deadRetained, err := a.outbox.findDeadFiles(syncd.RetainedWireEventID(id, origin))
	require.NoError(t, err)
	require.Len(t, deadRetained, 1, "the oversized retained event itself must be dead-lettered")

	deadLive, err := a.outbox.findDeadFiles(id)
	require.NoError(t, err)
	require.Empty(t, deadLive, "the live EventID must not gain a dead/ entry (it would block future appends)")
}

// TestOutbox_RoundTripsLane pins the durable outbox JSON shape: a persisted
// event's Lane survives Append -> List so a post-restart resume republishes it
// on the correct transport lane.
func TestOutbox_RoundTripsLane(t *testing.T) {
	ob := &Outbox{Root: t.TempDir()}
	require.NoError(t, ob.Init())
	require.NoError(t, ob.Append(proto.RemoteEvent{
		EventID: "evt-lane", ArtifactID: "art-1",
		Lane:  syncd.LaneRetained,
		Bytes: json.RawMessage(`{"sealed":"payload"}`),
	}))

	entries, err := ob.List()
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, syncd.LaneRetained, entries[0].Event.Lane)
}
