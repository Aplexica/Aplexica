package daemon

import (
	"context"
	"crypto/sha256"
	"sync"
	"testing"

	"github.com/aplexica/aplexica/internal/plugin/proto"
	syncd "github.com/aplexica/aplexica/internal/sync"
	"github.com/stretchr/testify/require"
)

type durablePolicyPublishClient struct {
	*fakePublishClient
	required bool
}

func (c *durablePolicyPublishClient) DurableReceiptRequired() bool { return c.required }

type rollbackDuringPublishClient struct {
	mu             sync.RWMutex
	required       bool
	publishStarted chan struct{}
	allowResponse  chan struct{}
}

func (c *rollbackDuringPublishClient) DurableReceiptRequired() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.required
}

func (c *rollbackDuringPublishClient) Publish(ctx context.Context, events []proto.RemoteEvent) (proto.RemotePublishResult, error) {
	close(c.publishStarted)
	select {
	case <-ctx.Done():
		return proto.RemotePublishResult{}, ctx.Err()
	case <-c.allowResponse:
	}
	return acceptAll(events), nil
}

func (c *rollbackDuringPublishClient) rollbackToLegacy() {
	c.mu.Lock()
	c.required = false
	c.mu.Unlock()
}

func TestDeltaModeKeepsOutboxUntilExactDurableReceipt(t *testing.T) {
	body := []byte(`{"sealed":"opaque"}`)
	digest := sealedBodyDigest(body)
	accessSetHash := sha256.Sum256([]byte("recipients"))
	securityBarrier := sha256.Sum256([]byte("barrier"))
	committed := proto.RemotePublishOutcome{
		EventID:             "event-1",
		Accepted:            true,
		Durability:          proto.RemoteDurabilityCommitted,
		CommitCursor:        "cursor",
		CommitPosition:      7,
		StreamID:            "stream-1",
		StreamEpoch:         "epoch-1",
		BodyDigest:          digest,
		EventIdentityDigest: watermarkTestDigest("event-identity"),
		MetadataDigest:      watermarkTestDigest("metadata"),
	}
	tests := []struct {
		name    string
		outcome proto.RemotePublishOutcome
		remove  bool
	}{
		{name: "legacy accepted is insufficient", outcome: proto.RemotePublishOutcome{EventID: "event-1", Accepted: true}},
		{name: "wrong digest is insufficient", outcome: func() proto.RemotePublishOutcome {
			value := committed
			value.BodyDigest = sealedBodyDigest([]byte("different"))
			return value
		}()},
		{name: "missing cursor is insufficient", outcome: func() proto.RemotePublishOutcome { value := committed; value.CommitCursor = ""; return value }()},
		{name: "missing position is insufficient", outcome: func() proto.RemotePublishOutcome { value := committed; value.CommitPosition = 0; return value }()},
		{name: "missing stream is insufficient", outcome: func() proto.RemotePublishOutcome { value := committed; value.StreamID = ""; return value }()},
		{name: "missing identity binding is insufficient", outcome: func() proto.RemotePublishOutcome { value := committed; value.EventIdentityDigest = ""; return value }()},
		{name: "missing metadata binding is insufficient", outcome: func() proto.RemotePublishOutcome { value := committed; value.MetadataDigest = ""; return value }()},
		{name: "exact committed receipt retires intent", outcome: committed, remove: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client := &durablePolicyPublishClient{required: true, fakePublishClient: &fakePublishClient{fn: func(_ int, _ []proto.RemoteEvent) (proto.RemotePublishResult, error) {
				return proto.RemotePublishResult{Outcomes: []proto.RemotePublishOutcome{tc.outcome}}, nil
			}}}
			adapter := newTestPublishAdapterWithOutbox(t, client)
			event := proto.RemoteEvent{
				EventID: "event-1", ArtifactID: "artifact-1", BranchID: "main", EventHash: watermarkTestDigest("event-hash"),
				AccessGeneration: 1, AccessSetHash: accessSetHash, SecurityGeneration: 1, SecurityBarrierID: securityBarrier,
				KeyMode: "recipient-wrap-v2", Bytes: body, BodyDigest: digest, Lane: syncd.LaneLive,
			}
			require.NoError(t, adapter.outbox.Append(event))
			adapter.publish(context.Background(), []proto.RemoteEvent{event}, remotePublishQueueBacklog)
			if tc.remove {
				require.False(t, outboxHas(t, adapter, event.EventID))
				require.Empty(t, drainQueue(adapter.queue))
				stored, err := adapter.watermarks.Load(DurablePublishWatermarkKey{StreamID: committed.StreamID, StreamEpoch: committed.StreamEpoch, ArtifactID: event.ArtifactID, BranchID: event.BranchID})
				require.NoError(t, err)
				require.Equal(t, committed.CommitPosition, stored.Position)
				require.Equal(t, event.EventID, stored.CanonicalEventID)
			} else {
				require.True(t, outboxHas(t, adapter, event.EventID))
				require.Len(t, drainQueue(adapter.queue), 1)
			}
		})
	}
}

