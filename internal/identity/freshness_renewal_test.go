package identity

import (
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/securityerr"
	"github.com/stretchr/testify/require"
)

var existingAccountGenesisRenewalTime = time.Date(2026, 7, 20, 2, 0, 0, 0, time.UTC)

func TestFreshnessRenewalExtendsGenesisWithoutChangingAccess(t *testing.T) {
	genesis := existingAccountGenesisInstallerFixture(t)
	previous := genesis.Verified
	proposal, err := PrepareFreshnessRenewal(previous, existingAccountGenesisRenewalTime)
	require.NoError(t, err)
	require.Equal(t, uint64(2), proposal.Epoch)
	require.Equal(t, [32]byte(previous.Hash), proposal.PreviousHash)
	require.Equal(t, existingAccountGenesisRenewalTime.Unix(), proposal.IssuedAtUnix)
	require.Equal(t, existingAccountGenesisRenewalTime.Add(24*time.Hour).Unix(), proposal.NotAfterUnix)
	require.Greater(t, proposal.NotAfterUnix, previous.Manifest.Manifest.NotAfterUnix)
	require.Equal(t, previous.Manifest.Manifest.AccessGeneration, proposal.AccessGeneration)
	require.Equal(t, previous.Manifest.Manifest.AccessSetHash, proposal.AccessSetHash)
	require.Equal(t, previous.Manifest.Manifest.MinEnvelopeVersion, proposal.MinEnvelopeVersion)
	require.Equal(t, previous.Manifest.Manifest.Devices, proposal.Devices)

	device := existingAccountGenesisTestDevice(t)
	privateBefore := append(ed25519.PrivateKey(nil), device.SigningPrivate...)
	endorsement, err := SignFreshnessRenewal(previous, proposal, device.SigningKeyID, device.SigningPrivate)
	require.NoError(t, err)
	require.Equal(t, privateBefore, device.SigningPrivate)
	require.Equal(t, device.SigningKeyID, endorsement.SignerKeyID)

	signed, verified, err := FinalizeFreshnessRenewal(previous, proposal, []RosterFreshnessEndorsementV1{endorsement})
	require.NoError(t, err)
	require.Equal(t, proposal, signed.Manifest)
	require.Equal(t, uint64(2), verified.Manifest.Manifest.Epoch)
	require.NotEqual(t, previous.Hash, verified.Hash)
	require.Equal(t, previous.Manifest.Manifest.AccessGeneration, verified.Manifest.Manifest.AccessGeneration)
	require.Equal(t, previous.Manifest.Manifest.AccessSetHash, verified.Manifest.Manifest.AccessSetHash)
	require.Equal(t, previous.Authority.StateHash, verified.Authority.StateHash)
}

func TestFreshnessRenewalRejectsMutationStaleTimingAndCredentialHorizon(t *testing.T) {
	previous := existingAccountGenesisInstallerFixture(t).Verified
	if _, err := PrepareFreshnessRenewal(previous, time.Unix(previous.Manifest.Manifest.IssuedAtUnix, 0)); !errors.Is(err, ErrInvalidFreshnessRenewal) {
		t.Fatalf("non-advancing issue time error = %v", err)
	}
	if _, err := PrepareFreshnessRenewal(previous, time.Unix(previous.Manifest.Manifest.NotAfterUnix, 0)); !errors.Is(err, ErrInvalidFreshnessRenewal) {
		t.Fatalf("expired roster renewal error = %v", err)
	}

	proposal, err := PrepareFreshnessRenewal(previous, existingAccountGenesisRenewalTime)
	require.NoError(t, err)
	mutations := []struct {
		name   string
		mutate func(*RosterManifestUnsignedV1)
	}{
		{name: "device key", mutate: func(value *RosterManifestUnsignedV1) { value.Devices[0].Certificate.WrapPublicKey[0] ^= 1 }},
		{name: "access generation", mutate: func(value *RosterManifestUnsignedV1) { value.AccessGeneration++ }},
		{name: "access hash", mutate: func(value *RosterManifestUnsignedV1) { value.AccessSetHash[0] ^= 1 }},
		{name: "minimum envelope", mutate: func(value *RosterManifestUnsignedV1) { value.MinEnvelopeVersion++ }},
		{name: "authority state", mutate: func(value *RosterManifestUnsignedV1) { value.AuthorityStateHash[0] ^= 1 }},
		{name: "non-extension", mutate: func(value *RosterManifestUnsignedV1) { value.NotAfterUnix = previous.Manifest.Manifest.NotAfterUnix }},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			candidate := cloneRosterManifestUnsigned(proposal)
			mutation.mutate(&candidate)
			_, err := SignFreshnessRenewal(previous, candidate, existingAccountGenesisTestDevice(t).SigningKeyID, existingAccountGenesisTestDevice(t).SigningPrivate)
			require.Error(t, err)
		})
	}

	credentialBound := previous
	credentialBound.Manifest.Manifest = cloneRosterManifestUnsigned(previous.Manifest.Manifest)
	credentialBound.Manifest.Manifest.Devices[0].Certificate.NotAfterUnix = previous.Manifest.Manifest.NotAfterUnix
	credentialBound.Hash, err = HashRoster(credentialBound.Manifest)
	require.NoError(t, err)
	_, err = PrepareFreshnessRenewal(credentialBound, existingAccountGenesisRenewalTime)
	require.ErrorIs(t, err, ErrCredentialRenewalRequired)
}

