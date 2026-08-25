package syncd

// Canonical outbound range recovery is deliberately an orchestrator concern:
// only this package can read ACF ancestry, distinguish local authorship, apply
// remote routing policy, and seal an event for the current authenticated
// recipient generation. The daemon owns marker/watermark durability and gives
// this method only generation-bound anchors plus a synchronous durable append
// callback.

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/keyrotation"
	"github.com/aplexica/aplexica/internal/keys"
	"github.com/aplexica/aplexica/internal/project"
	"github.com/aplexica/aplexica/internal/syncrules"
)

type RemoteRecoveryGeneration struct {
	AccessGeneration   uint64
	AccessSetHash      [sha256.Size]byte
	SecurityGeneration uint64
	SecurityBarrierID  [sha256.Size]byte
	KeyMode            string
	KeyVersion         uint64
}

type RemoteRecoveryAnchor struct {
	ArtifactID         string
	BranchID           string
	CanonicalEventID   string
	CanonicalEventHash string
	Generation         RemoteRecoveryGeneration
}

type RemoteRecoveryTarget struct {
	ArtifactID string
	BranchID   string
	EventID    string
	EventHash  string
}

type RemoteRecoveryRequest struct {
	ScopeID    string
	Generation RemoteRecoveryGeneration
	Target     RemoteRecoveryTarget
	Anchors    []RemoteRecoveryAnchor
}

type RemoteCheckpointObligation struct {
	ScopeID       string
	ArtifactID    string
	BranchID      string
	Kind          string
	HeadEventID   string
	HeadEventHash string
	Generation    RemoteRecoveryGeneration
	Reason        string
}

type RemoteRecoveryResult struct {
	// Complete is true only when the current durable anchors already prove
	// every locally-authored event selected by this exact marker generation.
	// A pass that appends even one missing event remains incomplete until a
	// later cloud receipt advances the daemon watermark.
	Complete    bool
	Rebuilt     int
	Obligations []RemoteCheckpointObligation
}

func validRemoteRecoveryGeneration(g RemoteRecoveryGeneration) bool {
	if g.AccessGeneration == 0 || g.AccessSetHash == ([sha256.Size]byte{}) ||
		g.SecurityGeneration == 0 || g.SecurityBarrierID == ([sha256.Size]byte{}) || g.KeyMode == "" {
		return false
	}
	switch g.KeyMode {
	case "recipient-wrap-v2":
		return g.KeyVersion == 0
	case "namespace-key-v1":
		return g.KeyVersion > 0
	default:
		return false
	}
}

func sameRemoteRecoveryGeneration(a, b RemoteRecoveryGeneration) bool { return a == b }

func recoveryScopeForArtifact(art acf.Artifact) string {
	if art.Scope == acf.ScopeNamespace {
		return art.NamespaceID
	}
	return "account"
}

func recoveryAnchorKey(artifactID, branchID string) string {
	return artifactID + "\x00" + normalizeBranchName(branchID)
}

func recoverySequenceForIndex(eventCount uint64, activeEvents, index int) (uint64, bool) {
	if eventCount == 0 || activeEvents <= 0 || index < 0 || index >= activeEvents {
		return 0, false
	}
	active := uint64(activeEvents)
	position := uint64(index)
	if eventCount >= active {
		return eventCount - active + position + 1, true
	}
	// Legacy artifacts began a fresh persistent cadence at their first
	// post-upgrade append. Only the final EventCount active records belong to
	// that cadence; assigning a sequence to the older prefix would fabricate
	// transport identity.
	legacyPrefix := active - eventCount
	if position < legacyPrefix {
		return 0, false
	}
	return position - legacyPrefix + 1, true
}

func (o *Orchestrator) recoveryLocallyAuthored(event acf.Event) bool {
	// Negotiated delta publication requires a paired identity. Refusing an
	// unattributed event is safer than replaying an imported peer event after
	// the in-memory remote-origin cache was lost on restart.
	return o.localDeviceID() != "" && event.Provenance.DeviceID == o.localDeviceID()
}

