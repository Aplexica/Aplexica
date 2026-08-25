// Package truststate persists the fail-closed monotonic authorization for the
// configured remote plugin.  Signed release artifacts establish authenticity;
// this private checkpoint establishes freshness across installs and restarts.
package truststate

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"github.com/aplexica/aplexica/internal/plugin/proto"
	"github.com/aplexica/aplexica/internal/privatefs"
	"github.com/fxamacker/cbor/v2"
)

const (
	checkpointFilename = "checkpoint.cbor"
	lockFilename       = "checkpoint.lock"
	checkpointDomain   = "aplexica/remote-plugin-checkpoint/v1"
	maxCheckpointBytes = 64 << 10
)

var ErrCheckpointMissing = errors.New("remote plugin trust checkpoint is missing")

// LegacyIdentity is one exact, finite v1 overlap artifact.  The retirement
// daemon ships an empty legacy set and therefore rejects even a checkpointed
// v1 plugin.
type LegacyIdentity struct {
	GOOS               string
	GOARCH             string
	PluginVersion      string
	BinarySHA256       [32]byte
	ManifestSHA256     [32]byte
	PublisherKeySHA256 [32]byte
}

type Policy struct {
	AllowLegacyV1 bool
	LegacyV1      []LegacyIdentity
	// V2Publishers is a finite set of provider-neutral publisher roots. During
	// overlap the legacy root may still be in the manifest verifier key ring,
	// but it is never permitted to authorize a v2 sequence.
	V2Publishers [][32]byte
}

type Bootstrap struct {
	// LegacyMigration must be explicitly set for the one-time v1 overlap.
	LegacyMigration bool
	// A first v2 acceptance requires all three values from an independent,
	// out-of-band release announcement.  They are ignored only after a v2
	// checkpoint already exists.
	Sequence        uint64
	RollbackFloor   uint64
	InventorySHA256 [32]byte
}

type Checkpoint struct {
	Schema             string   `cbor:"schema"`
	Executable         string   `cbor:"executable"`
	ManifestVersion    uint16   `cbor:"manifestVersion"`
	PluginVersion      string   `cbor:"pluginVersion"`
	Sequence           uint64   `cbor:"sequence"`
	RollbackFloor      uint64   `cbor:"rollbackFloor"`
	BinarySHA256       [32]byte `cbor:"binarySha256"`
	ManifestSHA256     [32]byte `cbor:"manifestSha256"`
	InventorySHA256    [32]byte `cbor:"inventorySha256"`
	PublisherKeySHA256 [32]byte `cbor:"publisherKeySha256"`
}

type checkpointEnvelope struct {
	Checkpoint Checkpoint `cbor:"checkpoint"`
	Checksum   [32]byte   `cbor:"checksum"`
}

type Store struct{ Root string }

func (s Store) Accept(executable string, verified proto.VerifiedRemotePlugin, policy Policy, bootstrap Bootstrap) (Checkpoint, error) {
	executable, err := validateExecutablePath(executable)
	if err != nil {
		return Checkpoint{}, err
	}
	if err := s.ensureRoot(); err != nil {
		return Checkpoint{}, err
	}
	lock, err := acquireCheckpointLock(s.Root)
	if err != nil {
		return Checkpoint{}, fmt.Errorf("remote plugin checkpoint lock: %w", err)
	}
	defer func() { _ = lock.Close() }()
	current, loadErr := loadLocked(lock.root)
	if loadErr != nil && !errors.Is(loadErr, ErrCheckpointMissing) {
		return Checkpoint{}, loadErr
	}
	next, err := authorizeTransition(executable, verified, policy, bootstrap, current, loadErr == nil)
	if err != nil {
		return Checkpoint{}, err
	}
	if err := writeLocked(lock.root, next); err != nil {
		return Checkpoint{}, err
	}
	return next, nil
}

