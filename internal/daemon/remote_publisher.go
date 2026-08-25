package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aplexica/aplexica/internal/plugin/proto"
	"github.com/aplexica/aplexica/internal/securityepoch"
	"github.com/aplexica/aplexica/internal/securityerr"
	syncd "github.com/aplexica/aplexica/internal/sync"
)

// RemotePublishAdapter bridges the sync orchestrator's outbound hook
// (syncd.RemoteEventPublisher) to the RemoteRunner's Publish call. It
// translates a syncd.OutboundEvent into the wire proto.RemoteEvent and hands
// it to the plugin for transmission.
//
// Contract (see syncd.RemoteEventPublisher): PublishOutbound is invoked
// SYNCHRONOUSLY from the orchestrator's import path and MUST NOT block on a
// network call. This adapter honours that by enqueuing the event onto a
// buffered channel and returning immediately; a single background pump
// goroutine drains the queue and performs the actual (blocking) Publish RPC.
// When the queue is full the event is NOT dropped: it was already persisted to
// the durable outbox (persist-before-publish, see the outbox field), and resume
// / periodicDrain re-enqueue it once the pump makes progress — so a backpressure
// spike never stalls the local import pipeline nor loses an event.
// remotePublishClient is the narrow seam the pump uses to transmit a batch —
// satisfied by *RemoteRunner. An interface so the retry logic is unit-testable
// with a fake.
type remotePublishClient interface {
	Publish(ctx context.Context, events []proto.RemoteEvent) (proto.RemotePublishResult, error)
}

type remotePublishConnState interface {
	ConnState() string
}

type remotePublishDurableReceiptPolicy interface {
	DurableReceiptRequired() bool
}

type remotePublishStagedCheckpointPolicy interface {
	SupportsLargeRetainedCheckpoint(proto.RemoteEvent) bool
	PrepareLargeRetainedCheckpoint(context.Context, proto.RemoteEvent) (proto.RemoteEvent, error)
}

type remoteSyncObservationClient interface {
	ObserveSyncV1Async(metric string, value float64, unit, sourceIdentity string) bool
}

type remoteGenerationActivationGate interface {
	Check(scope string) error
}

type RemotePublishAdapter struct {
	client remotePublishClient
	logger interface {
		Info(msg string, args ...any)
		Warn(msg string, args ...any)
		Error(msg string, args ...any)
	}

	// liveQueue carries newly committed local events. queue carries resumed
	// durable backlog and retry work. The pump prefers liveQueue so near-real-
	// time sync is not trapped behind historical catch-up.
	liveQueue chan proto.RemoteEvent
	queue     chan proto.RemoteEvent

	// outbox is the durable, crash-safe sidecar (B1) that backs the in-memory
	// queue. Every event is persisted to the outbox BEFORE it is enqueued
	// (persist-before-publish) and removed ONLY on a terminal outcome (relay
	// ACCEPTED) or dead-lettered on a non-retryable rejection. A full queue,
	// exhausted retry budget, or a crash therefore never loses an event: the
	// durable file survives and startup-resume re-enqueues it. May be nil in
	// unit tests that exercise pure in-memory behaviour.
	outbox *Outbox
	// durableRequired is set for production construction. If outbox
	// initialization fails, remote publication stays paused rather than sending
	// work that has no crash-recoverable intent.
	durableRequired bool
	// watermarks records the last exact server-committed canonical delta per
	// stream/artifact/branch. In negotiated delta mode the terminal order is
	// committed receipt -> watermark fsync -> outbox removal; nil therefore
	// fails closed and keeps the outbox intent retryable.
	watermarks *DurablePublishWatermarkStore
	// recoverySource is installed after the orchestrator is constructed. It is
	// consulted only while negotiated delta mode requires durable receipts.
	recoveryMu            sync.RWMutex
	recoverySource        remoteOutboundRecoverySource
	recoveryWake          chan struct{}
	checkpointObligations *RemoteCheckpointObligationStore

	// retryBackoff is the delay before re-enqueuing a batch after a failed
	// publish RPC (prevents a hot loop while the plugin is down). Overridable
	// in tests.
	retryBackoff time.Duration

	// retries counts per-event publish attempts so a permanently-failing (or
	// permanently-disconnected) event can't cycle forever. Touched ONLY by the
	// single pump goroutine, so it needs no lock.
	retries map[string]int

	// notify, when set, surfaces publisher-level conditions on the daemon
	// event bus (SSE). notifyMu guards notify AND lastOversizedNotify:
	// SetEventNotifier is called after the constructor has already spawned the
	// pump/resume/periodicDrain goroutines that read notify in notifyOversized.
	notify              func(kind string, body map[string]any)
	notifyMu            sync.Mutex
	lastOversizedNotify map[string]time.Time

	// pendingRetained parks a retained-lane event pulled off a queue during
	// coalescing: retained events ship alone (see coalesceFrom), so the pump's
	// next iteration publishes it as its own solo batch before pulling anything
	// newer — preserving queue order. Touched ONLY by the single pump goroutine
	// (like retries), so it needs no lock.
	pendingRetained       *proto.RemoteEvent
	pendingRetainedSource remotePublishQueueSource

	// pendingRetry front-parks a transiently-failed LIVE-source batch, in its
	// original order, so the pump re-attempts it BEFORE pulling anything newer
	// FOR THE SAME ARTIFACTS off the queues — preserving design rule 1's
	// per-artifact FIFO on the live lane across RPC failures. (Requeuing at
	// the BACK of liveQueue let delta N ship after a delta N+1 that arrived
	// during the failure, forcing receivers into a needs-baseline full-state
	// recovery for a mere RPC blip.) The ordering is deliberately PER-ARTIFACT,
	// not global: a freshly-pulled live event for an artifact with no parked
	// events overtakes the parked batch immediately (blockedByPendingRetry), so
	// one artifact's repeatedly-failing publish cannot head-of-line block every
	// other artifact for the backoff x budget window; a same-artifact fresh
	// event is diverted to the BACK of pendingRetry instead (arrival order
	// preserved). pendingRetryNotBefore is the batch's backoff deadline —
	// requeue no longer sleeps the pump inline for live-source failures, it
	// parks with a deadline and nextEventWithSource replays the batch once the
	// deadline passes. Backlog-source retries keep the channel requeue: outbox
	// resume re-delivers in seq order anyway and receivers content-classify
	// retained/full-state backlog. Touched ONLY by the single pump goroutine,
	// so no lock.
	pendingRetry          []proto.RemoteEvent
	pendingRetrySource    remotePublishQueueSource
	pendingRetryNotBefore time.Time

	authorizerMu      sync.RWMutex
	projectAuthorized func(projectID string, generation uint64) bool
	epochCoordinator  *securityepoch.Coordinator
	activationGate    remoteGenerationActivationGate
}

func (a *RemotePublishAdapter) SetSecurityEpochCoordinator(coordinator *securityepoch.Coordinator) {
	a.authorizerMu.Lock()
	a.epochCoordinator = coordinator
	a.authorizerMu.Unlock()
}

// SetGenerationActivationGate wires the read-only pending-activation barrier.
// The gate is checked for every scope before any publish lease is acquired or
// remote RPC is attempted. A nil gate disables this optional barrier.
func (a *RemotePublishAdapter) SetGenerationActivationGate(gate remoteGenerationActivationGate) {
	a.authorizerMu.Lock()
	a.activationGate = gate
	a.authorizerMu.Unlock()
}

// remotePublishQueueDepth bounds the outbound buffer. ~1024 in-flight events
// is generous for the per-edit cadence of artifact commits; beyond it we shed
// load and rely on remote.fetch to catch up.
const remotePublishQueueDepth = 1024

// remotePublishMaxBatch bounds a single remote.publish RPC. The cloud plugin's
// direct publisher does real QoS-1 MQTT work before acknowledging each event,
// so reconnect catch-up must move in small chunks. Fresh live work gets its own
// priority queue and can run between these backlog chunks.
const remotePublishMaxBatch = 4