func TestDeltaModeTerminalRejectionParksLiveIntentForCheckpoint(t *testing.T) {
	client := &durablePolicyPublishClient{required: true, fakePublishClient: &fakePublishClient{fn: func(_ int, _ []proto.RemoteEvent) (proto.RemotePublishResult, error) {
		return proto.RemotePublishResult{Outcomes: []proto.RemotePublishOutcome{{
			EventID: "event-terminal", Retryable: false, Error: "server rejected delta",
		}}}, nil
	}}}
	adapter := newTestPublishAdapterWithOutbox(t, client)
	accessHash := sha256.Sum256([]byte("access"))
	barrier := sha256.Sum256([]byte("barrier"))
	body := []byte(`{"sealed":"exact"}`)
	event := proto.RemoteEvent{
		EventID: "event-terminal", ArtifactID: "artifact-terminal", BranchID: "main",
		EventHash: watermarkTestDigest("event-terminal"), BodyDigest: sealedBodyDigest(body), Bytes: body,
		AccessGeneration: 1, AccessSetHash: accessHash, SecurityGeneration: 2, SecurityBarrierID: barrier,
		KeyMode: "recipient-wrap-v2", Kind: "memory", Type: "update", Origin: "device-a", Lane: syncd.LaneLive,
	}
	require.NoError(t, adapter.outbox.Append(event))
	adapter.publish(context.Background(), []proto.RemoteEvent{event}, remotePublishQueueBacklog)

	require.True(t, outboxHas(t, adapter, event.EventID), "terminal server rejection must preserve exact live ciphertext")
	require.False(t, outboxDeadHas(t, adapter, event.EventID), "durable live deltas must never be dead-lettered")
	obligations, err := adapter.checkpointObligations.List()
	require.NoError(t, err)
	require.Len(t, obligations, 1)
	require.Equal(t, "terminal-publish-rejection", obligations[0].Reason)
	markers, err := adapter.outbox.mutations.ListDirty()
	require.NoError(t, err)
	require.Len(t, markers, 1)
	require.NotZero(t, markers[0].Marker.ReasonFlags&rescanReasonCheckpoint)
}

func TestDeltaModeIncompleteGenerationCannotRetireLiveIntent(t *testing.T) {
	body := []byte(`{"sealed":"exact"}`)
	digest := sealedBodyDigest(body)
	outcome := proto.RemotePublishOutcome{
		EventID: "event-incomplete", Accepted: true, Durability: proto.RemoteDurabilityCommitted,
		CommitCursor: "cursor", CommitPosition: 1, StreamID: "stream-1", StreamEpoch: "epoch-1",
		BodyDigest: digest, EventIdentityDigest: watermarkTestDigest("identity"), MetadataDigest: watermarkTestDigest("metadata"),
	}
	client := &durablePolicyPublishClient{required: true, fakePublishClient: &fakePublishClient{fn: func(_ int, _ []proto.RemoteEvent) (proto.RemotePublishResult, error) {
		return proto.RemotePublishResult{Outcomes: []proto.RemotePublishOutcome{outcome}}, nil
	}}}
	adapter := newTestPublishAdapterWithOutbox(t, client)
	event := proto.RemoteEvent{
		EventID: "event-incomplete", ArtifactID: "artifact-incomplete", BranchID: "main",
		EventHash: watermarkTestDigest("event-incomplete"), BodyDigest: digest, Bytes: body,
		AccessGeneration: 1, AccessSetHash: sha256.Sum256([]byte("access")),
		// Security generation/barrier/key mode deliberately absent.
		Kind: "memory", Type: "update", Origin: "device-a", Lane: syncd.LaneLive,
	}
	require.NoError(t, adapter.outbox.Append(event))
	adapter.publish(context.Background(), []proto.RemoteEvent{event}, remotePublishQueueBacklog)
	require.True(t, outboxHas(t, adapter, event.EventID))
	require.False(t, outboxDeadHas(t, adapter, event.EventID))
	obligations, err := adapter.checkpointObligations.List()
	require.NoError(t, err)
	require.Len(t, obligations, 1)
	require.Equal(t, "publish-generation-authority-incomplete", obligations[0].Reason)
}