func (o *Orchestrator) recoveryRouteAllowed(art acf.Artifact, event acf.Event) bool {
	eng := o.rulesEngine()
	if eng == nil {
		return true
	}
	adapterNames := make([]string, 0, len(o.cfg.Adapters))
	for _, adapter := range o.cfg.Adapters {
		adapterNames = append(adapterNames, adapter.Name())
	}
	decision := eng.Evaluate(ruleInputFor(art, event.Provenance.SourceAgent, event.Branch), syncrules.EvaluateOpts{InstalledAgents: adapterNames})
	return decision.RemoteAllowed
}

type remoteRecoverySealState struct {
	art               acf.Artifact
	projectEntry      project.Entry
	namespaceID       string
	snapshot          RosterSnapshot
	namespaceKey      keyrotation.NamespaceKeySnapshot
	identity          keys.DeviceIdentity
	recipients        []recipient
	origin            string
	generation        RemoteRecoveryGeneration
	projectID         string
	projectGeneration uint64
}

func (o *Orchestrator) prepareRemoteRecoverySeal(ctx context.Context, art acf.Artifact, entry project.Entry, expected RemoteRecoveryGeneration) (remoteRecoverySealState, error) {
	if !o.cfg.RequireEnvelopeV2 || o.localDeviceID() == "" || !validRemoteRecoveryGeneration(expected) {
		return remoteRecoverySealState{}, errors.New("remote recovery: authenticated envelope generation unavailable")
	}
	provider := o.verifiedRosterProvider()
	identityProvider := o.v2IdentityProvider()
	if provider == nil || identityProvider == nil {
		return remoteRecoverySealState{}, errors.New("remote recovery: verified roster or identity unavailable")
	}
	namespaceID := ""
	scopeType, scopeID := "account", ""
	if art.Scope == acf.ScopeNamespace {
		namespaceID = art.NamespaceID
		if err := acf.ValidateWireUUIDv7(namespaceID); err != nil {
			return remoteRecoverySealState{}, errors.New("remote recovery: invalid namespace identity")
		}
		scopeType, scopeID = "namespace", namespaceID
	}
	snapshot, err := provider.Current(ctx, scopeType, scopeID)
	if err != nil || snapshot.BarrierID == ([sha256.Size]byte{}) {
		return remoteRecoverySealState{}, errors.New("remote recovery: verified roster is stale")
	}
	actual := RemoteRecoveryGeneration{
		AccessGeneration:   snapshot.Roster.Manifest.Manifest.AccessGeneration,
		AccessSetHash:      snapshot.Roster.Manifest.Manifest.AccessSetHash,
		SecurityGeneration: snapshot.CoordinatorGeneration,
		SecurityBarrierID:  snapshot.BarrierID,
		KeyMode:            snapshot.KeyMode,
		KeyVersion:         snapshot.KeyVersion,
	}
	if !sameRemoteRecoveryGeneration(actual, expected) {
		return remoteRecoverySealState{}, errors.New("remote recovery: marker generation is no longer current")
	}
	identity, err := identityProvider.Identity()
	if err != nil {
		return remoteRecoverySealState{}, errors.New("remote recovery: signing identity unavailable")
	}
	state := remoteRecoverySealState{
		art: art, projectEntry: entry, namespaceID: namespaceID, snapshot: snapshot,
		identity: identity, origin: o.localDeviceID(), generation: actual,
	}
	for _, certificate := range snapshot.Roster.Manifest.Manifest.Devices {
		state.recipients = append(state.recipients, recipient{deviceID: certificate.Certificate.DeviceID, pub: certificate.Certificate.WrapPublicKey})
	}
	if len(state.recipients) == 0 {
		return remoteRecoverySealState{}, errors.New("remote recovery: recipient set unavailable")
	}
	if art.Scope == acf.ScopeNamespace {
		keyProvider := o.namespaceKeyProvider()
		if keyProvider == nil || snapshot.KeyMode != "namespace-key-v1" || snapshot.KeyVersion == 0 {
			return remoteRecoverySealState{}, errors.New("remote recovery: namespace key unavailable")
		}
		state.namespaceKey, err = keyProvider.Current(ctx, namespaceID)
		if err != nil || !state.namespaceKey.Finalized || state.namespaceKey.Version != snapshot.KeyVersion ||
			state.namespaceKey.AccessGeneration != snapshot.Roster.Manifest.Manifest.AccessGeneration ||
			state.namespaceKey.AccessSetHash != snapshot.Roster.Manifest.Manifest.AccessSetHash ||
			state.namespaceKey.IssuedRosterEpoch != snapshot.Roster.Manifest.Manifest.Epoch ||
			state.namespaceKey.IssuedRosterHash != [sha256.Size]byte(snapshot.Roster.Hash) {
			return remoteRecoverySealState{}, errors.New("remote recovery: namespace key generation mismatch")
		}
	} else if snapshot.KeyMode != "recipient-wrap-v2" || snapshot.KeyVersion != 0 {
		return remoteRecoverySealState{}, errors.New("remote recovery: account key mode mismatch")
	}
	if art.Scope == acf.ScopeProject && art.Project != nil && o.cfg.ProjectRegistry != nil {
		state.projectID = entry.ID
		state.projectGeneration = entry.AuthorizationGeneration
	}
	return state, nil
}

