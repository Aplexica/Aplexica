package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/plugin/proto"
	syncd "github.com/aplexica/aplexica/internal/sync"
	"github.com/stretchr/testify/require"
)

type checkpointMaterializerSource struct {
	*fakeOutboundRecoverySource
	materialize func(syncd.RemoteCheckpointMaterializeRequest) (syncd.RemoteCheckpointMaterialization, error)
}

func (s *checkpointMaterializerSource) MaterializeRemoteCheckpoint(_ context.Context, request syncd.RemoteCheckpointMaterializeRequest) (syncd.RemoteCheckpointMaterialization, error) {
	return s.materialize(request)
}

func checkpointGenerationFixture() syncd.RemoteRecoveryGeneration {
	return syncd.RemoteRecoveryGeneration{
		AccessGeneration: 1, AccessSetHash: sha256.Sum256([]byte("access-set")),
		SecurityGeneration: 1, SecurityBarrierID: sha256.Sum256([]byte("security-barrier")),
		KeyMode: "recipient-wrap-v2",
	}
}

func checkpointLiveFixture() proto.RemoteEvent {
	body := []byte(`{"sealed":"live"}`)
	generation := checkpointGenerationFixture()
	return proto.RemoteEvent{
		AccessGeneration: generation.AccessGeneration, AccessSetHash: generation.AccessSetHash,
		SecurityGeneration: generation.SecurityGeneration, SecurityBarrierID: generation.SecurityBarrierID,
		KeyMode: generation.KeyMode, KeyVersion: generation.KeyVersion,
		BranchID: "main", ArtifactID: "artifact-1", EventID: "event-live",
		EventHash: watermarkTestDigest("live-head"), BodyDigest: sealedBodyDigest(body),
		Kind: "conversation", Type: "update", Timestamp: time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC),
		Bytes: body, Sequence: 9, Origin: "device-a", SourceAgent: "codex", Lane: syncd.LaneLive,
	}
}

func checkpointMaterializationFixture(request syncd.RemoteCheckpointMaterializeRequest, headID, headHash, body string, sequence uint64) syncd.RemoteCheckpointMaterialization {
	generation := checkpointGenerationFixture()
	generationEvent := proto.RemoteEvent{
		AccessGeneration: generation.AccessGeneration, AccessSetHash: generation.AccessSetHash,
		SecurityGeneration: generation.SecurityGeneration, SecurityBarrierID: generation.SecurityBarrierID,
		KeyMode: generation.KeyMode, KeyVersion: generation.KeyVersion,
	}
	return syncd.RemoteCheckpointMaterialization{
		HeadEventID: headID, HeadHash: headHash, Generation: generation,
		Event: syncd.OutboundEvent{
			AccessGeneration: generation.AccessGeneration, AccessSetHash: generation.AccessSetHash,
			SecurityGeneration: generation.SecurityGeneration, SecurityBarrierID: generation.SecurityBarrierID,
			KeyMode: generation.KeyMode, KeyVersion: generation.KeyVersion,
			CheckpointCoverage: request.Coverage, CheckpointGeneration: checkpointGenerationForEvent(generationEvent),
			BranchID: "main", ArtifactID: request.ArtifactID, EventID: headID + "-retained",
			ParentHash: watermarkTestDigest("parent"), CheckpointAlignmentHash: headHash,
			EventHash: watermarkTestDigest("materialized-" + headID), Kind: "conversation", Type: "update",
			Timestamp: time.Date(2026, 7, 19, 12, 1, 0, 0, time.UTC), Bytes: []byte(body),
			Sequence: sequence, Origin: "device-a", SourceAgent: "codex", Lane: syncd.LaneRetained,
		},
	}
}

