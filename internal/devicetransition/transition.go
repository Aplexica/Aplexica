// Package devicetransition verifies and installs a client-authorized
// device-access/rekey package through one durable, per-scope roll-forward
// transaction. The transported package contains public roster metadata and
// recipient-authenticated ciphertext only; the relay never sees a plaintext
// namespace key or any signing/wrapping private key.
package devicetransition

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/filelock"
	"github.com/aplexica/aplexica/internal/identity"
	"github.com/aplexica/aplexica/internal/keyrotation"
	"github.com/aplexica/aplexica/internal/plugin/proto"
	"github.com/aplexica/aplexica/internal/privatefs"
	"github.com/aplexica/aplexica/internal/securityepoch"
	"github.com/aplexica/aplexica/internal/securityerr"
	"github.com/fxamacker/cbor/v2"
)

const (
	planVersion         = uint16(1)
	journalVersion      = uint16(1)
	journalMaximum      = int64(8 << 20)
	planDomain          = "aplexica/device-rekey-transition-plan/v1\x00"
	planSignatureDomain = "aplexica/device-rekey-transition-plan-signature/v1"
	journalDomain       = "aplexica/device-rekey-transition-journal/v1\x00"
	packageDomain       = "aplexica/device-rekey-staged-package/v1"
	barrierDomain       = "aplexica/security-epoch-barrier/v1"
	securityEpochFile   = "security-epoch.json"
	rescanFile          = "rekey-rescan-required.json"

	phaseAwaitingDistribution = "awaiting-distribution"
	phaseStaged               = "staged"
	phasePluginPrepared       = "plugin-prepared"
	phaseLocalCommitted       = "local-committed"
	phasePluginCommitted      = "plugin-committed"
)

var ErrTransitionConflict = errors.New("device transition: durable state conflicts with transition plan")

var canonical cbor.EncMode

func init() {
	var err error
	canonical, err = cbor.CanonicalEncOptions().EncMode()
	if err != nil {
		panic(err)
	}
}

type SecurityEpochRecordV1 struct {
	Version               uint16   `json:"version" cbor:"version"`
	ScopeType             string   `json:"scopeType" cbor:"scopeType"`
	ScopeID               string   `json:"scopeId" cbor:"scopeId"`
	RosterHash            [32]byte `json:"rosterHash" cbor:"rosterHash"`
	AccessGeneration      uint64   `json:"accessGeneration" cbor:"accessGeneration"`
	AccessSetHash         [32]byte `json:"accessSetHash" cbor:"accessSetHash"`
	BarrierID             [32]byte `json:"barrierId" cbor:"barrierId"`
	TreeHeadDigest        [32]byte `json:"treeHeadDigest" cbor:"treeHeadDigest"`
	KeyMode               string   `json:"keyMode" cbor:"keyMode"`
	KeyVersion            uint64   `json:"keyVersion" cbor:"keyVersion"`
	CoordinatorGeneration uint64   `json:"coordinatorGeneration" cbor:"coordinatorGeneration"`
}

// SecurityBarrierPreimageV1 is the single canonical preimage shared by plan
// producers and consumers. In particular, a service-provided barrier ID is
// never accepted as authority.
type SecurityBarrierPreimageV1 struct {
	ScopeID                 string   `cbor:"scopeId"`
	PreviousBarrierID       [32]byte `cbor:"previousBarrierId"`
	CurrentAccessGeneration uint64   `cbor:"currentAccessGeneration"`
	CurrentAccessSetHash    [32]byte `cbor:"currentAccessSetHash"`
	NextAccessGeneration    uint64   `cbor:"nextAccessGeneration"`
	NextAccessSetHash       [32]byte `cbor:"nextAccessSetHash"`
	NextKeyMode             string   `cbor:"nextKeyMode"`
	NextKeyVersion          uint64   `cbor:"nextKeyVersion"`
	NextKeyManifestHash     [32]byte `cbor:"nextKeyManifestHash"`
	StagedPackageHash       [32]byte `cbor:"stagedPackageHash"`
}

// PlanV1 deliberately contains no private key, pairing master, recovery
// phrase, or plaintext namespace key. Statement and KeyManifest are signed
// client objects; KeyManifest contains only recipient-authenticated wraps.
type PlanV1 struct {
	Version              uint16                                   `json:"version" cbor:"version"`
	NamespaceID          string                                   `json:"namespaceId" cbor:"namespaceId"`
	PreviousRosterHash   [32]byte                                 `json:"previousRosterHash" cbor:"previousRosterHash"`
	CurrentSecurityEpoch SecurityEpochRecordV1                    `json:"currentSecurityEpoch" cbor:"currentSecurityEpoch"`
	NextRoster           identity.RosterManifestV1                `json:"nextRoster" cbor:"nextRoster"`
	Statement            keyrotation.SignedRotationStatementV1    `json:"statement" cbor:"statement"`
	KeyManifest          keyrotation.SignedNamespaceKeyManifestV1 `json:"keyManifest" cbor:"keyManifest"`
	AuthorizedAtUnix     int64                                    `json:"authorizedAtUnix" cbor:"authorizedAtUnix"`
	SecurityEpoch        SecurityEpochRecordV1                    `json:"securityEpoch" cbor:"securityEpoch"`
	RescanObligationID   [32]byte                                 `json:"rescanObligationId" cbor:"rescanObligationId"`
	StagedPackageHash    [32]byte                                 `json:"stagedPackageHash" cbor:"stagedPackageHash"`
	SignerKeyIDs         [][32]byte                               `json:"signerKeyIds" cbor:"signerKeyIds"`
	Signatures           [][64]byte                               `json:"signatures" cbor:"signatures"`
	Checksum             [32]byte                                 `json:"checksum" cbor:"checksum"`
}

type PlanEndorsementV1 struct {
	SignerKeyID [32]byte `json:"signerKeyId" cbor:"signerKeyId"`
	Signature   [64]byte `json:"signature" cbor:"signature"`
}

type journalStateV1 struct {
	Version  uint16   `json:"version"`
	Phase    string   `json:"phase"`
	Plan     PlanV1   `json:"plan"`
	Checksum [32]byte `json:"checksum"`
}

// CutoverHooks contains only daemon-owned, durable cutover actions. Both
// methods must be idempotent: every process death boundary replays them.
type CutoverHooks interface {
	PurgeOldSealingMaterial(context.Context, PlanV1) error
	RescanCanonical(context.Context, PlanV1) error
}

