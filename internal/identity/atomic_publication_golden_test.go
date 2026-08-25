package identity

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type atomicPublicationGoldenV1 struct {
	Version                          uint16                  `json:"version"`
	AccountID                        string                  `json:"account_id"`
	CurrentRosterEpoch               uint64                  `json:"current_roster_epoch"`
	VerifyAtUnix                     int64                   `json:"verify_at_unix"`
	RecoveryRootPublicKeyHex         string                  `json:"recovery_root_public_key_hex"`
	AuthorityTransitionHashHex       string                  `json:"authority_transition_hash_hex"`
	NextRosterHashHex                string                  `json:"next_roster_hash_hex"`
	RecoveryEnrollmentContextHashHex string                  `json:"recovery_enrollment_context_hash_hex"`
	Object                           atomicPublicationObject `json:"object"`
}

type atomicPublicationObject struct {
	ScopeType       string `json:"scope_type"`
	ScopeID         string `json:"scope_id"`
	Kind            string `json:"kind"`
	Sequence        uint64 `json:"sequence"`
	PreviousHashHex string `json:"previous_hash_hex"`
	HashHex         string `json:"hash_hex"`
	BlobHex         string `json:"blob_hex"`
}

func TestAtomicAuthorityRosterTransitionPublicationGolden(t *testing.T) {
	snapshot, atomic, verifyAt := deterministicRecoveryPublication(t)
	require.Len(t, snapshot.Objects, 5)
	require.Equal(t, "atomic-authority-roster-transition", snapshot.Objects[4].Kind)
	require.Equal(t, sha256.Sum256(snapshot.Objects[4].Blob), snapshot.Objects[4].Hash)
	require.Equal(t, snapshot.Objects[1].Hash, snapshot.Objects[4].PreviousHash)
	var decoded AtomicAuthorityRosterTransitionV1
	require.NoError(t, dec.Unmarshal(snapshot.Objects[4].Blob, &decoded))
	require.Equal(t, atomic, decoded)
	reencoded, err := enc.Marshal(decoded)
	require.NoError(t, err)
	require.Equal(t, snapshot.Objects[4].Blob, reencoded)

	transitionHash, err := AuthorityTransitionHash(atomic.AuthorityTransition)
	require.NoError(t, err)
	nextRosterHash, err := HashRoster(atomic.NextRoster)
	require.NoError(t, err)
	enrollmentContextHash, err := RecoveryEnrollmentContextHash(atomic.RecoveryEnrollments[0])
	require.NoError(t, err)
	recoveryPublic, _ := deterministicSigningKey("golden-recovery-root-v1")
	object := snapshot.Objects[4]
	golden := atomicPublicationGoldenV1{
		Version: 1, AccountID: snapshot.AccountID,
		CurrentRosterEpoch: snapshot.Current.Manifest.Manifest.Epoch, VerifyAtUnix: verifyAt.Unix(),
		RecoveryRootPublicKeyHex: hex.EncodeToString(recoveryPublic), AuthorityTransitionHashHex: hex.EncodeToString(transitionHash[:]),
		NextRosterHashHex: hex.EncodeToString(nextRosterHash[:]), RecoveryEnrollmentContextHashHex: hex.EncodeToString(enrollmentContextHash[:]),
		Object: atomicPublicationObject{
			ScopeType: object.ScopeType, ScopeID: object.ScopeID, Kind: object.Kind, Sequence: object.Sequence,
			PreviousHashHex: hex.EncodeToString(object.PreviousHash[:]), HashHex: hex.EncodeToString(object.Hash[:]), BlobHex: hex.EncodeToString(object.Blob),
		},
	}
	actual, err := json.MarshalIndent(golden, "", "  ")
	require.NoError(t, err)
	actual = append(actual, '\n')
	expected, err := os.ReadFile(filepath.Join("testdata", "atomic_authority_roster_transition_v1.json"))
	expected = bytes.ReplaceAll(expected, []byte("\r\n"), []byte("\n"))
	if err != nil || !bytes.Equal(expected, actual) {
		t.Fatalf("atomic publication golden mismatch (read error: %v); replace fixture with:\n%s", err, actual)
	}
}

