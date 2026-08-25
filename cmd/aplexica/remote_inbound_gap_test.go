package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aplexica/aplexica/internal/daemon"
	"github.com/aplexica/aplexica/internal/plugin/proto"
	syncd "github.com/aplexica/aplexica/internal/sync"
	"github.com/stretchr/testify/require"
)

type durableGapRemoteStub struct {
	parents          map[string]proto.RemoteFetchParentV1Result
	fetchErr         error
	checkpoint       proto.RemoteRequestCheckpointV1Result
	checkpointErr    error
	fetchedHashes    []string
	checkpointParams []proto.RemoteRequestCheckpointV1Params
}

type durableGapResolveFailureSpool struct {
	store *daemon.DurableGapStore
	err   error
}

func (spool *durableGapResolveFailureSpool) Put(key daemon.DurableGapKey, delivery proto.RemoteInboundDeliveryV2, missingParentHash string, missingEventIndex ...uint16) (daemon.DurableGap, error) {
	return spool.store.Put(key, delivery, missingParentHash, missingEventIndex...)
}

func (spool *durableGapResolveFailureSpool) Load(key daemon.DurableGapKey) (daemon.DurableGap, error) {
	return spool.store.Load(key)
}

func (spool *durableGapResolveFailureSpool) AdvanceSelector(key daemon.DurableGapKey, delivery proto.RemoteInboundDeliveryV2, priorMissingParentHash string, priorMissingEventIndex uint16, nextMissingParentHash string, nextMissingEventIndex uint16) (daemon.DurableGap, error) {
	return spool.store.AdvanceSelector(key, delivery, priorMissingParentHash, priorMissingEventIndex, nextMissingParentHash, nextMissingEventIndex)
}

func (spool *durableGapResolveFailureSpool) Resolve(daemon.DurableGapKey, string) error {
	return spool.err
}

func (stub *durableGapRemoteStub) FetchParentV1(_ context.Context, params proto.RemoteFetchParentV1Params) (proto.RemoteFetchParentV1Result, error) {
	stub.fetchedHashes = append(stub.fetchedHashes, params.EventHash)
	if stub.fetchErr != nil {
		return proto.RemoteFetchParentV1Result{}, stub.fetchErr
	}
	return stub.parents[params.EventHash], nil
}

func (stub *durableGapRemoteStub) RequestCheckpointV1(_ context.Context, params proto.RemoteRequestCheckpointV1Params) (proto.RemoteRequestCheckpointV1Result, error) {
	stub.checkpointParams = append(stub.checkpointParams, params)
	return stub.checkpoint, stub.checkpointErr
}

func durableGapTestBody(label string) (json.RawMessage, string) {
	body := json.RawMessage(`{"version":2,"ciphertext":"` + label + `"}`)
	digest := sha256.Sum256(body)
	return body, hex.EncodeToString(digest[:])
}

func durableGapRecoveryRecord(event proto.RemoteEvent, position uint64) *proto.RemoteRecoveryEventV1 {
	predecessor := "signed-cursor-" + string(rune('a'+position-1))
	cursor := "signed-cursor-" + string(rune('a'+position))
	digest := sha256.Sum256([]byte(cursor))
	return &proto.RemoteRecoveryEventV1{
		Event: event, PredecessorCursor: predecessor, PredecessorPosition: position - 1,
		Cursor: cursor, CursorDigest: hex.EncodeToString(digest[:]), Position: position,
	}
}

func durableGapCheckpointRecord(event proto.RemoteEvent, position uint64, coverageCursor string, coveragePosition uint64) *proto.RemoteRecoveryEventV1 {
	// The checkpoint covers the missing canonical head through its dedicated
	// authenticated alignment. ParentHash remains the checkpoint event's own
	// predecessor and is intentionally empty in this genesis-shaped fixture.
	event.CheckpointAlignmentHash = event.ParentHash
	event.ParentHash = ""
	event.Lane = syncd.LaneRetained
	record := durableGapRecoveryRecord(event, position)
	digest := sha256.Sum256([]byte(coverageCursor))
	record.CoverageCursor = coverageCursor
	record.CoverageCursorDigest = hex.EncodeToString(digest[:])
	record.CoveragePosition = coveragePosition
	return record
}

func durableGapDeliveryForTest() (proto.RemoteInboundDeliveryV2, *durableInboundCursorBinding) {
	delivery := durableInboundTestSuccessor("signed-main-cursor-4", "signed-main-cursor-5", 5)
	delivery.DeliveryID = "delivery-gap-recovery"
	body, digest := durableGapTestBody("original")
	delivery.Events[0].NamespaceID = "namespace-1"
	delivery.Events[0].BranchID = "main"
	delivery.Events[0].ArtifactID = "artifact-1"
	delivery.Events[0].EventID = "event-original"
	delivery.Events[0].EventHash = strings.Repeat("d", 64)
	delivery.Events[0].ParentHash = strings.Repeat("a", 64)
	delivery.Events[0].BodyDigest = digest
	delivery.Events[0].Bytes = body
	delivery.Events[0].AccessGeneration = 3
	delivery.Events[0].AccessSetHash = sha256.Sum256([]byte("access-generation-3"))
	delivery.Events[0].SecurityBarrierID = sha256.Sum256([]byte("security-barrier-7"))
	delivery.Events[0].SecurityGeneration = 7
	delivery.Events[0].KeyMode = "recipient-wrap-v2"
	binding, err := bindDurableInboundCursor("device-1", durableInboundTestNegotiation(proto.RemoteSyncModeDurableRead), delivery)
	if err != nil {
		panic(err)
	}
	return delivery, binding
}

func durableGapParentEvent(hash, parent, label string) proto.RemoteEvent {
	body, digest := durableGapTestBody(label)
	return proto.RemoteEvent{
		NamespaceID: "namespace-1", BranchID: "main", ArtifactID: "artifact-1", EventID: "event-" + label,
		EventHash: hash, ParentHash: parent, BodyDigest: digest, Bytes: body,
		AccessGeneration: 3, AccessSetHash: sha256.Sum256([]byte("access-generation-3")),
		SecurityBarrierID: sha256.Sum256([]byte("security-barrier-7")), SecurityGeneration: 7, KeyMode: "recipient-wrap-v2",
	}
}

