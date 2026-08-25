package generationactivation

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/aplexica/aplexica/internal/identity"
	"github.com/aplexica/aplexica/internal/keys"
)

type Transport interface {
	SubmitTrustAnchor(context.Context, SignedObject) error
	SubmitAuthorityTransition(context.Context, SignedObject) error
	SubmitRosterTransition(context.Context, SignedObject) error
	SubmitAtomicAuthorityRosterTransition(context.Context, SignedObject) error
	RegisterDeviceCredential(context.Context, CredentialRegistration) error
	ActivateGeneration(context.Context, []byte) (ActivationReceipt, error)
	GetActivationStatus(context.Context, []byte) (ActivationStatus, error)
}

type ExistingIdentitySource interface {
	LoadExisting() (keys.DeviceIdentity, error)
}

// ActivationEndorsementCollector obtains signatures from other authority
// devices over one exact prepared unsigned activation. It receives only public
// metadata and existing signatures; implementations must never transfer
// private keys.
type ActivationEndorsementCollector interface {
	CollectActivationEndorsements(context.Context, GenerationActivationUnsignedV1, []ActivationEndorsementV1) (GenerationActivationUnsignedV1, []ActivationEndorsementV1, error)
}

type EndorsementJournal interface {
	Load([32]byte) (GenerationActivationUnsignedV1, []ActivationEndorsementV1, error)
	Save([32]byte, GenerationActivationUnsignedV1, []ActivationEndorsementV1) error
	Remove([32]byte) error
}

type Coordinator struct {
	Chain       *identity.ChainStore
	Epoch       SecurityEpochState
	StreamEpoch string
	NamespaceID string
	DeviceID    string
	Identity    ExistingIdentitySource
	State       StateStore
	Transport   Transport
	Collector   ActivationEndorsementCollector
	Endorsement EndorsementJournal
	Now         func() time.Time
}

