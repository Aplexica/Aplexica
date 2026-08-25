package daemon

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/plugin/proto"
	syncd "github.com/aplexica/aplexica/internal/sync"
	"github.com/stretchr/testify/require"
)

type mutableActivationGate struct {
	mu  sync.Mutex
	err error
}

func (g *mutableActivationGate) Check(string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.err
}

func (g *mutableActivationGate) set(err error) {
	g.mu.Lock()
	g.err = err
	g.mu.Unlock()
}

// fakePublishClient is a controllable remotePublishClient for retry tests.
type fakePublishClient struct {
	mu    sync.Mutex
	calls [][]proto.RemoteEvent
	fn    func(n int, batch []proto.RemoteEvent) (proto.RemotePublishResult, error)
}

func (f *fakePublishClient) Publish(_ context.Context, events []proto.RemoteEvent) (proto.RemotePublishResult, error) {
	f.mu.Lock()
	n := len(f.calls)
	f.calls = append(f.calls, append([]proto.RemoteEvent{}, events...))
	f.mu.Unlock()
	return f.fn(n, events)
}

func (f *fakePublishClient) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func drainQueue(q chan proto.RemoteEvent) []proto.RemoteEvent {
	var out []proto.RemoteEvent
	for {
		select {
		case e := <-q:
			out = append(out, e)
		default:
			return out
		}
	}
}

func newTestPublishAdapter(client remotePublishClient) *RemotePublishAdapter {
	return &RemotePublishAdapter{
		client:       client,
		liveQueue:    make(chan proto.RemoteEvent, remotePublishQueueDepth),
		queue:        make(chan proto.RemoteEvent, remotePublishQueueDepth),
		retryBackoff: 0,
		retries:      map[string]int{},
	}
}

// newTestPublishAdapterWithOutbox is newTestPublishAdapter plus a durable
// outbox rooted at a t.TempDir() so no-loss / resume tests can inspect the
// on-disk state.
func newTestPublishAdapterWithOutbox(t *testing.T, client remotePublishClient) *RemotePublishAdapter {
	t.Helper()
	root := t.TempDir()
	ob := &Outbox{Root: root}
	if err := ob.Init(); err != nil {
		t.Fatalf("outbox init: %v", err)
	}
	watermarks := &DurablePublishWatermarkStore{Root: root + "-watermarks"}
	if err := watermarks.Init(); err != nil {
		t.Fatalf("watermark init: %v", err)
	}
	obligations := &RemoteCheckpointObligationStore{Root: root + "-checkpoint-obligations"}
	if err := obligations.Init(); err != nil {
		t.Fatalf("checkpoint obligation init: %v", err)
	}
	return &RemotePublishAdapter{
		client:                client,
		liveQueue:             make(chan proto.RemoteEvent, remotePublishQueueDepth),
		queue:                 make(chan proto.RemoteEvent, remotePublishQueueDepth),
		retryBackoff:          0,
		retries:               map[string]int{},
		outbox:                ob,
		watermarks:            watermarks,
		checkpointObligations: obligations,
	}
}

func TestPublishOutbound_PrioritizesLiveEventOverBacklog(t *testing.T) {
	a := newTestPublishAdapterWithOutbox(t, &fakePublishClient{})
	a.queue <- proto.RemoteEvent{EventID: "old-backlog", ArtifactID: "a-old"}

	a.PublishOutbound(syncd.OutboundEvent{EventID: "fresh-live", ArtifactID: "a-new"})

	first, ok := a.nextEvent(context.Background())
	require.True(t, ok)
	require.Equal(t, "fresh-live", first.EventID)

	second, ok := a.nextEvent(context.Background())
	require.True(t, ok)
	require.Equal(t, "old-backlog", second.EventID)
}

func TestCoalesce_LiveBatchDoesNotPullBacklog(t *testing.T) {
	a := newTestPublishAdapter(&fakePublishClient{})
	a.liveQueue <- proto.RemoteEvent{EventID: "fresh-live-2", ArtifactID: "a-new"}
	a.queue <- proto.RemoteEvent{EventID: "old-backlog", ArtifactID: "a-old"}

	batch := a.coalesceFrom(context.Background(),
		proto.RemoteEvent{EventID: "fresh-live-1", ArtifactID: "a-new"},
		remotePublishQueueLive,
	)

	require.Len(t, batch, 2)
	require.Equal(t, "fresh-live-1", batch[0].EventID)
	require.Equal(t, "fresh-live-2", batch[1].EventID)
	require.Equal(t, 1, len(a.queue), "backlog must remain for its own later RPC")
}

func acceptAll(b []proto.RemoteEvent) proto.RemotePublishResult {
	res := proto.RemotePublishResult{}
	for _, e := range b {
		res.Outcomes = append(res.Outcomes, proto.RemotePublishOutcome{EventID: e.EventID, Accepted: true})
	}
	return res
}