// remotePublishMaxBatchBytes bounds a single remote.publish RPC by raw event
// bytes. JSON-RPC base64 encoding adds overhead on the wire, so keep this well
// below the frame-reader cap. Events above remotePublishLargeEventBytes are sent
// alone; this preserves batching for normal small edits without turning a
// reconnect backlog of multi-MB conversations into a hundreds-of-MB stdio call.
const remotePublishMaxBatchBytes = 32 * 1024 * 1024
const remotePublishLargeEventBytes = remotePublishMaxBatchBytes / 2

// remotePublishMaxEventBytes is the largest single event the realtime MQTT path
// will attempt. Historical full-conversation snapshots can exceed broker/client
// practical limits and time out repeatedly, blocking fresh small edits behind
// retries. Oversized live entries remain durably pending and raise canonical
// checkpoint recovery instead of being dead-lettered. This is the lane=live
// cap; legacy laneless events (pre-lane outbox replays, non-conversation kinds)
// share it.
const remotePublishMaxEventBytes = remotePublishLargeEventBytes / remotePublishMaxBatch

// remotePublishMaxRetainedEventBytes is the per-event cap for the RETAINED
// lane (aligned-chains design rule 6). Retained baselines are recovery data,
// but attempting large retained MQTT publishes can wedge the client long enough
// to leave fresh small conversations stuck behind retryable failures. Until
// retained baselines are chunked, keep the retained lane on the same practical
// per-message budget as live sync. Above it the event is dead-lettered with
// the existing oversized notify; smaller conversations continue to drain.
const remotePublishMaxRetainedEventBytes = remotePublishMaxEventBytes

// oversizedNotifyMinInterval throttles per-artifact oversized notifications:
// every subsequent event of a too-big conversation is also too big, and one
// bus event per artifact per hour is signal, not spam.
const oversizedNotifyMinInterval = time.Hour

// publishBatchWindow is how long the pump waits to coalesce additional queued
// events into one remote.publish call after the first event arrives. Small
// enough to stay responsive, large enough to batch a burst (a multi-file edit).
const publishBatchWindow = 100 * time.Millisecond

// publishCallTimeout caps a single remote.publish RPC so a wedged plugin can't
// block the pump indefinitely. The cloud plugin publishes both live and
// retained MQTT messages per event, so this must be long enough for a slow-but-
// healthy QoS-1 broker round trip instead of turning it into an endless retry.
const publishCallTimeout = 6 * time.Second

// publishRetainedCallTimeout caps a remote.publish RPC whose batch carries a
// retained-lane event. Retained events ship alone (see coalesceFrom) and may
// be multi-MB, so the live-lane publishCallTimeout would abort a healthy but
// slow QoS-1 round trip and turn every larger baseline into an endless retry.
const publishRetainedCallTimeout = 60 * time.Second

// maxPublishRetries bounds how many times a single event is re-enqueued in the
// hot in-memory retry loop after a failed/rejected publish — so a permanently-
// failing (or permanently-disconnected) event can't cycle forever. Beyond this
// it is shed FROM THE IN-MEMORY QUEUE only; the durable outbox file persists and
// resume / periodicDrain recover it (B1), so this is not data loss.
const maxPublishRetries = 5

// publishRetryBackoff is the default delay before re-enqueuing after a failed
// publish RPC, so a disconnected plugin doesn't drive a hot retry loop.
const publishRetryBackoff = 2 * time.Second

// maxRetryAfterCap bounds how long we honor a plugin's per-event RetryAfter hint
// (a buggy/hostile plugin can't park the pump indefinitely).
const maxRetryAfterCap = 30 * time.Second

// outboxRescanInterval is how often periodicDrain re-checks the durable outbox
// for pending entries the in-memory queue could not hold (a backlog larger than
// remotePublishQueueDepth, or a stuck retryable left after the in-memory retry
// budget is exhausted). It acts ONLY when the queue has drained, so it never
// duplicates a still-queued event; a rare in-flight overlap is deduped by the
// relay (EventID/ParentHash).
//
// This is a stale-backlog recovery path, not the live-sync path. Fresh local
// events enter the priority live queue immediately; keep the durable scan slow
// enough that a large outage backlog cannot monopolize CPU while connected.
const outboxRescanInterval = 10 * time.Second

// Oldest-outbox age is a gauge, not a per-event stream. One sample per minute
// is responsive enough for rollback alarms while keeping local I/O and cloud
// transfer strictly bounded.
const oldestOutboxObservationInterval = time.Minute

// NewRemotePublishAdapter constructs the adapter and starts its background
// pump bound to ctx. The pump exits when ctx is cancelled. logger may be nil.
//
// outboxRoot is the directory backing the durable outbox (B1), a sibling of
// the conflicts sidecar under daemonStateDir (e.g. ~/.aplexica/outbox). The
// constructor calls outbox.Init() and kicks a startup-resume goroutine that
// re-enqueues any pending entries (left by a prior crash or a relay outage) in
// seq order. If outbox.Init fails the adapter still runs but without durability
// (logged): the in-memory queue degrades to the prior best-effort behaviour
// rather than blocking daemon startup.
func NewRemotePublishAdapter(ctx context.Context, runner *RemoteRunner, outboxRoot string, logger interface {
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}) *RemotePublishAdapter {
	a := &RemotePublishAdapter{
		client:       runner,
		logger:       logger,
		liveQueue:    make(chan proto.RemoteEvent, remotePublishQueueDepth),
		queue:        make(chan proto.RemoteEvent, remotePublishQueueDepth),
		retryBackoff: publishRetryBackoff,
		retries:      map[string]int{},
		recoveryWake: make(chan struct{}, 1),
	}
	if outboxRoot != "" {
		a.durableRequired = true
		ob := &Outbox{Root: outboxRoot, logger: logger}
		if err := ob.Init(); err != nil {
			if logger != nil {
				logger.Error("remote outbox init failed; durable retention disabled", "err", err)
			}
		} else {
			a.outbox = ob
			watermarks := &DurablePublishWatermarkStore{Root: filepath.Join(filepath.Dir(outboxRoot), "publish-watermarks")}
			if err := watermarks.Init(); err != nil {
				if logger != nil {
					logger.Error("remote publish watermark init failed; delta publication will remain retryable", "err", err)
				}
			} else {
				a.watermarks = watermarks
			}
			obligations := &RemoteCheckpointObligationStore{Root: filepath.Join(outboxRoot, "checkpoint-obligations")}
			if err := obligations.Init(); err != nil {
				if logger != nil {
					logger.Error("remote checkpoint obligation store init failed; canonical recovery will remain paused", "err", err)
				}
			} else {
				a.checkpointObligations = obligations
			}
			go ob.SweepDeadLetters()
		}
	}
	go a.pump(ctx)
	if a.outbox != nil {
		go a.resume(ctx)
		go a.periodicDrain(ctx)
		go a.observeOldestOutbox(ctx)
	}
	return a
}

func (a *RemotePublishAdapter) observeOldestOutbox(ctx context.Context) {
	ticker := time.NewTicker(oldestOutboxObservationInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			a.observeOldestOutboxOnce(now)
		}
	}
}

func (a *RemotePublishAdapter) observeOldestOutboxOnce(now time.Time) {
	observer, ok := a.client.(remoteSyncObservationClient)
	if !ok || a.outbox == nil || now.IsZero() {
		return
	}
	age, _, err := a.outbox.OldestPendingAge(now)
	if err != nil {
		if a.logger != nil {
			a.logger.Warn("remote outbox age observation unavailable", "err", err)
		}
		return
	}
	// A cadence bucket identifies this gauge sample. The private HMAC helper
	// binds the exact numeric value and prevents the bucket from being guessed
	// from SampleID by the remote service.
	sourceIdentity := "oldest-outbox:" + now.UTC().Truncate(oldestOutboxObservationInterval).Format(time.RFC3339)
	observer.ObserveSyncV1Async(
		proto.RemoteSyncMetricOldestOutboxAgeSeconds,
		age.Seconds(),
		proto.RemoteSyncObservationUnitSeconds,
		sourceIdentity,
	)
}

