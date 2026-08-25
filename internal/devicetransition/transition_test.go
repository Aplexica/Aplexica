package devicetransition

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/identity"
	"github.com/aplexica/aplexica/internal/keyrotation"
	"github.com/aplexica/aplexica/internal/keys"
	"github.com/aplexica/aplexica/internal/plugin/proto"
	"github.com/aplexica/aplexica/internal/privatefs"
	"github.com/aplexica/aplexica/internal/securityepoch"
	"github.com/fxamacker/cbor/v2"
	"github.com/stretchr/testify/require"
)

type testCutover struct{ purge, rescan int }

func (h *testCutover) PurgeOldSealingMaterial(context.Context, PlanV1) error { h.purge++; return nil }
func (h *testCutover) RescanCanonical(context.Context, PlanV1) error         { h.rescan++; return nil }

type testBarrier struct {
	states map[string]proto.RemoteSecurityEpochStatusResult
	off    bool
}

func (b *testBarrier) SecurityEpochPrepare(_ context.Context, p proto.RemoteSecurityEpochPrepareParams) (proto.RemoteSecurityEpochStatusResult, error) {
	if b.off {
		return proto.RemoteSecurityEpochStatusResult{}, errors.New("plugin offline")
	}
	if b.states == nil {
		b.states = make(map[string]proto.RemoteSecurityEpochStatusResult)
	}
	state := b.states[p.ScopeID]
	if state.Phase == "" {
		state = proto.RemoteSecurityEpochStatusResult{ScopeID: p.ScopeID, Phase: "prepared", Current: p.Current, Next: p.Next, StagedPackageHash: p.StagedPackageHash}
		b.states[p.ScopeID] = state
	}
	return state, nil
}
func (b *testBarrier) SecurityEpochCommit(_ context.Context, p proto.RemoteSecurityEpochCommandParams) (proto.RemoteSecurityEpochStatusResult, error) {
	if b.off {
		return proto.RemoteSecurityEpochStatusResult{}, errors.New("plugin offline")
	}
	state := b.states[p.ScopeID]
	if state.ScopeID != p.ScopeID || state.Next.BarrierID != p.BarrierID || state.Phase != "prepared" {
		return proto.RemoteSecurityEpochStatusResult{}, ErrTransitionConflict
	}
	state.Phase = "committed"
	b.states[p.ScopeID] = state
	return state, nil
}
func (b *testBarrier) SecurityEpochActivate(_ context.Context, p proto.RemoteSecurityEpochCommandParams) (proto.RemoteSecurityEpochStatusResult, error) {
	if b.off {
		return proto.RemoteSecurityEpochStatusResult{}, errors.New("plugin offline")
	}
	state := b.states[p.ScopeID]
	if state.ScopeID != p.ScopeID || state.Next.BarrierID != p.BarrierID || (state.Phase != "committed" && state.Phase != "active") {
		return proto.RemoteSecurityEpochStatusResult{}, ErrTransitionConflict
	}
	if state.Phase != "active" {
		state.Phase = "active"
		state.Current = state.Next
		b.states[p.ScopeID] = state
	}
	return state, nil
}
func (b *testBarrier) SecurityEpochStatus(_ context.Context, p proto.RemoteSecurityEpochCommandParams) (proto.RemoteSecurityEpochStatusResult, error) {
	if b.off {
		return proto.RemoteSecurityEpochStatusResult{}, errors.New("plugin offline")
	}
	state := b.states[p.ScopeID]
	if state.ScopeID != p.ScopeID || state.Next.BarrierID != p.BarrierID {
		return proto.RemoteSecurityEpochStatusResult{}, ErrTransitionConflict
	}
	return state, nil
}

type transitionFixture struct {
	root, namespace string
	chain           *identity.ChainStore
	keys            *keyrotation.NamespaceKeyStore
	coordinator     *securityepoch.Coordinator
	plan            PlanV1
	survivor        keys.DeviceIdentity
	cutover         *testCutover
	contentKey      [32]byte
	barrier         *testBarrier
}

