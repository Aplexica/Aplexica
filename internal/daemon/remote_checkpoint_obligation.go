package daemon

// This file persists content-free outbound checkpoint obligations produced
// when canonical delta history can no longer be reconstructed safely. It does
// not manufacture a checkpoint: the producer path consumes these obligations
// in a later slice. Until then the associated rescan marker remains dirty.

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
)

const (
	remoteCheckpointObligationSchema = 1
	remoteCheckpointObligationMax    = 32 << 10
)

type RemoteCheckpointObligationV1 struct {
	SchemaVersion      int       `json:"schema_version"`
	ScopeID            string    `json:"scope_id"`
	ArtifactID         string    `json:"artifact_id,omitempty"`
	BranchID           string    `json:"branch_id,omitempty"`
	Kind               string    `json:"kind,omitempty"`
	HeadEventID        string    `json:"head_event_id,omitempty"`
	HeadEventHash      string    `json:"head_event_hash,omitempty"`
	AccessGeneration   uint64    `json:"access_generation,omitempty"`
	AccessSetHash      string    `json:"access_set_hash,omitempty"`
	SecurityGeneration uint64    `json:"security_generation,omitempty"`
	SecurityBarrier    string    `json:"security_barrier,omitempty"`
	KeyMode            string    `json:"key_mode,omitempty"`
	KeyVersion         uint64    `json:"key_version,omitempty"`
	Reason             string    `json:"reason"`
	UpdatedAt          time.Time `json:"updated_at"`
	// Request fields bind an explicit cloud/bootstrap checkpoint request. They
	// are content-free and authenticated by the same record checksum.
	RequestID                   string `json:"request_id,omitempty"`
	RequestingDeviceID          string `json:"requesting_device_id,omitempty"`
	RequestStreamID             string `json:"request_stream_id,omitempty"`
	RequestStreamEpoch          string `json:"request_stream_epoch,omitempty"`
	RequestCoverage             uint64 `json:"request_coverage,omitempty"`
	RequestAlignmentHash        string `json:"request_alignment_hash,omitempty"`
	RequestCheckpointGeneration string `json:"request_checkpoint_generation,omitempty"`
	MissingParentHash           string `json:"missing_parent_hash,omitempty"`
	// The fields below are the crash-safe fulfillment state. They contain only
	// opaque identities, digests, positions, and timestamps; checkpoint body
	// bytes stay in the private outbox/staging stores.
	MarkerGeneration         uint64    `json:"marker_generation,omitempty"`
	CheckpointState          string    `json:"checkpoint_state,omitempty"`
	CheckpointEventID        string    `json:"checkpoint_event_id,omitempty"`
	CheckpointHeadEventID    string    `json:"checkpoint_head_event_id,omitempty"`
	CheckpointHeadHash       string    `json:"checkpoint_head_hash,omitempty"`
	CheckpointBodyDigest     string    `json:"checkpoint_body_digest,omitempty"`
	CheckpointCoverage       uint64    `json:"checkpoint_coverage,omitempty"`
	CheckpointSourceSequence uint64    `json:"checkpoint_source_sequence,omitempty"`
	CheckpointGeneration     string    `json:"checkpoint_generation,omitempty"`
	CheckpointPreparedAt     time.Time `json:"checkpoint_prepared_at,omitzero"`
	CheckpointCommitCursor   string    `json:"checkpoint_commit_cursor,omitempty"`
	CheckpointPosition       uint64    `json:"checkpoint_position,omitempty"`
	CheckpointStreamID       string    `json:"checkpoint_stream_id,omitempty"`
	CheckpointStreamEpoch    string    `json:"checkpoint_stream_epoch,omitempty"`
	CheckpointIdentityHash   string    `json:"checkpoint_identity_hash,omitempty"`
	CheckpointMetadataHash   string    `json:"checkpoint_metadata_hash,omitempty"`
	CheckpointCommittedAt    time.Time `json:"checkpoint_committed_at,omitzero"`
	Checksum                 string    `json:"checksum"`
}