// OutboxEvidenceStatus is a bounded, content-free operator snapshot. It never
// returns event identities, sealed bytes, native paths, or error strings.
func (a *RemotePublishAdapter) OutboxEvidenceStatus(now time.Time) OutboxEvidenceStatus {
	status := OutboxEvidenceStatus{}
	if a == nil || a.outbox == nil || now.IsZero() {
		return status
	}
	pending, age, present, err := a.outbox.PendingSnapshot(now)
	if err != nil {
		return status
	}
	status.Available = true
	status.Pending = pending
	status.OldestPendingPresent = present
	if present && age > 0 {
		status.OldestPendingAgeSeconds = uint64(age / time.Second)
	}
	return status
}

// resume re-enqueues any pending durable outbox entries onto the pump queue in
// seq (== FIFO) order after a restart or relay outage. It runs in its own
// goroutine so a large backlog cannot block daemon startup; if the in-memory
// queue fills it stops and leaves the remaining files on disk for the pump to
// drain as it makes progress (their durable files persist regardless). The
// dead/ subdir is not resumed. Re-publishing an already-accepted-but-not-yet-
// deleted event is safe: the relay dedupes by EventID/ParentHash.
func (a *RemotePublishAdapter) resume(ctx context.Context) {
	if a.outbox == nil {
		return
	}
	if !a.remoteConnected() {
		return
	}
	entries, err := a.outbox.List()
	if err != nil {
		if a.logger != nil {
			a.logger.Error("remote outbox resume: list failed", "err", err)
		}
		return
	}
	if len(entries) == 0 {
		return
	}
	if a.logger != nil {
		a.logger.Info("remote outbox resume: re-enqueuing pending events", "count", len(entries))
	}
	for _, e := range orderResumeEntries(entries) {
		if a.durableDeltaMode() && a.outbox.mutations != nil {
			dirty, dirtyErr := a.outbox.mutations.IsDirty(e.Event.NamespaceID)
			if dirtyErr != nil {
				if a.logger != nil {
					a.logger.Warn("remote outbox resume: scope recovery state unavailable", "event_id", e.Event.EventID, "err", dirtyErr)
				}
				continue
			}
			if dirty {
				// Sequence order is no longer authoritative once an earlier append
				// missed the bounded outbox. The canonical worker owns this scope.
				continue
			}
		}
		select {
		case <-ctx.Done():
			return
		case a.queue <- e.Event:
		default:
			// Queue full: leave the rest on disk; the pump drains them as it
			// makes progress (the durable files persist).
			return
		}
	}
}

// orderResumeEntries preserves the outbox's FIFO shape for non-retained work
// while sorting retained-lane snapshot slots by size. Retained baselines are
// self-contained recovery states, so cross-artifact/cross-lane ordering is not
// load-bearing; live deltas remain FIFO. This keeps a few very large retained
// snapshots from starving tiny recent conversation baselines during startup
// resume after an outage.
func orderResumeEntries(entries []outboxEntry) []outboxEntry {
	if len(entries) < 2 {
		return entries
	}
	ordered := append([]outboxEntry(nil), entries...)
	retained := make([]outboxEntry, 0)
	for _, entry := range ordered {
		if entry.Event.Lane == syncd.LaneRetained {
			retained = append(retained, entry)
		}
	}
	if len(retained) < 2 {
		return ordered
	}
	sort.SliceStable(retained, func(i, j int) bool {
		a, b := retained[i], retained[j]
		aTier, bTier := retainedResumeTier(a.Event), retainedResumeTier(b.Event)
		if aTier != bTier {
			return aTier < bTier
		}
		aBytes, bBytes := remoteEventApproxBytes(a.Event), remoteEventApproxBytes(b.Event)
		if aBytes != bBytes {
			return aBytes < bBytes
		}
		return a.Seq < b.Seq
	})
	next := 0
	for i := range ordered {
		if ordered[i].Event.Lane == syncd.LaneRetained {
			ordered[i] = retained[next]
			next++
		}
	}
	return ordered
}

// retainedResumeSmallEventBytes is the tier-0 boundary for resume ordering:
// retained baselines at or under it ship before larger ones so a reconnect
// gets many small conversations current before grinding through giants.
const retainedResumeSmallEventBytes = 256 * 1024

func retainedResumeTier(e proto.RemoteEvent) int {
	size := remoteEventApproxBytes(e)
	switch {
	case size <= retainedResumeSmallEventBytes:
		return 0
	case size <= remotePublishMaxEventBytes:
		return 1
	default:
		return 2
	}
}

// drainOnce re-enqueues pending durable outbox entries IF the in-memory queue
// has fully drained. The queue==0 guard means it can never enqueue a duplicate
// of a still-queued event (an in-flight batch the pump already pulled is, at
// worst, re-published once and deduped by the relay). It is the single testable
// step behind periodicDrain.
func (a *RemotePublishAdapter) drainOnce(ctx context.Context) {
	if a.outbox != nil && a.remoteConnected() && a.queuesIdle() {
		a.resume(ctx)
	}
}

func (a *RemotePublishAdapter) queuesIdle() bool {
	return len(a.queue) == 0 && (a.liveQueue == nil || len(a.liveQueue) == 0)
}

func (a *RemotePublishAdapter) remoteConnected() bool {
	if a == nil || a.client == nil {
		return false
	}
	if stateful, ok := a.client.(remotePublishConnState); ok {
		return stateful.ConnState() == "connected"
	}
	return true
}

// durableDeltaMode is the cutover boundary for canonical-range ordering.
// Shadow, durable-read, legacy, and rollback retain the established MQTT
// outbox/dead-letter behavior; only delta-preferred/required make
// DurableReceiptRequired true on the runner.
func (a *RemotePublishAdapter) durableDeltaMode() bool {
	if a == nil || a.client == nil {
		return false
	}
	policy, ok := a.client.(remotePublishDurableReceiptPolicy)
	return ok && policy.DurableReceiptRequired()
}

// SupportsLargeRetainedCheckpoint implements syncd's optional capability
// probe without performing I/O. The source keeps its legacy 4 MiB refusal
// unless the authenticated runner and fresh server negotiation both authorize
// the exceptional staged path.
func (a *RemotePublishAdapter) SupportsLargeRetainedCheckpoint(event syncd.OutboundEvent) bool {
	if a == nil || a.client == nil {
		return false
	}
	policy, ok := a.client.(remotePublishStagedCheckpointPolicy)
	return ok && policy.SupportsLargeRetainedCheckpoint(toRemoteEvent(event))
}

// periodicDrain re-runs drainOnce on a slow ticker so an outbox backlog larger
// than the in-memory queue depth — or a stuck retryable left after the in-memory
// retry budget is exhausted — drains without waiting for a daemon restart. Exits
// on ctx cancellation.
func (a *RemotePublishAdapter) periodicDrain(ctx context.Context) {
	if a.outbox == nil {
		return
	}
	t := time.NewTicker(outboxRescanInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.fulfillCheckpointObligationsOnce(ctx)
			a.recoverDirtyOnce(ctx)
			a.drainOnce(ctx)
		case <-a.recoveryWake:
			a.fulfillCheckpointObligationsOnce(ctx)
			a.recoverDirtyOnce(ctx)
			a.drainOnce(ctx)
		}
	}
}

// SetRecoverySource installs the canonical-store scanner after daemon startup
// has constructed both the orchestrator and publisher. The wake is nonblocking
// and gives startup markers an immediate pass without delaying command setup.
func (a *RemotePublishAdapter) SetRecoverySource(source remoteOutboundRecoverySource) {
	if a == nil {
		return
	}
	a.recoveryMu.Lock()
	a.recoverySource = source
	a.recoveryMu.Unlock()
	a.wakeOutboundRecovery()
}

func (a *RemotePublishAdapter) wakeOutboundRecovery() {
	if a == nil || a.recoveryWake == nil {
		return
	}
	select {
	case a.recoveryWake <- struct{}{}:
	default:
	}
}

