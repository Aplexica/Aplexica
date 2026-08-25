package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/aplexica/aplexica/internal/daemon"
	"github.com/aplexica/aplexica/internal/plugin/proto"
	syncd "github.com/aplexica/aplexica/internal/sync"
)

const (
	durableGapRecoveryMaxDepth = 64
	durableGapRecoveryMaxBytes = int64(32 << 20)
	durableGapRecoveryTimeout  = 15 * time.Second
)

var (
	errDurableGapPending   = errors.New("remote: durable gap recovery pending")
	errDurableGapMalformed = errors.New("remote: malformed durable recovery record")
)

type durableGapSpool interface {
	Put(daemon.DurableGapKey, proto.RemoteInboundDeliveryV2, string, ...uint16) (daemon.DurableGap, error)
	Load(daemon.DurableGapKey) (daemon.DurableGap, error)
	AdvanceSelector(daemon.DurableGapKey, proto.RemoteInboundDeliveryV2, string, uint16, string, uint16) (daemon.DurableGap, error)
	Resolve(daemon.DurableGapKey, string) error
}

type durableGapRemote interface {
	FetchParentV1(context.Context, proto.RemoteFetchParentV1Params) (proto.RemoteFetchParentV1Result, error)
	RequestCheckpointV1(context.Context, proto.RemoteRequestCheckpointV1Params) (proto.RemoteRequestCheckpointV1Result, error)
}

type durableCheckpointCursorStore interface {
	Load(daemon.DurableCursorKey) (daemon.DurableCursorState, error)
	SeedFromCheckpoint(daemon.DurableCursorKey, daemon.DurableCheckpointSeed) (daemon.DurableCursorState, error)
}

type durableGapImporter func([]proto.RemoteEvent) []syncd.ImportOutcome

type durableCheckpointCoveredEvent struct {
	params     proto.RemoteRequestCheckpointV1Params
	checkpoint proto.RemoteRecoveryEventV1
}

// durableGapRecoveryEvidence is ephemeral assembly state for the terminal
// evidence written to the durable inbox immediately after recovery. The gap
// spool remains the crash authority until that evidence is persisted; a
// restart requests and verifies the checkpoint again.
type durableGapRecoveryEvidence struct {
	covered                 map[int]durableCheckpointCoveredEvent
	checkpointRestoreFailed bool
}

func (e *durableGapRecoveryEvidence) markCheckpointRestoreFailed() {
	if e != nil {
		e.checkpointRestoreFailed = true
	}
}

func (e *durableGapRecoveryEvidence) record(index int, params proto.RemoteRequestCheckpointV1Params, checkpoint *proto.RemoteRecoveryEventV1) {
	if e == nil || checkpoint == nil {
		return
	}
	if e.covered == nil {
		e.covered = make(map[int]durableCheckpointCoveredEvent)
	}
	e.covered[index] = durableCheckpointCoveredEvent{params: params, checkpoint: *checkpoint}
}