func (state remoteRecoverySealState) liveEvent(event acf.Event, sequence uint64) (OutboundEvent, error) {
	branchID := normalizeBranchName(event.Branch)
	header := NewEventHeaderV2(event, state.art.Kind, state.namespaceID, event.EventID, LaneLive, sequence, state.snapshot.Roster, state.snapshot.BarrierID)
	header.TreeHeadDigest = state.snapshot.TreeHeadDigest
	header.KeyMode = state.snapshot.KeyMode
	header.KeyVersion = state.snapshot.KeyVersion
	var sealed []byte
	var err error
	if state.art.Scope == acf.ScopeNamespace {
		sealed, err = SealNamespaceEnvelopeV2(event, state.art.Scope, state.art.Project, header, state.snapshot.Roster, state.identity, state.namespaceKey)
	} else {
		sealed, err = SealEnvelopeV2(event, state.art.Scope, state.art.Project, header, state.snapshot.Roster, state.identity)
	}
	if err != nil {
		return OutboundEvent{}, err
	}
	if len(sealed) > remotePublishLiveMaxBytes {
		return OutboundEvent{}, errors.New("remote recovery: live event exceeds transport ceiling")
	}
	return OutboundEvent{
		ProjectID: state.projectID, ProjectAuthorizationGeneration: state.projectGeneration,
		AccessGeneration: state.generation.AccessGeneration, AccessSetHash: state.generation.AccessSetHash,
		SecurityBarrierID: state.generation.SecurityBarrierID, SecurityGeneration: state.generation.SecurityGeneration,
		KeyMode: state.generation.KeyMode, KeyVersion: state.generation.KeyVersion,
		NamespaceID: state.namespaceID, BranchID: branchID, ArtifactID: event.ArtifactID,
		EventID: event.EventID, ParentHash: event.ParentHash, EventHash: event.Hash,
		Kind: string(state.art.Kind), Type: string(event.Type), Timestamp: event.Timestamp,
		Bytes: sealed, Sequence: sequence, Origin: state.origin, SourceAgent: event.Provenance.SourceAgent, Lane: LaneLive,
	}, nil
}

func obligationForBranch(scopeID string, generation RemoteRecoveryGeneration, art acf.Artifact, branchID, reason string, events []acf.Event) RemoteCheckpointObligation {
	branchID = normalizeBranchName(branchID)
	result := RemoteCheckpointObligation{ScopeID: scopeID, ArtifactID: art.ArtifactID, BranchID: branchID, Kind: string(art.Kind), Generation: generation, Reason: reason}
	for i := len(events) - 1; i >= 0; i-- {
		if normalizeBranchName(events[i].Branch) == branchID {
			result.HeadEventID = events[i].EventID
			result.HeadEventHash = events[i].Hash
			break
		}
	}
	return result
}

