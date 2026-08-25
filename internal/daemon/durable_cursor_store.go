package daemon

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/aplexica/aplexica/internal/filelock"
	"github.com/aplexica/aplexica/internal/privatefs"
)

const (
	durableCursorStoreVersion = uint16(1)
	durableCursorStoreDomain  = "aplexica/durable-remote-cursor/v1\x00"
	durableCursorLockName     = ".durable-cursors.lock"
	durableCursorLockTimeout  = 10 * time.Second
	durableCursorMaxRecord    = int64(16 << 10)
	durableCursorMaxIdentity  = 512
	durableCursorMaxToken     = 4096
	durableCursorMaxDigest    = 512
	durableCursorRecordPrefix = "cursor-"
	durableCursorRecordSuffix = ".json"
	durableCursorPrintableMin = '\x21'
	durableCursorPrintableMax = '\x7e'
	durableCursorUint64Bytes  = 8
)

var (
	// ErrDurableCursorNotFound means this exact remote/stream/epoch has never
	// durably advanced. A corrupt or inaccessible record never maps to this
	// error, so callers cannot mistake damaged state for a new stream.
	ErrDurableCursorNotFound = errors.New("daemon: durable cursor not found")

	// ErrDurableCursorConflict means the current record did not match the
	// caller's compare-and-swap expectation. The caller must reload and prove
	// contiguity again; it must not retry as an unconditional overwrite.
	ErrDurableCursorConflict = errors.New("daemon: durable cursor compare-and-swap conflict")

	// ErrDurableCursorCorrupt identifies retained cursor bytes that cannot be
	// authenticated as the canonical, checksummed v1 record for their key.
	// Advancing fails closed until the record is recovered or repaired from a
	// completed durable inbox delivery.
	ErrDurableCursorCorrupt = errors.New("daemon: durable cursor state is corrupt")

	// ErrDurableCursorInvalid identifies invalid local API input. Cursor fields
	// are opaque transport metadata, but must remain bounded and content-free.
	ErrDurableCursorInvalid = errors.New("daemon: invalid durable cursor state")
)

// DurableCursorKey isolates canonical delivery state by stable authenticated
// remote identity, cloud stream, and stream epoch. RemoteIdentity must describe
// the remote service/account identity rather than a plugin executable path: a
// plugin replacement must continue from the same daemon-owned cursor.
//
// The three values are hashed before being used as a filename or retained
// identity. Their plaintext values are never written by this store.
type DurableCursorKey struct {
	RemoteIdentity string
	StreamID       string
	StreamEpoch    string
}

// DurableCursorState is the last contiguous cloud cursor that the daemon has
// durably applied. CursorDigest is the lowercase-hex SHA-256 of the exact
// opaque Cursor token; the token itself authenticates the fetched index entry.
// Position is the authenticated server position associated with that cursor.
// A zero Position is retained only for compatibility with the first additive
// cursor-store slice; it cannot transition into the positioned chain. Revision
// is a local monotonic CAS generation and has no cloud ordering authority.
type DurableCursorState struct {
	Cursor       string
	CursorDigest string
	Position     uint64
	Revision     uint64
}

// DurableCheckpointSeed is the narrow exception to positioned genesis. It is
// accepted only after the daemon has authenticated and durably imported one
// exact checkpoint event. The checkpoint must explicitly cover the cursor
// position being seeded; ordinary deliveries still begin at position one and
// advance by exact +1 CAS transitions.
type DurableCheckpointSeed struct {
	Cursor                  string
	CursorDigest            string
	Position                uint64
	CheckpointEventID       string
	CheckpointEventHash     string
	CheckpointAlignmentHash string
	CheckpointGeneration    string
	CheckpointPosition      uint64
	CheckpointCoverage      uint64
}

// DurableCursorStore owns private daemon cursor state. Its per-instance mutex
// avoids needless lock-file contention; a private cross-process lock preserves
// CAS semantics when multiple store instances or restart repair overlap.
type DurableCursorStore struct {
	Root string
	mu   sync.Mutex
}