func durableGapBatchDeliveryForTest(t *testing.T) (proto.RemoteInboundDeliveryV2, *durableInboundCursorBinding) {
	t.Helper()
	delivery, _ := durableGapDeliveryForTest()
	delivery.DeliveryID = "delivery-gap-batch-recovery"
	delivery.Cursor = "signed-main-cursor-6"
	cursorDigest := sha256.Sum256([]byte(delivery.Cursor))
	delivery.CursorDigest = hex.EncodeToString(cursorDigest[:])
	delivery.Position = 6
	delivery.Events[0].ParentHash = ""
	second := delivery.Events[0]
	second.ArtifactID = "artifact-2"
	second.EventID = "event-original-2"
	second.EventHash = strings.Repeat("e", 64)
	second.ParentHash = strings.Repeat("a", 64)
	second.Bytes, second.BodyDigest = durableGapTestBody("original-2")
	delivery.Events = append(delivery.Events, second)
	delivery.BatchEventCount = uint16(len(delivery.Events))
	batchDigest, err := proto.RemoteReplayBatchDigest(delivery)
	require.NoError(t, err)
	delivery.BatchDigest = batchDigest
	binding, err := bindDurableInboundCursor("device-1", durableInboundBatchNegotiation(), delivery)
	require.NoError(t, err)
	return delivery, binding
}

func TestRecoverDurableInboundGapFetchesAndAppliesAncestorsOldestFirst(t *testing.T) {
	delivery, binding := durableGapDeliveryForTest()
	hashA, hashB, hashC := strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64)
	remote := &durableGapRemoteStub{parents: map[string]proto.RemoteFetchParentV1Result{
		hashA: {Found: true, Record: durableGapRecoveryRecord(durableGapParentEvent(hashA, hashB, "a"), 4)},
		hashB: {Found: true, Record: durableGapRecoveryRecord(durableGapParentEvent(hashB, hashC, "b"), 3)},
		hashC: {Found: true, Record: durableGapRecoveryRecord(durableGapParentEvent(hashC, "", "c"), 2)},
	}}
	applied := make(map[string]bool)
	var applyOrder []string
	importer := func(events []proto.RemoteEvent) []syncd.ImportOutcome {
		event := events[0]
		if event.ParentHash != "" && !applied[event.ParentHash] {
			return []syncd.ImportOutcome{syncd.ImportDeferredNeedsBaseline}
		}
		applied[event.EventHash] = true
		applyOrder = append(applyOrder, event.EventHash)
		return []syncd.ImportOutcome{syncd.ImportApplied}
	}
	root := filepath.Join(t.TempDir(), "gaps")
	spool := &daemon.DurableGapStore{Root: root}

	results, err := recoverDurableInboundGap(context.Background(), spool, remote, nil, importer, binding, delivery, []syncd.ImportOutcome{syncd.ImportDeferredNeedsBaseline})
	require.NoError(t, err)
	require.Equal(t, []syncd.ImportOutcome{syncd.ImportApplied}, results)
	require.Equal(t, []string{hashC, hashB, hashA, delivery.Events[0].EventHash}, applyOrder)
	require.Equal(t, []string{hashA, hashB, hashC}, remote.fetchedHashes)
	require.Empty(t, remote.checkpointParams)
	key, err := durableGapKey(binding)
	require.NoError(t, err)
	_, err = spool.Load(key)
	require.ErrorIs(t, err, daemon.ErrDurableGapNotFound)
}

func TestRecoverDurableInboundGapAcceptsAuthenticatedCrossBranchParent(t *testing.T) {
	delivery, binding := durableGapDeliveryForTest()
	parent := durableGapRecoveryRecord(durableGapParentEvent(delivery.Events[0].ParentHash, "", "cross-branch-parent"), delivery.Position-1)
	parent.Event.BranchID = "fork-from-main"
	remote := &durableGapRemoteStub{parents: map[string]proto.RemoteFetchParentV1Result{
		delivery.Events[0].ParentHash: {Found: true, Record: parent},
	}}
	appliedParent := false
	results, err := recoverDurableInboundGap(context.Background(), &daemon.DurableGapStore{Root: filepath.Join(t.TempDir(), "gaps")}, remote, nil, func(events []proto.RemoteEvent) []syncd.ImportOutcome {
		if events[0].EventHash == delivery.Events[0].ParentHash {
			require.Equal(t, "fork-from-main", events[0].BranchID)
			appliedParent = true
			return []syncd.ImportOutcome{syncd.ImportApplied}
		}
		if appliedParent {
			return []syncd.ImportOutcome{syncd.ImportApplied}
		}
		return []syncd.ImportOutcome{syncd.ImportDeferredNeedsBaseline}
	}, binding, delivery, []syncd.ImportOutcome{syncd.ImportDeferredNeedsBaseline})
	require.NoError(t, err)
	require.Equal(t, []syncd.ImportOutcome{syncd.ImportApplied}, results)
}

func TestRecoverDurableInboundGapRejectsParentWithoutEarlierPositionOrSecurityBinding(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*proto.RemoteRecoveryEventV1, proto.RemoteInboundDeliveryV2)
	}{
		{
			name: "not earlier in durable log",
			mutate: func(record *proto.RemoteRecoveryEventV1, delivery proto.RemoteInboundDeliveryV2) {
				record.Position = delivery.Position
				record.PredecessorPosition = delivery.Position - 1
			},
		},
		{
			name: "different security generation",
			mutate: func(record *proto.RemoteRecoveryEventV1, _ proto.RemoteInboundDeliveryV2) {
				record.Event.SecurityGeneration++
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			delivery, binding := durableGapDeliveryForTest()
			parent := durableGapRecoveryRecord(durableGapParentEvent(delivery.Events[0].ParentHash, "", "parent"), delivery.Position-1)
			test.mutate(parent, delivery)
			remote := &durableGapRemoteStub{parents: map[string]proto.RemoteFetchParentV1Result{
				delivery.Events[0].ParentHash: {Found: true, Record: parent},
			}}
			spool := &daemon.DurableGapStore{Root: filepath.Join(t.TempDir(), "gaps")}
			results, err := recoverDurableInboundGap(context.Background(), spool, remote, nil, func([]proto.RemoteEvent) []syncd.ImportOutcome {
				return []syncd.ImportOutcome{syncd.ImportApplied}
			}, binding, delivery, []syncd.ImportOutcome{syncd.ImportDeferredNeedsBaseline})
			require.ErrorIs(t, err, errDurableGapMalformed)
			require.Equal(t, []syncd.ImportOutcome{syncd.ImportDeferredNeedsBaseline}, results)
			require.Empty(t, remote.checkpointParams)
			key, keyErr := durableGapKey(binding)
			require.NoError(t, keyErr)
			_, loadErr := spool.Load(key)
			require.NoError(t, loadErr, "malformed parent must leave the durable gap stopped")
		})
	}
}

