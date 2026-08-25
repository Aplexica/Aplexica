package identity

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/keys"
	"github.com/aplexica/aplexica/internal/securityerr"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/curve25519"
)

const existingAccountGenesisTestMnemonic = "abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon art"

var existingAccountGenesisTestTime = time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)

func existingAccountGenesisTestDevice(t *testing.T) keys.DeviceIdentity {
	t.Helper()
	signingSeed := bytes.Repeat([]byte{0x42}, ed25519.SeedSize)
	signingPrivate := ed25519.NewKeyFromSeed(signingSeed)
	clearBytes(signingSeed)
	signingPublic := append(ed25519.PublicKey(nil), signingPrivate.Public().(ed25519.PublicKey)...)
	var wrapPrivate [32]byte
	for index := range wrapPrivate {
		wrapPrivate[index] = byte(0x80 + index)
	}
	wrapPrivate[0] &= 248
	wrapPrivate[31] &= 127
	wrapPrivate[31] |= 64
	wrapPublicBytes, err := curve25519.X25519(wrapPrivate[:], curve25519.Basepoint)
	require.NoError(t, err)
	var wrapPublic [32]byte
	copy(wrapPublic[:], wrapPublicBytes)
	clearBytes(wrapPublicBytes)
	return keys.DeviceIdentity{
		WrapPrivate: wrapPrivate, WrapPublic: wrapPublic, WrapKeyID: sha256.Sum256(wrapPublic[:]),
		SigningPrivate: signingPrivate, SigningPublic: signingPublic, SigningKeyID: sha256.Sum256(signingPublic),
	}
}

func existingAccountGenesisRandom() []byte {
	raw := make([]byte, 16+16+32+32+32)
	for index := range raw {
		raw[index] = byte(index + 1)
	}
	return raw
}

func existingAccountGenesisInput(t *testing.T, mnemonic []byte) ExistingAccountGenesisInput {
	t.Helper()
	return ExistingAccountGenesisInput{
		ServiceOrigin: "https://api.aplexica.com", AccountID: "account-existing", UserID: "user-local", DeviceID: "device-local",
		Confirmed: true, ConfirmedRecoveryMnemonic: mnemonic, DeviceIdentity: existingAccountGenesisTestDevice(t),
		Random: bytes.NewReader(existingAccountGenesisRandom()), Clock: func() time.Time { return existingAccountGenesisTestTime },
	}
}

func requireCleared(t *testing.T, value []byte) {
	t.Helper()
	require.Equal(t, make([]byte, len(value)), value)
}