func durableCheckpointCoveragePlan(delivery proto.RemoteInboundDeliveryV2, evidence *durableGapRecoveryEvidence) (string, string, error) {
	if evidence == nil || len(evidence.covered) == 0 {
		return "", "", nil
	}
	indices := make([]int, 0, len(evidence.covered))
	for index := range evidence.covered {
		indices = append(indices, index)
	}
	sort.Ints(indices)
	entries := make([]proto.RemoteCheckpointCoverageEntryV1, 0, len(indices))
	for _, index := range indices {
		covered := evidence.covered[index]
		if index < 0 || index >= len(delivery.Events) || index >= proto.RemoteReplayBatchMaxEvents ||
			!checkpointCoversDeliveryEvent(&covered.checkpoint, delivery, index) {
			return "", "", errDurableGapMalformed
		}
		event := delivery.Events[index]
		blockedPosition := delivery.PredecessorPosition + uint64(index) + 1
		params, checkpoint := covered.params, covered.checkpoint
		if blockedPosition <= delivery.PredecessorPosition || params.MinimumCoverage != blockedPosition ||
			params.ArtifactID != event.ArtifactID || params.BranchID != event.BranchID || params.Kind != event.Kind ||
			params.AccessGeneration != event.AccessGeneration || params.AccessSetHash != event.AccessSetHash ||
			params.SecurityGeneration != event.SecurityGeneration || params.SecurityBarrierID != event.SecurityBarrierID ||
			params.KeyMode != event.KeyMode || params.KeyVersion != event.KeyVersion {
			return "", "", errDurableGapMalformed
		}
		entries = append(entries, proto.RemoteCheckpointCoverageEntryV1{
			Index: uint32(index), BlockedPosition: blockedPosition,
			RequestID: params.RequestID, MissingParentHash: params.MissingParentHash,
			ArtifactID: event.ArtifactID, BranchID: event.BranchID, Kind: event.Kind,
			CheckpointEventID: checkpoint.Event.EventID, CheckpointEventHash: checkpoint.Event.EventHash,
			CheckpointBodyDigest: checkpoint.Event.BodyDigest, CheckpointAlignmentHash: checkpoint.Event.CheckpointAlignmentHash,
			CheckpointGeneration: checkpoint.Event.CheckpointGeneration, CheckpointPosition: checkpoint.Position,
			CheckpointCursor: checkpoint.Cursor, CheckpointCursorDigest: checkpoint.CursorDigest,
			CheckpointCoverage: checkpoint.CoveragePosition, CheckpointCoverageCursor: checkpoint.CoverageCursor,
			CheckpointCoverageDigest: checkpoint.CoverageCursorDigest,
			AccessGeneration:         event.AccessGeneration, AccessSetHash: hex.EncodeToString(event.AccessSetHash[:]),
			SecurityGeneration: event.SecurityGeneration, SecurityBarrier: hex.EncodeToString(event.SecurityBarrierID[:]),
			KeyMode: event.KeyMode, KeyVersion: event.KeyVersion,
		})
	}
	return proto.EncodeRemoteCheckpointCoveragePlan(entries)
}

func durableGapKey(binding *durableInboundCursorBinding) (daemon.DurableGapKey, error) {
	if binding == nil {
		return daemon.DurableGapKey{}, daemon.ErrDurableGapInvalid
	}
	return daemon.DurableGapKey{
		RemoteIdentity: binding.key.RemoteIdentity,
		StreamID:       binding.key.StreamID,
		StreamEpoch:    binding.key.StreamEpoch,
		Position:       binding.next.Position,
	}, nil
}

func durableGapNeedsRecovery(results []syncd.ImportOutcome) bool {
	_, ok := durableGapMissingIndex(results)
	return ok
}

func durableGapMissingIndex(results []syncd.ImportOutcome) (int, bool) {
	for index, result := range results {
		if result == syncd.ImportDeferredNeedsBaseline {
			return index, true
		}
	}
	return 0, false
}

func durableGapApplied(outcome syncd.ImportOutcome) bool {
	return outcome == syncd.ImportApplied || outcome == syncd.ImportDeduped
}

func durableGapRetryableResults(count ...int) []syncd.ImportOutcome {
	// Preserve the durable ACK's stable "missing-parent" reason while every
	// recovery failure remains non-terminal and cursor-stopping.
	size := 1
	if len(count) == 1 && count[0] > 0 && count[0] <= proto.RemoteReplayBatchMaxEvents {
		size = count[0]
	}
	results := make([]syncd.ImportOutcome, size)
	for index := range results {
		results[index] = syncd.ImportDeferredNeedsBaseline
	}
	return results
}

func durableGapEventRecovered(results []syncd.ImportOutcome, eventIndex, expected int) bool {
	if len(results) != expected || eventIndex < 0 || eventIndex >= len(results) {
		return false
	}
	switch results[eventIndex] {
	case syncd.ImportApplied, syncd.ImportDeduped, syncd.ImportSkipped:
		return true
	default:
		return false
	}
}

func sameDurableRecoverySecurity(left, right proto.RemoteEvent) bool {
	return left.AccessGeneration == right.AccessGeneration && left.AccessSetHash == right.AccessSetHash &&
		left.SecurityBarrierID == right.SecurityBarrierID && left.SecurityGeneration == right.SecurityGeneration &&
		left.KeyMode == right.KeyMode && left.KeyVersion == right.KeyVersion
}

