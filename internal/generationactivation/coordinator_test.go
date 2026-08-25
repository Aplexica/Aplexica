package generationactivation

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/identity"
	"github.com/aplexica/aplexica/internal/keys"
	"github.com/aplexica/aplexica/internal/privatefs"
	"github.com/stretchr/testify/require"
)

type fixedIdentitySource struct{ identity keys.DeviceIdentity }

func (s fixedIdentitySource) LoadExisting() (keys.DeviceIdentity, error) { return s.identity, nil }

type memoryStateStore struct {
	state    durableState
	exists   bool
	saves    int
	failSave int
}

type removeFailJournal struct {
	failRemoves int
	removes     int
}

func (j *removeFailJournal) Load([32]byte) (GenerationActivationUnsignedV1, []ActivationEndorsementV1, error) {
	return GenerationActivationUnsignedV1{}, nil, os.ErrNotExist
}

func (j *removeFailJournal) Save([32]byte, GenerationActivationUnsignedV1, []ActivationEndorsementV1) error {
	return nil
}

func (j *removeFailJournal) Remove([32]byte) error {
	j.removes++
	if j.failRemoves > 0 {
		j.failRemoves--
		return errors.New("injected endorsement cleanup failure")
	}
	return nil
}

func (s *memoryStateStore) Load() (durableState, error) {
	if !s.exists {
		return durableState{}, os.ErrNotExist
	}
	return s.state, nil
}
func (s *memoryStateStore) Save(state durableState) error {
	s.saves++
	if s.failSave == s.saves {
		return errors.New("injected save crash")
	}
	s.state, s.exists = state, true
	return nil
}

type recordingTransport struct {
	objects       []SignedObject
	registrations []CredentialRegistration
	activations   [][]byte
	failActivate  bool
	failAtomic    int
	receipt       ActivationReceipt
	status        ActivationStatus
	statusErr     error
}

func (t *recordingTransport) SubmitTrustAnchor(_ context.Context, object SignedObject) error {
	t.objects = append(t.objects, object)
	return nil
}
func (t *recordingTransport) SubmitAuthorityTransition(_ context.Context, object SignedObject) error {
	t.objects = append(t.objects, object)
	return nil
}
func (t *recordingTransport) SubmitRosterTransition(_ context.Context, object SignedObject) error {
	t.objects = append(t.objects, object)
	return nil
}
func (t *recordingTransport) SubmitAtomicAuthorityRosterTransition(_ context.Context, object SignedObject) error {
	t.objects = append(t.objects, object)
	if t.failAtomic > 0 {
		t.failAtomic--
		return errors.New("atomic publication temporarily unavailable")
	}
	return nil
}
func (t *recordingTransport) RegisterDeviceCredential(_ context.Context, registration CredentialRegistration) error {
	t.registrations = append(t.registrations, registration)
	return nil
}
func (t *recordingTransport) ActivateGeneration(_ context.Context, blob []byte) (ActivationReceipt, error) {
	t.activations = append(t.activations, append([]byte(nil), blob...))
	if t.failActivate {
		return ActivationReceipt{}, errors.New("method unavailable")
	}
	return t.receipt, nil
}
func (t *recordingTransport) GetActivationStatus(_ context.Context, _ []byte) (ActivationStatus, error) {
	return t.status, t.statusErr
}

type activationFixture struct {
	chain    *identity.ChainStore
	device   keys.DeviceIdentity
	epoch    SecurityEpochState
	now      time.Time
	account  string
	scopeID  string
	deviceID string
}

