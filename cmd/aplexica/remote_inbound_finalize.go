package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"sync"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/daemon"
	"github.com/aplexica/aplexica/internal/plugin/proto"
	syncd "github.com/aplexica/aplexica/internal/sync"
)

var errDurableInboundFinalizeMetadata = errors.New("remote: invalid durable inbound finalize metadata")

func durableInboundFinalizeEvidence(
	remoteIdentity string,
	delivery proto.RemoteInboundDeliveryV2,
	canonical syncd.InboundCanonicalEvidence,
) proto.RemoteInboundFinalizeEvidenceV1 {
	event := delivery.Events[0]
	return proto.RemoteInboundFinalizeEvidenceV1{
		ProtocolVersion: delivery.ProtocolVersion, FinalizeKind: canonical.FinalizeKind,
		RemoteIdentity: remoteIdentity, DeliveryID: delivery.DeliveryID,
		StreamID: delivery.StreamID, StreamEpoch: delivery.StreamEpoch, Cursor: delivery.Cursor,
		CursorDigest: delivery.CursorDigest, Position: delivery.Position, NamespaceID: event.NamespaceID,
		BranchID: event.BranchID, Kind: event.Kind, ArtifactID: event.ArtifactID, WireEventID: event.EventID,
		WireEventHash: event.EventHash, BodyDigest: event.BodyDigest, ParentHash: event.ParentHash,
		CheckpointAlignmentHash: event.CheckpointAlignmentHash,
		EventType:               event.Type, TimestampUnixNano: event.Timestamp.UnixNano(), Sequence: event.Sequence,
		Origin: event.Origin, SourceAgent: event.SourceAgent, Lane: event.Lane, Clear: event.Clear,
		CanonicalEventID: canonical.EventID, CanonicalHash: canonical.EventHash,
		NoopReason: canonical.NoopReason, AuthenticatedHeaderDigest: canonical.AuthenticatedHeaderDigest,
		AuthenticatedSignerIdentity: canonical.AuthenticatedSigner,
	}
}

