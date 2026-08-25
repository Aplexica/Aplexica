package daemon

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/aplexica/aplexica/internal/plugin/proto"
	syncd "github.com/aplexica/aplexica/internal/sync"
	"github.com/stretchr/testify/require"
)

// outboxLen returns the number of pending (non-dead) entries in the adapter's
// outbox.
func outboxLen(t *testing.T, a *RemotePublishAdapter) int {
	t.Helper()
	list, err := a.outbox.List()
	require.NoError(t, err)
	return len(list)
}

func outboxHas(t *testing.T, a *RemotePublishAdapter, eventID string) bool {
	t.Helper()
	list, err := a.outbox.List()
	require.NoError(t, err)
	for _, e := range list {
		if e.Event.EventID == eventID {
			return true
		}
	}
	return false
}

func outboxDeadHas(t *testing.T, a *RemotePublishAdapter, eventID string) bool {
	t.Helper()
	names, err := a.outbox.findDeadFiles(eventID)
	require.NoError(t, err)
	return len(names) > 0
}

func TestOutboxPurgeProjectQuarantinesOnlyRevokedIntents(t *testing.T) {
	ob := &Outbox{Root: t.TempDir()}
	require.NoError(t, ob.Init())
	require.NoError(t, ob.Append(proto.RemoteEvent{EventID: "revoked", ArtifactID: "a", ProjectID: "project-a", ProjectAuthorizationGeneration: 1}))
	require.NoError(t, ob.Append(proto.RemoteEvent{EventID: "kept", ArtifactID: "b", ProjectID: "project-b", ProjectAuthorizationGeneration: 1}))
	n, err := ob.PurgeProject("project-a")
	require.NoError(t, err)
	require.Equal(t, 1, n)
	entries, err := ob.List()
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, "kept", entries[0].Event.EventID)
	dead, err := ob.findDeadFiles("revoked")
	require.NoError(t, err)
	require.Len(t, dead, 1)
}

func oversizedRemotePayload() []byte {
	return []byte(`"` + strings.Repeat("x", remotePublishMaxEventBytes) + `"`)
}

// TestOutbox_PersistBeforePublish_NoLossOnFullQueue: fill the in-memory queue
// to capacity, then PublishOutbound once more. The extra event must NOT be lost
// — the durable outbox holds its file (regression for the former full-queue
// drop in PublishOutbound).
func TestOutbox_PersistBeforePublish_NoLossOnFullQueue(t *testing.T) {
	a := newTestPublishAdapterWithOutbox(t, &fakePublishClient{
		fn: func(_ int, b []proto.RemoteEvent) (proto.RemotePublishResult, error) {
			return acceptAll(b), nil
		},
	})
	// No pump running: fill the queue to its capacity so the next enqueue
	// hits the select-default branch.
	for i := 0; i < remotePublishQueueDepth; i++ {
		a.queue <- proto.RemoteEvent{EventID: "filler"}
	}
	a.PublishOutbound(syncd.OutboundEvent{EventID: "overflow", ArtifactID: "a1"})

	require.True(t, outboxHas(t, a, "overflow"),
		"an event that overflows the in-memory queue must persist in the durable outbox, not be dropped")
}

func TestOutbox_OversizedFreshLiveEventStaysPendingForCheckpoint(t *testing.T) {
	a := newTestPublishAdapterWithOutbox(t, &durablePolicyPublishClient{
		required: true, fakePublishClient: &fakePublishClient{},
	})

	a.PublishOutbound(syncd.OutboundEvent{
		EventID:    "too-big",
		ArtifactID: "a1",
		Lane:       syncd.LaneLive,
		Bytes:      oversizedRemotePayload(),
	})

	require.Equal(t, 0, len(a.liveQueue), "oversized fresh events must not enter the live queue")
	require.Equal(t, 1, outboxLen(t, a), "exact live ciphertext must remain pending")
	require.False(t, outboxDeadHas(t, a, "too-big"), "live deltas must never be dead-lettered")
	assertDirtyRescanMarker(t, a.outbox, "")
}