func TestRecoverDurableInboundGapSurvivesCrashAndRetriesFromSpool(t *testing.T) {
	delivery, binding := durableGapDeliveryForTest()
	root := filepath.Join(t.TempDir(), "gaps")
	key, err := durableGapKey(binding)
	require.NoError(t, err)
	firstStore := &daemon.DurableGapStore{Root: root}
	_, err = firstStore.Put(key, delivery, delivery.Events[0].ParentHash)
	require.NoError(t, err)

	transient := errors.New("network unavailable")
	failedRemote := &durableGapRemoteStub{fetchErr: transient}
	results, err := recoverDurableInboundGap(context.Background(), &daemon.DurableGapStore{Root: root}, failedRemote, nil, func([]proto.RemoteEvent) []syncd.ImportOutcome {
		return []syncd.ImportOutcome{syncd.ImportDeferredNeedsBaseline}
	}, binding, delivery, []syncd.ImportOutcome{syncd.ImportDeferredNeedsBaseline})
	require.ErrorIs(t, err, transient)
	require.Equal(t, []syncd.ImportOutcome{syncd.ImportDeferredNeedsBaseline}, results)
	_, err = (&daemon.DurableGapStore{Root: root}).Load(key)
	require.NoError(t, err, "crash/retry must retain the encrypted gap")

	parent := durableGapParentEvent(delivery.Events[0].ParentHash, "", "parent")
	remote := &durableGapRemoteStub{parents: map[string]proto.RemoteFetchParentV1Result{
		delivery.Events[0].ParentHash: {Found: true, Record: durableGapRecoveryRecord(parent, 1)},
	}}
	applied := make(map[string]bool)
	importer := func(events []proto.RemoteEvent) []syncd.ImportOutcome {
		event := events[0]
		if event.ParentHash != "" && !applied[event.ParentHash] {
			return []syncd.ImportOutcome{syncd.ImportDeferredNeedsBaseline}
		}
		applied[event.EventHash] = true
		return []syncd.ImportOutcome{syncd.ImportApplied}
	}
	results, err = recoverDurableInboundGap(context.Background(), &daemon.DurableGapStore{Root: root}, remote, nil, importer, binding, delivery, []syncd.ImportOutcome{syncd.ImportDeferredNeedsBaseline})
	require.NoError(t, err)
	require.Equal(t, []syncd.ImportOutcome{syncd.ImportApplied}, results)
	_, err = (&daemon.DurableGapStore{Root: root}).Load(key)
	require.ErrorIs(t, err, daemon.ErrDurableGapNotFound)
}

func TestRecoverDurableInboundBatchGapSurvivesRestartAndReplaysWholeSpan(t *testing.T) {
	delivery, binding := durableGapBatchDeliveryForTest(t)
	root := filepath.Join(t.TempDir(), "gaps")
	key, err := durableGapKey(binding)
	require.NoError(t, err)
	transient := errors.New("network unavailable")

	results, err := recoverDurableInboundGap(
		context.Background(), &daemon.DurableGapStore{Root: root}, &durableGapRemoteStub{fetchErr: transient}, nil,
		func([]proto.RemoteEvent) []syncd.ImportOutcome { return nil }, binding, delivery,
		[]syncd.ImportOutcome{syncd.ImportApplied, syncd.ImportDeferredNeedsBaseline},
	)
	require.ErrorIs(t, err, transient)
	require.Equal(t, durableGapRetryableResults(2), results)
	persisted, err := (&daemon.DurableGapStore{Root: root}).Load(key)
	require.NoError(t, err)
	require.Equal(t, uint16(1), persisted.MissingEventIndex)
	require.Equal(t, delivery.BatchDigest, persisted.Delivery.BatchDigest)

	parent := durableGapParentEvent(delivery.Events[1].ParentHash, "", "batch-parent")
	parent.ArtifactID = delivery.Events[1].ArtifactID
	remote := &durableGapRemoteStub{parents: map[string]proto.RemoteFetchParentV1Result{
		delivery.Events[1].ParentHash: {Found: true, Record: durableGapRecoveryRecord(parent, 3)},
	}}
	parentApplied := false
	batchReplays := 0
	importer := func(events []proto.RemoteEvent) []syncd.ImportOutcome {
		if len(events) == 1 && events[0].EventHash == parent.EventHash {
			parentApplied = true
			return []syncd.ImportOutcome{syncd.ImportApplied}
		}
		require.Equal(t, delivery.Events, events)
		batchReplays++
		if !parentApplied {
			return []syncd.ImportOutcome{syncd.ImportDeduped, syncd.ImportDeferredNeedsBaseline}
		}
		return []syncd.ImportOutcome{syncd.ImportDeduped, syncd.ImportApplied}
	}
	results, err = recoverDurableInboundGap(
		context.Background(), &daemon.DurableGapStore{Root: root}, remote, nil, importer, binding, delivery,
		[]syncd.ImportOutcome{syncd.ImportDeduped, syncd.ImportDeferredNeedsBaseline},
	)
	require.NoError(t, err)
	require.Equal(t, []syncd.ImportOutcome{syncd.ImportDeduped, syncd.ImportApplied}, results)
	require.Equal(t, 1, batchReplays)
	require.Equal(t, []string{delivery.Events[1].ParentHash}, remote.fetchedHashes)
	_, err = (&daemon.DurableGapStore{Root: root}).Load(key)
	require.ErrorIs(t, err, daemon.ErrDurableGapNotFound)

	// An already-recovered restart is a pure idempotent pass-through: it does
	// not re-fetch, re-spool, or reapply any encrypted event.
	replayCalls := 0
	terminal := []syncd.ImportOutcome{syncd.ImportDeduped, syncd.ImportApplied}
	results, err = recoverDurableInboundGap(context.Background(), &daemon.DurableGapStore{Root: root}, remote, nil, func([]proto.RemoteEvent) []syncd.ImportOutcome {
		replayCalls++
		return nil
	}, binding, delivery, terminal)
	require.NoError(t, err)
	require.Equal(t, terminal, results)
	require.Zero(t, replayCalls)
}