// SetEventNotifier wires a bus-publish callback for publisher-level
// conditions (currently transport ceilings and checkpoint recovery). Call
// before heavy use.
// Safe to call while the background goroutines are running: the assignment
// is synchronized with notifyOversized's reads via notifyMu.
func (a *RemotePublishAdapter) SetEventNotifier(fn func(kind string, body map[string]any)) {
	a.notifyMu.Lock()
	a.notify = fn
	a.notifyMu.Unlock()
}

// PublishOutbound satisfies syncd.RemoteEventPublisher. It translates the
// event, durably persists it to the outbox FIRST (persist-before-publish),
// then enqueues it onto the in-memory pump queue without blocking. A full
// queue NO LONGER drops the event: the durable file already exists, so the
// startup-resume / periodic re-scan path re-enqueues it once the pump makes
// progress. This closes the former data-loss hole at the full-queue branch.
func (a *RemotePublishAdapter) PublishOutbound(e syncd.OutboundEvent) {
	re := toRemoteEvent(e)
	if a.durableRequired && a.outbox == nil {
		if a.logger != nil {
			a.logger.Error("remote publication paused: durable outbox unavailable", "event_id", re.EventID, "artifact_id", re.ArtifactID)
		}
		return
	}
	if !a.isProjectAuthorized(re) {
		if a.logger != nil {
			a.logger.Warn("remote publish rejected for revoked project", "project_id", re.ProjectID, "event_id", re.EventID)
		}
		return
	}
	if stagedRemoteCheckpointCandidate(re) {
		policy, ok := a.client.(remotePublishStagedCheckpointPolicy)
		if ok && policy.SupportsLargeRetainedCheckpoint(re) {
			stageCtx, cancel := context.WithTimeout(context.Background(), 2*publishRetainedCallTimeout)
			prepared, err := policy.PrepareLargeRetainedCheckpoint(stageCtx, re)
			cancel()
			if err != nil {
				if a.logger != nil {
					a.logger.Warn("remote staged checkpoint persist failed; retained recovery remains pending",
						"event_id", re.EventID, "artifact_id", re.ArtifactID, "err", err)
				}
				a.notifyOversized(re)
				return
			}
			re = prepared
		} else if re.DaemonStagedPayload != nil {
			// A raw retained body follows the exact historical size classifier
			// below when staged transfer is unavailable. This preserves legacy and
			// self-hosted behavior byte-for-byte. A lightweight descriptor, on the
			// other hand, is already owned by a durable outbox entry and must stay
			// pending until a staged-capable signed plugin reconnects.
			a.notifyOversized(re)
			return
		}
	}
	if a.outbox != nil {
		if !a.durableDeltaMode() {
			// Compatibility/rollback path: preserve the historical persist-first
			// MQTT behavior. A marker created during delta mode must not prevent
			// rollback from restoring legacy publication.
			if err := a.outbox.Append(re); err != nil {
				if re.DaemonStagedPayload != nil {
					_ = a.outbox.ReconcileStagedPayloads()
				}
				if a.logger != nil {
					a.logger.Error("remote outbox append failed; will recover via fetch/resume",
						"event_id", e.EventID, "artifact_id", e.ArtifactID, "err", err)
				}
				// Preserve the historical operator signal even when the bounded
				// outbox rejects the event before the dead-letter classifier can
				// inspect it.
				if remoteEventOversize(re) {
					a.notifyOversized(re)
				}
				return
			}
		} else if persisted, recoveryPending, err := a.outbox.AppendForPublish(re); err != nil {
			if re.DaemonStagedPayload != nil {
				_ = a.outbox.ReconcileStagedPayloads()
			}
			// Durable write failed (disk error). The orchestrator event log is
			// still RPO-0, so the next fan-out / startup-resume retries. Do not
			// enqueue an un-persisted event (that would reintroduce the loss).
			if a.logger != nil {
				a.logger.Error("remote outbox append failed; will recover via fetch/resume",
					"event_id", e.EventID, "artifact_id", e.ArtifactID, "err", err)
			}
			// Begin has already made the marker dirty. Surface the transport
			// ceiling even when the exact payload is too large for the bounded
			// outbox file itself and therefore could not be cached.
			if remoteEventOversize(re) {
				a.notifyOversized(re)
			}
			a.wakeOutboundRecovery()
			return
		} else if recoveryPending {
			// A prior event in this scope is missing from the outbox. Keep this
			// newer intent durable but off the live-priority queue; the canonical
			// recovery pass enqueues the exact range in ancestry order.
			if a.logger != nil {
				a.logger.Info("remote publish held behind canonical range recovery", "event_id", e.EventID, "artifact_id", e.ArtifactID)
			}
			a.wakeOutboundRecovery()
			return
		} else {
			re = persisted
		}
	}
	if a.deadletterIfOversize(re, "outbound") {
		return
	}
	q := a.liveQueue
	if q == nil {
		q = a.queue
	}
	select {
	case q <- re:
	default:
		// Queue full: the durable file persists, so this is NOT data loss.
		// Resume / the pump picks it up. Log at Info (not a loss event).
		if a.logger != nil {
			a.logger.Info("remote publish queue full; event persisted to outbox (will drain)",
				"event_id", e.EventID, "artifact_id", e.ArtifactID)
		}
	}
}

// SetProjectAuthorizer installs the final-check gate used immediately before
// durable append and network publication. Nil disables project publication
// rather than authorizing it when a project-scoped event is encountered.
func (a *RemotePublishAdapter) SetProjectAuthorizer(check func(string, uint64) bool) {
	a.authorizerMu.Lock()
	a.projectAuthorized = check
	a.authorizerMu.Unlock()
}

func (a *RemotePublishAdapter) isProjectAuthorized(ev proto.RemoteEvent) bool {
	if ev.ProjectID == "" {
		return true
	}
	a.authorizerMu.RLock()
	check := a.projectAuthorized
	a.authorizerMu.RUnlock()
	return check != nil && check(ev.ProjectID, ev.ProjectAuthorizationGeneration)
}

func (a *RemotePublishAdapter) PurgeProject(projectID string) (int, error) {
	if a.outbox == nil {
		return 0, nil
	}
	return a.outbox.PurgeProject(projectID)
}

// PurgeSecurityScope is used only inside the exclusive device-transition
// cutover after its generation-bound rescan marker is durable.
func (a *RemotePublishAdapter) PurgeSecurityScope(scopeID string, next securityepoch.SecurityEpoch) (int, error) {
	if a == nil || a.outbox == nil {
		return 0, errors.New("remote publisher: durable outbox unavailable")
	}
	return a.outbox.PurgeSecurityScope(scopeID, next)
}

func (a *RemotePublishAdapter) RequireSecurityCutover(scopeID string, next securityepoch.SecurityEpoch) error {
	if a == nil || a.outbox == nil || a.outbox.mutations == nil {
		return errors.New("remote publisher: rescan coordinator unavailable")
	}
	return a.outbox.mutations.RequireSecurityCutover(scopeID, next)
}

// pump drains the queue and performs batched remote.publish calls. It exits on
// ctx cancellation. A failed/rejected publish is retried by re-enqueuing
// (bounded per event; see publish) rather than dropped, honoring the wire retry
// contract.
func (a *RemotePublishAdapter) pump(ctx context.Context) {
	for {
		// A cancelled ctx must exit even while pendingRetry is non-empty:
		// nextEventWithSource returns parked retries without consulting ctx,
		// so without this guard a cancelled shutdown could spin through the
		// remaining (bounded) retry budget instead of stopping.
		if ctx.Err() != nil {
			return
		}
		first, source, ok := a.nextEventWithSource(ctx)
		if !ok {
			return
		}
		batch := a.coalesceFrom(ctx, first, source)
		a.publish(ctx, batch, source)
	}
}

type remotePublishQueueSource int

const (
	remotePublishQueueBacklog remotePublishQueueSource = iota
	remotePublishQueueLive
)

func (a *RemotePublishAdapter) nextEvent(ctx context.Context) (proto.RemoteEvent, bool) {
	e, _, ok := a.nextEventWithSource(ctx)
	return e, ok
}

