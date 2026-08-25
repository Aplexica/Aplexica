package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPublicationSnapshotExportsVerifiedCanonicalChain(t *testing.T) {
	anchor, recovery, genesis := signedIdentityFixture(t)
	store := &ChainStore{Path: filepath.Join(t.TempDir(), "chain.cbor")}
	_, err := store.Initialize(anchor, recovery, genesis)
	require.NoError(t, err)

	snapshot, err := store.PublicationSnapshot(time.Now())
	require.NoError(t, err)
	require.Equal(t, anchor.Anchor.AccountID, snapshot.AccountID)
	require.Len(t, snapshot.Objects, 2)
	require.Equal(t, "trust-anchor", snapshot.Objects[0].Kind)
	require.Equal(t, uint64(1), snapshot.Objects[0].Sequence)
	require.Zero(t, snapshot.Objects[0].PreviousHash)
	require.Equal(t, sha256.Sum256(snapshot.Objects[0].Blob), snapshot.Objects[0].Hash)
	require.Equal(t, "roster", snapshot.Objects[1].Kind)
	require.Equal(t, uint64(1), snapshot.Objects[1].Sequence)
	require.Zero(t, snapshot.Objects[1].PreviousHash)
	require.Equal(t, sha256.Sum256(snapshot.Objects[1].Blob), snapshot.Objects[1].Hash)

	var decodedAnchor AccountTrustAnchorV1
	require.NoError(t, dec.Unmarshal(snapshot.Objects[0].Blob, &decodedAnchor))
	require.Equal(t, anchor, decodedAnchor)
	var decodedRoster RosterManifestV1
	require.NoError(t, dec.Unmarshal(snapshot.Objects[1].Blob, &decodedRoster))
	require.Equal(t, genesis, decodedRoster)
}