// BarrierTransport is implemented by daemon.RemoteRunner. Every response is
// checked against the signed plan; plugin state can block or confirm a phase
// but can never create transition authority.
type BarrierTransport interface {
	SecurityEpochPrepare(context.Context, proto.RemoteSecurityEpochPrepareParams) (proto.RemoteSecurityEpochStatusResult, error)
	SecurityEpochCommit(context.Context, proto.RemoteSecurityEpochCommandParams) (proto.RemoteSecurityEpochStatusResult, error)
	SecurityEpochActivate(context.Context, proto.RemoteSecurityEpochCommandParams) (proto.RemoteSecurityEpochStatusResult, error)
	SecurityEpochStatus(context.Context, proto.RemoteSecurityEpochCommandParams) (proto.RemoteSecurityEpochStatusResult, error)
}

type transitionHooks struct {
	afterJournal        func() error
	afterPluginPrepare  func() error
	afterKey            func() error
	afterChain          func() error
	afterRescan         func() error
	afterEpoch          func() error
	afterPluginCommit   func() error
	afterPluginActivate func() error
}

type Installer struct {
	IdentityRoot string
	// Chain is retained for focused callers/tests, but it must resolve to this
	// plan's namespace. Production callers leave it nil and get one chain per
	// scope. This closes the old global-Chain recovery bug.
	Chain            *identity.ChainStore
	Keys             *keyrotation.NamespaceKeyStore
	Coordinator      *securityepoch.Coordinator
	RecipientPrivate [32]byte
	RecipientType    string
	RecipientID      string
	Barrier          BarrierTransport
	Cutover          CutoverHooks
	hooks            transitionHooks
}

// Validate authenticates a transition against the exact locally pinned
// namespace chain, namespace key generation, security coordinator, and any
// existing durable journal without mutating local or remote state. Producers
// call this before placing an opaque plan on a relay; the relay never becomes
// an authority merely because it accepted bytes from an authenticated device.
func (i *Installer) Validate(ctx context.Context, plan PlanV1) error {
	if ctx == nil {
		return ErrTransitionConflict
	}
	encoded, err := EncodePlan(plan)
	if err != nil {
		return err
	}
	root, coordinator, chain, err := i.open(plan.NamespaceID)
	if err != nil {
		return err
	}
	defer root.Close()
	lock, err := filelock.Acquire(filepath.Join(i.IdentityRoot, "namespaces", plan.NamespaceID, ".device-transition.lock"), 30*time.Second)
	if err != nil {
		return err
	}
	defer lock.Close()

	previous, next, err := verifyPlan(chain, plan)
	if err != nil {
		return err
	}
	if state, journalErr := readJournal(root); journalErr == nil {
		journalPlan, encodeErr := EncodePlan(state.Plan)
		if encodeErr != nil || !bytes.Equal(journalPlan, encoded) {
			return ErrTransitionConflict
		}
		return nil
	} else if !errors.Is(journalErr, os.ErrNotExist) {
		return journalErr
	}

	head, err := chain.Head()
	if err != nil {
		return err
	}
	wantEpoch := plan.CurrentSecurityEpoch
	if head.Hash == next.Hash {
		wantEpoch = plan.SecurityEpoch
	} else if head.Hash != previous.Hash {
		return ErrTransitionConflict
	}
	if err := coordinator.VerifyCurrent(plan.NamespaceID, coordinatorEpoch(wantEpoch)); err != nil {
		return err
	}
	// A newly admitted device is deliberately not required to possess the old
	// namespace key. Once the roster has already advanced, however, the exact
	// new key must be locally durable before an idempotent resubmission can pass.
	if head.Hash != next.Hash {
		return nil
	}
	key, err := i.Keys.Current(ctx, plan.NamespaceID)
	if err != nil {
		return err
	}
	if key.Version != plan.Statement.Statement.NewVersion || key.AccessSetHash != next.Manifest.Manifest.AccessSetHash {
		return ErrTransitionConflict
	}
	return nil
}

// StageForDistribution durably gates publication before a locally produced
// plan is sent to opaque cloud storage. It deliberately performs no plugin
// barrier mutation. After an exact cloud receipt, MarkDistributed advances the
// journal to the ordinary staged phase and Install/Recover may continue.
func (i *Installer) StageForDistribution(ctx context.Context, plan PlanV1) error {
	if ctx == nil {
		return ErrTransitionConflict
	}
	encoded, err := EncodePlan(plan)
	if err != nil {
		return err
	}
	root, coordinator, chain, err := i.open(plan.NamespaceID)
	if err != nil {
		return err
	}
	defer root.Close()
	lock, err := filelock.Acquire(filepath.Join(i.IdentityRoot, "namespaces", plan.NamespaceID, ".device-transition.lock"), 30*time.Second)
	if err != nil {
		return err
	}
	defer lock.Close()
	previous, next, err := verifyPlan(chain, plan)
	if err != nil {
		return err
	}
	if state, journalErr := readJournal(root); journalErr == nil {
		existing, encodeErr := EncodePlan(state.Plan)
		if encodeErr != nil || !bytes.Equal(existing, encoded) {
			return ErrTransitionConflict
		}
		return nil
	} else if !errors.Is(journalErr, os.ErrNotExist) {
		return journalErr
	}
	head, err := chain.Head()
	if err != nil {
		return err
	}
	if head.Hash == next.Hash {
		if err := coordinator.VerifyCurrent(plan.NamespaceID, coordinatorEpoch(plan.SecurityEpoch)); err != nil {
			return err
		}
		key, err := i.Keys.Current(ctx, plan.NamespaceID)
		if err != nil || key.Version != plan.SecurityEpoch.KeyVersion || key.AccessSetHash != next.Manifest.Manifest.AccessSetHash {
			return ErrTransitionConflict
		}
		return nil
	}
	if head.Hash != previous.Hash {
		return ErrTransitionConflict
	}
	if err := coordinator.VerifyCurrent(plan.NamespaceID, coordinatorEpoch(plan.CurrentSecurityEpoch)); err != nil {
		return err
	}
	return ensureJournal(root, journalStateV1{Version: journalVersion, Phase: phaseAwaitingDistribution, Plan: plan}, encoded)
}