func newActivationFixture(t *testing.T) activationFixture {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	recoveryPublic, recoveryPrivate, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	authorityPublic, authorityPrivate, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	authorityID := sha256.Sum256(authorityPublic)
	wrapPrivate, wrapPublic, err := keys.NewDeviceKey()
	require.NoError(t, err)
	device := keys.DeviceIdentity{WrapPrivate: wrapPrivate, WrapPublic: wrapPublic, WrapKeyID: sha256.Sum256(wrapPublic[:]), SigningPrivate: authorityPrivate, SigningPublic: authorityPublic, SigningKeyID: authorityID}
	var recoveryRoot [32]byte
	copy(recoveryRoot[:], recoveryPublic)
	var authorityKey [32]byte
	copy(authorityKey[:], authorityPublic)
	accountID := "account-a"
	scopeID := "0197f30a-3c58-7000-8000-000000000001"
	deviceID := "device-a"
	anchorUnsigned := identity.AccountTrustAnchorUnsignedV1{
		Version: 1, ServiceOrigin: "https://api.aplexica.com", AccountID: accountID, PersonalScopeID: scopeID,
		RecoveryKDFProfileID: identity.RecoveryKDFProfileArgon2idV1, RecoveryRootPublicKey: recoveryRoot,
		RecoveryWrapPublicKey: sha256.Sum256([]byte("recovery-wrap")), AuthorityEpoch: 1,
		Authorities: []identity.RosterAuthorityV1{{DeviceID: deviceID, SigningKeyID: authorityID, SigningPublicKey: authorityKey}}, AuthorityThreshold: 1,
	}
	anchorUnsigned.RecoveryWrapKeyID = sha256.Sum256(anchorUnsigned.RecoveryWrapPublicKey[:])
	anchorPreimage, err := canonicalEncoding.Marshal([]any{"aplexica/account-trust-anchor/v1", anchorUnsigned})
	require.NoError(t, err)
	anchor := identity.AccountTrustAnchorV1{Anchor: anchorUnsigned}
	copy(anchor.RecoverySignature[:], ed25519.Sign(recoveryPrivate, anchorPreimage))
	verifiedAuthority, err := identity.VerifyTrustAnchor(anchor, recoveryPublic)
	require.NoError(t, err)

	certificateUnsigned := identity.DeviceCertificateUnsignedV1{
		Version: 1, AccountID: accountID, UserID: "user-a", DeviceID: deviceID, KeyEpoch: 1,
		SigningKeyID: authorityID, SigningPublicKey: authorityKey, WrapKeyID: device.WrapKeyID, WrapPublicKey: device.WrapPublic,
		EnvelopeVersions: []uint16{2}, NotBeforeUnix: now.Add(-time.Minute).Unix(), NotAfterUnix: now.Add(time.Hour).Unix(),
		IssuanceMode: "genesis", IssuedUnderAuthorityEpoch: 1, IssuingAuthorityStateHash: verifiedAuthority.StateHash,
	}
	certificatePreimage, err := canonicalEncoding.Marshal([]any{"aplexica/device-credential/v1", certificateUnsigned})
	require.NoError(t, err)
	certificate := identity.DeviceCertificateV1{Certificate: certificateUnsigned, IssuerKeyIDs: [][32]byte{authorityID}, IssuanceSignatures: make([][64]byte, 1)}
	copy(certificate.IssuanceSignatures[0][:], ed25519.Sign(authorityPrivate, certificatePreimage))
	manifestUnsigned := identity.RosterManifestUnsignedV1{
		Version: 1, ScopeType: "account", ScopeID: scopeID, Epoch: 1,
		TrustAnchorHash: [32]byte(verifiedAuthority.AnchorHash), AuthorityStateHash: verifiedAuthority.StateHash, AuthorityEpoch: 1,
		AccessGeneration: 1, IssuedAtUnix: now.Unix(), NotAfterUnix: now.Add(time.Hour).Unix(), MinEnvelopeVersion: 2,
		Devices: []identity.DeviceCertificateV1{certificate},
	}
	manifestUnsigned.AccessSetHash, err = identity.AccessSetHash(manifestUnsigned)
	require.NoError(t, err)
	manifestPreimage, err := canonicalEncoding.Marshal([]any{"aplexica/roster-manifest/v1", manifestUnsigned})
	require.NoError(t, err)
	manifest := identity.RosterManifestV1{Manifest: manifestUnsigned, SignerKeyIDs: [][32]byte{authorityID}, Signatures: make([][64]byte, 1)}
	copy(manifest.Signatures[0][:], ed25519.Sign(authorityPrivate, manifestPreimage))
	chain := &identity.ChainStore{Path: filepath.Join(t.TempDir(), "chain.cbor")}
	verified, err := chain.Initialize(anchor, recoveryPublic, manifest)
	require.NoError(t, err)
	epoch := SecurityEpochState{
		Version: 1, ScopeType: "account", ScopeID: scopeID, RosterHash: [32]byte(verified.Hash),
		AccessGeneration: manifestUnsigned.AccessGeneration, AccessSetHash: manifestUnsigned.AccessSetHash,
		BarrierID: sha256.Sum256([]byte("barrier")), TreeHeadDigest: sha256.Sum256([]byte("tree-head")),
		KeyMode: "recipient-wrap-v2", CoordinatorGeneration: 1,
	}
	return activationFixture{chain: chain, device: device, epoch: epoch, now: now, account: accountID, scopeID: scopeID, deviceID: deviceID}
}