func TestDeviceRekeyInstallerRecoversEveryCrashBoundary(t *testing.T) {
	crash := errors.New("simulated crash")
	tests := []struct {
		name string
		set  func(*Installer)
	}{
		{"journal", func(i *Installer) { i.hooks.afterJournal = func() error { return crash } }},
		{"plugin-prepare", func(i *Installer) { i.hooks.afterPluginPrepare = func() error { return crash } }},
		{"key", func(i *Installer) { i.hooks.afterKey = func() error { return crash } }},
		{"chain", func(i *Installer) { i.hooks.afterChain = func() error { return crash } }},
		{"rescan", func(i *Installer) { i.hooks.afterRescan = func() error { return crash } }},
		{"epoch", func(i *Installer) { i.hooks.afterEpoch = func() error { return crash } }},
		{"plugin-commit", func(i *Installer) { i.hooks.afterPluginCommit = func() error { return crash } }},
		{"plugin-activate", func(i *Installer) { i.hooks.afterPluginActivate = func() error { return crash } }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newTransitionFixture(t)
			installer := fixture.installer()
			test.set(installer)
			require.ErrorIs(t, installer.Install(context.Background(), fixture.plan), crash)
			journalPath := filepath.Join(fixture.root, "namespaces", fixture.namespace, securityepoch.TransitionJournalFilename)
			require.FileExists(t, journalPath)
			restarted := fixture.installer()
			found, err := restarted.Recover(context.Background())
			require.NoError(t, err)
			require.True(t, found)
			require.NoFileExists(t, journalPath)
			head, err := fixture.chain.Head()
			require.NoError(t, err)
			require.Equal(t, fixture.plan.SecurityEpoch.RosterHash, [32]byte(head.Hash))
			current, err := fixture.keys.Current(context.Background(), fixture.namespace)
			require.NoError(t, err)
			require.Equal(t, fixture.plan.SecurityEpoch.KeyVersion, current.Version)
			require.Equal(t, fixture.plan.SecurityEpoch.AccessSetHash, current.AccessSetHash)
			require.GreaterOrEqual(t, fixture.cutover.purge, 1)
			require.GreaterOrEqual(t, fixture.cutover.rescan, 1)
		})
	}
}

func TestDeviceRekeyPlanIsCiphertextOnlyAndTamperFailsClosed(t *testing.T) {
	fixture := newTransitionFixture(t)
	raw, err := json.Marshal(fixture.plan)
	require.NoError(t, err)
	require.NotContains(t, raw, fixture.contentKey[:])
	require.Len(t, fixture.plan.KeyManifest.Manifest.Wrapped, 3) // two devices + recovery
	installer := fixture.installer()
	installer.hooks.afterJournal = func() error { return errors.New("stop") }
	require.Error(t, installer.Install(context.Background(), fixture.plan))
	path := filepath.Join(fixture.root, "namespaces", fixture.namespace, securityepoch.TransitionJournalFilename)
	journal, err := os.ReadFile(path)
	require.NoError(t, err)
	journal[len(journal)/2] ^= 1
	require.NoError(t, os.WriteFile(path, journal, 0o600))
	_, err = fixture.installer().Recover(context.Background())
	require.Error(t, err)
	_, err = fixture.coordinator.AcquirePublish(context.Background(), fixture.namespace, securityepoch.SecurityEpoch{})
	require.Error(t, err)
}

func TestDeviceRekeyOuterPlanSignatureRejectsRelayRewriteWithFreshChecksum(t *testing.T) {
	fixture := newTransitionFixture(t)
	tampered := fixture.plan
	tampered.SecurityEpoch.TreeHeadDigest = sha256.Sum256([]byte("relay-rewritten-tree-head"))
	var err error
	tampered.Checksum, err = planChecksum(tampered)
	require.NoError(t, err)
	_, err = EncodePlan(tampered)
	require.NoError(t, err, "transport checksum alone cannot be the authorization boundary")

	err = fixture.installer().Install(context.Background(), tampered)
	require.ErrorIs(t, err, ErrTransitionConflict)
	require.NoFileExists(t, filepath.Join(fixture.root, "namespaces", fixture.namespace, securityepoch.TransitionJournalFilename))
	lease, err := fixture.coordinator.AcquirePublish(context.Background(), fixture.namespace, coordinatorEpoch(fixture.plan.CurrentSecurityEpoch))
	require.NoError(t, err, "rejected remote bytes must not poison the publication gate")
	require.NoError(t, lease.Close())
}