// MarkDistributed records the exact opaque-store receipt boundary. Retrying it
// after a lost response is idempotent; it never advances a different plan.
func (i *Installer) MarkDistributed(plan PlanV1) error {
	encoded, err := EncodePlan(plan)
	if err != nil {
		return err
	}
	root, coordinator, chain, err := i.open(plan.NamespaceID)
	if err != nil {
		return err
	}
	defer root.Close()
	lock, err := filelock.Acquire(filepath.Join(i.IdentityRoot, "namespaces", plan.NamespaceID, ".device-transition.lock"), 30*time.Second)
	if err != nil {
		return err
	}
	defer lock.Close()
	state, err := readJournal(root)
	if errors.Is(err, os.ErrNotExist) {
		_, next, verifyErr := verifyPlan(chain, plan)
		if verifyErr != nil {
			return verifyErr
		}
		head, headErr := chain.Head()
		if headErr != nil || head.Hash != next.Hash || coordinator.VerifyCurrent(plan.NamespaceID, coordinatorEpoch(plan.SecurityEpoch)) != nil {
			return ErrTransitionConflict
		}
		key, keyErr := i.Keys.Current(context.Background(), plan.NamespaceID)
		if keyErr != nil || key.Version != plan.SecurityEpoch.KeyVersion || key.AccessSetHash != plan.SecurityEpoch.AccessSetHash {
			return ErrTransitionConflict
		}
		return nil
	}
	if err != nil {
		return err
	}
	existing, err := EncodePlan(state.Plan)
	if err != nil || !bytes.Equal(existing, encoded) {
		return ErrTransitionConflict
	}
	if state.Phase == phaseAwaitingDistribution {
		return writeJournalPhase(root, &state, phaseStaged)
	}
	return nil
}

