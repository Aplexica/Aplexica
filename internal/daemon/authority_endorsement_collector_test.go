package daemon

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/generationactivation"
	"github.com/aplexica/aplexica/internal/identity"
	"github.com/aplexica/aplexica/internal/keys"
	"github.com/aplexica/aplexica/internal/plugin/proto"
	"github.com/stretchr/testify/require"
)

type collectorIdentitySource struct{ value keys.DeviceIdentity }

func (s collectorIdentitySource) LoadExisting() (keys.DeviceIdentity, error) { return s.value, nil }

type electedProposalExchange struct {
	t             *testing.T
	purpose       string
	round         [32]byte
	proposal      []byte
	expiresAtUnix int64
	peer          proto.RemoteAuthorityEndorsementV1
	calls         int
}

func (e *electedProposalExchange) ExchangeAuthorityEndorsementsV1(_ context.Context, params proto.RemoteExchangeAuthorityEndorsementsV1Params) (proto.RemoteExchangeAuthorityEndorsementsV1Result, error) {
	e.t.Helper()
	e.calls++
	require.Equal(e.t, e.purpose, params.Purpose)
	require.Equal(e.t, e.round, params.RoundDigest)
	result := proto.RemoteExchangeAuthorityEndorsementsV1Result{
		ProposalDigest: sha256.Sum256(e.proposal), Proposal: append([]byte(nil), e.proposal...), ExpiresAtUnix: e.expiresAtUnix,
		Endorsements: []proto.RemoteAuthorityEndorsementV1{e.peer},
	}
	if e.calls == 2 {
		require.Equal(e.t, e.proposal, params.Proposal)
		require.Len(e.t, params.Endorsements, 1)
		result.Endorsements = append(result.Endorsements, params.Endorsements[0])
	}
	return result, nil
}

func TestGenerationActivationCollectorConvergesOnPeerProposalAndReachesThreshold(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	roster, devices := twoAuthorityCollectorRoster(t, now)
	epoch := generationactivation.SecurityEpochState{
		Version: 1, ScopeType: "account", ScopeID: roster.Manifest.Manifest.ScopeID, RosterHash: [32]byte(roster.Hash),
		AccessGeneration: 1, AccessSetHash: roster.Manifest.Manifest.AccessSetHash, BarrierID: sha256.Sum256([]byte("barrier")),
		TreeHeadDigest: sha256.Sum256([]byte("tree")), KeyMode: "recipient-wrap-v2", CoordinatorGeneration: 1,
	}
	base := generationactivation.BuildInput{AccountID: "account-a", StreamEpoch: "stream-a", Roster: roster, SecurityEpoch: epoch, Now: now}
	localInput := base
	localInput.DeviceID, localInput.DeviceIdentity = "device-a", devices[0]
	localInput.Random = bytes.NewReader(bytes.Repeat([]byte{0x11}, 32))
	localProposal, _, err := generationactivation.Prepare(localInput)
	require.NoError(t, err)

	peerInput := base
	peerInput.DeviceID, peerInput.DeviceIdentity = "device-b", devices[1]
	peerInput.Random = bytes.NewReader(bytes.Repeat([]byte{0x22}, 32))
	peerProposal, _, err := generationactivation.Prepare(peerInput)
	require.NoError(t, err)
	require.NotEqual(t, localProposal, peerProposal)
	peerSignature, err := generationactivation.Endorse(peerInput, peerProposal)
	require.NoError(t, err)
	peerBytes, err := generationactivation.EncodeUnsignedCanonical(peerProposal)
	require.NoError(t, err)
	round, err := generationactivation.EndorsementRoundDigest(peerProposal)
	require.NoError(t, err)

	exchange := &electedProposalExchange{t: t, purpose: proto.RemoteAuthorityPurposeGenerationActivation, round: round,
		proposal: peerBytes, expiresAtUnix: peerProposal.NotAfterUnix,
		peer: proto.RemoteAuthorityEndorsementV1{SignerKeyID: peerSignature.SignerKeyID, Signature: peerSignature.Signature}}
	collector := GenerationActivationEndorsementCollector{Exchange: exchange, Input: localInput}
	selected, endorsements, err := collector.CollectActivationEndorsements(context.Background(), localProposal, nil)
	require.NoError(t, err)
	require.Equal(t, peerProposal, selected)
	require.Equal(t, 2, exchange.calls)
	_, _, _, err = generationactivation.Finalize(localInput, selected, endorsements)
	require.NoError(t, err, "two independently produced authority signatures must activate threshold two")
}

