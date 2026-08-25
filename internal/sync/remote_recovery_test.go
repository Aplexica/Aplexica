package syncd

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/identity"
	"github.com/stretchr/testify/require"
)

func recoveryGenerationForTest(roster identity.VerifiedRoster, barrier [32]byte) RemoteRecoveryGeneration {
	return RemoteRecoveryGeneration{
		AccessGeneration:   roster.Manifest.Manifest.AccessGeneration,
		AccessSetHash:      roster.Manifest.Manifest.AccessSetHash,
		SecurityGeneration: 1,
		SecurityBarrierID:  barrier,
		KeyMode:            "recipient-wrap-v2",
	}
}

func appendRecoveryMemoryUpdate(t *testing.T, store *acf.Store, artifactID, parentHash, deviceID, content string) acf.Event {
	t.Helper()
	payload, err := json.Marshal(acf.MemoryPayload{Format: "markdown", Content: content})
	require.NoError(t, err)
	event := acf.Event{
		EventID: acf.NewID(), ArtifactID: artifactID, Type: acf.EventTypeUpdate,
		Timestamp:  time.Now().UTC().Add(time.Second),
		Provenance: acf.Provenance{DeviceID: deviceID, SourceAgent: "codex"},
		Payload:    payload, ParentHash: parentHash,
	}
	require.NoError(t, store.AppendEvent(acf.KindMemory, event))
	events, err := store.ReadEvents(acf.KindMemory, artifactID)
	require.NoError(t, err)
	return events[len(events)-1]
}