func recoveryEventValid(record *proto.RemoteRecoveryEventV1, wantedHash string, selector proto.RemoteEvent, beforePosition uint64, exactBranch bool) bool {
	if record == nil || record.Position == 0 || record.PredecessorPosition != record.Position-1 ||
		(beforePosition != 0 && record.Position >= beforePosition) ||
		!validDurableInboundOpaque(record.PredecessorCursor, proto.MaxDurableCursorBytes) ||
		!validDurableInboundOpaque(record.Cursor, proto.MaxDurableCursorBytes) || record.PredecessorCursor == record.Cursor ||
		!validDurableInboundDigest(record.CursorDigest) || !validDurableInboundDigest(wantedHash) ||
		!validDurableInboundDigest(record.Event.EventHash) || record.Event.EventHash != wantedHash ||
		(record.Event.ParentHash != "" && !validDurableInboundDigest(record.Event.ParentHash)) ||
		!validDurableInboundOpaque(record.Event.EventID, durableInboundIdentityMaxBytes) || record.Event.EventHash == record.Event.ParentHash ||
		record.Event.NamespaceID != selector.NamespaceID || record.Event.ArtifactID != selector.ArtifactID ||
		!validDurableInboundOpaque(record.Event.BranchID, durableInboundIdentityMaxBytes) ||
		(exactBranch && record.Event.BranchID != selector.BranchID) || !sameDurableRecoverySecurity(record.Event, selector) || record.Event.Clear ||
		!validRecoveryBody(record) ||
		!validDurableInboundDigest(record.Event.BodyDigest) {
		return false
	}
	cursorDigest := sha256.Sum256([]byte(record.Cursor))
	bodyDigest := sha256.Sum256(record.Event.Bytes)
	return record.CursorDigest == hex.EncodeToString(cursorDigest[:]) && record.Event.BodyDigest == hex.EncodeToString(bodyDigest[:])
}

func validRecoveryBody(record *proto.RemoteRecoveryEventV1) bool {
	if record == nil {
		return false
	}
	if record.StagedCheckpoint == nil {
		return len(record.Event.Bytes) > 0 && len(record.Event.Bytes) <= proto.MaxSealedEventBytes
	}
	staged := record.StagedCheckpoint
	return record.Event.Lane == syncd.LaneRetained && len(record.Event.Bytes) == int(staged.SealedBytes) &&
		staged.ProtocolVersion == proto.RemoteStagedTransferProtocolV1 && staged.SealedBytes > proto.MaxSealedEventBytes && staged.SealedBytes <= proto.MaxRemoteStagedCheckpointBytes &&
		validDurableInboundDigest(staged.TransferID) && validDurableInboundDigest(staged.BodyDigest) && staged.BodyDigest == record.Event.BodyDigest &&
		validDurableInboundDigest(staged.BindingDigest) && staged.BindingDigest == proto.RemoteStagedBindingDigest(func() proto.RemoteEvent { event := record.Event; event.Bytes = nil; return event }(), *staged)
}

type durableCheckpointGenerationState struct {
	AccessGeneration   uint64 `json:"access_generation"`
	AccessHash         string `json:"access_hash"`
	SecurityGeneration uint64 `json:"security_generation"`
	SecurityBarrier    string `json:"security_barrier"`
	KeyMode            string `json:"key_mode"`
	KeyVersion         uint64 `json:"key_version"`
}

func durableCheckpointGeneration(event proto.RemoteEvent) (string, error) {
	if event.AccessGeneration == 0 || event.AccessSetHash == ([proto.RemoteDigestBytes]byte{}) ||
		event.SecurityGeneration == 0 || event.SecurityBarrierID == ([proto.RemoteDigestBytes]byte{}) ||
		(event.KeyMode != "recipient-wrap-v2" || event.KeyVersion != 0) && (event.KeyMode != "namespace-key-v1" || event.KeyVersion == 0) {
		return "", errDurableGapMalformed
	}
	state := durableCheckpointGenerationState{
		AccessGeneration: event.AccessGeneration, AccessHash: hex.EncodeToString(event.AccessSetHash[:]),
		SecurityGeneration: event.SecurityGeneration, SecurityBarrier: hex.EncodeToString(event.SecurityBarrierID[:]),
		KeyMode: event.KeyMode, KeyVersion: event.KeyVersion,
	}
	raw, err := json.Marshal(state)
	if err != nil {
		return "", err
	}
	stateDigest := sha256.Sum256(append([]byte("aplexica/security-generation/v1\x00"), raw...))
	generationDigest := sha256.Sum256([]byte("aplexica/checkpoint-generation/v1\x00" + hex.EncodeToString(stateDigest[:])))
	return hex.EncodeToString(generationDigest[:]), nil
}