func TestBuildExistingAccountGenesisCreatesOnlyTheLocalVerifiedDevice(t *testing.T) {
	mnemonic := []byte(existingAccountGenesisTestMnemonic)
	input := existingAccountGenesisInput(t, mnemonic)
	randomBytes := existingAccountGenesisRandom()
	var salt [16]byte
	copy(salt[:], randomBytes[:16])
	derived, err := DeriveRecoveryKeys([]byte(existingAccountGenesisTestMnemonic), salt, RecoveryKDFProfileArgon2idV1)
	require.NoError(t, err)
	privateSigningSeed := derived.SigningSeed
	privateWrapKey := derived.WrapPrivate
	derived.Clear()
	defer clearBytes(privateSigningSeed[:])
	defer clearBytes(privateWrapKey[:])

	result, err := BuildExistingAccountGenesis(input)
	require.NoError(t, err)
	requireCleared(t, mnemonic)

	anchor := result.Chain.Anchor.Anchor
	require.Equal(t, uint16(1), anchor.Version)
	require.Equal(t, "019f7cd2-8800-7718-991a-1b1c1d1e1f20", anchor.PersonalScopeID)
	require.Equal(t, salt, anchor.RecoverySalt)
	require.Equal(t, result.Chain.ExpectedRecoveryRoot, ed25519.PublicKey(anchor.RecoveryRootPublicKey[:]))
	require.Len(t, anchor.Authorities, 1)
	require.Equal(t, uint16(1), anchor.AuthorityThreshold)
	require.Equal(t, input.DeviceID, anchor.Authorities[0].DeviceID)
	require.Equal(t, input.DeviceIdentity.SigningKeyID, anchor.Authorities[0].SigningKeyID)

	verifiedAuthority, err := VerifyTrustAnchor(result.Chain.Anchor, result.Chain.ExpectedRecoveryRoot)
	require.NoError(t, err)
	require.Equal(t, verifiedAuthority, result.Verified.Authority)
	require.Len(t, result.Chain.Roster.Manifest.Devices, 1)
	credential := result.Chain.Roster.Manifest.Devices[0]
	require.Equal(t, input.DeviceID, credential.Certificate.DeviceID)
	require.Equal(t, input.UserID, credential.Certificate.UserID)
	require.Equal(t, "recovery", credential.Certificate.IssuanceMode)
	require.Equal(t, []uint16{2, 3}, credential.Certificate.EnvelopeVersions)
	require.Equal(t, existingAccountGenesisTestTime.Unix(), credential.Certificate.NotBeforeUnix)
	require.Equal(t, existingAccountGenesisTestTime.Add(365*24*time.Hour).Unix(), credential.Certificate.NotAfterUnix)
	require.Zero(t, credential.Certificate.ApproverDeviceID)
	require.Zero(t, credential.Certificate.ApproverSigningKeyID)
	require.Zero(t, credential.Certificate.ApproverProof)
	require.Equal(t, [32]byte(verifiedAuthority.AnchorHash), credential.Certificate.EnrollmentContextHash)
	var wantJoinNonce [32]byte
	copy(wantJoinNonce[:], randomBytes[96:128])
	require.Equal(t, wantJoinNonce, credential.Certificate.JoinNonce)
	require.NoError(t, verifyExistingAccountGenesisCredential(credential.Certificate, verifiedAuthority))

	verifiedRoster, err := VerifyGenesis(verifiedAuthority, result.Chain.Roster)
	require.NoError(t, err)
	require.Equal(t, verifiedRoster, result.Verified)
	require.Equal(t, uint64(1), result.Verified.Manifest.Manifest.Epoch)
	require.Equal(t, uint64(1), result.Verified.Manifest.Manifest.AccessGeneration)
	require.Equal(t, uint16(2), result.Verified.Manifest.Manifest.MinEnvelopeVersion)
	require.Equal(t, existingAccountGenesisTestTime.Add(24*time.Hour).Unix(), result.Verified.Manifest.Manifest.NotAfterUnix)

	epoch := result.SecurityEpoch
	require.Equal(t, uint16(1), epoch.Version)
	require.Equal(t, "account", epoch.ScopeType)
	require.Equal(t, anchor.PersonalScopeID, epoch.ScopeID)
	require.Equal(t, [32]byte(result.Verified.Hash), epoch.RosterHash)
	require.Equal(t, result.Verified.Manifest.Manifest.AccessSetHash, epoch.AccessSetHash)
	require.Equal(t, uint64(1), epoch.AccessGeneration)
	require.Equal(t, uint64(1), epoch.CoordinatorGeneration)
	require.Equal(t, "recipient-wrap-v2", epoch.KeyMode)
	require.Zero(t, epoch.KeyVersion)
	var wantBarrier, wantTreeHead [32]byte
	copy(wantBarrier[:], randomBytes[32:64])
	copy(wantTreeHead[:], randomBytes[64:96])
	require.Equal(t, wantBarrier, epoch.BarrierID)
	require.Equal(t, wantTreeHead, epoch.TreeHeadDigest)

	encodedEpoch, err := json.Marshal(epoch)
	require.NoError(t, err)
	var epochFields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(encodedEpoch, &epochFields))
	require.Len(t, epochFields, 11)
	for _, field := range []string{"version", "scopeType", "scopeId", "rosterHash", "accessGeneration", "accessSetHash", "barrierId", "treeHeadDigest", "keyMode", "keyVersion", "coordinatorGeneration"} {
		require.Contains(t, epochFields, field)
	}

	chainStore := &ChainStore{Path: filepath.Join(t.TempDir(), "identity", "account", "chain.cbor")}
	initialized, err := chainStore.Initialize(result.Chain.Anchor, result.Chain.ExpectedRecoveryRoot, result.Chain.Roster)
	require.NoError(t, err)
	require.Equal(t, result.Verified.Hash, initialized.Hash)
	current, err := chainStore.Current(existingAccountGenesisTestTime.Add(time.Hour))
	require.NoError(t, err)
	require.Equal(t, result.Verified.Hash, current.Hash)

	encodedResult, err := enc.Marshal(result)
	require.NoError(t, err)
	require.NotContains(t, string(encodedResult), existingAccountGenesisTestMnemonic)
	require.False(t, bytes.Contains(encodedResult, privateSigningSeed[:]))
	require.False(t, bytes.Contains(encodedResult, privateWrapKey[:]))
}