func installCheckpointCoverage(t *testing.T, adapter *RemotePublishAdapter, live proto.RemoteEvent, position uint64) {
	t.Helper()
	watermark := watermarkFixture(position)
	watermark.Key.ArtifactID = live.ArtifactID
	watermark.Key.BranchID = live.BranchID
	watermark.CanonicalEventID = "previous-event"
	watermark.CanonicalEventHash = watermarkTestDigest("previous-head")
	watermark.RecipientFingerprint = hex.EncodeToString(live.AccessSetHash[:])
	watermark.AccessGeneration = live.AccessGeneration
	watermark.SecurityGeneration = live.SecurityGeneration
	watermark.SecurityBarrier = hex.EncodeToString(live.SecurityBarrierID[:])
	watermark.KeyMode = live.KeyMode
	watermark.KeyVersion = live.KeyVersion
	_, err := adapter.watermarks.Advance(watermark)
	require.NoError(t, err)
}

func reopenCheckpointAdapter(t *testing.T, prior *RemotePublishAdapter, client remotePublishClient) *RemotePublishAdapter {
	t.Helper()
	outbox := &Outbox{Root: prior.outbox.Root}
	require.NoError(t, outbox.Init())
	watermarks := &DurablePublishWatermarkStore{Root: prior.watermarks.Root}
	require.NoError(t, watermarks.Init())
	obligations := &RemoteCheckpointObligationStore{Root: prior.checkpointObligations.Root}
	require.NoError(t, obligations.Init())
	return &RemotePublishAdapter{
		client: client, liveQueue: make(chan proto.RemoteEvent, remotePublishQueueDepth), queue: make(chan proto.RemoteEvent, remotePublishQueueDepth),
		retryBackoff: 0, retries: map[string]int{}, outbox: outbox, watermarks: watermarks,
		checkpointObligations: obligations, recoveryWake: make(chan struct{}, 1),
	}
}

func committedCheckpointOutcome(event proto.RemoteEvent, position uint64) proto.RemotePublishOutcome {
	return proto.RemotePublishOutcome{
		EventID: event.EventID, Accepted: true, Durability: proto.RemoteDurabilityCommitted,
		CommitCursor: "checkpoint-cursor", CommitPosition: position, StreamID: "stream-1", StreamEpoch: "epoch-1",
		BodyDigest: event.BodyDigest, EventIdentityDigest: watermarkTestDigest("checkpoint-identity"),
		MetadataDigest: watermarkTestDigest("checkpoint-metadata"),
	}
}

func TestExplicitCheckpointRequestMaterializesInEveryNonLegacyMode(t *testing.T) {
	modes := []string{
		proto.RemoteSyncModeShadow,
		proto.RemoteSyncModeDurableRead,
		proto.RemoteSyncModeDeltaPreferred,
		proto.RemoteSyncModeDeltaRequired,
	}
	for _, mode := range modes {
		t.Run(mode, func(t *testing.T) {
			adapter, client := checkpointRequestAdapter(t)
			client.negotiated.Mode = mode
			notification, _ := checkpointNeededFixture()
			require.NoError(t, adapter.HandleCheckpointNeededV1(notification))

			calls := 0
			source := &checkpointMaterializerSource{
				fakeOutboundRecoverySource: &fakeOutboundRecoverySource{fn: func(syncd.RemoteRecoveryRequest, func(syncd.OutboundEvent) error) (syncd.RemoteRecoveryResult, error) {
					return syncd.RemoteRecoveryResult{}, nil
				}},
				materialize: func(request syncd.RemoteCheckpointMaterializeRequest) (syncd.RemoteCheckpointMaterialization, error) {
					calls++
					require.Equal(t, notification.CheckpointCoverage, request.Coverage)
					require.Equal(t, notification.CheckpointAlignmentHash, request.ExpectedAlignmentHash)
					require.Equal(t, checkpointGenerationFixture(), request.Generation)
					return checkpointMaterializationFixture(request, "requested-head-event", notification.CheckpointAlignmentHash, `{"sealed":"requested-checkpoint"}`, 9), nil
				},
			}
			adapter.SetRecoverySource(source)
			adapter.fulfillCheckpointObligationsOnce(context.Background())
			require.Equal(t, 1, calls)
			select {
			case checkpoint := <-adapter.queue:
				require.Equal(t, notification.CheckpointCoverage, checkpoint.CheckpointCoverage)
				require.Equal(t, notification.CheckpointAlignmentHash, checkpoint.CheckpointAlignmentHash)
				require.Equal(t, notification.CheckpointGeneration, checkpoint.CheckpointGeneration)
			case <-time.After(time.Second):
				t.Fatal("request-bound checkpoint was not enqueued")
			}
		})
	}
}