func durableCheckpointCoverage(record *proto.RemoteRecoveryEventV1) (daemon.DurableCursorState, error) {
	if record == nil || record.CoveragePosition == 0 || record.CoveragePosition != record.Event.CheckpointCoverage ||
		record.CoveragePosition >= record.Position || !validDurableInboundOpaque(record.CoverageCursor, proto.MaxDurableCursorBytes) ||
		!validDurableInboundDigest(record.CoverageCursorDigest) || record.CoverageCursor == record.Cursor {
		return daemon.DurableCursorState{}, errDurableGapMalformed
	}
	digest := sha256.Sum256([]byte(record.CoverageCursor))
	if record.CoverageCursorDigest != hex.EncodeToString(digest[:]) {
		return daemon.DurableCursorState{}, errDurableGapMalformed
	}
	return daemon.DurableCursorState{Cursor: record.CoverageCursor, CursorDigest: record.CoverageCursorDigest, Position: record.CoveragePosition}, nil
}

func checkpointRecordValid(record *proto.RemoteRecoveryEventV1, delivery proto.RemoteInboundDeliveryV2, missingEventIndex int, missingParentHash string) bool {
	if record == nil || len(delivery.Events) == 0 || len(delivery.Events) > proto.RemoteReplayBatchMaxEvents ||
		missingEventIndex < 0 || missingEventIndex >= len(delivery.Events) {
		return false
	}
	event := delivery.Events[missingEventIndex]
	if record.StagedCheckpoint != nil && (record.StagedCheckpoint.StreamID != delivery.StreamID || record.StagedCheckpoint.StreamEpoch != delivery.StreamEpoch) {
		return false
	}
	expectedGeneration, err := durableCheckpointGeneration(event)
	if err != nil || !recoveryEventValid(record, record.Event.EventHash, event, 0, true) ||
		record.Event.Kind != event.Kind ||
		!validDurableInboundDigest(missingParentHash) || record.Event.Lane != syncd.LaneRetained ||
		!validDurableInboundDigest(record.Event.CheckpointAlignmentHash) ||
		record.Event.CheckpointGeneration != expectedGeneration {
		return false
	}
	coverage, err := durableCheckpointCoverage(record)
	blockedPosition := delivery.PredecessorPosition + uint64(missingEventIndex) + 1
	if err != nil {
		return false
	}
	// Two recovery shapes are safe:
	//
	//   1. an older exact-ancestry checkpoint aligned to the unavailable parent;
	//   2. a producer-current full checkpoint whose independently authenticated
	//      cloud coverage reaches the blocked event.
	//
	// The second shape is what prevents a permanent wedge after the producer's
	// canonical head advances. It remains exact for artifact, branch, recipient
	// and security generation, and the cloud coverage cursor is separately
	// authenticated. A checkpoint that is neither exact ancestry nor covering is
	// unrelated and must remain retryable.
	exactAncestry := record.Event.CheckpointAlignmentHash == missingParentHash && coverage.Position < blockedPosition
	compatibleCurrent := coverage.Position >= blockedPosition
	if !exactAncestry && !compatibleCurrent {
		return false
	}
	if event.CheckpointGeneration != "" && event.CheckpointGeneration != expectedGeneration {
		return false
	}
	return true
}

type durableGapStagedRemote interface {
	HydrateRecoveryStagedCheckpoint(context.Context, *proto.RemoteRecoveryEventV1) (*proto.RemoteRecoveryEventV1, error)
	CompleteRecoveryStagedCheckpoint(*proto.RemoteRecoveryEventV1) error
}