// RunOnce publishes the complete already-signed chain, registers only this
// authenticated device's exact signed credential, and advances one generation.
// The exact attestation is durable before transport. A different local target
// can never replace an unresolved pending write.
func (c *Coordinator) RunOnce(ctx context.Context) (Result, error) {
	if c == nil || c.Chain == nil || c.Identity == nil || c.State == nil || c.Transport == nil || !validOpaque(c.StreamEpoch, 256) || !validOpaque(c.DeviceID, 256) {
		return Result{}, ErrInvalidState
	}
	now := time.Now().UTC()
	if c.Now != nil {
		now = c.Now().UTC()
	}
	state, err := c.State.Load()
	switch {
	case err == nil:
	case errors.Is(err, os.ErrNotExist):
		state = durableState{Version: stateVersion, StreamEpoch: c.StreamEpoch}
	default:
		return Result{}, fmt.Errorf("generation activation: load durable state: %w", err)
	}
	if state.StreamEpoch != c.StreamEpoch {
		if state.Pending != nil {
			return Result{}, ErrPendingActivation
		}
		state = durableState{Version: stateVersion, StreamEpoch: c.StreamEpoch}
	}
	if state.Pending != nil {
		pendingBlob := append([]byte(nil), state.Pending.AttestationBlob...)
		signed, decodeErr := DecodeCanonical(pendingBlob)
		if decodeErr != nil {
			return Result{}, ErrInvalidState
		}
		// A finalized pending attestation supersedes its threshold-collection
		// journal. Cleanup must be observed before either retrying it or preparing
		// a fresh statement under the same binding: silently leaving an expired
		// proposal here would make the authenticated-absent recovery path reload
		// that stale nonce/freshness window forever.
		if err := c.removeEndorsementJournal(state.Pending.BindingDigest); err != nil {
			return Result{BindingDigest: state.Pending.BindingDigest, AttestationBlob: pendingBlob}, err
		}
		if now.Unix() < signed.Attestation.NotAfterUnix {
			return c.activatePending(ctx, state, state.Pending.BindingDigest, pendingBlob, now)
		}
		status, statusErr := c.Transport.GetActivationStatus(ctx, pendingBlob)
		if statusErr != nil {
			return Result{BindingDigest: state.Pending.BindingDigest, AttestationBlob: pendingBlob}, statusErr
		}
		if status.Committed == status.Absent {
			return Result{}, ErrInvalidState
		}
		if status.Committed {
			return c.acceptPendingReceipt(state, state.Pending.BindingDigest, pendingBlob, status.Receipt, now)
		}
		if status.Receipt != (ActivationReceipt{}) {
			return Result{}, ErrInvalidState
		}
		// Keep the expired statement durable on disk while preparing its fresh
		// replacement. Any failure below leaves the old traffic gate closed;
		// the eventual Save atomically replaces it with the new exact bytes.
		state.Pending = nil
	}

	snapshot, err := c.Chain.PublicationSnapshot(now)
	if err != nil {
		return Result{}, fmt.Errorf("generation activation: verified identity chain: %w", err)
	}

	previous := state.AuthorityDigest
	buildInput := BuildInput{
		AccountID: snapshot.AccountID, NamespaceID: c.NamespaceID, StreamEpoch: c.StreamEpoch,
		Roster: snapshot.Current, SecurityEpoch: c.Epoch, DeviceID: c.DeviceID,
		PreviousAuthorityDigest: previous, Now: now,
	}
	unsigned, err := prepareUnsigned(buildInput)
	if err != nil {
		return Result{}, err
	}
	binding, err := BindingDigest(unsigned)
	if err != nil {
		return Result{}, err
	}
	if state.ActivatedBindingDigest == binding && state.Pending == nil {
		return Result{AlreadyActivated: true, BindingDigest: binding, Receipt: ActivationReceipt{AuthorityDigest: fmt.Sprintf("%x", state.AuthorityDigest), Revision: state.AuthorityRevision}, ActivatedAt: now}, nil
	}

	deviceIdentity, err := c.Identity.LoadExisting()
	if err != nil {
		return Result{}, fmt.Errorf("%w: %v", ErrSigningAuthorityUnavailable, err)
	}
	plan, err := c.publicationPlan(snapshot, deviceIdentity)
	if err != nil {
		return Result{}, err
	}
	if state.PublishedDigest != plan.digest {
		if err := c.publishPlan(ctx, plan); err != nil {
			return Result{}, err
		}
		state.PublishedDigest = plan.digest
		if err := c.State.Save(state); err != nil {
			return Result{}, fmt.Errorf("generation activation: persist publication receipt: %w", err)
		}
	}
	buildInput.DeviceIdentity = deviceIdentity
	var attestationBlob []byte
	var signedBinding [32]byte
	if snapshot.Current.Authority.Threshold == 1 {
		_, attestationBlob, signedBinding, err = Build(buildInput)
	} else {
		attestationBlob, signedBinding, err = c.buildThresholdAttestation(ctx, buildInput, binding)
	}
	if err != nil {
		// A non-authority device still publishes its immutable trust chain and
		// registers its own credential exactly once, but it cannot activate.
		return Result{}, err
	}
	if signedBinding != binding {
		return Result{}, ErrInvalidState
	}
	state.Pending = &pendingActivation{BindingDigest: binding, AttestationBlob: append([]byte(nil), attestationBlob...), PreparedAt: now}
	if err := c.State.Save(state); err != nil {
		return Result{}, fmt.Errorf("generation activation: persist pending attestation: %w", err)
	}
	if err := c.removeEndorsementJournal(binding); err != nil {
		return Result{}, err
	}
	return c.activatePending(ctx, state, binding, attestationBlob, now)
}

func (c *Coordinator) removeEndorsementJournal(binding [32]byte) error {
	if c == nil || c.Endorsement == nil {
		return nil
	}
	if err := c.Endorsement.Remove(binding); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("generation activation: remove endorsement journal: %w", err)
	}
	return nil
}