func (f activationFixture) coordinator(state StateStore, transport Transport) *Coordinator {
	return &Coordinator{
		Chain: f.chain, Epoch: f.epoch, StreamEpoch: "stream-epoch-a", DeviceID: f.deviceID,
		Identity: fixedIdentitySource{identity: f.device}, State: state, Transport: transport,
		Now: func() time.Time { return f.now },
	}
}

func TestBuildProducesCanonicalAuthoritySignature(t *testing.T) {
	fixture := newActivationFixture(t)
	snapshot, err := fixture.chain.PublicationSnapshot(fixture.now)
	require.NoError(t, err)
	previous := sha256.Sum256([]byte("previous-authority"))
	signed, blob, binding, err := Build(BuildInput{
		AccountID: fixture.account, StreamEpoch: "stream-epoch-a", Roster: snapshot.Current,
		SecurityEpoch: fixture.epoch, DeviceID: fixture.deviceID, DeviceIdentity: fixture.device,
		PreviousAuthorityDigest: previous, Now: fixture.now, Random: bytesReader(1),
	})
	require.NoError(t, err)
	require.NotZero(t, binding)
	decoded, err := DecodeCanonical(blob)
	require.NoError(t, err)
	require.Equal(t, signed, decoded)
	require.Equal(t, fixture.scopeID, decoded.Attestation.RosterScopeID)
	require.Equal(t, previous, decoded.Attestation.PreviousAuthorityDigest)
	preimage, err := canonicalEncoding.Marshal([]any{attestationDomain, decoded.Attestation})
	require.NoError(t, err)
	require.True(t, ed25519.Verify(fixture.device.SigningPublic, preimage, decoded.Signatures[0][:]))
}

type repeatedReader byte

func bytesReader(value byte) repeatedReader { return repeatedReader(value) }
func (r repeatedReader) Read(target []byte) (int, error) {
	for i := range target {
		target[i] = byte(r)
	}
	return len(target), nil
}

func TestCoordinatorPersistsPendingRetriesExactBytesAndBecomesIdempotent(t *testing.T) {
	fixture := newActivationFixture(t)
	state := &memoryStateStore{}
	transport := &recordingTransport{failActivate: true, receipt: ActivationReceipt{AuthorityDigest: sha256Hex("authority-one"), Revision: 1}}
	_, err := fixture.coordinator(state, transport).RunOnce(context.Background())
	require.Error(t, err)
	require.True(t, state.exists)
	require.NotNil(t, state.state.Pending)
	require.Len(t, transport.activations, 1)
	first := append([]byte(nil), transport.activations[0]...)

	transport.failActivate = false
	result, err := fixture.coordinator(state, transport).RunOnce(context.Background())
	require.NoError(t, err)
	require.False(t, result.AlreadyActivated)
	require.Len(t, transport.activations, 2)
	require.Equal(t, first, transport.activations[1])
	require.Nil(t, state.state.Pending)
	require.Equal(t, uint64(1), state.state.AuthorityRevision)

	result, err = fixture.coordinator(state, transport).RunOnce(context.Background())
	require.NoError(t, err)
	require.True(t, result.AlreadyActivated)
	require.Len(t, transport.activations, 2)
}

func TestCoordinatorBindsNextGenerationToDurablePriorAuthorityReceipt(t *testing.T) {
	fixture := newActivationFixture(t)
	state := &memoryStateStore{}
	firstDigest := sha256Hex("authority-one")
	transport := &recordingTransport{receipt: ActivationReceipt{AuthorityDigest: firstDigest, Revision: 1}}
	_, err := fixture.coordinator(state, transport).RunOnce(context.Background())
	require.NoError(t, err)

	next := fixture.coordinator(state, transport)
	next.Epoch.CoordinatorGeneration = 2
	next.Epoch.BarrierID = sha256.Sum256([]byte("barrier-two"))
	transport.receipt = ActivationReceipt{AuthorityDigest: sha256Hex("authority-two"), Revision: 2}
	_, err = next.RunOnce(context.Background())
	require.NoError(t, err)
	require.Len(t, transport.activations, 2)
	second, err := DecodeCanonical(transport.activations[1])
	require.NoError(t, err)
	firstRaw, err := hex.DecodeString(firstDigest)
	require.NoError(t, err)
	var expected [32]byte
	copy(expected[:], firstRaw)
	require.Equal(t, expected, second.Attestation.PreviousAuthorityDigest)
}

