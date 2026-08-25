package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/keys"
	"github.com/aplexica/aplexica/internal/securityerr"
	"github.com/stretchr/testify/require"
)

func TestPairingAdditionAndSameDeviceKeyReplacementRequireCompleteProofs(t *testing.T) {
	previous := existingAccountGenesisInstallerFixture(t).Verified
	approver := existingAccountGenesisTestDevice(t)
	for _, test := range []struct {
		name      string
		deviceID  string
		keyEpoch  uint64
		wantCount int
		ordinary  bool
	}{
		{"new device", "device-b", 1, 2, true},
		{"same authority device replacement requires atomic authority transition", "device-local", 2, 1, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := newTransitionTestDevice(t)
			credential, transcript := prepareTransitionTestCredential(t, previous, approver, candidate, test.deviceID, test.keyEpoch)
			endorsement, err := EndorseDeviceCredential(previous, credential, approver.SigningKeyID, approver.SigningPrivate)
			require.NoError(t, err)
			signedCredential, err := FinalizeDeviceCredential(previous, credential, []DeviceTransitionEndorsementV1{endorsement})
			require.NoError(t, err)
			tampered := credential
			tampered.CandidateProof[0] ^= 1
			_, err = FinalizeDeviceCredential(previous, tampered, []DeviceTransitionEndorsementV1{endorsement})
			require.Error(t, err)
			tampered = credential
			tampered.EnrollmentContextHash[0] ^= 1
			_, err = SignPairingApproval(previous, tampered, transcript, approver.SigningPrivate)
			require.ErrorIs(t, err, ErrInvalidDeviceTransition)
			proposal, err := PrepareDeviceRosterTransition(previous, signedCredential, existingAccountGenesisTestTime.Add(2*time.Hour))
			require.NoError(t, err)
			rosterEndorsement, err := EndorseDeviceRosterTransition(previous, proposal, approver.SigningKeyID, approver.SigningPrivate)
			require.NoError(t, err)
			_, verified, err := FinalizeDeviceRosterTransition(previous, proposal, []DeviceTransitionEndorsementV1{rosterEndorsement})
			if !test.ordinary {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Len(t, verified.Manifest.Manifest.Devices, test.wantCount)
			require.Equal(t, previous.Manifest.Manifest.AccessGeneration+1, verified.Manifest.Manifest.AccessGeneration)
		})
	}
}

func TestOrdinaryRosterRejectsAuthorityWithoutExactActivePreviousCredential(t *testing.T) {
	previous := existingAccountGenesisInstallerFixture(t).Verified
	device := existingAccountGenesisTestDevice(t)
	proposal, err := PrepareFreshnessRenewal(previous, existingAccountGenesisRenewalTime)
	require.NoError(t, err)
	endorsement, err := SignFreshnessRenewal(previous, proposal, device.SigningKeyID, device.SigningPrivate)
	require.NoError(t, err)
	signed := RosterManifestV1{Manifest: proposal, SignerKeyIDs: [][32]byte{endorsement.SignerKeyID}, Signatures: [][64]byte{endorsement.Signature}}

	stale := previous
	stale.Manifest.Manifest = cloneRosterManifestUnsigned(previous.Manifest.Manifest)
	stale.Manifest.Manifest.Devices[0].Certificate.SigningKeyID[0] ^= 1
	_, err = VerifyTransition(stale, stale.Authority, signed)
	require.ErrorIs(t, err, securityerr.ErrInvalidSignature)
}

