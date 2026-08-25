package daemon

// This file closes the durable outbound checkpoint-obligation lifecycle. The
// canonical orchestrator materializes and seals the exact current head; this
// layer persists only ciphertext plus content-free progress. Terminal order:
//
//   obligation prepared -> outbox/staged bytes fsync -> cloud commit receipt
//   -> receipt fsync -> exact outbox removal -> rescan marker CAS -> obligation
//   removal
//
// Every prefix is restart-safe. In particular, an obligation is never removed
// before the marker is clean, and a marker is never cleaned by a receipt for a
// checkpoint whose aligned canonical head has since been superseded.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/aplexica/aplexica/internal/plugin/proto"
	syncd "github.com/aplexica/aplexica/internal/sync"
)

type remoteCheckpointMaterializer interface {
	MaterializeRemoteCheckpoint(context.Context, syncd.RemoteCheckpointMaterializeRequest) (syncd.RemoteCheckpointMaterialization, error)
}

func obligationGeneration(value RemoteCheckpointObligationV1) syncd.RemoteRecoveryGeneration {
	var accessHash, barrier [sha256.Size]byte
	if decoded, ok := decodeRecoveryDigest(value.AccessSetHash); ok {
		accessHash = decoded
	}
	if decoded, ok := decodeRecoveryDigest(value.SecurityBarrier); ok {
		barrier = decoded
	}
	return syncd.RemoteRecoveryGeneration{
		AccessGeneration: value.AccessGeneration, AccessSetHash: accessHash,
		SecurityGeneration: value.SecurityGeneration, SecurityBarrierID: barrier,
		KeyMode: value.KeyMode, KeyVersion: value.KeyVersion,
	}
}

func setObligationGeneration(value *RemoteCheckpointObligationV1, generation syncd.RemoteRecoveryGeneration) {
	if value == nil {
		return
	}
	value.AccessGeneration = generation.AccessGeneration
	value.AccessSetHash = hex.EncodeToString(generation.AccessSetHash[:])
	value.SecurityGeneration = generation.SecurityGeneration
	value.SecurityBarrier = hex.EncodeToString(generation.SecurityBarrierID[:])
	value.KeyMode = generation.KeyMode
	value.KeyVersion = generation.KeyVersion
}

func checkpointScope(event proto.RemoteEvent) string {
	if event.NamespaceID == "" {
		return "account"
	}
	return event.NamespaceID
}

func sameCheckpointGeneration(event proto.RemoteEvent, value RemoteCheckpointObligationV1) bool {
	return event.AccessGeneration == value.AccessGeneration &&
		hex.EncodeToString(event.AccessSetHash[:]) == value.AccessSetHash &&
		event.SecurityGeneration == value.SecurityGeneration &&
		hex.EncodeToString(event.SecurityBarrierID[:]) == value.SecurityBarrier &&
		event.KeyMode == value.KeyMode && event.KeyVersion == value.KeyVersion
}

type checkpointGenerationJSON struct {
	AccessGeneration   uint64 `json:"access_generation"`
	AccessHash         string `json:"access_hash"`
	SecurityGeneration uint64 `json:"security_generation"`
	SecurityBarrier    string `json:"security_barrier"`
	KeyMode            string `json:"key_mode"`
	KeyVersion         uint64 `json:"key_version"`
}

func checkpointGenerationForEvent(event proto.RemoteEvent) string {
	if !validRemoteEventRecoveryGeneration(event) {
		return ""
	}
	state := checkpointGenerationJSON{
		AccessGeneration: event.AccessGeneration, AccessHash: hex.EncodeToString(event.AccessSetHash[:]),
		SecurityGeneration: event.SecurityGeneration, SecurityBarrier: hex.EncodeToString(event.SecurityBarrierID[:]),
		KeyMode: event.KeyMode, KeyVersion: event.KeyVersion,
	}
	raw, err := json.Marshal(state)
	if err != nil {
		return ""
	}
	stateDigest := sha256.Sum256(append([]byte("aplexica/security-generation/v1\x00"), raw...))
	generationDigest := sha256.Sum256([]byte("aplexica/checkpoint-generation/v1\x00" + hex.EncodeToString(stateDigest[:])))
	return hex.EncodeToString(generationDigest[:])
}