// PendingDistribution returns only fully authenticated local journals that
// still need an exact opaque-store receipt. The returned plans contain no
// plaintext or private key material.
func (i *Installer) PendingDistribution() ([]PlanV1, error) {
	if i == nil || !validRoot(i.IdentityRoot) {
		return nil, ErrTransitionConflict
	}
	entries, err := os.ReadDir(filepath.Join(i.IdentityRoot, "namespaces"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var plans []PlanV1
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		root, err := privatefs.OpenRoot(filepath.Join(i.IdentityRoot, "namespaces", entry.Name()), privatefs.DirPolicy{Access: privatefs.AccessPrivate, RepairOwned: true, AllowExisting: true})
		if err != nil {
			return nil, err
		}
		state, readErr := readJournal(root)
		_ = root.Close()
		if errors.Is(readErr, os.ErrNotExist) {
			continue
		}
		if readErr != nil || state.Plan.NamespaceID != entry.Name() {
			if readErr != nil {
				return nil, readErr
			}
			return nil, ErrTransitionConflict
		}
		chain := &identity.ChainStore{Path: filepath.Join(i.IdentityRoot, "namespaces", entry.Name(), "chain.cbor")}
		if _, _, err := verifyPlan(chain, state.Plan); err != nil {
			return nil, err
		}
		if state.Phase == phaseAwaitingDistribution {
			plans = append(plans, state.Plan)
		}
	}
	return plans, nil
}

// BuildPlan constructs the unsigned outer-plan proposal. It accepts only
// already threshold-signed roster/rotation objects and derives every package,
// barrier, and rescan binding locally. Callers must collect EndorsePlan results
// and pass them to FinalizePlan before the plan can be encoded or relayed.
func BuildPlan(previous, next identity.VerifiedRoster, current SecurityEpochRecordV1, statement keyrotation.SignedRotationStatementV1, manifest keyrotation.SignedNamespaceKeyManifestV1, authorizedAt time.Time, treeHeadDigest [32]byte) (PlanV1, error) {
	authorizedAt = authorizedAt.UTC().Truncate(time.Second)
	verifiedNext, err := identity.VerifyTransition(previous, previous.Authority, next.Manifest)
	if err != nil || verifiedNext.Hash != next.Hash || keyrotation.VerifyRotationStatement(previous, next, statement, authorizedAt) != nil ||
		keyrotation.VerifyNamespaceKeyManifest(next, statement, manifest) != nil {
		return PlanV1{}, ErrTransitionConflict
	}
	nextManifest := next.Manifest.Manifest
	if nextManifest.ScopeType != "namespace" || nextManifest.ScopeID == "" || current.Version != 1 ||
		current.ScopeType != "namespace" || current.ScopeID != nextManifest.ScopeID || current.RosterHash != [32]byte(previous.Hash) ||
		current.AccessGeneration != previous.Manifest.Manifest.AccessGeneration || current.AccessSetHash != previous.Manifest.Manifest.AccessSetHash ||
		current.KeyMode != "namespace-key-v1" || current.KeyVersion != statement.Statement.PreviousVersion || current.CoordinatorGeneration == 0 || current.CoordinatorGeneration == ^uint64(0) ||
		current.BarrierID == ([32]byte{}) || current.TreeHeadDigest == ([32]byte{}) || treeHeadDigest == ([32]byte{}) {
		return PlanV1{}, ErrTransitionConflict
	}
	plan := PlanV1{
		Version: planVersion, NamespaceID: nextManifest.ScopeID, PreviousRosterHash: [32]byte(previous.Hash), CurrentSecurityEpoch: current,
		NextRoster: next.Manifest, Statement: statement, KeyManifest: manifest,
		AuthorizedAtUnix: authorizedAt.Unix(), SecurityEpoch: SecurityEpochRecordV1{
			Version: 1, ScopeType: "namespace", ScopeID: nextManifest.ScopeID, RosterHash: [32]byte(next.Hash),
			AccessGeneration: nextManifest.AccessGeneration, AccessSetHash: nextManifest.AccessSetHash,
			TreeHeadDigest: treeHeadDigest, KeyMode: "namespace-key-v1", KeyVersion: statement.Statement.NewVersion,
			CoordinatorGeneration: current.CoordinatorGeneration + 1,
		},
	}
	plan.StagedPackageHash, err = stagedPackageHash(plan)
	if err != nil {
		return PlanV1{}, err
	}
	manifestHash, err := keyrotation.ManifestHash(manifest)
	if err != nil {
		return PlanV1{}, err
	}
	preimage := SecurityBarrierPreimageV1{
		ScopeID: plan.NamespaceID, PreviousBarrierID: current.BarrierID,
		CurrentAccessGeneration: current.AccessGeneration, CurrentAccessSetHash: current.AccessSetHash,
		NextAccessGeneration: plan.SecurityEpoch.AccessGeneration, NextAccessSetHash: plan.SecurityEpoch.AccessSetHash,
		NextKeyMode: plan.SecurityEpoch.KeyMode, NextKeyVersion: plan.SecurityEpoch.KeyVersion,
		NextKeyManifestHash: manifestHash, StagedPackageHash: plan.StagedPackageHash,
	}
	plan.SecurityEpoch.BarrierID, err = BarrierID(preimage)
	if err != nil {
		return PlanV1{}, err
	}
	obligation, err := canonical.Marshal([]any{"aplexica/rekey-rescan-obligation/v1", next.Hash, manifestHash, plan.SecurityEpoch.BarrierID})
	if err != nil {
		return PlanV1{}, err
	}
	plan.RescanObligationID = sha256.Sum256(obligation)
	return plan, nil
}

// EndorsePlan signs every outer orchestration field. This prevents an opaque
// relay from changing otherwise-unsigned metadata (for example a tree-head
// digest or authorization instant) and merely recomputing the integrity
// checksum. FinalizePlan still proves that each signer is an active previous-
// roster authority and that the threshold is met.
func EndorsePlan(proposal PlanV1, signerKeyID [32]byte, signingPrivate ed25519.PrivateKey) (PlanEndorsementV1, error) {
	if len(proposal.SignerKeyIDs) != 0 || len(proposal.Signatures) != 0 || proposal.Checksum != ([32]byte{}) || signerKeyID == ([32]byte{}) || len(signingPrivate) != ed25519.PrivateKeySize {
		return PlanEndorsementV1{}, ErrTransitionConflict
	}
	public, ok := signingPrivate.Public().(ed25519.PublicKey)
	if !ok || sha256.Sum256(public) != signerKeyID {
		return PlanEndorsementV1{}, ErrTransitionConflict
	}
	preimage, err := planSignaturePreimage(proposal)
	if err != nil {
		return PlanEndorsementV1{}, err
	}
	endorsement := PlanEndorsementV1{SignerKeyID: signerKeyID}
	copy(endorsement.Signature[:], ed25519.Sign(signingPrivate, preimage))
	return endorsement, nil
}

// FinalizePlan canonicalizes and verifies active-authority endorsements, then
// adds a transport checksum. The checksum detects accidental byte corruption;
// the threshold signatures are the authorization boundary.
func FinalizePlan(previous identity.VerifiedRoster, proposal PlanV1, endorsements []PlanEndorsementV1) (PlanV1, error) {
	if len(proposal.SignerKeyIDs) != 0 || len(proposal.Signatures) != 0 || proposal.Checksum != ([32]byte{}) ||
		len(endorsements) < int(previous.Authority.Threshold) || len(endorsements) > len(previous.Authority.Authorities) {
		return PlanV1{}, ErrTransitionConflict
	}
	next, err := identity.VerifyTransition(previous, previous.Authority, proposal.NextRoster)
	if err != nil {
		return PlanV1{}, ErrTransitionConflict
	}
	rebuilt, err := BuildPlan(previous, next, proposal.CurrentSecurityEpoch, proposal.Statement, proposal.KeyManifest,
		time.Unix(proposal.AuthorizedAtUnix, 0), proposal.SecurityEpoch.TreeHeadDigest)
	if err != nil {
		return PlanV1{}, err
	}
	wantProposal, err := planSignaturePreimage(rebuilt)
	if err != nil {
		return PlanV1{}, err
	}
	gotProposal, err := planSignaturePreimage(proposal)
	if err != nil || !bytes.Equal(gotProposal, wantProposal) {
		return PlanV1{}, ErrTransitionConflict
	}

	sorted := append([]PlanEndorsementV1(nil), endorsements...)
	sort.Slice(sorted, func(left, right int) bool {
		return bytes.Compare(sorted[left].SignerKeyID[:], sorted[right].SignerKeyID[:]) < 0
	})
	for index, endorsement := range sorted {
		if index > 0 && bytes.Compare(sorted[index-1].SignerKeyID[:], endorsement.SignerKeyID[:]) >= 0 {
			return PlanV1{}, ErrTransitionConflict
		}
		authority, ok := previous.Authority.Authorities[identity.DeviceKeyID(endorsement.SignerKeyID)]
		if !ok || !activePlanAuthority(previous, authority, proposal.AuthorizedAtUnix) ||
			!ed25519.Verify(authority.SigningPublicKey[:], gotProposal, endorsement.Signature[:]) {
			return PlanV1{}, ErrTransitionConflict
		}
	}
	proposal.SignerKeyIDs = make([][32]byte, len(sorted))
	proposal.Signatures = make([][64]byte, len(sorted))
	for index, endorsement := range sorted {
		proposal.SignerKeyIDs[index] = endorsement.SignerKeyID
		proposal.Signatures[index] = endorsement.Signature
	}
	proposal.Checksum, err = planChecksum(proposal)
	if err != nil {
		return PlanV1{}, err
	}
	if _, err := EncodePlan(proposal); err != nil {
		return PlanV1{}, err
	}
	return proposal, nil
}

func planSignaturePreimage(plan PlanV1) ([]byte, error) {
	plan.SignerKeyIDs = nil
	plan.Signatures = nil
	plan.Checksum = [32]byte{}
	return canonical.Marshal([]any{planSignatureDomain, plan})
}

func activePlanAuthority(previous identity.VerifiedRoster, authority identity.RosterAuthorityV1, authorizedAt int64) bool {
	for _, signed := range previous.Manifest.Manifest.Devices {
		credential := signed.Certificate
		if credential.DeviceID == authority.DeviceID && credential.SigningKeyID == authority.SigningKeyID &&
			credential.SigningPublicKey == authority.SigningPublicKey && credential.NotBeforeUnix <= authorizedAt && authorizedAt < credential.NotAfterUnix {
			return true
		}
	}
	return false
}

func BarrierID(preimage SecurityBarrierPreimageV1) ([32]byte, error) {
	if preimage.ScopeID == "" || preimage.PreviousBarrierID == ([32]byte{}) || preimage.CurrentAccessGeneration == 0 ||
		preimage.CurrentAccessSetHash == ([32]byte{}) || preimage.NextAccessGeneration != preimage.CurrentAccessGeneration+1 ||
		preimage.NextAccessSetHash == ([32]byte{}) || preimage.NextKeyMode != "namespace-key-v1" || preimage.NextKeyVersion == 0 ||
		preimage.NextKeyManifestHash == ([32]byte{}) || preimage.StagedPackageHash == ([32]byte{}) {
		return [32]byte{}, ErrTransitionConflict
	}
	b, err := canonical.Marshal([]any{barrierDomain, preimage})
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(b), nil
}

func (i *Installer) Install(ctx context.Context, plan PlanV1) error {
	encoded, err := EncodePlan(plan)
	if err != nil {
		return err
	}
	root, coordinator, chain, err := i.open(plan.NamespaceID)
	if err != nil {
		return err
	}
	defer root.Close()
	lock, err := filelock.Acquire(filepath.Join(i.IdentityRoot, "namespaces", plan.NamespaceID, ".device-transition.lock"), 30*time.Second)
	if err != nil {
		return err
	}
	defer lock.Close()
	if _, readErr := readJournal(root); errors.Is(readErr, os.ErrNotExist) {
		complete, completeErr := i.completed(ctx, coordinator, chain, plan)
		if completeErr != nil {
			return completeErr
		}
		if complete {
			return nil
		}
		// Do not let a stale-but-well-signed relayed plan create the journal
		// that blocks publication. The current local coordinator tuple is the
		// final pre-mutation authority gate; recovery rechecks it again below.
		if err := coordinator.VerifyCurrent(plan.NamespaceID, coordinatorEpoch(plan.CurrentSecurityEpoch)); err != nil {
			return err
		}
	} else if readErr != nil {
		return readErr
	}
	state := journalStateV1{Version: journalVersion, Phase: phaseStaged, Plan: plan}
	if err := ensureJournal(root, state, encoded); err != nil {
		return err
	}
	if err := boundary(ctx, i.hooks.afterJournal); err != nil {
		return err
	}
	persisted, err := readJournal(root)
	if err != nil {
		return err
	}
	return i.apply(ctx, root, coordinator, chain, persisted)
}

func (i *Installer) completed(ctx context.Context, coordinator *securityepoch.Coordinator, chain *identity.ChainStore, plan PlanV1) (bool, error) {
	previous, next, err := verifyPlan(chain, plan)
	if err != nil {
		return false, err
	}
	head, err := chain.Head()
	if err != nil {
		return false, err
	}
	if head.Hash != next.Hash {
		if head.Hash != previous.Hash {
			return false, ErrTransitionConflict
		}
		return false, nil
	}
	key, err := i.Keys.Current(ctx, plan.NamespaceID)
	if err != nil {
		return false, err
	}
	manifestHash, err := keyrotation.ManifestHash(plan.KeyManifest)
	if err != nil || key.ManifestHash != manifestHash || key.Version != plan.SecurityEpoch.KeyVersion || key.AccessSetHash != plan.SecurityEpoch.AccessSetHash {
		return false, ErrTransitionConflict
	}
	if err := coordinator.VerifyCurrent(plan.NamespaceID, coordinatorEpoch(plan.SecurityEpoch)); err != nil {
		return false, err
	}
	prepare := proto.RemoteSecurityEpochPrepareParams{ScopeID: plan.NamespaceID, BarrierID: plan.SecurityEpoch.BarrierID, Current: epochWire(plan.CurrentSecurityEpoch), Next: epochWire(plan.SecurityEpoch), StagedPackageHash: plan.StagedPackageHash}
	status, err := i.Barrier.SecurityEpochStatus(ctx, proto.RemoteSecurityEpochCommandParams{ScopeID: plan.NamespaceID, BarrierID: plan.SecurityEpoch.BarrierID})
	if err != nil || verifyBarrierStatus(status, prepare, "active") != nil {
		if err != nil {
			return false, err
		}
		return false, ErrTransitionConflict
	}
	return true, nil
}

// Recover rolls every pending namespace forward independently using its own
// verified chain. Corrupt scope state is never skipped.
func (i *Installer) Recover(ctx context.Context) (bool, error) {
	if i == nil || !validRoot(i.IdentityRoot) {
		return false, ErrTransitionConflict
	}
	namespacesPath := filepath.Join(i.IdentityRoot, "namespaces")
	entries, err := os.ReadDir(namespacesPath)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	found := false
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		root, coordinator, chain, openErr := i.open(entry.Name())
		if openErr != nil {
			return found, openErr
		}
		state, readErr := readJournal(root)
		if errors.Is(readErr, os.ErrNotExist) {
			_ = root.Close()
			continue
		}
		if readErr != nil || state.Plan.NamespaceID != entry.Name() {
			_ = root.Close()
			if readErr != nil {
				return true, readErr
			}
			return true, ErrTransitionConflict
		}
		found = true
		lock, lockErr := filelock.Acquire(filepath.Join(i.IdentityRoot, "namespaces", entry.Name(), ".device-transition.lock"), 30*time.Second)
		if lockErr == nil {
			lockErr = i.apply(ctx, root, coordinator, chain, state)
			_ = lock.Close()
		}
		_ = root.Close()
		if lockErr != nil {
			return true, lockErr
		}
	}
	return found, nil
}

