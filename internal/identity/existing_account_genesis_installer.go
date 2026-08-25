package identity

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/aplexica/aplexica/internal/privatefs"
	"github.com/aplexica/aplexica/internal/securityepoch"
	"github.com/aplexica/aplexica/internal/securityerr"
)

const (
	existingAccountGenesisJournalVersion = uint16(1)
	existingAccountGenesisJournalDomain  = "aplexica/existing-account-genesis-journal/v1\x00"
	existingAccountGenesisJournalMax     = int64(2 << 20)
	existingAccountGenesisEpochMax       = int64(64 << 10)
	existingAccountGenesisChainFile      = "chain.cbor"
	existingAccountGenesisEpochFile      = "security-epoch.json"
)

var ErrExistingAccountGenesisConflict = errors.New("identity: existing-account genesis conflicts with durable state")

type existingAccountGenesisJournal struct {
	Version       uint16                            `json:"version"`
	Chain         ExistingAccountGenesisChainInputs `json:"chain"`
	SecurityEpoch GenesisSecurityEpochV1            `json:"securityEpoch"`
	Checksum      [32]byte                          `json:"checksum"`
}

type existingAccountGenesisInstallHooks struct {
	afterJournalDurable      func() error
	afterChainInstalled      func() error
	afterSecurityEpochStored func() error
	afterCoordinatorStored   func() error
}

// ExistingAccountGenesisInstaller installs or recovers the public, already
// signed result produced by BuildExistingAccountGenesis. Recover is designed
// to run before remote publisher/inbound startup; an unresolved journal keeps
// those paths fail-closed through securityepoch.Coordinator.
type ExistingAccountGenesisInstaller struct {
	IdentityRoot string
	Coordinator  *securityepoch.Coordinator
	hooks        existingAccountGenesisInstallHooks
}

// Install durably records the immutable signed package before making any
// authority visible, then rolls every participant forward to the exact package.
func (i *ExistingAccountGenesisInstaller) Install(ctx context.Context, result ExistingAccountGenesisResult) (VerifiedRoster, error) {
	if ctx == nil {
		return VerifiedRoster{}, fmt.Errorf("identity: genesis install context required")
	}
	record := existingAccountGenesisJournal{
		Version: existingAccountGenesisJournalVersion, Chain: result.Chain, SecurityEpoch: result.SecurityEpoch,
	}
	encoded, verified, err := encodeExistingAccountGenesisJournal(record)
	if err != nil {
		return VerifiedRoster{}, err
	}
	root, coordinator, err := i.open(ctx)
	if err != nil {
		return VerifiedRoster{}, err
	}
	defer root.Close()
	if err := ensureExactPrivateFile(root, securityepoch.TransitionJournalFilename, securityepoch.TransitionJournalFilename+".stage", encoded, existingAccountGenesisJournalMax); err != nil {
		return VerifiedRoster{}, err
	}
	if err := checkGenesisBoundary(ctx, i.hooks.afterJournalDurable); err != nil {
		return VerifiedRoster{}, err
	}
	persisted, persistedBytes, found, persistedVerified, err := readExistingAccountGenesisJournal(root)
	if err != nil {
		return VerifiedRoster{}, err
	}
	if !found || !bytes.Equal(persistedBytes, encoded) || persistedVerified.Hash != verified.Hash {
		return VerifiedRoster{}, ErrExistingAccountGenesisConflict
	}
	if err := i.apply(ctx, root, coordinator, persisted, persistedVerified); err != nil {
		return VerifiedRoster{}, err
	}
	return persistedVerified, nil
}

// Recover resumes a previously journaled genesis before remote startup. The
// bool reports whether a journal was present and successfully completed.
func (i *ExistingAccountGenesisInstaller) Recover(ctx context.Context) (VerifiedRoster, bool, error) {
	if ctx == nil {
		return VerifiedRoster{}, false, fmt.Errorf("identity: genesis recovery context required")
	}
	root, coordinator, err := i.open(ctx)
	if err != nil {
		return VerifiedRoster{}, false, err
	}
	defer root.Close()
	record, _, found, verified, err := readExistingAccountGenesisJournal(root)
	if err != nil || !found {
		return VerifiedRoster{}, false, err
	}
	if err := i.apply(ctx, root, coordinator, record, verified); err != nil {
		return VerifiedRoster{}, false, err
	}
	return verified, true, nil
}