func checkpointGenerationForObligation(value RemoteCheckpointObligationV1) string {
	accessHash, accessOK := decodeRecoveryDigest(value.AccessSetHash)
	barrier, barrierOK := decodeRecoveryDigest(value.SecurityBarrier)
	if !accessOK || !barrierOK {
		return ""
	}
	return checkpointGenerationForEvent(proto.RemoteEvent{
		AccessGeneration: value.AccessGeneration, AccessSetHash: accessHash,
		SecurityGeneration: value.SecurityGeneration, SecurityBarrierID: barrier,
		KeyMode: value.KeyMode, KeyVersion: value.KeyVersion,
	})
}

func validDurableCheckpointReceipt(event proto.RemoteEvent, outcome proto.RemotePublishOutcome) bool {
	return event.Lane == syncd.LaneRetained && !event.Clear && event.CheckpointCoverage > 0 &&
		validateWatermarkDigest(strings.ToLower(event.CheckpointGeneration)) && strings.EqualFold(event.CheckpointGeneration, checkpointGenerationForEvent(event)) &&
		validateWatermarkDigest(strings.ToLower(event.CheckpointAlignmentHash)) &&
		validateWatermarkDigest(strings.ToLower(event.EventHash)) && validRemoteEventRecoveryGeneration(event) &&
		outcome.EventID == event.EventID && outcome.Durability == proto.RemoteDurabilityCommitted &&
		outcome.CommitCursor != "" && outcome.CommitPosition > event.CheckpointCoverage &&
		outcome.StreamID != "" && outcome.StreamEpoch != "" &&
		validateWatermarkDigest(strings.ToLower(event.BodyDigest)) &&
		strings.EqualFold(outcome.BodyDigest, event.BodyDigest) &&
		validateWatermarkDigest(strings.ToLower(outcome.EventIdentityDigest)) &&
		validateWatermarkDigest(strings.ToLower(outcome.MetadataDigest))
}

func (a *RemotePublishAdapter) checkpointObligationForEvent(event proto.RemoteEvent) (RemoteCheckpointObligationV1, bool, error) {
	if a == nil || a.checkpointObligations == nil || event.Lane != syncd.LaneRetained || event.CheckpointCoverage == 0 || event.CheckpointGeneration == "" {
		return RemoteCheckpointObligationV1{}, false, nil
	}
	values, err := a.checkpointObligations.List()
	if err != nil {
		return RemoteCheckpointObligationV1{}, false, err
	}
	for _, value := range values {
		if value.ScopeID == checkpointScope(event) && value.ArtifactID == event.ArtifactID &&
			recoveryBranchID(value.BranchID) == recoveryBranchID(event.BranchID) && value.Kind == event.Kind &&
			(value.CheckpointState == "prepared" || value.CheckpointState == "committed" || value.CheckpointState == "verified") && value.CheckpointEventID == event.EventID &&
			value.CheckpointCoverage == event.CheckpointCoverage && value.CheckpointSourceSequence == event.Sequence && value.CheckpointGeneration == strings.ToLower(event.CheckpointGeneration) &&
			value.CheckpointBodyDigest == strings.ToLower(event.BodyDigest) && value.CheckpointHeadHash == strings.ToLower(event.CheckpointAlignmentHash) &&
			sameCheckpointGeneration(event, value) {
			return value, true, nil
		}
	}
	return RemoteCheckpointObligationV1{}, false, nil
}

