package daemon

import (
	"context"
	"errors"
	"strings"

	"github.com/aplexica/aplexica/internal/plugin/proto"
	"github.com/aplexica/aplexica/internal/securityepoch"
)

func validCheckpointRequestReason(reason string) bool {
	switch reason {
	case "bootstrap", "new-recipient", "rekey", "compaction", "cursor-expired", "missing-parent", "divergence", "redaction", "disaster-recovery", "rollback":
		return true
	default:
		return false
	}
}

func checkpointNotificationGeneration(notification proto.RemoteCheckpointNeededV1Notification) string {
	return checkpointGenerationForEvent(proto.RemoteEvent{
		AccessGeneration: notification.AccessGeneration, AccessSetHash: notification.AccessSetHash,
		SecurityGeneration: notification.SecurityGeneration, SecurityBarrierID: notification.SecurityBarrierID,
		KeyMode: notification.KeyMode, KeyVersion: notification.KeyVersion,
	})
}

func validCheckpointNeededNotification(notification proto.RemoteCheckpointNeededV1Notification) bool {
	return validObligationOpaque(notification.RequestID, 512) &&
		validObligationOpaque(notification.RequestingDeviceID, 512) &&
		validObligationOpaque(notification.StreamID, 512) && validObligationOpaque(notification.StreamEpoch, 512) &&
		(notification.NamespaceID == "" || validObligationOpaque(notification.NamespaceID, 256)) &&
		validObligationOpaque(notification.ArtifactID, 256) && validObligationOpaque(notification.BranchID, 128) &&
		validObligationOpaque(notification.Kind, 64) && validCheckpointRequestReason(notification.Reason) &&
		notification.CheckpointCoverage > 0 && validateWatermarkDigest(strings.ToLower(notification.CheckpointAlignmentHash)) &&
		validateWatermarkDigest(strings.ToLower(notification.CheckpointGeneration)) &&
		strings.EqualFold(notification.CheckpointGeneration, checkpointNotificationGeneration(notification)) &&
		(notification.MinAvailableCursor == "" || validObligationOpaque(notification.MinAvailableCursor, 4096)) &&
		(notification.Reason == "missing-parent" && validateWatermarkDigest(strings.ToLower(notification.MissingParentHash)) ||
			notification.Reason != "missing-parent" && notification.MissingParentHash == "")
}

// HandleCheckpointNeededV1 admits one exact cloud/bootstrap request into the
// crash-safe local fulfillment path. The caller wires this method to
// RemoteRunner.OnCheckpointNeededV1; it intentionally contains no CLI wiring.
func (a *RemotePublishAdapter) HandleCheckpointNeededV1(notification proto.RemoteCheckpointNeededV1Notification) error {
	if a == nil || a.outbox == nil || a.outbox.mutations == nil || a.checkpointObligations == nil || !validCheckpointNeededNotification(notification) {
		return errors.New("remote checkpoint request: invalid notification")
	}
	policy, ok := a.client.(remoteOutboundRecoveryPolicy)
	if !ok {
		return errors.New("remote checkpoint request: negotiation unavailable")
	}
	negotiated := policy.SyncNegotiation()
	if negotiated.SelectedProtocol != 1 || !negotiated.FeatureGateEnabled || negotiated.Mode == proto.RemoteSyncModeLegacy {
		return errors.New("remote checkpoint request: inactive negotiation")
	}
	scopeID := notification.NamespaceID
	if scopeID == "" {
		scopeID = "account"
	}
	descriptor, exists := recoveryDescriptorForScope(negotiated, scopeID)
	if !exists || descriptor.StreamID != notification.StreamID || descriptor.StreamEpoch != notification.StreamEpoch || descriptor.NamespaceID != notification.NamespaceID {
		return errors.New("remote checkpoint request: stream generation mismatch")
	}

	a.authorizerMu.RLock()
	coordinator := a.epochCoordinator
	a.authorizerMu.RUnlock()
	if coordinator == nil {
		return errors.New("remote checkpoint request: security generation unavailable")
	}
	lease, err := coordinator.AcquirePublish(context.Background(), scopeID, securityepoch.SecurityEpoch{
		CoordinatorGeneration: notification.SecurityGeneration,
		AccessGeneration:      notification.AccessGeneration, AccessSetHash: notification.AccessSetHash,
		BarrierID: notification.SecurityBarrierID, KeyMode: notification.KeyMode, KeyVersion: notification.KeyVersion,
	})
	if err != nil {
		return errors.New("remote checkpoint request: stale security generation")
	}
	if err := lease.Close(); err != nil {
		return errors.New("remote checkpoint request: security generation release failed")
	}

	record := RemoteCheckpointObligationV1{
		ScopeID: scopeID, ArtifactID: notification.ArtifactID, BranchID: notification.BranchID, Kind: notification.Kind,
		HeadEventHash:    strings.ToLower(notification.CheckpointAlignmentHash),
		AccessGeneration: notification.AccessGeneration, AccessSetHash: strings.ToLower(hexDigest(notification.AccessSetHash)),
		SecurityGeneration: notification.SecurityGeneration, SecurityBarrier: strings.ToLower(hexDigest(notification.SecurityBarrierID)),
		KeyMode: notification.KeyMode, KeyVersion: notification.KeyVersion, Reason: notification.Reason,
		RequestID: notification.RequestID, RequestingDeviceID: notification.RequestingDeviceID,
		RequestStreamID: notification.StreamID, RequestStreamEpoch: notification.StreamEpoch,
		RequestCoverage:             notification.CheckpointCoverage,
		RequestAlignmentHash:        strings.ToLower(notification.CheckpointAlignmentHash),
		RequestCheckpointGeneration: strings.ToLower(notification.CheckpointGeneration),
		MissingParentHash:           strings.ToLower(notification.MissingParentHash),
	}
	if err := a.checkpointObligations.Put(record); err != nil {
		return err
	}
	if err := a.outbox.mutations.RequireCheckpoint(scopeID); err != nil {
		return err
	}
	a.wakeOutboundRecovery()
	return nil
}

func hexDigest(value [32]byte) string {
	const digits = "0123456789abcdef"
	encoded := make([]byte, len(value)*2)
	for i, item := range value {
		encoded[i*2] = digits[item>>4]
		encoded[i*2+1] = digits[item&0x0f]
	}
	return string(encoded)
}