func TestFreshnessCollectorConvergesOnPeerProposalAndReachesThreshold(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	previous, devices := twoAuthorityCollectorRoster(t, now)
	localProposal, err := identity.PrepareFreshnessRenewal(previous, now)
	require.NoError(t, err)
	peerProposal, err := identity.PrepareFreshnessRenewal(previous, now.Add(time.Second))
	require.NoError(t, err)
	require.NotEqual(t, localProposal, peerProposal)
	peerSignature, err := identity.SignFreshnessRenewal(previous, peerProposal, devices[1].SigningKeyID, devices[1].SigningPrivate)
	require.NoError(t, err)
	peerBytes, err := identity.CanonicalRosterBytes(peerProposal)
	require.NoError(t, err)
	round, err := identity.FreshnessEndorsementRoundDigest(previous)
	require.NoError(t, err)

	exchange := &electedProposalExchange{t: t, purpose: proto.RemoteAuthorityPurposeRosterFreshness, round: round,
		proposal: peerBytes, expiresAtUnix: previous.Manifest.Manifest.NotAfterUnix,
		peer: proto.RemoteAuthorityEndorsementV1{SignerKeyID: peerSignature.SignerKeyID, Signature: peerSignature.Signature}}
	collector := FreshnessEndorsementCollector{Exchange: exchange, Identity: collectorIdentitySource{value: devices[0]}}
	selected, endorsements, err := collector.CollectFreshnessEndorsements(context.Background(), previous, localProposal)
	require.NoError(t, err)
	require.Equal(t, peerProposal, selected)
	require.Equal(t, 2, exchange.calls)
	_, _, err = identity.FinalizeFreshnessRenewal(previous, selected, endorsements)
	require.NoError(t, err, "two independently produced authority signatures must renew threshold two")
}

func twoAuthorityCollectorRoster(t *testing.T, now time.Time) (identity.VerifiedRoster, [2]keys.DeviceIdentity) {
	t.Helper()
	var devices [2]keys.DeviceIdentity
	authorities := make(map[identity.DeviceKeyID]identity.RosterAuthorityV1, 2)
	certificates := make([]identity.DeviceCertificateV1, 2)
	for index, deviceID := range []string{"device-a", "device-b"} {
		public, private, err := ed25519.GenerateKey(rand.Reader)
		require.NoError(t, err)
		wrapPrivate, wrapPublic, err := keys.NewDeviceKey()
		require.NoError(t, err)
		devices[index] = keys.DeviceIdentity{SigningPrivate: private, SigningPublic: public, SigningKeyID: sha256.Sum256(public),
			WrapPrivate: wrapPrivate, WrapPublic: wrapPublic, WrapKeyID: sha256.Sum256(wrapPublic[:])}
		var signingPublic [32]byte
		copy(signingPublic[:], public)
		authority := identity.RosterAuthorityV1{DeviceID: deviceID, SigningKeyID: devices[index].SigningKeyID, SigningPublicKey: signingPublic}
		authorities[identity.DeviceKeyID(authority.SigningKeyID)] = authority
		certificates[index].Certificate = identity.DeviceCertificateUnsignedV1{
			Version: 1, AccountID: "account-a", UserID: "user-a", DeviceID: deviceID, KeyEpoch: 1,
			SigningKeyID: devices[index].SigningKeyID, SigningPublicKey: signingPublic, WrapKeyID: devices[index].WrapKeyID,
			WrapPublicKey: devices[index].WrapPublic, EnvelopeVersions: []uint16{2}, NotBeforeUnix: now.Add(-24 * time.Hour).Unix(),
			NotAfterUnix: now.Add(300 * 24 * time.Hour).Unix(), IssuanceMode: "pairing", IssuedUnderAuthorityEpoch: 1,
		}
	}
	anchorHash := identity.TrustAnchorHash(sha256.Sum256([]byte("anchor")))
	stateHash := sha256.Sum256([]byte("authority-state"))
	scopeID := "0197f30a-3c58-7000-8000-000000000001"
	manifest := identity.RosterManifestUnsignedV1{
		Version: 1, ScopeType: "account", ScopeID: scopeID, Epoch: 1, TrustAnchorHash: [32]byte(anchorHash),
		AuthorityStateHash: stateHash, AuthorityEpoch: 1, AccessGeneration: 1, IssuedAtUnix: now.Add(-7 * time.Hour).Unix(),
		NotAfterUnix: now.Add(17 * time.Hour).Unix(), MinEnvelopeVersion: 2, Devices: certificates,
	}
	var err error
	manifest.AccessSetHash, err = identity.AccessSetHash(manifest)
	require.NoError(t, err)
	signed := identity.RosterManifestV1{Manifest: manifest}
	hash, err := identity.HashRoster(signed)
	require.NoError(t, err)
	authority := identity.VerifiedAuthorityState{
		Anchor:     identity.AccountTrustAnchorV1{Anchor: identity.AccountTrustAnchorUnsignedV1{AccountID: "account-a", PersonalScopeID: scopeID}},
		AnchorHash: anchorHash, StateHash: stateHash, AuthorityEpoch: 1, Authorities: authorities, Threshold: 2,
	}
	return identity.VerifiedRoster{Manifest: signed, Hash: hash, Authority: authority}, devices
}