func validDurableInboundFinalizeEvidence(evidence proto.RemoteInboundFinalizeEvidenceV1) bool {
	if evidence.ProtocolVersion != 1 || evidence.Position == 0 ||
		!validDurableInboundOpaque(evidence.RemoteIdentity, durableInboundIdentityMaxBytes) ||
		!validDurableInboundOpaque(evidence.DeliveryID, proto.MaxDeliveryIDBytes) ||
		!validDurableInboundOpaque(evidence.StreamID, durableInboundIdentityMaxBytes) ||
		!validDurableInboundOpaque(evidence.StreamEpoch, durableInboundIdentityMaxBytes) ||
		!validDurableInboundOpaque(evidence.Cursor, proto.MaxDurableCursorBytes) || !validDurableInboundDigest(evidence.CursorDigest) ||
		!validDurableInboundOptionalOpaque(evidence.NamespaceID, durableInboundIdentityMaxBytes) {
		return false
	}
	if evidence.BatchEventCount != 0 {
		return validDurableInboundBatchFinalizeEvidence(evidence)
	}
	if evidence.BatchDigest != "" || evidence.BatchResultDigest != "" || evidence.BatchMaterializationPlan != "" || evidence.BatchMaterializationDigest != "" ||
		!validDurableInboundOptionalOpaque(evidence.BranchID, durableInboundIdentityMaxBytes) ||
		!validDurableInboundOpaque(evidence.Kind, 128) || acf.ValidateKind(acf.Kind(evidence.Kind)) != nil ||
		!validDurableInboundOpaque(evidence.ArtifactID, durableInboundIdentityMaxBytes) ||
		!validDurableInboundOpaque(evidence.WireEventID, durableInboundIdentityMaxBytes) ||
		!validDurableInboundDigest(evidence.BodyDigest) ||
		!validDurableInboundOptionalDigest(evidence.ParentHash) ||
		!validDurableInboundOpaque(evidence.EventType, 128) ||
		!validDurableInboundOpaque(evidence.Origin, durableInboundIdentityMaxBytes) ||
		!validDurableInboundOptionalOpaque(evidence.SourceAgent, durableInboundIdentityMaxBytes) ||
		!validDurableInboundOptionalOpaque(evidence.Lane, 128) {
		return false
	}
	if evidence.Lane == syncd.LaneRetained {
		if !validDurableInboundDigest(evidence.CheckpointAlignmentHash) {
			return false
		}
	} else if evidence.CheckpointAlignmentHash != "" {
		return false
	}
	switch evidence.FinalizeKind {
	case proto.InboundFinalizeCanonicalMaterialize:
		return !evidence.Clear && validDurableInboundDigest(evidence.WireEventHash) &&
			validDurableInboundOpaque(evidence.CanonicalEventID, durableInboundIdentityMaxBytes) &&
			validDurableInboundDigest(evidence.CanonicalHash) &&
			evidence.NoopReason == "" && evidence.AuthenticatedHeaderDigest == "" && evidence.AuthenticatedSignerIdentity == "" &&
			evidence.CheckpointCoveragePlan == "" && evidence.CheckpointCoverageDigest == ""
	case proto.InboundFinalizeCheckpointCovered:
		entries, err := proto.DecodeRemoteCheckpointCoveragePlan(evidence.CheckpointCoveragePlan, evidence.CheckpointCoverageDigest)
		return err == nil && len(entries) == 1 && entries[0].Index == 0 && entries[0].BlockedPosition == evidence.Position &&
			entries[0].ArtifactID == evidence.ArtifactID && entries[0].BranchID == evidence.BranchID && entries[0].Kind == evidence.Kind &&
			validDurableCheckpointCoverageEntry(entries[0]) && !evidence.Clear && validDurableInboundDigest(evidence.WireEventHash) &&
			validDurableInboundOpaque(evidence.CanonicalEventID, durableInboundIdentityMaxBytes) && validDurableInboundDigest(evidence.CanonicalHash) &&
			evidence.NoopReason == "" && evidence.AuthenticatedHeaderDigest == "" && evidence.AuthenticatedSignerIdentity == ""
	case proto.InboundFinalizeAuthenticatedNoop:
		if evidence.Clear || evidence.CanonicalEventID != "" || evidence.CanonicalHash != "" ||
			evidence.CheckpointCoveragePlan != "" || evidence.CheckpointCoverageDigest != "" ||
			!validDurableInboundDigest(evidence.AuthenticatedHeaderDigest) ||
			!validAuthenticatedSignerIdentity(evidence.AuthenticatedSignerIdentity) {
			return false
		}
		switch evidence.NoopReason {
		case proto.InboundFinalizeNoopNotRecipient:
			return validDurableInboundDigest(evidence.WireEventHash)
		default:
			return false
		}
	default:
		return false
	}
}

func validDurableInboundBatchFinalizeEvidence(evidence proto.RemoteInboundFinalizeEvidenceV1) bool {
	if evidence.BatchEventCount < 2 || evidence.BatchEventCount > proto.RemoteReplayBatchMaxEvents ||
		!validDurableInboundDigest(evidence.BatchDigest) || !validDurableInboundDigest(evidence.BatchResultDigest) || evidence.BranchID != "" ||
		evidence.Kind != "" || evidence.ArtifactID != "" || evidence.WireEventID != "" || evidence.WireEventHash != "" || evidence.BodyDigest != "" ||
		evidence.ParentHash != "" || evidence.CheckpointAlignmentHash != "" || evidence.EventType != "" || evidence.TimestampUnixNano != 0 ||
		evidence.Sequence != 0 || evidence.Origin != "" || evidence.SourceAgent != "" || evidence.Lane != "" || evidence.Clear ||
		evidence.CanonicalEventID != "" || evidence.CanonicalHash != "" || evidence.NoopReason != "" || evidence.AuthenticatedHeaderDigest != "" ||
		evidence.AuthenticatedSignerIdentity != "" {
		return false
	}
	entries, err := proto.DecodeRemoteBatchMaterializationPlan(evidence.BatchMaterializationPlan, evidence.BatchMaterializationDigest)
	if err != nil {
		return false
	}
	if (evidence.CheckpointCoveragePlan == "") != (evidence.CheckpointCoverageDigest == "") {
		return false
	}
	if evidence.CheckpointCoveragePlan != "" {
		covered, decodeErr := proto.DecodeRemoteCheckpointCoveragePlan(evidence.CheckpointCoveragePlan, evidence.CheckpointCoverageDigest)
		if decodeErr != nil || evidence.Position < uint64(evidence.BatchEventCount) {
			return false
		}
		firstPosition := evidence.Position - uint64(evidence.BatchEventCount) + 1
		materialized := make(map[string]struct{}, len(entries))
		for _, entry := range entries {
			materialized[entry.Kind+"\x00"+entry.ArtifactID] = struct{}{}
		}
		for _, entry := range covered {
			if entry.Index >= uint32(evidence.BatchEventCount) || entry.BlockedPosition != firstPosition+uint64(entry.Index) || !validDurableCheckpointCoverageEntry(entry) {
				return false
			}
			if _, ok := materialized[entry.Kind+"\x00"+entry.ArtifactID]; !ok {
				return false
			}
		}
	}
	if evidence.FinalizeKind == proto.InboundFinalizeCanonicalBatch {
		return len(entries) > 0
	}
	return evidence.FinalizeKind == proto.InboundFinalizeAuthenticatedBatchNoop && len(entries) == 0
}