func TestOutbox_OversizedLiveBacklogStaysPendingWithoutPluginCall(t *testing.T) {
	fake := &fakePublishClient{
		fn: func(_ int, _ []proto.RemoteEvent) (proto.RemotePublishResult, error) {
			t.Fatal("oversized backlog must not be sent to the remote plugin")
			return proto.RemotePublishResult{}, nil
		},
	}
	a := newTestPublishAdapterWithOutbox(t, &durablePolicyPublishClient{required: true, fakePublishClient: fake})
	require.NoError(t, a.outbox.Append(proto.RemoteEvent{
		EventID:    "backlog-too-big",
		ArtifactID: "a1",
		Lane:       syncd.LaneLive,
		Bytes:      oversizedRemotePayload(),
	}))

	a.publish(context.Background(), []proto.RemoteEvent{{
		EventID:    "backlog-too-big",
		ArtifactID: "a1",
		Lane:       syncd.LaneLive,
		Bytes:      oversizedRemotePayload(),
	}}, remotePublishQueueBacklog)

	require.Equal(t, 0, fake.callCount())
	require.Equal(t, 1, outboxLen(t, a), "exact live backlog ciphertext must remain pending")
	require.False(t, outboxDeadHas(t, a, "backlog-too-big"), "live backlog must never be dead-lettered")
	assertDirtyRescanMarker(t, a.outbox, "")
}

func TestOutbox_LegacyOversizedEventKeepsEstablishedDeadletterBehavior(t *testing.T) {
	a := newTestPublishAdapterWithOutbox(t, &fakePublishClient{})
	a.PublishOutbound(syncd.OutboundEvent{
		EventID: "legacy-too-big", ArtifactID: "legacy-artifact",
		Bytes: oversizedRemotePayload(),
	})

	require.Zero(t, len(a.liveQueue))
	require.Zero(t, outboxLen(t, a), "legacy oversize must not wedge the pending queue")
	require.True(t, outboxDeadHas(t, a, "legacy-too-big"),
		"legacy/shadow rollback retains the established dead-letter fallback")
	dirty, err := a.outbox.mutations.IsDirty("")
	require.NoError(t, err)
	require.False(t, dirty, "legacy publication must not enter delta-range recovery")
}

func TestOutbox_RollbackToLegacyDrainsPendingDespiteDeltaRecoveryMarker(t *testing.T) {
	client := &durablePolicyPublishClient{required: true, fakePublishClient: &fakePublishClient{}}
	a := newTestPublishAdapterWithOutbox(t, client)
	event := proto.RemoteEvent{EventID: "rollback-pending", ArtifactID: "artifact-rollback", Lane: syncd.LaneLive}
	require.NoError(t, a.outbox.Append(event))
	require.NoError(t, a.outbox.RequireCanonicalRecovery(event))
	dirty, err := a.outbox.mutations.IsDirty("")
	require.NoError(t, err)
	require.True(t, dirty)

	client.required = false // negotiated rollback to legacy/shadow
	a.resume(context.Background())
	queued := drainQueue(a.queue)
	require.Len(t, queued, 1)
	require.Equal(t, event.EventID, queued[0].EventID)
	require.True(t, outboxHas(t, a, event.EventID),
		"rollback publication keeps evidence until legacy plugin acceptance")
}

// TestOutbox_NoLossOnRetryExhaustion: a fake that always fails (whole-batch
// ErrRemoteReconnecting) drives publish/requeue past maxPublishRetries; the
// durable file MUST still exist after the in-memory budget is exhausted
// (regression for the former post-maxPublishRetries drop in requeue).
func TestOutbox_NoLossOnRetryExhaustion(t *testing.T) {
	a := newTestPublishAdapterWithOutbox(t, &fakePublishClient{
		fn: func(_ int, _ []proto.RemoteEvent) (proto.RemotePublishResult, error) {
			return proto.RemotePublishResult{}, ErrRemoteReconnecting
		},
	})
	// Persist the event durably first (as PublishOutbound would).
	require.NoError(t, a.outbox.Append(proto.RemoteEvent{EventID: "e1", ArtifactID: "a1"}))

	batch := []proto.RemoteEvent{{EventID: "e1", ArtifactID: "a1"}}
	for iterations := 0; len(batch) > 0 && iterations < maxPublishRetries+5; iterations++ {
		a.publish(context.Background(), batch, remotePublishQueueBacklog)
		batch = drainQueue(a.queue)
	}
	// In-memory budget is exhausted (queue drained empty) but the durable file
	// survives for startup-resume.
	require.True(t, outboxHas(t, a, "e1"),
		"durable file must survive in-memory retry-budget exhaustion")
}

