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
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aplexica/aplexica/internal/filelock"
	"github.com/aplexica/aplexica/internal/plugin/proto"
	"github.com/aplexica/aplexica/internal/privatefs"
)

const (
	durableGapVersion       = uint16(1)
	durableGapDomain        = "aplexica/durable-gap/v1\x00"
	durableGapKeyDomain     = "aplexica/durable-gap-key/v1\x00"
	durableGapLockName      = ".durable-gaps.lock"
	durableGapLockTimeout   = 10 * time.Second
	durableGapRecordPrefix  = "gap-"
	durableGapRecordSuffix  = ".json"
	durableGapMaxRecord     = int64(proto.MaxInboundBytes + 1<<20)
	durableGapMaxRecords    = 1024
	durableGapMaxTotalBytes = int64(256 << 20)
	durableGapMaxIdentity   = 512
)

var (
	ErrDurableGapNotFound = errors.New("daemon: durable gap not found")
	ErrDurableGapInvalid  = errors.New("daemon: invalid durable gap")
	ErrDurableGapConflict = errors.New("daemon: durable gap conflict")
	ErrDurableGapCorrupt  = errors.New("daemon: corrupt durable gap")
	ErrDurableGapFull     = errors.New("daemon: durable gap spool full")
)

// DurableGapKey names the one stopped stream position whose canonical parent
// is unavailable. All values are opaque transport identities; the filename is
// derived from a length-prefixed digest and never contains them directly.
type DurableGapKey struct {
	RemoteIdentity string
	StreamID       string
	StreamEpoch    string
	Position       uint64
}

// DurableGap is a bounded restart-safe copy of one deferred durable delivery.
// Event bodies are already recipient-encrypted envelopes. MissingEventIndex
// identifies the first event whose canonical parent is unavailable. The spool
// never decrypts or materializes content and lives under a private directory.
type DurableGap struct {
	Key               DurableGapKey
	Delivery          proto.RemoteInboundDeliveryV2
	MissingParentHash string
	MissingEventIndex uint16
	CreatedAt         time.Time
}

type durableGapRecord struct {
	Version           uint16                        `json:"version"`
	KeyDigest         string                        `json:"key_digest"`
	RemoteIdentity    string                        `json:"remote_identity"`
	StreamID          string                        `json:"stream_id"`
	StreamEpoch       string                        `json:"stream_epoch"`
	Position          uint64                        `json:"position"`
	Delivery          proto.RemoteInboundDeliveryV2 `json:"delivery"`
	MissingParentHash string                        `json:"missing_parent_hash"`
	MissingEventIndex uint16                        `json:"missing_event_index,omitempty"`
	CreatedAt         time.Time                     `json:"created_at"`
	Checksum          string                        `json:"checksum"`
}

type DurableGapStore struct {
	Root string
	mu   sync.Mutex
}

func validDurableGapOpaque(value string, maximum int) bool {
	if value == "" || len(value) > maximum {
		return false
	}
	for _, r := range value {
		if r < '\x21' || r > '\x7e' {
			return false
		}
	}
	return true
}

func validDurableGapDigest(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size && hex.EncodeToString(decoded) == value
}

func validateDurableGapKey(key DurableGapKey) error {
	if !validDurableGapOpaque(key.RemoteIdentity, durableGapMaxIdentity) ||
		!validDurableGapOpaque(key.StreamID, durableGapMaxIdentity) ||
		!validDurableGapOpaque(key.StreamEpoch, durableGapMaxIdentity) || key.Position == 0 {
		return ErrDurableGapInvalid
	}
	return nil
}

func durableGapKeyDigest(key DurableGapKey) (string, error) {
	if err := validateDurableGapKey(key); err != nil {
		return "", err
	}
	h := sha256.New()
	_, _ = h.Write([]byte(durableGapKeyDomain))
	for _, value := range []string{key.RemoteIdentity, key.StreamID, key.StreamEpoch} {
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(len(value)))
		_, _ = h.Write(size[:])
		_, _ = h.Write([]byte(value))
	}
	var position [8]byte
	binary.BigEndian.PutUint64(position[:], key.Position)
	_, _ = h.Write(position[:])
	return hex.EncodeToString(h.Sum(nil)), nil
}

func durableGapName(keyDigest string) string {
	return durableGapRecordPrefix + keyDigest + durableGapRecordSuffix
}