func TestDeltaModeKeepsOutboxWhenWatermarkStoreUnavailable(t *testing.T) {
	body := []byte(`{"sealed":"opaque"}`)
	digest := sealedBodyDigest(body)
	event := proto.RemoteEvent{
		EventID: "event-1", ArtifactID: "artifact-1", BranchID: "main",
		EventHash: watermarkTestDigest("event-hash"), AccessGeneration: 1, AccessSetHash: sha256.Sum256([]byte("recipients")),
		SecurityGeneration: 1, SecurityBarrierID: sha256.Sum256([]byte("barrier")), KeyMode: "recipient-wrap-v2",
		Bytes: body, BodyDigest: digest, Lane: syncd.LaneLive,
	}
	outcome := proto.RemotePublishOutcome{
		EventID: event.EventID, Accepted: true, Durability: proto.RemoteDurabilityCommitted,
		CommitCursor: "cursor", CommitPosition: 7, StreamID: "stream-1", StreamEpoch: "epoch-1",
		BodyDigest: digest, EventIdentityDigest: watermarkTestDigest("identity"), MetadataDigest: watermarkTestDigest("metadata"),
	}
	client := &durablePolicyPublishClient{required: true, fakePublishClient: &fakePublishClient{fn: func(_ int, _ []proto.RemoteEvent) (proto.RemotePublishResult, error) {
		return proto.RemotePublishResult{Outcomes: []proto.RemotePublishOutcome{outcome}}, nil
	}}}
	adapter := newTestPublishAdapterWithOutbox(t, client)
	adapter.watermarks = nil
	require.NoError(t, adapter.outbox.Append(event))

	adapter.publish(context.Background(), []proto.RemoteEvent{event}, remotePublishQueueBacklog)

	require.True(t, outboxHas(t, adapter, event.EventID), "watermark failure must not remove publish intent")
	require.Len(t, drainQueue(adapter.queue), 1, "watermark failure must retry exact append")
}

func TestLegacyModeStillUsesExistingAcceptedSemantics(t *testing.T) {
	client := &durablePolicyPublishClient{required: false, fakePublishClient: &fakePublishClient{fn: func(_ int, events []proto.RemoteEvent) (proto.RemotePublishResult, error) {
		return acceptAll(events), nil
	}}}
	adapter := newTestPublishAdapterWithOutbox(t, client)
	event := proto.RemoteEvent{EventID: "legacy-event", Bytes: []byte(`"opaque"`)}
	require.NoError(t, adapter.outbox.Append(event))
	adapter.publish(context.Background(), []proto.RemoteEvent{event}, remotePublishQueueBacklog)
	require.False(t, outboxHas(t, adapter, event.EventID))
}

func TestDeltaModeTrustedAcceptanceRetiresNonLiveCompatibilityWork(t *testing.T) {
	tests := []struct {
		name  string
		event proto.RemoteEvent
	}{
		{
			name: "retained checkpoint",
			event: proto.RemoteEvent{
				EventID: "retained-checkpoint", ArtifactID: "artifact-checkpoint",
				Lane: syncd.LaneRetained, Bytes: []byte(`{"sealed":"checkpoint"}`),
			},
		},
		{
			name: "retained suppressed twin",
			event: proto.RemoteEvent{
				EventID: "retained-suppressed", ArtifactID: "artifact-suppressed",
				Lane: syncd.LaneRetained, Bytes: []byte(`{"sealed":"suppressed-twin"}`),
			},
		},
		{
			name: "retained clear",
			event: proto.RemoteEvent{
				EventID: "retained-clear", ArtifactID: "artifact-clear",
				Lane: syncd.LaneRetained, Clear: true,
			},
		},
		{
			name: "laneless compatibility event",
			event: proto.RemoteEvent{
				EventID: "legacy-laneless", ArtifactID: "artifact-legacy",
				Bytes: []byte(`{"sealed":"legacy"}`),
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client := &durablePolicyPublishClient{required: true, fakePublishClient: &fakePublishClient{fn: func(_ int, _ []proto.RemoteEvent) (proto.RemotePublishResult, error) {
				return proto.RemotePublishResult{Outcomes: []proto.RemotePublishOutcome{{
					EventID: tc.event.EventID, Accepted: true,
				}}}, nil
			}}}
			adapter := newTestPublishAdapterWithOutbox(t, client)
			require.NoError(t, adapter.outbox.Append(tc.event))

			adapter.publish(context.Background(), []proto.RemoteEvent{tc.event}, remotePublishQueueBacklog)

			require.False(t, outboxHas(t, adapter, tc.event.EventID),
				"trusted plugin acceptance must retire non-live compatibility work without a live-delta receipt")
			require.Empty(t, drainQueue(adapter.queue))
		})
	}
}