func TestCoordinatorCrashAfterServerCommitRetainsExactPending(t *testing.T) {
	fixture := newActivationFixture(t)
	state := &memoryStateStore{failSave: 3}
	transport := &recordingTransport{receipt: ActivationReceipt{AuthorityDigest: sha256Hex("authority-one"), Revision: 1}}
	_, err := fixture.coordinator(state, transport).RunOnce(context.Background())
	require.ErrorContains(t, err, "persist receipt")
	require.NotNil(t, state.state.Pending)
	require.Len(t, transport.activations, 1)
	committed := append([]byte(nil), transport.activations[0]...)

	result, err := fixture.coordinator(state, transport).RunOnce(context.Background())
	require.NoError(t, err)
	require.Equal(t, uint64(1), result.Receipt.Revision)
	require.Len(t, transport.activations, 2)
	require.Equal(t, committed, transport.activations[1])
	require.Nil(t, state.state.Pending)
}

func TestCoordinatorPendingRetrySurvivesExpiredRosterAndMissingPrivateKey(t *testing.T) {
	fixture := newActivationFixture(t)
	state := &memoryStateStore{}
	transport := &recordingTransport{failActivate: true, receipt: ActivationReceipt{AuthorityDigest: sha256Hex("authority-one"), Revision: 1, Duplicate: true}}
	_, err := fixture.coordinator(state, transport).RunOnce(context.Background())
	require.Error(t, err)
	require.NotNil(t, state.state.Pending)

	retry := fixture.coordinator(state, transport)
	retry.Now = func() time.Time { return fixture.now.Add(48 * time.Hour) }
	retry.Identity = failingIdentitySource{}
	transport.failActivate = false
	transport.status = ActivationStatus{Committed: true, Receipt: transport.receipt}
	result, err := retry.RunOnce(context.Background())
	require.NoError(t, err)
	require.True(t, result.Receipt.Duplicate)
	require.Len(t, transport.activations, 1, "expired recovery must query durable status, not replay a stale statement")
}

func TestCoordinatorReplacesExpiredAbsentPendingWithoutOpeningGate(t *testing.T) {
	fixture := newActivationFixture(t)
	state := &memoryStateStore{}
	transport := &recordingTransport{failActivate: true}
	_, err := fixture.coordinator(state, transport).RunOnce(context.Background())
	require.Error(t, err)
	require.NotNil(t, state.state.Pending)
	first := append([]byte(nil), state.state.Pending.AttestationBlob...)

	retry := fixture.coordinator(state, transport)
	retry.Now = func() time.Time { return fixture.now.Add(11 * time.Minute) }
	transport.status = ActivationStatus{Absent: true}
	_, err = retry.RunOnce(context.Background())
	require.Error(t, err)
	require.NotNil(t, state.state.Pending, "the durable gate must never be cleared between stale and fresh statements")
	require.NotEqual(t, first, state.state.Pending.AttestationBlob)
	require.Len(t, transport.activations, 2)
}