func durableGapCapacityAvailable(existingRecords int, existingBytes, nextBytes int64) bool {
	return existingRecords >= 0 && existingRecords < durableGapMaxRecords &&
		existingBytes >= 0 && nextBytes > 0 && nextBytes <= durableGapMaxRecord &&
		existingBytes <= durableGapMaxTotalBytes-nextBytes
}

func validateDurableGapRecord(record durableGapRecord, keyDigest string) error {
	key := DurableGapKey{RemoteIdentity: record.RemoteIdentity, StreamID: record.StreamID, StreamEpoch: record.StreamEpoch, Position: record.Position}
	if record.Version != durableGapVersion || record.KeyDigest != keyDigest || !validDurableGapDigest(keyDigest) || validateDurableGapKey(key) != nil ||
		record.CreatedAt.IsZero() || len(record.Delivery.Events) == 0 || len(record.Delivery.Events) > proto.RemoteReplayBatchMaxEvents || record.Delivery.ProtocolVersion != 1 ||
		record.Delivery.StreamID != record.StreamID || record.Delivery.StreamEpoch != record.StreamEpoch || record.Delivery.Position != record.Position ||
		record.Delivery.PredecessorPosition+uint64(len(record.Delivery.Events)) != record.Position || record.Delivery.PredecessorCursor == record.Delivery.Cursor ||
		!validDurableGapOpaque(record.Delivery.DeliveryID, proto.MaxDeliveryIDBytes) ||
		!validDurableGapOpaque(record.Delivery.Cursor, proto.MaxDurableCursorBytes) ||
		!validDurableGapOpaque(record.Delivery.PredecessorCursor, proto.MaxDurableCursorBytes) ||
		!validDurableGapDigest(record.Delivery.CursorDigest) || !validDurableGapDigest(record.MissingParentHash) ||
		int(record.MissingEventIndex) >= len(record.Delivery.Events) {
		return ErrDurableGapInvalid
	}
	if len(record.Delivery.Events) == 1 {
		if record.Delivery.BatchEventCount != 0 || record.Delivery.BatchDigest != "" || record.MissingEventIndex != 0 {
			return ErrDurableGapInvalid
		}
	} else {
		if record.Delivery.StagedCheckpoint != nil || record.Delivery.BatchEventCount != uint16(len(record.Delivery.Events)) ||
			!validDurableGapDigest(record.Delivery.BatchDigest) {
			return ErrDurableGapInvalid
		}
		batchDigest, err := proto.RemoteReplayBatchDigest(record.Delivery)
		if err != nil || batchDigest != record.Delivery.BatchDigest {
			return ErrDurableGapInvalid
		}
	}
	cursorDigest := sha256.Sum256([]byte(record.Delivery.Cursor))
	if record.Delivery.CursorDigest != hex.EncodeToString(cursorDigest[:]) {
		return ErrDurableGapInvalid
	}
	first := record.Delivery.Events[0]
	for _, candidate := range record.Delivery.Events {
		if !validDurableGapDigest(candidate.EventHash) || candidate.EventHash == candidate.ParentHash ||
			(candidate.ParentHash != "" && !validDurableGapDigest(candidate.ParentHash)) ||
			len(candidate.Bytes) == 0 || len(candidate.Bytes) > proto.MaxSealedEventBytes || !validDurableGapDigest(candidate.BodyDigest) {
			return ErrDurableGapInvalid
		}
		if candidate.NamespaceID != first.NamespaceID || candidate.AccessGeneration != first.AccessGeneration ||
			candidate.AccessSetHash != first.AccessSetHash || candidate.SecurityBarrierID != first.SecurityBarrierID ||
			candidate.SecurityGeneration != first.SecurityGeneration || candidate.KeyMode != first.KeyMode || candidate.KeyVersion != first.KeyVersion {
			return ErrDurableGapInvalid
		}
		bodyDigest := sha256.Sum256(candidate.Bytes)
		if candidate.BodyDigest != hex.EncodeToString(bodyDigest[:]) {
			return ErrDurableGapInvalid
		}
	}
	event := record.Delivery.Events[record.MissingEventIndex]
	if event.ParentHash != record.MissingParentHash || !validDurableGapDigest(event.EventHash) || event.EventHash == event.ParentHash ||
		len(event.Bytes) == 0 || len(event.Bytes) > proto.MaxSealedEventBytes || !validDurableGapDigest(event.BodyDigest) {
		return ErrDurableGapInvalid
	}
	return nil
}

func durableGapChecksum(record durableGapRecord) (string, error) {
	record.Checksum = ""
	encoded, err := json.Marshal(record)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(append([]byte(durableGapDomain), encoded...))
	return hex.EncodeToString(digest[:]), nil
}