type durableCursorRecord struct {
	Version                          uint16 `json:"version"`
	KeyDigest                        string `json:"key_digest"`
	Cursor                           string `json:"cursor"`
	CursorDigest                     string `json:"cursor_digest"`
	Position                         uint64 `json:"position,omitempty"`
	Revision                         uint64 `json:"revision"`
	BootstrapCheckpointEventID       string `json:"bootstrap_checkpoint_event_id,omitempty"`
	BootstrapCheckpointEventHash     string `json:"bootstrap_checkpoint_event_hash,omitempty"`
	BootstrapCheckpointAlignmentHash string `json:"bootstrap_checkpoint_alignment_hash,omitempty"`
	BootstrapCheckpointGeneration    string `json:"bootstrap_checkpoint_generation,omitempty"`
	BootstrapCheckpointPosition      uint64 `json:"bootstrap_checkpoint_position,omitempty"`
	BootstrapCheckpointCoverage      uint64 `json:"bootstrap_checkpoint_coverage,omitempty"`
	BootstrapCursor                  string `json:"bootstrap_cursor,omitempty"`
	BootstrapCursorDigest            string `json:"bootstrap_cursor_digest,omitempty"`
	Checksum                         string `json:"checksum"`
}

func validDurableCursorOpaque(value string, maximum int) bool {
	if value == "" || len(value) > maximum {
		return false
	}
	for _, r := range value {
		// Durable protocol identifiers and tokens are printable ASCII. Rejecting
		// whitespace/control characters also prevents accidental user-visible
		// content or structured records from becoming cursor metadata.
		if r < durableCursorPrintableMin || r > durableCursorPrintableMax {
			return false
		}
	}
	return true
}

func validateDurableCursorKey(key DurableCursorKey) error {
	if !validDurableCursorOpaque(key.RemoteIdentity, durableCursorMaxIdentity) ||
		!validDurableCursorOpaque(key.StreamID, durableCursorMaxIdentity) ||
		!validDurableCursorOpaque(key.StreamEpoch, durableCursorMaxIdentity) {
		return ErrDurableCursorInvalid
	}
	return nil
}

func validateDurableCursorValue(state DurableCursorState, persisted bool) error {
	if !validDurableCursorOpaque(state.Cursor, durableCursorMaxToken) {
		return ErrDurableCursorInvalid
	}
	if state.Position == 0 {
		// Version-one records written before authenticated stream positions
		// carried an opaque fetched-index digest. Keep them readable so an
		// upgrade fails closed at the position-zero transition instead of
		// misclassifying valid local state as corruption.
		if !validDurableCursorOpaque(state.CursorDigest, durableCursorMaxDigest) {
			return ErrDurableCursorInvalid
		}
	} else {
		if !validLowerHexDigest(state.CursorDigest) {
			return ErrDurableCursorInvalid
		}
		digest := sha256.Sum256([]byte(state.Cursor))
		if state.CursorDigest != hex.EncodeToString(digest[:]) {
			return ErrDurableCursorInvalid
		}
	}
	if persisted != (state.Revision > 0) {
		return ErrDurableCursorInvalid
	}
	return nil
}

func validateDurableCheckpointSeed(seed DurableCheckpointSeed) error {
	state := DurableCursorState{Cursor: seed.Cursor, CursorDigest: seed.CursorDigest, Position: seed.Position}
	if validateDurableCursorValue(state, false) != nil || seed.Position == 0 ||
		!validDurableCursorOpaque(seed.CheckpointEventID, durableCursorMaxIdentity) ||
		!validLowerHexDigest(seed.CheckpointEventHash) || !validLowerHexDigest(seed.CheckpointAlignmentHash) ||
		!validLowerHexDigest(seed.CheckpointGeneration) ||
		seed.CheckpointCoverage != seed.Position || seed.CheckpointPosition <= seed.CheckpointCoverage {
		return ErrDurableCursorInvalid
	}
	return nil
}

func durableCursorRecordSeed(record durableCursorRecord) (DurableCheckpointSeed, bool) {
	present := record.BootstrapCheckpointEventID != "" || record.BootstrapCheckpointEventHash != "" ||
		record.BootstrapCheckpointAlignmentHash != "" ||
		record.BootstrapCheckpointGeneration != "" || record.BootstrapCheckpointPosition != 0 || record.BootstrapCheckpointCoverage != 0 ||
		record.BootstrapCursor != "" || record.BootstrapCursorDigest != ""
	if !present {
		return DurableCheckpointSeed{}, false
	}
	return DurableCheckpointSeed{
		Cursor: record.BootstrapCursor, CursorDigest: record.BootstrapCursorDigest, Position: record.BootstrapCheckpointCoverage,
		CheckpointEventID: record.BootstrapCheckpointEventID, CheckpointEventHash: record.BootstrapCheckpointEventHash,
		CheckpointAlignmentHash: record.BootstrapCheckpointAlignmentHash,
		CheckpointGeneration:    record.BootstrapCheckpointGeneration, CheckpointPosition: record.BootstrapCheckpointPosition,
		CheckpointCoverage: record.BootstrapCheckpointCoverage,
	}, true
}