func TestCoordinatorExpiredAbsentRecoveryWaitsForEndorsementCleanupAcrossRestart(t *testing.T) {
	fixture := newActivationFixture(t)
	state := &memoryStateStore{}
	transport := &recordingTransport{failActivate: true}
	_, err := fixture.coordinator(state, transport).RunOnce(context.Background())
	require.Error(t, err)
	require.NotNil(t, state.state.Pending)
	first := append([]byte(nil), state.state.Pending.AttestationBlob...)

	journal := &removeFailJournal{failRemoves: 1}
	retry := fixture.coordinator(state, transport)
	retry.Now = func() time.Time { return fixture.now.Add(11 * time.Minute) }
	retry.Endorsement = journal
	transport.status = ActivationStatus{Absent: true}
	result, err := retry.RunOnce(context.Background())
	require.ErrorContains(t, err, "remove endorsement journal")
	require.Equal(t, first, result.AttestationBlob)
	require.NotNil(t, state.state.Pending, "cleanup failure must leave the exact durable traffic gate in place")
	require.Equal(t, first, state.state.Pending.AttestationBlob)
	require.Len(t, transport.activations, 1, "cleanup failure must not publish a replacement")

	// A process restart retries cleanup against the same durable pending state.
	// Once cleanup succeeds, authenticated status=absent permits one atomic
	// pending-state replacement; the gate is never cleared on disk in between.
	restarted := fixture.coordinator(state, transport)
	restarted.Now = retry.Now
	restarted.Endorsement = journal
	_, err = restarted.RunOnce(context.Background())
	require.Error(t, err, "the injected activation outage remains retryable")
	require.NotNil(t, state.state.Pending)
	require.NotEqual(t, first, state.state.Pending.AttestationBlob)
	require.Len(t, transport.activations, 2)
	require.GreaterOrEqual(t, journal.removes, 2)
}

func TestFileStateStoreRestartsWithExactPendingAttestation(t *testing.T) {
	fixture := newActivationFixture(t)
	stateDir := t.TempDir()
	require.NoError(t, os.Chmod(stateDir, 0o700))
	statePath := filepath.Join(stateDir, "generation-activation.json")
	transport := &recordingTransport{failActivate: true, receipt: ActivationReceipt{AuthorityDigest: sha256Hex("authority-one"), Revision: 1, Duplicate: true}}
	firstCoordinator := fixture.coordinator(FileStateStore{Path: statePath}, transport)
	_, err := firstCoordinator.RunOnce(context.Background())
	require.Error(t, err)
	require.Len(t, transport.activations, 1)
	first := append([]byte(nil), transport.activations[0]...)

	transport.failActivate = false
	restarted := fixture.coordinator(FileStateStore{Path: statePath}, transport)
	_, err = restarted.RunOnce(context.Background())
	require.NoError(t, err)
	require.Len(t, transport.activations, 2)
	require.Equal(t, first, transport.activations[1])
	stateRoot, err := privatefs.OpenRoot(stateDir, privatefs.DirPolicy{Access: privatefs.AccessPrivate})
	require.NoError(t, err)
	stateFile, err := stateRoot.OpenReadRegular(filepath.Base(statePath))
	require.NoError(t, err)
	require.NoError(t, stateFile.Close())
	require.NoError(t, stateRoot.Close())
}

type failingIdentitySource struct{}

func (failingIdentitySource) LoadExisting() (keys.DeviceIdentity, error) {
	return keys.DeviceIdentity{}, errors.New("private key unavailable")
}

func TestCoordinatorResolvesPendingBeforeConsideringNewGeneration(t *testing.T) {
	fixture := newActivationFixture(t)
	state := &memoryStateStore{}
	transport := &recordingTransport{failActivate: true}
	_, err := fixture.coordinator(state, transport).RunOnce(context.Background())
	require.Error(t, err)

	changed := fixture.coordinator(state, transport)
	changed.Epoch.CoordinatorGeneration++
	changed.Epoch.BarrierID = sha256.Sum256([]byte("new-barrier"))
	_, err = changed.RunOnce(context.Background())
	require.Error(t, err)
	require.Len(t, transport.activations, 2)
	require.Equal(t, transport.activations[0], transport.activations[1])
}

func TestCoordinatorFailsClosedWithoutThresholdPrivateKeys(t *testing.T) {
	fixture := newActivationFixture(t)
	snapshot, err := fixture.chain.PublicationSnapshot(fixture.now)
	require.NoError(t, err)
	snapshot.Current.Authority.Threshold = 2
	_, _, _, err = Build(BuildInput{
		AccountID: fixture.account, StreamEpoch: "stream-epoch-a", Roster: snapshot.Current,
		SecurityEpoch: fixture.epoch, DeviceID: fixture.deviceID, DeviceIdentity: fixture.device, Now: fixture.now,
	})
	require.ErrorIs(t, err, ErrSigningAuthorityUnavailable)
}

func sha256Hex(value string) string {
	digest := sha256.Sum256([]byte(value))
	return fmt.Sprintf("%x", digest)
}