func TestRecoverDurableInboundBatchResolvesIndependentGapsInOrder(t *testing.T) {
	delivery, _ := durableGapBatchDeliveryForTest(t)
	delivery.Events = append([]proto.RemoteEvent(nil), delivery.Events...)
	delivery.Events[0].ParentHash = strings.Repeat("b", 64)
	delivery.BatchDigest = ""
	batchDigest, err := proto.RemoteReplayBatchDigest(delivery)
	require.NoError(t, err)
	delivery.BatchDigest = batchDigest
	binding, err := bindDurableInboundCursor("device-1", durableInboundBatchNegotiation(), delivery)
	require.NoError(t, err)
	key, err := durableGapKey(binding)
	require.NoError(t, err)

	firstParent := durableGapParentEvent(delivery.Events[0].ParentHash, "", "first-independent-parent")
	firstParent.ArtifactID = delivery.Events[0].ArtifactID
	secondParent := durableGapParentEvent(delivery.Events[1].ParentHash, "", "second-independent-parent")
	secondParent.ArtifactID = delivery.Events[1].ArtifactID
	remote := &durableGapRemoteStub{parents: map[string]proto.RemoteFetchParentV1Result{
		firstParent.EventHash:  {Found: true, Record: durableGapRecoveryRecord(firstParent, 3)},
		secondParent.EventHash: {Found: true, Record: durableGapRecoveryRecord(secondParent, 4)},
	}}
	applied := make(map[string]bool)
	importer := func(events []proto.RemoteEvent) []syncd.ImportOutcome {
		outcomes := make([]syncd.ImportOutcome, len(events))
		for index, event := range events {
			if event.ParentHash != "" && !applied[event.ParentHash] {
				outcomes[index] = syncd.ImportDeferredNeedsBaseline
				continue
			}
			if applied[event.EventHash] {
				outcomes[index] = syncd.ImportDeduped
				continue
			}
			applied[event.EventHash] = true
			outcomes[index] = syncd.ImportApplied
		}
		return outcomes
	}
	root := filepath.Join(t.TempDir(), "gaps")

	resolveFailure := errors.New("crash before gap resolve")
	firstResults, err := recoverDurableInboundGap(
		context.Background(), &durableGapResolveFailureSpool{store: &daemon.DurableGapStore{Root: root}, err: resolveFailure}, remote, nil, importer, binding, delivery,
		[]syncd.ImportOutcome{syncd.ImportDeferredNeedsBaseline, syncd.ImportDeferredNeedsBaseline},
	)
	require.ErrorIs(t, err, resolveFailure)
	require.Equal(t, durableGapRetryableResults(2), firstResults)
	crashRecord, err := (&daemon.DurableGapStore{Root: root}).Load(key)
	require.NoError(t, err)
	require.Equal(t, uint16(0), crashRecord.MissingEventIndex, "the pre-resolve crash must retain the encrypted first selector")

	secondResults, err := recoverDurableInboundGap(
		context.Background(), &daemon.DurableGapStore{Root: root}, remote, nil, importer, binding, delivery,
		[]syncd.ImportOutcome{syncd.ImportDeduped, syncd.ImportDeferredNeedsBaseline},
	)
	require.NoError(t, err)
	require.Equal(t, []syncd.ImportOutcome{syncd.ImportDeduped, syncd.ImportApplied}, secondResults)
	require.Equal(t, []string{firstParent.EventHash, secondParent.EventHash}, remote.fetchedHashes)
	_, err = (&daemon.DurableGapStore{Root: root}).Load(key)
	require.ErrorIs(t, err, daemon.ErrDurableGapNotFound)
}

func TestRecoverDurableInboundBatchGapFallsBackToSelectedCheckpoint(t *testing.T) {
	delivery, binding := durableGapBatchDeliveryForTest(t)
	selected := delivery.Events[1]
	generation, err := durableCheckpointGeneration(selected)
	require.NoError(t, err)
	checkpoint := durableGapParentEvent(strings.Repeat("f", 64), selected.ParentHash, "batch-checkpoint")
	checkpoint.ArtifactID = selected.ArtifactID
	checkpoint.Kind = selected.Kind
	checkpoint.CheckpointCoverage = delivery.PredecessorPosition
	checkpoint.CheckpointGeneration = generation
	checkpointRecord := durableGapCheckpointRecord(checkpoint, delivery.Position+1, delivery.PredecessorCursor, delivery.PredecessorPosition)
	remote := &durableGapRemoteStub{
		parents: map[string]proto.RemoteFetchParentV1Result{
			selected.ParentHash: {Found: false, CheckpointRequired: true, ReasonCode: "compacted"},
		},
		checkpoint: proto.RemoteRequestCheckpointV1Result{Requested: true, RequestID: "request-batch", Checkpoint: checkpointRecord},
	}
	checkpointApplied := false
	root := filepath.Join(t.TempDir(), "gaps")
	results, err := recoverDurableInboundGap(
		context.Background(), &daemon.DurableGapStore{Root: root}, remote,
		&daemon.DurableCursorStore{Root: filepath.Join(t.TempDir(), "cursors")},
		func(events []proto.RemoteEvent) []syncd.ImportOutcome {
			if len(events) == 1 && events[0].CheckpointCoverage > 0 {
				checkpointApplied = true
				return []syncd.ImportOutcome{syncd.ImportApplied}
			}
			require.Equal(t, delivery.Events, events)
			require.True(t, checkpointApplied)
			return []syncd.ImportOutcome{syncd.ImportDeduped, syncd.ImportApplied}
		},
		binding, delivery, []syncd.ImportOutcome{syncd.ImportDeduped, syncd.ImportDeferredNeedsBaseline},
	)
	require.NoError(t, err)
	require.Equal(t, []syncd.ImportOutcome{syncd.ImportDeduped, syncd.ImportApplied}, results)
	require.Len(t, remote.checkpointParams, 1)
	request := remote.checkpointParams[0]
	require.Equal(t, selected.ArtifactID, request.ArtifactID)
	require.Equal(t, selected.ParentHash, request.MissingParentHash)
	require.Equal(t, generation, request.CheckpointGeneration)
	require.Equal(t, delivery.PredecessorPosition+2, request.MinimumCoverage, "batch request must cover the selected blocked event, not only the page predecessor")
	require.Contains(t, request.RequestID, "gap-")
	key, keyErr := durableGapKey(binding)
	require.NoError(t, keyErr)
	_, loadErr := (&daemon.DurableGapStore{Root: root}).Load(key)
	require.ErrorIs(t, loadErr, daemon.ErrDurableGapNotFound)
}