// persistCheckpointReceipt fsyncs exact receipt evidence before the outbox can
// be removed. The recovery worker, not the publish callback, clears the marker
// because it must first prove the canonical head was not superseded.
func (a *RemotePublishAdapter) persistCheckpointReceipt(event proto.RemoteEvent, outcome proto.RemotePublishOutcome) (bool, error) {
	value, ok, err := a.checkpointObligationForEvent(event)
	if err != nil || !ok {
		return false, err
	}
	if !validDurableCheckpointReceipt(event, outcome) {
		return true, ErrDurablePublishWatermarkInvalid
	}
	_, err = a.checkpointObligations.update(value.ScopeID, value.ArtifactID, value.BranchID, func(current *RemoteCheckpointObligationV1) error {
		if current.CheckpointEventID != event.EventID ||
			current.CheckpointBodyDigest != strings.ToLower(event.BodyDigest) || current.CheckpointCoverage != event.CheckpointCoverage ||
			current.CheckpointGeneration != strings.ToLower(event.CheckpointGeneration) || current.CheckpointHeadHash != strings.ToLower(event.CheckpointAlignmentHash) ||
			!sameCheckpointGeneration(event, *current) {
			return errors.New("remote checkpoint obligation: prepared checkpoint superseded")
		}
		if current.CheckpointState == "committed" || current.CheckpointState == "verified" {
			if current.CheckpointCommitCursor != outcome.CommitCursor || current.CheckpointPosition != outcome.CommitPosition ||
				current.CheckpointStreamID != outcome.StreamID || current.CheckpointStreamEpoch != outcome.StreamEpoch ||
				current.CheckpointIdentityHash != strings.ToLower(outcome.EventIdentityDigest) || current.CheckpointMetadataHash != strings.ToLower(outcome.MetadataDigest) {
				return errors.New("remote checkpoint obligation: committed receipt conflict")
			}
			return nil
		}
		if current.CheckpointState != "prepared" {
			return errors.New("remote checkpoint obligation: invalid checkpoint state")
		}
		current.CheckpointState = "committed"
		current.CheckpointCommitCursor = outcome.CommitCursor
		current.CheckpointPosition = outcome.CommitPosition
		current.CheckpointStreamID = outcome.StreamID
		current.CheckpointStreamEpoch = outcome.StreamEpoch
		current.CheckpointIdentityHash = strings.ToLower(outcome.EventIdentityDigest)
		current.CheckpointMetadataHash = strings.ToLower(outcome.MetadataDigest)
		current.CheckpointCommittedAt = a.checkpointObligations.clock()
		return nil
	})
	return true, err
}

func (a *RemotePublishAdapter) checkpointCoverage(value RemoteCheckpointObligationV1, descriptor proto.RemoteStreamDescriptorV1) (uint64, bool) {
	if value.RequestID != "" {
		return value.RequestCoverage, value.RequestCoverage > 0 && value.RequestStreamID == descriptor.StreamID && value.RequestStreamEpoch == descriptor.StreamEpoch
	}
	if a == nil || a.watermarks == nil {
		return 0, false
	}
	watermarks, err := a.watermarks.List()
	if err != nil {
		return 0, false
	}
	var coverage uint64
	for _, watermark := range watermarks {
		if watermark.Key.StreamID == descriptor.StreamID && watermark.Key.StreamEpoch == descriptor.StreamEpoch &&
			watermark.Key.ArtifactID == value.ArtifactID && recoveryBranchID(watermark.Key.BranchID) == recoveryBranchID(value.BranchID) &&
			watermark.Position > coverage {
			coverage = watermark.Position
		}
	}
	return coverage, coverage > 0
}