func TestExplicitCheckpointRequestWaitsWhenCanonicalHeadAdvanced(t *testing.T) {
	adapter, _ := checkpointRequestAdapter(t)
	notification, _ := checkpointNeededFixture()
	require.NoError(t, adapter.HandleCheckpointNeededV1(notification))

	source := &checkpointMaterializerSource{
		fakeOutboundRecoverySource: &fakeOutboundRecoverySource{fn: func(syncd.RemoteRecoveryRequest, func(syncd.OutboundEvent) error) (syncd.RemoteRecoveryResult, error) {
			return syncd.RemoteRecoveryResult{}, nil
		}},
		materialize: func(request syncd.RemoteCheckpointMaterializeRequest) (syncd.RemoteCheckpointMaterialization, error) {
			require.Equal(t, notification.CheckpointAlignmentHash, request.ExpectedAlignmentHash)
			return checkpointMaterializationFixture(request, "advanced-head-event", watermarkTestDigest("advanced-head"), `{"sealed":"advanced"}`, 10), nil
		},
	}
	adapter.SetRecoverySource(source)
	adapter.fulfillCheckpointObligationsOnce(context.Background())
	select {
	case checkpoint := <-adapter.queue:
		t.Fatalf("advanced head checkpoint was enqueued: %+v", checkpoint)
	default:
	}
	values, err := adapter.checkpointObligations.List()
	require.NoError(t, err)
	require.Len(t, values, 1)
	require.Empty(t, values[0].CheckpointState)
	require.Equal(t, notification.CheckpointAlignmentHash, values[0].RequestAlignmentHash)
}

func TestCheckpointRequestSupersessionCannotRaceStalePreparationOrReplaceProgress(t *testing.T) {
	adapter, _ := checkpointRequestAdapter(t)
	original, _ := checkpointNeededFixture()
	require.NoError(t, adapter.HandleCheckpointNeededV1(original))
	values, err := adapter.checkpointObligations.List()
	require.NoError(t, err)
	require.Len(t, values, 1)
	staleAuthority := values[0]
	marker, exists, err := adapter.outbox.mutations.Snapshot("account")
	require.NoError(t, err)
	require.True(t, exists)

	newer := original
	newer.RequestID = "request-newer"
	newer.CheckpointCoverage++
	newer.CheckpointAlignmentHash = watermarkTestDigest("newer-head")
	require.NoError(t, adapter.HandleCheckpointNeededV1(newer))
	staleRequest := syncd.RemoteCheckpointMaterializeRequest{
		ScopeID: "account", ArtifactID: original.ArtifactID, BranchID: original.BranchID, Kind: original.Kind,
		Coverage: original.CheckpointCoverage, ExpectedAlignmentHash: original.CheckpointAlignmentHash, Generation: checkpointGenerationFixture(),
	}
	staleMaterialization := checkpointMaterializationFixture(staleRequest, "stale-head-event", original.CheckpointAlignmentHash, `{"sealed":"stale"}`, 9)
	_, err = adapter.prepareCheckpointObligation(staleAuthority, marker.MutationGeneration, staleMaterialization)
	require.Error(t, err, "a worker holding superseded authority must lose the persistence CAS")

	// Once the newer authority has any prepared progress, another notification
	// cannot overwrite its exact randomized seal or receipt lifecycle.
	adapter.SetRecoverySource(&checkpointMaterializerSource{
		fakeOutboundRecoverySource: &fakeOutboundRecoverySource{fn: func(syncd.RemoteRecoveryRequest, func(syncd.OutboundEvent) error) (syncd.RemoteRecoveryResult, error) {
			return syncd.RemoteRecoveryResult{}, nil
		}},
		materialize: func(request syncd.RemoteCheckpointMaterializeRequest) (syncd.RemoteCheckpointMaterialization, error) {
			return checkpointMaterializationFixture(request, "newer-head-event", newer.CheckpointAlignmentHash, `{"sealed":"newer"}`, 10), nil
		},
	})
	adapter.fulfillCheckpointObligationsOnce(context.Background())
	values, err = adapter.checkpointObligations.List()
	require.NoError(t, err)
	require.Len(t, values, 1)
	require.Equal(t, "prepared", values[0].CheckpointState)
	preparedEventID := values[0].CheckpointEventID

	latest := newer
	latest.RequestID = "request-latest"
	latest.CheckpointCoverage++
	latest.CheckpointAlignmentHash = watermarkTestDigest("latest-head")
	require.Error(t, adapter.HandleCheckpointNeededV1(latest))
	values, err = adapter.checkpointObligations.List()
	require.NoError(t, err)
	require.Len(t, values, 1)
	require.Equal(t, newer.RequestID, values[0].RequestID)
	require.Equal(t, preparedEventID, values[0].CheckpointEventID)
}