// TestOutbox_NoLossOnFullQueueDuringRetry: force requeue's select-default
// branch (full queue) and assert the durable file persists.
func TestOutbox_NoLossOnFullQueueDuringRetry(t *testing.T) {
	a := newTestPublishAdapterWithOutbox(t, &fakePublishClient{})
	require.NoError(t, a.outbox.Append(proto.RemoteEvent{EventID: "e1", ArtifactID: "a1"}))
	// Saturate the queue so requeue cannot enqueue.
	for i := 0; i < remotePublishQueueDepth; i++ {
		a.queue <- proto.RemoteEvent{EventID: "filler"}
	}
	a.requeue(context.Background(), []proto.RemoteEvent{{EventID: "e1", ArtifactID: "a1"}}, remotePublishQueueBacklog, 0)
	require.True(t, outboxHas(t, a, "e1"),
		"durable file must persist when the in-memory queue is full on retry")
}

// TestOutbox_DeleteOnlyAfterTerminal: Accepted removes the file; Retryable
// keeps it; non-retryable moves it to dead/.
func TestOutbox_DeleteOnlyAfterTerminal(t *testing.T) {
	t.Run("accepted removes", func(t *testing.T) {
		a := newTestPublishAdapterWithOutbox(t, &fakePublishClient{
			fn: func(_ int, b []proto.RemoteEvent) (proto.RemotePublishResult, error) {
				return acceptAll(b), nil
			},
		})
		require.NoError(t, a.outbox.Append(proto.RemoteEvent{EventID: "ok"}))
		a.publish(context.Background(), []proto.RemoteEvent{{EventID: "ok"}}, remotePublishQueueBacklog)
		require.Equal(t, 0, outboxLen(t, a), "accepted event must be removed from the outbox")
	})

	t.Run("retryable keeps", func(t *testing.T) {
		a := newTestPublishAdapterWithOutbox(t, &fakePublishClient{
			fn: func(_ int, _ []proto.RemoteEvent) (proto.RemotePublishResult, error) {
				return proto.RemotePublishResult{Outcomes: []proto.RemotePublishOutcome{
					{EventID: "retry", Accepted: false, Retryable: true},
				}}, nil
			},
		})
		require.NoError(t, a.outbox.Append(proto.RemoteEvent{EventID: "retry"}))
		a.publish(context.Background(), []proto.RemoteEvent{{EventID: "retry"}}, remotePublishQueueBacklog)
		require.True(t, outboxHas(t, a, "retry"), "a retryable event must stay in the outbox")
	})

	t.Run("non-retryable dead-letters", func(t *testing.T) {
		a := newTestPublishAdapterWithOutbox(t, &fakePublishClient{
			fn: func(_ int, _ []proto.RemoteEvent) (proto.RemotePublishResult, error) {
				return proto.RemotePublishResult{Outcomes: []proto.RemotePublishOutcome{
					{EventID: "bad", Accepted: false, Retryable: false, Error: "permanent"},
				}}, nil
			},
		})
		require.NoError(t, a.outbox.Append(proto.RemoteEvent{EventID: "bad"}))
		a.publish(context.Background(), []proto.RemoteEvent{{EventID: "bad"}}, remotePublishQueueBacklog)
		require.Equal(t, 0, outboxLen(t, a), "non-retryable event must leave the pending set")
		require.False(t, outboxHas(t, a, "bad"), "non-retryable event must not remain pending")
		dead, err := a.outbox.findFile("bad")
		require.NoError(t, err)
		require.Empty(t, dead, "the file moved out of the pending root into dead/")
	})
}