func (c *Coordinator) buildThresholdAttestation(ctx context.Context, input BuildInput, binding [32]byte) ([]byte, [32]byte, error) {
	if c.Collector == nil || c.Endorsement == nil {
		return nil, [32]byte{}, ErrSigningAuthorityUnavailable
	}
	unsigned, endorsements, err := c.Endorsement.Load(binding)
	proposalLocked := false
	if errors.Is(err, os.ErrNotExist) {
		unsigned, _, err = Prepare(input)
		if err != nil {
			return nil, [32]byte{}, err
		}
		endorsements = nil
		// Persist the tentative local proposal before the first network call.
		// With no collected endorsements it may still be replaced by the
		// relay-elected proposal for this round; once any returned signature is
		// durable the exact proposal is immutable across restarts.
		if err := c.Endorsement.Save(binding, unsigned, nil); err != nil {
			return nil, [32]byte{}, fmt.Errorf("generation activation: persist tentative endorsement proposal: %w", err)
		}
	} else if err != nil {
		return nil, [32]byte{}, fmt.Errorf("generation activation: load endorsement proposal: %w", err)
	} else if preparedBinding, digestErr := BindingDigest(unsigned); digestErr != nil || preparedBinding != binding || validatePrepared(input, unsigned) != nil {
		return nil, [32]byte{}, ErrPendingActivation
	}
	proposalLocked = len(endorsements) > 0
	if local, localErr := Endorse(input, unsigned); localErr == nil {
		endorsements = mergeEndorsements(endorsements, local)
	} else if !errors.Is(localErr, ErrSigningAuthorityUnavailable) {
		return nil, [32]byte{}, localErr
	}
	selected, collected, err := c.Collector.CollectActivationEndorsements(ctx, unsigned, append([]ActivationEndorsementV1(nil), endorsements...))
	if err != nil {
		return nil, [32]byte{}, err
	}
	selectedBinding, bindingErr := BindingDigest(selected)
	if bindingErr != nil || selectedBinding != binding || validatePrepared(input, selected) != nil || proposalLocked && selected != unsigned {
		return nil, [32]byte{}, ErrPendingActivation
	}
	if err := c.Endorsement.Save(binding, selected, collected); err != nil {
		return nil, [32]byte{}, fmt.Errorf("generation activation: persist endorsements: %w", err)
	}
	_, blob, finalizedBinding, err := Finalize(input, selected, collected)
	return blob, finalizedBinding, err
}

func mergeEndorsements(values []ActivationEndorsementV1, value ActivationEndorsementV1) []ActivationEndorsementV1 {
	result := append([]ActivationEndorsementV1(nil), values...)
	for index := range result {
		if result[index].SignerKeyID == value.SignerKeyID {
			result[index] = value
			return result
		}
	}
	return append(result, value)
}

func (c *Coordinator) activatePending(ctx context.Context, state durableState, binding [32]byte, attestationBlob []byte, now time.Time) (Result, error) {
	receipt, err := c.Transport.ActivateGeneration(ctx, attestationBlob)
	if err != nil {
		return Result{BindingDigest: binding, AttestationBlob: append([]byte(nil), attestationBlob...)}, err
	}
	return c.acceptPendingReceipt(state, binding, attestationBlob, receipt, now)
}