func TestCheckpointWorkerRepairsCrashBetweenObligationAndMarker(t *testing.T) {
	adapter, _ := checkpointRequestAdapter(t)
	notification, _ := checkpointNeededFixture()
	require.NoError(t, adapter.HandleCheckpointNeededV1(notification))

	// Model process death after the obligation fsync but before the handler's
	// marker update by reopening the obligation against a marker root with no
	// authenticated marker yet.
	adapter.outbox.mutations = &RemoteMutationCoordinator{Root: filepath.Join(t.TempDir(), "restarted-markers")}
	materializeCalls := 0
	adapter.SetRecoverySource(&checkpointMaterializerSource{
		fakeOutboundRecoverySource: &fakeOutboundRecoverySource{fn: func(syncd.RemoteRecoveryRequest, func(syncd.OutboundEvent) error) (syncd.RemoteRecoveryResult, error) {
			return syncd.RemoteRecoveryResult{}, nil
		}},
		materialize: func(syncd.RemoteCheckpointMaterializeRequest) (syncd.RemoteCheckpointMaterialization, error) {
			materializeCalls++
			return syncd.RemoteCheckpointMaterialization{}, syncd.ErrRemoteCheckpointUnavailable
		},
	})
	adapter.fulfillCheckpointObligationsOnce(context.Background())
	require.Zero(t, materializeCalls, "repair pass must establish durable marker authority first")
	marker, exists, err := adapter.outbox.mutations.Snapshot("account")
	require.NoError(t, err)
	require.True(t, exists)
	require.Equal(t, "dirty", marker.State)
	require.NotZero(t, marker.ReasonFlags&rescanReasonCheckpoint)
}