func validDurableCheckpointCoverageEntry(entry proto.RemoteCheckpointCoverageEntryV1) bool {
	if entry.BlockedPosition == 0 || entry.CheckpointCoverage < entry.BlockedPosition || entry.CheckpointPosition <= entry.CheckpointCoverage ||
		!validDurableInboundOpaque(entry.RequestID, durableInboundIdentityMaxBytes) || !validDurableInboundDigest(entry.MissingParentHash) ||
		!validDurableInboundOpaque(entry.ArtifactID, durableInboundIdentityMaxBytes) || !validDurableInboundOpaque(entry.BranchID, durableInboundIdentityMaxBytes) ||
		!validDurableInboundOpaque(entry.Kind, 128) || acf.ValidateKind(acf.Kind(entry.Kind)) != nil ||
		!validDurableInboundOpaque(entry.CheckpointEventID, durableInboundIdentityMaxBytes) || !validDurableInboundDigest(entry.CheckpointEventHash) ||
		!validDurableInboundDigest(entry.CheckpointBodyDigest) || !validDurableInboundDigest(entry.CheckpointAlignmentHash) ||
		!validDurableInboundDigest(entry.CheckpointGeneration) || !validDurableInboundOpaque(entry.CheckpointCursor, proto.MaxDurableCursorBytes) ||
		!validDurableInboundDigest(entry.CheckpointCursorDigest) || !validDurableInboundOpaque(entry.CheckpointCoverageCursor, proto.MaxDurableCursorBytes) ||
		!validDurableInboundDigest(entry.CheckpointCoverageDigest) || entry.CheckpointCursor == entry.CheckpointCoverageCursor ||
		entry.AccessGeneration == 0 || !validDurableInboundDigest(entry.AccessSetHash) || entry.SecurityGeneration == 0 || !validDurableInboundDigest(entry.SecurityBarrier) ||
		(entry.KeyMode != "recipient-wrap-v2" || entry.KeyVersion != 0) && (entry.KeyMode != "namespace-key-v1" || entry.KeyVersion == 0) {
		return false
	}
	checkpointDigest := sha256.Sum256([]byte(entry.CheckpointCursor))
	coverageDigest := sha256.Sum256([]byte(entry.CheckpointCoverageCursor))
	return entry.CheckpointCursorDigest == hex.EncodeToString(checkpointDigest[:]) && entry.CheckpointCoverageDigest == hex.EncodeToString(coverageDigest[:])
}

func validDurableInboundOptionalDigest(value string) bool {
	return value == "" || validDurableInboundDigest(value)
}

func validAuthenticatedSignerIdentity(value string) bool {
	separator := strings.LastIndexByte(value, ':')
	return separator > 0 &&
		validDurableInboundOpaque(value[:separator], durableInboundIdentityMaxBytes) &&
		validDurableInboundDigest(value[separator+1:])
}