func (i *ExistingAccountGenesisInstaller) open(ctx context.Context) (*privatefs.Root, *securityepoch.Coordinator, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if i == nil || !filepath.IsAbs(i.IdentityRoot) || filepath.Clean(i.IdentityRoot) != i.IdentityRoot {
		return nil, nil, fmt.Errorf("identity: absolute clean identity root required")
	}
	coordinator := i.Coordinator
	if coordinator == nil {
		coordinator = &securityepoch.Coordinator{Root: i.IdentityRoot}
	} else {
		coordinatorRoot, err := filepath.Abs(coordinator.Root)
		if err != nil || filepath.Clean(coordinatorRoot) != i.IdentityRoot {
			return nil, nil, fmt.Errorf("identity: genesis coordinator root mismatch")
		}
	}
	accountPath := filepath.Join(i.IdentityRoot, "account")
	root, err := privatefs.OpenRoot(accountPath, privatefs.DirPolicy{Access: privatefs.AccessPrivate, RepairOwned: true, AllowExisting: true})
	if err != nil {
		return nil, nil, err
	}
	return root, coordinator, nil
}

func (i *ExistingAccountGenesisInstaller) apply(ctx context.Context, root *privatefs.Root, coordinator *securityepoch.Coordinator, record existingAccountGenesisJournal, verified VerifiedRoster) error {
	next := coordinatorEpochFromGenesis(record.SecurityEpoch)
	err := coordinator.Transition(ctx, "account", next, func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		chainBytes, chainVerified, err := canonicalGenesisChain(record.Chain)
		if err != nil || chainVerified.Hash != verified.Hash {
			return ErrExistingAccountGenesisConflict
		}
		if err := ensureExactPrivateFile(root, existingAccountGenesisChainFile, existingAccountGenesisChainFile+".genesis-stage", chainBytes, chainStateMaxBytes); err != nil {
			return err
		}
		if err := checkGenesisBoundary(ctx, i.hooks.afterChainInstalled); err != nil {
			return err
		}
		epochBytes, err := json.Marshal(record.SecurityEpoch)
		if err != nil {
			return err
		}
		if err := ensureExactPrivateFile(root, existingAccountGenesisEpochFile, existingAccountGenesisEpochFile+".genesis-stage", epochBytes, existingAccountGenesisEpochMax); err != nil {
			return err
		}
		return checkGenesisBoundary(ctx, i.hooks.afterSecurityEpochStored)
	})
	if err != nil {
		return err
	}
	if err := checkGenesisBoundary(ctx, i.hooks.afterCoordinatorStored); err != nil {
		return err
	}
	// Re-prove every participant from its durable representation. Transition's
	// exact-generation retry runs the callback too, so this is safe after every
	// crash boundary and never accepts an in-memory-only success.
	if err := verifyInstalledGenesis(root, coordinator, record, verified); err != nil {
		return err
	}
	// RemoveRegular validates the journal object and fsyncs its containing
	// directory. A failed sync can only leave an exact journal to replay.
	if err := root.RemoveRegular(securityepoch.TransitionJournalFilename); err != nil {
		return err
	}
	return nil
}

func checkGenesisBoundary(ctx context.Context, hook func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if hook != nil {
		return hook()
	}
	return nil
}

func existingAccountGenesisJournalChecksum(record existingAccountGenesisJournal) ([32]byte, error) {
	record.Checksum = [32]byte{}
	encoded, err := json.Marshal(record)
	if err != nil {
		return [32]byte{}, err
	}
	hash := sha256.New()
	_, _ = hash.Write([]byte(existingAccountGenesisJournalDomain))
	_, _ = hash.Write(encoded)
	var checksum [32]byte
	copy(checksum[:], hash.Sum(nil))
	return checksum, nil
}

func encodeExistingAccountGenesisJournal(record existingAccountGenesisJournal) ([]byte, VerifiedRoster, error) {
	verified, err := validateExistingAccountGenesisJournal(record)
	if err != nil {
		return nil, VerifiedRoster{}, err
	}
	record.Checksum, err = existingAccountGenesisJournalChecksum(record)
	if err != nil {
		return nil, VerifiedRoster{}, err
	}
	encoded, err := json.Marshal(record)
	if err != nil || int64(len(encoded)) > existingAccountGenesisJournalMax {
		return nil, VerifiedRoster{}, securityerr.ErrLimitExceeded
	}
	return encoded, verified, nil
}

