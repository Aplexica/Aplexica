package proto

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
)

const RemoteCheckpointCoveragePlanMaxBytes = 64 << 10

// RemoteCheckpointCoverageEntryV1 is content-free proof that a full
// checkpoint returned for one blocked event was authorized by the plugin and
// durably covered that exact cloud-log position. The sealed body never enters
// this plan; BodyDigest binds it without exposing artifact content.
type RemoteCheckpointCoverageEntryV1 struct {
	Index                    uint32 `json:"index"`
	BlockedPosition          uint64 `json:"blocked_position"`
	RequestID                string `json:"request_id"`
	MissingParentHash        string `json:"missing_parent_hash"`
	ArtifactID               string `json:"artifact_id"`
	BranchID                 string `json:"branch_id"`
	Kind                     string `json:"kind"`
	CheckpointEventID        string `json:"checkpoint_event_id"`
	CheckpointEventHash      string `json:"checkpoint_event_hash"`
	CheckpointBodyDigest     string `json:"checkpoint_body_digest"`
	CheckpointAlignmentHash  string `json:"checkpoint_alignment_hash"`
	CheckpointGeneration     string `json:"checkpoint_generation"`
	CheckpointPosition       uint64 `json:"checkpoint_position"`
	CheckpointCursor         string `json:"checkpoint_cursor"`
	CheckpointCursorDigest   string `json:"checkpoint_cursor_digest"`
	CheckpointCoverage       uint64 `json:"checkpoint_coverage"`
	CheckpointCoverageCursor string `json:"checkpoint_coverage_cursor"`
	CheckpointCoverageDigest string `json:"checkpoint_coverage_cursor_digest"`
	AccessGeneration         uint64 `json:"access_generation"`
	AccessSetHash            string `json:"access_set_hash"`
	SecurityGeneration       uint64 `json:"security_generation"`
	SecurityBarrier          string `json:"security_barrier"`
	KeyMode                  string `json:"key_mode"`
	KeyVersion               uint64 `json:"key_version"`
}

func EncodeRemoteCheckpointCoveragePlan(entries []RemoteCheckpointCoverageEntryV1) (string, string, error) {
	if len(entries) == 0 || len(entries) > RemoteReplayBatchMaxEvents {
		return "", "", ErrRemoteReplayBatchInvalid
	}
	for index := range entries {
		if entries[index].Index >= RemoteReplayBatchMaxEvents || index > 0 && entries[index-1].Index >= entries[index].Index {
			return "", "", ErrRemoteReplayBatchInvalid
		}
	}
	encoded, err := json.Marshal(entries)
	if err != nil || len(encoded) == 0 || len(encoded) > RemoteCheckpointCoveragePlanMaxBytes {
		return "", "", ErrRemoteReplayBatchInvalid
	}
	digest := sha256.Sum256(append([]byte("aplexica/checkpoint-covered-finalize/v1\x00"), encoded...))
	return base64.RawURLEncoding.EncodeToString(encoded), hex.EncodeToString(digest[:]), nil
}

func DecodeRemoteCheckpointCoveragePlan(plan, digest string) ([]RemoteCheckpointCoverageEntryV1, error) {
	encoded, err := base64.RawURLEncoding.DecodeString(plan)
	if err != nil || len(encoded) == 0 || len(encoded) > RemoteCheckpointCoveragePlanMaxBytes {
		return nil, ErrRemoteReplayBatchInvalid
	}
	want := sha256.Sum256(append([]byte("aplexica/checkpoint-covered-finalize/v1\x00"), encoded...))
	if digest != hex.EncodeToString(want[:]) {
		return nil, ErrRemoteReplayBatchInvalid
	}
	var entries []RemoteCheckpointCoverageEntryV1
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&entries); err != nil || len(entries) == 0 || len(entries) > RemoteReplayBatchMaxEvents {
		return nil, ErrRemoteReplayBatchInvalid
	}
	canonical, err := json.Marshal(entries)
	if err != nil || !bytes.Equal(canonical, encoded) {
		return nil, ErrRemoteReplayBatchInvalid
	}
	for index := range entries {
		if entries[index].Index >= RemoteReplayBatchMaxEvents || index > 0 && entries[index-1].Index >= entries[index].Index {
			return nil, ErrRemoteReplayBatchInvalid
		}
	}
	return entries, nil
}