func TestCheckpointObligationSurvivesPreparationAndReceiptCrashesThenClearsThroughCanonicalProof(t *testing.T) {
	adapter, client := newRecoveryTestAdapter(t, proto.RemoteSyncModeDeltaRequired)
	client.fakePublishClient.fn = func(_ int, _ []proto.RemoteEvent) (proto.RemotePublishResult, error) {
		return proto.RemotePublishResult{}, nil
	}
	live := checkpointLiveFixture()
	require.NoError(t, adapter.outbox.Append(live))
	require.NoError(t, adapter.parkLiveForCheckpoint(live, "test-gap"))
	installCheckpointCoverage(t, adapter, live, 7)

	source := &checkpointMaterializerSource{
		fakeOutboundRecoverySource: &fakeOutboundRecoverySource{fn: func(request syncd.RemoteRecoveryRequest, _ func(syncd.OutboundEvent) error) (syncd.RemoteRecoveryResult, error) {
			if len(request.Anchors) == 1 && request.Anchors[0].CanonicalEventHash == watermarkTestDigest("live-head") {
				return syncd.RemoteRecoveryResult{Complete: true}, nil
			}
			return syncd.RemoteRecoveryResult{}, nil
		}},
		materialize: func(request syncd.RemoteCheckpointMaterializeRequest) (syncd.RemoteCheckpointMaterialization, error) {
			require.Equal(t, uint64(7), request.Coverage)
			return checkpointMaterializationFixture(request, live.EventID, live.EventHash, `{"sealed":"checkpoint"}`, live.Sequence), nil
		},
	}
	adapter.SetRecoverySource(source)
	adapter.fulfillCheckpointObligationsOnce(context.Background())
	prepared := <-adapter.queue
	require.Equal(t, uint64(7), prepared.CheckpointCoverage)
	require.True(t, outboxHas(t, adapter, prepared.EventID))
	values, err := adapter.checkpointObligations.List()
	require.NoError(t, err)
	require.Len(t, values, 1)
	require.Equal(t, "prepared", values[0].CheckpointState)

	// Crash after preparation/outbox fsync: reopening must queue the exact
	// persisted randomized seal, not rematerialize a different body.
	crashed := reopenCheckpointAdapter(t, adapter, client)
	crashed.SetRecoverySource(source)
	crashed.fulfillCheckpointObligationsOnce(context.Background())
	replayed := <-crashed.queue
	require.Equal(t, prepared.EventID, replayed.EventID)
	require.Equal(t, prepared.BodyDigest, replayed.BodyDigest)

	client.fakePublishClient.fn = func(_ int, batch []proto.RemoteEvent) (proto.RemotePublishResult, error) {
		return proto.RemotePublishResult{Outcomes: []proto.RemotePublishOutcome{committedCheckpointOutcome(batch[0], 8)}}, nil
	}
	crashed.publish(context.Background(), []proto.RemoteEvent{replayed}, remotePublishQueueBacklog)
	values, err = crashed.checkpointObligations.List()
	require.NoError(t, err)
	require.Len(t, values, 1)
	require.Equal(t, "committed", values[0].CheckpointState)
	dirty, err := crashed.outbox.mutations.IsDirty("")
	require.NoError(t, err)
	require.True(t, dirty, "receipt fsync alone must not clear the marker")

	// Crash after receipt fsync: the reopened worker verifies the current head,
	// installs a checkpoint-backed canonical watermark, retires covered live
	// bytes, then ordinary recovery proves and clears the rescan marker.
	afterReceipt := reopenCheckpointAdapter(t, crashed, client)
	afterReceipt.SetRecoverySource(source)
	afterReceipt.fulfillCheckpointObligationsOnce(context.Background())
	afterReceipt.recoverDirtyOnce(context.Background())
	values, err = afterReceipt.checkpointObligations.List()
	require.NoError(t, err)
	require.Empty(t, values)
	require.False(t, outboxHas(t, afterReceipt, live.EventID), "checkpoint receipt supersedes the covered live retry")
	dirty, err = afterReceipt.outbox.mutations.IsDirty("")
	require.NoError(t, err)
	require.False(t, dirty)
}