// ValidatePending performs the pre-plugin startup gate. It authenticates every
// journal and its path binding without requiring network access. The daemon
// then starts the verified plugin and Recover completes the remote phases.
func ValidatePending(identityRoot string) (bool, error) {
	if !validRoot(identityRoot) {
		return false, ErrTransitionConflict
	}
	entries, err := os.ReadDir(filepath.Join(identityRoot, "namespaces"))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	found := false
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(identityRoot, "namespaces", entry.Name(), securityepoch.TransitionJournalFilename)
		if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return found, err
		}
		root, err := privatefs.OpenRoot(filepath.Dir(path), privatefs.DirPolicy{Access: privatefs.AccessPrivate, RepairOwned: true})
		if err != nil {
			return true, err
		}
		state, readErr := readJournal(root)
		_ = root.Close()
		if readErr != nil || state.Plan.NamespaceID != entry.Name() {
			if readErr != nil {
				return true, readErr
			}
			return true, ErrTransitionConflict
		}
		chain := &identity.ChainStore{Path: filepath.Join(identityRoot, "namespaces", entry.Name(), "chain.cbor")}
		if _, _, err := verifyPlan(chain, state.Plan); err != nil {
			return true, err
		}
		found = true
	}
	return found, nil
}

func validRoot(root string) bool {
	return filepath.IsAbs(root) && filepath.Clean(root) == root
}

func (i *Installer) open(namespaceID string) (*privatefs.Root, *securityepoch.Coordinator, *identity.ChainStore, error) {
	if i == nil || !validRoot(i.IdentityRoot) || acf.ValidateWireUUIDv7(namespaceID) != nil || i.Keys == nil || i.Barrier == nil || i.Cutover == nil {
		return nil, nil, nil, ErrTransitionConflict
	}
	expectedKeys := filepath.Join(i.IdentityRoot, "namespace-keys")
	keysRoot, err := filepath.Abs(i.Keys.Root)
	if err != nil || filepath.Clean(keysRoot) != expectedKeys {
		return nil, nil, nil, ErrTransitionConflict
	}
	coordinator := i.Coordinator
	if coordinator == nil {
		coordinator = &securityepoch.Coordinator{Root: i.IdentityRoot}
	} else if absolute, err := filepath.Abs(coordinator.Root); err != nil || filepath.Clean(absolute) != i.IdentityRoot {
		return nil, nil, nil, ErrTransitionConflict
	}
	expectedChain := filepath.Join(i.IdentityRoot, "namespaces", namespaceID, "chain.cbor")
	chain := i.Chain
	if chain == nil {
		chain = &identity.ChainStore{Path: expectedChain}
	} else if absolute, err := filepath.Abs(chain.Path); err != nil || filepath.Clean(absolute) != expectedChain {
		return nil, nil, nil, ErrTransitionConflict
	}
	root, err := privatefs.OpenRoot(filepath.Join(i.IdentityRoot, "namespaces", namespaceID), privatefs.DirPolicy{Access: privatefs.AccessPrivate, RepairOwned: true, AllowExisting: true})
	return root, coordinator, chain, err
}