type remoteCheckpointObligationUnsigned struct {
	SchemaVersion               int       `json:"schema_version"`
	ScopeID                     string    `json:"scope_id"`
	ArtifactID                  string    `json:"artifact_id,omitempty"`
	BranchID                    string    `json:"branch_id,omitempty"`
	Kind                        string    `json:"kind,omitempty"`
	HeadEventID                 string    `json:"head_event_id,omitempty"`
	HeadEventHash               string    `json:"head_event_hash,omitempty"`
	AccessGeneration            uint64    `json:"access_generation,omitempty"`
	AccessSetHash               string    `json:"access_set_hash,omitempty"`
	SecurityGeneration          uint64    `json:"security_generation,omitempty"`
	SecurityBarrier             string    `json:"security_barrier,omitempty"`
	KeyMode                     string    `json:"key_mode,omitempty"`
	KeyVersion                  uint64    `json:"key_version,omitempty"`
	Reason                      string    `json:"reason"`
	UpdatedAt                   time.Time `json:"updated_at"`
	RequestID                   string    `json:"request_id,omitempty"`
	RequestingDeviceID          string    `json:"requesting_device_id,omitempty"`
	RequestStreamID             string    `json:"request_stream_id,omitempty"`
	RequestStreamEpoch          string    `json:"request_stream_epoch,omitempty"`
	RequestCoverage             uint64    `json:"request_coverage,omitempty"`
	RequestAlignmentHash        string    `json:"request_alignment_hash,omitempty"`
	RequestCheckpointGeneration string    `json:"request_checkpoint_generation,omitempty"`
	MissingParentHash           string    `json:"missing_parent_hash,omitempty"`
	MarkerGeneration            uint64    `json:"marker_generation,omitempty"`
	CheckpointState             string    `json:"checkpoint_state,omitempty"`
	CheckpointEventID           string    `json:"checkpoint_event_id,omitempty"`
	CheckpointHeadEventID       string    `json:"checkpoint_head_event_id,omitempty"`
	CheckpointHeadHash          string    `json:"checkpoint_head_hash,omitempty"`
	CheckpointBodyDigest        string    `json:"checkpoint_body_digest,omitempty"`
	CheckpointCoverage          uint64    `json:"checkpoint_coverage,omitempty"`
	CheckpointSourceSequence    uint64    `json:"checkpoint_source_sequence,omitempty"`
	CheckpointGeneration        string    `json:"checkpoint_generation,omitempty"`
	CheckpointPreparedAt        time.Time `json:"checkpoint_prepared_at,omitzero"`
	CheckpointCommitCursor      string    `json:"checkpoint_commit_cursor,omitempty"`
	CheckpointPosition          uint64    `json:"checkpoint_position,omitempty"`
	CheckpointStreamID          string    `json:"checkpoint_stream_id,omitempty"`
	CheckpointStreamEpoch       string    `json:"checkpoint_stream_epoch,omitempty"`
	CheckpointIdentityHash      string    `json:"checkpoint_identity_hash,omitempty"`
	CheckpointMetadataHash      string    `json:"checkpoint_metadata_hash,omitempty"`
	CheckpointCommittedAt       time.Time `json:"checkpoint_committed_at,omitzero"`
}

type RemoteCheckpointObligationStore struct {
	Root string
	mu   sync.Mutex
	now  func() time.Time
}

func (s *RemoteCheckpointObligationStore) Init() error {
	if s == nil || !filepath.IsAbs(s.Root) || s.Root == string(filepath.Separator) {
		return errors.New("remote checkpoint obligation: invalid root")
	}
	if err := os.MkdirAll(s.Root, 0o700); err != nil {
		return fmt.Errorf("remote checkpoint obligation: create root: %w", err)
	}
	return os.Chmod(s.Root, 0o700)
}

func (s *RemoteCheckpointObligationStore) clock() time.Time {
	if s != nil && s.now != nil {
		return s.now().UTC()
	}
	return time.Now().UTC()
}

func validObligationOpaque(value string, max int) bool {
	return value != "" && len(value) <= max && !strings.ContainsAny(value, "\x00\r\n")
}