func validateDurableInboundFinalizeBinding(
	remoteIdentity string,
	negotiated proto.RemoteNegotiateSyncV1Result,
	evidence proto.RemoteInboundFinalizeEvidenceV1,
) (daemon.DurableCursorKey, daemon.DurableCursorState, error) {
	if evidence.RemoteIdentity != remoteIdentity || !validDurableInboundFinalizeEvidence(evidence) {
		return daemon.DurableCursorKey{}, daemon.DurableCursorState{}, errDurableInboundFinalizeMetadata
	}
	if len(negotiated.Streams) != 0 && !containsRemoteCapability(negotiated.ServerCapabilities, proto.CapabilityDurableMultiStreamV1) {
		return daemon.DurableCursorKey{}, daemon.DurableCursorState{}, errDurableInboundFinalizeMetadata
	}
	if evidence.BatchEventCount != 0 && !containsRemoteCapability(negotiated.ServerCapabilities, proto.CapabilityRedactionSafeBatchV1) {
		return daemon.DurableCursorKey{}, daemon.DurableCursorState{}, errDurableInboundFinalizeMetadata
	}
	wantCursorDigest := sha256.Sum256([]byte(evidence.Cursor))
	if evidence.CursorDigest != hex.EncodeToString(wantCursorDigest[:]) || negotiated.SelectedProtocol != 1 ||
		!negotiated.FeatureGateEnabled || !negotiatedDurableScopedStream(negotiated, evidence.StreamID, evidence.StreamEpoch, evidence.NamespaceID) ||
		!containsRemoteCapability(negotiated.ServerCapabilities, proto.CapabilityDurableDeltaSyncV1) ||
		!containsRemoteCapability(negotiated.ServerCapabilities, proto.CapabilityInboundAckV2) ||
		!containsRemoteCapability(negotiated.ServerCapabilities, proto.CapabilityInboundFinalizeV1) {
		return daemon.DurableCursorKey{}, daemon.DurableCursorState{}, errDurableInboundFinalizeMetadata
	}
	switch negotiated.Mode {
	case proto.RemoteSyncModeShadow,
		proto.RemoteSyncModeDurableRead, proto.RemoteSyncModeDeltaPreferred, proto.RemoteSyncModeDeltaRequired:
	default:
		return daemon.DurableCursorKey{}, daemon.DurableCursorState{}, errDurableInboundFinalizeMetadata
	}
	return daemon.DurableCursorKey{RemoteIdentity: remoteIdentity, StreamID: evidence.StreamID, StreamEpoch: evidence.StreamEpoch},
		daemon.DurableCursorState{Cursor: evidence.Cursor, CursorDigest: evidence.CursorDigest, Position: evidence.Position}, nil
}

func validDurableInboundOptionalOpaque(value string, maximum int) bool {
	return value == "" || validDurableInboundOpaque(value, maximum)
}

// durableInboundFinalizeBarrier enforces plugin sequencing independently of
// plugin correctness. Exact redelivery of the current cursor is allowed so a
// lost inbound ACK can be repaired. A new successor is admitted only after the
// predecessor's native-finalize marker is durable.
func durableInboundFinalizeBarrier(inbox *daemon.InboundInbox, cursors *daemon.DurableCursorStore, binding *durableInboundCursorBinding) error {
	if inbox == nil || cursors == nil || binding == nil || binding.predecessor == nil {
		return errDurableInboundFinalizeMetadata
	}
	completed, err := inbox.CompletedDurable()
	if err != nil {
		return err
	}
	unfinalized := 0
	for _, completion := range completed {
		if completion.NativeFinalized {
			continue
		}
		unfinalized++
		if unfinalized > 1 || completion.RemoteIdentity != binding.key.RemoteIdentity ||
			completion.StreamID != binding.key.StreamID || completion.StreamEpoch != binding.key.StreamEpoch ||
			completion.Cursor != binding.next.Cursor || completion.CursorDigest != binding.next.CursorDigest || completion.Position != binding.next.Position {
			return errDurableInboundFinalizeMetadata
		}
	}
	current, err := cursors.Load(binding.key)
	if errors.Is(err, daemon.ErrDurableCursorNotFound) && binding.span > 0 && binding.next.Position == uint64(binding.span) && binding.predecessor.Position == 0 {
		return nil
	}
	if err != nil {
		return err
	}
	if current.Cursor == binding.next.Cursor && current.CursorDigest == binding.next.CursorDigest && current.Position == binding.next.Position {
		return nil
	}
	if current.Cursor != binding.predecessor.Cursor || current.CursorDigest != binding.predecessor.CursorDigest || current.Position != binding.predecessor.Position {
		return daemon.ErrDurableCursorConflict
	}
	finalized, err := inbox.DurableCompletionFinalized(binding.key, current)
	if errors.Is(err, daemon.ErrInboundFinalizeEvidenceNotFound) {
		seeded, seedErr := cursors.IsCurrentCheckpointSeed(binding.key, current)
		if seedErr != nil {
			return seedErr
		}
		if seeded {
			return nil
		}
		return errDurableInboundFinalizeMetadata
	}
	if err != nil {
		return err
	}
	if !finalized {
		return errDurableInboundFinalizeMetadata
	}
	return nil
}