func TestRecoverDurableInboundGapFallsBackToCompatibleCheckpoint(t *testing.T) {
	delivery, binding := durableGapDeliveryForTest()
	expectedGeneration, err := durableCheckpointGeneration(delivery.Events[0])
	require.NoError(t, err)
	require.Equal(t, "b126075a807c857e17335f13d874c90bdb19d1e70cc74f30ac64ec3ee086093a", expectedGeneration, "checkpoint generation must match the cloud protocol vector")
	require.Empty(t, delivery.Events[0].CheckpointGeneration, "ordinary deltas do not carry checkpoint generations")
	checkpoint := durableGapParentEvent(strings.Repeat("e", 64), delivery.Events[0].ParentHash, "checkpoint")
	checkpoint.Kind = delivery.Events[0].Kind
	checkpoint.CheckpointCoverage = 2
	checkpoint.CheckpointGeneration = expectedGeneration
	checkpointRecord := durableGapCheckpointRecord(checkpoint, 6, "signed-main-cursor-2", checkpoint.CheckpointCoverage)
	remote := &durableGapRemoteStub{
		parents: map[string]proto.RemoteFetchParentV1Result{
			delivery.Events[0].ParentHash: {Found: false, CheckpointRequired: true, ReasonCode: "compacted"},
		},
		checkpoint: proto.RemoteRequestCheckpointV1Result{Requested: true, RequestID: "request-1", Checkpoint: checkpointRecord},
	}
	baselineApplied := false
	importer := func(events []proto.RemoteEvent) []syncd.ImportOutcome {
		event := events[0]
		if event.CheckpointCoverage > 0 {
			baselineApplied = true
			return []syncd.ImportOutcome{syncd.ImportApplied}
		}
		if event.EventID == delivery.Events[0].EventID && baselineApplied {
			return []syncd.ImportOutcome{syncd.ImportApplied}
		}
		return []syncd.ImportOutcome{syncd.ImportDeferredNeedsBaseline}
	}
	spool := &daemon.DurableGapStore{Root: filepath.Join(t.TempDir(), "gaps")}
	cursors := &daemon.DurableCursorStore{Root: filepath.Join(t.TempDir(), "cursors")}
	var current *daemon.DurableCursorState
	for position, cursor := range []string{"signed-main-cursor-1", "signed-main-cursor-2", "signed-main-cursor-3", delivery.PredecessorCursor} {
		digest := sha256.Sum256([]byte(cursor))
		next := daemon.DurableCursorState{Cursor: cursor, CursorDigest: hex.EncodeToString(digest[:]), Position: uint64(position + 1)}
		persisted, advanceErr := cursors.CompareAndSwap(binding.key, current, next)
		require.NoError(t, advanceErr)
		current = &persisted
	}
	results, err := recoverDurableInboundGap(context.Background(), spool, remote, cursors, importer, binding, delivery, []syncd.ImportOutcome{syncd.ImportDeferredNeedsBaseline})
	require.NoError(t, err)
	require.Equal(t, []syncd.ImportOutcome{syncd.ImportApplied}, results)
	require.Len(t, remote.checkpointParams, 1)
	request := remote.checkpointParams[0]
	require.Equal(t, delivery.Events[0].ParentHash, request.MissingParentHash)
	require.Equal(t, expectedGeneration, request.CheckpointGeneration)
	require.Equal(t, delivery.PredecessorCursor, request.Cursor)
	require.Equal(t, delivery.PredecessorPosition, request.Position)
	require.Equal(t, delivery.Position, request.MinimumCoverage)
	persisted, err := cursors.Load(binding.key)
	require.NoError(t, err)
	require.Equal(t, delivery.PredecessorCursor, persisted.Cursor)
	require.Equal(t, delivery.PredecessorPosition, persisted.Position)
	require.NotEqual(t, checkpoint.CheckpointCoverage, delivery.PredecessorPosition, "artifact coverage may precede interleaved stream records")
}

func TestCheckpointRecordAcceptsArtifactAlignedInterleavedCoverageAndRejectsUnrelatedCheckpoint(t *testing.T) {
	delivery, _ := durableGapDeliveryForTest()
	generation, err := durableCheckpointGeneration(delivery.Events[0])
	require.NoError(t, err)
	checkpoint := durableGapParentEvent(strings.Repeat("e", 64), delivery.Events[0].ParentHash, "checkpoint")
	checkpoint.Kind = delivery.Events[0].Kind
	checkpoint.CheckpointCoverage = 2
	checkpoint.CheckpointGeneration = generation
	valid := durableGapCheckpointRecord(checkpoint, delivery.Position+1, "signed-main-cursor-2", checkpoint.CheckpointCoverage)
	require.NotEqual(t, delivery.PredecessorPosition, valid.CoveragePosition)
	require.True(t, checkpointRecordValid(valid, delivery, 0, delivery.Events[0].ParentHash), "artifact-aligned coverage remains valid across interleaved stream positions")
	require.Empty(t, valid.Event.ParentHash, "checkpoint predecessor may be empty independently of alignment")
	require.Equal(t, delivery.Events[0].ParentHash, valid.Event.CheckpointAlignmentHash)
	withIndependentPredecessor := *valid
	withIndependentPredecessor.Event.ParentHash = strings.Repeat("f", 64)
	require.True(t, checkpointRecordValid(&withIndependentPredecessor, delivery, 0, delivery.Events[0].ParentHash),
		"missing-parent selection compares alignment, never the checkpoint event predecessor")

	for name, mutate := range map[string]func(*proto.RemoteRecoveryEventV1){
		"unrelated checkpoint alignment": func(record *proto.RemoteRecoveryEventV1) {
			record.Event.CheckpointAlignmentHash = strings.Repeat("c", 64)
		},
		"missing checkpoint alignment":     func(record *proto.RemoteRecoveryEventV1) { record.Event.CheckpointAlignmentHash = "" },
		"checkpoint sent on live lane":     func(record *proto.RemoteRecoveryEventV1) { record.Event.Lane = syncd.LaneLive },
		"coverage position differs":        func(record *proto.RemoteRecoveryEventV1) { record.Event.CheckpointCoverage++ },
		"coverage cursor digest differs":   func(record *proto.RemoteRecoveryEventV1) { record.CoverageCursorDigest = strings.Repeat("f", 64) },
		"checkpoint event identity absent": func(record *proto.RemoteRecoveryEventV1) { record.Event.EventID = "" },
		"generation differs":               func(record *proto.RemoteRecoveryEventV1) { record.Event.CheckpointGeneration = strings.Repeat("f", 64) },
		"kind differs":                     func(record *proto.RemoteRecoveryEventV1) { record.Event.Kind = "skill" },
		"branch differs":                   func(record *proto.RemoteRecoveryEventV1) { record.Event.BranchID = "other-branch" },
	} {
		t.Run(name, func(t *testing.T) {
			changed := *valid
			mutate(&changed)
			require.False(t, checkpointRecordValid(&changed, delivery, 0, delivery.Events[0].ParentHash))
		})
	}

	producerCurrent := *valid
	producerCurrent.Event.CheckpointAlignmentHash = strings.Repeat("c", 64)
	producerCurrent.CoverageCursor = delivery.Cursor
	producerCurrent.CoverageCursorDigest = delivery.CursorDigest
	producerCurrent.CoveragePosition = delivery.Position
	producerCurrent.Event.CheckpointCoverage = delivery.Position
	producerCurrent.Position = delivery.Position + 1
	producerCurrent.PredecessorPosition = delivery.Position
	require.True(t, checkpointRecordValid(&producerCurrent, delivery, 0, delivery.Events[0].ParentHash),
		"a current full checkpoint is compatible when authenticated coverage reaches the blocked event")
}

func seedDurableGapCursorAtPredecessor(t *testing.T, cursors *daemon.DurableCursorStore, binding *durableInboundCursorBinding) {
	t.Helper()
	var current *daemon.DurableCursorState
	for position := uint64(1); position <= binding.predecessor.Position; position++ {
		cursor := "signed-main-cursor-" + fmt.Sprint(position)
		if position == binding.predecessor.Position {
			cursor = binding.predecessor.Cursor
		}
		digest := sha256.Sum256([]byte(cursor))
		next := daemon.DurableCursorState{Cursor: cursor, CursorDigest: hex.EncodeToString(digest[:]), Position: position}
		persisted, err := cursors.CompareAndSwap(binding.key, current, next)
		require.NoError(t, err)
		current = &persisted
	}
}

func producerCurrentCheckpoint(t *testing.T, delivery proto.RemoteInboundDeliveryV2, coverage uint64) *proto.RemoteRecoveryEventV1 {
	t.Helper()
	return producerCurrentCheckpointForEvent(t, delivery.Events[0], coverage)
}