func TestRecoveryDeviceTransitionUsesRootEnrollmentAndNewThresholdWithoutServerTrust(t *testing.T) {
	fixture := existingAccountGenesisInstallerFixture(t)
	previous := fixture.Verified
	candidate := newTransitionTestDevice(t)
	var candidatePublic [32]byte
	copy(candidatePublic[:], candidate.SigningPublic)
	authority := RosterAuthorityV1{DeviceID: "device-recovered", SigningKeyID: candidate.SigningKeyID, SigningPublicKey: candidatePublic}
	now := existingAccountGenesisTestTime.Add(3 * time.Hour)
	transitionUnsigned := AuthorityTransitionUnsignedV1{Version: 1, AccountID: previous.Authority.Anchor.Anchor.AccountID,
		TrustAnchorHash: [32]byte(previous.Authority.AnchorHash), PreviousStateHash: previous.Authority.StateHash,
		PreviousAuthorityEpoch: previous.Authority.AuthorityEpoch, NewAuthorityEpoch: previous.Authority.AuthorityEpoch + 1,
		NewAuthorities: []RosterAuthorityV1{authority}, NewThreshold: 1, AuthorizationMode: "recovery", IssuedAtUnix: now.Unix(), Nonce: sha256.Sum256([]byte("recovery-transition"))}
	recovery, err := DeriveRecoveryKeys([]byte(existingAccountGenesisTestMnemonic), previous.Authority.Anchor.Anchor.RecoverySalt, RecoveryKDFProfileArgon2idV1)
	require.NoError(t, err)
	defer recovery.Clear()
	recoveryPrivate := ed25519.NewKeyFromSeed(recovery.SigningSeed[:])
	defer clearBytes(recoveryPrivate)
	transition := AuthorityTransitionV1{Transition: transitionUnsigned, SignerKeyIDs: [][32]byte{sha256.Sum256(previous.Authority.Anchor.Anchor.RecoveryRootPublicKey[:])}, Signatures: make([][64]byte, 1)}
	transition.Signatures[0], err = signExistingAccountValue(recoveryPrivate, "aplexica/authority-recovery/v1", transitionUnsigned)
	require.NoError(t, err)
	transitionHash, err := AuthorityTransitionHash(transition)
	require.NoError(t, err)
	enrollmentUnsigned := RecoveryEnrollmentUnsignedV1{Version: 1, AccountID: previous.Authority.Anchor.Anchor.AccountID,
		TrustAnchorHash: [32]byte(previous.Authority.AnchorHash), AuthorityTransitionHash: transitionHash, CandidateDeviceID: authority.DeviceID,
		CandidateSigningKeyID: candidate.SigningKeyID, CandidateSigningPublic: candidatePublic, CandidateWrapKeyID: candidate.WrapKeyID, CandidateWrapPublic: candidate.WrapPublic,
		EnvelopeVersions: []uint16{2}, JoinNonce: sha256.Sum256([]byte("recovery-join")), RecoveryNonce: sha256.Sum256([]byte("recovery-enrollment")),
		NotBeforeUnix: now.Unix(), NotAfterUnix: now.Add(364 * 24 * time.Hour).Unix()}
	enrollment := RecoveryEnrollmentV1{Enrollment: enrollmentUnsigned}
	enrollment.RecoverySignature, err = signExistingAccountValue(recoveryPrivate, "aplexica/recovery-device-enrollment/v1", enrollmentUnsigned)
	require.NoError(t, err)
	credential, candidateState, err := PrepareRecoveryCredential(previous, RecoveryCredentialProposalV1{Transition: transition, Enrollments: []RecoveryEnrollmentV1{enrollment}, DeviceID: authority.DeviceID, UserID: "user-local", KeyEpoch: 1})
	require.NoError(t, err)
	credential.CandidateProof, err = SignDevicePossession(credential, candidate.SigningPrivate)
	require.NoError(t, err)
	endorsement, err := EndorseRecoveryCredential(previous, transition, []RecoveryEnrollmentV1{enrollment}, credential, candidate.SigningKeyID, candidate.SigningPrivate)
	require.NoError(t, err)
	signedCredential, err := FinalizeRecoveryCredential(previous, transition, []RecoveryEnrollmentV1{enrollment}, credential, []DeviceTransitionEndorsementV1{endorsement})
	require.NoError(t, err)
	proposal, preparedState, err := PrepareRecoveryRosterTransition(previous, transition, []DeviceCertificateV1{signedCredential}, now)
	require.NoError(t, err)
	require.Equal(t, candidateState.StateHash, preparedState.StateHash)
	rosterEndorsement, err := EndorseRecoveryRoster(candidateState, proposal, candidate.SigningKeyID, candidate.SigningPrivate)
	require.NoError(t, err)
	pkg, verified, err := FinalizeRecoveryTransition(previous, transition, []RecoveryEnrollmentV1{enrollment}, proposal, []DeviceTransitionEndorsementV1{rosterEndorsement})
	require.NoError(t, err)
	require.Equal(t, authority.DeviceID, verified.Manifest.Manifest.Devices[0].Certificate.DeviceID)
	require.Equal(t, previous.Manifest.Manifest.AccessGeneration+1, verified.Manifest.Manifest.AccessGeneration)
	require.Equal(t, transition, pkg.AuthorityTransition)

	tampered := enrollment
	tampered.Enrollment.CandidateWrapPublic[0] ^= 1
	_, _, err = PrepareRecoveryCredential(previous, RecoveryCredentialProposalV1{Transition: transition, Enrollments: []RecoveryEnrollmentV1{tampered}, DeviceID: authority.DeviceID, UserID: "user-local", KeyEpoch: 1})
	require.Error(t, err)
}

