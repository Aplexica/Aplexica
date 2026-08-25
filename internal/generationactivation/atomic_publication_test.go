package generationactivation

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"testing"

	"github.com/aplexica/aplexica/internal/identity"
	"github.com/stretchr/testify/require"
)

func appendOperationalAtomicStep(t *testing.T, fixture *activationFixture) identity.AtomicAuthorityRosterTransitionV1 {
	t.Helper()
	previous, err := fixture.chain.Current(fixture.now)
	require.NoError(t, err)
	authority := previous.Authority.Authorities[identity.DeviceKeyID(fixture.device.SigningKeyID)]
	transitionUnsigned := identity.AuthorityTransitionUnsignedV1{
		Version:                1,
		AccountID:              fixture.account,
		TrustAnchorHash:        [32]byte(previous.Authority.AnchorHash),
		PreviousStateHash:      previous.Authority.StateHash,
		PreviousAuthorityEpoch: previous.Authority.AuthorityEpoch,
		NewAuthorityEpoch:      previous.Authority.AuthorityEpoch + 1,
		NewAuthorities:         []identity.RosterAuthorityV1{authority},
		NewThreshold:           1,
		AuthorizationMode:      "operational",
		IssuedAtUnix:           fixture.now.Unix(),
		Nonce:                  sha256.Sum256([]byte("atomic-publication-transition")),
	}
	transitionPreimage, err := canonicalEncoding.Marshal([]any{"aplexica/authority-transition/v1", transitionUnsigned})
	require.NoError(t, err)
	transition := identity.AuthorityTransitionV1{
		Transition: transitionUnsigned, SignerKeyIDs: [][32]byte{fixture.device.SigningKeyID}, Signatures: make([][64]byte, 1),
	}
	copy(transition.Signatures[0][:], ed25519.Sign(fixture.device.SigningPrivate, transitionPreimage))
	candidate, err := identity.VerifyAuthorityTransition(previous.Authority, transition)
	require.NoError(t, err)

	nextUnsigned := previous.Manifest.Manifest
	nextUnsigned.Epoch++
	nextUnsigned.PreviousHash = [32]byte(previous.Hash)
	nextUnsigned.AuthorityEpoch = candidate.AuthorityEpoch
	nextUnsigned.AuthorityStateHash = candidate.StateHash
	nextUnsigned.IssuedAtUnix = fixture.now.Unix()
	nextUnsigned.NotAfterUnix = previous.Manifest.Manifest.NotAfterUnix
	nextUnsigned.Devices = append([]identity.DeviceCertificateV1(nil), previous.Manifest.Manifest.Devices...)
	nextUnsigned.AccessSetHash, err = identity.AccessSetHash(nextUnsigned)
	require.NoError(t, err)
	rosterPreimage, err := canonicalEncoding.Marshal([]any{"aplexica/roster-manifest/v1", nextUnsigned})
	require.NoError(t, err)
	nextRoster := identity.RosterManifestV1{
		Manifest: nextUnsigned, SignerKeyIDs: [][32]byte{fixture.device.SigningKeyID}, Signatures: make([][64]byte, 1),
	}
	copy(nextRoster.Signatures[0][:], ed25519.Sign(fixture.device.SigningPrivate, rosterPreimage))
	atomic := identity.AtomicAuthorityRosterTransitionV1{AuthorityTransition: transition, NextRoster: nextRoster}
	verified, err := fixture.chain.AppendAtomic(atomic)
	require.NoError(t, err)
	fixture.epoch.RosterHash = [32]byte(verified.Hash)
	fixture.epoch.AccessGeneration = verified.Manifest.Manifest.AccessGeneration
	fixture.epoch.AccessSetHash = verified.Manifest.Manifest.AccessSetHash
	fixture.epoch.CoordinatorGeneration++
	fixture.epoch.BarrierID = sha256.Sum256([]byte("atomic-publication-barrier"))
	return atomic
}

func TestCoordinatorPublishesHashedAtomicPackageInOrderAndRetriesExactBytes(t *testing.T) {
	fixture := newActivationFixture(t)
	atomic := appendOperationalAtomicStep(t, &fixture)
	state := &memoryStateStore{}
	transport := &recordingTransport{
		failAtomic: 1,
		receipt:    ActivationReceipt{AuthorityDigest: sha256Hex("atomic-publication-authority"), Revision: 1},
	}

	_, err := fixture.coordinator(state, transport).RunOnce(context.Background())
	require.EqualError(t, err, "generation activation: publish atomic-authority-roster-transition/2: atomic publication temporarily unavailable")
	require.False(t, state.exists, "partial publication must not persist a completed publication digest")
	require.Empty(t, transport.registrations)
	require.Empty(t, transport.activations)
	require.Len(t, transport.objects, 5)
	require.Equal(t, []string{"trust-anchor", "roster", "authority-transition", "roster", "atomic-authority-roster-transition"}, objectKinds(transport.objects))
	firstAttempt := cloneSignedObjects(transport.objects)
	atomicObject := firstAttempt[4]
	require.Equal(t, fixture.scopeID, atomicObject.ScopeID)
	require.Equal(t, uint64(2), atomicObject.Sequence)
	require.Equal(t, firstAttempt[1].Hash, atomicObject.PreviousHash)
	require.Equal(t, sha256.Sum256(atomicObject.Blob), atomicObject.Hash)
	require.Empty(t, atomicObject.ProofBlob)
	encodedAtomic, err := canonicalEncoding.Marshal(atomic)
	require.NoError(t, err)
	require.Equal(t, encodedAtomic, atomicObject.Blob)

	// A process restart before the completed publication receipt republishes
	// the same immutable bytes in the same order. Exact duplicates are the
	// server-side idempotency contract; no replacement object is generated.
	restarted := fixture.coordinator(state, transport)
	result, err := restarted.RunOnce(context.Background())
	require.NoError(t, err)
	require.False(t, result.AlreadyActivated)
	require.Len(t, transport.objects, 10)
	require.Equal(t, firstAttempt, transport.objects[5:])
	require.Len(t, transport.registrations, 1)
	require.Len(t, transport.activations, 1)
	require.NotZero(t, state.state.PublishedDigest)

	result, err = fixture.coordinator(state, transport).RunOnce(context.Background())
	require.NoError(t, err)
	require.True(t, result.AlreadyActivated)
	require.Len(t, transport.objects, 10)
	require.Len(t, transport.registrations, 1)
}

func objectKinds(objects []SignedObject) []string {
	kinds := make([]string, len(objects))
	for index := range objects {
		kinds[index] = objects[index].Kind
	}
	return kinds
}

func cloneSignedObjects(objects []SignedObject) []SignedObject {
	cloned := make([]SignedObject, len(objects))
	for index, object := range objects {
		cloned[index] = object
		cloned[index].Blob = append([]byte(nil), object.Blob...)
		cloned[index].ProofBlob = append([]byte(nil), object.ProofBlob...)
	}
	return cloned
}