func producerCurrentCheckpointForEvent(t *testing.T, event proto.RemoteEvent, coverage uint64) *proto.RemoteRecoveryEventV1 {
	t.Helper()
	generation, err := durableCheckpointGeneration(event)
	require.NoError(t, err)
	checkpoint := event
	checkpoint.EventID = "event-producer-current-checkpoint"
	checkpoint.EventHash = strings.Repeat("f", 64)
	checkpoint.ParentHash = strings.Repeat("c", 64)
	checkpoint.Bytes, checkpoint.BodyDigest = durableGapTestBody("producer-current-checkpoint")
	checkpoint.Clear = false
	checkpoint.CheckpointCoverage = coverage
	checkpoint.CheckpointGeneration = generation
	return durableGapCheckpointRecord(checkpoint, coverage+1, "signed-main-cursor-"+fmt.Sprint(coverage), coverage)
}

func TestRecoverDurableInboundGapAcceptsProducerCurrentCompatibleCoverage(t *testing.T) {
	delivery, binding := durableGapDeliveryForTest()
	checkpoint := producerCurrentCheckpoint(t, delivery, delivery.Position+2)
	remote := &durableGapRemoteStub{
		parents: map[string]proto.RemoteFetchParentV1Result{
			delivery.Events[0].ParentHash: {Found: false, CheckpointRequired: true, ReasonCode: "compacted"},
		},
		checkpoint: proto.RemoteRequestCheckpointV1Result{Requested: true, RequestID: "request-current", Checkpoint: checkpoint},
	}
	cursors := &daemon.DurableCursorStore{Root: filepath.Join(t.TempDir(), "cursors")}
	seedDurableGapCursorAtPredecessor(t, cursors, binding)
	checkpointImports, coveredDeltaImports := 0, 0
	var recoveryEvidence durableGapRecoveryEvidence
	results, err := recoverDurableInboundGap(
		context.Background(), &daemon.DurableGapStore{Root: filepath.Join(t.TempDir(), "gaps")}, remote, cursors,
		func(events []proto.RemoteEvent) []syncd.ImportOutcome {
			require.Len(t, events, 1)
			if events[0].CheckpointCoverage != 0 {
				checkpointImports++
				return []syncd.ImportOutcome{syncd.ImportApplied}
			}
			coveredDeltaImports++
			return []syncd.ImportOutcome{syncd.ImportDeferredNeedsBaseline}
		}, binding, delivery, []syncd.ImportOutcome{syncd.ImportDeferredNeedsBaseline}, &recoveryEvidence,
	)
	require.NoError(t, err)
	require.Equal(t, []syncd.ImportOutcome{syncd.ImportDeduped}, results)
	require.Equal(t, 1, checkpointImports)
	require.Zero(t, coveredDeltaImports, "the current full checkpoint supersedes the covered missing-parent delta")
	require.Equal(t, delivery.Position, remote.checkpointParams[0].MinimumCoverage)
	persisted, err := cursors.Load(binding.key)
	require.NoError(t, err)
	require.Equal(t, delivery.PredecessorPosition, persisted.Position, "artifact coverage must never skip the global stream cursor")
	plan, digest, err := durableCheckpointCoveragePlan(delivery, &recoveryEvidence)
	require.NoError(t, err)
	entries, err := proto.DecodeRemoteCheckpointCoveragePlan(plan, digest)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, delivery.Position, entries[0].BlockedPosition)
	require.Equal(t, checkpoint.Event.EventID, entries[0].CheckpointEventID)
	finalize := durableInboundFinalizeEvidence("device-1", delivery, syncd.InboundCanonicalEvidence{
		FinalizeKind: proto.InboundFinalizeCanonicalMaterialize, Kind: "memory", ArtifactID: delivery.Events[0].ArtifactID,
		EventID: "canonical-current-checkpoint", EventHash: strings.Repeat("9", 64),
	})
	finalize.FinalizeKind = proto.InboundFinalizeCheckpointCovered
	finalize.CheckpointCoveragePlan, finalize.CheckpointCoverageDigest = plan, digest
	require.True(t, validDurableInboundFinalizeEvidence(finalize))
	finalize.CheckpointCoverageDigest = strings.Repeat("0", 64)
	require.False(t, validDurableInboundFinalizeEvidence(finalize), "a substituted coverage proof must fail before cursor advancement")
}

func TestProducerCurrentCheckpointCrashRetryIsIdempotent(t *testing.T) {
	delivery, binding := durableGapDeliveryForTest()
	checkpoint := producerCurrentCheckpoint(t, delivery, delivery.Position+1)
	remote := &durableGapRemoteStub{
		parents:    map[string]proto.RemoteFetchParentV1Result{delivery.Events[0].ParentHash: {Found: false, CheckpointRequired: true}},
		checkpoint: proto.RemoteRequestCheckpointV1Result{Requested: true, RequestID: "request-crash", Checkpoint: checkpoint},
	}
	root := filepath.Join(t.TempDir(), "gaps")
	cursors := &daemon.DurableCursorStore{Root: filepath.Join(t.TempDir(), "cursors")}
	seedDurableGapCursorAtPredecessor(t, cursors, binding)
	checkpointDurable := false
	checkpointCalls, staleDeltaCalls := 0, 0
	importer := func(events []proto.RemoteEvent) []syncd.ImportOutcome {
		require.Len(t, events, 1)
		if events[0].CheckpointCoverage == 0 {
			staleDeltaCalls++
			return []syncd.ImportOutcome{syncd.ImportDeferredNeedsBaseline}
		}
		checkpointCalls++
		if checkpointDurable {
			return []syncd.ImportOutcome{syncd.ImportDeduped}
		}
		checkpointDurable = true
		return []syncd.ImportOutcome{syncd.ImportApplied}
	}
	crash := errors.New("crash before durable gap removal")
	results, err := recoverDurableInboundGap(context.Background(), &durableGapResolveFailureSpool{store: &daemon.DurableGapStore{Root: root}, err: crash}, remote, cursors, importer, binding, delivery, []syncd.ImportOutcome{syncd.ImportDeferredNeedsBaseline})
	require.ErrorIs(t, err, crash)
	require.Equal(t, durableGapRetryableResults(), results)
	key, keyErr := durableGapKey(binding)
	require.NoError(t, keyErr)
	_, loadErr := (&daemon.DurableGapStore{Root: root}).Load(key)
	require.NoError(t, loadErr, "crash must retain exact encrypted retry authority")

	results, err = recoverDurableInboundGap(context.Background(), &daemon.DurableGapStore{Root: root}, remote, cursors, importer, binding, delivery, []syncd.ImportOutcome{syncd.ImportDeferredNeedsBaseline})
	require.NoError(t, err)
	require.Equal(t, []syncd.ImportOutcome{syncd.ImportDeduped}, results)
	require.Equal(t, 2, checkpointCalls, "restart reuses the exact checkpoint and canonical dedupe")
	require.Zero(t, staleDeltaCalls)
	_, loadErr = (&daemon.DurableGapStore{Root: root}).Load(key)
	require.ErrorIs(t, loadErr, daemon.ErrDurableGapNotFound)
}