func TestExistingAccountGenesisUUIDv7UsesInjectedClockAndRandomness(t *testing.T) {
	random := existingAccountGenesisRandom()[16:32]
	first, err := randomUUIDv7(bytes.NewReader(random), existingAccountGenesisTestTime)
	require.NoError(t, err)
	second, err := randomUUIDv7(bytes.NewReader(random), existingAccountGenesisTestTime)
	require.NoError(t, err)
	require.Equal(t, "019f7cd2-8800-7718-991a-1b1c1d1e1f20", first)
	require.Equal(t, first, second)
}

func TestBuildExistingAccountGenesisRejectsInvalidLocalInputsAndClearsMnemonic(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*ExistingAccountGenesisInput)
	}{
		{name: "not confirmed", mutate: func(input *ExistingAccountGenesisInput) { input.Confirmed = false }},
		{name: "service origin", mutate: func(input *ExistingAccountGenesisInput) { input.ServiceOrigin = "" }},
		{name: "account id", mutate: func(input *ExistingAccountGenesisInput) { input.AccountID = "account\nleak" }},
		{name: "user id", mutate: func(input *ExistingAccountGenesisInput) { input.UserID = "" }},
		{name: "device id", mutate: func(input *ExistingAccountGenesisInput) { input.DeviceID = "" }},
		{name: "signing key id", mutate: func(input *ExistingAccountGenesisInput) { input.DeviceIdentity.SigningKeyID[0] ^= 1 }},
		{name: "signing private", mutate: func(input *ExistingAccountGenesisInput) { input.DeviceIdentity.SigningPrivate[0] ^= 1 }},
		{name: "wrap key id", mutate: func(input *ExistingAccountGenesisInput) { input.DeviceIdentity.WrapKeyID[0] ^= 1 }},
		{name: "wrap private", mutate: func(input *ExistingAccountGenesisInput) { input.DeviceIdentity.WrapPrivate[8] ^= 1 }},
		{name: "zero clock", mutate: func(input *ExistingAccountGenesisInput) { input.Clock = func() time.Time { return time.Time{} } }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mnemonic := []byte(existingAccountGenesisTestMnemonic)
			input := existingAccountGenesisInput(t, mnemonic)
			test.mutate(&input)
			result, err := BuildExistingAccountGenesis(input)
			require.ErrorIs(t, err, ErrInvalidExistingAccountGenesis)
			require.Zero(t, result)
			requireCleared(t, mnemonic)
			require.NotContains(t, err.Error(), "abandon")
		})
	}
}

func TestBuildExistingAccountGenesisRejectsMnemonicAndRandomFailuresWithoutSecretEcho(t *testing.T) {
	tests := []struct {
		name     string
		mnemonic string
		random   io.Reader
	}{
		{name: "bad mnemonic", mnemonic: strings.Repeat("abandon ", 24), random: bytes.NewReader(existingAccountGenesisRandom())},
		{name: "short random read", mnemonic: existingAccountGenesisTestMnemonic, random: bytes.NewReader([]byte{1})},
		{name: "zero random salt", mnemonic: existingAccountGenesisTestMnemonic, random: bytes.NewReader(make([]byte, 16))},
		{name: "short UUID random", mnemonic: existingAccountGenesisTestMnemonic, random: bytes.NewReader(bytes.Repeat([]byte{1}, 16+15))},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mnemonic := []byte(test.mnemonic)
			input := existingAccountGenesisInput(t, mnemonic)
			input.Random = test.random
			result, err := BuildExistingAccountGenesis(input)
			require.Error(t, err)
			require.Zero(t, result)
			requireCleared(t, mnemonic)
			require.NotContains(t, err.Error(), "abandon")
			require.False(t, errors.Is(err, securityerr.ErrUntrustedRoster))
		})
	}
}
