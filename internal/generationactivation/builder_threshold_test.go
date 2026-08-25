package generationactivation

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"sort"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/identity"
	"github.com/aplexica/aplexica/internal/keys"
	"github.com/stretchr/testify/require"
)

func TestActivationAcceptsExternallyCollectedThresholdWithoutPrivateKeySharing(t *testing.T) {
	fixture := newActivationFixture(t)
	snapshot, err := fixture.chain.PublicationSnapshot(fixture.now)
	require.NoError(t, err)
	roster := snapshot.Current

	secondPublic, secondPrivate, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	secondID := sha256.Sum256(secondPublic)
	wrapPrivate, wrapPublic, err := keys.NewDeviceKey()
	require.NoError(t, err)
	secondIdentity := keys.DeviceIdentity{SigningPrivate: secondPrivate, SigningPublic: secondPublic, SigningKeyID: secondID, WrapPrivate: wrapPrivate, WrapPublic: wrapPublic, WrapKeyID: sha256.Sum256(wrapPublic[:])}
	var secondPublicArray [32]byte
	copy(secondPublicArray[:], secondPublic)
	secondAuthority := identity.RosterAuthorityV1{DeviceID: "device-b", SigningKeyID: secondID, SigningPublicKey: secondPublicArray}
	roster.Authority.Authorities[identity.DeviceKeyID(secondID)] = secondAuthority
	roster.Authority.Threshold = 2
	secondCredential := identity.DeviceCertificateV1{Certificate: identity.DeviceCertificateUnsignedV1{
		Version: 1, AccountID: fixture.account, UserID: "user-a", DeviceID: "device-b", KeyEpoch: 1,
		SigningKeyID: secondID, SigningPublicKey: secondPublicArray, WrapKeyID: secondIdentity.WrapKeyID, WrapPublicKey: secondIdentity.WrapPublic,
		EnvelopeVersions: []uint16{2}, NotBeforeUnix: fixture.now.Add(-time.Minute).Unix(), NotAfterUnix: fixture.now.Add(time.Hour).Unix(),
	}}
	roster.Manifest.Manifest.Devices = append(roster.Manifest.Manifest.Devices, secondCredential)
	sort.Slice(roster.Manifest.Manifest.Devices, func(i, j int) bool {
		return roster.Manifest.Manifest.Devices[i].Certificate.DeviceID < roster.Manifest.Manifest.Devices[j].Certificate.DeviceID
	})
	roster.Manifest.Manifest.AccessSetHash, err = identity.AccessSetHash(roster.Manifest.Manifest)
	require.NoError(t, err)
	roster.Hash, err = identity.HashRoster(roster.Manifest)
	require.NoError(t, err)
	epoch := fixture.epoch
	epoch.RosterHash = [32]byte(roster.Hash)
	epoch.AccessSetHash = roster.Manifest.Manifest.AccessSetHash

	first := BuildInput{AccountID: fixture.account, StreamEpoch: "stream-epoch-a", Roster: roster, SecurityEpoch: epoch,
		DeviceID: fixture.deviceID, DeviceIdentity: fixture.device, Now: fixture.now, Random: bytesReader(41)}
	unsigned, binding, err := Prepare(first)
	require.NoError(t, err)
	firstEndorsement, err := Endorse(first, unsigned)
	require.NoError(t, err)
	second := first
	second.DeviceID = "device-b"
	second.DeviceIdentity = secondIdentity
	secondEndorsement, err := Endorse(second, unsigned)
	require.NoError(t, err)

	signed, blob, finalizedBinding, err := Finalize(first, unsigned, []ActivationEndorsementV1{secondEndorsement, firstEndorsement})
	require.NoError(t, err)
	require.Equal(t, binding, finalizedBinding)
	require.Len(t, signed.SignerKeyIDs, 2)
	require.Less(t, string(signed.SignerKeyIDs[0][:]), string(signed.SignerKeyIDs[1][:]))
	decoded, err := DecodeCanonical(blob)
	require.NoError(t, err)
	require.Equal(t, signed, decoded)

	_, _, _, err = Finalize(first, unsigned, []ActivationEndorsementV1{firstEndorsement})
	require.ErrorIs(t, err, ErrSigningAuthorityUnavailable)
	_, _, _, err = Finalize(first, unsigned, []ActivationEndorsementV1{firstEndorsement, firstEndorsement})
	require.ErrorIs(t, err, ErrSigningAuthorityUnavailable)
	tampered := secondEndorsement
	tampered.Signature[0] ^= 1
	_, _, _, err = Finalize(first, unsigned, []ActivationEndorsementV1{firstEndorsement, tampered})
	require.ErrorIs(t, err, ErrSigningAuthorityUnavailable)
}