func TestPublicationSnapshotIncludesAuthorityAndRosterRawHashChains(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	recoveryPublic, recoveryPrivate, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	authorityOnePublic, authorityOnePrivate, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	authorityOneID := sha256.Sum256(authorityOnePublic)
	var recoveryRoot [32]byte
	copy(recoveryRoot[:], recoveryPublic)
	var authorityOneKey [32]byte
	copy(authorityOneKey[:], authorityOnePublic)
	anchorUnsigned := AccountTrustAnchorUnsignedV1{
		Version: 1, ServiceOrigin: "https://api.aplexica.com", AccountID: "account-a",
		PersonalScopeID:      "0197f30a-3c58-7000-8000-000000000001",
		RecoveryKDFProfileID: RecoveryKDFProfileArgon2idV1, RecoveryRootPublicKey: recoveryRoot,
		RecoveryWrapPublicKey: sha256.Sum256([]byte("recovery-wrap")), AuthorityEpoch: 1,
		Authorities: []RosterAuthorityV1{{DeviceID: "device-a", SigningKeyID: authorityOneID, SigningPublicKey: authorityOneKey}}, AuthorityThreshold: 1,
	}
	anchorUnsigned.RecoveryWrapKeyID = sha256.Sum256(anchorUnsigned.RecoveryWrapPublicKey[:])
	anchorPreimage, err := canonical("aplexica/account-trust-anchor/v1", anchorUnsigned)
	require.NoError(t, err)
	anchor := AccountTrustAnchorV1{Anchor: anchorUnsigned}
	copy(anchor.RecoverySignature[:], ed25519.Sign(recoveryPrivate, anchorPreimage))
	verifiedAuthority, err := VerifyTrustAnchor(anchor, recoveryPublic)
	require.NoError(t, err)

	// Operational authority keys are device signing keys, not detached server
	// metadata. Keep the publication fixture valid under the hardened verifier.
	deviceOnePublic := authorityOnePublic
	deviceOneID := authorityOneID
	deviceWrap := sha256.Sum256([]byte("device-wrap-one"))
	certificateOneUnsigned := DeviceCertificateUnsignedV1{
		Version: 1, AccountID: anchorUnsigned.AccountID, UserID: "user-a", DeviceID: "device-a", KeyEpoch: 1,
		SigningKeyID: deviceOneID, WrapKeyID: sha256.Sum256(deviceWrap[:]), EnvelopeVersions: []uint16{2},
		NotBeforeUnix: now.Add(-time.Minute).Unix(), NotAfterUnix: now.Add(time.Hour).Unix(),
		IssuanceMode: "genesis", IssuedUnderAuthorityEpoch: 1, IssuingAuthorityStateHash: verifiedAuthority.StateHash,
	}
	copy(certificateOneUnsigned.SigningPublicKey[:], deviceOnePublic)
	certificateOneUnsigned.WrapPublicKey = deviceWrap
	certificatePreimage, err := canonical("aplexica/device-credential/v1", certificateOneUnsigned)
	require.NoError(t, err)
	certificateOne := DeviceCertificateV1{Certificate: certificateOneUnsigned, IssuerKeyIDs: [][32]byte{authorityOneID}, IssuanceSignatures: make([][64]byte, 1)}
	copy(certificateOne.IssuanceSignatures[0][:], ed25519.Sign(authorityOnePrivate, certificatePreimage))
	genesisUnsigned := RosterManifestUnsignedV1{
		Version: 1, ScopeType: "account", ScopeID: anchorUnsigned.PersonalScopeID, Epoch: 1,
		TrustAnchorHash: [32]byte(verifiedAuthority.AnchorHash), AuthorityStateHash: verifiedAuthority.StateHash, AuthorityEpoch: 1,
		AccessGeneration: 1, IssuedAtUnix: now.Unix(), NotAfterUnix: now.Add(time.Hour).Unix(),
		MinEnvelopeVersion: 2, Devices: []DeviceCertificateV1{certificateOne},
	}
	genesisUnsigned.AccessSetHash, err = AccessSetHash(genesisUnsigned)
	require.NoError(t, err)
	genesisPreimage, err := canonical("aplexica/roster-manifest/v1", genesisUnsigned)
	require.NoError(t, err)
	genesis := RosterManifestV1{Manifest: genesisUnsigned, SignerKeyIDs: [][32]byte{authorityOneID}, Signatures: make([][64]byte, 1)}
	copy(genesis.Signatures[0][:], ed25519.Sign(authorityOnePrivate, genesisPreimage))

	store := &ChainStore{Path: filepath.Join(t.TempDir(), "chain.cbor")}
	current, err := store.Initialize(anchor, recoveryPublic, genesis)
	require.NoError(t, err)

	// This publication-only fixture advances the authority epoch while retaining
	// the already active operational key. New authority keys require the full
	// pairing/recovery lineage package tested separately.
	authorityTwoPrivate := authorityOnePrivate
	authorityTwoID := authorityOneID
	authorityTwoKey := authorityOneKey
	nextAuthority := RosterAuthorityV1{DeviceID: "device-a", SigningKeyID: authorityTwoID, SigningPublicKey: authorityTwoKey}
	unsignedTransition := AuthorityTransitionUnsignedV1{
		Version: 1, AccountID: anchor.Anchor.AccountID,
		TrustAnchorHash: [32]byte(current.Authority.AnchorHash), PreviousStateHash: current.Authority.StateHash,
		PreviousAuthorityEpoch: 1, NewAuthorityEpoch: 2,
		NewAuthorities: []RosterAuthorityV1{nextAuthority}, NewThreshold: 1,
		AuthorizationMode: "operational", IssuedAtUnix: now.Unix(), Nonce: sha256.Sum256([]byte("transition")),
	}
	transition := AuthorityTransitionV1{Transition: unsignedTransition, SignerKeyIDs: [][32]byte{authorityOneID}, Signatures: make([][64]byte, 1)}
	transitionPreimage, err := canonical("aplexica/authority-transition/v1", unsignedTransition)
	require.NoError(t, err)
	copy(transition.Signatures[0][:], ed25519.Sign(authorityOnePrivate, transitionPreimage))
	nextAuthorityState, err := VerifyAuthorityTransition(current.Authority, transition)
	require.NoError(t, err)

	certificateTwoUnsigned := certificateOneUnsigned
	certificateTwoUnsigned.IssuedUnderAuthorityEpoch = 1
	certificateTwoUnsigned.IssuingAuthorityStateHash = current.Authority.StateHash
	certificateTwoPreimage, err := canonical("aplexica/device-credential/v1", certificateTwoUnsigned)
	require.NoError(t, err)
	certificateTwo := DeviceCertificateV1{Certificate: certificateTwoUnsigned, IssuerKeyIDs: [][32]byte{authorityTwoID}, IssuanceSignatures: make([][64]byte, 1)}
	copy(certificateTwo.IssuanceSignatures[0][:], ed25519.Sign(authorityTwoPrivate, certificateTwoPreimage))
	nextRosterUnsigned := genesisUnsigned
	nextRosterUnsigned.Epoch = 2
	nextRosterUnsigned.PreviousHash = [32]byte(current.Hash)
	nextRosterUnsigned.AuthorityEpoch = 2
	nextRosterUnsigned.AuthorityStateHash = nextAuthorityState.StateHash
	nextRosterUnsigned.IssuedAtUnix = now.Unix()
	nextRosterUnsigned.NotAfterUnix = now.Add(time.Hour).Unix()
	nextRosterUnsigned.Devices = []DeviceCertificateV1{certificateTwo}
	nextRosterUnsigned.AccessSetHash, err = AccessSetHash(nextRosterUnsigned)
	require.NoError(t, err)
	nextRosterPreimage, err := canonical("aplexica/roster-manifest/v1", nextRosterUnsigned)
	require.NoError(t, err)
	nextRoster := RosterManifestV1{Manifest: nextRosterUnsigned, SignerKeyIDs: [][32]byte{authorityTwoID}, Signatures: make([][64]byte, 1)}
	copy(nextRoster.Signatures[0][:], ed25519.Sign(authorityTwoPrivate, nextRosterPreimage))
	_, err = store.AppendAtomic(AtomicAuthorityRosterTransitionV1{AuthorityTransition: transition, NextRoster: nextRoster})
	require.NoError(t, err)

	snapshot, err := store.PublicationSnapshot(now)
	require.NoError(t, err)
	require.Len(t, snapshot.Objects, 5)
	require.Equal(t, "trust-anchor", snapshot.Objects[0].Kind)
	require.Equal(t, "roster", snapshot.Objects[1].Kind)
	require.Equal(t, "authority-transition", snapshot.Objects[2].Kind)
	require.Equal(t, snapshot.Objects[0].Hash, snapshot.Objects[2].PreviousHash)
	require.Equal(t, uint64(2), snapshot.Objects[2].Sequence)
	require.Equal(t, "roster", snapshot.Objects[3].Kind)
	require.Equal(t, snapshot.Objects[1].Hash, snapshot.Objects[3].PreviousHash)
	require.Equal(t, uint64(2), snapshot.Objects[3].Sequence)
	require.Equal(t, "atomic-authority-roster-transition", snapshot.Objects[4].Kind)
	require.Equal(t, snapshot.Objects[1].Hash, snapshot.Objects[4].PreviousHash)
	require.Equal(t, uint64(2), snapshot.Objects[4].Sequence)
	require.Equal(t, sha256.Sum256(snapshot.Objects[4].Blob), snapshot.Objects[4].Hash)
	var atomic AtomicAuthorityRosterTransitionV1
	require.NoError(t, dec.Unmarshal(snapshot.Objects[4].Blob, &atomic))
	require.Equal(t, transition, atomic.AuthorityTransition)
	require.Empty(t, atomic.RecoveryEnrollments)
	require.Equal(t, nextRoster, atomic.NextRoster)

	// The new atomic object is additive: the legacy raw transition and roster
	// blobs retain their exact canonical bytes and SHA-256 identities.
	transitionBlob, err := enc.Marshal(transition)
	require.NoError(t, err)
	rosterBlob, err := enc.Marshal(nextRoster)
	require.NoError(t, err)
	require.Equal(t, transitionBlob, snapshot.Objects[2].Blob)
	require.Equal(t, rosterBlob, snapshot.Objects[3].Blob)
}