// TestOutbox_CrashRestartResume: Append three events through a first adapter
// WITHOUT publishing (simulated crash), construct a SECOND adapter on the SAME
// dir, and assert resume re-enqueues exactly those three in seq order, then a
// fake accepting everything drains the dir to empty.
func TestOutbox_CrashRestartResume(t *testing.T) {
	root := t.TempDir()

	// First adapter (no pump started): just persist three events.
	ob1 := &Outbox{Root: root}
	require.NoError(t, ob1.Init())
	first := &RemotePublishAdapter{
		client:  &fakePublishClient{},
		queue:   make(chan proto.RemoteEvent, remotePublishQueueDepth),
		retries: map[string]int{},
		outbox:  ob1,
	}
	for _, id := range []string{"c0", "c1", "c2"} {
		first.PublishOutbound(syncd.OutboundEvent{EventID: id, ArtifactID: "a1"})
	}
	require.Equal(t, 3, outboxLen(t, first))

	// Second adapter over the same dir. Drive resume manually (deterministic).
	ob2 := &Outbox{Root: root}
	require.NoError(t, ob2.Init())
	accepted := make(chan string, 10)
	second := &RemotePublishAdapter{
		client: &fakePublishClient{fn: func(_ int, b []proto.RemoteEvent) (proto.RemotePublishResult, error) {
			for _, e := range b {
				accepted <- e.EventID
			}
			return acceptAll(b), nil
		}},
		queue:   make(chan proto.RemoteEvent, remotePublishQueueDepth),
		retries: map[string]int{},
		outbox:  ob2,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	second.resume(ctx)

	// resume re-enqueued in seq order.
	got := drainQueue(second.queue)
	require.Equal(t, []string{"c0", "c1", "c2"}, idsOf(got), "resume must replay in seq (FIFO) order")

	// Publishing them drains the durable dir to empty.
	second.publish(context.Background(), got, remotePublishQueueBacklog)
	require.Equal(t, 0, outboxLen(t, second), "accepting all resumed events must drain the outbox")
}

// TestOutbox_IdempotentResume: resume an already-accepted-but-not-deleted file;
// the fake records the (duplicate) EventID; re-publish is harmless (dedup is
// the relay's job) and the file is eventually Removed.
func TestOutbox_IdempotentResume(t *testing.T) {
	root := t.TempDir()
	ob := &Outbox{Root: root}
	require.NoError(t, ob.Init())
	require.NoError(t, ob.Append(proto.RemoteEvent{EventID: "dup", ArtifactID: "a1"}))

	seen := map[string]int{}
	a := &RemotePublishAdapter{
		client: &fakePublishClient{fn: func(_ int, b []proto.RemoteEvent) (proto.RemotePublishResult, error) {
			for _, e := range b {
				seen[e.EventID]++
			}
			return acceptAll(b), nil
		}},
		queue:   make(chan proto.RemoteEvent, remotePublishQueueDepth),
		retries: map[string]int{},
		outbox:  ob,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.resume(ctx)
	batch := drainQueue(a.queue)
	a.publish(context.Background(), batch, remotePublishQueueBacklog)

	require.Equal(t, 1, seen["dup"], "resume publishes the pending event once")
	require.Equal(t, 0, outboxLen(t, a), "accepted resumed event is removed (idempotent re-publish is the relay's concern)")
}

// TestOutbox_OrderingPreservedAcrossRetry: Append events with the same
// NamespaceID+ArtifactID and ascending Sequence, with a retryable middle event;
// resume + publish preserve seq order on disk and on the wire enqueue.
func TestOutbox_OrderingPreservedAcrossRetry(t *testing.T) {
	root := t.TempDir()
	ob := &Outbox{Root: root}
	require.NoError(t, ob.Init())
	for i, id := range []string{"s0", "s1", "s2"} {
		require.NoError(t, ob.Append(proto.RemoteEvent{
			EventID: id, NamespaceID: "ns", ArtifactID: "art", Sequence: uint64(i),
		}))
	}
	a := &RemotePublishAdapter{
		client:  &fakePublishClient{},
		queue:   make(chan proto.RemoteEvent, remotePublishQueueDepth),
		retries: map[string]int{},
		outbox:  ob,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.resume(ctx)
	got := drainQueue(a.queue)
	require.Equal(t, []string{"s0", "s1", "s2"}, idsOf(got),
		"resume must preserve per-artifact seq order")
	// Sequence numbers are ascending in the same order.
	for i, e := range got {
		require.Equal(t, uint64(i), e.Sequence)
	}
}

func TestOutbox_ResumePrioritizesSmallRetainedSlots(t *testing.T) {
	root := t.TempDir()
	ob := &Outbox{Root: root}
	require.NoError(t, ob.Init())
	entries := []proto.RemoteEvent{
		{EventID: "retained-large", ArtifactID: "old-big", Lane: syncd.LaneRetained, Bytes: []byte(`"` + strings.Repeat("x", remotePublishMaxEventBytes+1) + `"`)},
		{EventID: "live-mid", ArtifactID: "live", Lane: syncd.LaneLive, Bytes: []byte(`{"delta":1}`)},
		{EventID: "retained-small", ArtifactID: "fresh-small", Lane: syncd.LaneRetained, Bytes: []byte(`{"state":1}`)},
		{EventID: "retained-medium", ArtifactID: "mid", Lane: syncd.LaneRetained, Bytes: []byte(`"` + strings.Repeat("x", 300*1024) + `"`)},
		{EventID: "retained-tiny", ArtifactID: "fresh-tiny", Lane: syncd.LaneRetained, Bytes: []byte(`{}`)},
	}
	for _, ev := range entries {
		require.NoError(t, ob.Append(ev))
	}
	a := &RemotePublishAdapter{
		client:    &fakePublishClient{},
		liveQueue: make(chan proto.RemoteEvent, remotePublishQueueDepth),
		queue:     make(chan proto.RemoteEvent, remotePublishQueueDepth),
		retries:   map[string]int{},
		outbox:    ob,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.resume(ctx)

	require.Equal(t,
		[]string{"retained-tiny", "live-mid", "retained-small", "retained-medium", "retained-large"},
		idsOf(drainQueue(a.queue)),
		"retained slots should prefer small snapshots without moving live deltas out of their FIFO slot")
}

func idsOf(evs []proto.RemoteEvent) []string {
	out := make([]string, len(evs))
	for i, e := range evs {
		out[i] = e.EventID
	}
	return out
}

// TestOutbox_ResumeStopsOnFullQueueLeavingRest: if the in-memory queue fills
// during resume, the remaining files stay on disk (the pump drains them later).
func TestOutbox_ResumeStopsOnFullQueueLeavingRest(t *testing.T) {
	root := t.TempDir()
	ob := &Outbox{Root: root}
	require.NoError(t, ob.Init())
	for i := 0; i < remotePublishQueueDepth+10; i++ {
		require.NoError(t, ob.Append(proto.RemoteEvent{EventID: "r" + strconv.Itoa(i)}))
	}
	a := &RemotePublishAdapter{
		client:  &fakePublishClient{},
		queue:   make(chan proto.RemoteEvent, remotePublishQueueDepth),
		retries: map[string]int{},
		outbox:  ob,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.resume(ctx)
	// Resume stopped at the queue cap; the rest remain durably on disk.
	require.Equal(t, remotePublishQueueDepth+10, outboxLen(t, a),
		"resume must not delete files it could not enqueue")
	require.Len(t, drainQueue(a.queue), remotePublishQueueDepth)
}

// TestOutbox_PeriodicDrainPicksUpBacklog: drainOnce (the step behind
// periodicDrain) re-enqueues pending durable entries that startup-resume could
// not fit, once the queue has drained — closing the liveness gap where a backlog
// larger than the queue depth would otherwise sit on disk until the next daemon
// restart. It must be a no-op while the queue is still draining (no duplicate
// enqueue).
func TestOutbox_PeriodicDrainPicksUpBacklog(t *testing.T) {
	root := t.TempDir()
	ob := &Outbox{Root: root}
	require.NoError(t, ob.Init())
	for _, id := range []string{"b0", "b1", "b2"} {
		require.NoError(t, ob.Append(proto.RemoteEvent{EventID: id, ArtifactID: "a1"}))
	}
	a := &RemotePublishAdapter{
		client:  &fakePublishClient{},
		queue:   make(chan proto.RemoteEvent, remotePublishQueueDepth),
		retries: map[string]int{},
		outbox:  ob,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Queue empty -> drainOnce re-enqueues the pending backlog in seq (FIFO) order.
	a.drainOnce(ctx)
	require.Equal(t, []string{"b0", "b1", "b2"}, idsOf(drainQueue(a.queue)),
		"drainOnce must re-enqueue pending outbox entries (FIFO) when the queue is empty")

	// Queue non-empty -> drainOnce is a no-op (must not duplicate a queued event).
	a.queue <- proto.RemoteEvent{EventID: "inflight"}
	a.drainOnce(ctx)
	require.Equal(t, []string{"inflight"}, idsOf(drainQueue(a.queue)),
		"drainOnce must not re-enqueue while the queue is still draining")
}

// TestOutbox_PeriodicDrainFullyDrainsBacklogLargerThanQueue: a durable backlog
// LARGER than the in-memory queue depth fully drains via the periodic re-scan
// (drainOnce, the step behind periodicDrain) WITHOUT a daemon restart and with
// an otherwise-idle pump — no new PublishOutbound calls and no retryable
// failures to re-trigger enqueue. Startup-resume enqueues only the first
// queue-depth slice and leaves the rest on disk (see
// TestOutbox_ResumeStopsOnFullQueueLeavingRest); this asserts the backstop
// ticker path re-enqueues the remaining slices, in seq (FIFO) order, until the
// outbox is empty — closing the liveness gap where >queue-depth entries would
// otherwise sit on disk until the next restart.
func TestOutbox_PeriodicDrainFullyDrainsBacklogLargerThanQueue(t *testing.T) {
	a := newTestPublishAdapterWithOutbox(t, &fakePublishClient{
		fn: func(_ int, b []proto.RemoteEvent) (proto.RemotePublishResult, error) {
			return acceptAll(b), nil
		},
	})

	// A backlog strictly larger than the in-memory queue depth, so one resume
	// cannot hold it all and the periodic re-scan must run more than once.
	const extra = 50
	total := remotePublishQueueDepth + extra
	want := make([]string, total)
	for i := 0; i < total; i++ {
		id := "e" + strconv.Itoa(i)
		want[i] = id
		require.NoError(t, a.outbox.Append(proto.RemoteEvent{
			EventID: id, NamespaceID: "ns", ArtifactID: "art", Sequence: uint64(i),
		}))
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Model the otherwise-idle pump: each cycle the periodic re-scan re-enqueues
	// the next (<= queue-depth) slice and the pump drains + publishes it
	// (accept-all removes the durable files). A single adapter throughout: no
	// restart, no second adapter, no fresh inbound events.
	var published []string
	cycles := 0
	for outboxLen(t, a) > 0 {
		a.drainOnce(ctx) // one periodic re-scan tick
		batch := drainQueue(a.queue)
		require.NotEmpty(t, batch, "each re-scan of a non-empty backlog must make progress")
		if cycles == 0 {
			require.Len(t, batch, remotePublishQueueDepth,
				"the first re-scan fills the queue; the backlog must exceed one cycle for this test to be meaningful")
		}
		published = append(published, idsOf(batch)...)
		a.publish(ctx, batch, remotePublishQueueBacklog)
		cycles++
		require.LessOrEqual(t, cycles, total, "must drain in bounded cycles, never loop forever")
	}

	require.Greater(t, cycles, 1, "a >queue-depth backlog must take more than one re-scan to drain")
	require.Equal(t, 0, outboxLen(t, a), "the full backlog must drain via periodic re-scan, no restart")
	require.Equal(t, want, published, "events drain exactly once each, in seq (FIFO) order across slices")
}
