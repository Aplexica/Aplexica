package daemon

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/aplexica/aplexica/internal/plugin/proto"
	"github.com/aplexica/aplexica/internal/securityepoch"
	"github.com/stretchr/testify/require"
)

func checkpointNeededFixture() (proto.RemoteCheckpointNeededV1Notification, securityepoch.SecurityEpoch) {
	generation := checkpointGenerationFixture()
	epoch := securityepoch.SecurityEpoch{
		CoordinatorGeneration: generation.SecurityGeneration,
		AccessGeneration:      generation.AccessGeneration,
		AccessSetHash:         generation.AccessSetHash,
		BarrierID:             generation.SecurityBarrierID,
		KeyMode:               generation.KeyMode,
		KeyVersion:            generation.KeyVersion,
	}
	notification := proto.RemoteCheckpointNeededV1Notification{
		RequestID: "request-1", RequestingDeviceID: "device-requesting",
		StreamID: "stream-1", StreamEpoch: "epoch-1",
		BranchID: "main", ArtifactID: "artifact-1", Kind: "conversation",
		Reason: "missing-parent", MissingParentHash: watermarkTestDigest("missing-parent"),
		CheckpointCoverage: 7, CheckpointAlignmentHash: watermarkTestDigest("requested-head"),
		AccessGeneration: generation.AccessGeneration, AccessSetHash: generation.AccessSetHash,
		SecurityGeneration: generation.SecurityGeneration, SecurityBarrierID: generation.SecurityBarrierID,
		KeyMode: generation.KeyMode, KeyVersion: generation.KeyVersion,
	}
	notification.CheckpointGeneration = checkpointNotificationGeneration(notification)
	return notification, epoch
}

func checkpointRequestAdapter(t *testing.T) (*RemotePublishAdapter, *recoveryPolicyClient) {
	t.Helper()
	adapter, client := newRecoveryTestAdapter(t, proto.RemoteSyncModeShadow)
	client.negotiated.SelectedProtocol = 1
	client.negotiated.FeatureGateEnabled = true
	coordinator := &securityepoch.Coordinator{Root: filepath.Join(t.TempDir(), "security")}
	_, epoch := checkpointNeededFixture()
	require.NoError(t, coordinator.Transition(context.Background(), "account", epoch, func() error { return nil }))
	adapter.SetSecurityEpochCoordinator(coordinator)
	return adapter, client
}

func TestHandleCheckpointNeededV1PersistsExactAuthorityAndIsIdempotent(t *testing.T) {
	adapter, client := checkpointRequestAdapter(t)
	notification, _ := checkpointNeededFixture()
	require.NoError(t, adapter.HandleCheckpointNeededV1(notification))

	values, err := adapter.checkpointObligations.List()
	require.NoError(t, err)
	require.Len(t, values, 1)
	value := values[0]
	require.Equal(t, notification.RequestID, value.RequestID)
	require.Equal(t, notification.RequestingDeviceID, value.RequestingDeviceID)
	require.Equal(t, notification.StreamID, value.RequestStreamID)
	require.Equal(t, notification.StreamEpoch, value.RequestStreamEpoch)
	require.Equal(t, notification.CheckpointCoverage, value.RequestCoverage)
	require.Equal(t, notification.CheckpointAlignmentHash, value.RequestAlignmentHash)
	require.Equal(t, notification.CheckpointGeneration, value.RequestCheckpointGeneration)
	require.Equal(t, notification.MissingParentHash, value.MissingParentHash)

	marker, exists, err := adapter.outbox.mutations.Snapshot("account")
	require.NoError(t, err)
	require.True(t, exists)
	require.Equal(t, "dirty", marker.State)
	require.NotZero(t, marker.ReasonFlags&rescanReasonCheckpoint)
	generation := marker.MutationGeneration

	// The exact notification is an idempotent replay: neither the persisted
	// authority nor the marker generation is advanced a second time.
	require.NoError(t, adapter.HandleCheckpointNeededV1(notification))
	marker, exists, err = adapter.outbox.mutations.Snapshot("account")
	require.NoError(t, err)
	require.True(t, exists)
	require.Equal(t, generation, marker.MutationGeneration)

	// Before any checkpoint progress exists, a safety-poll observation of a
	// strictly newer local head may supersede the stale request without
	// changing stream or security generation authority.
	newer := notification
	newer.RequestID = "request-2"
	newer.CheckpointCoverage++
	newer.CheckpointAlignmentHash = watermarkTestDigest("newer-requested-head")
	require.NoError(t, adapter.HandleCheckpointNeededV1(newer))
	require.NoError(t, adapter.HandleCheckpointNeededV1(newer), "exact superseding request replay must be idempotent")

	// Equal/lower coverage is not a new authority and must fail closed.
	conflict := newer
	conflict.RequestID = "request-3"
	conflict.CheckpointAlignmentHash = watermarkTestDigest("conflicting-head")
	require.Error(t, adapter.HandleCheckpointNeededV1(conflict))
	require.Error(t, adapter.HandleCheckpointNeededV1(notification), "older request must not roll authority back")
	values, err = adapter.checkpointObligations.List()
	require.NoError(t, err)
	require.Len(t, values, 1)
	require.Equal(t, newer.RequestID, values[0].RequestID)
	require.Equal(t, newer.CheckpointCoverage, values[0].RequestCoverage)
	require.Equal(t, newer.CheckpointAlignmentHash, values[0].RequestAlignmentHash)

	// Stream authority is rechecked before any obligation mutation.
	client.negotiated.StreamEpoch = "epoch-2"
	require.Error(t, adapter.HandleCheckpointNeededV1(newer))

	// Restart loading authenticates the complete request binding.
	reopened := &RemoteCheckpointObligationStore{Root: adapter.checkpointObligations.Root}
	require.NoError(t, reopened.Init())
	values, err = reopened.List()
	require.NoError(t, err)
	require.Len(t, values, 1)
	require.Equal(t, newer.CheckpointGeneration, values[0].RequestCheckpointGeneration)
}

func TestHandleCheckpointNeededV1RejectsStaleGenerationAndInvalidMissingParent(t *testing.T) {
	adapter, _ := checkpointRequestAdapter(t)
	notification, _ := checkpointNeededFixture()

	stale := notification
	stale.AccessGeneration++
	stale.CheckpointGeneration = checkpointNotificationGeneration(stale)
	require.Error(t, adapter.HandleCheckpointNeededV1(stale))

	invalid := notification
	invalid.MissingParentHash = ""
	require.Error(t, adapter.HandleCheckpointNeededV1(invalid))

	values, err := adapter.checkpointObligations.List()
	require.NoError(t, err)
	require.Empty(t, values)
}