func validateExistingAccountGenesisJournal(record existingAccountGenesisJournal) (VerifiedRoster, error) {
	if record.Version != existingAccountGenesisJournalVersion || len(record.Chain.ExpectedRecoveryRoot) != ed25519.PublicKeySize {
		return VerifiedRoster{}, ErrExistingAccountGenesisConflict
	}
	chainBytes, verified, err := canonicalGenesisChain(record.Chain)
	if err != nil || len(chainBytes) == 0 {
		return VerifiedRoster{}, ErrExistingAccountGenesisConflict
	}
	authority := verified.Authority
	manifest := verified.Manifest.Manifest
	if authority.Threshold != 1 || len(authority.Anchor.Anchor.Authorities) != 1 || len(manifest.Devices) != 1 ||
		len(verified.Manifest.SignerKeyIDs) != 1 || len(verified.Manifest.Signatures) != 1 {
		return VerifiedRoster{}, ErrExistingAccountGenesisConflict
	}
	authorizer := authority.Anchor.Anchor.Authorities[0]
	credential := manifest.Devices[0]
	if authorizer.DeviceID != credential.Certificate.DeviceID || authorizer.SigningKeyID != credential.Certificate.SigningKeyID ||
		authorizer.SigningPublicKey != credential.Certificate.SigningPublicKey || verified.Manifest.SignerKeyIDs[0] != authorizer.SigningKeyID ||
		len(credential.IssuerKeyIDs) != 1 || len(credential.IssuanceSignatures) != 1 || credential.IssuerKeyIDs[0] != authorizer.SigningKeyID ||
		verifyExistingAccountGenesisCredential(credential.Certificate, authority) != nil {
		return VerifiedRoster{}, ErrExistingAccountGenesisConflict
	}
	if err := validateGenesisSecurityEpoch(record.SecurityEpoch, verified); err != nil {
		return VerifiedRoster{}, err
	}
	return verified, nil
}

func validateGenesisSecurityEpoch(epoch GenesisSecurityEpochV1, verified VerifiedRoster) error {
	manifest := verified.Manifest.Manifest
	if epoch.Version != 1 || epoch.ScopeType != "account" || epoch.ScopeID != verified.Authority.Anchor.Anchor.PersonalScopeID ||
		epoch.RosterHash != [32]byte(verified.Hash) || epoch.AccessGeneration != 1 || epoch.AccessGeneration != manifest.AccessGeneration ||
		epoch.AccessSetHash != manifest.AccessSetHash || epoch.BarrierID == ([32]byte{}) || epoch.TreeHeadDigest == ([32]byte{}) ||
		epoch.KeyMode != "recipient-wrap-v2" || epoch.KeyVersion != 0 || epoch.CoordinatorGeneration != 1 {
		return ErrExistingAccountGenesisConflict
	}
	return nil
}

func canonicalGenesisChain(chain ExistingAccountGenesisChainInputs) ([]byte, VerifiedRoster, error) {
	if len(chain.ExpectedRecoveryRoot) != ed25519.PublicKeySize {
		return nil, VerifiedRoster{}, ErrExistingAccountGenesisConflict
	}
	var expected [32]byte
	copy(expected[:], chain.ExpectedRecoveryRoot)
	state := chainState{
		Version: 1, ExpectedRecoveryRoot: expected, Anchor: chain.Anchor, Steps: []rosterStep{{Roster: chain.Roster}},
	}
	verified, err := verifyChain(state)
	if err != nil {
		return nil, VerifiedRoster{}, err
	}
	encoded, err := enc.Marshal(state)
	if err != nil || len(encoded) > chainStateMaxBytes {
		return nil, VerifiedRoster{}, securityerr.ErrLimitExceeded
	}
	return encoded, verified, nil
}

func coordinatorEpochFromGenesis(epoch GenesisSecurityEpochV1) securityepoch.SecurityEpoch {
	return securityepoch.SecurityEpoch{
		CoordinatorGeneration: epoch.CoordinatorGeneration, AccessGeneration: epoch.AccessGeneration,
		AccessSetHash: epoch.AccessSetHash, BarrierID: epoch.BarrierID, KeyMode: epoch.KeyMode, KeyVersion: epoch.KeyVersion,
	}
}