func TestDeltaModeRetainedClearStillRequiresPluginAcceptance(t *testing.T) {
	event := proto.RemoteEvent{
		EventID: "retained-clear", ArtifactID: "artifact-clear",
		Lane: syncd.LaneRetained, Clear: true,
	}
	client := &durablePolicyPublishClient{required: true, fakePublishClient: &fakePublishClient{fn: func(_ int, _ []proto.RemoteEvent) (proto.RemotePublishResult, error) {
		return proto.RemotePublishResult{Outcomes: []proto.RemotePublishOutcome{{
			EventID:   event.EventID,
			Accepted:  false,
			Retryable: true,
			// Receipt-shaped fields must not bypass Accepted=false.
			Durability:     proto.RemoteDurabilityCommitted,
			CommitCursor:   "cursor",
			CommitPosition: 9,
			StreamID:       "stream-1",
			StreamEpoch:    "epoch-1",
		}}}, nil
	}}}
	adapter := newTestPublishAdapterWithOutbox(t, client)
	require.NoError(t, adapter.outbox.Append(event))

	adapter.publish(context.Background(), []proto.RemoteEvent{event}, remotePublishQueueBacklog)

	require.True(t, outboxHas(t, adapter, event.EventID),
		"a retained clear must remain durable until the plugin accepts it")
	requeued := drainQueue(adapter.queue)
	require.Len(t, requeued, 1)
	require.Equal(t, event.EventID, requeued[0].EventID)
}

func TestDeltaPublishBindsReceiptPolicyBeforeConcurrentTeardown(t *testing.T) {
	client := &rollbackDuringPublishClient{
		required:       true,
		publishStarted: make(chan struct{}),
		allowResponse:  make(chan struct{}),
	}
	adapter := newTestPublishAdapterWithOutbox(t, client)
	event := proto.RemoteEvent{
		EventID: "event-race", ArtifactID: "artifact-race", BranchID: "main", EventHash: watermarkTestDigest("event-race"),
		AccessGeneration: 1, AccessSetHash: sha256.Sum256([]byte("recipients")),
		SecurityGeneration: 1, SecurityBarrierID: sha256.Sum256([]byte("barrier")), KeyMode: "recipient-wrap-v2",
		Bytes: []byte(`{"sealed":"opaque"}`), Lane: syncd.LaneLive,
	}
	require.NoError(t, adapter.outbox.Append(event))

	done := make(chan struct{})
	go func() {
		adapter.publish(context.Background(), []proto.RemoteEvent{event}, remotePublishQueueBacklog)
		close(done)
	}()

	<-client.publishStarted
	client.rollbackToLegacy() // Simulates RemoteRunner teardown while the RPC is in flight.
	close(client.allowResponse)
	<-done

	require.True(t, outboxHas(t, adapter, event.EventID), "an in-flight delta attempt must not be downgraded by teardown")
	require.Len(t, drainQueue(adapter.queue), 1, "broker-only acceptance must retry under the policy bound before the RPC")
}

func TestToRemoteEventCarriesCanonicalAndSealedDigests(t *testing.T) {
	sealed := []byte(`{"opaque":true}`)
	remote := toRemoteEvent(syncd.OutboundEvent{EventID: "event-1", EventHash: "canonical-hash", Bytes: sealed})
	require.Equal(t, "canonical-hash", remote.EventHash)
	require.Equal(t, sealedBodyDigest(sealed), remote.BodyDigest)
}