func (a *RemotePublishAdapter) nextEventWithSource(ctx context.Context) (proto.RemoteEvent, remotePublishQueueSource, bool) {
	for {
		// Front-parked retry work is strictly the OLDEST unpublished work for
		// its artifacts (see pendingRetry): once its backoff deadline passes
		// it goes out before the parked retained event and before anything in
		// the channels. While the deadline is pending, OTHER artifacts' events
		// overtake it below (per-artifact FIFO, not global).
		if len(a.pendingRetry) > 0 && !time.Now().Before(a.pendingRetryNotBefore) {
			return a.popPendingRetry(), a.pendingRetrySource, true
		}
		if a.pendingRetained != nil {
			e, source := *a.pendingRetained, a.pendingRetainedSource
			a.pendingRetained = nil
			return e, source, true
		}
		if a.liveQueue != nil {
			select {
			case e := <-a.liveQueue:
				if a.blockedByPendingRetry(e) {
					a.pendingRetry = append(a.pendingRetry, e)
					continue
				}
				return e, remotePublishQueueLive, true
			default:
			}
		}
		// Blocking wait. When parked retry work is waiting out its backoff,
		// also arm a wake-up at the deadline so the retry is not stranded
		// behind an idle queue.
		var retryTimer *time.Timer
		var retryC <-chan time.Time
		if len(a.pendingRetry) > 0 {
			retryTimer = time.NewTimer(time.Until(a.pendingRetryNotBefore))
			retryC = retryTimer.C
		}
		stopRetryTimer := func() {
			if retryTimer != nil {
				retryTimer.Stop()
			}
		}
		select {
		case <-ctx.Done():
			stopRetryTimer()
			return proto.RemoteEvent{}, remotePublishQueueBacklog, false
		case <-retryC:
			continue // deadline passed; the loop pops the parked head
		case e := <-a.liveQueue:
			stopRetryTimer()
			if a.blockedByPendingRetry(e) {
				a.pendingRetry = append(a.pendingRetry, e)
				continue
			}
			return e, remotePublishQueueLive, true
		case e := <-a.queue:
			stopRetryTimer()
			return e, remotePublishQueueBacklog, true
		}
	}
}

// popPendingRetry removes and returns the head of the front-parked retry
// queue. Pump-goroutine-only, like the field itself.
func (a *RemotePublishAdapter) popPendingRetry() proto.RemoteEvent {
	e := a.pendingRetry[0]
	a.pendingRetry = a.pendingRetry[1:]
	if len(a.pendingRetry) == 0 {
		a.pendingRetry = nil
	}
	return e
}

// blockedByPendingRetry reports whether a freshly-pulled live-queue event must
// wait behind parked retry work. Per-artifact FIFO (design rule 1) forbids a
// live/laneless event overtaking an OLDER parked live/laneless event of the
// SAME artifact; everything else overtakes freely — a failing artifact must
// not head-of-line block the rest of the store. Retained events are exempt on
// both sides of the comparison: they ship solo and receivers content-classify
// them (baseline adoption / convEqual / stale-skip), so cross-lane order is
// free — and shipping a retained recovery baseline AHEAD of its parked live
// twin only speeds up the receiver's re-align.
func (a *RemotePublishAdapter) blockedByPendingRetry(e proto.RemoteEvent) bool {
	if e.Lane == syncd.LaneRetained {
		return false
	}
	for _, p := range a.pendingRetry {
		if p.ArtifactID == e.ArtifactID && p.Lane != syncd.LaneRetained {
			return true
		}
	}
	return false
}

// coalesce gathers `first` plus any events that arrive within publishBatchWindow
// (up to remotePublishMaxBatch / remotePublishMaxBatchBytes) into one batch.
func (a *RemotePublishAdapter) coalesce(ctx context.Context, first proto.RemoteEvent) []proto.RemoteEvent {
	return a.coalesceFrom(ctx, first, remotePublishQueueBacklog)
}

// coalesceFrom gathers from the same priority lane as first. Live events never
// pull historical backlog into their RPC; backlog batches never absorb live
// events that arrive during the coalescing window. This keeps old retries from
// poisoning fresh realtime publishes.
//
// Transport-lane rule (aligned-chains design rule 6): a lane=retained event
// ALWAYS ships as a solo batch — a retained first returns immediately, and a
// retained event pulled mid-window ends the current batch and is parked in
// pendingRetained so the next pump iteration publishes it alone, before
// anything newer is pulled (queue order preserved).
func (a *RemotePublishAdapter) coalesceFrom(ctx context.Context, first proto.RemoteEvent, source remotePublishQueueSource) []proto.RemoteEvent {
	batch := []proto.RemoteEvent{first}
	batchBytes := remoteEventApproxBytes(first)
	if len(batch) >= remotePublishMaxBatch || batchBytes >= remotePublishLargeEventBytes || first.Lane == syncd.LaneRetained {
		return batch
	}
	// Drain the remainder of a front-parked retry batch FIRST — but only once
	// its backoff deadline has passed (when `first` came off a channel while
	// parked work was still waiting out its backoff, pulling that work into
	// this fresh batch would both defeat the backoff pacing and couple the
	// fresh events' fate to a known-failing batch). When the deadline HAS
	// passed, `first` is the parked head nextEventWithSource just popped, and
	// the remainder is strictly older than anything in the channels: replaying
	// it intact (in order) is what preserves per-artifact live-lane FIFO
	// across a failed RPC. A retained event parked mid-list (a failed solo
	// retained retry that got prepended behind newer failures) ends the drain:
	// it must ship as its own solo batch.
	if !time.Now().Before(a.pendingRetryNotBefore) {
		for len(a.pendingRetry) > 0 {
			e := a.pendingRetry[0]
			if e.Lane == syncd.LaneRetained {
				break
			}
			a.pendingRetry = a.pendingRetry[1:]
			if len(a.pendingRetry) == 0 {
				a.pendingRetry = nil
			}
			batch = append(batch, e)
			batchBytes += remoteEventApproxBytes(e)
			if len(batch) >= remotePublishMaxBatch || batchBytes >= remotePublishMaxBatchBytes {
				return batch
			}
		}
	}
	timer := time.NewTimer(publishBatchWindow)
	defer timer.Stop()
	for {
		var q <-chan proto.RemoteEvent
		if source == remotePublishQueueLive {
			q = a.liveQueue
		} else {
			q = a.queue
		}
		select {
		case <-ctx.Done():
			return batch
		case e := <-q:
			if e.Lane == syncd.LaneRetained {
				a.pendingRetained = &e
				a.pendingRetainedSource = source
				return batch
			}
			if source == remotePublishQueueLive && a.blockedByPendingRetry(e) {
				// A same-artifact live event pulled mid-window while parked
				// retry work remains (e.g. behind a parked retained event):
				// divert it BEHIND the parked work to keep per-artifact order.
				a.pendingRetry = append(a.pendingRetry, e)
				continue
			}
			batch = append(batch, e)
			batchBytes += remoteEventApproxBytes(e)
			if len(batch) >= remotePublishMaxBatch || batchBytes >= remotePublishMaxBatchBytes {
				return batch
			}
		case <-timer.C:
			return batch
		}
	}
}

func remoteEventApproxBytes(e proto.RemoteEvent) int {
	n := len(e.Bytes)
	n += len(e.NamespaceID) + len(e.BranchID) + len(e.ArtifactID) + len(e.EventID)
	n += len(e.ParentHash) + len(e.CheckpointAlignmentHash) + len(e.Kind) + len(e.Type) + len(e.Origin) + len(e.Lane)
	return n
}

// capFor returns the per-event byte cap for a transport lane: the retained
// lane carries full materialized baselines and gets the practical retained
// publish cap; live and legacy laneless events keep the realtime
// remotePublishMaxEventBytes.
func capFor(lane string) int {
	if lane == syncd.LaneRetained {
		return remotePublishMaxRetainedEventBytes
	}
	return remotePublishMaxEventBytes
}

func remoteEventOversize(e proto.RemoteEvent) bool {
	return remoteEventApproxBytes(e) > capFor(e.Lane)
}