// VerifyCurrent is called before every daemon spawn and pairing/status
// subprocess.  Missing, corrupt, retired, substituted, or stale state is an
// error; it never creates or repairs authority state.
func (s Store) VerifyCurrent(executable string, verified proto.VerifiedRemotePlugin, policy Policy) (Checkpoint, error) {
	executable, err := validateExecutablePath(executable)
	if err != nil {
		return Checkpoint{}, err
	}
	if err := privatefs.ValidateDir(s.Root); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Checkpoint{}, ErrCheckpointMissing
		}
		return Checkpoint{}, fmt.Errorf("validate remote plugin checkpoint root: %w", err)
	}
	lock, err := acquireCheckpointLock(s.Root)
	if err != nil {
		return Checkpoint{}, fmt.Errorf("remote plugin checkpoint lock: %w", err)
	}
	defer func() { _ = lock.Close() }()
	checkpoint, err := loadLocked(lock.root)
	if err != nil {
		return Checkpoint{}, err
	}
	if err := authorizeRuntime(executable, verified, policy, checkpoint); err != nil {
		return Checkpoint{}, err
	}
	return checkpoint, nil
}

func authorizeTransition(executable string, verified proto.VerifiedRemotePlugin, policy Policy, bootstrap Bootstrap, current Checkpoint, exists bool) (Checkpoint, error) {
	if verified.Manifest.Version == 1 {
		if !bootstrap.LegacyMigration {
			return Checkpoint{}, errors.New("legacy plugin migration requires explicit overlap authorization")
		}
		if bootstrap.Sequence != 0 || bootstrap.RollbackFloor != 0 || bootstrap.InventorySHA256 != ([32]byte{}) {
			return Checkpoint{}, errors.New("legacy migration cannot claim a v2 bootstrap identity")
		}
		if !legacyAllowed(verified, policy) {
			return Checkpoint{}, errors.New("legacy plugin is not an exact compiled overlap artifact")
		}
		next := checkpointFromVerification(executable, verified)
		if exists && !sameArtifact(current, next, false) {
			return Checkpoint{}, errors.New("legacy checkpoint cannot be replaced or equivocated")
		}
		return next, nil
	}
	if verified.Manifest.Version != 2 || verified.InventorySHA256 == ([32]byte{}) {
		return Checkpoint{}, errors.New("remote plugin lacks v2 signed inventory authorization")
	}
	if !v2PublisherAllowed(verified.PublisherKeySHA256, policy) {
		return Checkpoint{}, errors.New("remote plugin v2 publisher is not an authorized provider-neutral root")
	}
	next := checkpointFromVerification(executable, verified)
	if !exists || current.ManifestVersion == 1 {
		if bootstrap.LegacyMigration || bootstrap.Sequence == 0 || bootstrap.RollbackFloor == 0 || bootstrap.InventorySHA256 == ([32]byte{}) ||
			bootstrap.Sequence != next.Sequence || bootstrap.RollbackFloor != next.RollbackFloor || bootstrap.InventorySHA256 != next.InventorySHA256 {
			return Checkpoint{}, errors.New("first v2 install requires exact out-of-band sequence, rollback floor, and inventory digest")
		}
		return next, nil
	}
	if bootstrap != (Bootstrap{}) {
		return Checkpoint{}, errors.New("bootstrap authorization is valid only for the first v2 install")
	}
	if current.ManifestVersion != 2 {
		return Checkpoint{}, errors.New("unsupported checkpoint manifest version")
	}
	if next.Sequence == current.Sequence {
		if !sameArtifact(current, next, false) {
			return Checkpoint{}, errors.New("same-sequence remote plugin equivocation rejected")
		}
		return next, nil // exact reinstall may intentionally move the path.
	}
	if next.Sequence != current.Sequence+1 || next.RollbackFloor < current.RollbackFloor || verified.Manifest.Previous == nil ||
		verified.Manifest.Previous.Sequence != current.Sequence || verified.Manifest.Previous.InventorySHA256 != current.InventorySHA256 ||
		verified.Manifest.Previous.PluginVersion != current.PluginVersion {
		return Checkpoint{}, errors.New("remote plugin upgrade is not the exact monotonic successor")
	}
	return next, nil
}