func durableCheckpointRequestID(key daemon.DurableGapKey, deliveryID string, eventCount, missingEventIndex int, missingParentHash string) string {
	// Preserve the frozen singleton request identity across mixed-version
	// restart. A batch can expose more than one independent gap over successive
	// retries, so its additive identity also binds the exact event selector and
	// missing hash; two checkpoint requests in one delivery can never alias.
	value := fmt.Sprintf("%s\x00%s\x00%s\x00%d\x00%s", key.RemoteIdentity, key.StreamID, key.StreamEpoch, key.Position, deliveryID)
	if eventCount > 1 {
		value = fmt.Sprintf("%s\x00batch-v1\x00%d\x00%d\x00%s", value, eventCount, missingEventIndex, missingParentHash)
	}
	digest := sha256.Sum256([]byte(value))
	return "gap-" + hex.EncodeToString(digest[:])
}

func durableCheckpointParams(key daemon.DurableGapKey, delivery proto.RemoteInboundDeliveryV2, missingEventIndex int, missingParentHash string) (proto.RemoteRequestCheckpointV1Params, error) {
	if missingEventIndex < 0 || missingEventIndex >= len(delivery.Events) {
		return proto.RemoteRequestCheckpointV1Params{}, errDurableGapMalformed
	}
	event := delivery.Events[missingEventIndex]
	minimumCoverage := delivery.PredecessorPosition + uint64(missingEventIndex) + 1
	if minimumCoverage <= delivery.PredecessorPosition {
		return proto.RemoteRequestCheckpointV1Params{}, errDurableGapMalformed
	}
	generation, err := durableCheckpointGeneration(event)
	if err != nil {
		return proto.RemoteRequestCheckpointV1Params{}, err
	}
	return proto.RemoteRequestCheckpointV1Params{
		RequestID:         durableCheckpointRequestID(key, delivery.DeliveryID, len(delivery.Events), missingEventIndex, missingParentHash),
		StreamID:          key.StreamID,
		StreamEpoch:       key.StreamEpoch,
		NamespaceID:       event.NamespaceID,
		BranchID:          event.BranchID,
		ArtifactID:        event.ArtifactID,
		Kind:              event.Kind,
		MissingParentHash: missingParentHash,
		Reason:            "missing-parent",
		Cursor:            delivery.PredecessorCursor,
		CursorDigest: func() string {
			digest := sha256.Sum256([]byte(delivery.PredecessorCursor))
			return hex.EncodeToString(digest[:])
		}(),
		Position:             delivery.PredecessorPosition,
		MinimumCoverage:      minimumCoverage,
		CheckpointGeneration: generation,
		AccessGeneration:     event.AccessGeneration,
		AccessSetHash:        event.AccessSetHash,
		SecurityBarrierID:    event.SecurityBarrierID,
		SecurityGeneration:   event.SecurityGeneration,
		KeyMode:              event.KeyMode,
		KeyVersion:           event.KeyVersion,
	}, nil
}

func sameDurableCheckpointCursor(left, right daemon.DurableCursorState) bool {
	return left.Cursor == right.Cursor && left.CursorDigest == right.CursorDigest && left.Position == right.Position
}

// ensureDurableCheckpointCursor returns ready=true only when the daemon is
// already at the blocked delivery's predecessor. A bootstrap checkpoint may
// seed an earlier artifact-specific coverage position, but the later delivery
// stays retryable until normal contiguous replay crosses interleaved records.
func ensureDurableCheckpointCursor(store durableCheckpointCursorStore, binding *durableInboundCursorBinding, checkpoint *proto.RemoteRecoveryEventV1) (bool, error) {
	if store == nil || binding == nil || binding.predecessor == nil || checkpoint == nil {
		return false, daemon.ErrDurableCursorInvalid
	}
	coverage, err := durableCheckpointCoverage(checkpoint)
	if err != nil {
		return false, err
	}
	current, err := store.Load(binding.key)
	if err == nil {
		if sameDurableCheckpointCursor(current, *binding.predecessor) {
			return true, nil
		}
		if sameDurableCheckpointCursor(current, coverage) {
			return sameDurableCheckpointCursor(coverage, *binding.predecessor), nil
		}
		return false, daemon.ErrDurableCursorConflict
	}
	if !errors.Is(err, daemon.ErrDurableCursorNotFound) {
		return false, err
	}
	// A current/newer artifact checkpoint is not authority to skip unrelated
	// records in the scope-wide stream. Such a checkpoint can repair an artifact
	// only when the daemon is already durably parked at this delivery's exact
	// predecessor. Bootstrap seeding remains limited to an older/equal coverage
	// position and normal contiguous replay crosses every interleaved record.
	if binding.predecessor == nil || coverage.Position > binding.predecessor.Position {
		return false, daemon.ErrDurableCursorConflict
	}
	_, err = store.SeedFromCheckpoint(binding.key, daemon.DurableCheckpointSeed{
		Cursor: coverage.Cursor, CursorDigest: coverage.CursorDigest, Position: coverage.Position,
		CheckpointEventID: checkpoint.Event.EventID, CheckpointEventHash: checkpoint.Event.EventHash,
		CheckpointAlignmentHash: checkpoint.Event.CheckpointAlignmentHash,
		CheckpointGeneration:    checkpoint.Event.CheckpointGeneration, CheckpointPosition: checkpoint.Position,
		CheckpointCoverage: checkpoint.Event.CheckpointCoverage,
	})
	return err == nil && sameDurableCheckpointCursor(coverage, *binding.predecessor), err
}

