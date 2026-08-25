package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"

	"github.com/aplexica/aplexica/internal/plugin/proto"
	syncd "github.com/aplexica/aplexica/internal/sync"
)

type remoteOutboundRecoverySource interface {
	RebuildRemoteOutbound(context.Context, syncd.RemoteRecoveryRequest, func(syncd.OutboundEvent) error) (syncd.RemoteRecoveryResult, error)
}

type remoteOutboundRecoveryPolicy interface {
	DurableReceiptRequired() bool
	SyncNegotiation() proto.RemoteNegotiateSyncV1Result
}

func recoveryDescriptorForScope(negotiated proto.RemoteNegotiateSyncV1Result, scopeID string) (proto.RemoteStreamDescriptorV1, bool) {
	namespaceID := scopeID
	if namespaceID == "account" {
		namespaceID = ""
	}
	if len(negotiated.Streams) > 0 {
		for _, descriptor := range negotiated.Streams {
			if descriptor.NamespaceID == namespaceID && descriptor.StreamID != "" && descriptor.StreamEpoch != "" {
				return descriptor, true
			}
		}
		return proto.RemoteStreamDescriptorV1{}, false
	}
	if namespaceID == "" && negotiated.StreamID != "" && negotiated.StreamEpoch != "" {
		return proto.RemoteStreamDescriptorV1{StreamID: negotiated.StreamID, StreamEpoch: negotiated.StreamEpoch}, true
	}
	return proto.RemoteStreamDescriptorV1{}, false
}

func markerRecoveryGeneration(marker RemoteRescanMarkerV1) syncd.RemoteRecoveryGeneration {
	return syncd.RemoteRecoveryGeneration{
		AccessGeneration: marker.TargetAccessGeneration, AccessSetHash: marker.TargetAccessSetHash,
		SecurityGeneration: marker.TargetSecurityGeneration, SecurityBarrierID: marker.TargetSecurityBarrierID,
		KeyMode: marker.TargetKeyMode, KeyVersion: marker.TargetKeyVersion,
	}
}

func decodeRecoveryDigest(value string) ([sha256.Size]byte, bool) {
	var result [sha256.Size]byte
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != sha256.Size {
		return result, false
	}
	copy(result[:], decoded)
	return result, true
}

func watermarkRecoveryGeneration(watermark DurablePublishWatermark) (syncd.RemoteRecoveryGeneration, bool) {
	if !watermark.HasRecoveryGeneration() {
		return syncd.RemoteRecoveryGeneration{}, false
	}
	accessHash, ok := decodeRecoveryDigest(watermark.RecipientFingerprint)
	if !ok {
		return syncd.RemoteRecoveryGeneration{}, false
	}
	barrier, ok := decodeRecoveryDigest(watermark.SecurityBarrier)
	if !ok {
		return syncd.RemoteRecoveryGeneration{}, false
	}
	return syncd.RemoteRecoveryGeneration{
		AccessGeneration: watermark.AccessGeneration, AccessSetHash: accessHash,
		SecurityGeneration: watermark.SecurityGeneration, SecurityBarrierID: barrier,
		KeyMode: watermark.KeyMode, KeyVersion: watermark.KeyVersion,
	}, true
}

func sameRecoveryGeneration(a, b syncd.RemoteRecoveryGeneration) bool { return a == b }

func targetHashString(value [sha256.Size]byte) string {
	if value == ([sha256.Size]byte{}) {
		return ""
	}
	return hex.EncodeToString(value[:])
}

func (a *RemotePublishAdapter) recoverySourceSnapshot() remoteOutboundRecoverySource {
	a.recoveryMu.RLock()
	defer a.recoveryMu.RUnlock()
	return a.recoverySource
}