func (c *Coordinator) acceptPendingReceipt(state durableState, binding [32]byte, attestationBlob []byte, receipt ActivationReceipt, now time.Time) (Result, error) {
	authorityDigest, err := parseAuthorityDigest(receipt.AuthorityDigest)
	if err != nil || receipt.Revision == 0 || state.AuthorityRevision > 0 && receipt.Revision <= state.AuthorityRevision || state.AuthorityDigest != ([32]byte{}) && authorityDigest == state.AuthorityDigest {
		return Result{}, ErrInvalidState
	}
	state.ActivatedBindingDigest = binding
	state.AuthorityDigest = authorityDigest
	state.AuthorityRevision = receipt.Revision
	state.Pending = nil
	if err := c.State.Save(state); err != nil {
		// The persisted pending blob remains authoritative. An exact retry is
		// safe even after its freshness window because the server recognizes
		// the already-committed attestation source digest before freshness.
		return Result{}, fmt.Errorf("generation activation: persist receipt: %w", err)
	}
	return Result{Receipt: receipt, BindingDigest: binding, AttestationBlob: append([]byte(nil), attestationBlob...), ActivatedAt: now}, nil
}

type publicationPlan struct {
	objects      []SignedObject
	registration CredentialRegistration
	digest       [32]byte
}

func (c *Coordinator) publicationPlan(snapshot identity.PublicationSnapshot, deviceIdentity keys.DeviceIdentity) (publicationPlan, error) {
	plan := publicationPlan{objects: make([]SignedObject, 0, len(snapshot.Objects))}
	for _, object := range snapshot.Objects {
		plan.objects = append(plan.objects, SignedObject{
			ScopeType: object.ScopeType, ScopeID: object.ScopeID, Kind: object.Kind,
			Sequence: object.Sequence, PreviousHash: object.PreviousHash, Hash: object.Hash,
			Blob: append([]byte(nil), object.Blob...),
		})
	}
	manifest := snapshot.Current.Manifest.Manifest
	found := false
	for _, signedCredential := range manifest.Devices {
		credential := signedCredential.Certificate
		if credential.DeviceID != c.DeviceID || credential.SigningKeyID != deviceIdentity.SigningKeyID || credential.SigningPublicKey != [32]byte(deviceIdentity.SigningPublic) {
			continue
		}
		blob, err := identity.CanonicalDeviceCredentialBytes(signedCredential)
		if err != nil {
			return publicationPlan{}, err
		}
		plan.registration = CredentialRegistration{
			CredentialBlob: blob, SigningKeyID: credential.SigningKeyID, WrapKeyID: credential.WrapKeyID,
			EnvelopeVersions: append([]uint16(nil), credential.EnvelopeVersions...), RosterEpoch: manifest.Epoch,
		}
		found = true
		break
	}
	if !found {
		return publicationPlan{}, ErrSigningAuthorityUnavailable
	}
	projection := make([]any, 0, len(plan.objects))
	for _, object := range plan.objects {
		projection = append(projection, []any{object.ScopeType, object.ScopeID, object.Kind, object.Sequence, object.PreviousHash, object.Hash})
	}
	credentialHash := sha256.Sum256(plan.registration.CredentialBlob)
	raw, err := canonicalEncoding.Marshal([]any{"aplexica/durable-sync-identity-publication/v1", projection, c.DeviceID, credentialHash, plan.registration.RosterEpoch})
	if err != nil {
		return publicationPlan{}, err
	}
	plan.digest = sha256.Sum256(raw)
	return plan, nil
}

func (c *Coordinator) publishPlan(ctx context.Context, plan publicationPlan) error {
	for _, wire := range plan.objects {
		var err error
		switch wire.Kind {
		case "trust-anchor":
			err = c.Transport.SubmitTrustAnchor(ctx, wire)
		case "authority-transition":
			err = c.Transport.SubmitAuthorityTransition(ctx, wire)
		case "roster":
			err = c.Transport.SubmitRosterTransition(ctx, wire)
		case "atomic-authority-roster-transition":
			err = c.Transport.SubmitAtomicAuthorityRosterTransition(ctx, wire)
		default:
			return ErrInvalidState
		}
		if err != nil {
			return fmt.Errorf("generation activation: publish %s/%d: %w", wire.Kind, wire.Sequence, err)
		}
	}
	return c.Transport.RegisterDeviceCredential(ctx, plan.registration)
}