func normalizeObligation(value RemoteCheckpointObligationV1) RemoteCheckpointObligationV1 {
	if value.SchemaVersion == 0 {
		value.SchemaVersion = remoteCheckpointObligationSchema
	}
	if value.ArtifactID != "" && value.BranchID == "" {
		value.BranchID = "main"
	}
	value.HeadEventHash = strings.ToLower(value.HeadEventHash)
	value.AccessSetHash = strings.ToLower(value.AccessSetHash)
	value.SecurityBarrier = strings.ToLower(value.SecurityBarrier)
	value.RequestAlignmentHash = strings.ToLower(value.RequestAlignmentHash)
	value.RequestCheckpointGeneration = strings.ToLower(value.RequestCheckpointGeneration)
	value.MissingParentHash = strings.ToLower(value.MissingParentHash)
	value.CheckpointHeadHash = strings.ToLower(value.CheckpointHeadHash)
	value.CheckpointBodyDigest = strings.ToLower(value.CheckpointBodyDigest)
	value.CheckpointGeneration = strings.ToLower(value.CheckpointGeneration)
	value.CheckpointIdentityHash = strings.ToLower(value.CheckpointIdentityHash)
	value.CheckpointMetadataHash = strings.ToLower(value.CheckpointMetadataHash)
	value.UpdatedAt = value.UpdatedAt.UTC()
	value.CheckpointPreparedAt = value.CheckpointPreparedAt.UTC()
	value.CheckpointCommittedAt = value.CheckpointCommittedAt.UTC()
	return value
}

func validateObligation(value RemoteCheckpointObligationV1) error {
	value = normalizeObligation(value)
	if value.SchemaVersion != remoteCheckpointObligationSchema || !validObligationOpaque(value.ScopeID, 256) ||
		!validObligationOpaque(value.Reason, 128) || value.UpdatedAt.IsZero() ||
		(value.ArtifactID != "" && (!validObligationOpaque(value.ArtifactID, 256) || !validObligationOpaque(value.BranchID, 128))) ||
		(value.Kind != "" && !validObligationOpaque(value.Kind, 64)) ||
		(value.HeadEventID != "" && !validObligationOpaque(value.HeadEventID, 512)) ||
		(value.HeadEventHash != "" && !validateWatermarkDigest(value.HeadEventHash)) {
		return errors.New("remote checkpoint obligation: invalid record")
	}
	hasGeneration := value.AccessGeneration != 0 || value.AccessSetHash != "" || value.SecurityGeneration != 0 ||
		value.SecurityBarrier != "" || value.KeyMode != "" || value.KeyVersion != 0
	if hasGeneration && (value.AccessGeneration == 0 || !validateWatermarkDigest(value.AccessSetHash) ||
		value.SecurityGeneration == 0 || !validateWatermarkDigest(value.SecurityBarrier) ||
		!validObligationOpaque(value.KeyMode, 64) || !validRecoveryKeyModeVersion(value.KeyMode, value.KeyVersion)) {
		return errors.New("remote checkpoint obligation: invalid generation")
	}
	hasCheckpoint := value.MarkerGeneration != 0 || value.CheckpointState != "" || value.CheckpointEventID != "" ||
		value.CheckpointHeadEventID != "" || value.CheckpointHeadHash != "" || value.CheckpointBodyDigest != "" ||
		value.CheckpointCoverage != 0 || value.CheckpointSourceSequence != 0 || value.CheckpointGeneration != "" || !value.CheckpointPreparedAt.IsZero() ||
		value.CheckpointCommitCursor != "" || value.CheckpointPosition != 0 || value.CheckpointStreamID != "" ||
		value.CheckpointStreamEpoch != "" || value.CheckpointIdentityHash != "" || value.CheckpointMetadataHash != "" ||
		!value.CheckpointCommittedAt.IsZero()
	hasRequest := value.RequestID != "" || value.RequestingDeviceID != "" || value.RequestStreamID != "" || value.RequestStreamEpoch != "" ||
		value.RequestCoverage != 0 || value.RequestAlignmentHash != "" || value.RequestCheckpointGeneration != "" || value.MissingParentHash != ""
	if hasRequest && (!validObligationOpaque(value.RequestID, 512) || !validObligationOpaque(value.RequestStreamID, 512) ||
		!validObligationOpaque(value.RequestStreamEpoch, 512) || value.RequestCoverage == 0 || !validateWatermarkDigest(value.RequestAlignmentHash) ||
		!validateWatermarkDigest(value.RequestCheckpointGeneration) || value.RequestCheckpointGeneration != checkpointGenerationForObligation(value) ||
		!validObligationOpaque(value.RequestingDeviceID, 512) || value.Kind == "" || value.HeadEventHash != value.RequestAlignmentHash ||
		(value.Reason == "missing-parent" && !validateWatermarkDigest(value.MissingParentHash)) ||
		(value.Reason != "missing-parent" && value.MissingParentHash != "")) {
		return errors.New("remote checkpoint obligation: invalid request authority")
	}
	if !hasCheckpoint {
		return nil
	}
	if value.CheckpointState != "prepared" && value.CheckpointState != "committed" && value.CheckpointState != "verified" || value.MarkerGeneration == 0 ||
		!validObligationOpaque(value.CheckpointEventID, 512) || !validObligationOpaque(value.CheckpointHeadEventID, 512) ||
		!validateWatermarkDigest(value.CheckpointHeadHash) || !validateWatermarkDigest(value.CheckpointBodyDigest) ||
		value.CheckpointCoverage == 0 || value.CheckpointSourceSequence == 0 || !validateWatermarkDigest(value.CheckpointGeneration) || value.CheckpointPreparedAt.IsZero() ||
		!hasGeneration {
		return errors.New("remote checkpoint obligation: invalid prepared checkpoint")
	}
	if value.CheckpointState == "prepared" {
		if value.CheckpointCommitCursor != "" || value.CheckpointPosition != 0 || value.CheckpointStreamID != "" ||
			value.CheckpointStreamEpoch != "" || value.CheckpointIdentityHash != "" || value.CheckpointMetadataHash != "" ||
			!value.CheckpointCommittedAt.IsZero() {
			return errors.New("remote checkpoint obligation: prepared checkpoint has receipt")
		}
		return nil
	}
	if !validObligationOpaque(value.CheckpointCommitCursor, 4096) || value.CheckpointPosition <= value.CheckpointCoverage ||
		!validObligationOpaque(value.CheckpointStreamID, 512) || !validObligationOpaque(value.CheckpointStreamEpoch, 512) ||
		!validateWatermarkDigest(value.CheckpointIdentityHash) || !validateWatermarkDigest(value.CheckpointMetadataHash) ||
		value.CheckpointCommittedAt.IsZero() {
		return errors.New("remote checkpoint obligation: invalid committed checkpoint")
	}
	return nil
}