func TestValidatePendingAuthenticatesOuterSignatureBeforePluginStartup(t *testing.T) {
	fixture := newTransitionFixture(t)
	installer := fixture.installer()
	installer.hooks.afterJournal = func() error { return errors.New("stop") }
	require.Error(t, installer.Install(context.Background(), fixture.plan))
	found, err := ValidatePending(fixture.root)
	require.NoError(t, err)
	require.True(t, found)

	scopeRoot, err := privatefs.OpenRoot(filepath.Join(fixture.root, "namespaces", fixture.namespace), privatefs.DirPolicy{Access: privatefs.AccessPrivate, RepairOwned: true})
	require.NoError(t, err)
	state, err := readJournal(scopeRoot)
	require.NoError(t, err)
	state.Plan.SecurityEpoch.TreeHeadDigest = sha256.Sum256([]byte("startup-relay-rewrite"))
	state.Plan.Checksum, err = planChecksum(state.Plan)
	require.NoError(t, err)
	journal, err := encodeJournal(state)
	require.NoError(t, err)
	require.NoError(t, scopeRoot.WriteFile(securityepoch.TransitionJournalFilename, journal, privatefs.FilePolicy{RejectWritableByOthers: true}))
	require.NoError(t, scopeRoot.Close())

	_, err = ValidatePending(fixture.root)
	require.ErrorIs(t, err, ErrTransitionConflict)
}

func TestDeviceRekeyOfflineJournalBlocksPublishAndRecoversWhenPluginReturns(t *testing.T) {
	fixture := newTransitionFixture(t)
	fixture.barrier.off = true
	err := fixture.installer().Install(context.Background(), fixture.plan)
	require.ErrorContains(t, err, "plugin offline")
	journalPath := filepath.Join(fixture.root, "namespaces", fixture.namespace, securityepoch.TransitionJournalFilename)
	require.FileExists(t, journalPath)
	_, err = fixture.coordinator.AcquirePublish(context.Background(), fixture.namespace, coordinatorEpoch(fixture.plan.CurrentSecurityEpoch))
	require.Error(t, err, "a pending transition must block old-generation publication")

	fixture.barrier.off = false
	found, err := fixture.installer().Recover(context.Background())
	require.NoError(t, err)
	require.True(t, found)
	require.NoFileExists(t, journalPath)
}

func TestLocallyProducedPlanCannotCutOverBeforeDurableDistributionReceipt(t *testing.T) {
	fixture := newTransitionFixture(t)
	installer := fixture.installer()
	require.NoError(t, installer.StageForDistribution(context.Background(), fixture.plan))
	pending, err := installer.PendingDistribution()
	require.NoError(t, err)
	require.Len(t, pending, 1)
	require.Equal(t, fixture.plan.Checksum, pending[0].Checksum)
	require.Empty(t, fixture.barrier.states, "staging for relay must not prepare the plugin")

	_, err = installer.Recover(context.Background())
	require.ErrorIs(t, err, ErrTransitionConflict, "restart recovery must wait for an exact relay receipt")
	require.Empty(t, fixture.barrier.states)
	require.NoError(t, installer.MarkDistributed(fixture.plan))
	found, err := installer.Recover(context.Background())
	require.NoError(t, err)
	require.True(t, found)
	require.NoFileExists(t, filepath.Join(fixture.root, "namespaces", fixture.namespace, securityepoch.TransitionJournalFilename))
}

func TestDeviceRekeyRecoveryResolvesEachNamespaceChainIndependently(t *testing.T) {
	root := t.TempDir()
	survivor := testDeviceIdentity(t)
	first := newTransitionFixtureAt(t, root, "0197f30a-3c58-7000-8000-000000000001", survivor)
	second := newTransitionFixtureAt(t, root, "0197f30a-3c58-7000-8000-000000000002", survivor)
	barrier := &testBarrier{}
	cutover := &testCutover{}
	first.barrier, second.barrier = barrier, barrier
	first.cutover, second.cutover = cutover, cutover

	for _, fixture := range []*transitionFixture{&first, &second} {
		installer := fixture.installer()
		installer.Chain = nil // production path: resolve chain from journal scope
		installer.hooks.afterJournal = func() error { return errors.New("stop before plugin") }
		require.Error(t, installer.Install(context.Background(), fixture.plan))
	}
	recovery := first.installer()
	recovery.Chain = nil
	found, err := recovery.Recover(context.Background())
	require.NoError(t, err)
	require.True(t, found)
	for _, fixture := range []transitionFixture{first, second} {
		head, headErr := fixture.chain.Head()
		require.NoError(t, headErr)
		require.Equal(t, fixture.plan.SecurityEpoch.RosterHash, [32]byte(head.Hash))
		require.NoFileExists(t, filepath.Join(root, "namespaces", fixture.namespace, securityepoch.TransitionJournalFilename))
	}
}