func (a *RemotePublishAdapter) pendingCheckpoint(value RemoteCheckpointObligationV1) (proto.RemoteEvent, bool, error) {
	if a == nil || a.outbox == nil || value.CheckpointEventID == "" {
		return proto.RemoteEvent{}, false, nil
	}
	entries, err := a.outbox.List()
	if err != nil {
		return proto.RemoteEvent{}, false, err
	}
	for _, entry := range entries {
		event := entry.Event
		if event.EventID != value.CheckpointEventID {
			continue
		}
		if event.Lane != syncd.LaneRetained || event.ArtifactID != value.ArtifactID ||
			recoveryBranchID(event.BranchID) != recoveryBranchID(value.BranchID) || event.CheckpointCoverage != value.CheckpointCoverage ||
			event.Sequence != value.CheckpointSourceSequence ||
			event.CheckpointGeneration != value.CheckpointGeneration || event.CheckpointAlignmentHash != value.CheckpointHeadHash ||
			event.BodyDigest != value.CheckpointBodyDigest || !sameCheckpointGeneration(event, value) {
			return proto.RemoteEvent{}, false, errors.New("remote checkpoint obligation: pending outbox authority conflict")
		}
		return event, true, nil
	}
	return proto.RemoteEvent{}, false, nil
}

func (a *RemotePublishAdapter) prepareCheckpointObligation(value RemoteCheckpointObligationV1, markerGeneration uint64, materialized syncd.RemoteCheckpointMaterialization) (RemoteCheckpointObligationV1, error) {
	event := toRemoteEvent(materialized.Event)
	if value.RequestID != "" && (!strings.EqualFold(materialized.HeadHash, value.RequestAlignmentHash) || materialized.Generation != obligationGeneration(value) ||
		event.CheckpointCoverage != value.RequestCoverage || !strings.EqualFold(event.CheckpointAlignmentHash, value.RequestAlignmentHash) ||
		!strings.EqualFold(event.CheckpointGeneration, value.RequestCheckpointGeneration)) {
		return RemoteCheckpointObligationV1{}, errors.New("remote checkpoint obligation: request materialization mismatch")
	}
	if markerGeneration == 0 || !validDurableCheckpointReceipt(event, proto.RemotePublishOutcome{
		EventID: event.EventID, Durability: proto.RemoteDurabilityCommitted, CommitCursor: "pending",
		CommitPosition: event.CheckpointCoverage + 1, StreamID: "pending", StreamEpoch: "pending",
		BodyDigest: event.BodyDigest, EventIdentityDigest: strings.Repeat("a", sha256.Size*2), MetadataDigest: strings.Repeat("b", sha256.Size*2),
	}) {
		return RemoteCheckpointObligationV1{}, errors.New("remote checkpoint obligation: invalid materialization")
	}
	return a.checkpointObligations.update(value.ScopeID, value.ArtifactID, value.BranchID, func(current *RemoteCheckpointObligationV1) error {
		if value.RequestID != "" && (!sameObligationRequestAuthority(*current, value) || hasObligationCheckpointProgress(*current)) {
			return errors.New("remote checkpoint obligation: request authority superseded")
		}
		setObligationGeneration(current, materialized.Generation)
		current.Kind = event.Kind
		current.HeadEventID = materialized.HeadEventID
		current.HeadEventHash = strings.ToLower(materialized.HeadHash)
		current.MarkerGeneration = markerGeneration
		current.CheckpointState = "prepared"
		current.CheckpointEventID = event.EventID
		current.CheckpointHeadEventID = materialized.HeadEventID
		current.CheckpointHeadHash = strings.ToLower(materialized.HeadHash)
		current.CheckpointBodyDigest = strings.ToLower(event.BodyDigest)
		current.CheckpointCoverage = event.CheckpointCoverage
		current.CheckpointSourceSequence = event.Sequence
		current.CheckpointGeneration = strings.ToLower(event.CheckpointGeneration)
		current.CheckpointPreparedAt = a.checkpointObligations.clock()
		current.CheckpointCommitCursor = ""
		current.CheckpointPosition = 0
		current.CheckpointStreamID = ""
		current.CheckpointStreamEpoch = ""
		current.CheckpointIdentityHash = ""
		current.CheckpointMetadataHash = ""
		current.CheckpointCommittedAt = time.Time{}
		return nil
	})
}

