package daemon

// DurablePublishWatermarkStore persists the last canonical event for which the
// cloud returned a fully authenticated, committed durable-sync receipt.  It is
// deliberately separate from the outbox: the safe terminal order is
//
//   committed receipt -> watermark fsync -> outbox removal
//
// A crash before the watermark fsync therefore leaves the outbox entry to be
// retried.  A crash after the watermark fsync is harmless because an exact
// retry is idempotent.  The records contain only opaque identifiers, digests,
// positions, and recipient-generation metadata; never artifact content.

import (
	"crypto/sha256"
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

	"github.com/aplexica/aplexica/internal/atomicfile"
	"github.com/aplexica/aplexica/internal/privatefs"
)

const (
	durablePublishWatermarkSchema = 1
	durablePublishWatermarkPerm   = 0o600
	durablePublishWatermarkMax    = 16 << 10
)

var (
	ErrDurablePublishWatermarkInvalid  = errors.New("durable publish watermark: invalid")
	ErrDurablePublishWatermarkConflict = errors.New("durable publish watermark: conflict")
	ErrDurablePublishWatermarkRegress  = errors.New("durable publish watermark: position regression")
)

// DurablePublishWatermarkKey identifies one canonical branch in one server
// stream epoch.  StreamEpoch is part of the key so an epoch transition cannot
// silently reuse an old recovery anchor; the new epoch must establish its own
// checkpoint/commit evidence.
type DurablePublishWatermarkKey struct {
	StreamID    string `json:"stream_id"`
	StreamEpoch string `json:"stream_epoch"`
	ArtifactID  string `json:"artifact_id"`
	BranchID    string `json:"branch_id"`
}

// DurablePublishWatermark is the exact content-free evidence required to
// rebuild a missing outbound range from the canonical event log.
type DurablePublishWatermark struct {
	SchemaVersion        int                        `json:"schema_version"`
	Key                  DurablePublishWatermarkKey `json:"key"`
	CanonicalEventID     string                     `json:"canonical_event_id"`
	CanonicalEventHash   string                     `json:"canonical_event_hash"`
	Position             uint64                     `json:"position"`
	RecipientFingerprint string                     `json:"recipient_fingerprint"`
	AccessGeneration     uint64                     `json:"access_generation,omitempty"`
	SecurityGeneration   uint64                     `json:"security_generation,omitempty"`
	SecurityBarrier      string                     `json:"security_barrier,omitempty"`
	KeyMode              string                     `json:"key_mode,omitempty"`
	KeyVersion           uint64                     `json:"key_version,omitempty"`
	BodyDigest           string                     `json:"body_digest"`
	EventIdentityDigest  string                     `json:"event_identity_digest"`
	MetadataDigest       string                     `json:"metadata_digest"`
	CommittedAt          time.Time                  `json:"committed_at"`
	Checksum             string                     `json:"checksum"`
}

type durablePublishWatermarkUnsigned struct {
	SchemaVersion        int                        `json:"schema_version"`
	Key                  DurablePublishWatermarkKey `json:"key"`
	CanonicalEventID     string                     `json:"canonical_event_id"`
	CanonicalEventHash   string                     `json:"canonical_event_hash"`
	Position             uint64                     `json:"position"`
	RecipientFingerprint string                     `json:"recipient_fingerprint"`
	AccessGeneration     uint64                     `json:"access_generation,omitempty"`
	SecurityGeneration   uint64                     `json:"security_generation,omitempty"`
	SecurityBarrier      string                     `json:"security_barrier,omitempty"`
	KeyMode              string                     `json:"key_mode,omitempty"`
	KeyVersion           uint64                     `json:"key_version,omitempty"`
	BodyDigest           string                     `json:"body_digest"`
	EventIdentityDigest  string                     `json:"event_identity_digest"`
	MetadataDigest       string                     `json:"metadata_digest"`
	CommittedAt          time.Time                  `json:"committed_at"`
}

type DurablePublishWatermarkStore struct {
	Root string
	mu   sync.Mutex
	now  func() time.Time
}

