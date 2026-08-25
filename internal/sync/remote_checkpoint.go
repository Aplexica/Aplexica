package syncd

// Explicit outbound checkpoint materialization is deliberately separate from
// the normal two-lane fan-out path. It is invoked only for a durable recovery
// obligation after live replay can no longer be proven. The caller supplies
// the exact server position being replaced and, when available, the exact
// recipient/security generation recorded by the dirty marker. This package is
// the only layer allowed to read canonical plaintext and seal the full current
// state; the daemon and cloud continue to handle opaque ciphertext only.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/aplexica/aplexica/internal/acf"
)

var (
	ErrRemoteCheckpointUnavailable          = errors.New("remote checkpoint: unavailable")
	ErrRemoteCheckpointGenerationSuperseded = errors.New("remote checkpoint: generation superseded")
)

type RemoteCheckpointMaterializeRequest struct {
	ScopeID    string
	ArtifactID string
	BranchID   string
	Kind       string
	Coverage   uint64
	// ExpectedAlignmentHash is set only for an explicit cloud/bootstrap
	// request. A mismatch is rejected immediately after reading the canonical
	// branch head, before event/body reads or sealing work.
	ExpectedAlignmentHash string
	// Generation is the authenticated generation recorded by the durable
	// obligation. A zero value asks the orchestrator to select the current
	// verified generation; callers use that only to repair an old/incomplete
	// marker and must persist the selected generation before publishing.
	Generation RemoteRecoveryGeneration
}

type RemoteCheckpointMaterialization struct {
	Event       OutboundEvent
	HeadEventID string
	HeadHash    string
	Generation  RemoteRecoveryGeneration
}

type remoteCheckpointGenerationJSON struct {
	AccessGeneration   uint64 `json:"access_generation"`
	AccessHash         string `json:"access_hash"`
	SecurityGeneration uint64 `json:"security_generation"`
	SecurityBarrier    string `json:"security_barrier"`
	KeyMode            string `json:"key_mode"`
	KeyVersion         uint64 `json:"key_version"`
}

func remoteCheckpointGenerationDigest(generation RemoteRecoveryGeneration) (string, error) {
	if !validRemoteRecoveryGeneration(generation) {
		return "", ErrRemoteCheckpointUnavailable
	}
	state := remoteCheckpointGenerationJSON{
		AccessGeneration:   generation.AccessGeneration,
		AccessHash:         hex.EncodeToString(generation.AccessSetHash[:]),
		SecurityGeneration: generation.SecurityGeneration,
		SecurityBarrier:    hex.EncodeToString(generation.SecurityBarrierID[:]),
		KeyMode:            generation.KeyMode,
		KeyVersion:         generation.KeyVersion,
	}
	raw, err := json.Marshal(state)
	if err != nil {
		return "", err
	}
	stateDigest := sha256.Sum256(append([]byte("aplexica/security-generation/v1\x00"), raw...))
	generationDigest := sha256.Sum256([]byte("aplexica/checkpoint-generation/v1\x00" + hex.EncodeToString(stateDigest[:])))
	return hex.EncodeToString(generationDigest[:]), nil
}

func (o *Orchestrator) currentRemoteCheckpointGeneration(ctx context.Context, art acf.Artifact) (RemoteRecoveryGeneration, error) {
	if o == nil || !o.cfg.RequireEnvelopeV2 || o.localDeviceID() == "" {
		return RemoteRecoveryGeneration{}, ErrRemoteCheckpointUnavailable
	}
	provider := o.verifiedRosterProvider()
	if provider == nil {
		return RemoteRecoveryGeneration{}, ErrRemoteCheckpointUnavailable
	}
	scopeType, scopeID := "account", ""
	if art.Scope == acf.ScopeNamespace {
		scopeType, scopeID = "namespace", art.NamespaceID
		if err := acf.ValidateWireUUIDv7(scopeID); err != nil {
			return RemoteRecoveryGeneration{}, ErrRemoteCheckpointUnavailable
		}
	}
	snapshot, err := provider.Current(ctx, scopeType, scopeID)
	if err != nil {
		return RemoteRecoveryGeneration{}, fmt.Errorf("%w: verified roster unavailable", ErrRemoteCheckpointUnavailable)
	}
	generation := RemoteRecoveryGeneration{
		AccessGeneration:   snapshot.Roster.Manifest.Manifest.AccessGeneration,
		AccessSetHash:      snapshot.Roster.Manifest.Manifest.AccessSetHash,
		SecurityGeneration: snapshot.CoordinatorGeneration,
		SecurityBarrierID:  snapshot.BarrierID,
		KeyMode:            snapshot.KeyMode,
		KeyVersion:         snapshot.KeyVersion,
	}
	if !validRemoteRecoveryGeneration(generation) {
		return RemoteRecoveryGeneration{}, ErrRemoteCheckpointUnavailable
	}
	return generation, nil
}