func authorizeRuntime(executable string, verified proto.VerifiedRemotePlugin, policy Policy, current Checkpoint) error {
	now := checkpointFromVerification(executable, verified)
	if verified.Manifest.Version == 1 && !legacyAllowed(verified, policy) {
		return errors.New("legacy remote plugin overlap has been retired")
	}
	if verified.Manifest.Version == 2 && !v2PublisherAllowed(verified.PublisherKeySHA256, policy) {
		return errors.New("remote plugin v2 publisher is not an authorized provider-neutral root")
	}
	if !sameArtifact(current, now, true) {
		return errors.New("configured remote plugin differs from the durable accepted checkpoint")
	}
	return nil
}

func v2PublisherAllowed(publisher [32]byte, policy Policy) bool {
	for _, allowed := range policy.V2Publishers {
		if allowed == publisher && allowed != ([32]byte{}) {
			return true
		}
	}
	return false
}

func checkpointFromVerification(executable string, verified proto.VerifiedRemotePlugin) Checkpoint {
	return Checkpoint{Schema: checkpointDomain, Executable: executable, ManifestVersion: verified.Manifest.Version,
		PluginVersion: verified.Manifest.PluginVersion, Sequence: verified.Manifest.Sequence, RollbackFloor: verified.Manifest.RollbackFloor,
		BinarySHA256: verified.Manifest.BinarySHA256, ManifestSHA256: verified.ManifestSHA256, InventorySHA256: verified.InventorySHA256,
		PublisherKeySHA256: verified.PublisherKeySHA256}
}

func sameArtifact(left, right Checkpoint, includePath bool) bool {
	if left.Schema != right.Schema || left.ManifestVersion != right.ManifestVersion || left.PluginVersion != right.PluginVersion ||
		left.Sequence != right.Sequence || left.RollbackFloor != right.RollbackFloor || left.BinarySHA256 != right.BinarySHA256 ||
		left.ManifestSHA256 != right.ManifestSHA256 || left.InventorySHA256 != right.InventorySHA256 || left.PublisherKeySHA256 != right.PublisherKeySHA256 {
		return false
	}
	return !includePath || left.Executable == right.Executable
}

func legacyAllowed(verified proto.VerifiedRemotePlugin, policy Policy) bool {
	if !policy.AllowLegacyV1 || verified.Manifest.Version != 1 {
		return false
	}
	for _, allowed := range policy.LegacyV1 {
		if allowed.GOOS == runtime.GOOS && allowed.GOARCH == runtime.GOARCH && allowed.PluginVersion == verified.Manifest.PluginVersion &&
			allowed.BinarySHA256 == verified.Manifest.BinarySHA256 && allowed.ManifestSHA256 == verified.ManifestSHA256 &&
			allowed.PublisherKeySHA256 == verified.PublisherKeySHA256 {
			return true
		}
	}
	return false
}

func (s Store) ensureRoot() error {
	if !filepath.IsAbs(s.Root) || filepath.Clean(s.Root) != s.Root {
		return errors.New("remote plugin checkpoint root must be an absolute clean path")
	}
	return privatefs.EnsureDir(s.Root, privatefs.DirPolicy{Access: privatefs.AccessPrivate, AllowExisting: true})
}

func validateExecutablePath(path string) (string, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", errors.New("remote plugin executable must be an absolute clean path")
	}
	return path, nil
}

func loadLocked(root *privatefs.Root) (Checkpoint, error) {
	f, err := root.OpenReadRegular(checkpointFilename)
	if errors.Is(err, os.ErrNotExist) {
		return Checkpoint{}, ErrCheckpointMissing
	}
	if err != nil {
		return Checkpoint{}, fmt.Errorf("open remote plugin checkpoint: %w", err)
	}
	defer func() { _ = f.Close() }()
	raw, err := io.ReadAll(io.LimitReader(f, maxCheckpointBytes+1))
	if err != nil || len(raw) == 0 || len(raw) > maxCheckpointBytes {
		return Checkpoint{}, errors.New("remote plugin checkpoint has invalid size")
	}
	return decodeCheckpoint(raw)
}