func applyDurableCursorSeed(record *durableCursorRecord, seed DurableCheckpointSeed) {
	record.BootstrapCheckpointEventID = seed.CheckpointEventID
	record.BootstrapCheckpointEventHash = seed.CheckpointEventHash
	record.BootstrapCheckpointAlignmentHash = seed.CheckpointAlignmentHash
	record.BootstrapCheckpointGeneration = seed.CheckpointGeneration
	record.BootstrapCheckpointPosition = seed.CheckpointPosition
	record.BootstrapCheckpointCoverage = seed.CheckpointCoverage
	record.BootstrapCursor = seed.Cursor
	record.BootstrapCursorDigest = seed.CursorDigest
}

func carryDurableCursorSeed(record *durableCursorRecord, previous durableCursorRecord) {
	record.BootstrapCheckpointEventID = previous.BootstrapCheckpointEventID
	record.BootstrapCheckpointEventHash = previous.BootstrapCheckpointEventHash
	record.BootstrapCheckpointAlignmentHash = previous.BootstrapCheckpointAlignmentHash
	record.BootstrapCheckpointGeneration = previous.BootstrapCheckpointGeneration
	record.BootstrapCheckpointPosition = previous.BootstrapCheckpointPosition
	record.BootstrapCheckpointCoverage = previous.BootstrapCheckpointCoverage
	record.BootstrapCursor = previous.BootstrapCursor
	record.BootstrapCursorDigest = previous.BootstrapCursorDigest
}