func checkpointScopeForArtifact(art acf.Artifact) string {
	if art.Scope == acf.ScopeNamespace {
		return art.NamespaceID
	}
	return "account"
}

func checkpointSequenceForHead(art acf.Artifact, events []acf.Event, head acf.Event) (uint64, bool) {
	for index := len(events) - 1; index >= 0; index-- {
		if events[index].EventID == head.EventID && strings.EqualFold(events[index].Hash, head.Hash) {
			return recoverySequenceForIndex(art.EventCount, len(events), index)
		}
	}
	return 0, false
}

// MaterializeRemoteCheckpoint produces a full retained checkpoint for the
// exact current canonical branch head. It never publishes. The daemon must
// persist the returned opaque bytes before handing them to a plugin and must
// wait for a receipt bound to Event.BodyDigest before clearing the obligation.
func (o *Orchestrator) MaterializeRemoteCheckpoint(ctx context.Context, request RemoteCheckpointMaterializeRequest) (RemoteCheckpointMaterialization, error) {
	if o == nil || o.cfg.Store == nil || ctx == nil || request.ScopeID == "" || request.ArtifactID == "" || request.Coverage == 0 ||
		(request.ExpectedAlignmentHash != "" && !validLowerHexSHA256(strings.ToLower(request.ExpectedAlignmentHash))) {
		return RemoteCheckpointMaterialization{}, ErrRemoteCheckpointUnavailable
	}
	if err := ctx.Err(); err != nil {
		return RemoteCheckpointMaterialization{}, err
	}
	art, found := o.findArtifact(request.ArtifactID)
	if !found || checkpointScopeForArtifact(art) != request.ScopeID || request.Kind != "" && request.Kind != string(art.Kind) {
		return RemoteCheckpointMaterialization{}, ErrRemoteCheckpointUnavailable
	}
	entry, authorized := o.remoteProjectAuthorization(art)
	if !authorized {
		return RemoteCheckpointMaterialization{}, ErrRemoteCheckpointUnavailable
	}
	branchID := normalizeBranchName(request.BranchID)
	if request.ExpectedAlignmentHash != "" {
		metadataHead := art.HeadEventHash
		if branchID != acf.MainBranch {
			metadataHead = art.BranchHeads[branchID]
		}
		// Artifact metadata is durably advanced with the canonical append. This
		// cheap comparison prevents BranchHeadEvent/ReadEvents and any seal work
		// when the explicit request is already stale.
		if !strings.EqualFold(metadataHead, request.ExpectedAlignmentHash) {
			return RemoteCheckpointMaterialization{}, ErrRemoteCheckpointUnavailable
		}
	}
	head, ok, err := o.cfg.Store.BranchHeadEvent(art.Kind, art.ArtifactID, branchID)
	if err != nil || !ok {
		if err != nil {
			return RemoteCheckpointMaterialization{}, err
		}
		return RemoteCheckpointMaterialization{}, ErrRemoteCheckpointUnavailable
	}
	if request.ExpectedAlignmentHash != "" && !strings.EqualFold(head.Hash, request.ExpectedAlignmentHash) {
		return RemoteCheckpointMaterialization{}, ErrRemoteCheckpointUnavailable
	}
	if !o.recoveryRouteAllowed(art, head) {
		return RemoteCheckpointMaterialization{}, ErrRemoteCheckpointUnavailable
	}
	events, err := o.cfg.Store.ReadEvents(art.Kind, art.ArtifactID)
	if err != nil {
		return RemoteCheckpointMaterialization{}, err
	}
	sequence, ok := checkpointSequenceForHead(art, events, head)
	if !ok || sequence == 0 {
		return RemoteCheckpointMaterialization{}, ErrRemoteCheckpointUnavailable
	}

	currentGeneration, err := o.currentRemoteCheckpointGeneration(ctx, art)
	if err != nil {
		return RemoteCheckpointMaterialization{}, err
	}
	if validRemoteRecoveryGeneration(request.Generation) && !sameRemoteRecoveryGeneration(request.Generation, currentGeneration) {
		return RemoteCheckpointMaterialization{}, ErrRemoteCheckpointGenerationSuperseded
	}
	state, err := o.prepareRemoteRecoverySeal(ctx, art, entry, currentGeneration)
	if err != nil {
		return RemoteCheckpointMaterialization{}, fmt.Errorf("%w: %v", ErrRemoteCheckpointUnavailable, err)
	}

	checkpoint := head
	if art.Kind == acf.KindConversation {
		var materialized bool
		var redacted bool
		checkpoint, materialized, redacted, err = o.retainedConversationEvent(art, head)
		if err != nil {
			return RemoteCheckpointMaterialization{}, err
		}
		if !materialized {
			if redacted {
				return RemoteCheckpointMaterialization{}, fmt.Errorf("%w: redaction requires retained clear", ErrRemoteCheckpointUnavailable)
			}
			return RemoteCheckpointMaterialization{}, ErrRemoteCheckpointUnavailable
		}
	}

	wireID := RetainedWireEventID(head.EventID, state.origin)
	header := NewEventHeaderV2(checkpoint, art.Kind, state.namespaceID, wireID, LaneRetained, sequence, state.snapshot.Roster, state.snapshot.BarrierID)
	header.TreeHeadDigest = state.snapshot.TreeHeadDigest
	header.KeyMode = state.snapshot.KeyMode
	header.KeyVersion = state.snapshot.KeyVersion
	var sealed []byte
	if art.Scope == acf.ScopeNamespace {
		sealed, err = SealNamespaceEnvelopeV2(checkpoint, art.Scope, art.Project, header, state.snapshot.Roster, state.identity, state.namespaceKey)
	} else {
		sealed, err = SealEnvelopeV2(checkpoint, art.Scope, art.Project, header, state.snapshot.Roster, state.identity)
	}
	if err != nil {
		return RemoteCheckpointMaterialization{}, err
	}
	checkpointGeneration, err := remoteCheckpointGenerationDigest(currentGeneration)
	if err != nil {
		return RemoteCheckpointMaterialization{}, err
	}
	result := RemoteCheckpointMaterialization{
		HeadEventID: head.EventID,
		HeadHash:    strings.ToLower(head.Hash),
		Generation:  currentGeneration,
		Event: OutboundEvent{
			ProjectID: state.projectID, ProjectAuthorizationGeneration: state.projectGeneration,
			AccessGeneration: currentGeneration.AccessGeneration, AccessSetHash: currentGeneration.AccessSetHash,
			SecurityGeneration: currentGeneration.SecurityGeneration, SecurityBarrierID: currentGeneration.SecurityBarrierID,
			KeyMode: currentGeneration.KeyMode, KeyVersion: currentGeneration.KeyVersion,
			CheckpointCoverage: request.Coverage, CheckpointGeneration: checkpointGeneration,
			NamespaceID: state.namespaceID, BranchID: branchID, ArtifactID: art.ArtifactID,
			EventID: wireID, ParentHash: checkpoint.ParentHash, CheckpointAlignmentHash: head.Hash,
			EventHash: checkpoint.Hash, Kind: string(art.Kind), Type: string(checkpoint.Type), Timestamp: checkpoint.Timestamp,
			Bytes: sealed, Sequence: sequence, Origin: state.origin, SourceAgent: checkpoint.Provenance.SourceAgent, Lane: LaneRetained,
		},
	}
	return result, nil
}
