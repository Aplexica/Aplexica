package generationactivation

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/identity"
	"github.com/aplexica/aplexica/internal/keys"
	"github.com/stretchr/testify/require"
)

type thresholdCollector struct {
	input       BuildInput
	second      keys.DeviceIdentity
	secondID    string
	fail        bool
	first       GenerationActivationUnsignedV1
	collections int
}

func (c *thresholdCollector) CollectActivationEndorsements(_ context.Context, unsigned GenerationActivationUnsignedV1, existing []ActivationEndorsementV1) (GenerationActivationUnsignedV1, []ActivationEndorsementV1, error) {
	c.collections++
	if c.first == (GenerationActivationUnsignedV1{}) {
		c.first = unsigned
	} else if c.first != unsigned {
		return GenerationActivationUnsignedV1{}, nil, errors.New("proposal changed across restart")
	}
	if c.fail {
		return GenerationActivationUnsignedV1{}, nil, errors.New("authority temporarily offline")
	}
	input := c.input
	input.DeviceID, input.DeviceIdentity = c.secondID, c.second
	endorsement, err := Endorse(input, unsigned)
	if err != nil {
		return GenerationActivationUnsignedV1{}, nil, err
	}
	return unsigned, append(append([]ActivationEndorsementV1(nil), existing...), endorsement), nil
}