func readExistingAccountGenesisJournal(root *privatefs.Root) (existingAccountGenesisJournal, []byte, bool, VerifiedRoster, error) {
	encoded, found, err := readExactPrivateFile(root, securityepoch.TransitionJournalFilename, existingAccountGenesisJournalMax)
	if err != nil || !found {
		return existingAccountGenesisJournal{}, nil, found, VerifiedRoster{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var record existingAccountGenesisJournal
	if err := decoder.Decode(&record); err != nil {
		return existingAccountGenesisJournal{}, nil, true, VerifiedRoster{}, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return existingAccountGenesisJournal{}, nil, true, VerifiedRoster{}, ErrExistingAccountGenesisConflict
	}
	checksum, err := existingAccountGenesisJournalChecksum(record)
	if err != nil || checksum != record.Checksum {
		return existingAccountGenesisJournal{}, nil, true, VerifiedRoster{}, ErrExistingAccountGenesisConflict
	}
	verified, err := validateExistingAccountGenesisJournal(record)
	if err != nil {
		return existingAccountGenesisJournal{}, nil, true, VerifiedRoster{}, err
	}
	canonical, err := json.Marshal(record)
	if err != nil || !bytes.Equal(canonical, encoded) {
		return existingAccountGenesisJournal{}, nil, true, VerifiedRoster{}, ErrExistingAccountGenesisConflict
	}
	return record, encoded, true, verified, nil
}

func verifyInstalledGenesis(root *privatefs.Root, coordinator *securityepoch.Coordinator, record existingAccountGenesisJournal, verified VerifiedRoster) error {
	chainBytes, chainVerified, err := canonicalGenesisChain(record.Chain)
	if err != nil || chainVerified.Hash != verified.Hash {
		return ErrExistingAccountGenesisConflict
	}
	if err := requireExactPrivateFile(root, existingAccountGenesisChainFile, chainBytes, chainStateMaxBytes); err != nil {
		return err
	}
	epochBytes, err := json.Marshal(record.SecurityEpoch)
	if err != nil {
		return err
	}
	if err := requireExactPrivateFile(root, existingAccountGenesisEpochFile, epochBytes, existingAccountGenesisEpochMax); err != nil {
		return err
	}
	return coordinator.VerifyCurrent("account", coordinatorEpochFromGenesis(record.SecurityEpoch))
}

func ensureExactPrivateFile(root *privatefs.Root, final, stage string, expected []byte, maximum int64) error {
	if int64(len(expected)) == 0 || int64(len(expected)) > maximum {
		return securityerr.ErrLimitExceeded
	}
	if err := requireExactPrivateFile(root, final, expected, maximum); err == nil {
		return removeExactStage(root, stage, expected, maximum)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	stageBytes, stageExists, err := readExactPrivateFile(root, stage, maximum)
	if err != nil {
		return err
	}
	if stageExists {
		if !bytes.Equal(stageBytes, expected) {
			return ErrExistingAccountGenesisConflict
		}
	} else if err := createExactStage(root, stage, expected, maximum); err != nil {
		return err
	}
	if err := root.InstallNoReplace(stage, final); err != nil {
		if !errors.Is(err, fs.ErrExist) {
			return err
		}
		if err := requireExactPrivateFile(root, final, expected, maximum); err != nil {
			return err
		}
		return removeExactStage(root, stage, expected, maximum)
	}
	return requireExactPrivateFile(root, final, expected, maximum)
}

func createExactStage(root *privatefs.Root, stage string, expected []byte, maximum int64) error {
	file, temporary, err := root.CreateTemp(".", ".existing-genesis-write-")
	if err != nil {
		return err
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = file.Close()
			_ = root.RemoveRegular(temporary)
		}
	}()
	written, err := io.Copy(file, io.LimitReader(bytes.NewReader(expected), maximum+1))
	if err == nil && written != int64(len(expected)) {
		err = io.ErrShortWrite
	}
	if err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err := root.InstallNoReplace(temporary, stage); err != nil {
		if !errors.Is(err, fs.ErrExist) {
			return err
		}
		if err := requireExactPrivateFile(root, stage, expected, maximum); err != nil {
			return err
		}
		if err := root.RemoveRegular(temporary); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	cleanup = false
	return nil
}

func removeExactStage(root *privatefs.Root, stage string, expected []byte, maximum int64) error {
	actual, found, err := readExactPrivateFile(root, stage, maximum)
	if err != nil || !found {
		return err
	}
	if !bytes.Equal(actual, expected) {
		return ErrExistingAccountGenesisConflict
	}
	if err := root.RemoveRegular(stage); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func requireExactPrivateFile(root *privatefs.Root, name string, expected []byte, maximum int64) error {
	actual, found, err := readExactPrivateFile(root, name, maximum)
	if err != nil {
		return err
	}
	if !found {
		return os.ErrNotExist
	}
	if !bytes.Equal(actual, expected) {
		return ErrExistingAccountGenesisConflict
	}
	return nil
}

func readExactPrivateFile(root *privatefs.Root, name string, maximum int64) ([]byte, bool, error) {
	file, err := root.OpenReadRegular(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	encoded, readErr := io.ReadAll(io.LimitReader(file, maximum+1))
	closeErr := file.Close()
	if readErr != nil {
		return nil, true, readErr
	}
	if closeErr != nil {
		return nil, true, closeErr
	}
	if int64(len(encoded)) > maximum {
		return nil, true, securityerr.ErrLimitExceeded
	}
	return encoded, true, nil
}