func (a *RemotePublishAdapter) persistCheckpointObligation(obligation syncd.RemoteCheckpointObligation) error {
	if a.checkpointObligations == nil {
		return errors.New("remote outbound recovery: checkpoint obligation store unavailable")
	}
	record := RemoteCheckpointObligationV1{
		ScopeID: obligation.ScopeID, ArtifactID: obligation.ArtifactID, BranchID: obligation.BranchID,
		Kind: obligation.Kind, HeadEventID: obligation.HeadEventID, HeadEventHash: strings.ToLower(obligation.HeadEventHash),
		Reason: obligation.Reason,
	}
	hasAnyGeneration := obligation.Generation.AccessGeneration != 0 || obligation.Generation.AccessSetHash != ([sha256.Size]byte{}) ||
		obligation.Generation.SecurityGeneration != 0 || obligation.Generation.SecurityBarrierID != ([sha256.Size]byte{}) ||
		obligation.Generation.KeyMode != "" || obligation.Generation.KeyVersion != 0
	if hasAnyGeneration {
		if obligation.Generation.AccessGeneration == 0 || obligation.Generation.AccessSetHash == ([sha256.Size]byte{}) ||
			obligation.Generation.SecurityGeneration == 0 || obligation.Generation.SecurityBarrierID == ([sha256.Size]byte{}) ||
			!validRecoveryKeyModeVersion(obligation.Generation.KeyMode, obligation.Generation.KeyVersion) {
			return errors.New("remote outbound recovery: invalid checkpoint generation")
		}
		record.AccessGeneration = obligation.Generation.AccessGeneration
		record.AccessSetHash = hex.EncodeToString(obligation.Generation.AccessSetHash[:])
		record.SecurityGeneration = obligation.Generation.SecurityGeneration
		record.SecurityBarrier = hex.EncodeToString(obligation.Generation.SecurityBarrierID[:])
		record.KeyMode = obligation.Generation.KeyMode
		record.KeyVersion = obligation.Generation.KeyVersion
	}
	return a.checkpointObligations.Put(record)
}

func (a *RemotePublishAdapter) notifyCheckpointObligation(obligation syncd.RemoteCheckpointObligation) {
	a.notifyMu.Lock()
	notify := a.notify
	a.notifyMu.Unlock()
	if notify == nil {
		return
	}
	notify("remote.checkpoint_required", map[string]any{
		"scope_id": obligation.ScopeID, "artifact_id": obligation.ArtifactID,
		"branch_id": obligation.BranchID, "head_event_id": obligation.HeadEventID,
		"reason": obligation.Reason,
	})
}

// parkLiveForCheckpoint converts a terminal realtime condition into durable,
// content-free recovery state without deleting the exact pending ciphertext.
// The marker is written first; if obligation persistence fails, canonical
// recovery remains dirty and can retry instead of silently losing the delta.
func (a *RemotePublishAdapter) parkLiveForCheckpoint(event proto.RemoteEvent, reason string) error {
	if a == nil || a.outbox == nil {
		return errors.New("remote outbound recovery: durable outbox unavailable")
	}
	if err := a.outbox.RequireCanonicalRecovery(event); err != nil {
		return err
	}
	defer a.wakeOutboundRecovery()
	generation := syncd.RemoteRecoveryGeneration{
		AccessGeneration: event.AccessGeneration, AccessSetHash: event.AccessSetHash,
		SecurityGeneration: event.SecurityGeneration, SecurityBarrierID: event.SecurityBarrierID,
		KeyMode: event.KeyMode, KeyVersion: event.KeyVersion,
	}
	if !validRemoteEventRecoveryGeneration(event) {
		// The obligation records that authority is unavailable; never preserve
		// a partial or unknown tuple as if it could authorize a checkpoint.
		generation = syncd.RemoteRecoveryGeneration{}
	}
	obligation := syncd.RemoteCheckpointObligation{
		ScopeID: event.NamespaceID, ArtifactID: event.ArtifactID, BranchID: event.BranchID,
		Kind: event.Kind, HeadEventID: event.EventID, HeadEventHash: event.EventHash,
		Generation: generation, Reason: reason,
	}
	if obligation.ScopeID == "" {
		obligation.ScopeID = "account"
	}
	if err := a.persistCheckpointObligation(obligation); err != nil {
		return err
	}
	if err := a.outbox.mutations.RequireCheckpoint(event.NamespaceID); err != nil {
		return err
	}
	a.notifyCheckpointObligation(obligation)
	return nil
}