func (i *Installer) apply(ctx context.Context, root *privatefs.Root, coordinator *securityepoch.Coordinator, chain *identity.ChainStore, state journalStateV1) error {
	plan := state.Plan
	previous, next, err := verifyPlan(chain, plan)
	if err != nil {
		return err
	}
	currentWire := epochWire(plan.CurrentSecurityEpoch)
	nextWire := epochWire(plan.SecurityEpoch)
	prepare := proto.RemoteSecurityEpochPrepareParams{ScopeID: plan.NamespaceID, BarrierID: plan.SecurityEpoch.BarrierID, Current: currentWire, Next: nextWire, StagedPackageHash: plan.StagedPackageHash}
	command := proto.RemoteSecurityEpochCommandParams{ScopeID: plan.NamespaceID, BarrierID: plan.SecurityEpoch.BarrierID}

	if state.Phase == phaseStaged {
		// The signed roster transition constrains the plan, but the current
		// local coordinator is a separate durable participant. Pin it exactly
		// before asking the plugin to prepare so a stale-but-valid roster plan
		// cannot advance a divergent local security generation.
		if err := coordinator.VerifyCurrent(plan.NamespaceID, coordinatorEpoch(plan.CurrentSecurityEpoch)); err != nil {
			return err
		}
		status, prepareErr := i.Barrier.SecurityEpochPrepare(ctx, prepare)
		if prepareErr != nil {
			return prepareErr
		}
		if err := verifyBarrierStatus(status, prepare, "prepared"); err != nil {
			return err
		}
		if err := writeJournalPhase(root, &state, phasePluginPrepared); err != nil {
			return err
		}
		if err := boundary(ctx, i.hooks.afterPluginPrepare); err != nil {
			return err
		}
	}

	if state.Phase == phasePluginPrepared {
		status, err := i.Barrier.SecurityEpochStatus(ctx, command)
		if err != nil || verifyBarrierStatus(status, prepare, "prepared") != nil {
			if err != nil {
				return err
			}
			return ErrTransitionConflict
		}
		if (i.RecipientType != "device" && i.RecipientType != "recovery") || i.RecipientID == "" {
			return ErrTransitionConflict
		}
		snapshot, err := i.Keys.InstallVerifiedAt(ctx, previous, next, plan.Statement, plan.KeyManifest, i.RecipientType, i.RecipientID, i.RecipientPrivate, time.Unix(plan.AuthorizedAtUnix, 0))
		if err != nil {
			return err
		}
		if err := boundary(ctx, i.hooks.afterKey); err != nil {
			return err
		}
		obligation, _ := json.Marshal(struct {
			Version uint16   `json:"version"`
			ID      [32]byte `json:"id"`
		}{1, plan.RescanObligationID})
		if err := root.WriteFile(rescanFile, obligation, privatefs.FilePolicy{RejectWritableByOthers: true}); err != nil {
			return err
		}
		nextEpoch := coordinatorEpoch(plan.SecurityEpoch)
		err = coordinator.Transition(ctx, plan.NamespaceID, nextEpoch, func() error {
			// The dirty generation-bound recovery marker must precede deletion of
			// any reconstructable old ciphertext intent.
			if err := i.Cutover.RescanCanonical(ctx, plan); err != nil {
				return err
			}
			if err := boundary(ctx, i.hooks.afterRescan); err != nil {
				return err
			}
			if err := i.Cutover.PurgeOldSealingMaterial(ctx, plan); err != nil {
				return err
			}
			if _, err := chain.AppendRosterExact(plan.NextRoster); err != nil {
				return err
			}
			if err := boundary(ctx, i.hooks.afterChain); err != nil {
				return err
			}
			current, err := i.Keys.Current(ctx, plan.NamespaceID)
			if err != nil || current.ManifestHash != snapshot.ManifestHash || current.Version != plan.SecurityEpoch.KeyVersion || current.AccessSetHash != plan.SecurityEpoch.AccessSetHash {
				return ErrTransitionConflict
			}
			epochBytes, err := json.Marshal(plan.SecurityEpoch)
			if err != nil {
				return err
			}
			return root.WriteFile(securityEpochFile, epochBytes, privatefs.FilePolicy{RejectWritableByOthers: true})
		})
		if err != nil {
			return err
		}
		if err := boundary(ctx, i.hooks.afterEpoch); err != nil {
			return err
		}
		if err := writeJournalPhase(root, &state, phaseLocalCommitted); err != nil {
			return err
		}
	}

	if state.Phase == phaseLocalCommitted {
		status, err := i.Barrier.SecurityEpochCommit(ctx, command)
		if err != nil {
			return err
		}
		if err := verifyBarrierStatus(status, prepare, "committed"); err != nil {
			return err
		}
		if err := writeJournalPhase(root, &state, phasePluginCommitted); err != nil {
			return err
		}
		if err := boundary(ctx, i.hooks.afterPluginCommit); err != nil {
			return err
		}
	}

	if state.Phase != phasePluginCommitted {
		return ErrTransitionConflict
	}
	status, err := i.Barrier.SecurityEpochActivate(ctx, command)
	if err != nil {
		return err
	}
	if err := verifyBarrierStatus(status, prepare, "active"); err != nil {
		return err
	}
	status, err = i.Barrier.SecurityEpochStatus(ctx, command)
	if err != nil || verifyBarrierStatus(status, prepare, "active") != nil {
		if err != nil {
			return err
		}
		return ErrTransitionConflict
	}
	if err := boundary(ctx, i.hooks.afterPluginActivate); err != nil {
		return err
	}
	head, err := chain.Head()
	if err != nil || head.Hash != next.Hash || coordinator.VerifyCurrent(plan.NamespaceID, coordinatorEpoch(plan.SecurityEpoch)) != nil {
		return ErrTransitionConflict
	}
	if err := root.RemoveRegular(rescanFile); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return root.RemoveRegular(securityepoch.TransitionJournalFilename)
}