// RebuildRemoteOutbound scans active canonical history after the exact durable
// anchors supplied by the daemon. appendDurable must synchronously persist the
// intent before returning. Imported events are never emitted, ancestry is
// never invented, and any unprovable range becomes an explicit checkpoint
// obligation.
func (o *Orchestrator) RebuildRemoteOutbound(ctx context.Context, request RemoteRecoveryRequest, appendDurable func(OutboundEvent) error) (RemoteRecoveryResult, error) {
	result := RemoteRecoveryResult{}
	if o == nil || o.cfg.Store == nil || appendDurable == nil || request.ScopeID == "" {
		return result, errors.New("remote recovery: invalid request")
	}
	if !validRemoteRecoveryGeneration(request.Generation) || request.Target.ArtifactID == "" || request.Target.EventID == "" ||
		!validLowerHexSHA256(strings.ToLower(request.Target.EventHash)) {
		result.Obligations = append(result.Obligations, RemoteCheckpointObligation{ScopeID: request.ScopeID, Generation: request.Generation, Reason: "marker-authority-incomplete"})
		return result, nil
	}
	request.Target.BranchID = normalizeBranchName(request.Target.BranchID)
	request.Target.EventHash = strings.ToLower(request.Target.EventHash)

	anchors := make(map[string]RemoteRecoveryAnchor, len(request.Anchors))
	for _, anchor := range request.Anchors {
		anchor.BranchID = normalizeBranchName(anchor.BranchID)
		anchor.CanonicalEventHash = strings.ToLower(anchor.CanonicalEventHash)
		if anchor.ArtifactID == "" || anchor.CanonicalEventID == "" || !validLowerHexSHA256(anchor.CanonicalEventHash) ||
			!sameRemoteRecoveryGeneration(anchor.Generation, request.Generation) {
			return result, errors.New("remote recovery: invalid generation-bound anchor")
		}
		key := recoveryAnchorKey(anchor.ArtifactID, anchor.BranchID)
		if _, duplicate := anchors[key]; duplicate {
			return result, errors.New("remote recovery: duplicate anchor")
		}
		anchors[key] = anchor
	}

	type artifactCandidate struct{ art acf.Artifact }
	var candidates []artifactCandidate
	for _, kind := range []acf.Kind{acf.KindMemory, acf.KindSkill, acf.KindTool, acf.KindConversation} {
		artifacts, err := o.cfg.Store.ListArtifacts(kind)
		if err != nil {
			return result, err
		}
		for _, art := range artifacts {
			if recoveryScopeForArtifact(art) == request.ScopeID {
				candidates = append(candidates, artifactCandidate{art: art})
			}
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].art.ArtifactID != candidates[j].art.ArtifactID {
			return candidates[i].art.ArtifactID < candidates[j].art.ArtifactID
		}
		return candidates[i].art.Kind < candidates[j].art.Kind
	})

	seenArtifacts := make(map[string]bool, len(candidates))
	seenAnchors := make(map[string]bool, len(anchors))
	targetSeen := false
	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		art := candidate.art
		seenArtifacts[art.ArtifactID] = true
		entry, authorized := o.remoteProjectAuthorization(art)
		events, err := o.cfg.Store.ReadEvents(art.Kind, art.ArtifactID)
		if err != nil {
			return result, err
		}
		if len(events) == 0 {
			if art.ArtifactID == request.Target.ArtifactID {
				result.Obligations = append(result.Obligations, obligationForBranch(request.ScopeID, request.Generation, art, request.Target.BranchID, "active-history-unavailable", events))
			}
			continue
		}
		for _, event := range events {
			branchID := normalizeBranchName(event.Branch)
			if art.ArtifactID == request.Target.ArtifactID && branchID == request.Target.BranchID &&
				event.EventID == request.Target.EventID && strings.EqualFold(event.Hash, request.Target.EventHash) &&
				o.recoveryLocallyAuthored(event) {
				targetSeen = true
				break
			}
		}
		// A revoked project is no longer selected for remote publication. Its
		// exact target can satisfy the marker identity check, but no delta or
		// checkpoint may be emitted under stale authorization.
		if !authorized {
			continue
		}

		anchorIndexes := make(map[string]int)
		provenHashes := make(map[string]struct{})
		blockedBranches := make(map[string]bool)
		for key, anchor := range anchors {
			if anchor.ArtifactID != art.ArtifactID {
				continue
			}
			found := -1
			for index, event := range events {
				if normalizeBranchName(event.Branch) == anchor.BranchID && event.EventID == anchor.CanonicalEventID && strings.EqualFold(event.Hash, anchor.CanonicalEventHash) {
					found = index
					break
				}
			}
			if found < 0 {
				result.Obligations = append(result.Obligations, obligationForBranch(request.ScopeID, request.Generation, art, anchor.BranchID, "durable-anchor-compacted-or-missing", events))
				blockedBranches[anchor.BranchID] = true
				continue
			}
			anchorIndexes[anchor.BranchID] = found
			provenHashes[anchor.CanonicalEventHash] = struct{}{}
			seenAnchors[key] = true
		}
		if o.inUnresolvedConflict(art.ArtifactID) {
			// Conflict policy forbids choosing a branch winner. Keep recovery
			// explicitly blocked only when this generation still has local work
			// beyond its exact durable anchor.
			for index, event := range events {
				branchID := normalizeBranchName(event.Branch)
				anchorIndex, anchored := anchorIndexes[branchID]
				if blockedBranches[branchID] || (anchored && index <= anchorIndex) ||
					!o.recoveryLocallyAuthored(event) || !o.recoveryRouteAllowed(art, event) {
					continue
				}
				result.Obligations = append(result.Obligations, obligationForBranch(request.ScopeID, request.Generation, art, branchID, "artifact-conflict-unresolved", events))
				blockedBranches[branchID] = true
			}
			continue
		}

		var sealState remoteRecoverySealState
		sealReady := false
		for index, event := range events {
			branchID := normalizeBranchName(event.Branch)
			anchorIndex, anchored := anchorIndexes[branchID]
			if blockedBranches[branchID] || anchored && index <= anchorIndex {
				continue
			}
			if !o.recoveryLocallyAuthored(event) || !o.recoveryRouteAllowed(art, event) {
				continue
			}
			for _, dependency := range []string{event.ParentHash, event.MergeFromHash} {
				if dependency == "" {
					continue
				}
				if _, proven := provenHashes[strings.ToLower(dependency)]; !proven {
					result.Obligations = append(result.Obligations, obligationForBranch(request.ScopeID, request.Generation, art, branchID, "canonical-parent-unavailable", events))
					blockedBranches[branchID] = true
					break
				}
			}
			if blockedBranches[branchID] {
				continue
			}
			sequence, sequenceOK := recoverySequenceForIndex(art.EventCount, len(events), index)
			if !sequenceOK {
				result.Obligations = append(result.Obligations, obligationForBranch(request.ScopeID, request.Generation, art, branchID, "canonical-sequence-unavailable", events))
				blockedBranches[branchID] = true
				continue
			}
			if !sealReady {
				sealState, err = o.prepareRemoteRecoverySeal(ctx, art, entry, request.Generation)
				if err != nil {
					if ctx.Err() != nil {
						return result, ctx.Err()
					}
					result.Obligations = append(result.Obligations, obligationForBranch(request.ScopeID, request.Generation, art, branchID, "generation-or-seal-authority-unavailable", events))
					blockedBranches[branchID] = true
					continue
				}
				sealReady = true
			}
			outbound, buildErr := sealState.liveEvent(event, sequence)
			if buildErr != nil {
				result.Obligations = append(result.Obligations, obligationForBranch(request.ScopeID, request.Generation, art, branchID, "live-path-unavailable", events))
				blockedBranches[branchID] = true
				continue
			}
			if err := appendDurable(outbound); err != nil {
				return result, fmt.Errorf("remote recovery: durable append %s: %w", event.EventID, err)
			}
			result.Rebuilt++
			provenHashes[strings.ToLower(event.Hash)] = struct{}{}
		}
	}

	if !targetSeen || !seenArtifacts[request.Target.ArtifactID] {
		result.Obligations = append(result.Obligations, RemoteCheckpointObligation{
			ScopeID: request.ScopeID, ArtifactID: request.Target.ArtifactID, BranchID: request.Target.BranchID,
			HeadEventID: request.Target.EventID, HeadEventHash: request.Target.EventHash,
			Generation: request.Generation, Reason: "marker-target-compacted-or-missing",
		})
	}
	for key, anchor := range anchors {
		if !seenAnchors[key] && !seenArtifacts[anchor.ArtifactID] {
			result.Obligations = append(result.Obligations, RemoteCheckpointObligation{
				ScopeID: request.ScopeID, ArtifactID: anchor.ArtifactID, BranchID: anchor.BranchID,
				HeadEventID: anchor.CanonicalEventID, HeadEventHash: anchor.CanonicalEventHash,
				Generation: request.Generation, Reason: "anchored-artifact-unavailable",
			})
		}
	}
	result.Complete = result.Rebuilt == 0 && len(result.Obligations) == 0
	return result, nil
}