func checkpointCoversDeliveryEvent(checkpoint *proto.RemoteRecoveryEventV1, delivery proto.RemoteInboundDeliveryV2, index int) bool {
	if checkpoint == nil || index < 0 || index >= len(delivery.Events) {
		return false
	}
	position := delivery.PredecessorPosition + uint64(index) + 1
	if position <= delivery.PredecessorPosition || position > checkpoint.CoveragePosition {
		return false
	}
	event := delivery.Events[index]
	covered := checkpoint.Event
	return event.NamespaceID == covered.NamespaceID && event.ArtifactID == covered.ArtifactID &&
		event.BranchID == covered.BranchID && event.Kind == covered.Kind &&
		sameDurableRecoverySecurity(event, covered)
}

// replayAfterCompatibleCheckpoint omits only events whose exact global
// positions and artifact/security tuple are covered by the authenticated full
// checkpoint. This is more than an optimization: replaying an old pre-redaction
// delta after importing a newer checkpoint could transiently resurrect content.
// Unrelated interleaved events remain in order and are imported as one batch so
// their redaction-safe staging semantics are preserved.
func replayAfterCompatibleCheckpoint(importer durableGapImporter, checkpoint *proto.RemoteRecoveryEventV1, delivery proto.RemoteInboundDeliveryV2) []syncd.ImportOutcome {
	if importer == nil || checkpoint == nil || len(delivery.Events) == 0 {
		return nil
	}
	results := make([]syncd.ImportOutcome, len(delivery.Events))
	uncovered := make([]proto.RemoteEvent, 0, len(delivery.Events))
	indices := make([]int, 0, len(delivery.Events))
	for index := range delivery.Events {
		if checkpointCoversDeliveryEvent(checkpoint, delivery, index) {
			results[index] = syncd.ImportDeduped
			continue
		}
		uncovered = append(uncovered, delivery.Events[index])
		indices = append(indices, index)
	}
	if len(uncovered) == 0 {
		return results
	}
	outcomes := importer(uncovered)
	if len(outcomes) != len(uncovered) {
		return nil
	}
	for index, outcome := range outcomes {
		results[indices[index]] = outcome
	}
	return results
}