func TestCoordinatorResumesExactThresholdProposalWithoutSharingPrivateKeys(t *testing.T) {
	fixture := newActivationFixture(t)
	snapshot, err := fixture.chain.PublicationSnapshot(fixture.now)
	require.NoError(t, err)
	previous := snapshot.Current

	secondPublic, secondPrivate, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	secondWrapPrivate, secondWrapPublic, err := keys.NewDeviceKey()
	require.NoError(t, err)
	second := keys.DeviceIdentity{SigningPrivate: secondPrivate, SigningPublic: secondPublic, SigningKeyID: sha256.Sum256(secondPublic),
		WrapPrivate: secondWrapPrivate, WrapPublic: secondWrapPublic, WrapKeyID: sha256.Sum256(secondWrapPublic[:])}
	var secondSigningPublic [32]byte
	copy(secondSigningPublic[:], secondPublic)
	transcript := identity.PairingTranscriptV1{
		Version: 1, ServiceOrigin: previous.Authority.Anchor.Anchor.ServiceOrigin, AccountID: fixture.account, PendingID: "pending-threshold",
		PairingNonce: sha256.Sum256([]byte("pairing-nonce")), CandidateDeviceID: "device-b",
		CandidateEphemeralPublic: sha256.Sum256([]byte("candidate-ephemeral")), CandidateSigningPublic: secondSigningPublic,
		CandidateWrapPublic: second.WrapPublic, CandidateEnvelopeVersions: []uint16{2}, ApproverDeviceID: fixture.deviceID,
		ApproverEphemeralPublic: sha256.Sum256([]byte("approver-ephemeral")), TrustAnchorHash: [32]byte(previous.Authority.AnchorHash), CurrentRosterHash: [32]byte(previous.Hash),
	}
	issued := fixture.now.Add(time.Second)
	credentialUnsigned, err := identity.PreparePairingCredential(previous, identity.PairingCredentialProposalV1{
		Transcript: transcript, UserID: "user-a", KeyEpoch: 1, JoinNonce: sha256.Sum256([]byte("join")), IssuedAt: issued, NotAfter: issued.Add(365 * 24 * time.Hour),
	})
	require.NoError(t, err)
	credentialUnsigned.CandidateProof, err = identity.SignDevicePossession(credentialUnsigned, second.SigningPrivate)
	require.NoError(t, err)
	credentialUnsigned.ApproverProof, err = identity.SignPairingApproval(previous, credentialUnsigned, transcript, fixture.device.SigningPrivate)
	require.NoError(t, err)
	credentialEndorsement, err := identity.EndorseDeviceCredential(previous, credentialUnsigned, fixture.device.SigningKeyID, fixture.device.SigningPrivate)
	require.NoError(t, err)
	secondCredential, err := identity.FinalizeDeviceCredential(previous, credentialUnsigned, []identity.DeviceTransitionEndorsementV1{credentialEndorsement})
	require.NoError(t, err)

	firstAuthority := previous.Authority.Authorities[identity.DeviceKeyID(fixture.device.SigningKeyID)]
	secondAuthority := identity.RosterAuthorityV1{DeviceID: "device-b", SigningKeyID: second.SigningKeyID, SigningPublicKey: secondSigningPublic}
	authorities := []identity.RosterAuthorityV1{firstAuthority, secondAuthority}
	sort.Slice(authorities, func(i, j int) bool {
		return string(authorities[i].SigningKeyID[:]) < string(authorities[j].SigningKeyID[:])
	})
	transitionUnsigned := identity.AuthorityTransitionUnsignedV1{
		Version: 1, AccountID: fixture.account, TrustAnchorHash: [32]byte(previous.Authority.AnchorHash), PreviousStateHash: previous.Authority.StateHash,
		PreviousAuthorityEpoch: previous.Authority.AuthorityEpoch, NewAuthorityEpoch: previous.Authority.AuthorityEpoch + 1,
		NewAuthorities: authorities, NewThreshold: 2, AuthorizationMode: "operational", IssuedAtUnix: issued.Unix(), Nonce: sha256.Sum256([]byte("authority-transition")),
	}
	transitionPreimage, err := canonicalEncoding.Marshal([]any{"aplexica/authority-transition/v1", transitionUnsigned})
	require.NoError(t, err)
	transition := identity.AuthorityTransitionV1{Transition: transitionUnsigned, SignerKeyIDs: [][32]byte{fixture.device.SigningKeyID}, Signatures: make([][64]byte, 1)}
	copy(transition.Signatures[0][:], ed25519.Sign(fixture.device.SigningPrivate, transitionPreimage))
	nextAuthority, err := identity.VerifyAuthorityTransition(previous.Authority, transition)
	require.NoError(t, err)

	nextUnsigned := previous.Manifest.Manifest
	nextUnsigned.Epoch++
	nextUnsigned.PreviousHash = [32]byte(previous.Hash)
	nextUnsigned.AuthorityEpoch = nextAuthority.AuthorityEpoch
	nextUnsigned.AuthorityStateHash = nextAuthority.StateHash
	nextUnsigned.AccessGeneration++
	nextUnsigned.IssuedAtUnix = issued.Unix()
	nextUnsigned.NotAfterUnix = fixture.now.Add(time.Hour).Unix()
	nextUnsigned.Devices = append(append([]identity.DeviceCertificateV1(nil), previous.Manifest.Manifest.Devices...), secondCredential)
	sort.Slice(nextUnsigned.Devices, func(i, j int) bool {
		return nextUnsigned.Devices[i].Certificate.DeviceID < nextUnsigned.Devices[j].Certificate.DeviceID
	})
	nextUnsigned.AccessSetHash, err = identity.AccessSetHash(nextUnsigned)
	require.NoError(t, err)
	rosterPreimage, err := canonicalEncoding.Marshal([]any{"aplexica/roster-manifest/v1", nextUnsigned})
	require.NoError(t, err)
	type signer struct {
		id      [32]byte
		private ed25519.PrivateKey
	}
	signers := []signer{{fixture.device.SigningKeyID, fixture.device.SigningPrivate}, {second.SigningKeyID, second.SigningPrivate}}
	sort.Slice(signers, func(i, j int) bool { return string(signers[i].id[:]) < string(signers[j].id[:]) })
	nextRoster := identity.RosterManifestV1{Manifest: nextUnsigned, SignerKeyIDs: make([][32]byte, 2), Signatures: make([][64]byte, 2)}
	for index, signer := range signers {
		nextRoster.SignerKeyIDs[index] = signer.id
		copy(nextRoster.Signatures[index][:], ed25519.Sign(signer.private, rosterPreimage))
	}
	verified, err := fixture.chain.AppendAtomic(identity.AtomicAuthorityRosterTransitionV1{AuthorityTransition: transition, NextRoster: nextRoster})
	require.NoError(t, err)
	require.Equal(t, uint16(2), verified.Authority.Threshold)

	epoch := fixture.epoch
	epoch.RosterHash = [32]byte(verified.Hash)
	epoch.AccessGeneration = verified.Manifest.Manifest.AccessGeneration
	epoch.AccessSetHash = verified.Manifest.Manifest.AccessSetHash
	epoch.CoordinatorGeneration++
	epoch.BarrierID = sha256.Sum256([]byte("threshold-barrier"))
	state := FileStateStore{Path: filepath.Join(t.TempDir(), "activation-state.json")}
	journalPath := filepath.Join(t.TempDir(), "endorsement-state.json")
	journal := FileEndorsementJournal{Path: journalPath}
	transport := &recordingTransport{receipt: ActivationReceipt{AuthorityDigest: sha256Hex("threshold-authority"), Revision: 1}}
	collector := &thresholdCollector{second: second, secondID: "device-b", fail: true}
	coordinator := &Coordinator{Chain: fixture.chain, Epoch: epoch, StreamEpoch: "stream-threshold", DeviceID: fixture.deviceID,
		Identity: fixedIdentitySource{identity: fixture.device}, State: state, Transport: transport, Collector: collector, Endorsement: journal, Now: func() time.Time { return issued }}
	collector.input = BuildInput{AccountID: fixture.account, StreamEpoch: "stream-threshold", Roster: verified, SecurityEpoch: epoch, Now: issued}
	_, err = coordinator.RunOnce(context.Background())
	require.EqualError(t, err, "authority temporarily offline")
	firstProposal := collector.first
	require.FileExists(t, journalPath)

	collector.fail = false
	restarted := *coordinator
	result, err := restarted.RunOnce(context.Background())
	require.NoError(t, err)
	require.Equal(t, firstProposal, collector.first)
	require.NotZero(t, result.BindingDigest)
	require.Len(t, transport.activations, 1)
	require.NoFileExists(t, journalPath)
	decoded, err := DecodeCanonical(transport.activations[0])
	require.NoError(t, err)
	require.Len(t, decoded.SignerKeyIDs, 2)
	require.Less(t, string(decoded.SignerKeyIDs[0][:]), string(decoded.SignerKeyIDs[1][:]))
}