// publishTimeoutFor picks the RPC budget for one batch: a batch carrying a
// retained-lane event (always solo — coalesceFrom guarantees it) gets the long
// publishRetainedCallTimeout transfer budget; everything else keeps the
// realtime publishCallTimeout.
func publishTimeoutFor(batch []proto.RemoteEvent) time.Duration {
	for _, e := range batch {
		if e.Lane == syncd.LaneRetained {
			return publishRetainedCallTimeout
		}
	}
	return publishCallTimeout
}

// publish performs the remote.publish RPC for a batch and HONORS the wire retry
// contract (proto.RemotePublishResult): a whole-batch RPC failure (e.g.
// ErrRemoteReconnecting) re-enqueues the batch after a backoff; a per-event
// outcome with Accepted=false && Retryable is re-enqueued (honoring RetryAfter).
// A delta-mode live terminal outcome is parked for checkpoint recovery; legacy
// and retained terminal work keeps its bounded quarantine behavior. Every hot
// retry is bounded per event (maxPublishRetries).
func (a *RemotePublishAdapter) publish(ctx context.Context, batch []proto.RemoteEvent, source remotePublishQueueSource) {
	if len(batch) == 0 {
		return
	}
	for i := range batch {
		if batch[i].BodyDigest == "" {
			batch[i].BodyDigest = sealedBodyDigest(batch[i].Bytes)
		}
	}
	batch = a.filterOversize(batch, "queued")
	batch = a.filterAuthorized(batch)
	if len(batch) == 0 {
		return
	}
	if !a.remoteConnected() {
		a.requeue(ctx, batch, source, a.retryBackoff)
		return
	}
	leases, err := a.acquireSecurityLeases(ctx, batch)
	if err != nil {
		if a.logger != nil {
			a.logger.Warn("remote publish blocked by security epoch", "err", err)
		}
		a.requeue(ctx, batch, source, a.retryBackoff)
		return
	}
	defer func() {
		for i := len(leases) - 1; i >= 0; i-- {
			_ = leases[i].Close()
		}
	}()
	// Bind the durability policy to this publish attempt before entering the
	// blocking RPC. A concurrent RemoteRunner teardown resets its negotiated
	// mode to legacy; sampling after Publish returned could therefore turn a
	// delta-mode live request into a legacy-policy response and incorrectly
	// retire the outbox intent on a broker-only Accepted outcome. Once required
	// for an attempt, a teardown or renegotiation may not weaken the live-lane
	// receipt contract. Retained checkpoint/suppression outcomes remain trusted
	// plugin decisions and laneless compatibility events keep legacy semantics.
	durableReceiptRequired := false
	if policy, ok := a.client.(remotePublishDurableReceiptPolicy); ok {
		durableReceiptRequired = policy.DurableReceiptRequired()
	}
	callCtx, cancel := context.WithTimeout(ctx, publishTimeoutFor(batch))
	res, err := a.client.Publish(callCtx, batch)
	cancel()
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		// Whole-batch failure: nothing landed. Re-enqueue after a backoff
		// rather than dropping (the previous behavior lost the events).
		if a.logger != nil {
			a.logger.Warn("remote publish failed; will retry", "count", len(batch), "err", err)
		}
		a.requeue(ctx, batch, source, a.retryBackoff)
		return
	}
	// Per-event outcomes. Index matches input position 1:1 (proto contract).
	var retry []proto.RemoteEvent
	var retryAfter time.Duration
	for i, o := range res.Outcomes {
		if i >= len(batch) {
			break // defensive: plugin returned more outcomes than events
		}
		ev := batch[i]
		requireDurableReceipt := durableReceiptRequired && ev.Lane == syncd.LaneLive
		_, requireCheckpointReceipt, checkpointLookupErr := a.checkpointObligationForEvent(ev)
		switch {
		case checkpointLookupErr != nil:
			// Obligation state is part of the durability boundary. A corrupt or
			// temporarily unreadable store may not downgrade this checkpoint to a
			// generic retained acknowledgement.
			retry = append(retry, ev)
			if retryAfter < a.retryBackoff {
				retryAfter = a.retryBackoff
			}
		case o.Accepted && requireCheckpointReceipt && validDurableCheckpointReceipt(ev, o):
			matched, persistErr := a.persistCheckpointReceipt(ev, o)
			if persistErr != nil || !matched {
				retry = append(retry, ev)
				if retryAfter < a.retryBackoff {
					retryAfter = a.retryBackoff
				}
				break
			}
			delete(a.retries, ev.EventID)
			a.outboxRemove(ev.EventID)
			a.wakeOutboundRecovery()
		case o.Accepted && requireCheckpointReceipt && (o.Durability != "" || o.CommitCursor != "" || o.CommitPosition != 0):
			// Receipt-shaped but invalid evidence is never a compatibility ACK.
			// Keep the exact checkpoint bytes and retry idempotently.
			retry = append(retry, ev)
			if retryAfter < a.retryBackoff {
				retryAfter = a.retryBackoff
			}
		case o.Accepted && requireCheckpointReceipt:
			// Suppressed/superseded retained decisions intentionally carry no
			// receipt. They may retire this exact outbox attempt, but they do not
			// clear the obligation; the worker re-probes the canonical head and
			// produces a receipt-bearing checkpoint if recovery is still needed.
			delete(a.retries, ev.EventID)
			a.outboxRemove(ev.EventID)
			a.wakeOutboundRecovery()
		case o.Accepted && requireDurableReceipt && !validDurableReceipt(ev, o):
			// A broker acknowledgement is not origin durability after retained
			// suppression. Keep the local outbox intent and retry until the plugin
			// returns a receipt bound to the exact sealed body.
			if !validRemoteEventRecoveryGeneration(ev) {
				if err := a.parkLiveForCheckpoint(ev, "publish-generation-authority-incomplete"); err != nil && a.logger != nil {
					a.logger.Warn("remote invalid generation checkpoint persist failed", "event_id", ev.EventID, "err", err)
				}
				delete(a.retries, ev.EventID)
				break
			}
			if a.logger != nil {
				a.logger.Warn("remote durable receipt invalid; will retry", "event_id", ev.EventID, "artifact_id", ev.ArtifactID)
			}
			retry = append(retry, ev)
			if retryAfter < a.retryBackoff {
				retryAfter = a.retryBackoff
			}
		case o.Accepted && requireDurableReceipt:
			// Persist the canonical recovery anchor before deleting the only
			// crash-recoverable publish intent. Any validation/fsync failure keeps
			// the outbox entry and retries the exact idempotent append.
			if err := a.commitDurableWatermark(ev, o); err != nil {
				if a.logger != nil {
					a.logger.Warn("remote durable watermark persist failed; will retry", "event_id", ev.EventID, "artifact_id", ev.ArtifactID, "err", err)
				}
				retry = append(retry, ev)
				if retryAfter < a.retryBackoff {
					retryAfter = a.retryBackoff
				}
				break
			}
			delete(a.retries, ev.EventID)
			a.outboxRemove(ev.EventID)
			a.wakeOutboundRecovery()
		case o.Accepted:
			// Terminal-accepted: the relay owns it now. Delete the durable
			// file (the ONLY delete path) and forget the in-memory retry count.
			delete(a.retries, ev.EventID)
			a.outboxRemove(ev.EventID)
		case o.Retryable:
			// Retryable: the durable file STAYS; we re-enqueue in-memory.
			if a.logger != nil {
				a.logger.Info("remote publish event retryable",
					"event_id", ev.EventID, "artifact_id", ev.ArtifactID, "err", o.Error)
			}
			retry = append(retry, ev)
			if o.RetryAfter > retryAfter {
				retryAfter = o.RetryAfter
			}
		default:
			delete(a.retries, ev.EventID)
			if requireDurableReceipt {
				// The server's terminal rejection does not prove that this
				// immutable canonical delta may be discarded. Preserve its exact
				// ciphertext and require an explicit checkpoint replacement.
				if err := a.parkLiveForCheckpoint(ev, "terminal-publish-rejection"); err != nil && a.logger != nil {
					a.logger.Warn("remote publish rejection checkpoint persist failed", "event_id", ev.EventID, "err", err)
				}
				if a.logger != nil {
					a.logger.Info("remote publish event rejected; exact live intent parked for checkpoint",
						"event_id", o.EventID, "err", o.Error)
				}
				a.wakeOutboundRecovery()
				break
			}
			if ev.Lane == syncd.LaneRetained && ev.DaemonStagedPayload != nil {
				// A staged file is the only exact authority for a randomized large
				// checkpoint seal. Never dead-letter/delete it on a terminal plugin
				// rejection; convert it into explicit checkpoint recovery instead.
				if err := a.parkRetainedForCheckpoint(ev, "terminal-staged-checkpoint-rejection"); err != nil && a.logger != nil {
					a.logger.Warn("remote staged checkpoint rejection recovery persist failed", "event_id", ev.EventID, "err", err)
				}
				break
			}
			// Legacy and retained publication has no canonical delta-receipt
			// contract, so its historical terminal quarantine behavior remains.
			a.outboxDeadletter(ev.EventID)
			if a.logger != nil {
				a.logger.Info("remote publish event rejected (non-retryable; dead-lettered)",
					"event_id", o.EventID, "err", o.Error)
			}
		}
	}
	if len(retry) > 0 {
		if retryAfter > maxRetryAfterCap {
			retryAfter = maxRetryAfterCap
		}
		a.requeue(ctx, retry, source, retryAfter)
	}
}