func verifyPlan(chain *identity.ChainStore, plan PlanV1) (identity.VerifiedRoster, identity.VerifiedRoster, error) {
	if chain == nil {
		return identity.VerifiedRoster{}, identity.VerifiedRoster{}, ErrTransitionConflict
	}
	if _, err := EncodePlan(plan); err != nil {
		return identity.VerifiedRoster{}, identity.VerifiedRoster{}, err
	}
	previous, err := chain.VerifiedByHash(identity.RosterHash(plan.PreviousRosterHash))
	if err != nil {
		return identity.VerifiedRoster{}, identity.VerifiedRoster{}, err
	}
	if err := verifyPlanAuthorization(previous, plan); err != nil {
		return identity.VerifiedRoster{}, identity.VerifiedRoster{}, err
	}
	next, err := identity.VerifyTransition(previous, previous.Authority, plan.NextRoster)
	if err != nil || keyrotation.VerifyRotationStatement(previous, next, plan.Statement, time.Unix(plan.AuthorizedAtUnix, 0)) != nil || keyrotation.VerifyNamespaceKeyManifest(next, plan.Statement, plan.KeyManifest) != nil {
		return identity.VerifiedRoster{}, identity.VerifiedRoster{}, ErrTransitionConflict
	}
	if plan.NamespaceID != next.Manifest.Manifest.ScopeID || plan.CurrentSecurityEpoch.ScopeID != plan.NamespaceID || plan.SecurityEpoch.ScopeID != plan.NamespaceID ||
		plan.CurrentSecurityEpoch.Version != 1 || plan.CurrentSecurityEpoch.ScopeType != "namespace" || plan.CurrentSecurityEpoch.RosterHash != [32]byte(previous.Hash) ||
		plan.CurrentSecurityEpoch.AccessGeneration != previous.Manifest.Manifest.AccessGeneration || plan.CurrentSecurityEpoch.AccessSetHash != previous.Manifest.Manifest.AccessSetHash ||
		plan.CurrentSecurityEpoch.KeyMode != "namespace-key-v1" || plan.CurrentSecurityEpoch.KeyVersion != plan.Statement.Statement.PreviousVersion ||
		plan.CurrentSecurityEpoch.CoordinatorGeneration == 0 || plan.CurrentSecurityEpoch.BarrierID == ([32]byte{}) || plan.CurrentSecurityEpoch.TreeHeadDigest == ([32]byte{}) ||
		plan.SecurityEpoch.Version != 1 || plan.SecurityEpoch.ScopeType != "namespace" || plan.SecurityEpoch.RosterHash != [32]byte(next.Hash) ||
		plan.SecurityEpoch.AccessGeneration != next.Manifest.Manifest.AccessGeneration || plan.SecurityEpoch.AccessSetHash != next.Manifest.Manifest.AccessSetHash ||
		plan.SecurityEpoch.KeyMode != "namespace-key-v1" || plan.SecurityEpoch.KeyVersion != plan.Statement.Statement.NewVersion ||
		plan.SecurityEpoch.CoordinatorGeneration != plan.CurrentSecurityEpoch.CoordinatorGeneration+1 || plan.SecurityEpoch.BarrierID == ([32]byte{}) || plan.SecurityEpoch.TreeHeadDigest == ([32]byte{}) {
		return identity.VerifiedRoster{}, identity.VerifiedRoster{}, ErrTransitionConflict
	}
	staged, err := stagedPackageHash(plan)
	if err != nil || staged != plan.StagedPackageHash {
		return identity.VerifiedRoster{}, identity.VerifiedRoster{}, ErrTransitionConflict
	}
	manifestHash, err := keyrotation.ManifestHash(plan.KeyManifest)
	if err != nil {
		return identity.VerifiedRoster{}, identity.VerifiedRoster{}, err
	}
	barrier, err := BarrierID(SecurityBarrierPreimageV1{
		ScopeID: plan.NamespaceID, PreviousBarrierID: plan.CurrentSecurityEpoch.BarrierID,
		CurrentAccessGeneration: plan.CurrentSecurityEpoch.AccessGeneration, CurrentAccessSetHash: plan.CurrentSecurityEpoch.AccessSetHash,
		NextAccessGeneration: plan.SecurityEpoch.AccessGeneration, NextAccessSetHash: plan.SecurityEpoch.AccessSetHash,
		NextKeyMode: plan.SecurityEpoch.KeyMode, NextKeyVersion: plan.SecurityEpoch.KeyVersion,
		NextKeyManifestHash: manifestHash, StagedPackageHash: plan.StagedPackageHash,
	})
	if err != nil || barrier != plan.SecurityEpoch.BarrierID {
		return identity.VerifiedRoster{}, identity.VerifiedRoster{}, ErrTransitionConflict
	}
	obligation, err := canonical.Marshal([]any{"aplexica/rekey-rescan-obligation/v1", next.Hash, manifestHash, barrier})
	if err != nil || sha256.Sum256(obligation) != plan.RescanObligationID {
		return identity.VerifiedRoster{}, identity.VerifiedRoster{}, ErrTransitionConflict
	}
	return previous, next, nil
}

func verifyPlanAuthorization(previous identity.VerifiedRoster, plan PlanV1) error {
	if len(plan.SignerKeyIDs) != len(plan.Signatures) || len(plan.SignerKeyIDs) < int(previous.Authority.Threshold) ||
		len(plan.SignerKeyIDs) > len(previous.Authority.Authorities) {
		return ErrTransitionConflict
	}
	preimage, err := planSignaturePreimage(plan)
	if err != nil {
		return err
	}
	for index, signerKeyID := range plan.SignerKeyIDs {
		if index > 0 && bytes.Compare(plan.SignerKeyIDs[index-1][:], signerKeyID[:]) >= 0 {
			return ErrTransitionConflict
		}
		authority, ok := previous.Authority.Authorities[identity.DeviceKeyID(signerKeyID)]
		if !ok || !activePlanAuthority(previous, authority, plan.AuthorizedAtUnix) ||
			!ed25519.Verify(authority.SigningPublicKey[:], preimage, plan.Signatures[index][:]) {
			return ErrTransitionConflict
		}
	}
	return nil
}

func epochWire(value SecurityEpochRecordV1) proto.RemoteSecurityEpoch {
	return proto.RemoteSecurityEpoch{Generation: value.CoordinatorGeneration, AccessGeneration: value.AccessGeneration, AccessSetHash: value.AccessSetHash, BarrierID: value.BarrierID, KeyMode: value.KeyMode, KeyVersion: value.KeyVersion}
}

func coordinatorEpoch(value SecurityEpochRecordV1) securityepoch.SecurityEpoch {
	return securityepoch.SecurityEpoch{CoordinatorGeneration: value.CoordinatorGeneration, AccessGeneration: value.AccessGeneration, AccessSetHash: value.AccessSetHash, BarrierID: value.BarrierID, KeyMode: value.KeyMode, KeyVersion: value.KeyVersion}
}

func verifyBarrierStatus(status proto.RemoteSecurityEpochStatusResult, prepare proto.RemoteSecurityEpochPrepareParams, phase string) error {
	wantCurrent := prepare.Current
	if phase == "active" {
		wantCurrent = prepare.Next
	}
	if status.ScopeID != prepare.ScopeID || status.Phase != phase || status.Current != wantCurrent || status.Next != prepare.Next || status.StagedPackageHash != prepare.StagedPackageHash {
		return ErrTransitionConflict
	}
	return nil
}