func writeLocked(root *privatefs.Root, checkpoint Checkpoint) error {
	raw, err := encodeCheckpoint(checkpoint)
	if err != nil {
		return err
	}
	if _, _, err := root.WriteReader(checkpointFilename, bytes.NewReader(raw), maxCheckpointBytes,
		privatefs.FilePolicy{RequirePrivateParent: true, RejectWritableByOthers: true}); err != nil {
		return fmt.Errorf("persist remote plugin checkpoint: %w", err)
	}
	return nil
}

func encodeCheckpoint(checkpoint Checkpoint) ([]byte, error) {
	if err := validateCheckpoint(checkpoint); err != nil {
		return nil, err
	}
	enc, err := cbor.CanonicalEncOptions().EncMode()
	if err != nil {
		return nil, err
	}
	body, err := enc.Marshal([]any{checkpointDomain, checkpoint})
	if err != nil {
		return nil, err
	}
	return enc.Marshal(checkpointEnvelope{Checkpoint: checkpoint, Checksum: sha256.Sum256(body)})
}

func decodeCheckpoint(raw []byte) (Checkpoint, error) {
	dec, err := cbor.DecOptions{DupMapKey: cbor.DupMapKeyEnforcedAPF, IndefLength: cbor.IndefLengthForbidden, TagsMd: cbor.TagsForbidden, MaxNestedLevels: 8, MaxArrayElements: 16, MaxMapPairs: 32, ExtraReturnErrors: cbor.ExtraDecErrorUnknownField}.DecMode()
	if err != nil {
		return Checkpoint{}, err
	}
	enc, err := cbor.CanonicalEncOptions().EncMode()
	if err != nil {
		return Checkpoint{}, err
	}
	var envelope checkpointEnvelope
	if err := dec.Unmarshal(raw, &envelope); err != nil {
		return Checkpoint{}, fmt.Errorf("decode remote plugin checkpoint: %w", err)
	}
	canonical, err := enc.Marshal(envelope)
	if err != nil || !bytes.Equal(canonical, raw) {
		return Checkpoint{}, errors.New("remote plugin checkpoint is not canonical")
	}
	if err := validateCheckpoint(envelope.Checkpoint); err != nil {
		return Checkpoint{}, err
	}
	body, err := enc.Marshal([]any{checkpointDomain, envelope.Checkpoint})
	if err != nil || sha256.Sum256(body) != envelope.Checksum {
		return Checkpoint{}, errors.New("remote plugin checkpoint checksum mismatch")
	}
	return envelope.Checkpoint, nil
}

func validateCheckpoint(checkpoint Checkpoint) error {
	if checkpoint.Schema != checkpointDomain || checkpoint.Executable == "" || !filepath.IsAbs(checkpoint.Executable) || filepath.Clean(checkpoint.Executable) != checkpoint.Executable ||
		checkpoint.PluginVersion == "" || checkpoint.BinarySHA256 == ([32]byte{}) || checkpoint.ManifestSHA256 == ([32]byte{}) || checkpoint.PublisherKeySHA256 == ([32]byte{}) {
		return errors.New("invalid remote plugin checkpoint identity")
	}
	switch checkpoint.ManifestVersion {
	case 1:
		if checkpoint.Sequence != 0 || checkpoint.RollbackFloor != 0 || checkpoint.InventorySHA256 != ([32]byte{}) {
			return errors.New("legacy remote plugin checkpoint carries v2 state")
		}
	case 2:
		if checkpoint.Sequence == 0 || checkpoint.RollbackFloor == 0 || checkpoint.RollbackFloor > checkpoint.Sequence || checkpoint.InventorySHA256 == ([32]byte{}) {
			return errors.New("invalid v2 remote plugin checkpoint")
		}
	default:
		return errors.New("unsupported remote plugin checkpoint version")
	}
	return nil
}