func recoverDurableCheckpoint(ctx context.Context, remote durableGapRemote, cursorStore durableCheckpointCursorStore, importer durableGapImporter, binding *durableInboundCursorBinding, key daemon.DurableGapKey, delivery proto.RemoteInboundDeliveryV2, missingEventIndex int, missingParentHash string, recoveryEvidence *durableGapRecoveryEvidence) ([]syncd.ImportOutcome, error) {
	retryable := func() []syncd.ImportOutcome { return durableGapRetryableResults(len(delivery.Events)) }
	params, err := durableCheckpointParams(key, delivery, missingEventIndex, missingParentHash)
	if err != nil {
		return retryable(), err
	}
	result, err := remote.RequestCheckpointV1(ctx, params)
	if err != nil {
		return retryable(), err
	}
	if result.Checkpoint == nil {
		return retryable(), errDurableGapPending
	}
	checkpoint := result.Checkpoint
	stagedRemote, staged := remote.(durableGapStagedRemote)
	if checkpoint.StagedCheckpoint != nil {
		if !staged {
			recoveryEvidence.markCheckpointRestoreFailed()
			return retryable(), errDurableGapMalformed
		}
		checkpoint, err = stagedRemote.HydrateRecoveryStagedCheckpoint(ctx, checkpoint)
		if err != nil {
			recoveryEvidence.markCheckpointRestoreFailed()
			return retryable(), err
		}
	}
	if !checkpointRecordValid(checkpoint, delivery, missingEventIndex, missingParentHash) {
		recoveryEvidence.markCheckpointRestoreFailed()
		return retryable(), errDurableGapMalformed
	}
	outcomes := importer([]proto.RemoteEvent{checkpoint.Event})
	if len(outcomes) != 1 || !durableGapApplied(outcomes[0]) {
		recoveryEvidence.markCheckpointRestoreFailed()
		return retryable(), errDurableGapPending
	}
	ready, err := ensureDurableCheckpointCursor(cursorStore, binding, checkpoint)
	if err != nil {
		return retryable(), err
	}
	if !ready {
		return retryable(), errDurableGapPending
	}
	if checkpoint.StagedCheckpoint != nil {
		if err := stagedRemote.CompleteRecoveryStagedCheckpoint(checkpoint); err != nil {
			return retryable(), err
		}
	}
	replayed := replayAfterCompatibleCheckpoint(importer, checkpoint, delivery)
	if durableGapEventRecovered(replayed, missingEventIndex, len(delivery.Events)) {
		for index := range delivery.Events {
			if checkpointCoversDeliveryEvent(checkpoint, delivery, index) {
				recoveryEvidence.record(index, params, checkpoint)
			}
		}
		return replayed, nil
	}
	return retryable(), errDurableGapPending
}