func TestProducerCurrentCheckpointDoesNotReplayCoveredPreRedactionContent(t *testing.T) {
	delivery, binding := durableGapDeliveryForTest()
	delivery.Events[0].Type = "update"
	delivery.Events[0].Bytes, delivery.Events[0].BodyDigest = durableGapTestBody("pre-redaction-secret")
	checkpoint := producerCurrentCheckpoint(t, delivery, delivery.Position+3)
	remote := &durableGapRemoteStub{
		parents:    map[string]proto.RemoteFetchParentV1Result{delivery.Events[0].ParentHash: {Found: false, CheckpointRequired: true}},
		checkpoint: proto.RemoteRequestCheckpointV1Result{Requested: true, RequestID: "request-redaction", Checkpoint: checkpoint},
	}
	cursors := &daemon.DurableCursorStore{Root: filepath.Join(t.TempDir(), "cursors")}
	seedDurableGapCursorAtPredecessor(t, cursors, binding)
	var imported [][]proto.RemoteEvent
	results, err := recoverDurableInboundGap(context.Background(), &daemon.DurableGapStore{Root: filepath.Join(t.TempDir(), "gaps")}, remote, cursors, func(events []proto.RemoteEvent) []syncd.ImportOutcome {
		imported = append(imported, append([]proto.RemoteEvent(nil), events...))
		return []syncd.ImportOutcome{syncd.ImportApplied}
	}, binding, delivery, []syncd.ImportOutcome{syncd.ImportDeferredNeedsBaseline})
	require.NoError(t, err)
	require.Equal(t, []syncd.ImportOutcome{syncd.ImportDeduped}, results)
	require.Len(t, imported, 1)
	require.NotZero(t, imported[0][0].CheckpointCoverage)
	require.NotEqual(t, delivery.Events[0].EventID, imported[0][0].EventID,
		"covered pre-redaction content must never be reopened after the newer baseline is durable")
}

func TestProducerCurrentCheckpointPreservesUnrelatedInterleavedBatchEvent(t *testing.T) {
	delivery, binding := durableGapBatchDeliveryForTest(t)
	selected := delivery.Events[1]
	checkpoint := producerCurrentCheckpointForEvent(t, selected, delivery.Position+2)
	remote := &durableGapRemoteStub{
		parents: map[string]proto.RemoteFetchParentV1Result{
			selected.ParentHash: {Found: false, CheckpointRequired: true, ReasonCode: "compacted"},
		},
		checkpoint: proto.RemoteRequestCheckpointV1Result{Requested: true, RequestID: "request-current-batch", Checkpoint: checkpoint},
	}
	cursors := &daemon.DurableCursorStore{Root: filepath.Join(t.TempDir(), "cursors")}
	seedDurableGapCursorAtPredecessor(t, cursors, binding)
	checkpointImports, uncoveredImports, selectedDeltaImports := 0, 0, 0
	var recoveryEvidence durableGapRecoveryEvidence
	results, err := recoverDurableInboundGap(
		context.Background(), &daemon.DurableGapStore{Root: filepath.Join(t.TempDir(), "gaps")}, remote, cursors,
		func(events []proto.RemoteEvent) []syncd.ImportOutcome {
			require.Len(t, events, 1)
			switch {
			case events[0].CheckpointCoverage != 0:
				checkpointImports++
				return []syncd.ImportOutcome{syncd.ImportApplied}
			case events[0].EventID == delivery.Events[0].EventID:
				uncoveredImports++
				return []syncd.ImportOutcome{syncd.ImportDeduped}
			default:
				selectedDeltaImports++
				return []syncd.ImportOutcome{syncd.ImportDeferredNeedsBaseline}
			}
		}, binding, delivery, []syncd.ImportOutcome{syncd.ImportDeduped, syncd.ImportDeferredNeedsBaseline}, &recoveryEvidence,
	)
	require.NoError(t, err)
	require.Equal(t, []syncd.ImportOutcome{syncd.ImportDeduped, syncd.ImportDeduped}, results)
	require.Equal(t, 1, checkpointImports)
	require.Equal(t, 1, uncoveredImports, "an interleaved event for another artifact must still be replayed")
	require.Zero(t, selectedDeltaImports, "the covered missing-parent delta must not be reopened")
	require.Equal(t, delivery.PredecessorPosition+2, remote.checkpointParams[0].MinimumCoverage)
	persisted, err := cursors.Load(binding.key)
	require.NoError(t, err)
	require.Equal(t, delivery.PredecessorPosition, persisted.Position, "artifact recovery must not skip the interleaved global stream")
	plan, digest, err := durableCheckpointCoveragePlan(delivery, &recoveryEvidence)
	require.NoError(t, err)
	entries, err := proto.DecodeRemoteCheckpointCoveragePlan(plan, digest)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, uint32(1), entries[0].Index)
	require.Equal(t, selected.ArtifactID, entries[0].ArtifactID)
}

func TestBatchCheckpointRequestIdentityBindsMissingEventSelector(t *testing.T) {
	delivery, binding := durableGapBatchDeliveryForTest(t)
	key, err := durableGapKey(binding)
	require.NoError(t, err)
	first := durableCheckpointRequestID(key, delivery.DeliveryID, len(delivery.Events), 0, strings.Repeat("a", 64))
	second := durableCheckpointRequestID(key, delivery.DeliveryID, len(delivery.Events), 1, strings.Repeat("a", 64))
	differentHash := durableCheckpointRequestID(key, delivery.DeliveryID, len(delivery.Events), 1, strings.Repeat("b", 64))
	require.NotEqual(t, first, second)
	require.NotEqual(t, second, differentHash)
	require.Equal(t,
		durableCheckpointRequestID(key, delivery.DeliveryID, 1, 0, strings.Repeat("a", 64)),
		durableCheckpointRequestID(key, delivery.DeliveryID, 1, 0, strings.Repeat("b", 64)),
		"singleton request IDs stay wire-compatible with pre-batch restarts",
	)
}