func TestCommittedCheckpointCannotClearMarkerAfterCanonicalHeadSupersession(t *testing.T) {
	adapter, client := newRecoveryTestAdapter(t, proto.RemoteSyncModeDeltaPreferred)
	live := checkpointLiveFixture()
	require.NoError(t, adapter.outbox.Append(live))
	require.NoError(t, adapter.parkLiveForCheckpoint(live, "test-gap"))
	installCheckpointCoverage(t, adapter, live, 11)
	currentID, currentHash, currentSequence := live.EventID, live.EventHash, live.Sequence
	source := &checkpointMaterializerSource{
		fakeOutboundRecoverySource: &fakeOutboundRecoverySource{fn: func(syncd.RemoteRecoveryRequest, func(syncd.OutboundEvent) error) (syncd.RemoteRecoveryResult, error) {
			return syncd.RemoteRecoveryResult{Complete: true}, nil
		}},
		materialize: func(request syncd.RemoteCheckpointMaterializeRequest) (syncd.RemoteCheckpointMaterialization, error) {
			return checkpointMaterializationFixture(request, currentID, currentHash, `{"sealed":"checkpoint"}`, currentSequence), nil
		},
	}
	adapter.SetRecoverySource(source)
	adapter.fulfillCheckpointObligationsOnce(context.Background())
	first := <-adapter.queue
	client.fakePublishClient.fn = func(_ int, batch []proto.RemoteEvent) (proto.RemotePublishResult, error) {
		return proto.RemotePublishResult{Outcomes: []proto.RemotePublishOutcome{committedCheckpointOutcome(batch[0], 12)}}, nil
	}
	adapter.publish(context.Background(), []proto.RemoteEvent{first}, remotePublishQueueBacklog)

	newLive := live
	newLive.EventID = "event-new-head"
	newLive.EventHash = watermarkTestDigest("new-live-head")
	newLive.BodyDigest = sealedBodyDigest([]byte(`{"sealed":"new-live"}`))
	newLive.Bytes = []byte(`{"sealed":"new-live"}`)
	newLive.Sequence++
	require.NoError(t, adapter.outbox.RequireCanonicalRecovery(newLive))
	currentID, currentHash, currentSequence = newLive.EventID, newLive.EventHash, newLive.Sequence
	adapter.fulfillCheckpointObligationsOnce(context.Background())
	second := <-adapter.queue
	require.Equal(t, currentHash, second.CheckpointAlignmentHash)
	require.NotEqual(t, first.EventID, second.EventID)
	dirty, err := adapter.outbox.mutations.IsDirty("")
	require.NoError(t, err)
	require.True(t, dirty, "older committed checkpoint must not clear superseding canonical work")
	values, err := adapter.checkpointObligations.List()
	require.NoError(t, err)
	require.Len(t, values, 1)
	require.Equal(t, "prepared", values[0].CheckpointState)
	require.Equal(t, currentHash, values[0].CheckpointHeadHash)
}

func TestConcurrentMultiArtifactMutationRevalidatesAndRebindsVerifiedObligations(t *testing.T) {
	adapter, client := newRecoveryTestAdapter(t, proto.RemoteSyncModeDeltaRequired)
	firstLive := checkpointLiveFixture()
	firstLive.ArtifactID = "artifact-1"
	firstLive.EventID = "head-1"
	firstLive.EventHash = watermarkTestDigest("head-1")
	secondLive := checkpointLiveFixture()
	secondLive.ArtifactID = "artifact-2"
	secondLive.EventID = "head-2"
	secondLive.EventHash = watermarkTestDigest("head-2")
	require.NoError(t, adapter.outbox.Append(firstLive))
	require.NoError(t, adapter.outbox.Append(secondLive))
	require.NoError(t, adapter.parkLiveForCheckpoint(firstLive, "gap-1"))
	require.NoError(t, adapter.parkLiveForCheckpoint(secondLive, "gap-2"))
	installCheckpointCoverage(t, adapter, firstLive, 7)
	installCheckpointCoverage(t, adapter, secondLive, 9)

	injectConcurrentMutation := false
	source := &checkpointMaterializerSource{
		fakeOutboundRecoverySource: &fakeOutboundRecoverySource{fn: func(syncd.RemoteRecoveryRequest, func(syncd.OutboundEvent) error) (syncd.RemoteRecoveryResult, error) {
			return syncd.RemoteRecoveryResult{Complete: true}, nil
		}},
		materialize: func(request syncd.RemoteCheckpointMaterializeRequest) (syncd.RemoteCheckpointMaterialization, error) {
			live := firstLive
			if request.ArtifactID == secondLive.ArtifactID {
				live = secondLive
				if injectConcurrentMutation {
					require.NoError(t, adapter.outbox.RequireCanonicalRecovery(secondLive))
					injectConcurrentMutation = false
				}
			}
			return checkpointMaterializationFixture(request, live.EventID, live.EventHash, `{"sealed":"checkpoint"}`, live.Sequence), nil
		},
	}
	adapter.SetRecoverySource(source)
	adapter.fulfillCheckpointObligationsOnce(context.Background())
	checkpoints := drainQueue(adapter.queue)
	require.Len(t, checkpoints, 2)
	client.fakePublishClient.fn = func(_ int, batch []proto.RemoteEvent) (proto.RemotePublishResult, error) {
		position := batch[0].CheckpointCoverage + 1
		return proto.RemotePublishResult{Outcomes: []proto.RemotePublishOutcome{committedCheckpointOutcome(batch[0], position)}}, nil
	}
	for _, checkpoint := range checkpoints {
		adapter.publish(context.Background(), []proto.RemoteEvent{checkpoint}, remotePublishQueueBacklog)
	}

	injectConcurrentMutation = true
	adapter.fulfillCheckpointObligationsOnce(context.Background())
	values, err := adapter.checkpointObligations.List()
	require.NoError(t, err)
	require.Len(t, values, 2)
	markers, err := adapter.outbox.mutations.ListDirty()
	require.NoError(t, err)
	require.Len(t, markers, 1)
	for _, value := range values {
		require.Equal(t, "verified", value.CheckpointState)
		require.Less(t, value.MarkerGeneration, markers[0].Marker.MutationGeneration,
			"concurrent reservation must defeat the stale finalization CAS")
	}

	adapter.fulfillCheckpointObligationsOnce(context.Background())
	values, err = adapter.checkpointObligations.List()
	require.NoError(t, err)
	require.Empty(t, values, "next pass must revalidate and rebind every verified artifact")
	adapter.recoverDirtyOnce(context.Background())
	dirty, err := adapter.outbox.mutations.IsDirty("")
	require.NoError(t, err)
	require.False(t, dirty)
}