func validRemoteEventRecoveryGeneration(event proto.RemoteEvent) bool {
	return event.AccessGeneration > 0 && event.AccessSetHash != ([sha256.Size]byte{}) &&
		event.SecurityGeneration > 0 && event.SecurityBarrierID != ([sha256.Size]byte{}) &&
		validRecoveryKeyModeVersion(event.KeyMode, event.KeyVersion)
}

func validDurableReceipt(event proto.RemoteEvent, outcome proto.RemotePublishOutcome) bool {
	return outcome.EventID == event.EventID &&
		outcome.Durability == proto.RemoteDurabilityCommitted &&
		outcome.CommitCursor != "" &&
		outcome.CommitPosition > 0 &&
		outcome.StreamID != "" &&
		outcome.StreamEpoch != "" &&
		event.Lane == syncd.LaneLive &&
		validateWatermarkDigest(event.EventHash) &&
		validRemoteEventRecoveryGeneration(event) &&
		validateWatermarkDigest(outcome.EventIdentityDigest) &&
		validateWatermarkDigest(outcome.MetadataDigest) &&
		validateWatermarkDigest(event.BodyDigest) &&
		strings.EqualFold(outcome.BodyDigest, event.BodyDigest)
}

func (a *RemotePublishAdapter) commitDurableWatermark(event proto.RemoteEvent, outcome proto.RemotePublishOutcome) error {
	if a.watermarks == nil || !validDurableReceipt(event, outcome) {
		return ErrDurablePublishWatermarkInvalid
	}
	watermark := DurablePublishWatermark{
		Key: DurablePublishWatermarkKey{
			StreamID:    outcome.StreamID,
			StreamEpoch: outcome.StreamEpoch,
			ArtifactID:  event.ArtifactID,
			BranchID:    event.BranchID,
		},
		CanonicalEventID:     event.EventID,
		CanonicalEventHash:   event.EventHash,
		Position:             outcome.CommitPosition,
		RecipientFingerprint: hex.EncodeToString(event.AccessSetHash[:]),
		AccessGeneration:     event.AccessGeneration,
		SecurityGeneration:   event.SecurityGeneration,
		SecurityBarrier:      hex.EncodeToString(event.SecurityBarrierID[:]),
		KeyMode:              event.KeyMode,
		KeyVersion:           event.KeyVersion,
		BodyDigest:           event.BodyDigest,
		EventIdentityDigest:  outcome.EventIdentityDigest,
		MetadataDigest:       outcome.MetadataDigest,
	}
	_, err := a.watermarks.Advance(watermark)
	return err
}

func (a *RemotePublishAdapter) acquireSecurityLeases(ctx context.Context, batch []proto.RemoteEvent) ([]securityepoch.SecurityPublishLease, error) {
	a.authorizerMu.RLock()
	coordinator := a.epochCoordinator
	activationGate := a.activationGate
	a.authorizerMu.RUnlock()
	type scopeEpoch struct {
		scope string
		epoch securityepoch.SecurityEpoch
	}
	byScope := map[string]securityepoch.SecurityEpoch{}
	for _, event := range batch {
		scope := event.NamespaceID
		if scope == "" {
			scope = "account"
		}
		epoch := securityepoch.SecurityEpoch{CoordinatorGeneration: event.SecurityGeneration, AccessGeneration: event.AccessGeneration, AccessSetHash: event.AccessSetHash, BarrierID: event.SecurityBarrierID, KeyMode: event.KeyMode, KeyVersion: event.KeyVersion}
		if existing, ok := byScope[scope]; ok && existing != epoch {
			return nil, securityerr.ErrMetadataMismatch
		}
		byScope[scope] = epoch
	}
	ordered := make([]scopeEpoch, 0, len(byScope))
	for scope, epoch := range byScope {
		ordered = append(ordered, scopeEpoch{scope, epoch})
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].scope < ordered[j].scope })
	if activationGate != nil {
		for _, item := range ordered {
			if err := activationGate.Check(item.scope); err != nil {
				return nil, err
			}
		}
	}
	if coordinator == nil {
		return nil, nil
	}
	leases := make([]securityepoch.SecurityPublishLease, 0, len(ordered))
	for _, item := range ordered {
		lease, err := coordinator.AcquirePublish(ctx, item.scope, item.epoch)
		if err != nil {
			for i := len(leases) - 1; i >= 0; i-- {
				_ = leases[i].Close()
			}
			return nil, err
		}
		leases = append(leases, lease)
	}
	return leases, nil
}

func (a *RemotePublishAdapter) filterAuthorized(batch []proto.RemoteEvent) []proto.RemoteEvent {
	kept := make([]proto.RemoteEvent, 0, len(batch))
	for _, ev := range batch {
		if a.isProjectAuthorized(ev) {
			kept = append(kept, ev)
			continue
		}
		delete(a.retries, ev.EventID)
		a.outboxDeadletter(ev.EventID)
		if a.logger != nil {
			a.logger.Warn("remote queued event quarantined after project revocation", "project_id", ev.ProjectID, "event_id", ev.EventID)
		}
	}
	return kept
}

func (a *RemotePublishAdapter) filterOversize(batch []proto.RemoteEvent, source string) []proto.RemoteEvent {
	kept := make([]proto.RemoteEvent, 0, len(batch))
	for _, ev := range batch {
		if a.deadletterIfOversize(ev, source) {
			continue
		}
		kept = append(kept, ev)
	}
	return kept
}

func (a *RemotePublishAdapter) deadletterIfOversize(ev proto.RemoteEvent, source string) bool {
	if !remoteEventOversize(ev) {
		return false
	}
	// In delta-preferred/required, a live delta is immutable canonical history.
	// Once its ciphertext may have reached the relay, deleting or replacing it
	// would break exact retry. Park it and force a generation-bound checkpoint
	// obligation. Compatibility modes retain their established dead-letter path.
	if a.durableDeltaMode() && ev.Lane == syncd.LaneLive {
		if a.outbox != nil {
			if err := a.parkLiveForCheckpoint(ev, "live-event-exceeds-transport-ceiling"); err != nil && a.logger != nil {
				a.logger.Warn("remote oversized live event could not reserve checkpoint recovery",
					"event_id", ev.EventID, "artifact_id", ev.ArtifactID, "bytes", remoteEventApproxBytes(ev), "limit", capFor(ev.Lane), "err", err)
			}
		}
		delete(a.retries, ev.EventID)
		if a.logger != nil {
			a.logger.Warn("remote oversized live event parked for checkpoint recovery",
				"event_id", ev.EventID, "artifact_id", ev.ArtifactID, "bytes", remoteEventApproxBytes(ev), "limit", capFor(ev.Lane), "source", source)
		}
		a.notifyOversized(ev)
		return true
	}
	if a.outbox != nil {
		if err := a.outbox.Append(ev); err != nil {
			if a.logger != nil {
				a.logger.Warn("remote publish oversized event could not be persisted before dead-letter",
					"event_id", ev.EventID, "artifact_id", ev.ArtifactID, "bytes", remoteEventApproxBytes(ev), "limit", capFor(ev.Lane), "err", err)
			}
			a.notifyOversized(ev)
			return true
		}
		a.outboxDeadletter(ev.EventID)
	}
	delete(a.retries, ev.EventID)
	if a.logger != nil {
		a.logger.Warn("remote publish oversized event dead-lettered; live sync will use later smaller events",
			"event_id", ev.EventID, "artifact_id", ev.ArtifactID, "bytes", remoteEventApproxBytes(ev), "limit", capFor(ev.Lane), "source", source)
	}
	a.notifyOversized(ev)
	return true
}