func TestRebuildRemoteOutboundReplaysCanonicalLocalRangeThenCompletesFromExactAnchor(t *testing.T) {
	roster, device := signedTestRoster(t)
	var barrier [32]byte
	barrier[0] = 7
	orchestrator, store := newV2InboundOrchestratorForTest(t, roster, device, barrier, "device-a")
	artifactID, genesis := seedArtifact(t, store, acf.KindMemory, "device-a")
	update := appendRecoveryMemoryUpdate(t, store, artifactID, genesis.Hash, "device-a", "second")
	generation := recoveryGenerationForTest(roster, barrier)
	request := RemoteRecoveryRequest{
		ScopeID: "account", Generation: generation,
		Target: RemoteRecoveryTarget{ArtifactID: artifactID, BranchID: "main", EventID: update.EventID, EventHash: update.Hash},
	}

	var rebuilt []OutboundEvent
	result, err := orchestrator.RebuildRemoteOutbound(context.Background(), request, func(event OutboundEvent) error {
		rebuilt = append(rebuilt, event)
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, 2, result.Rebuilt)
	require.False(t, result.Complete, "a pass that appends needs a later durable watermark proof")
	require.Empty(t, result.Obligations)
	require.Equal(t, []string{genesis.EventID, update.EventID}, []string{rebuilt[0].EventID, rebuilt[1].EventID})
	require.Equal(t, []uint64{1, 2}, []uint64{rebuilt[0].Sequence, rebuilt[1].Sequence})
	require.Equal(t, generation.SecurityGeneration, rebuilt[1].SecurityGeneration)
	require.NotEmpty(t, rebuilt[0].Bytes)

	request.Anchors = []RemoteRecoveryAnchor{{
		ArtifactID: artifactID, BranchID: "main", CanonicalEventID: update.EventID,
		CanonicalEventHash: update.Hash, Generation: generation,
	}}
	result, err = orchestrator.RebuildRemoteOutbound(context.Background(), request, func(OutboundEvent) error {
		t.Fatal("an exact head anchor must not rebuild anything")
		return nil
	})
	require.NoError(t, err)
	require.True(t, result.Complete)
	require.Zero(t, result.Rebuilt)
}

func TestRebuildRemoteOutboundRestartAfterPartialAppendReplaysWholeUnprovenRange(t *testing.T) {
	roster, device := signedTestRoster(t)
	var barrier [32]byte
	barrier[0] = 8
	orchestrator, store := newV2InboundOrchestratorForTest(t, roster, device, barrier, "device-a")
	artifactID, genesis := seedArtifact(t, store, acf.KindMemory, "device-a")
	update := appendRecoveryMemoryUpdate(t, store, artifactID, genesis.Hash, "device-a", "second")
	request := RemoteRecoveryRequest{
		ScopeID: "account", Generation: recoveryGenerationForTest(roster, barrier),
		Target: RemoteRecoveryTarget{ArtifactID: artifactID, BranchID: "main", EventID: update.EventID, EventHash: update.Hash},
	}
	crash := errors.New("simulated crash after durable append")
	calls := 0
	_, err := orchestrator.RebuildRemoteOutbound(context.Background(), request, func(OutboundEvent) error {
		calls++
		return crash
	})
	require.ErrorIs(t, err, crash)
	require.Equal(t, 1, calls)

	var replayed []string
	result, err := orchestrator.RebuildRemoteOutbound(context.Background(), request, func(event OutboundEvent) error {
		replayed = append(replayed, event.EventID)
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, []string{genesis.EventID, update.EventID}, replayed)
	require.Equal(t, 2, result.Rebuilt)
}

func TestRebuildRemoteOutboundRequiresCheckpointForUnprovenOrMissingHistory(t *testing.T) {
	roster, device := signedTestRoster(t)
	var barrier [32]byte
	barrier[0] = 9
	orchestrator, store := newV2InboundOrchestratorForTest(t, roster, device, barrier, "device-a")
	artifactID, remoteGenesis := seedArtifact(t, store, acf.KindMemory, "peer-device")
	local := appendRecoveryMemoryUpdate(t, store, artifactID, remoteGenesis.Hash, "device-a", "local child")
	generation := recoveryGenerationForTest(roster, barrier)
	request := RemoteRecoveryRequest{
		ScopeID: "account", Generation: generation,
		Target: RemoteRecoveryTarget{ArtifactID: artifactID, BranchID: "main", EventID: local.EventID, EventHash: local.Hash},
	}
	result, err := orchestrator.RebuildRemoteOutbound(context.Background(), request, func(OutboundEvent) error {
		t.Fatal("a local child of an unproven remote parent must not be synthesized")
		return nil
	})
	require.NoError(t, err)
	require.False(t, result.Complete)
	require.NotEmpty(t, result.Obligations)
	require.Equal(t, "canonical-parent-unavailable", result.Obligations[0].Reason)

	request.Anchors = []RemoteRecoveryAnchor{{
		ArtifactID: artifactID, BranchID: "main", CanonicalEventID: acf.NewID(),
		CanonicalEventHash: local.Hash, Generation: generation,
	}}
	result, err = orchestrator.RebuildRemoteOutbound(context.Background(), request, func(OutboundEvent) error { return nil })
	require.NoError(t, err)
	require.Contains(t, func() []string {
		var reasons []string
		for _, obligation := range result.Obligations {
			reasons = append(reasons, obligation.Reason)
		}
		return reasons
	}(), "durable-anchor-compacted-or-missing")
}

func TestRebuildRemoteOutboundRejectsStaleGenerationAndCancellation(t *testing.T) {
	roster, device := signedTestRoster(t)
	var barrier [32]byte
	barrier[0] = 10
	orchestrator, store := newV2InboundOrchestratorForTest(t, roster, device, barrier, "device-a")
	artifactID, event := seedArtifact(t, store, acf.KindMemory, "device-a")
	generation := recoveryGenerationForTest(roster, barrier)
	generation.SecurityGeneration++
	request := RemoteRecoveryRequest{
		ScopeID: "account", Generation: generation,
		Target: RemoteRecoveryTarget{ArtifactID: artifactID, BranchID: "main", EventID: event.EventID, EventHash: event.Hash},
	}
	result, err := orchestrator.RebuildRemoteOutbound(context.Background(), request, func(OutboundEvent) error { return nil })
	require.NoError(t, err)
	require.NotEmpty(t, result.Obligations)
	require.Equal(t, "generation-or-seal-authority-unavailable", result.Obligations[0].Reason)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = orchestrator.RebuildRemoteOutbound(ctx, request, func(OutboundEvent) error { return nil })
	require.ErrorIs(t, err, context.Canceled)
}

func TestRebuildRemoteOutboundPreservesSequenceAcrossCompactedActivePrefix(t *testing.T) {
	roster, device := signedTestRoster(t)
	var barrier [32]byte
	barrier[0] = 11
	orchestrator, store := newV2InboundOrchestratorForTest(t, roster, device, barrier, "device-a")
	artifactID, genesis := seedArtifact(t, store, acf.KindMemory, "device-a")
	update := appendRecoveryMemoryUpdate(t, store, artifactID, genesis.Hash, "device-a", "after compacted prefix")
	artifact, err := store.ReadArtifact(acf.KindMemory, artifactID)
	require.NoError(t, err)
	artifact.EventCount = 10 // active records represent original positions 9 and 10
	require.NoError(t, store.WriteArtifact(artifact))
	generation := recoveryGenerationForTest(roster, barrier)
	request := RemoteRecoveryRequest{
		ScopeID: "account", Generation: generation,
		Target: RemoteRecoveryTarget{ArtifactID: artifactID, BranchID: "main", EventID: update.EventID, EventHash: update.Hash},
		Anchors: []RemoteRecoveryAnchor{{
			ArtifactID: artifactID, BranchID: "main", CanonicalEventID: genesis.EventID,
			CanonicalEventHash: genesis.Hash, Generation: generation,
		}},
	}
	var got OutboundEvent
	result, err := orchestrator.RebuildRemoteOutbound(context.Background(), request, func(event OutboundEvent) error {
		got = event
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, 1, result.Rebuilt)
	require.Equal(t, uint64(10), got.Sequence)
}

func TestRebuildRemoteOutboundLegacySequencePrefixRequiresCheckpoint(t *testing.T) {
	roster, device := signedTestRoster(t)
	var barrier [32]byte
	barrier[0] = 12
	orchestrator, store := newV2InboundOrchestratorForTest(t, roster, device, barrier, "device-a")
	artifactID, genesis := seedArtifact(t, store, acf.KindMemory, "device-a")
	update := appendRecoveryMemoryUpdate(t, store, artifactID, genesis.Hash, "device-a", "new cadence")
	artifact, err := store.ReadArtifact(acf.KindMemory, artifactID)
	require.NoError(t, err)
	artifact.EventCount = 1 // genesis predates the persistent sequence cadence
	require.NoError(t, store.WriteArtifact(artifact))
	request := RemoteRecoveryRequest{
		ScopeID: "account", Generation: recoveryGenerationForTest(roster, barrier),
		Target: RemoteRecoveryTarget{ArtifactID: artifactID, BranchID: "main", EventID: update.EventID, EventHash: update.Hash},
	}
	result, err := orchestrator.RebuildRemoteOutbound(context.Background(), request, func(OutboundEvent) error {
		t.Fatal("legacy sequence prefix must not be assigned a fabricated transport position")
		return nil
	})
	require.NoError(t, err)
	require.NotEmpty(t, result.Obligations)
	require.Equal(t, "canonical-sequence-unavailable", result.Obligations[0].Reason)
}