func obligationUnsigned(value RemoteCheckpointObligationV1) remoteCheckpointObligationUnsigned {
	value = normalizeObligation(value)
	return remoteCheckpointObligationUnsigned{
		SchemaVersion: value.SchemaVersion, ScopeID: value.ScopeID, ArtifactID: value.ArtifactID,
		BranchID: value.BranchID, Kind: value.Kind, HeadEventID: value.HeadEventID, HeadEventHash: value.HeadEventHash,
		AccessGeneration: value.AccessGeneration, AccessSetHash: value.AccessSetHash,
		SecurityGeneration: value.SecurityGeneration, SecurityBarrier: value.SecurityBarrier,
		KeyMode: value.KeyMode, KeyVersion: value.KeyVersion, Reason: value.Reason, UpdatedAt: value.UpdatedAt,
		RequestID: value.RequestID, RequestingDeviceID: value.RequestingDeviceID,
		RequestStreamID: value.RequestStreamID, RequestStreamEpoch: value.RequestStreamEpoch,
		RequestCoverage: value.RequestCoverage, RequestAlignmentHash: value.RequestAlignmentHash,
		RequestCheckpointGeneration: value.RequestCheckpointGeneration, MissingParentHash: value.MissingParentHash,
		MarkerGeneration: value.MarkerGeneration, CheckpointState: value.CheckpointState,
		CheckpointEventID: value.CheckpointEventID, CheckpointHeadEventID: value.CheckpointHeadEventID,
		CheckpointHeadHash: value.CheckpointHeadHash, CheckpointBodyDigest: value.CheckpointBodyDigest,
		CheckpointCoverage: value.CheckpointCoverage, CheckpointSourceSequence: value.CheckpointSourceSequence, CheckpointGeneration: value.CheckpointGeneration,
		CheckpointPreparedAt: value.CheckpointPreparedAt, CheckpointCommitCursor: value.CheckpointCommitCursor,
		CheckpointPosition: value.CheckpointPosition, CheckpointStreamID: value.CheckpointStreamID,
		CheckpointStreamEpoch: value.CheckpointStreamEpoch, CheckpointIdentityHash: value.CheckpointIdentityHash,
		CheckpointMetadataHash: value.CheckpointMetadataHash, CheckpointCommittedAt: value.CheckpointCommittedAt,
	}
}

