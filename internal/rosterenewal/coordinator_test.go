package rosterenewal

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/identity"
	"github.com/aplexica/aplexica/internal/keys"
	"github.com/aplexica/aplexica/internal/privatefs"
	"github.com/aplexica/aplexica/internal/securityepoch"
	"github.com/fxamacker/cbor/v2"
	"github.com/stretchr/testify/require"
)

type freshnessCollector struct {
	private ed25519.PrivateKey
	keyID   [32]byte
	calls   int
}

func (c *freshnessCollector) CollectFreshnessEndorsements(_ context.Context, previous identity.VerifiedRoster, proposal identity.RosterManifestUnsignedV1) (identity.RosterManifestUnsignedV1, []identity.RosterFreshnessEndorsementV1, error) {
	c.calls++
	endorsement, err := identity.SignFreshnessRenewal(previous, proposal, c.keyID, c.private)
	if err != nil {
		return identity.RosterManifestUnsignedV1{}, nil, err
	}
	return proposal, []identity.RosterFreshnessEndorsementV1{endorsement}, nil
}

type renewalFixture struct {
	root        string
	chain       *identity.ChainStore
	security    *securityepoch.Coordinator
	collector   *freshnessCollector
	issuedAt    time.Time
	expiresAt   time.Time
	coordinator *Coordinator
}

func TestFreshnessCoordinatorRecoversEveryJournalPhaseAndReopensAfterOldExpiry(t *testing.T) {
	crash := errors.New("crash")
	tests := []struct {
		name string
		set  func(*Coordinator)
	}{
		{"journal", func(c *Coordinator) { c.hooks.afterJournal = func() error { return crash } }},
		{"chain", func(c *Coordinator) { c.hooks.afterChain = func() error { return crash } }},
		{"epoch", func(c *Coordinator) { c.hooks.afterEpoch = func() error { return crash } }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newRenewalFixture(t)
			test.set(fixture.coordinator)
			_, err := fixture.coordinator.RunOnce(context.Background())
			require.ErrorIs(t, err, crash)
			journal := filepath.Join(fixture.root, "account", securityepoch.TransitionJournalFilename)
			require.FileExists(t, journal)
			restarted := *fixture.coordinator
			restarted.hooks = crashHooks{}
			restarted.Now = func() time.Time { return fixture.expiresAt.Add(time.Minute) }
			result, err := restarted.RunOnce(context.Background())
			require.NoError(t, err)
			require.True(t, result.Renewed)
			require.NoFileExists(t, journal)
			current, err := fixture.chain.Current(fixture.expiresAt.Add(time.Minute))
			require.NoError(t, err)
			require.Equal(t, result.RosterHash, current.Hash)
			require.Equal(t, uint64(1), current.Manifest.Manifest.AccessGeneration)
		})
	}
}

func TestFreshnessCoordinatorPausesAtExpiryAndPlansCredentialRenewal(t *testing.T) {
	fixture := newRenewalFixture(t)
	fixture.coordinator.Now = func() time.Time { return fixture.expiresAt }
	_, err := fixture.coordinator.RunOnce(context.Background())
	require.ErrorIs(t, err, ErrRosterExpired)
	require.Zero(t, fixture.collector.calls)

	credentialFixture := newRenewalFixture(t)
	credentialFixture.coordinator.Policy.CredentialRenewBefore = 400 * 24 * time.Hour
	result, err := credentialFixture.coordinator.RunOnce(context.Background())
	require.ErrorIs(t, err, ErrCredentialRenewalUnavailable)
	require.False(t, result.NextTry.IsZero())
	require.Zero(t, credentialFixture.collector.calls)
}