// enqueueCheckpointObligation bypasses the ordinary dirty-scope gate: the
// checkpoint is the recovery operation that will satisfy that gate. It still
// performs synchronous durable staging/outbox persistence before enqueue.
func (a *RemotePublishAdapter) enqueueCheckpointObligation(ctx context.Context, event syncd.OutboundEvent) error {
	if a == nil || a.outbox == nil || ctx.Err() != nil {
		return errors.New("remote checkpoint obligation: durable publisher unavailable")
	}
	wire := toRemoteEvent(event)
	if !a.isProjectAuthorized(wire) {
		return errors.New("remote checkpoint obligation: project authorization changed")
	}
	if stagedRemoteCheckpointCandidate(wire) {
		policy, ok := a.client.(remotePublishStagedCheckpointPolicy)
		if !ok || !policy.SupportsLargeRetainedCheckpoint(wire) {
			return errors.New("remote checkpoint obligation: staged transfer unavailable")
		}
		prepared, err := policy.PrepareLargeRetainedCheckpoint(ctx, wire)
		if err != nil {
			return err
		}
		wire = prepared
	}
	if remoteEventOversize(wire) {
		return errors.New("remote checkpoint obligation: checkpoint exceeds active transport ceiling")
	}
	if _, err := a.outbox.append(wire, false); err != nil {
		return err
	}
	persisted, err := a.outbox.pendingRecoveryAuthority(wire)
	if err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case a.queue <- persisted:
		return nil
	default:
		return nil // exact bytes are durable; a later obligation pass enqueues
	}
}

func (a *RemotePublishAdapter) materializeCheckpoint(ctx context.Context, source remoteCheckpointMaterializer, value RemoteCheckpointObligationV1, coverage uint64) (syncd.RemoteCheckpointMaterialization, error) {
	request := syncd.RemoteCheckpointMaterializeRequest{
		ScopeID: value.ScopeID, ArtifactID: value.ArtifactID, BranchID: value.BranchID, Kind: value.Kind,
		Coverage: coverage, Generation: obligationGeneration(value),
	}
	if value.RequestID != "" {
		request.ExpectedAlignmentHash = value.RequestAlignmentHash
	}
	materialized, err := source.MaterializeRemoteCheckpoint(ctx, request)
	if value.RequestID != "" {
		return materialized, err
	}
	if errors.Is(err, syncd.ErrRemoteCheckpointGenerationSuperseded) || !validRemoteEventRecoveryGeneration(toRemoteEvent(materialized.Event)) && err == nil {
		request.Generation = syncd.RemoteRecoveryGeneration{}
		return source.MaterializeRemoteCheckpoint(ctx, request)
	}
	return materialized, err
}

func (a *RemotePublishAdapter) finishCommittedCheckpoint(ctx context.Context, source remoteCheckpointMaterializer, value RemoteCheckpointObligationV1, marker RemoteRescanMarkerV1, coverage uint64) {
	materialized, err := a.materializeCheckpoint(ctx, source, value, coverage)
	if err != nil {
		return
	}
	if materialized.HeadEventID != value.CheckpointHeadEventID || !strings.EqualFold(materialized.HeadHash, value.CheckpointHeadHash) || materialized.Generation != obligationGeneration(value) {
		if value.RequestID != "" {
			// The cloud request is bound to one exact local canonical head. If the
			// head advanced, wait for a newer notification instead of inventing a
			// different coverage/alignment authority.
			return
		}
		prepared, prepErr := a.prepareCheckpointObligation(value, marker.MutationGeneration, materialized)
		if prepErr == nil {
			_ = prepared
			_ = a.enqueueCheckpointObligation(ctx, materialized.Event)
		}
		return
	}
	// Rebind a still-current receipt to the latest marker generation. Duplicate
	// callbacks can advance the marker without changing the canonical head.
	if value.MarkerGeneration != marker.MutationGeneration {
		updated, updateErr := a.checkpointObligations.update(value.ScopeID, value.ArtifactID, value.BranchID, func(current *RemoteCheckpointObligationV1) error {
			if current.CheckpointState != "committed" && current.CheckpointState != "verified" || current.CheckpointEventID != value.CheckpointEventID || current.CheckpointHeadHash != value.CheckpointHeadHash {
				return errors.New("remote checkpoint obligation: committed checkpoint superseded")
			}
			current.MarkerGeneration = marker.MutationGeneration
			return nil
		})
		if updateErr != nil {
			return
		}
		value = updated
	}
	_, _ = a.checkpointObligations.update(value.ScopeID, value.ArtifactID, value.BranchID, func(current *RemoteCheckpointObligationV1) error {
		if current.CheckpointState != "committed" && current.CheckpointState != "verified" || current.CheckpointEventID != value.CheckpointEventID || current.MarkerGeneration != value.MarkerGeneration {
			return errors.New("remote checkpoint obligation: committed checkpoint superseded")
		}
		current.CheckpointState = "verified"
		return nil
	})
}