func (f transitionFixture) installer() *Installer {
	return &Installer{IdentityRoot: f.root, Chain: f.chain, Keys: f.keys, Coordinator: f.coordinator, RecipientPrivate: f.survivor.WrapPrivate, RecipientType: "device", RecipientID: "device-a", Barrier: f.barrier, Cutover: f.cutover}
}

func newTransitionFixture(t *testing.T) transitionFixture {
	t.Helper()
	return newTransitionFixtureAt(t, t.TempDir(), "0197f30a-3c58-7000-8000-000000000001", keys.DeviceIdentity{})
}

func newTransitionFixtureAt(t *testing.T, root, namespace string, survivor keys.DeviceIdentity) transitionFixture {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	canonical, err := cbor.CanonicalEncOptions().EncMode()
	require.NoError(t, err)
	sign := func(private ed25519.PrivateKey, domain string, value any) [64]byte {
		preimage, encodeErr := canonical.Marshal([]any{domain, value})
		require.NoError(t, encodeErr)
		var signature [64]byte
		copy(signature[:], ed25519.Sign(private, preimage))
		return signature
	}
	recoveryPublic, recoveryPrivate, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	recoveryWrapPrivate, recoveryWrapPublic, err := keys.NewDeviceKey()
	require.NoError(t, err)
	_ = recoveryWrapPrivate
	if len(survivor.SigningPrivate) == 0 {
		survivor = testDeviceIdentity(t)
	}
	var survivorPublic [32]byte
	copy(survivorPublic[:], survivor.SigningPublic)
	authority := identity.RosterAuthorityV1{DeviceID: "device-a", SigningKeyID: survivor.SigningKeyID, SigningPublicKey: survivorPublic}
	var recoveryRoot [32]byte
	copy(recoveryRoot[:], recoveryPublic)
	anchorUnsigned := identity.AccountTrustAnchorUnsignedV1{Version: 1, ServiceOrigin: "https://api.aplexica.com", AccountID: "account-a", PersonalScopeID: namespace,
		RecoveryKDFProfileID: identity.RecoveryKDFProfileArgon2idV1, RecoveryRootPublicKey: recoveryRoot,
		RecoveryWrapKeyID: sha256.Sum256(recoveryWrapPublic[:]), RecoveryWrapPublicKey: recoveryWrapPublic,
		AuthorityEpoch: 1, Authorities: []identity.RosterAuthorityV1{authority}, AuthorityThreshold: 1}
	anchor := identity.AccountTrustAnchorV1{Anchor: anchorUnsigned, RecoverySignature: sign(recoveryPrivate, "aplexica/account-trust-anchor/v1", anchorUnsigned)}
	verifiedAuthority, err := identity.VerifyTrustAnchor(anchor, recoveryPublic)
	require.NoError(t, err)
	credentialUnsigned := identity.DeviceCertificateUnsignedV1{Version: 1, AccountID: "account-a", UserID: "user-a", DeviceID: "device-a", KeyEpoch: 1,
		SigningKeyID: survivor.SigningKeyID, SigningPublicKey: survivorPublic, WrapKeyID: survivor.WrapKeyID, WrapPublicKey: survivor.WrapPublic,
		EnvelopeVersions: []uint16{2}, NotBeforeUnix: now.Add(-time.Minute).Unix(), NotAfterUnix: now.Add(364 * 24 * time.Hour).Unix(),
		IssuanceMode: "genesis", IssuedUnderAuthorityEpoch: 1, IssuingAuthorityStateHash: verifiedAuthority.StateHash}
	credential := identity.DeviceCertificateV1{Certificate: credentialUnsigned, IssuerKeyIDs: [][32]byte{survivor.SigningKeyID}, IssuanceSignatures: [][64]byte{sign(survivor.SigningPrivate, "aplexica/device-credential/v1", credentialUnsigned)}}
	rosterUnsigned := identity.RosterManifestUnsignedV1{Version: 1, ScopeType: "namespace", ScopeID: namespace, Epoch: 1,
		TrustAnchorHash: [32]byte(verifiedAuthority.AnchorHash), AuthorityStateHash: verifiedAuthority.StateHash, AuthorityEpoch: 1,
		AccessGeneration: 1, IssuedAtUnix: now.Unix(), NotAfterUnix: now.Add(24 * time.Hour).Unix(), MinEnvelopeVersion: 2, Devices: []identity.DeviceCertificateV1{credential}}
	rosterUnsigned.AccessSetHash, err = identity.AccessSetHash(rosterUnsigned)
	require.NoError(t, err)
	genesis := identity.RosterManifestV1{Manifest: rosterUnsigned, SignerKeyIDs: [][32]byte{survivor.SigningKeyID}, Signatures: [][64]byte{sign(survivor.SigningPrivate, "aplexica/roster-manifest/v1", rosterUnsigned)}}
	chain := &identity.ChainStore{Path: filepath.Join(root, "namespaces", namespace, "chain.cbor")}
	previous, err := chain.Initialize(anchor, recoveryPublic, genesis)
	require.NoError(t, err)

	candidate := testDeviceIdentity(t)
	var candidatePublic [32]byte
	copy(candidatePublic[:], candidate.SigningPublic)
	transcript := identity.PairingTranscriptV1{Version: 1, ServiceOrigin: anchorUnsigned.ServiceOrigin, AccountID: anchorUnsigned.AccountID, PendingID: "pending-b",
		PairingNonce: sha256.Sum256([]byte("nonce")), CandidateDeviceID: "device-b", CandidateEphemeralPublic: sha256.Sum256([]byte("candidate-eph")),
		CandidateSigningPublic: candidatePublic, CandidateWrapPublic: candidate.WrapPublic, CandidateEnvelopeVersions: []uint16{2}, ApproverDeviceID: "device-a",
		ApproverEphemeralPublic: sha256.Sum256([]byte("approver-eph")), TrustAnchorHash: [32]byte(previous.Authority.AnchorHash), CurrentRosterHash: [32]byte(previous.Hash)}
	credentialB, err := identity.PreparePairingCredential(previous, identity.PairingCredentialProposalV1{Transcript: transcript, UserID: "user-a", KeyEpoch: 1,
		JoinNonce: sha256.Sum256([]byte("join-b")), IssuedAt: now.Add(time.Second), NotAfter: now.Add(365 * 24 * time.Hour)})
	require.NoError(t, err)
	credentialB.CandidateProof, err = identity.SignDevicePossession(credentialB, candidate.SigningPrivate)
	require.NoError(t, err)
	credentialB.ApproverProof, err = identity.SignPairingApproval(previous, credentialB, transcript, survivor.SigningPrivate)
	require.NoError(t, err)
	credentialEndorsement, err := identity.EndorseDeviceCredential(previous, credentialB, survivor.SigningKeyID, survivor.SigningPrivate)
	require.NoError(t, err)
	signedB, err := identity.FinalizeDeviceCredential(previous, credentialB, []identity.DeviceTransitionEndorsementV1{credentialEndorsement})
	require.NoError(t, err)
	nextUnsigned, err := identity.PrepareDeviceRosterTransition(previous, signedB, now.Add(2*time.Second))
	require.NoError(t, err)
	rosterEndorsement, err := identity.EndorseDeviceRosterTransition(previous, nextUnsigned, survivor.SigningKeyID, survivor.SigningPrivate)
	require.NoError(t, err)
	nextRoster, next, err := identity.FinalizeDeviceRosterTransition(previous, nextUnsigned, []identity.DeviceTransitionEndorsementV1{rosterEndorsement})
	require.NoError(t, err)
	_ = nextRoster

	currentKey := keyrotation.NamespaceKeySnapshot{NamespaceID: namespace, Version: 1, Key: sha256.Sum256([]byte("old-key")), AccessGeneration: previous.Manifest.Manifest.AccessGeneration,
		AccessSetHash: previous.Manifest.Manifest.AccessSetHash, IssuedRosterEpoch: previous.Manifest.Manifest.Epoch, IssuedRosterHash: [32]byte(previous.Hash), Finalized: true}
	nonce := sha256.Sum256([]byte("rotation-nonce"))
	statementUnsigned, err := keyrotation.PrepareRotationStatement(previous, next, currentKey, now.Add(3*time.Second), now.Add(10*time.Minute), nonce)
	require.NoError(t, err)
	rotationEndorsement, err := keyrotation.EndorseRotationStatement(previous, statementUnsigned, survivor.SigningKeyID, survivor.SigningPrivate)
	require.NoError(t, err)
	statement, err := keyrotation.FinalizeRotationStatement(previous, next, statementUnsigned, []keyrotation.RotationEndorsementV1{rotationEndorsement}, now.Add(3*time.Second))
	require.NoError(t, err)
	randomBytes := make([]byte, 32+(32+24)*3)
	for index := range randomBytes {
		randomBytes[index] = byte(index + 1)
	}
	var contentKey [32]byte
	copy(contentKey[:], randomBytes[:32])
	keyManifest, err := keyrotation.BuildNamespaceKeyManifest(previous, next, statement, "device-a", survivor.SigningPrivate, bytes.NewReader(randomBytes), now.Add(3*time.Second))
	require.NoError(t, err)
	currentEpoch := SecurityEpochRecordV1{Version: 1, ScopeType: "namespace", ScopeID: namespace, RosterHash: [32]byte(previous.Hash), AccessGeneration: previous.Manifest.Manifest.AccessGeneration,
		AccessSetHash: previous.Manifest.Manifest.AccessSetHash, BarrierID: sha256.Sum256([]byte("old-barrier")), TreeHeadDigest: sha256.Sum256([]byte("old-tree")),
		KeyMode: "namespace-key-v1", KeyVersion: 1, CoordinatorGeneration: 1}
	proposal, err := BuildPlan(previous, next, currentEpoch, statement, keyManifest, now.Add(3*time.Second), sha256.Sum256([]byte("new-tree")))
	require.NoError(t, err)
	planEndorsement, err := EndorsePlan(proposal, survivor.SigningKeyID, survivor.SigningPrivate)
	require.NoError(t, err)
	plan, err := FinalizePlan(previous, proposal, []PlanEndorsementV1{planEndorsement})
	require.NoError(t, err)
	coordinator := &securityepoch.Coordinator{Root: root}
	require.NoError(t, coordinator.Transition(context.Background(), namespace, securityepoch.SecurityEpoch{CoordinatorGeneration: 1, AccessGeneration: currentEpoch.AccessGeneration,
		AccessSetHash: currentEpoch.AccessSetHash, BarrierID: currentEpoch.BarrierID, KeyMode: currentEpoch.KeyMode, KeyVersion: currentEpoch.KeyVersion}, func() error { return nil }))
	scopeRoot, err := privatefs.OpenRoot(filepath.Join(root, "namespaces", namespace), privatefs.DirPolicy{Access: privatefs.AccessPrivate, RepairOwned: true, AllowExisting: true})
	require.NoError(t, err)
	epochBytes, err := json.Marshal(currentEpoch)
	require.NoError(t, err)
	require.NoError(t, scopeRoot.WriteFile(securityEpochFile, epochBytes, privatefs.FilePolicy{RejectWritableByOthers: true}))
	require.NoError(t, scopeRoot.Close())
	return transitionFixture{root: root, namespace: namespace, chain: chain, keys: &keyrotation.NamespaceKeyStore{Root: filepath.Join(root, "namespace-keys")},
		coordinator: coordinator, plan: plan, survivor: survivor, cutover: &testCutover{}, contentKey: contentKey, barrier: &testBarrier{}}
}

func testDeviceIdentity(t *testing.T) keys.DeviceIdentity {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	wrapPrivate, wrapPublic, err := keys.NewDeviceKey()
	require.NoError(t, err)
	return keys.DeviceIdentity{SigningPrivate: private, SigningPublic: public, SigningKeyID: sha256.Sum256(public), WrapPrivate: wrapPrivate, WrapPublic: wrapPublic, WrapKeyID: sha256.Sum256(wrapPublic[:])}
}