func TestCheckpointBootstrapSeedsAuthenticatedCoverageAndKeepsLaterDeliveryStopped(t *testing.T) {
	delivery, binding := durableGapDeliveryForTest()
	generation, err := durableCheckpointGeneration(delivery.Events[0])
	require.NoError(t, err)
	checkpoint := durableGapParentEvent(strings.Repeat("e", 64), delivery.Events[0].ParentHash, "checkpoint")
	checkpoint.Kind = delivery.Events[0].Kind
	checkpoint.CheckpointCoverage = 2
	checkpoint.CheckpointGeneration = generation
	remote := &durableGapRemoteStub{
		parents: map[string]proto.RemoteFetchParentV1Result{
			delivery.Events[0].ParentHash: {Found: false, CheckpointRequired: true, ReasonCode: "compacted"},
		},
		checkpoint: proto.RemoteRequestCheckpointV1Result{Requested: true, RequestID: "request-bootstrap", Checkpoint: durableGapCheckpointRecord(checkpoint, 6, "signed-main-cursor-2", 2)},
	}
	spool := &daemon.DurableGapStore{Root: filepath.Join(t.TempDir(), "gaps")}
	cursors := &daemon.DurableCursorStore{Root: filepath.Join(t.TempDir(), "cursors")}
	originalReplayed := false
	results, err := recoverDurableInboundGap(context.Background(), spool, remote, cursors, func(events []proto.RemoteEvent) []syncd.ImportOutcome {
		if events[0].CheckpointCoverage > 0 {
			return []syncd.ImportOutcome{syncd.ImportApplied}
		}
		originalReplayed = true
		return []syncd.ImportOutcome{syncd.ImportApplied}
	}, binding, delivery, []syncd.ImportOutcome{syncd.ImportDeferredNeedsBaseline})
	require.ErrorIs(t, err, errDurableGapPending)
	require.Equal(t, durableGapRetryableResults(), results)
	require.False(t, originalReplayed, "a later interleaved delivery cannot skip directly past checkpoint coverage")
	seeded, err := cursors.Load(binding.key)
	require.NoError(t, err)
	require.Equal(t, "signed-main-cursor-2", seeded.Cursor)
	require.Equal(t, uint64(2), seeded.Position)
	key, keyErr := durableGapKey(binding)
	require.NoError(t, keyErr)
	_, loadErr := spool.Load(key)
	require.NoError(t, loadErr, "the later blocked delivery remains recoverable after bootstrap")
}

func TestRecoverDurableInboundGapKeepsCursorStoppedWhileCheckpointPending(t *testing.T) {
	delivery, binding := durableGapDeliveryForTest()
	remote := &durableGapRemoteStub{
		parents:    map[string]proto.RemoteFetchParentV1Result{delivery.Events[0].ParentHash: {Found: false, CheckpointRequired: true}},
		checkpoint: proto.RemoteRequestCheckpointV1Result{Requested: true, Pending: true, RequestID: "request-pending"},
	}
	spool := &daemon.DurableGapStore{Root: filepath.Join(t.TempDir(), "gaps")}
	results, err := recoverDurableInboundGap(context.Background(), spool, remote, nil, func([]proto.RemoteEvent) []syncd.ImportOutcome {
		return []syncd.ImportOutcome{syncd.ImportDeferredNeedsBaseline}
	}, binding, delivery, []syncd.ImportOutcome{syncd.ImportDeferredNeedsBaseline})
	require.ErrorIs(t, err, errDurableGapPending)
	require.Equal(t, []syncd.ImportOutcome{syncd.ImportDeferredNeedsBaseline}, results)
	ack, terminal := inboundV2AckFromResults(delivery, results, true)
	require.False(t, terminal)
	require.Equal(t, "missing-parent", ack.Outcomes[0].ReasonCode)
	key, keyErr := durableGapKey(binding)
	require.NoError(t, keyErr)
	_, loadErr := spool.Load(key)
	require.NoError(t, loadErr)
}

func TestCheckpointRestoreFailureEvidenceRequiresRealReturnedCheckpointFailure(t *testing.T) {
	delivery, binding := durableGapDeliveryForTest()
	spool := &daemon.DurableGapStore{Root: filepath.Join(t.TempDir(), "gaps")}
	parentMissing := map[string]proto.RemoteFetchParentV1Result{
		delivery.Events[0].ParentHash: {Found: false, CheckpointRequired: true},
	}

	t.Run("pending without checkpoint is not a restore failure", func(t *testing.T) {
		evidence := &durableGapRecoveryEvidence{}
		remote := &durableGapRemoteStub{parents: parentMissing, checkpoint: proto.RemoteRequestCheckpointV1Result{Requested: true, Pending: true}}
		_, err := recoverDurableInboundGap(context.Background(), spool, remote, nil, func([]proto.RemoteEvent) []syncd.ImportOutcome {
			return []syncd.ImportOutcome{syncd.ImportDeferredNeedsBaseline}
		}, binding, delivery, []syncd.ImportOutcome{syncd.ImportDeferredNeedsBaseline}, evidence)
		require.ErrorIs(t, err, errDurableGapPending)
		require.False(t, evidence.checkpointRestoreFailed)
	})

	t.Run("invalid returned checkpoint is a restore failure", func(t *testing.T) {
		evidence := &durableGapRecoveryEvidence{}
		remote := &durableGapRemoteStub{parents: parentMissing, checkpoint: proto.RemoteRequestCheckpointV1Result{
			Requested: true, Checkpoint: &proto.RemoteRecoveryEventV1{},
		}}
		_, err := recoverDurableInboundGap(context.Background(), &daemon.DurableGapStore{Root: filepath.Join(t.TempDir(), "gaps")}, remote, nil, func([]proto.RemoteEvent) []syncd.ImportOutcome {
			t.Fatal("invalid checkpoint must fail before canonical application")
			return nil
		}, binding, delivery, []syncd.ImportOutcome{syncd.ImportDeferredNeedsBaseline}, evidence)
		require.ErrorIs(t, err, errDurableGapMalformed)
		require.True(t, evidence.checkpointRestoreFailed)
	})

	t.Run("canonical application failure after valid checkpoint is a restore failure", func(t *testing.T) {
		generation, err := durableCheckpointGeneration(delivery.Events[0])
		require.NoError(t, err)
		checkpoint := durableGapParentEvent(strings.Repeat("e", 64), delivery.Events[0].ParentHash, "failed-application")
		checkpoint.Kind = delivery.Events[0].Kind
		checkpoint.CheckpointCoverage = 2
		checkpoint.CheckpointGeneration = generation
		record := durableGapCheckpointRecord(checkpoint, 6, "signed-main-cursor-2", 2)
		evidence := &durableGapRecoveryEvidence{}
		remote := &durableGapRemoteStub{parents: parentMissing, checkpoint: proto.RemoteRequestCheckpointV1Result{Requested: true, Checkpoint: record}}
		_, err = recoverDurableInboundGap(context.Background(), &daemon.DurableGapStore{Root: filepath.Join(t.TempDir(), "gaps")}, remote, nil, func([]proto.RemoteEvent) []syncd.ImportOutcome {
			return []syncd.ImportOutcome{syncd.ImportRetryable}
		}, binding, delivery, []syncd.ImportOutcome{syncd.ImportDeferredNeedsBaseline}, evidence)
		require.ErrorIs(t, err, errDurableGapPending)
		require.True(t, evidence.checkpointRestoreFailed)
	})
}