// recoverDirtyOnce performs one bounded-by-source proof pass. It is a no-op in
// legacy, shadow, and durable-read modes, preserving their exact transport
// behavior. A dirty marker is cleared only after a later pass sees no missing
// canonical local events under current generation-bound durable watermarks.
func (a *RemotePublishAdapter) recoverDirtyOnce(ctx context.Context) {
	if a == nil || ctx.Err() != nil || a.outbox == nil || a.outbox.mutations == nil || a.watermarks == nil || a.checkpointObligations == nil {
		return
	}
	policy, ok := a.client.(remoteOutboundRecoveryPolicy)
	if !ok || !policy.DurableReceiptRequired() {
		return
	}
	negotiated := policy.SyncNegotiation()
	if negotiated.Mode != proto.RemoteSyncModeDeltaPreferred && negotiated.Mode != proto.RemoteSyncModeDeltaRequired {
		return
	}
	source := a.recoverySourceSnapshot()
	if source == nil {
		return
	}
	markers, err := a.outbox.mutations.ListDirty()
	if err != nil {
		if a.logger != nil {
			a.logger.Warn("remote outbound recovery marker scan failed", "err", err)
		}
		return
	}
	if len(markers) == 0 {
		return
	}
	watermarks, err := a.watermarks.List()
	if err != nil {
		if a.logger != nil {
			a.logger.Warn("remote outbound recovery watermark scan failed", "err", err)
		}
		return
	}
	for _, snapshot := range markers {
		if ctx.Err() != nil {
			return
		}
		marker := snapshot.Marker
		if marker.ReasonFlags&rescanReasonCheckpoint != 0 {
			// A durable checkpoint obligation supersedes delta reconstruction.
			// Leave both the marker and any exact pending ciphertext untouched
			// until the checkpoint producer explicitly fulfills it.
			continue
		}
		descriptor, exists := recoveryDescriptorForScope(negotiated, marker.ScopeID)
		if !exists {
			continue
		}
		generation := markerRecoveryGeneration(marker)
		request := syncd.RemoteRecoveryRequest{
			ScopeID: marker.ScopeID, Generation: generation,
			Target: syncd.RemoteRecoveryTarget{
				ArtifactID: marker.TargetArtifactID, BranchID: marker.TargetBranchID,
				EventID: marker.TargetEventID, EventHash: targetHashString(marker.TargetEventHash),
			},
		}
		for _, watermark := range watermarks {
			if watermark.Key.StreamID != descriptor.StreamID || watermark.Key.StreamEpoch != descriptor.StreamEpoch {
				continue
			}
			anchorGeneration, generationOK := watermarkRecoveryGeneration(watermark)
			if !generationOK || !sameRecoveryGeneration(anchorGeneration, generation) {
				continue
			}
			request.Anchors = append(request.Anchors, syncd.RemoteRecoveryAnchor{
				ArtifactID: watermark.Key.ArtifactID, BranchID: watermark.Key.BranchID,
				CanonicalEventID: watermark.CanonicalEventID, CanonicalEventHash: watermark.CanonicalEventHash,
				Generation: anchorGeneration,
			})
		}

		var terminalEvent *proto.RemoteEvent
		result, rebuildErr := source.RebuildRemoteOutbound(ctx, request, func(event syncd.OutboundEvent) error {
			wire := toRemoteEvent(event)
			wireGeneration := syncd.RemoteRecoveryGeneration{
				AccessGeneration: wire.AccessGeneration, AccessSetHash: wire.AccessSetHash,
				SecurityGeneration: wire.SecurityGeneration, SecurityBarrierID: wire.SecurityBarrierID,
				KeyMode: wire.KeyMode, KeyVersion: wire.KeyVersion,
			}
			if wire.Lane != syncd.LaneLive || remoteEventOversize(wire) || !validRemoteEventRecoveryGeneration(wire) || !sameRecoveryGeneration(wireGeneration, generation) {
				copy := wire
				terminalEvent = &copy
				return ErrOutboxRecoveryAuthorityUnavailable
			}
			if !a.isProjectAuthorized(wire) {
				return errors.New("remote outbound recovery: project authorization changed")
			}
			allowCreate := wire.ArtifactID == marker.TargetArtifactID && recoveryBranchID(wire.BranchID) == recoveryBranchID(marker.TargetBranchID) &&
				wire.EventID == marker.TargetEventID && strings.EqualFold(wire.EventHash, targetHashString(marker.TargetEventHash))
			persisted, err := a.outbox.AppendRecovered(wire, allowCreate)
			if errors.Is(err, ErrOutboxRecoveryTerminal) || errors.Is(err, ErrOutboxRecoveryAuthorityConflict) || errors.Is(err, ErrOutboxRecoveryAuthorityUnavailable) {
				copy := wire
				terminalEvent = &copy
				return err
			}
			if err != nil {
				return err
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case a.queue <- persisted:
			default:
				// The file is durable. A later proof pass retries enqueue in the
				// same canonical scan order after the pump makes room.
			}
			return nil
		})
		if rebuildErr != nil {
			if (errors.Is(rebuildErr, ErrOutboxRecoveryTerminal) || errors.Is(rebuildErr, ErrOutboxRecoveryAuthorityConflict) || errors.Is(rebuildErr, ErrOutboxRecoveryAuthorityUnavailable)) && terminalEvent != nil {
				reason := "terminal-outbox-state"
				if errors.Is(rebuildErr, ErrOutboxRecoveryAuthorityConflict) {
					reason = "pending-recovery-authority-conflict"
				} else if errors.Is(rebuildErr, ErrOutboxRecoveryAuthorityUnavailable) {
					reason = "exact-seal-authority-unavailable"
				}
				obligation := syncd.RemoteCheckpointObligation{
					ScopeID: marker.ScopeID, ArtifactID: terminalEvent.ArtifactID, BranchID: terminalEvent.BranchID,
					Kind: terminalEvent.Kind, HeadEventID: terminalEvent.EventID, HeadEventHash: terminalEvent.EventHash,
					Generation: generation, Reason: reason,
				}
				if persistErr := a.persistCheckpointObligation(obligation); persistErr == nil {
					_ = a.outbox.mutations.RequireCheckpoint(marker.ScopeID)
					a.notifyCheckpointObligation(obligation)
				}
			}
			if a.logger != nil {
				a.logger.Info("remote outbound canonical recovery remains pending", "scope_id", marker.ScopeID, "err", rebuildErr)
			}
			continue
		}
		if len(result.Obligations) > 0 {
			persisted := true
			for _, obligation := range result.Obligations {
				if obligation.ScopeID == "" {
					obligation.ScopeID = marker.ScopeID
				}
				if err := a.persistCheckpointObligation(obligation); err != nil {
					persisted = false
					if a.logger != nil {
						a.logger.Warn("remote checkpoint obligation persist failed", "scope_id", marker.ScopeID, "artifact_id", obligation.ArtifactID, "err", err)
					}
					continue
				}
				a.notifyCheckpointObligation(obligation)
			}
			if persisted {
				if err := a.outbox.mutations.RequireCheckpoint(marker.ScopeID); err != nil && a.logger != nil {
					a.logger.Warn("remote rescan checkpoint reason persist failed", "scope_id", marker.ScopeID, "err", err)
				}
			}
			continue
		}
		if result.Rebuilt > 0 {
			if a.logger != nil {
				a.logger.Info("remote outbound canonical range re-enqueued", "scope_id", marker.ScopeID, "events", result.Rebuilt)
			}
			continue
		}
		if !result.Complete || marker.ReasonFlags&rescanReasonCheckpoint != 0 {
			continue
		}
		completed, completeErr := a.outbox.mutations.CompleteRecovery(marker.ScopeID, marker.MutationGeneration)
		if completeErr != nil {
			if a.logger != nil {
				a.logger.Warn("remote outbound recovery completion failed", "scope_id", marker.ScopeID, "err", completeErr)
			}
			continue
		}
		if completed && a.logger != nil {
			a.logger.Info("remote outbound canonical recovery complete", "scope_id", marker.ScopeID)
		}
	}
}

var _ remoteOutboundRecoverySource = (*syncd.Orchestrator)(nil)