func TestTerminalStagedRetainedRejectionPreservesExactFileAndCreatesObligation(t *testing.T) {
	client := &durablePolicyPublishClient{required: true, fakePublishClient: &fakePublishClient{fn: func(_ int, batch []proto.RemoteEvent) (proto.RemotePublishResult, error) {
		return proto.RemotePublishResult{Outcomes: []proto.RemotePublishOutcome{{EventID: batch[0].EventID, Accepted: false, Retryable: false, Error: "terminal"}}}, nil
	}}}
	adapter := newTestPublishAdapterWithOutbox(t, client)
	body := make([]byte, proto.MaxSealedEventBytes+1)
	copy(body, []byte(`{"sealed":"large-checkpoint"}`))
	digest := sealedBodyDigest(body)
	fileID := watermarkTestDigest("staged-file")
	path := filepath.Join(adapter.outbox.staged(), fileID)
	require.NoError(t, os.WriteFile(path, body, 0o600))
	generation := checkpointGenerationFixture()
	event := proto.RemoteEvent{
		AccessGeneration: generation.AccessGeneration, AccessSetHash: generation.AccessSetHash,
		SecurityGeneration: generation.SecurityGeneration, SecurityBarrierID: generation.SecurityBarrierID,
		KeyMode: generation.KeyMode, BranchID: "main", ArtifactID: "artifact-staged", EventID: "checkpoint-staged",
		EventHash: watermarkTestDigest("checkpoint-wrapper"), CheckpointAlignmentHash: watermarkTestDigest("covered-head"),
		BodyDigest: digest, Kind: "conversation", Type: "update", Timestamp: time.Now().UTC(), Sequence: 5,
		Origin: "device-a", Lane: syncd.LaneRetained,
		DaemonStagedPayload: &proto.RemoteDaemonStagedPayloadV1{FileID: fileID, SealedBytes: uint64(len(body)), BodyDigest: digest},
	}
	require.NoError(t, adapter.outbox.Append(event))
	adapter.publish(context.Background(), []proto.RemoteEvent{event}, remotePublishQueueBacklog)
	require.True(t, outboxHas(t, adapter, event.EventID))
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, int64(len(body)), info.Size())
	values, err := adapter.checkpointObligations.List()
	require.NoError(t, err)
	require.Len(t, values, 1)
	require.Equal(t, "terminal-staged-checkpoint-rejection", values[0].Reason)
	markers, err := adapter.outbox.mutations.ListDirty()
	require.NoError(t, err)
	require.Len(t, markers, 1)
	require.NotZero(t, markers[0].Marker.ReasonFlags&rescanReasonCheckpoint)
}
