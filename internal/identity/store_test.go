package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"path/filepath"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/keys"
	"github.com/aplexica/aplexica/internal/securityerr"
	"github.com/stretchr/testify/require"
)

func signedIdentityFixture(t *testing.T) (AccountTrustAnchorV1, ed25519.PublicKey, RosterManifestV1) {
	t.Helper()
	recoveryPub, recoveryPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	authorityPub, authorityPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	_, recoveryWrap, err := keys.NewDeviceKey()
	require.NoError(t, err)
	wrapPriv, wrapPub, err := keys.NewDeviceKey()
	require.NoError(t, err)
	_ = wrapPriv
	devicePub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	copy32 := func(b []byte) (out [32]byte) { copy(out[:], b); return }
	authority := RosterAuthorityV1{DeviceID: "device-authority", SigningKeyID: sha256.Sum256(authorityPub), SigningPublicKey: copy32(authorityPub)}
	au := AccountTrustAnchorUnsignedV1{Version: 1, ServiceOrigin: "https://api.aplexica.com", AccountID: "account-a", PersonalScopeID: "0197f30a-3c58-7000-8000-000000000001", RecoveryKDFProfileID: "argon2id-256m-t3-p1-v1", RecoveryRootPublicKey: copy32(recoveryPub), RecoveryWrapPublicKey: recoveryWrap, RecoveryWrapKeyID: sha256.Sum256(recoveryWrap[:]), AuthorityEpoch: 1, Authorities: []RosterAuthorityV1{authority}, AuthorityThreshold: 1}
	b, err := canonical("aplexica/account-trust-anchor/v1", au)
	require.NoError(t, err)
	anchor := AccountTrustAnchorV1{Anchor: au}
	copy(anchor.RecoverySignature[:], ed25519.Sign(recoveryPriv, b))
	va, err := VerifyTrustAnchor(anchor, recoveryPub)
	require.NoError(t, err)
	now := time.Now().Add(-time.Minute).Unix()
	certU := DeviceCertificateUnsignedV1{Version: 1, AccountID: "account-a", UserID: "user-a", DeviceID: "device-authority", KeyEpoch: 1, SigningKeyID: sha256.Sum256(devicePub), SigningPublicKey: copy32(devicePub), WrapKeyID: sha256.Sum256(wrapPub[:]), WrapPublicKey: wrapPub, EnvelopeVersions: []uint16{2}, NotBeforeUnix: now, NotAfterUnix: now + 86400, IssuanceMode: "recovery", IssuedUnderAuthorityEpoch: 1, IssuingAuthorityStateHash: va.StateHash}
	cb, err := canonical("aplexica/device-credential/v1", certU)
	require.NoError(t, err)
	cert := DeviceCertificateV1{Certificate: certU, IssuerKeyIDs: [][32]byte{authority.SigningKeyID}}
	var sig [64]byte
	copy(sig[:], ed25519.Sign(authorityPriv, cb))
	cert.IssuanceSignatures = [][64]byte{sig}
	m := RosterManifestUnsignedV1{Version: 1, ScopeType: "account", ScopeID: au.PersonalScopeID, Epoch: 1, TrustAnchorHash: [32]byte(va.AnchorHash), AuthorityStateHash: va.StateHash, AuthorityEpoch: 1, AccessGeneration: 1, IssuedAtUnix: now, NotAfterUnix: now + 3600, MinEnvelopeVersion: 2, Devices: []DeviceCertificateV1{cert}}
	m.AccessSetHash, err = AccessSetHash(m)
	require.NoError(t, err)
	mb, err := canonical("aplexica/roster-manifest/v1", m)
	require.NoError(t, err)
	var ms [64]byte
	copy(ms[:], ed25519.Sign(authorityPriv, mb))
	return anchor, recoveryPub, RosterManifestV1{Manifest: m, SignerKeyIDs: [][32]byte{authority.SigningKeyID}, Signatures: [][64]byte{ms}}
}

func TestChainStorePinsAndReloadsVerifiedGenesis(t *testing.T) {
	anchor, recovery, roster := signedIdentityFixture(t)
	s := &ChainStore{Path: filepath.Join(t.TempDir(), "identity", "chain.cbor")}
	v, err := s.Initialize(anchor, recovery, roster)
	require.NoError(t, err)
	got, err := s.Current(time.Now())
	require.NoError(t, err)
	require.Equal(t, v.Hash, got.Hash)
	_, err = s.Current(time.Now().Add(2 * time.Hour))
	require.ErrorIs(t, err, securityerr.ErrStaleRoster)
}

func TestAccountRosterMustUseRecoverySignedPersonalScope(t *testing.T) {
	anchor, recovery, roster := signedIdentityFixture(t)
	authority, err := VerifyTrustAnchor(anchor, recovery)
	require.NoError(t, err)
	authority.Anchor.Anchor.PersonalScopeID = "0197f30a-3c58-7000-8000-000000000099"
	_, err = VerifyGenesis(authority, roster)
	require.Error(t, err)
}