func (s *DurablePublishWatermarkStore) Init() error {
	if s == nil || !filepath.IsAbs(s.Root) || s.Root == string(filepath.Separator) {
		return ErrDurablePublishWatermarkInvalid
	}
	if err := privatefs.EnsureDir(s.Root, privatefs.DirPolicy{
		Access: privatefs.AccessPrivate, RepairOwned: true, AllowExisting: true,
	}); err != nil {
		return fmt.Errorf("durable publish watermark: protect root: %w", err)
	}
	return nil
}

func (s *DurablePublishWatermarkStore) clock() time.Time {
	if s != nil && s.now != nil {
		return s.now().UTC()
	}
	return time.Now().UTC()
}

func validateWatermarkOpaque(value string) bool {
	if value == "" || len(value) > 512 {
		return false
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func validateWatermarkDigest(value string) bool {
	if len(value) != sha256.Size*2 || value != strings.ToLower(value) {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}

func validRecoveryKeyModeVersion(mode string, version uint64) bool {
	switch mode {
	case "recipient-wrap-v2":
		return version == 0
	case "namespace-key-v1":
		return version > 0
	default:
		return false
	}
}

func normalizeWatermark(w DurablePublishWatermark) DurablePublishWatermark {
	if w.SchemaVersion == 0 {
		w.SchemaVersion = durablePublishWatermarkSchema
	}
	if w.Key.BranchID == "" {
		w.Key.BranchID = "main"
	}
	w.CanonicalEventHash = strings.ToLower(w.CanonicalEventHash)
	w.RecipientFingerprint = strings.ToLower(w.RecipientFingerprint)
	w.SecurityBarrier = strings.ToLower(w.SecurityBarrier)
	w.BodyDigest = strings.ToLower(w.BodyDigest)
	w.EventIdentityDigest = strings.ToLower(w.EventIdentityDigest)
	w.MetadataDigest = strings.ToLower(w.MetadataDigest)
	w.CommittedAt = w.CommittedAt.UTC()
	return w
}

func validateWatermark(w DurablePublishWatermark) error {
	w = normalizeWatermark(w)
	if w.SchemaVersion != durablePublishWatermarkSchema || w.Position == 0 || w.CommittedAt.IsZero() ||
		!validateWatermarkOpaque(w.Key.StreamID) || !validateWatermarkOpaque(w.Key.StreamEpoch) ||
		!validateWatermarkOpaque(w.Key.ArtifactID) || !validateWatermarkOpaque(w.Key.BranchID) ||
		!validateWatermarkOpaque(w.CanonicalEventID) || !validateWatermarkDigest(w.CanonicalEventHash) ||
		!validateWatermarkDigest(w.RecipientFingerprint) || !validateWatermarkDigest(w.BodyDigest) ||
		!validateWatermarkDigest(w.EventIdentityDigest) || !validateWatermarkDigest(w.MetadataDigest) {
		return ErrDurablePublishWatermarkInvalid
	}
	// Schema-v1 records written before generation-bound recovery omit the new
	// tuple and remain readable for diagnostics. Recovery itself rejects that
	// legacy shape through HasRecoveryGeneration rather than guessing.
	hasAnyGeneration := w.AccessGeneration != 0 || w.SecurityGeneration != 0 || w.SecurityBarrier != "" || w.KeyMode != "" || w.KeyVersion != 0
	if hasAnyGeneration && (w.AccessGeneration == 0 || w.SecurityGeneration == 0 || !validateWatermarkDigest(w.SecurityBarrier) ||
		!validateWatermarkOpaque(w.KeyMode) || !validRecoveryKeyModeVersion(w.KeyMode, w.KeyVersion)) {
		return ErrDurablePublishWatermarkInvalid
	}
	return nil
}

// HasRecoveryGeneration reports whether this anchor carries the complete
// authenticated recipient/security tuple required by canonical range replay.
// Legacy diagnostic watermarks deliberately return false.
func (w DurablePublishWatermark) HasRecoveryGeneration() bool {
	w = normalizeWatermark(w)
	return validateWatermark(w) == nil && w.AccessGeneration != 0 && w.SecurityGeneration != 0 &&
		validateWatermarkDigest(w.SecurityBarrier) && validRecoveryKeyModeVersion(w.KeyMode, w.KeyVersion)
}

func watermarkUnsigned(w DurablePublishWatermark) durablePublishWatermarkUnsigned {
	w = normalizeWatermark(w)
	return durablePublishWatermarkUnsigned{
		SchemaVersion: w.SchemaVersion, Key: w.Key, CanonicalEventID: w.CanonicalEventID,
		CanonicalEventHash: w.CanonicalEventHash, Position: w.Position,
		RecipientFingerprint: w.RecipientFingerprint, BodyDigest: w.BodyDigest,
		AccessGeneration: w.AccessGeneration, SecurityGeneration: w.SecurityGeneration,
		SecurityBarrier: w.SecurityBarrier, KeyMode: w.KeyMode, KeyVersion: w.KeyVersion,
		EventIdentityDigest: w.EventIdentityDigest, MetadataDigest: w.MetadataDigest,
		CommittedAt: w.CommittedAt,
	}
}

func watermarkChecksum(w DurablePublishWatermark) (string, error) {
	raw, err := json.Marshal(watermarkUnsigned(w))
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(append([]byte("aplexica/durable-publish-watermark/v1\x00"), raw...))
	return hex.EncodeToString(sum[:]), nil
}

func watermarkKeyDigest(key DurablePublishWatermarkKey) (string, error) {
	probe := normalizeWatermark(DurablePublishWatermark{Key: key}).Key
	if !validateWatermarkOpaque(probe.StreamID) || !validateWatermarkOpaque(probe.StreamEpoch) ||
		!validateWatermarkOpaque(probe.ArtifactID) || !validateWatermarkOpaque(probe.BranchID) {
		return "", ErrDurablePublishWatermarkInvalid
	}
	raw, err := json.Marshal(probe)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(append([]byte("aplexica/durable-publish-watermark-key/v1\x00"), raw...))
	return hex.EncodeToString(sum[:]), nil
}

func (s *DurablePublishWatermarkStore) path(key DurablePublishWatermarkKey) (string, error) {
	digest, err := watermarkKeyDigest(key)
	if err != nil {
		return "", err
	}
	return filepath.Join(s.Root, digest+".json"), nil
}

func encodeWatermark(w DurablePublishWatermark) ([]byte, DurablePublishWatermark, error) {
	w = normalizeWatermark(w)
	if err := validateWatermark(w); err != nil {
		return nil, DurablePublishWatermark{}, err
	}
	checksum, err := watermarkChecksum(w)
	if err != nil {
		return nil, DurablePublishWatermark{}, err
	}
	w.Checksum = checksum
	raw, err := json.Marshal(w)
	if err != nil {
		return nil, DurablePublishWatermark{}, err
	}
	if len(raw) > durablePublishWatermarkMax {
		return nil, DurablePublishWatermark{}, ErrDurablePublishWatermarkInvalid
	}
	return raw, w, nil
}

func decodeWatermark(raw []byte, key DurablePublishWatermarkKey) (DurablePublishWatermark, error) {
	if len(raw) == 0 || len(raw) > durablePublishWatermarkMax {
		return DurablePublishWatermark{}, ErrDurablePublishWatermarkInvalid
	}
	var w DurablePublishWatermark
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&w); err != nil {
		return DurablePublishWatermark{}, ErrDurablePublishWatermarkInvalid
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return DurablePublishWatermark{}, ErrDurablePublishWatermarkInvalid
	}
	w = normalizeWatermark(w)
	if w.Key != normalizeWatermark(DurablePublishWatermark{Key: key}).Key || validateWatermark(w) != nil || !validateWatermarkDigest(w.Checksum) {
		return DurablePublishWatermark{}, ErrDurablePublishWatermarkInvalid
	}
	want, err := watermarkChecksum(w)
	if err != nil || !strings.EqualFold(w.Checksum, want) {
		return DurablePublishWatermark{}, ErrDurablePublishWatermarkInvalid
	}
	return w, nil
}

// Load returns the last committed watermark for key. os.ErrNotExist is
// preserved so callers can distinguish a new stream/artifact from corruption.
func (s *DurablePublishWatermarkStore) Load(key DurablePublishWatermarkKey) (DurablePublishWatermark, error) {
	if s == nil {
		return DurablePublishWatermark{}, ErrDurablePublishWatermarkInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	path, err := s.path(key)
	if err != nil {
		return DurablePublishWatermark{}, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return DurablePublishWatermark{}, err
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > durablePublishWatermarkMax {
		return DurablePublishWatermark{}, ErrDurablePublishWatermarkInvalid
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return DurablePublishWatermark{}, err
	}
	return decodeWatermark(raw, key)
}

// List returns all authenticated watermark records in stable key order.
// A malformed or unsafe entry fails the whole read so recovery can never clear
// a dirty marker from a partial view.
func (s *DurablePublishWatermarkStore) List() ([]DurablePublishWatermark, error) {
	if s == nil {
		return nil, ErrDurablePublishWatermarkInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(s.Root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	result := make([]DurablePublishWatermark, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		path := filepath.Join(s.Root, entry.Name())
		info, statErr := os.Lstat(path)
		if statErr != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > durablePublishWatermarkMax {
			return nil, ErrDurablePublishWatermarkInvalid
		}
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil, readErr
		}
		var probe DurablePublishWatermark
		decoder := json.NewDecoder(strings.NewReader(string(raw)))
		decoder.DisallowUnknownFields()
		if decodeErr := decoder.Decode(&probe); decodeErr != nil {
			return nil, ErrDurablePublishWatermarkInvalid
		}
		decoded, decodeErr := decodeWatermark(raw, probe.Key)
		if decodeErr != nil {
			return nil, decodeErr
		}
		digest, digestErr := watermarkKeyDigest(decoded.Key)
		if digestErr != nil || entry.Name() != digest+".json" {
			return nil, ErrDurablePublishWatermarkInvalid
		}
		result = append(result, decoded)
	}
	sort.Slice(result, func(i, j int) bool {
		a, b := result[i].Key, result[j].Key
		if a.StreamID != b.StreamID {
			return a.StreamID < b.StreamID
		}
		if a.StreamEpoch != b.StreamEpoch {
			return a.StreamEpoch < b.StreamEpoch
		}
		if a.ArtifactID != b.ArtifactID {
			return a.ArtifactID < b.ArtifactID
		}
		return a.BranchID < b.BranchID
	})
	return result, nil
}

// Advance atomically persists a strictly newer committed position, or returns
// the existing record for an exact idempotent retry. A regression or a second
// identity at the same position is rejected and the existing evidence is left
// untouched.
func (s *DurablePublishWatermarkStore) Advance(next DurablePublishWatermark) (DurablePublishWatermark, error) {
	if s == nil {
		return DurablePublishWatermark{}, ErrDurablePublishWatermarkInvalid
	}
	next = normalizeWatermark(next)
	if next.CommittedAt.IsZero() {
		next.CommittedAt = s.clock()
	}
	raw, normalized, err := encodeWatermark(next)
	if err != nil {
		return DurablePublishWatermark{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	path, err := s.path(normalized.Key)
	if err != nil {
		return DurablePublishWatermark{}, err
	}
	if existingRaw, readErr := os.ReadFile(path); readErr == nil {
		existing, decodeErr := decodeWatermark(existingRaw, normalized.Key)
		if decodeErr != nil {
			return DurablePublishWatermark{}, decodeErr
		}
		switch {
		case normalized.Position < existing.Position:
			return DurablePublishWatermark{}, ErrDurablePublishWatermarkRegress
		case normalized.Position == existing.Position:
			// CommittedAt is local persistence evidence and may differ on a
			// repeated response. Compare every server/canonical identity field.
			normalized.CommittedAt = existing.CommittedAt
			normalized.Checksum = existing.Checksum
			if normalized != existing {
				return DurablePublishWatermark{}, ErrDurablePublishWatermarkConflict
			}
			return existing, nil
		}
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return DurablePublishWatermark{}, readErr
	}
	if err := atomicfile.WriteFile(path, raw, durablePublishWatermarkPerm); err != nil {
		return DurablePublishWatermark{}, fmt.Errorf("durable publish watermark: persist: %w", err)
	}
	return normalized, nil
}