func (a *RemotePublishAdapter) checkpointWatermark(value RemoteCheckpointObligationV1) DurablePublishWatermark {
	return DurablePublishWatermark{
		Key: DurablePublishWatermarkKey{
			StreamID: value.CheckpointStreamID, StreamEpoch: value.CheckpointStreamEpoch,
			ArtifactID: value.ArtifactID, BranchID: value.BranchID,
		},
		CanonicalEventID: value.CheckpointHeadEventID, CanonicalEventHash: value.CheckpointHeadHash,
		Position: value.CheckpointPosition, RecipientFingerprint: value.AccessSetHash,
		AccessGeneration: value.AccessGeneration, SecurityGeneration: value.SecurityGeneration,
		SecurityBarrier: value.SecurityBarrier, KeyMode: value.KeyMode, KeyVersion: value.KeyVersion,
		BodyDigest: value.CheckpointBodyDigest, EventIdentityDigest: value.CheckpointIdentityHash,
		MetadataDigest: value.CheckpointMetadataHash, CommittedAt: value.CheckpointCommittedAt,
	}
}

func (a *RemotePublishAdapter) removeCheckpointCoveredOutbox(value RemoteCheckpointObligationV1) error {
	entries, err := a.outbox.List()
	if err != nil {
		return err
	}
	for _, entry := range entries {
		event := entry.Event
		if event.EventID == value.CheckpointEventID {
			if err := a.outbox.Remove(event.EventID); err != nil {
				return err
			}
			continue
		}
		if event.Lane != syncd.LaneLive || checkpointScope(event) != value.ScopeID || event.ArtifactID != value.ArtifactID ||
			recoveryBranchID(event.BranchID) != recoveryBranchID(value.BranchID) || event.Sequence == 0 ||
			event.Sequence > value.CheckpointSourceSequence || !sameCheckpointGeneration(event, value) {
			continue
		}
		if err := a.outbox.Remove(event.EventID); err != nil {
			return err
		}
	}
	return nil
}

func (a *RemotePublishAdapter) verifiedCheckpointWatermarkInstalled(value RemoteCheckpointObligationV1) bool {
	if a == nil || a.watermarks == nil || value.CheckpointState != "verified" {
		return false
	}
	want := normalizeWatermark(a.checkpointWatermark(value))
	got, err := a.watermarks.Load(want.Key)
	return err == nil && got.Position >= want.Position && got.CanonicalEventID == want.CanonicalEventID &&
		got.CanonicalEventHash == want.CanonicalEventHash && got.AccessGeneration == want.AccessGeneration &&
		got.RecipientFingerprint == want.RecipientFingerprint && got.SecurityGeneration == want.SecurityGeneration &&
		got.SecurityBarrier == want.SecurityBarrier && got.KeyMode == want.KeyMode && got.KeyVersion == want.KeyVersion
}