func TestFreshnessRenewalRequiresActiveAuthorityAndValidThresholdSignatures(t *testing.T) {
	previous := existingAccountGenesisInstallerFixture(t).Verified
	proposal, err := PrepareFreshnessRenewal(previous, existingAccountGenesisRenewalTime)
	require.NoError(t, err)
	device := existingAccountGenesisTestDevice(t)
	endorsement, err := SignFreshnessRenewal(previous, proposal, device.SigningKeyID, device.SigningPrivate)
	require.NoError(t, err)

	otherSeed := make([]byte, ed25519.SeedSize)
	for index := range otherSeed {
		otherSeed[index] = 0x99
	}
	otherPrivate := ed25519.NewKeyFromSeed(otherSeed)
	clearBytes(otherSeed)
	otherPublic := otherPrivate.Public().(ed25519.PublicKey)
	otherID := sha256.Sum256(otherPublic)
	if _, err := SignFreshnessRenewal(previous, proposal, otherID, otherPrivate); !errors.Is(err, ErrFreshnessAuthorityUnavailable) {
		t.Fatalf("non-authority signing error = %v", err)
	}
	wrongPrivate := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	if _, err := SignFreshnessRenewal(previous, proposal, device.SigningKeyID, wrongPrivate); !errors.Is(err, ErrFreshnessAuthorityUnavailable) {
		t.Fatalf("wrong private key error = %v", err)
	}

	if _, _, err := FinalizeFreshnessRenewal(previous, proposal, nil); !errors.Is(err, securityerr.ErrInvalidSignature) {
		t.Fatalf("missing threshold error = %v", err)
	}
	if _, _, err := FinalizeFreshnessRenewal(previous, proposal, []RosterFreshnessEndorsementV1{endorsement, endorsement}); !errors.Is(err, securityerr.ErrInvalidSignature) {
		t.Fatalf("duplicate endorsement error = %v", err)
	}
	tampered := endorsement
	tampered.Signature[0] ^= 1
	if _, _, err := FinalizeFreshnessRenewal(previous, proposal, []RosterFreshnessEndorsementV1{tampered}); !errors.Is(err, securityerr.ErrInvalidSignature) {
		t.Fatalf("tampered endorsement error = %v", err)
	}
}

func TestFreshnessRenewalRejectsForgedPreviousHash(t *testing.T) {
	previous := existingAccountGenesisInstallerFixture(t).Verified
	proposal, err := PrepareFreshnessRenewal(previous, existingAccountGenesisRenewalTime)
	require.NoError(t, err)
	device := existingAccountGenesisTestDevice(t)
	previous.Hash[0] ^= 1
	_, err = SignFreshnessRenewal(previous, proposal, device.SigningKeyID, device.SigningPrivate)
	require.ErrorIs(t, err, ErrInvalidFreshnessRenewal)
	_, _, err = FinalizeFreshnessRenewal(previous, proposal, nil)
	require.ErrorIs(t, err, ErrInvalidFreshnessRenewal)
}