// handleDurableInboundFinalize is deliberately independent from the remote
// runner. Its shared phase gate serializes cursor commit and native finalize,
// while the exact cursor comparison makes a request stale as soon as a later
// durable delivery has advanced. No code in this function writes cursor or
// cloud ACK state.
func handleDurableInboundFinalize(
	phaseGate *sync.Mutex,
	inbox *daemon.InboundInbox,
	cursors *daemon.DurableCursorStore,
	remoteIdentity string,
	negotiated proto.RemoteNegotiateSyncV1Result,
	params proto.RemoteInboundFinalizeV1Params,
	materialize func(syncd.InboundCanonicalEvidence) error,
) proto.RemoteInboundFinalizeV1Result {
	reject := func(reason string) proto.RemoteInboundFinalizeV1Result {
		return proto.RemoteInboundFinalizeV1Result{ReasonCode: reason}
	}
	if phaseGate == nil || inbox == nil || cursors == nil || materialize == nil {
		return reject("handler-unavailable")
	}
	phaseGate.Lock()
	defer phaseGate.Unlock()
	key, expected, err := validateDurableInboundFinalizeBinding(remoteIdentity, negotiated, params.Evidence)
	if err != nil {
		return reject("metadata-invalid")
	}
	current, err := cursors.Load(key)
	if err != nil {
		return reject("cursor-unavailable")
	}
	if current.Cursor != expected.Cursor || current.CursorDigest != expected.CursorDigest || current.Position != expected.Position {
		return reject("cursor-stale")
	}
	already, err := inbox.PrepareInboundFinalize(params.Evidence)
	if err != nil {
		return reject("terminal-evidence-unavailable")
	}
	if already {
		return proto.RemoteInboundFinalizeV1Result{Accepted: true, AlreadyFinalized: true}
	}
	if params.Evidence.FinalizeKind == proto.InboundFinalizeCanonicalMaterialize || params.Evidence.FinalizeKind == proto.InboundFinalizeCheckpointCovered {
		canonical := syncd.InboundCanonicalEvidence{
			FinalizeKind: proto.InboundFinalizeCanonicalMaterialize,
			Kind:         acf.Kind(params.Evidence.Kind), ArtifactID: params.Evidence.ArtifactID,
			EventID: params.Evidence.CanonicalEventID, EventHash: params.Evidence.CanonicalHash,
		}
		if err := materialize(canonical); err != nil {
			if errors.Is(err, syncd.ErrInboundNativeMaterialization) {
				return reject("native-materialization-retryable")
			}
			return reject("canonical-evidence-invalid")
		}
	} else if params.Evidence.FinalizeKind == proto.InboundFinalizeCanonicalBatch {
		entries, decodeErr := proto.DecodeRemoteBatchMaterializationPlan(params.Evidence.BatchMaterializationPlan, params.Evidence.BatchMaterializationDigest)
		if decodeErr != nil {
			return reject("canonical-evidence-invalid")
		}
		for _, entry := range entries {
			canonical := syncd.InboundCanonicalEvidence{
				FinalizeKind: proto.InboundFinalizeCanonicalMaterialize,
				Kind:         acf.Kind(entry.Kind), ArtifactID: entry.ArtifactID,
				EventID: entry.CanonicalEventID, EventHash: entry.CanonicalHash,
			}
			if err := materialize(canonical); err != nil {
				if errors.Is(err, syncd.ErrInboundNativeMaterialization) {
					return reject("native-materialization-retryable")
				}
				return reject("canonical-evidence-invalid")
			}
		}
	}
	already, err = inbox.MarkInboundFinalized(params.Evidence)
	if err != nil {
		return reject("finalize-commit-failed")
	}
	if already {
		return proto.RemoteInboundFinalizeV1Result{Accepted: true, AlreadyFinalized: true}
	}
	if params.Evidence.FinalizeKind == proto.InboundFinalizeAuthenticatedNoop || params.Evidence.FinalizeKind == proto.InboundFinalizeAuthenticatedBatchNoop {
		return proto.RemoteInboundFinalizeV1Result{Accepted: true, NoopFinalized: true}
	}
	return proto.RemoteInboundFinalizeV1Result{Accepted: true, Materialized: true}
}