func encodeDurableGapRecord(record durableGapRecord) ([]byte, error) {
	if err := validateDurableGapRecord(record, record.KeyDigest); err != nil {
		return nil, err
	}
	checksum, err := durableGapChecksum(record)
	if err != nil {
		return nil, err
	}
	record.Checksum = checksum
	encoded, err := json.Marshal(record)
	if err != nil || int64(len(encoded)) > durableGapMaxRecord {
		return nil, ErrDurableGapInvalid
	}
	return encoded, nil
}

func decodeDurableGapRecord(encoded []byte, keyDigest string) (durableGapRecord, error) {
	if len(encoded) == 0 || int64(len(encoded)) > durableGapMaxRecord {
		return durableGapRecord{}, ErrDurableGapCorrupt
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var record durableGapRecord
	if err := decoder.Decode(&record); err != nil {
		return durableGapRecord{}, fmt.Errorf("%w: decode", ErrDurableGapCorrupt)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return durableGapRecord{}, fmt.Errorf("%w: trailing data", ErrDurableGapCorrupt)
	}
	canonical, err := json.Marshal(record)
	if err != nil || !bytes.Equal(canonical, encoded) {
		return durableGapRecord{}, fmt.Errorf("%w: non-canonical record", ErrDurableGapCorrupt)
	}
	if err := validateDurableGapRecord(record, keyDigest); err != nil {
		return durableGapRecord{}, fmt.Errorf("%w: metadata", ErrDurableGapCorrupt)
	}
	want, err := durableGapChecksum(record)
	if err != nil || record.Checksum != want || !validDurableGapDigest(record.Checksum) {
		return durableGapRecord{}, fmt.Errorf("%w: checksum", ErrDurableGapCorrupt)
	}
	return record, nil
}

func (s *DurableGapStore) openLockedRoot() (*privatefs.Root, *filelock.Lock, error) {
	if s == nil || !filepath.IsAbs(s.Root) || filepath.Clean(s.Root) != s.Root {
		return nil, nil, ErrDurableGapInvalid
	}
	if err := privatefs.EnsureDir(s.Root, privatefs.DirPolicy{Access: privatefs.AccessPrivate, RepairOwned: true, AllowExisting: true}); err != nil {
		return nil, nil, fmt.Errorf("daemon: secure durable gap root: %w", err)
	}
	lock, err := filelock.Acquire(filepath.Join(s.Root, durableGapLockName), durableGapLockTimeout)
	if err != nil {
		return nil, nil, fmt.Errorf("daemon: lock durable gap spool: %w", err)
	}
	root, err := privatefs.OpenRoot(s.Root, privatefs.DirPolicy{Access: privatefs.AccessPrivate, RepairOwned: true})
	if err != nil {
		_ = lock.Close()
		return nil, nil, err
	}
	return root, lock, nil
}

func readDurableGapRecord(root *privatefs.Root, name, keyDigest string) (durableGapRecord, error) {
	file, err := root.OpenReadRegular(name)
	if errors.Is(err, os.ErrNotExist) {
		return durableGapRecord{}, ErrDurableGapNotFound
	}
	if err != nil {
		return durableGapRecord{}, fmt.Errorf("%w: open", ErrDurableGapCorrupt)
	}
	encoded, readErr := io.ReadAll(io.LimitReader(file, durableGapMaxRecord+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil || int64(len(encoded)) > durableGapMaxRecord {
		return durableGapRecord{}, fmt.Errorf("%w: read", ErrDurableGapCorrupt)
	}
	return decodeDurableGapRecord(encoded, keyDigest)
}

func durableGapFromRecord(record durableGapRecord) DurableGap {
	return DurableGap{
		Key:      DurableGapKey{RemoteIdentity: record.RemoteIdentity, StreamID: record.StreamID, StreamEpoch: record.StreamEpoch, Position: record.Position},
		Delivery: record.Delivery, MissingParentHash: record.MissingParentHash, MissingEventIndex: record.MissingEventIndex, CreatedAt: record.CreatedAt,
	}
}

func sameDurableGapDelivery(left, right proto.RemoteInboundDeliveryV2) bool {
	leftEncoded, leftErr := json.Marshal(left)
	rightEncoded, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftEncoded, rightEncoded)
}

// Put durably spools one recipient-encrypted deferred replay unit. Exact
// redelivery is idempotent. A different body or missing-event selector at the
// same stream position fails closed. The optional selector preserves source
// compatibility for singleton callers; batches must supply it explicitly.
func (s *DurableGapStore) Put(key DurableGapKey, delivery proto.RemoteInboundDeliveryV2, missingParentHash string, missingEventIndex ...uint16) (DurableGap, error) {
	keyDigest, err := durableGapKeyDigest(key)
	if err != nil {
		return DurableGap{}, err
	}
	index := uint16(0)
	if len(missingEventIndex) > 1 {
		return DurableGap{}, ErrDurableGapInvalid
	}
	if len(missingEventIndex) == 1 {
		index = missingEventIndex[0]
	}
	record := durableGapRecord{Version: durableGapVersion, KeyDigest: keyDigest, RemoteIdentity: key.RemoteIdentity, StreamID: key.StreamID, StreamEpoch: key.StreamEpoch, Position: key.Position, Delivery: delivery, MissingParentHash: missingParentHash, MissingEventIndex: index, CreatedAt: time.Now().UTC()}
	if err := validateDurableGapRecord(record, keyDigest); err != nil {
		return DurableGap{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	root, lock, err := s.openLockedRoot()
	if err != nil {
		return DurableGap{}, err
	}
	defer func() { _ = lock.Close() }()
	defer func() { _ = root.Close() }()
	name := durableGapName(keyDigest)
	if existing, readErr := readDurableGapRecord(root, name, keyDigest); readErr == nil {
		existingGap := durableGapFromRecord(existing)
		if existing.Delivery.DeliveryID != delivery.DeliveryID || existing.MissingParentHash != missingParentHash || existing.MissingEventIndex != index {
			return DurableGap{}, ErrDurableGapConflict
		}
		if !sameDurableGapDelivery(existing.Delivery, delivery) {
			return DurableGap{}, ErrDurableGapConflict
		}
		return existingGap, nil
	} else if !errors.Is(readErr, ErrDurableGapNotFound) {
		return DurableGap{}, readErr
	}
	encoded, err := encodeDurableGapRecord(record)
	if err != nil {
		return DurableGap{}, err
	}
	entries, err := root.ReadDir(".")
	if err != nil {
		return DurableGap{}, err
	}
	count, total := 0, int64(0)
	for _, entry := range entries {
		if !entry.Type().IsRegular() || !strings.HasPrefix(entry.Name(), durableGapRecordPrefix) || filepath.Ext(entry.Name()) != durableGapRecordSuffix {
			continue
		}
		count++
		file, openErr := root.OpenReadRegular(entry.Name())
		if openErr != nil {
			return DurableGap{}, openErr
		}
		body, readErr := io.ReadAll(io.LimitReader(file, durableGapMaxRecord+1))
		closeErr := file.Close()
		if readErr != nil || closeErr != nil || int64(len(body)) > durableGapMaxRecord {
			return DurableGap{}, ErrDurableGapCorrupt
		}
		total += int64(len(body))
	}
	if !durableGapCapacityAvailable(count, total, int64(len(encoded))) {
		return DurableGap{}, ErrDurableGapFull
	}
	if err := root.WriteFile(name, encoded, privatefs.FilePolicy{RequirePrivateParent: true, RejectWritableByOthers: true}); err != nil {
		return DurableGap{}, err
	}
	return durableGapFromRecord(record), nil
}

// AdvanceSelector atomically moves an exact persisted batch to a later
// missing event after the caller has durably observed the prior selector as
// canonical-terminal. Binding both the expected and replacement selectors
// closes the crash window between canonical replay and ordinary Resolve: a
// restart can advance without deleting the only encrypted recovery copy.
// Exact replacement retries are idempotent; backward or cross-delivery moves
// fail closed.
func (s *DurableGapStore) AdvanceSelector(
	key DurableGapKey,
	delivery proto.RemoteInboundDeliveryV2,
	priorMissingParentHash string,
	priorMissingEventIndex uint16,
	nextMissingParentHash string,
	nextMissingEventIndex uint16,
) (DurableGap, error) {
	keyDigest, err := durableGapKeyDigest(key)
	if err != nil || nextMissingEventIndex <= priorMissingEventIndex ||
		int(priorMissingEventIndex) >= len(delivery.Events) ||
		delivery.Events[priorMissingEventIndex].ParentHash != priorMissingParentHash {
		return DurableGap{}, ErrDurableGapInvalid
	}
	replacement := durableGapRecord{
		Version: durableGapVersion, KeyDigest: keyDigest,
		RemoteIdentity: key.RemoteIdentity, StreamID: key.StreamID, StreamEpoch: key.StreamEpoch, Position: key.Position,
		Delivery: delivery, MissingParentHash: nextMissingParentHash, MissingEventIndex: nextMissingEventIndex, CreatedAt: time.Now().UTC(),
	}
	if err := validateDurableGapRecord(replacement, keyDigest); err != nil {
		return DurableGap{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	root, lock, err := s.openLockedRoot()
	if err != nil {
		return DurableGap{}, err
	}
	defer func() { _ = lock.Close() }()
	defer func() { _ = root.Close() }()
	existing, err := readDurableGapRecord(root, durableGapName(keyDigest), keyDigest)
	if err != nil {
		return DurableGap{}, err
	}
	if !sameDurableGapDelivery(existing.Delivery, delivery) || existing.Delivery.DeliveryID != delivery.DeliveryID {
		return DurableGap{}, ErrDurableGapConflict
	}
	if existing.MissingEventIndex == nextMissingEventIndex && existing.MissingParentHash == nextMissingParentHash {
		return durableGapFromRecord(existing), nil
	}
	if existing.MissingEventIndex != priorMissingEventIndex || existing.MissingParentHash != priorMissingParentHash {
		return DurableGap{}, ErrDurableGapConflict
	}
	replacement.CreatedAt = existing.CreatedAt
	encoded, err := encodeDurableGapRecord(replacement)
	if err != nil {
		return DurableGap{}, err
	}
	if err := root.WriteFile(durableGapName(keyDigest), encoded, privatefs.FilePolicy{RequirePrivateParent: true, RejectWritableByOthers: true}); err != nil {
		return DurableGap{}, err
	}
	return durableGapFromRecord(replacement), nil
}

func (s *DurableGapStore) Load(key DurableGapKey) (DurableGap, error) {
	keyDigest, err := durableGapKeyDigest(key)
	if err != nil {
		return DurableGap{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	root, lock, err := s.openLockedRoot()
	if err != nil {
		return DurableGap{}, err
	}
	defer func() { _ = lock.Close() }()
	defer func() { _ = root.Close() }()
	record, err := readDurableGapRecord(root, durableGapName(keyDigest), keyDigest)
	if err != nil {
		return DurableGap{}, err
	}
	return durableGapFromRecord(record), nil
}

// Resolve removes only the exact spooled delivery after canonical replay is
// terminal. Missing records are idempotent; a mismatched delivery never unlinks
// another stopped stream position.
func (s *DurableGapStore) Resolve(key DurableGapKey, deliveryID string) error {
	keyDigest, err := durableGapKeyDigest(key)
	if err != nil || !validDurableGapOpaque(deliveryID, proto.MaxDeliveryIDBytes) {
		return ErrDurableGapInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	root, lock, err := s.openLockedRoot()
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()
	defer func() { _ = root.Close() }()
	name := durableGapName(keyDigest)
	record, err := readDurableGapRecord(root, name, keyDigest)
	if errors.Is(err, ErrDurableGapNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if record.Delivery.DeliveryID != deliveryID {
		return ErrDurableGapConflict
	}
	return root.RemoveRegular(name)
}

func (s *DurableGapStore) List() ([]DurableGap, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	root, lock, err := s.openLockedRoot()
	if err != nil {
		return nil, err
	}
	defer func() { _ = lock.Close() }()
	defer func() { _ = root.Close() }()
	entries, err := root.ReadDir(".")
	if err != nil {
		return nil, err
	}
	gaps := make([]DurableGap, 0)
	for _, entry := range entries {
		if !entry.Type().IsRegular() || !strings.HasPrefix(entry.Name(), durableGapRecordPrefix) || filepath.Ext(entry.Name()) != durableGapRecordSuffix {
			continue
		}
		keyDigest := entry.Name()[len(durableGapRecordPrefix) : len(entry.Name())-len(durableGapRecordSuffix)]
		record, readErr := readDurableGapRecord(root, entry.Name(), keyDigest)
		if readErr != nil {
			return nil, readErr
		}
		gaps = append(gaps, durableGapFromRecord(record))
	}
	sort.Slice(gaps, func(left, right int) bool {
		a, b := gaps[left].Key, gaps[right].Key
		if a.RemoteIdentity != b.RemoteIdentity {
			return a.RemoteIdentity < b.RemoteIdentity
		}
		if a.StreamID != b.StreamID {
			return a.StreamID < b.StreamID
		}
		if a.StreamEpoch != b.StreamEpoch {
			return a.StreamEpoch < b.StreamEpoch
		}
		return a.Position < b.Position
	})
	return gaps, nil
}
