package daemon

import (
	"context"

	"github.com/aplexica/aplexica/internal/generationactivation"
	"github.com/aplexica/aplexica/internal/plugin/proto"
)

func (r *RemoteRunner) SubmitTrustAnchor(ctx context.Context, object proto.RemoteOpaqueSignedObject) (proto.RemoteSubmitTrustAnchorResult, error) {
	r.proxyMu.Lock()
	p := r.proxy
	r.proxyMu.Unlock()
	if p == nil {
		return proto.RemoteSubmitTrustAnchorResult{}, ErrRemoteReconnecting
	}
	return p.SubmitTrustAnchor(ctx, object)
}

func (r *RemoteRunner) SubmitAuthorityTransition(ctx context.Context, object proto.RemoteOpaqueSignedObject) error {
	r.proxyMu.Lock()
	p := r.proxy
	r.proxyMu.Unlock()
	if p == nil {
		return ErrRemoteReconnecting
	}
	return p.SubmitAuthorityTransition(ctx, object)
}

func (r *RemoteRunner) SubmitRosterTransition(ctx context.Context, object proto.RemoteOpaqueSignedObject) error {
	r.proxyMu.Lock()
	p := r.proxy
	r.proxyMu.Unlock()
	if p == nil {
		return ErrRemoteReconnecting
	}
	return p.SubmitRosterTransition(ctx, object)
}

func (r *RemoteRunner) SubmitAtomicAuthorityRosterTransition(ctx context.Context, object proto.RemoteOpaqueSignedObject) error {
	r.proxyMu.Lock()
	p := r.proxy
	r.proxyMu.Unlock()
	if p == nil {
		return ErrRemoteReconnecting
	}
	return p.SubmitAtomicAuthorityRosterTransition(ctx, object)
}

func (r *RemoteRunner) RegisterDeviceCredential(ctx context.Context, params proto.RemoteRegisterDeviceCredentialParams) error {
	r.proxyMu.Lock()
	p := r.proxy
	r.proxyMu.Unlock()
	if p == nil {
		return ErrRemoteReconnecting
	}
	return p.RegisterDeviceCredential(ctx, params)
}

func (r *RemoteRunner) ActivateSyncGeneration(ctx context.Context, params proto.RemoteActivateSyncGenerationParams) (proto.RemoteActivateSyncGenerationResult, error) {
	r.proxyMu.Lock()
	p := r.proxy
	r.proxyMu.Unlock()
	if p == nil {
		return proto.RemoteActivateSyncGenerationResult{}, ErrRemoteReconnecting
	}
	return p.ActivateSyncGeneration(ctx, params)
}

func (r *RemoteRunner) GetSyncGenerationStatus(ctx context.Context, params proto.RemoteGetSyncGenerationStatusParams) (proto.RemoteGetSyncGenerationStatusResult, error) {
	r.proxyMu.Lock()
	p := r.proxy
	r.proxyMu.Unlock()
	if p == nil {
		return proto.RemoteGetSyncGenerationStatusResult{}, ErrRemoteReconnecting
	}
	return p.GetSyncGenerationStatus(ctx, params)
}

// RemoteGenerationActivationTransport adapts the narrow durable coordinator
// contract to the live, signed-plugin RPC. It contains no trust decisions.
type RemoteGenerationActivationTransport struct{ Runner *RemoteRunner }

func activationWireObject(object generationactivation.SignedObject) proto.RemoteOpaqueSignedObject {
	return proto.RemoteOpaqueSignedObject{
		ScopeType: object.ScopeType, ScopeID: object.ScopeID, Kind: object.Kind,
		Sequence: object.Sequence, PreviousHash: object.PreviousHash, Hash: object.Hash,
		Blob: append([]byte(nil), object.Blob...), ProofBlob: append([]byte(nil), object.ProofBlob...),
	}
}

func (t RemoteGenerationActivationTransport) SubmitTrustAnchor(ctx context.Context, object generationactivation.SignedObject) error {
	_, err := t.Runner.SubmitTrustAnchor(ctx, activationWireObject(object))
	return err
}

func (t RemoteGenerationActivationTransport) SubmitAuthorityTransition(ctx context.Context, object generationactivation.SignedObject) error {
	return t.Runner.SubmitAuthorityTransition(ctx, activationWireObject(object))
}

func (t RemoteGenerationActivationTransport) SubmitRosterTransition(ctx context.Context, object generationactivation.SignedObject) error {
	return t.Runner.SubmitRosterTransition(ctx, activationWireObject(object))
}

func (t RemoteGenerationActivationTransport) SubmitAtomicAuthorityRosterTransition(ctx context.Context, object generationactivation.SignedObject) error {
	return t.Runner.SubmitAtomicAuthorityRosterTransition(ctx, activationWireObject(object))
}

func (t RemoteGenerationActivationTransport) RegisterDeviceCredential(ctx context.Context, registration generationactivation.CredentialRegistration) error {
	return t.Runner.RegisterDeviceCredential(ctx, proto.RemoteRegisterDeviceCredentialParams{
		CredentialBlob: append([]byte(nil), registration.CredentialBlob...), SigningKeyID: registration.SigningKeyID,
		WrapKeyID: registration.WrapKeyID, EnvelopeVersions: append([]uint16(nil), registration.EnvelopeVersions...), RosterEpoch: registration.RosterEpoch,
	})
}

func (t RemoteGenerationActivationTransport) ActivateGeneration(ctx context.Context, blob []byte) (generationactivation.ActivationReceipt, error) {
	result, err := t.Runner.ActivateSyncGeneration(ctx, proto.RemoteActivateSyncGenerationParams{AttestationBlob: append([]byte(nil), blob...)})
	return generationactivation.ActivationReceipt{AuthorityDigest: result.AuthorityDigest, Revision: result.Revision, Duplicate: result.Duplicate}, err
}

func (t RemoteGenerationActivationTransport) GetActivationStatus(ctx context.Context, blob []byte) (generationactivation.ActivationStatus, error) {
	result, err := t.Runner.GetSyncGenerationStatus(ctx, proto.RemoteGetSyncGenerationStatusParams{AttestationBlob: append([]byte(nil), blob...)})
	if err != nil {
		return generationactivation.ActivationStatus{}, err
	}
	switch result.Status {
	case "committed":
		return generationactivation.ActivationStatus{Committed: true, Receipt: generationactivation.ActivationReceipt{
			AuthorityDigest: result.AuthorityDigest, Revision: result.Revision, Duplicate: result.Duplicate,
		}}, nil
	case "absent":
		if result.AuthorityDigest != "" || result.Revision != 0 || result.Duplicate {
			return generationactivation.ActivationStatus{}, generationactivation.ErrInvalidState
		}
		return generationactivation.ActivationStatus{Absent: true}, nil
	default:
		return generationactivation.ActivationStatus{}, generationactivation.ErrInvalidState
	}
}

var _ generationactivation.Transport = RemoteGenerationActivationTransport{}