func prepareTransitionTestCredential(t *testing.T, previous VerifiedRoster, approver, candidate keys.DeviceIdentity, deviceID string, keyEpoch uint64) (DeviceCertificateUnsignedV1, PairingTranscriptV1) {
	t.Helper()
	var candidatePublic [32]byte
	copy(candidatePublic[:], candidate.SigningPublic)
	transcript := PairingTranscriptV1{Version: 1, ServiceOrigin: previous.Authority.Anchor.Anchor.ServiceOrigin, AccountID: previous.Authority.Anchor.Anchor.AccountID,
		PendingID: "pending-" + deviceID, PairingNonce: sha256.Sum256([]byte("nonce-" + deviceID)), CandidateDeviceID: deviceID,
		CandidateEphemeralPublic: sha256.Sum256([]byte("candidate-ephemeral-" + deviceID)), CandidateSigningPublic: candidatePublic,
		CandidateWrapPublic: candidate.WrapPublic, CandidateEnvelopeVersions: []uint16{2}, ApproverDeviceID: "device-local",
		ApproverEphemeralPublic: sha256.Sum256([]byte("approver-ephemeral-" + deviceID)), TrustAnchorHash: [32]byte(previous.Authority.AnchorHash), CurrentRosterHash: [32]byte(previous.Hash)}
	credential, err := PreparePairingCredential(previous, PairingCredentialProposalV1{Transcript: transcript, UserID: "user-local", KeyEpoch: keyEpoch,
		JoinNonce: sha256.Sum256([]byte("join-" + deviceID)), IssuedAt: existingAccountGenesisTestTime.Add(time.Hour), NotAfter: existingAccountGenesisTestTime.Add(364 * 24 * time.Hour)})
	require.NoError(t, err)
	credential.CandidateProof, err = SignDevicePossession(credential, candidate.SigningPrivate)
	require.NoError(t, err)
	credential.ApproverProof, err = SignPairingApproval(previous, credential, transcript, approver.SigningPrivate)
	require.NoError(t, err)
	return credential, transcript
}

func newTransitionTestDevice(t *testing.T) keys.DeviceIdentity {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	wrapPrivate, wrapPublic, err := keys.NewDeviceKey()
	require.NoError(t, err)
	return keys.DeviceIdentity{SigningPrivate: private, SigningPublic: public, SigningKeyID: sha256.Sum256(public), WrapPrivate: wrapPrivate, WrapPublic: wrapPublic, WrapKeyID: sha256.Sum256(wrapPublic[:])}
}