func TestPublishPendingGenerationGateBlocksRPCAndCompletedStateReopens(t *testing.T) {
	pending := errors.New("pending generation activation")
	gate := &mutableActivationGate{err: pending}
	fake := &fakePublishClient{fn: func(_ int, batch []proto.RemoteEvent) (proto.RemotePublishResult, error) {
		return acceptAll(batch), nil
	}}
	a := newTestPublishAdapter(fake)
	a.SetGenerationActivationGate(gate)
	event := proto.RemoteEvent{EventID: "evt-1", ArtifactID: "artifact-1", NamespaceID: "", Bytes: []byte(`{"sealed":true}`)}

	a.publish(context.Background(), []proto.RemoteEvent{event}, remotePublishQueueLive)
	require.Zero(t, fake.callCount(), "pending activation must block before the plugin publish RPC")
	require.Len(t, a.pendingRetry, 1, "blocked work must remain retryable")

	// Model the durable state moving from pending to activated. The same
	// adapter must reopen without restart and transmit the exact queued work.
	gate.set(nil)
	a.pendingRetry = nil
	a.publish(context.Background(), []proto.RemoteEvent{event}, remotePublishQueueLive)
	require.Equal(t, 1, fake.callCount())
}

// TestPublish_TransientErrorRetriesUntilSuccess: a publish RPC error
// (ErrRemoteReconnecting) must NOT drop the batch — the pump re-enqueues and
// retries until it lands. The old code swallowed the error and lost the event.
func TestPublish_TransientErrorRetriesUntilSuccess(t *testing.T) {
	fake := &fakePublishClient{fn: func(n int, b []proto.RemoteEvent) (proto.RemotePublishResult, error) {
		if n == 0 {
			return proto.RemotePublishResult{}, ErrRemoteReconnecting
		}
		return acceptAll(b), nil
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := newTestPublishAdapter(fake)
	a.retryBackoff = time.Millisecond
	go a.pump(ctx)

	a.PublishOutbound(syncd.OutboundEvent{EventID: "e1", ArtifactID: "a1"})

	require.Eventually(t, func() bool { return fake.callCount() >= 2 }, 2*time.Second, 5*time.Millisecond,
		"transient publish failure must be retried, not dropped")
}

// TestPublish_RetryableOutcomeReEnqueued: a per-event Accepted=false,Retryable=true
// outcome is re-enqueued; an Accepted=true event is not.
func TestPublish_RetryableOutcomeReEnqueued(t *testing.T) {
	fake := &fakePublishClient{fn: func(_ int, _ []proto.RemoteEvent) (proto.RemotePublishResult, error) {
		return proto.RemotePublishResult{Outcomes: []proto.RemotePublishOutcome{
			{EventID: "e1", Accepted: false, Retryable: true},
			{EventID: "e2", Accepted: true},
		}}, nil
	}}
	a := newTestPublishAdapter(fake)
	a.publish(context.Background(), []proto.RemoteEvent{{EventID: "e1"}, {EventID: "e2"}}, remotePublishQueueBacklog)

	got := drainQueue(a.queue)
	require.Len(t, got, 1, "only the retryable event should be re-enqueued")
	require.Equal(t, "e1", got[0].EventID)
}

func TestPublish_RetryableLiveOutcomeStaysOnLiveQueue(t *testing.T) {
	fake := &fakePublishClient{fn: func(_ int, _ []proto.RemoteEvent) (proto.RemotePublishResult, error) {
		return proto.RemotePublishResult{Outcomes: []proto.RemotePublishOutcome{
			{EventID: "live-retry", Accepted: false, Retryable: true},
		}}, nil
	}}
	a := newTestPublishAdapter(fake)
	a.queue <- proto.RemoteEvent{EventID: "old-backlog", ArtifactID: "a-old"}

	a.publish(context.Background(),
		[]proto.RemoteEvent{{EventID: "live-retry", ArtifactID: "a-live"}},
		remotePublishQueueLive,
	)

	next, ok := a.nextEvent(context.Background())
	require.True(t, ok)
	require.Equal(t, "live-retry", next.EventID)
	require.Equal(t, []string{"old-backlog"}, idsOf(drainQueue(a.queue)))
}

func TestPublish_ContextCancelledDoesNotRequeue(t *testing.T) {
	fake := &fakePublishClient{fn: func(_ int, _ []proto.RemoteEvent) (proto.RemotePublishResult, error) {
		return proto.RemotePublishResult{}, ErrRemoteReconnecting
	}}
	a := newTestPublishAdapter(fake)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	a.publish(ctx, []proto.RemoteEvent{{EventID: "e1", ArtifactID: "a1"}}, remotePublishQueueBacklog)

	require.Empty(t, drainQueue(a.queue), "shutdown/cancel should stop the pump instead of requeueing forever")
}

// TestPublish_NonRetryableDropped: a non-retryable rejection is dropped, not
// re-enqueued.
func TestPublish_NonRetryableDropped(t *testing.T) {
	fake := &fakePublishClient{fn: func(_ int, _ []proto.RemoteEvent) (proto.RemotePublishResult, error) {
		return proto.RemotePublishResult{Outcomes: []proto.RemotePublishOutcome{
			{EventID: "e1", Accepted: false, Retryable: false, Error: "permanent"},
		}}, nil
	}}
	a := newTestPublishAdapter(fake)
	a.publish(context.Background(), []proto.RemoteEvent{{EventID: "e1"}}, remotePublishQueueBacklog)

	require.Empty(t, drainQueue(a.queue), "a non-retryable rejection must be dropped")
}

// Design rule 1: the live lane is FIFO per artifact. A transiently-failed
// live batch used to requeue at the BACK of liveQueue, so delta N shipped
// after a delta N+1 that arrived during the failure — forcing the receiver
// into a needs-baseline full-state recovery (a multi-MB transfer) for a mere
// RPC blip. A failed live-source batch must be re-attempted BEFORE anything
// newer is pulled off the queues.
func TestPublish_FailedLiveBatchRetriesAheadOfNewerLiveEvents(t *testing.T) {
	fake := &fakePublishClient{fn: func(_ int, _ []proto.RemoteEvent) (proto.RemotePublishResult, error) {
		return proto.RemotePublishResult{}, ErrRemoteReconnecting
	}}
	a := newTestPublishAdapter(fake)

	// delta-2 arrives on the live queue while delta-1's publish RPC fails.
	a.liveQueue <- proto.RemoteEvent{EventID: "delta-2", ArtifactID: "art-1", Lane: syncd.LaneLive, Sequence: 2}
	a.publish(context.Background(),
		[]proto.RemoteEvent{{EventID: "delta-1", ArtifactID: "art-1", Lane: syncd.LaneLive, Sequence: 1}},
		remotePublishQueueLive,
	)

	first, src, ok := a.nextEventWithSource(context.Background())
	require.True(t, ok)
	require.Equal(t, "delta-1", first.EventID,
		"a failed live delta must retry AHEAD of newer live events (per-artifact FIFO)")
	require.Equal(t, remotePublishQueueLive, src)
	batch := a.coalesceFrom(context.Background(), first, src)
	require.Equal(t, []string{"delta-1", "delta-2"}, idsOf(batch),
		"the retried delta leads; newer deltas may only join BEHIND it")
}

// A partially-accepted live batch keeps the retryable remainder's ORDER
// intact, still ahead of newer arrivals.
func TestPublish_RetryableLiveSubsetKeepsOrderAheadOfNewerEvents(t *testing.T) {
	fake := &fakePublishClient{fn: func(_ int, _ []proto.RemoteEvent) (proto.RemotePublishResult, error) {
		return proto.RemotePublishResult{Outcomes: []proto.RemotePublishOutcome{
			{EventID: "delta-1", Accepted: true},
			{EventID: "delta-2", Accepted: false, Retryable: true},
			{EventID: "delta-3", Accepted: false, Retryable: true},
		}}, nil
	}}
	a := newTestPublishAdapter(fake)
	a.liveQueue <- proto.RemoteEvent{EventID: "delta-4", ArtifactID: "art-1", Lane: syncd.LaneLive, Sequence: 4}

	a.publish(context.Background(), []proto.RemoteEvent{
		{EventID: "delta-1", ArtifactID: "art-1", Lane: syncd.LaneLive, Sequence: 1},
		{EventID: "delta-2", ArtifactID: "art-1", Lane: syncd.LaneLive, Sequence: 2},
		{EventID: "delta-3", ArtifactID: "art-1", Lane: syncd.LaneLive, Sequence: 3},
	}, remotePublishQueueLive)

	first, src, ok := a.nextEventWithSource(context.Background())
	require.True(t, ok)
	require.Equal(t, "delta-2", first.EventID)
	batch := a.coalesceFrom(context.Background(), first, src)
	require.Equal(t, []string{"delta-2", "delta-3", "delta-4"}, idsOf(batch),
		"the retryable remainder replays in order, ahead of newer queue entries")
}

// Design rule 1 is PER-ARTIFACT FIFO on the live lane — not a global order. A
// parked retry batch for one artifact must not head-of-line block every other
// artifact for the whole backoff x budget window (a single repeatedly-failing
// publish would otherwise stall ALL outbound sync for minutes): a fresh live
// event for a DIFFERENT artifact overtakes the parked batch immediately,
// while the parked work replays once its backoff deadline passes.
func TestPublish_OtherArtifactLiveEventsOvertakeParkedRetry(t *testing.T) {
	fake := &fakePublishClient{fn: func(_ int, _ []proto.RemoteEvent) (proto.RemotePublishResult, error) {
		return proto.RemotePublishResult{}, ErrRemoteReconnecting
	}}
	a := newTestPublishAdapter(fake)
	a.retryBackoff = 250 * time.Millisecond

	// art-B's fresh delta is already queued when art-A's publish RPC fails.
	a.liveQueue <- proto.RemoteEvent{EventID: "b-1", ArtifactID: "art-B", Lane: syncd.LaneLive, Sequence: 1}
	a.publish(context.Background(),
		[]proto.RemoteEvent{{EventID: "a-1", ArtifactID: "art-A", Lane: syncd.LaneLive, Sequence: 1}},
		remotePublishQueueLive,
	)

	start := time.Now()
	first, src, ok := a.nextEventWithSource(context.Background())
	require.True(t, ok)
	require.Equal(t, "b-1", first.EventID,
		"another artifact's live event must overtake a parked retry batch (per-artifact FIFO, not global)")
	require.Equal(t, remotePublishQueueLive, src)
	require.Less(t, time.Since(start), 200*time.Millisecond,
		"the overtake must not wait out the failing batch's backoff")

	// The parked batch is not lost: it replays once its backoff deadline passes.
	second, _, ok := a.nextEventWithSource(context.Background())
	require.True(t, ok)
	require.Equal(t, "a-1", second.EventID, "the parked retry must still replay after its backoff")
}

// The overtake above must NEVER apply within one artifact: a fresh live event
// for the SAME artifact as a parked retry batch waits behind it (diverted into
// the parked queue in arrival order), so a transient RPC failure can not
// reorder an artifact's delta chain.
func TestPublish_SameArtifactLiveEventWaitsBehindParkedRetry(t *testing.T) {
	fake := &fakePublishClient{fn: func(_ int, _ []proto.RemoteEvent) (proto.RemotePublishResult, error) {
		return proto.RemotePublishResult{}, ErrRemoteReconnecting
	}}
	a := newTestPublishAdapter(fake)
	a.retryBackoff = 250 * time.Millisecond

	// a-2 (same artifact, newer) and b-1 (other artifact) are queued when a-1's
	// publish fails and parks.
	a.liveQueue <- proto.RemoteEvent{EventID: "a-2", ArtifactID: "art-A", Lane: syncd.LaneLive, Sequence: 2}
	a.liveQueue <- proto.RemoteEvent{EventID: "b-1", ArtifactID: "art-B", Lane: syncd.LaneLive, Sequence: 1}
	a.publish(context.Background(),
		[]proto.RemoteEvent{{EventID: "a-1", ArtifactID: "art-A", Lane: syncd.LaneLive, Sequence: 1}},
		remotePublishQueueLive,
	)

	first, src, ok := a.nextEventWithSource(context.Background())
	require.True(t, ok)
	require.Equal(t, "b-1", first.EventID,
		"a-2 must NOT overtake its own artifact's parked a-1; b-1 (other artifact) may")
	require.Equal(t, remotePublishQueueLive, src)

	// Once the backoff passes the parked batch replays with the diverted
	// same-artifact event BEHIND it, in original order.
	second, src2, ok := a.nextEventWithSource(context.Background())
	require.True(t, ok)
	require.Equal(t, "a-1", second.EventID)
	batch := a.coalesceFrom(context.Background(), second, src2)
	require.Equal(t, []string{"a-1", "a-2"}, idsOf(batch),
		"same-artifact order must survive the failure: parked a-1 leads, diverted a-2 follows")
}

// TestPublish_RetryBudgetExhausted: a permanently-failing event is dropped after
// maxPublishRetries re-enqueues instead of cycling forever.
func TestPublish_RetryBudgetExhausted(t *testing.T) {
	fake := &fakePublishClient{fn: func(_ int, _ []proto.RemoteEvent) (proto.RemotePublishResult, error) {
		return proto.RemotePublishResult{}, ErrRemoteReconnecting
	}}
	a := newTestPublishAdapter(fake)

	batch := []proto.RemoteEvent{{EventID: "e1", ArtifactID: "a1"}}
	iterations := 0
	for len(batch) > 0 && iterations < maxPublishRetries+5 {
		a.publish(context.Background(), batch, remotePublishQueueBacklog)
		batch = drainQueue(a.queue)
		iterations++
	}
	require.Empty(t, batch, "event must eventually be dropped, not cycle forever")
	require.LessOrEqual(t, iterations, maxPublishRetries+1, "must drop within the retry budget")
}