func deterministicRecoveryPublication(t *testing.T) (PublicationSnapshot, AtomicAuthorityRosterTransitionV1, time.Time) {
	t.Helper()
	base := time.Unix(1_700_000_000, 0).UTC()
	accountID := "golden-account-v1"
	scopeID := "0197f30a-3c58-7000-8000-000000000001"
	recoveryPublic, recoveryPrivate := deterministicSigningKey("golden-recovery-root-v1")
	previousPublic, previousPrivate := deterministicSigningKey("golden-previous-authority-v1")
	previousKeyID := sha256.Sum256(previousPublic)
	var recoveryPublicArray, previousPublicArray [32]byte
	copy(recoveryPublicArray[:], recoveryPublic)
	copy(previousPublicArray[:], previousPublic)
	recoveryWrap := sha256.Sum256([]byte("golden-recovery-wrap-public-v1"))
	previousWrap := sha256.Sum256([]byte("golden-previous-wrap-public-v1"))
	var recoverySalt [16]byte
	recoverySaltDigest := sha256.Sum256([]byte("golden-recovery-salt-v1"))
	copy(recoverySalt[:], recoverySaltDigest[:16])
	anchorUnsigned := AccountTrustAnchorUnsignedV1{
		Version: 1, ServiceOrigin: "https://api.aplexica.com", AccountID: accountID, PersonalScopeID: scopeID,
		RecoveryKDFProfileID: RecoveryKDFProfileArgon2idV1, RecoverySalt: recoverySalt,
		RecoveryRootPublicKey: recoveryPublicArray, RecoveryWrapPublicKey: recoveryWrap,
		AuthorityEpoch:     1,
		Authorities:        []RosterAuthorityV1{{DeviceID: "golden-device-old", SigningKeyID: previousKeyID, SigningPublicKey: previousPublicArray}},
		AuthorityThreshold: 1,
	}
	anchorUnsigned.RecoveryWrapKeyID = sha256.Sum256(recoveryWrap[:])
	anchor := AccountTrustAnchorV1{Anchor: anchorUnsigned}
	anchor.RecoverySignature = mustSignGolden(t, recoveryPrivate, "aplexica/account-trust-anchor/v1", anchorUnsigned)
	verifiedAuthority, err := VerifyTrustAnchor(anchor, recoveryPublic)
	require.NoError(t, err)

	previousCredentialUnsigned := DeviceCertificateUnsignedV1{
		Version: 1, AccountID: accountID, UserID: "golden-user", DeviceID: "golden-device-old", KeyEpoch: 1,
		SigningKeyID: previousKeyID, SigningPublicKey: previousPublicArray,
		WrapKeyID: sha256.Sum256(previousWrap[:]), WrapPublicKey: previousWrap, EnvelopeVersions: []uint16{2},
		NotBeforeUnix: base.Add(-time.Minute).Unix(), NotAfterUnix: base.Add(364 * 24 * time.Hour).Unix(),
		IssuanceMode: "genesis", IssuedUnderAuthorityEpoch: 1, IssuingAuthorityStateHash: verifiedAuthority.StateHash,
	}
	previousCredential := DeviceCertificateV1{
		Certificate: previousCredentialUnsigned, IssuerKeyIDs: [][32]byte{previousKeyID}, IssuanceSignatures: make([][64]byte, 1),
	}
	previousCredential.IssuanceSignatures[0] = mustSignGolden(t, previousPrivate, "aplexica/device-credential/v1", previousCredentialUnsigned)
	genesisUnsigned := RosterManifestUnsignedV1{
		Version: 1, ScopeType: "account", ScopeID: scopeID, Epoch: 1,
		TrustAnchorHash: [32]byte(verifiedAuthority.AnchorHash), AuthorityStateHash: verifiedAuthority.StateHash, AuthorityEpoch: 1,
		AccessGeneration: 1, IssuedAtUnix: base.Unix(), NotAfterUnix: base.Add(time.Hour).Unix(), MinEnvelopeVersion: 2,
		Devices: []DeviceCertificateV1{previousCredential},
	}
	genesisUnsigned.AccessSetHash, err = AccessSetHash(genesisUnsigned)
	require.NoError(t, err)
	genesis := RosterManifestV1{Manifest: genesisUnsigned, SignerKeyIDs: [][32]byte{previousKeyID}, Signatures: make([][64]byte, 1)}
	genesis.Signatures[0] = mustSignGolden(t, previousPrivate, "aplexica/roster-manifest/v1", genesisUnsigned)
	store := &ChainStore{Path: filepath.Join(t.TempDir(), "golden-chain.cbor")}
	previous, err := store.Initialize(anchor, recoveryPublic, genesis)
	require.NoError(t, err)

	candidatePublic, candidatePrivate := deterministicSigningKey("golden-recovered-authority-v1")
	candidateKeyID := sha256.Sum256(candidatePublic)
	var candidatePublicArray [32]byte
	copy(candidatePublicArray[:], candidatePublic)
	candidateWrap := sha256.Sum256([]byte("golden-recovered-wrap-public-v1"))
	issued := base.Add(10 * time.Minute)
	authority := RosterAuthorityV1{DeviceID: "golden-device-recovered", SigningKeyID: candidateKeyID, SigningPublicKey: candidatePublicArray}
	transitionUnsigned := AuthorityTransitionUnsignedV1{
		Version: 1, AccountID: accountID, TrustAnchorHash: [32]byte(previous.Authority.AnchorHash), PreviousStateHash: previous.Authority.StateHash,
		PreviousAuthorityEpoch: 1, NewAuthorityEpoch: 2, NewAuthorities: []RosterAuthorityV1{authority}, NewThreshold: 1,
		AuthorizationMode: "recovery", IssuedAtUnix: issued.Unix(), Nonce: sha256.Sum256([]byte("golden-recovery-transition-nonce-v1")),
	}
	transition := AuthorityTransitionV1{
		Transition:   transitionUnsigned,
		SignerKeyIDs: [][32]byte{sha256.Sum256(recoveryPublic)}, Signatures: make([][64]byte, 1),
	}
	transition.Signatures[0] = mustSignGolden(t, recoveryPrivate, "aplexica/authority-recovery/v1", transitionUnsigned)
	transitionHash, err := AuthorityTransitionHash(transition)
	require.NoError(t, err)
	enrollmentUnsigned := RecoveryEnrollmentUnsignedV1{
		Version: 1, AccountID: accountID, TrustAnchorHash: [32]byte(previous.Authority.AnchorHash), AuthorityTransitionHash: transitionHash,
		CandidateDeviceID: authority.DeviceID, CandidateSigningKeyID: candidateKeyID, CandidateSigningPublic: candidatePublicArray,
		CandidateWrapKeyID: sha256.Sum256(candidateWrap[:]), CandidateWrapPublic: candidateWrap, EnvelopeVersions: []uint16{2},
		JoinNonce: sha256.Sum256([]byte("golden-recovery-join-nonce-v1")), RecoveryNonce: sha256.Sum256([]byte("golden-recovery-enrollment-nonce-v1")),
		NotBeforeUnix: issued.Unix(), NotAfterUnix: issued.Add(364 * 24 * time.Hour).Unix(),
	}
	enrollment := RecoveryEnrollmentV1{Enrollment: enrollmentUnsigned}
	enrollment.RecoverySignature = mustSignGolden(t, recoveryPrivate, "aplexica/recovery-device-enrollment/v1", enrollmentUnsigned)
	credentialUnsigned, candidateState, err := PrepareRecoveryCredential(previous, RecoveryCredentialProposalV1{
		Transition: transition, Enrollments: []RecoveryEnrollmentV1{enrollment}, DeviceID: authority.DeviceID, UserID: "golden-user", KeyEpoch: 1,
	})
	require.NoError(t, err)
	credentialUnsigned.CandidateProof, err = SignDevicePossession(credentialUnsigned, candidatePrivate)
	require.NoError(t, err)
	credentialEndorsement, err := EndorseRecoveryCredential(previous, transition, []RecoveryEnrollmentV1{enrollment}, credentialUnsigned, candidateKeyID, candidatePrivate)
	require.NoError(t, err)
	credential, err := FinalizeRecoveryCredential(previous, transition, []RecoveryEnrollmentV1{enrollment}, credentialUnsigned, []DeviceTransitionEndorsementV1{credentialEndorsement})
	require.NoError(t, err)
	rosterUnsigned, preparedState, err := PrepareRecoveryRosterTransition(previous, transition, []DeviceCertificateV1{credential}, issued)
	require.NoError(t, err)
	require.Equal(t, candidateState.StateHash, preparedState.StateHash)
	rosterEndorsement, err := EndorseRecoveryRoster(candidateState, rosterUnsigned, candidateKeyID, candidatePrivate)
	require.NoError(t, err)
	atomic, verified, err := FinalizeRecoveryTransition(previous, transition, []RecoveryEnrollmentV1{enrollment}, rosterUnsigned, []DeviceTransitionEndorsementV1{rosterEndorsement})
	require.NoError(t, err)
	_, err = store.AppendAtomic(atomic)
	require.NoError(t, err)
	verifyAt := issued.Add(time.Second)
	snapshot, err := store.PublicationSnapshot(verifyAt)
	require.NoError(t, err)
	require.Equal(t, verified.Hash, snapshot.Current.Hash)
	return snapshot, atomic, verifyAt
}

func deterministicSigningKey(label string) (ed25519.PublicKey, ed25519.PrivateKey) {
	seed := sha256.Sum256([]byte(label))
	private := ed25519.NewKeyFromSeed(seed[:])
	return private.Public().(ed25519.PublicKey), private
}

func mustSignGolden(t *testing.T, private ed25519.PrivateKey, domain string, value any) [64]byte {
	t.Helper()
	preimage, err := canonical(domain, value)
	require.NoError(t, err)
	var signature [64]byte
	copy(signature[:], ed25519.Sign(private, preimage))
	return signature
}