func (a *RemotePublishAdapter) finalizeVerifiedCheckpointScopes(markers map[string]RemoteRescanMarkerV1) {
	values, err := a.checkpointObligations.List()
	if err != nil {
		return
	}
	byScope := make(map[string][]RemoteCheckpointObligationV1)
	for _, value := range values {
		byScope[value.ScopeID] = append(byScope[value.ScopeID], value)
	}
	for scopeID, scoped := range byScope {
		marker, ok := markers[scopeID]
		if !ok || marker.State != "dirty" || marker.ReasonFlags&rescanReasonCheckpoint == 0 || len(scoped) == 0 {
			continue
		}
		ready := true
		for _, value := range scoped {
			if value.CheckpointState != "verified" || value.MarkerGeneration != marker.MutationGeneration ||
				value.AccessGeneration != scoped[0].AccessGeneration || value.AccessSetHash != scoped[0].AccessSetHash ||
				value.SecurityGeneration != scoped[0].SecurityGeneration || value.SecurityBarrier != scoped[0].SecurityBarrier ||
				value.KeyMode != scoped[0].KeyMode || value.KeyVersion != scoped[0].KeyVersion {
				ready = false
				break
			}
		}
		if !ready {
			continue
		}
		bound, err := a.outbox.mutations.BindCheckpointTarget(scoped[0])
		if err != nil || !bound {
			continue
		}
		for _, value := range scoped {
			if a.verifiedCheckpointWatermarkInstalled(value) {
				continue
			}
			if _, err := a.watermarks.Advance(a.checkpointWatermark(value)); err != nil {
				ready = false
				break
			}
		}
		if !ready {
			continue
		}
		for _, value := range scoped {
			if err := a.removeCheckpointCoveredOutbox(value); err != nil {
				ready = false
				break
			}
		}
		if !ready {
			continue
		}
		satisfied, err := a.outbox.mutations.SatisfyCheckpoint(scopeID, marker.MutationGeneration)
		if err != nil || !satisfied {
			continue
		}
		for _, value := range scoped {
			_, _ = a.checkpointObligations.RemoveCommitted(value.ScopeID, value.ArtifactID, value.BranchID, value.CheckpointEventID, value.MarkerGeneration)
		}
	}
}