func newRenewalFixture(t *testing.T) renewalFixture {
	t.Helper()
	issuedAt := time.Now().UTC().Truncate(time.Second).Add(-7 * time.Hour)
	expiresAt := issuedAt.Add(24 * time.Hour)
	root := t.TempDir()
	canonical, err := cbor.CanonicalEncOptions().EncMode()
	require.NoError(t, err)
	sign := func(private ed25519.PrivateKey, domain string, value any) [64]byte {
		preimage, encodeErr := canonical.Marshal([]any{domain, value})
		require.NoError(t, encodeErr)
		var signature [64]byte
		copy(signature[:], ed25519.Sign(private, preimage))
		return signature
	}
	authorityPublic, authorityPrivate, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	authorityID := sha256.Sum256(authorityPublic)
	var authorityPublicArray [32]byte
	copy(authorityPublicArray[:], authorityPublic)
	wrapPrivate, wrapPublic, err := keys.NewDeviceKey()
	require.NoError(t, err)
	_ = wrapPrivate
	recoveryPublic, recoveryPrivate, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	_, recoveryWrap, err := keys.NewDeviceKey()
	require.NoError(t, err)
	var recoveryRoot [32]byte
	copy(recoveryRoot[:], recoveryPublic)
	scopeID := "0197f30a-3c58-7000-8000-000000000001"
	authority := identity.RosterAuthorityV1{DeviceID: "device-a", SigningKeyID: authorityID, SigningPublicKey: authorityPublicArray}
	anchorUnsigned := identity.AccountTrustAnchorUnsignedV1{Version: 1, ServiceOrigin: "https://api.aplexica.com", AccountID: "account-a", PersonalScopeID: scopeID,
		RecoveryKDFProfileID: identity.RecoveryKDFProfileArgon2idV1, RecoveryRootPublicKey: recoveryRoot, RecoveryWrapKeyID: sha256.Sum256(recoveryWrap[:]), RecoveryWrapPublicKey: recoveryWrap,
		AuthorityEpoch: 1, Authorities: []identity.RosterAuthorityV1{authority}, AuthorityThreshold: 1}
	anchor := identity.AccountTrustAnchorV1{Anchor: anchorUnsigned, RecoverySignature: sign(recoveryPrivate, "aplexica/account-trust-anchor/v1", anchorUnsigned)}
	verifiedAuthority, err := identity.VerifyTrustAnchor(anchor, recoveryPublic)
	require.NoError(t, err)
	credentialUnsigned := identity.DeviceCertificateUnsignedV1{Version: 1, AccountID: "account-a", UserID: "user-a", DeviceID: "device-a", KeyEpoch: 1,
		SigningKeyID: authorityID, SigningPublicKey: authorityPublicArray, WrapKeyID: sha256.Sum256(wrapPublic[:]), WrapPublicKey: wrapPublic,
		EnvelopeVersions: []uint16{2}, NotBeforeUnix: issuedAt.Add(-time.Minute).Unix(), NotAfterUnix: issuedAt.Add(364 * 24 * time.Hour).Unix(),
		IssuanceMode: "genesis", IssuedUnderAuthorityEpoch: 1, IssuingAuthorityStateHash: verifiedAuthority.StateHash}
	credential := identity.DeviceCertificateV1{Certificate: credentialUnsigned, IssuerKeyIDs: [][32]byte{authorityID}, IssuanceSignatures: [][64]byte{sign(authorityPrivate, "aplexica/device-credential/v1", credentialUnsigned)}}
	rosterUnsigned := identity.RosterManifestUnsignedV1{Version: 1, ScopeType: "account", ScopeID: scopeID, Epoch: 1,
		TrustAnchorHash: [32]byte(verifiedAuthority.AnchorHash), AuthorityStateHash: verifiedAuthority.StateHash, AuthorityEpoch: 1,
		AccessGeneration: 1, IssuedAtUnix: issuedAt.Unix(), NotAfterUnix: expiresAt.Unix(), MinEnvelopeVersion: 2, Devices: []identity.DeviceCertificateV1{credential}}
	rosterUnsigned.AccessSetHash, err = identity.AccessSetHash(rosterUnsigned)
	require.NoError(t, err)
	genesis := identity.RosterManifestV1{Manifest: rosterUnsigned, SignerKeyIDs: [][32]byte{authorityID}, Signatures: [][64]byte{sign(authorityPrivate, "aplexica/roster-manifest/v1", rosterUnsigned)}}
	chain := &identity.ChainStore{Path: filepath.Join(root, "account", "chain.cbor")}
	verified, err := chain.Initialize(anchor, recoveryPublic, genesis)
	require.NoError(t, err)
	epoch := epochRecord{Version: 1, ScopeType: "account", ScopeID: scopeID, RosterHash: [32]byte(verified.Hash), AccessGeneration: 1,
		AccessSetHash: rosterUnsigned.AccessSetHash, BarrierID: sha256.Sum256([]byte("barrier")), TreeHeadDigest: sha256.Sum256([]byte("tree")),
		KeyMode: "recipient-wrap-v2", KeyVersion: 0, CoordinatorGeneration: 1}
	security := &securityepoch.Coordinator{Root: root}
	require.NoError(t, security.Transition(context.Background(), "account", securityepoch.SecurityEpoch{CoordinatorGeneration: 1, AccessGeneration: 1,
		AccessSetHash: epoch.AccessSetHash, BarrierID: epoch.BarrierID, KeyMode: epoch.KeyMode, KeyVersion: epoch.KeyVersion}, func() error { return nil }))
	accountRoot, err := privatefs.OpenRoot(filepath.Join(root, "account"), privatefs.DirPolicy{Access: privatefs.AccessPrivate, RepairOwned: true, AllowExisting: true})
	require.NoError(t, err)
	raw, err := json.Marshal(epoch)
	require.NoError(t, err)
	require.NoError(t, accountRoot.WriteFile("security-epoch.json", raw, privatefs.FilePolicy{RejectWritableByOthers: true}))
	require.NoError(t, accountRoot.Close())
	collector := &freshnessCollector{private: authorityPrivate, keyID: authorityID}
	coordinator := &Coordinator{IdentityRoot: root, Chain: chain, Security: security, Collector: collector,
		Policy: Policy{RenewAfter: 6 * time.Hour, RetryInterval: time.Minute, CredentialRenewBefore: 30 * 24 * time.Hour},
		Now:    func() time.Time { return issuedAt.Add(6 * time.Hour) }}
	return renewalFixture{root: root, chain: chain, security: security, collector: collector, issuedAt: issuedAt, expiresAt: expiresAt, coordinator: coordinator}
}