func boundary(ctx context.Context, hook func() error) error {
	if ctx == nil {
		return ErrTransitionConflict
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if hook != nil {
		return hook()
	}
	return nil
}

func stagedPackageHash(plan PlanV1) ([32]byte, error) {
	b, err := canonical.Marshal([]any{packageDomain, plan.NamespaceID, plan.PreviousRosterHash, plan.NextRoster, plan.Statement, plan.KeyManifest, plan.AuthorizedAtUnix})
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(b), nil
}

func planChecksum(plan PlanV1) ([32]byte, error) {
	plan.Checksum = [32]byte{}
	encoded, err := json.Marshal(plan)
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(append([]byte(planDomain), encoded...)), nil
}

// EncodePlan emits one canonical, bounded transport blob and refuses to repair
// a caller-supplied checksum. Received data is either the exact produced plan
// or it is rejected.
func EncodePlan(plan PlanV1) ([]byte, error) {
	if plan.Version != planVersion || acf.ValidateWireUUIDv7(plan.NamespaceID) != nil || plan.PreviousRosterHash == ([32]byte{}) || plan.RescanObligationID == ([32]byte{}) || plan.StagedPackageHash == ([32]byte{}) ||
		len(plan.SignerKeyIDs) == 0 || len(plan.SignerKeyIDs) != len(plan.Signatures) {
		return nil, ErrTransitionConflict
	}
	want, err := planChecksum(plan)
	if err != nil || want != plan.Checksum {
		return nil, ErrTransitionConflict
	}
	encoded, err := json.Marshal(plan)
	if err != nil || int64(len(encoded)) > journalMaximum {
		return nil, securityerr.ErrLimitExceeded
	}
	return encoded, nil
}

// DecodePlan is the bounded transport ingress. Unknown fields, noncanonical
// JSON, trailing values, and any checksum mismatch fail closed.
func DecodePlan(encoded []byte) (PlanV1, error) {
	if len(encoded) == 0 || int64(len(encoded)) > journalMaximum {
		return PlanV1{}, securityerr.ErrLimitExceeded
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var plan PlanV1
	if decoder.Decode(&plan) != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return PlanV1{}, ErrTransitionConflict
	}
	canonicalBytes, err := EncodePlan(plan)
	if err != nil || !bytes.Equal(canonicalBytes, encoded) {
		return PlanV1{}, ErrTransitionConflict
	}
	return plan, nil
}

func journalChecksum(state journalStateV1) ([32]byte, error) {
	state.Checksum = [32]byte{}
	b, err := json.Marshal(state)
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(append([]byte(journalDomain), b...)), nil
}

func validPhase(phase string) bool {
	return phase == phaseAwaitingDistribution || phase == phaseStaged || phase == phasePluginPrepared || phase == phaseLocalCommitted || phase == phasePluginCommitted
}

func encodeJournal(state journalStateV1) ([]byte, error) {
	if state.Version != journalVersion || !validPhase(state.Phase) {
		return nil, ErrTransitionConflict
	}
	if _, err := EncodePlan(state.Plan); err != nil {
		return nil, err
	}
	var err error
	state.Checksum, err = journalChecksum(state)
	if err != nil {
		return nil, err
	}
	b, err := json.Marshal(state)
	if err != nil || int64(len(b)) > journalMaximum {
		return nil, securityerr.ErrLimitExceeded
	}
	return b, nil
}

func ensureJournal(root *privatefs.Root, state journalStateV1, planBytes []byte) error {
	existing, err := readJournal(root)
	if err == nil {
		existingPlan, encodeErr := EncodePlan(existing.Plan)
		if encodeErr != nil || !bytes.Equal(existingPlan, planBytes) {
			return ErrTransitionConflict
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	want, err := encodeJournal(state)
	if err != nil {
		return err
	}
	file, err := root.CreateExclusive(securityepoch.TransitionJournalFilename, privatefs.FilePolicy{RejectWritableByOthers: true})
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return ErrTransitionConflict
		}
		return err
	}
	written, writeErr := file.Write(want)
	if writeErr == nil && written != len(want) {
		writeErr = io.ErrShortWrite
	}
	if writeErr == nil {
		writeErr = file.Sync()
	}
	if closeErr := file.Close(); writeErr == nil {
		writeErr = closeErr
	}
	if writeErr != nil {
		return writeErr
	}
	return root.SyncDir(".")
}

func writeJournalPhase(root *privatefs.Root, state *journalStateV1, phase string) error {
	if state == nil || !validPhase(phase) {
		return ErrTransitionConflict
	}
	rank := map[string]int{phaseAwaitingDistribution: 0, phaseStaged: 1, phasePluginPrepared: 2, phaseLocalCommitted: 3, phasePluginCommitted: 4}
	if rank[phase] < rank[state.Phase] || rank[phase] > rank[state.Phase]+1 {
		return ErrTransitionConflict
	}
	state.Phase = phase
	b, err := encodeJournal(*state)
	if err != nil {
		return err
	}
	return root.WriteFile(securityepoch.TransitionJournalFilename, b, privatefs.FilePolicy{RejectWritableByOthers: true})
}

func readJournal(root *privatefs.Root) (journalStateV1, error) {
	encoded, err := readRegular(root, securityepoch.TransitionJournalFilename)
	if err != nil {
		return journalStateV1{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var state journalStateV1
	if decoder.Decode(&state) != nil || decoder.Decode(&struct{}{}) != io.EOF || !validPhase(state.Phase) || state.Version != journalVersion {
		return journalStateV1{}, ErrTransitionConflict
	}
	want, err := journalChecksum(state)
	if err != nil || want != state.Checksum {
		return journalStateV1{}, ErrTransitionConflict
	}
	canonicalBytes, err := json.Marshal(state)
	if err != nil || !bytes.Equal(canonicalBytes, encoded) {
		return journalStateV1{}, ErrTransitionConflict
	}
	if _, err := EncodePlan(state.Plan); err != nil {
		return journalStateV1{}, ErrTransitionConflict
	}
	return state, nil
}

func readRegular(root *privatefs.Root, name string) ([]byte, error) {
	file, err := root.OpenReadRegular(name)
	if err != nil {
		return nil, err
	}
	encoded, err := io.ReadAll(io.LimitReader(file, journalMaximum+1))
	closeErr := file.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if int64(len(encoded)) > journalMaximum {
		return nil, securityerr.ErrLimitExceeded
	}
	return encoded, nil
}