// fulfillCheckpointObligationsOnce performs one bounded pass. It runs only in
// negotiated durable-delta modes and leaves every unsafe/incomplete case
// untouched for a later retry or operator-visible intervention.
func (a *RemotePublishAdapter) fulfillCheckpointObligationsOnce(ctx context.Context) {
	if a == nil || ctx.Err() != nil || a.outbox == nil || a.outbox.mutations == nil || a.checkpointObligations == nil {
		return
	}
	source, ok := a.recoverySourceSnapshot().(remoteCheckpointMaterializer)
	if !ok {
		return
	}
	policy, ok := a.client.(remoteOutboundRecoveryPolicy)
	if !ok {
		return
	}
	negotiated := policy.SyncNegotiation()
	markers, err := a.outbox.mutations.ListDirty()
	if err != nil {
		return
	}
	byScope := make(map[string]RemoteRescanMarkerV1, len(markers))
	for _, snapshot := range markers {
		byScope[snapshot.Marker.ScopeID] = snapshot.Marker
	}
	values, err := a.checkpointObligations.List()
	if err != nil {
		return
	}
	for _, value := range values {
		if ctx.Err() != nil {
			return
		}
		requestBound := value.RequestID != ""
		if !requestBound && !a.durableDeltaMode() {
			continue
		}
		if requestBound && negotiated.Mode == proto.RemoteSyncModeLegacy {
			continue
		}
		marker, dirty := byScope[value.ScopeID]
		if !dirty {
			// Crash-after-marker-completion: finish only the exact verified
			// obligation covered by the authenticated clean marker generation.
			state, exists, stateErr := a.outbox.mutations.Snapshot(value.ScopeID)
			if stateErr == nil && exists && state.State == "clean" && value.CheckpointState == "verified" && state.CompletedGeneration >= value.MarkerGeneration && a.verifiedCheckpointWatermarkInstalled(value) {
				_ = a.outbox.Remove(value.CheckpointEventID)
				_, _ = a.checkpointObligations.RemoveCommitted(value.ScopeID, value.ArtifactID, value.BranchID, value.CheckpointEventID, value.MarkerGeneration)
			} else if value.CheckpointState != "verified" {
				// The handler persists the authenticated obligation before it
				// advances the marker. A process death in that narrow interval is
				// repaired here; the next pass materializes from the unchanged
				// request authority instead of silently stranding it.
				_ = a.outbox.mutations.RequireCheckpoint(value.ScopeID)
			}
			continue
		}
		if marker.ReasonFlags&rescanReasonCheckpoint == 0 {
			// Crash after SatisfyCheckpoint but before obligation deletion. The
			// installed canonical-head watermark is the durable recovery anchor;
			// remove only that exact verified obligation and let ordinary range
			// recovery clear the remaining capacity marker.
			if value.CheckpointState == "verified" && a.verifiedCheckpointWatermarkInstalled(value) {
				_ = a.outbox.Remove(value.CheckpointEventID)
				_, _ = a.checkpointObligations.RemoveCommitted(value.ScopeID, value.ArtifactID, value.BranchID, value.CheckpointEventID, value.MarkerGeneration)
			} else {
				_ = a.outbox.mutations.RequireCheckpoint(value.ScopeID)
			}
			continue
		}
		descriptor, exists := recoveryDescriptorForScope(negotiated, value.ScopeID)
		if !exists || requestBound && (descriptor.StreamID != value.RequestStreamID || descriptor.StreamEpoch != value.RequestStreamEpoch) {
			continue
		}
		coverage, exists := a.checkpointCoverage(value, descriptor)
		if !exists {
			continue
		}
		if value.CheckpointState == "committed" || value.CheckpointState == "verified" {
			a.finishCommittedCheckpoint(ctx, source, value, marker, coverage)
			continue
		}
		if value.CheckpointState == "prepared" {
			pending, exists, pendingErr := a.pendingCheckpoint(value)
			if pendingErr != nil {
				continue
			}
			if exists {
				if a.queuesIdle() {
					select {
					case a.queue <- pending:
					default:
					}
				}
				continue
			}
		}
		materialized, err := a.materializeCheckpoint(ctx, source, value, coverage)
		if err != nil {
			continue
		}
		if _, err := a.prepareCheckpointObligation(value, marker.MutationGeneration, materialized); err != nil {
			continue
		}
		_ = a.enqueueCheckpointObligation(ctx, materialized.Event)
	}
	a.finalizeVerifiedCheckpointScopes(byScope)
}

// parkRetainedForCheckpoint preserves a staged retained file after a terminal
// plugin rejection. Deleting it would lose the only exact randomized seal.
func (a *RemotePublishAdapter) parkRetainedForCheckpoint(event proto.RemoteEvent, reason string) error {
	if a == nil || a.outbox == nil || event.Lane != syncd.LaneRetained {
		return errors.New("remote checkpoint obligation: retained park unavailable")
	}
	if err := a.outbox.RequireCanonicalRecovery(event); err != nil {
		return err
	}
	obligation := syncd.RemoteCheckpointObligation{
		ScopeID: checkpointScope(event), ArtifactID: event.ArtifactID, BranchID: event.BranchID, Kind: event.Kind,
		HeadEventHash: event.CheckpointAlignmentHash, Generation: syncd.RemoteRecoveryGeneration{
			AccessGeneration: event.AccessGeneration, AccessSetHash: event.AccessSetHash,
			SecurityGeneration: event.SecurityGeneration, SecurityBarrierID: event.SecurityBarrierID,
			KeyMode: event.KeyMode, KeyVersion: event.KeyVersion,
		}, Reason: reason,
	}
	if obligation.HeadEventHash == "" {
		obligation.HeadEventHash = event.EventHash
	}
	if err := a.persistCheckpointObligation(obligation); err != nil {
		return err
	}
	if err := a.outbox.mutations.RequireCheckpoint(event.NamespaceID); err != nil {
		return err
	}
	a.notifyCheckpointObligation(obligation)
	a.wakeOutboundRecovery()
	return nil
}

var _ remoteCheckpointMaterializer = (*syncd.Orchestrator)(nil)