// recoverDurableInboundGap persists the stopped encrypted delivery before any
// network request, fetches missing ancestors without advancing the main cloud
// cursor, applies them oldest-first, and replays the original event. A compacted
// or unavailable parent falls back to an exact-generation checkpoint request.
func recoverDurableInboundGap(ctx context.Context, spool durableGapSpool, remote durableGapRemote, cursorStore durableCheckpointCursorStore, importer durableGapImporter, binding *durableInboundCursorBinding, delivery proto.RemoteInboundDeliveryV2, initial []syncd.ImportOutcome, evidenceOut ...*durableGapRecoveryEvidence) ([]syncd.ImportOutcome, error) {
	var recoveryEvidence *durableGapRecoveryEvidence
	if len(evidenceOut) == 1 {
		recoveryEvidence = evidenceOut[0]
	} else if len(evidenceOut) > 1 {
		return durableGapRetryableResults(len(delivery.Events)), daemon.ErrDurableGapInvalid
	}
	missingEventIndex, needsRecovery := durableGapMissingIndex(initial)
	if !needsRecovery {
		return initial, nil
	}
	retryable := func() []syncd.ImportOutcome { return durableGapRetryableResults(len(delivery.Events)) }
	if spool == nil || remote == nil || importer == nil || binding == nil || len(delivery.Events) == 0 ||
		len(delivery.Events) > proto.RemoteReplayBatchMaxEvents || len(initial) != len(delivery.Events) {
		return retryable(), daemon.ErrDurableGapInvalid
	}
	key, err := durableGapKey(binding)
	if err != nil {
		return retryable(), err
	}
	selector := delivery.Events[missingEventIndex]
	missingParentHash := selector.ParentHash
	existing, loadErr := spool.Load(key)
	switch {
	case errors.Is(loadErr, daemon.ErrDurableGapNotFound):
		_, err = spool.Put(key, delivery, missingParentHash, uint16(missingEventIndex))
	case loadErr != nil:
		err = loadErr
	case int(existing.MissingEventIndex) == missingEventIndex:
		_, err = spool.Put(key, delivery, missingParentHash, uint16(missingEventIndex))
	case int(existing.MissingEventIndex) < missingEventIndex &&
		durableGapEventRecovered(initial, int(existing.MissingEventIndex), len(delivery.Events)):
		_, err = spool.AdvanceSelector(
			key, delivery,
			existing.MissingParentHash, existing.MissingEventIndex,
			missingParentHash, uint16(missingEventIndex),
		)
	default:
		err = daemon.ErrDurableGapConflict
	}
	if err != nil {
		return retryable(), err
	}

	recoveryCtx, cancel := context.WithTimeout(ctx, durableGapRecoveryTimeout)
	defer cancel()
	wanted := missingParentHash
	beforePosition := delivery.PredecessorPosition + uint64(missingEventIndex) + 1
	pending := make([]proto.RemoteEvent, 0, durableGapRecoveryMaxDepth)
	seen := make(map[string]struct{}, durableGapRecoveryMaxDepth)
	var fetchedBytes int64
	for depth := 0; depth < durableGapRecoveryMaxDepth; depth++ {
		if !validDurableInboundDigest(wanted) {
			return retryable(), errDurableGapMalformed
		}
		if _, duplicate := seen[wanted]; duplicate {
			return retryable(), errDurableGapMalformed
		}
		seen[wanted] = struct{}{}
		event := selector
		fetched, fetchErr := remote.FetchParentV1(recoveryCtx, proto.RemoteFetchParentV1Params{
			StreamID: key.StreamID, StreamEpoch: key.StreamEpoch, NamespaceID: event.NamespaceID, BranchID: event.BranchID,
			ArtifactID: event.ArtifactID, EventHash: wanted, AccessGeneration: event.AccessGeneration, AccessSetHash: event.AccessSetHash,
			SecurityBarrierID: event.SecurityBarrierID, SecurityGeneration: event.SecurityGeneration, KeyMode: event.KeyMode, KeyVersion: event.KeyVersion,
		})
		if fetchErr != nil {
			return retryable(), fetchErr
		}
		if !fetched.Found {
			results, checkpointErr := recoverDurableCheckpoint(recoveryCtx, remote, cursorStore, importer, binding, key, delivery, missingEventIndex, wanted, recoveryEvidence)
			if checkpointErr == nil {
				if resolveErr := spool.Resolve(key, delivery.DeliveryID); resolveErr != nil {
					return retryable(), resolveErr
				}
			}
			return results, checkpointErr
		}
		if fetched.Record == nil {
			return retryable(), errDurableGapMalformed
		}
		if !recoveryEventValid(fetched.Record, wanted, event, beforePosition, false) {
			return retryable(), errDurableGapMalformed
		}
		fetchedBytes += int64(len(fetched.Record.Event.Bytes))
		if fetchedBytes > durableGapRecoveryMaxBytes {
			return retryable(), daemon.ErrDurableGapFull
		}
		outcomes := importer([]proto.RemoteEvent{fetched.Record.Event})
		if len(outcomes) != 1 {
			return retryable(), errDurableGapPending
		}
		switch outcomes[0] {
		case syncd.ImportApplied, syncd.ImportDeduped:
			for index := len(pending) - 1; index >= 0; index-- {
				ancestorOutcome := importer([]proto.RemoteEvent{pending[index]})
				if len(ancestorOutcome) != 1 || !durableGapApplied(ancestorOutcome[0]) {
					return retryable(), errDurableGapPending
				}
			}
			replayed := importer(delivery.Events)
			if !durableGapEventRecovered(replayed, missingEventIndex, len(delivery.Events)) {
				results, checkpointErr := recoverDurableCheckpoint(recoveryCtx, remote, cursorStore, importer, binding, key, delivery, missingEventIndex, wanted, recoveryEvidence)
				if checkpointErr == nil {
					if resolveErr := spool.Resolve(key, delivery.DeliveryID); resolveErr != nil {
						return retryable(), resolveErr
					}
				}
				return results, checkpointErr
			}
			if err := spool.Resolve(key, delivery.DeliveryID); err != nil {
				return retryable(), err
			}
			return replayed, nil
		case syncd.ImportDeferredNeedsBaseline:
			pending = append(pending, fetched.Record.Event)
			wanted = fetched.Record.Event.ParentHash
			beforePosition = fetched.Record.Position
		case syncd.ImportRetryable:
			return retryable(), errDurableGapPending
		default:
			results, checkpointErr := recoverDurableCheckpoint(recoveryCtx, remote, cursorStore, importer, binding, key, delivery, missingEventIndex, wanted, recoveryEvidence)
			if checkpointErr == nil {
				if resolveErr := spool.Resolve(key, delivery.DeliveryID); resolveErr != nil {
					return retryable(), resolveErr
				}
			}
			return results, checkpointErr
		}
	}
	return retryable(), daemon.ErrDurableGapFull
}