func durableCursorKeyDigest(key DurableCursorKey) (string, error) {
	if err := validateDurableCursorKey(key); err != nil {
		return "", err
	}
	h := sha256.New()
	_, _ = h.Write([]byte("aplexica/durable-remote-cursor-key/v1\x00"))
	for _, value := range []string{key.RemoteIdentity, key.StreamID, key.StreamEpoch} {
		var size [durableCursorUint64Bytes]byte
		binary.BigEndian.PutUint64(size[:], uint64(len(value)))
		_, _ = h.Write(size[:])
		_, _ = h.Write([]byte(value))
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func durableCursorRecordName(keyDigest string) string {
	return durableCursorRecordPrefix + keyDigest + durableCursorRecordSuffix
}

func durableCursorChecksum(record durableCursorRecord) (string, error) {
	record.Checksum = ""
	encoded, err := json.Marshal(record)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(append([]byte(durableCursorStoreDomain), encoded...))
	return hex.EncodeToString(sum[:]), nil
}

func validLowerHexDigest(value string) bool {
	if len(value) != sha256.Size*2 || value != strings.ToLower(value) {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}

func encodeDurableCursorRecord(record durableCursorRecord) ([]byte, error) {
	state := DurableCursorState{Cursor: record.Cursor, CursorDigest: record.CursorDigest, Position: record.Position, Revision: record.Revision}
	if record.Version != durableCursorStoreVersion || !validLowerHexDigest(record.KeyDigest) || validateDurableCursorValue(state, true) != nil {
		return nil, ErrDurableCursorInvalid
	}
	if seed, present := durableCursorRecordSeed(record); present {
		if validateDurableCheckpointSeed(seed) != nil || seed.CheckpointCoverage > record.Position {
			return nil, ErrDurableCursorInvalid
		}
	}
	checksum, err := durableCursorChecksum(record)
	if err != nil {
		return nil, err
	}
	record.Checksum = checksum
	encoded, err := json.Marshal(record)
	if err != nil || int64(len(encoded)) > durableCursorMaxRecord {
		return nil, ErrDurableCursorInvalid
	}
	return encoded, nil
}

func decodeDurableCursorRecord(encoded []byte, keyDigest string) (durableCursorRecord, error) {
	if len(encoded) == 0 || int64(len(encoded)) > durableCursorMaxRecord {
		return durableCursorRecord{}, ErrDurableCursorCorrupt
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var record durableCursorRecord
	if err := decoder.Decode(&record); err != nil {
		return durableCursorRecord{}, fmt.Errorf("%w: decode", ErrDurableCursorCorrupt)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return durableCursorRecord{}, fmt.Errorf("%w: trailing bytes", ErrDurableCursorCorrupt)
	}
	canonical, err := json.Marshal(record)
	if err != nil || !bytes.Equal(canonical, encoded) {
		return durableCursorRecord{}, fmt.Errorf("%w: non-canonical record", ErrDurableCursorCorrupt)
	}
	state := DurableCursorState{Cursor: record.Cursor, CursorDigest: record.CursorDigest, Position: record.Position, Revision: record.Revision}
	if record.Version != durableCursorStoreVersion || record.KeyDigest != keyDigest || !validLowerHexDigest(record.KeyDigest) || validateDurableCursorValue(state, true) != nil {
		return durableCursorRecord{}, fmt.Errorf("%w: invalid metadata", ErrDurableCursorCorrupt)
	}
	if seed, present := durableCursorRecordSeed(record); present {
		if validateDurableCheckpointSeed(seed) != nil || seed.CheckpointCoverage > record.Position {
			return durableCursorRecord{}, fmt.Errorf("%w: invalid checkpoint seed", ErrDurableCursorCorrupt)
		}
	}
	want, err := durableCursorChecksum(record)
	if err != nil || !validLowerHexDigest(record.Checksum) || record.Checksum != want {
		return durableCursorRecord{}, fmt.Errorf("%w: checksum mismatch", ErrDurableCursorCorrupt)
	}
	return record, nil
}

func (s *DurableCursorStore) ensureRoot() error {
	if s == nil || !filepath.IsAbs(s.Root) || filepath.Clean(s.Root) != s.Root {
		return ErrDurableCursorInvalid
	}
	if err := privatefs.EnsureDir(s.Root, privatefs.DirPolicy{Access: privatefs.AccessPrivate, RepairOwned: true, AllowExisting: true}); err != nil {
		return fmt.Errorf("daemon: secure durable cursor root: %w", err)
	}
	return nil
}

func (s *DurableCursorStore) openLockedRoot() (*privatefs.Root, *filelock.Lock, error) {
	if err := s.ensureRoot(); err != nil {
		return nil, nil, err
	}
	lock, err := filelock.Acquire(filepath.Join(s.Root, durableCursorLockName), durableCursorLockTimeout)
	if err != nil {
		return nil, nil, fmt.Errorf("daemon: lock durable cursor store: %w", err)
	}
	root, err := privatefs.OpenRoot(s.Root, privatefs.DirPolicy{Access: privatefs.AccessPrivate, RepairOwned: true})
	if err != nil {
		_ = lock.Close()
		return nil, nil, fmt.Errorf("daemon: open durable cursor root: %w", err)
	}
	return root, lock, nil
}

func readDurableCursorRecord(root *privatefs.Root, name, keyDigest string) (durableCursorRecord, error) {
	f, err := root.OpenReadRegular(name)
	if errors.Is(err, os.ErrNotExist) {
		return durableCursorRecord{}, ErrDurableCursorNotFound
	}
	if err != nil {
		return durableCursorRecord{}, fmt.Errorf("%w: open record", ErrDurableCursorCorrupt)
	}
	encoded, readErr := io.ReadAll(io.LimitReader(f, durableCursorMaxRecord+1))
	closeErr := f.Close()
	if readErr != nil || closeErr != nil || int64(len(encoded)) > durableCursorMaxRecord {
		return durableCursorRecord{}, fmt.Errorf("%w: read record", ErrDurableCursorCorrupt)
	}
	return decodeDurableCursorRecord(encoded, keyDigest)
}

func writeDurableCursorRecord(root *privatefs.Root, name string, record durableCursorRecord) error {
	encoded, err := encodeDurableCursorRecord(record)
	if err != nil {
		return err
	}
	if err := root.WriteFile(name, encoded, privatefs.FilePolicy{RequirePrivateParent: true, RejectWritableByOthers: true}); err != nil {
		return fmt.Errorf("daemon: persist durable cursor: %w", err)
	}
	return nil
}

func durableCursorState(record durableCursorRecord) DurableCursorState {
	return DurableCursorState{Cursor: record.Cursor, CursorDigest: record.CursorDigest, Position: record.Position, Revision: record.Revision}
}

func sameDurableCursorPosition(left, right DurableCursorState) bool {
	return left.Cursor == right.Cursor && left.CursorDigest == right.CursorDigest && left.Position == right.Position
}

// Load returns the last contiguous state for one exact remote/stream/epoch.
// Missing state is distinct from corruption so a caller cannot reset delivery
// merely because retained bytes became unreadable.
func (s *DurableCursorStore) Load(key DurableCursorKey) (DurableCursorState, error) {
	if s == nil {
		return DurableCursorState{}, ErrDurableCursorInvalid
	}
	keyDigest, err := durableCursorKeyDigest(key)
	if err != nil {
		return DurableCursorState{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	root, lock, err := s.openLockedRoot()
	if err != nil {
		return DurableCursorState{}, err
	}
	defer func() { _ = lock.Close() }()
	defer func() { _ = root.Close() }()
	record, err := readDurableCursorRecord(root, durableCursorRecordName(keyDigest), keyDigest)
	if err != nil {
		return DurableCursorState{}, err
	}
	return durableCursorState(record), nil
}

// IsCurrentCheckpointSeed reports whether expected is still the exact
// authenticated coverage cursor installed by SeedFromCheckpoint. A bootstrap
// coverage cursor has no ordinary inbound delivery (and therefore no inbox
// finalize record) of its own; this distinction lets a replacement plugin
// resume at that cursor without treating the intentional absence as lost
// terminal evidence. The full persisted state, including its local revision,
// must still match expected so a concurrent advance can never be mistaken for
// the original seed.
func (s *DurableCursorStore) IsCurrentCheckpointSeed(key DurableCursorKey, expected DurableCursorState) (bool, error) {
	if s == nil || validateDurableCursorValue(expected, true) != nil {
		return false, ErrDurableCursorInvalid
	}
	keyDigest, err := durableCursorKeyDigest(key)
	if err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	root, lock, err := s.openLockedRoot()
	if err != nil {
		return false, err
	}
	defer func() { _ = lock.Close() }()
	defer func() { _ = root.Close() }()
	record, err := readDurableCursorRecord(root, durableCursorRecordName(keyDigest), keyDigest)
	if err != nil {
		return false, err
	}
	current := durableCursorState(record)
	if current != expected {
		return false, ErrDurableCursorConflict
	}
	seed, present := durableCursorRecordSeed(record)
	if !present {
		return false, nil
	}
	return sameDurableCursorPosition(current, DurableCursorState{
		Cursor: seed.Cursor, CursorDigest: seed.CursorDigest, Position: seed.Position,
	}), nil
}

// SeedFromCheckpoint initializes a previously unseen positioned stream at an
// authenticated checkpoint coverage cursor. It never overwrites or skips an
// existing daemon cursor. Exact replay of the same seed is idempotent; a
// different checkpoint, generation, coverage, or cursor conflicts.
func (s *DurableCursorStore) SeedFromCheckpoint(key DurableCursorKey, seed DurableCheckpointSeed) (DurableCursorState, error) {
	if s == nil {
		return DurableCursorState{}, ErrDurableCursorInvalid
	}
	keyDigest, err := durableCursorKeyDigest(key)
	if err != nil {
		return DurableCursorState{}, err
	}
	if err := validateDurableCheckpointSeed(seed); err != nil {
		return DurableCursorState{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	root, lock, err := s.openLockedRoot()
	if err != nil {
		return DurableCursorState{}, err
	}
	defer func() { _ = lock.Close() }()
	defer func() { _ = root.Close() }()

	name := durableCursorRecordName(keyDigest)
	record, readErr := readDurableCursorRecord(root, name, keyDigest)
	if readErr == nil {
		current := durableCursorState(record)
		existingSeed, seeded := durableCursorRecordSeed(record)
		if sameDurableCursorPosition(current, DurableCursorState{Cursor: seed.Cursor, CursorDigest: seed.CursorDigest, Position: seed.Position}) && seeded && existingSeed == seed {
			return current, nil
		}
		return DurableCursorState{}, ErrDurableCursorConflict
	}
	if !errors.Is(readErr, ErrDurableCursorNotFound) {
		return DurableCursorState{}, readErr
	}
	record = durableCursorRecord{
		Version: durableCursorStoreVersion, KeyDigest: keyDigest, Cursor: seed.Cursor,
		CursorDigest: seed.CursorDigest, Position: seed.Position, Revision: 1,
	}
	applyDurableCursorSeed(&record, seed)
	if err := writeDurableCursorRecord(root, name, record); err != nil {
		return DurableCursorState{}, err
	}
	return durableCursorState(record), nil
}

// CompareAndSwap atomically advances one exact remote/stream/epoch.
//
// expected == nil means the record must be absent. A repeated call whose
// desired Cursor+CursorDigest+Position is already current succeeds without
// increasing the revision, even after a daemon restart. This makes the crash
// window after canonical/inbox durability but before cursor persistence
// repairable by safe redelivery. A positioned chain may begin only at position
// one and then advance exactly one position per CAS. Position-zero legacy
// records remain readable but can never be promoted by guessing adjacency.
// Every other stale expectation returns ErrDurableCursorConflict.
//
// next.Revision must be zero; revisions are assigned by the store. Since cloud
// cursors are opaque, this method never guesses ordering from token bytes.
func (s *DurableCursorStore) CompareAndSwap(key DurableCursorKey, expected *DurableCursorState, next DurableCursorState) (DurableCursorState, error) {
	return s.compareAndSwapSpan(key, expected, next, 1)
}

// CompareAndSwapSpan advances one authenticated bounded replay batch. Span is
// the exact number of contiguous cloud positions bound by the batch digest;
// callers cannot use it for an unproven singleton skip.
func (s *DurableCursorStore) CompareAndSwapSpan(key DurableCursorKey, expected *DurableCursorState, next DurableCursorState, span uint16) (DurableCursorState, error) {
	if span == 0 || span > 16 {
		return DurableCursorState{}, ErrDurableCursorInvalid
	}
	return s.compareAndSwapSpan(key, expected, next, uint64(span))
}

func (s *DurableCursorStore) compareAndSwapSpan(key DurableCursorKey, expected *DurableCursorState, next DurableCursorState, span uint64) (DurableCursorState, error) {
	if s == nil {
		return DurableCursorState{}, ErrDurableCursorInvalid
	}
	keyDigest, err := durableCursorKeyDigest(key)
	if err != nil {
		return DurableCursorState{}, err
	}
	if err := validateDurableCursorValue(next, false); err != nil {
		return DurableCursorState{}, err
	}
	if expected != nil {
		if err := validateDurableCursorValue(*expected, true); err != nil {
			return DurableCursorState{}, err
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	root, lock, err := s.openLockedRoot()
	if err != nil {
		return DurableCursorState{}, err
	}
	defer func() { _ = lock.Close() }()
	defer func() { _ = root.Close() }()

	name := durableCursorRecordName(keyDigest)
	record, readErr := readDurableCursorRecord(root, name, keyDigest)
	if errors.Is(readErr, ErrDurableCursorNotFound) {
		if expected != nil || next.Position != span && !(span == 1 && next.Position == 0) {
			return DurableCursorState{}, ErrDurableCursorConflict
		}
		next.Revision = 1
		if err := writeDurableCursorRecord(root, name, durableCursorRecord{Version: durableCursorStoreVersion, KeyDigest: keyDigest, Cursor: next.Cursor, CursorDigest: next.CursorDigest, Position: next.Position, Revision: next.Revision}); err != nil {
			return DurableCursorState{}, err
		}
		return next, nil
	}
	if readErr != nil {
		// In particular, never replace a corrupt record as though it were
		// missing. Recovery must prove the prior contiguous position.
		return DurableCursorState{}, readErr
	}

	current := durableCursorState(record)
	if sameDurableCursorPosition(current, next) {
		return current, nil
	}
	if current.Cursor == next.Cursor {
		// The exact opaque cursor cannot be rebound to a different digest.
		return DurableCursorState{}, ErrDurableCursorConflict
	}
	if expected == nil || current != *expected || current.Revision == math.MaxUint64 {
		return DurableCursorState{}, ErrDurableCursorConflict
	}
	if current.Position == 0 || next.Position == 0 {
		if current.Position != next.Position {
			return DurableCursorState{}, ErrDurableCursorConflict
		}
	} else if current.Position > math.MaxUint64-span || next.Position != current.Position+span {
		return DurableCursorState{}, ErrDurableCursorConflict
	}
	next.Revision = current.Revision + 1
	nextRecord := durableCursorRecord{Version: durableCursorStoreVersion, KeyDigest: keyDigest, Cursor: next.Cursor, CursorDigest: next.CursorDigest, Position: next.Position, Revision: next.Revision}
	carryDurableCursorSeed(&nextRecord, record)
	if err := writeDurableCursorRecord(root, name, nextRecord); err != nil {
		return DurableCursorState{}, err
	}
	return next, nil
}

// RepairFromCompletedDurable closes only the exact crash window between a
// terminal durable inbox commit and its cursor CAS. The completed record still
// carries the server-authenticated predecessor and successor positions, so a
// missing genesis cursor or one exact adjacent predecessor can be advanced
// without redelivering artifact content. Non-adjacent, malformed, or
// same-position/different-token state fails closed. A cursor already beyond an
// older completion is returned unchanged so restart cleanup can prune that
// older finalized record; callers recovering an unfinalized obligation must
// independently require the returned cursor to equal the completion.
func (s *DurableCursorStore) RepairFromCompletedDurable(completion InboundDurableCompletion) (DurableCursorState, error) {
	span := uint16(1)
	if completion.BatchEventCount != 0 {
		span = completion.BatchEventCount
	}
	if s == nil || completion.ProtocolVersion != 1 || completion.Position == 0 ||
		span == 0 || span > 16 || completion.PredecessorPos+uint64(span) != completion.Position || completion.PredecessorCursor == completion.Cursor ||
		!validDurableCursorOpaque(completion.PredecessorCursor, durableCursorMaxToken) {
		return DurableCursorState{}, ErrDurableCursorInvalid
	}
	key := DurableCursorKey{RemoteIdentity: completion.RemoteIdentity, StreamID: completion.StreamID, StreamEpoch: completion.StreamEpoch}
	next := DurableCursorState{Cursor: completion.Cursor, CursorDigest: completion.CursorDigest, Position: completion.Position}
	evidence := completion.Ack.FinalizeEvidence
	if validateDurableCursorKey(key) != nil || validateDurableCursorValue(next, false) != nil ||
		evidence == nil || completion.Ack.DeliveryID == "" || completion.Ack.DeliveryID != evidence.DeliveryID ||
		completion.Ack.NextCursor != completion.Cursor || completion.Ack.NextCursorDigest != completion.CursorDigest ||
		completion.Ack.NextPosition != completion.Position || len(completion.Ack.Outcomes) != int(span) {
		return DurableCursorState{}, ErrDurableCursorInvalid
	}
	for index, outcome := range completion.Ack.Outcomes {
		if outcome.Index != uint32(index) || outcome.Disposition != "accepted" {
			return DurableCursorState{}, ErrDurableCursorInvalid
		}
	}
	if evidence.ProtocolVersion != completion.ProtocolVersion || evidence.RemoteIdentity != completion.RemoteIdentity ||
		evidence.StreamID != completion.StreamID || evidence.StreamEpoch != completion.StreamEpoch || evidence.Cursor != completion.Cursor ||
		evidence.CursorDigest != completion.CursorDigest || evidence.Position != completion.Position || evidence.BatchEventCount != completion.BatchEventCount ||
		evidence.BatchDigest != completion.BatchDigest || !validInboxFinalizeEvidenceShape(*evidence) {
		return DurableCursorState{}, ErrDurableCursorInvalid
	}

	current, err := s.Load(key)
	if errors.Is(err, ErrDurableCursorNotFound) {
		if completion.Position != uint64(span) || completion.PredecessorPos != 0 {
			return DurableCursorState{}, ErrDurableCursorConflict
		}
		return s.CompareAndSwapSpan(key, nil, next, span)
	}
	if err != nil {
		return DurableCursorState{}, err
	}
	if sameDurableCursorPosition(current, next) || current.Position > next.Position {
		return current, nil
	}
	predecessorDigest := sha256.Sum256([]byte(completion.PredecessorCursor))
	if current.Cursor != completion.PredecessorCursor || current.CursorDigest != hex.EncodeToString(predecessorDigest[:]) ||
		current.Position != completion.PredecessorPos {
		return DurableCursorState{}, ErrDurableCursorConflict
	}
	return s.CompareAndSwapSpan(key, &current, next, span)
}