func obligationChecksum(value RemoteCheckpointObligationV1) (string, error) {
	raw, err := json.Marshal(obligationUnsigned(value))
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(append([]byte("aplexica/remote-checkpoint-obligation/v1\x00"), raw...))
	return hex.EncodeToString(sum[:]), nil
}

func obligationKey(scopeID, artifactID, branchID string) string {
	sum := sha256.Sum256([]byte("v1\x00" + scopeID + "\x00" + artifactID + "\x00" + branchID))
	return hex.EncodeToString(sum[:])
}

func (s *RemoteCheckpointObligationStore) Put(value RemoteCheckpointObligationV1) error {
	if s == nil {
		return errors.New("remote checkpoint obligation: store unavailable")
	}
	value = normalizeObligation(value)
	if value.UpdatedAt.IsZero() {
		value.UpdatedAt = s.clock()
	}
	if err := validateObligation(value); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	path := filepath.Join(s.Root, obligationKey(value.ScopeID, value.ArtifactID, value.BranchID)+".json")
	if raw, err := os.ReadFile(path); err == nil {
		existing, decodeErr := decodeObligation(raw)
		if decodeErr != nil {
			return decodeErr
		}
		if existing.RequestID != "" || value.RequestID != "" {
			if sameObligationRequestAuthority(existing, value) {
				value.Kind = existing.Kind
				value.HeadEventID = existing.HeadEventID
				value.HeadEventHash = existing.HeadEventHash
				copyObligationCheckpointProgress(&value, existing)
			} else if canSupersedeObligationRequest(existing, value) {
				// A safety-poll notification may observe that the local head
				// advanced after the prior request was admitted. Before any
				// checkpoint progress exists, a strictly newer exact coverage is
				// safe to adopt. Prepared/committed/verified work is immutable.
			} else {
				return errors.New("remote checkpoint obligation: request authority conflict")
			}
		} else if sameObligationAuthority(existing, value) {
			copyObligationCheckpointProgress(&value, existing)
		} else if existing.CheckpointState != "" && sameObligationGenerationKey(existing, value) {
			// A repeated/in-flight terminal callback must not roll a newer
			// materialization back to its original head. The marker generation
			// already records the concurrent mutation; the worker re-probes the
			// current canonical head before it verifies or supersedes progress.
			value.Kind = existing.Kind
			value.HeadEventID = existing.HeadEventID
			value.HeadEventHash = existing.HeadEventHash
			copyObligationCheckpointProgress(&value, existing)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return writeObligationLocked(path, value)
}

func hasObligationCheckpointProgress(value RemoteCheckpointObligationV1) bool {
	return value.MarkerGeneration != 0 || value.CheckpointState != "" || value.CheckpointEventID != "" ||
		value.CheckpointHeadEventID != "" || value.CheckpointHeadHash != "" || value.CheckpointBodyDigest != "" ||
		value.CheckpointCoverage != 0 || value.CheckpointSourceSequence != 0 || value.CheckpointGeneration != "" ||
		!value.CheckpointPreparedAt.IsZero() || value.CheckpointCommitCursor != "" || value.CheckpointPosition != 0 ||
		value.CheckpointStreamID != "" || value.CheckpointStreamEpoch != "" || value.CheckpointIdentityHash != "" ||
		value.CheckpointMetadataHash != "" || !value.CheckpointCommittedAt.IsZero()
}

func canSupersedeObligationRequest(existing, next RemoteCheckpointObligationV1) bool {
	existing, next = normalizeObligation(existing), normalizeObligation(next)
	return existing.RequestID != "" && next.RequestID != "" && !hasObligationCheckpointProgress(existing) &&
		sameObligationGenerationKey(existing, next) && existing.Kind == next.Kind &&
		existing.RequestStreamID == next.RequestStreamID && existing.RequestStreamEpoch == next.RequestStreamEpoch &&
		next.RequestCoverage > existing.RequestCoverage
}

func sameObligationRequestAuthority(a, b RemoteCheckpointObligationV1) bool {
	a, b = normalizeObligation(a), normalizeObligation(b)
	return sameObligationGenerationKey(a, b) && a.Kind == b.Kind && a.RequestID == b.RequestID && a.RequestingDeviceID == b.RequestingDeviceID &&
		a.RequestStreamID == b.RequestStreamID && a.RequestStreamEpoch == b.RequestStreamEpoch &&
		a.RequestCoverage == b.RequestCoverage && a.RequestAlignmentHash == b.RequestAlignmentHash &&
		a.RequestCheckpointGeneration == b.RequestCheckpointGeneration && a.MissingParentHash == b.MissingParentHash && a.Reason == b.Reason
}

func sameObligationGenerationKey(a, b RemoteCheckpointObligationV1) bool {
	a, b = normalizeObligation(a), normalizeObligation(b)
	return a.ScopeID == b.ScopeID && a.ArtifactID == b.ArtifactID && a.BranchID == b.BranchID &&
		a.AccessGeneration == b.AccessGeneration && a.AccessSetHash == b.AccessSetHash &&
		a.SecurityGeneration == b.SecurityGeneration && a.SecurityBarrier == b.SecurityBarrier &&
		a.KeyMode == b.KeyMode && a.KeyVersion == b.KeyVersion
}

func sameObligationAuthority(a, b RemoteCheckpointObligationV1) bool {
	a, b = normalizeObligation(a), normalizeObligation(b)
	return a.ScopeID == b.ScopeID && a.ArtifactID == b.ArtifactID && a.BranchID == b.BranchID && a.Kind == b.Kind &&
		a.HeadEventID == b.HeadEventID && a.HeadEventHash == b.HeadEventHash &&
		a.AccessGeneration == b.AccessGeneration && a.AccessSetHash == b.AccessSetHash &&
		a.SecurityGeneration == b.SecurityGeneration && a.SecurityBarrier == b.SecurityBarrier &&
		a.KeyMode == b.KeyMode && a.KeyVersion == b.KeyVersion
}

func copyObligationCheckpointProgress(dst *RemoteCheckpointObligationV1, src RemoteCheckpointObligationV1) {
	if dst == nil {
		return
	}
	dst.MarkerGeneration = src.MarkerGeneration
	dst.CheckpointState = src.CheckpointState
	dst.CheckpointEventID = src.CheckpointEventID
	dst.CheckpointHeadEventID = src.CheckpointHeadEventID
	dst.CheckpointHeadHash = src.CheckpointHeadHash
	dst.CheckpointBodyDigest = src.CheckpointBodyDigest
	dst.CheckpointCoverage = src.CheckpointCoverage
	dst.CheckpointSourceSequence = src.CheckpointSourceSequence
	dst.CheckpointGeneration = src.CheckpointGeneration
	dst.CheckpointPreparedAt = src.CheckpointPreparedAt
	dst.CheckpointCommitCursor = src.CheckpointCommitCursor
	dst.CheckpointPosition = src.CheckpointPosition
	dst.CheckpointStreamID = src.CheckpointStreamID
	dst.CheckpointStreamEpoch = src.CheckpointStreamEpoch
	dst.CheckpointIdentityHash = src.CheckpointIdentityHash
	dst.CheckpointMetadataHash = src.CheckpointMetadataHash
	dst.CheckpointCommittedAt = src.CheckpointCommittedAt
}

func writeObligationLocked(path string, value RemoteCheckpointObligationV1) error {
	value = normalizeObligation(value)
	if err := validateObligation(value); err != nil {
		return err
	}
	checksum, err := obligationChecksum(value)
	if err != nil {
		return err
	}
	value.Checksum = checksum
	raw, err := json.Marshal(value)
	if err != nil || len(raw) > remoteCheckpointObligationMax {
		return errors.New("remote checkpoint obligation: record too large")
	}
	return atomicfile.WriteFile(path, raw, 0o600)
}

func (s *RemoteCheckpointObligationStore) update(scopeID, artifactID, branchID string, mutate func(*RemoteCheckpointObligationV1) error) (RemoteCheckpointObligationV1, error) {
	if s == nil || mutate == nil {
		return RemoteCheckpointObligationV1{}, errors.New("remote checkpoint obligation: store unavailable")
	}
	branchID = normalizeObligation(RemoteCheckpointObligationV1{ArtifactID: artifactID, BranchID: branchID}).BranchID
	path := filepath.Join(s.Root, obligationKey(scopeID, artifactID, branchID)+".json")
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, err := os.ReadFile(path)
	if err != nil {
		return RemoteCheckpointObligationV1{}, err
	}
	value, err := decodeObligation(raw)
	if err != nil {
		return RemoteCheckpointObligationV1{}, err
	}
	if err := mutate(&value); err != nil {
		return RemoteCheckpointObligationV1{}, err
	}
	value.UpdatedAt = s.clock()
	if err := writeObligationLocked(path, value); err != nil {
		return RemoteCheckpointObligationV1{}, err
	}
	return normalizeObligation(value), nil
}

func (s *RemoteCheckpointObligationStore) RemoveCommitted(scopeID, artifactID, branchID, checkpointEventID string, markerGeneration uint64) (bool, error) {
	if s == nil || checkpointEventID == "" || markerGeneration == 0 {
		return false, errors.New("remote checkpoint obligation: invalid removal")
	}
	branchID = normalizeObligation(RemoteCheckpointObligationV1{ArtifactID: artifactID, BranchID: branchID}).BranchID
	path := filepath.Join(s.Root, obligationKey(scopeID, artifactID, branchID)+".json")
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	value, err := decodeObligation(raw)
	if err != nil {
		return false, err
	}
	if value.CheckpointState != "verified" || value.CheckpointEventID != checkpointEventID || value.MarkerGeneration != markerGeneration {
		return false, nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	return true, nil
}

func decodeObligation(raw []byte) (RemoteCheckpointObligationV1, error) {
	if len(raw) == 0 || len(raw) > remoteCheckpointObligationMax {
		return RemoteCheckpointObligationV1{}, errors.New("remote checkpoint obligation: invalid record")
	}
	var value RemoteCheckpointObligationV1
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return RemoteCheckpointObligationV1{}, errors.New("remote checkpoint obligation: invalid record")
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return RemoteCheckpointObligationV1{}, errors.New("remote checkpoint obligation: invalid record")
	}
	value = normalizeObligation(value)
	if err := validateObligation(value); err != nil || !validateWatermarkDigest(value.Checksum) {
		return RemoteCheckpointObligationV1{}, errors.New("remote checkpoint obligation: invalid record")
	}
	want, err := obligationChecksum(value)
	if err != nil || value.Checksum != want {
		return RemoteCheckpointObligationV1{}, errors.New("remote checkpoint obligation: checksum mismatch")
	}
	return value, nil
}

func (s *RemoteCheckpointObligationStore) List() ([]RemoteCheckpointObligationV1, error) {
	if s == nil {
		return nil, errors.New("remote checkpoint obligation: store unavailable")
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
	result := make([]RemoteCheckpointObligationV1, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		path := filepath.Join(s.Root, entry.Name())
		info, statErr := os.Lstat(path)
		if statErr != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > remoteCheckpointObligationMax {
			return nil, errors.New("remote checkpoint obligation: unsafe entry")
		}
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil, readErr
		}
		value, decodeErr := decodeObligation(raw)
		if decodeErr != nil || entry.Name() != obligationKey(value.ScopeID, value.ArtifactID, value.BranchID)+".json" {
			if decodeErr != nil {
				return nil, decodeErr
			}
			return nil, errors.New("remote checkpoint obligation: filename binding mismatch")
		}
		result = append(result, value)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].ScopeID != result[j].ScopeID {
			return result[i].ScopeID < result[j].ScopeID
		}
		if result[i].ArtifactID != result[j].ArtifactID {
			return result[i].ArtifactID < result[j].ArtifactID
		}
		return result[i].BranchID < result[j].BranchID
	})
	return result, nil
}