// notifyOversized surfaces an oversized transport condition on the event bus
// (via the wired notifier), throttled per artifact: without this the condition
// is a Warn log only and a growing conversation silently stops syncing forever.
// The body carries ids/counts only — never artifact content (zero-knowledge).
func (a *RemotePublishAdapter) notifyOversized(ev proto.RemoteEvent) {
	a.notifyMu.Lock()
	notify := a.notify
	if notify == nil {
		a.notifyMu.Unlock()
		return
	}
	if a.lastOversizedNotify == nil {
		a.lastOversizedNotify = map[string]time.Time{}
	}
	last := a.lastOversizedNotify[ev.ArtifactID]
	now := time.Now()
	throttled := !last.IsZero() && now.Sub(last) < oversizedNotifyMinInterval
	if !throttled {
		a.lastOversizedNotify[ev.ArtifactID] = now
	}
	a.notifyMu.Unlock()
	if throttled {
		return
	}
	// retained_too_large tells the status surface that the RETAINED lane —
	// the recovery baseline peers depend on — is what is too large: peers
	// missing a live delta for this artifact have no recovery path until its
	// materialized state shrinks below the cap. (The orchestrator normally
	// refuses over-cap retained seals at the source with the same bus event;
	// this daemon-side path is the backstop for legacy outbox replays and the
	// small envelope-metadata window between the two measures.)
	notify("remote.outbound_oversized", map[string]any{
		"artifact_id":        ev.ArtifactID,
		"event_id":           ev.EventID,
		"bytes":              remoteEventApproxBytes(ev),
		"limit":              capFor(ev.Lane),
		"retained_too_large": ev.Lane == syncd.LaneRetained,
	})
}

// requeue re-enqueues each event for another in-memory publish attempt after
// delay, bounded by maxPublishRetries. With the durable outbox (B1) the
// in-memory retry budget is now only a hot-loop guard: when it is exhausted,
// or the in-memory queue is full, the event is NOT lost — its durable outbox
// file already exists and is left in place (PERSISTED) for startup-resume /
// the periodic re-scan to pick up. The durable file is the source of truth;
// only a terminal ACCEPTED removes delta-mode live work; legacy/retained
// terminal rejection may still quarantine it in dead/.
//
// Routing: a LIVE-source batch is front-parked in pendingRetry (prepended, so
// order is preserved even against a leftover or a diverted same-artifact
// event, both strictly newer) with pendingRetryNotBefore = now+delay — the
// pump does NOT sleep, so other artifacts' events keep flowing during the
// backoff and only same-artifact per-lane order is enforced (design rule 1 is
// per-artifact FIFO, not global; see pendingRetry). A backlog-source batch
// keeps the inline wait + back-of-queue channel requeue; the relay enforces
// per-artifact Sequence order regardless.
func (a *RemotePublishAdapter) requeue(ctx context.Context, batch []proto.RemoteEvent, source remotePublishQueueSource, delay time.Duration) {
	keep := make([]proto.RemoteEvent, 0, len(batch))
	for _, ev := range batch {
		a.retries[ev.EventID]++
		if a.retries[ev.EventID] > maxPublishRetries {
			delete(a.retries, ev.EventID)
			if a.logger != nil {
				a.logger.Info("remote publish in-memory retry budget exhausted; event persists in outbox (resume will retry)",
					"event_id", ev.EventID, "artifact_id", ev.ArtifactID)
			}
			continue
		}
		keep = append(keep, ev)
	}
	if len(keep) == 0 {
		return
	}
	if source == remotePublishQueueLive {
		a.pendingRetry = append(keep, a.pendingRetry...)
		a.pendingRetrySource = source
		a.pendingRetryNotBefore = time.Now().Add(delay)
		return
	}
	a.wait(ctx, delay)
	for _, ev := range keep {
		select {
		case <-ctx.Done():
			return
		case a.queue <- ev:
		default:
			// In-memory queue full on retry: NOT data loss — the durable
			// outbox file persists and resume re-enqueues it later.
			if a.logger != nil {
				a.logger.Info("remote publish queue full on retry; event persists in outbox (resume will retry)",
					"event_id", ev.EventID, "artifact_id", ev.ArtifactID)
			}
		}
	}
}

// outboxRemove deletes a terminal-accepted event's durable file (best effort;
// a failure is logged but not fatal — a stale file is re-published and the
// relay dedupes by EventID).
func (a *RemotePublishAdapter) outboxRemove(eventID string) {
	if a.outbox == nil {
		return
	}
	if err := a.outbox.Remove(eventID); err != nil && a.logger != nil {
		a.logger.Warn("remote outbox remove failed", "event_id", eventID, "err", err)
	}
}

// outboxDeadletter moves a terminal-nonretryable event's durable file into
// dead/ (best effort).
func (a *RemotePublishAdapter) outboxDeadletter(eventID string) {
	if a.outbox == nil {
		return
	}
	if err := a.outbox.Deadletter(eventID); err != nil && a.logger != nil {
		a.logger.Warn("remote outbox deadletter failed", "event_id", eventID, "err", err)
	}
}

// wait sleeps for d, returning early if ctx is cancelled. Non-positive d is a
// no-op.
func (a *RemotePublishAdapter) wait(ctx context.Context, d time.Duration) {
	if d <= 0 {
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// toRemoteEvent translates the orchestrator's OutboundEvent into the wire
// proto.RemoteEvent. OutboundEvent.Bytes is the opaque per-event payload (see
// the zero-knowledge flag in internal/sync/remote_sync.go) and is passed
// through verbatim as the RemoteEvent.Bytes raw message.
func toRemoteEvent(e syncd.OutboundEvent) proto.RemoteEvent {
	return proto.RemoteEvent{
		ProjectID:                      e.ProjectID,
		ProjectAuthorizationGeneration: e.ProjectAuthorizationGeneration,
		AccessGeneration:               e.AccessGeneration,
		AccessSetHash:                  e.AccessSetHash,
		SecurityBarrierID:              e.SecurityBarrierID,
		SecurityGeneration:             e.SecurityGeneration,
		KeyMode:                        e.KeyMode,
		KeyVersion:                     e.KeyVersion,
		CheckpointCoverage:             e.CheckpointCoverage,
		CheckpointGeneration:           e.CheckpointGeneration,
		NamespaceID:                    e.NamespaceID,
		BranchID:                       e.BranchID,
		ArtifactID:                     e.ArtifactID,
		EventID:                        e.EventID,
		ParentHash:                     e.ParentHash,
		CheckpointAlignmentHash:        e.CheckpointAlignmentHash,
		EventHash:                      e.EventHash,
		BodyDigest:                     sealedBodyDigest(e.Bytes),
		Kind:                           e.Kind,
		Type:                           e.Type,
		Timestamp:                      e.Timestamp,
		Bytes:                          e.Bytes,
		Sequence:                       e.Sequence,
		Origin:                         e.Origin,
		SourceAgent:                    e.SourceAgent,
		Lane:                           e.Lane,
		Clear:                          e.Clear,
	}
}

func sealedBodyDigest(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}
