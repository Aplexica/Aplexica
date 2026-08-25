package proto

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
)

const (
	RemoteReplayBatchMaxEvents         = 16
	RemoteBatchMaterializationMaxBytes = 64 << 10
)

var ErrRemoteReplayBatchInvalid = errors.New("plugin/proto: invalid replay batch")

// remoteReplayBatchDigestEvent is the frozen digest preimage mirrored by the
// cloud plugin. Payload contains sealed bytes, never plaintext at this layer.
type remoteReplayBatchDigestEvent struct {
	ProjectID                      string                  `json:"project_id,omitempty"`
	ProjectAuthorizationGeneration uint64                  `json:"project_authorization_generation,omitempty"`
	AccessGeneration               uint64                  `json:"access_generation,omitempty"`
	AccessSetHash                  [RemoteDigestBytes]byte `json:"access_set_hash,omitempty"`
	SecurityBarrierID              [RemoteDigestBytes]byte `json:"security_barrier_id,omitempty"`
	SecurityGeneration             uint64                  `json:"security_generation,omitempty"`
	KeyMode                        string                  `json:"key_mode,omitempty"`
	KeyVersion                     uint64                  `json:"key_version,omitempty"`
	CheckpointCoverage             uint64                  `json:"checkpoint_coverage,omitempty"`
	CheckpointGeneration           string                  `json:"checkpoint_generation,omitempty"`
	NamespaceID                    string                  `json:"namespace_id"`
	BranchID                       string                  `json:"branch_id"`
	ArtifactID                     string                  `json:"artifact_id"`
	EventID                        string                  `json:"event_id"`
	ParentHash                     string                  `json:"parent_hash,omitempty"`
	CheckpointAlignmentHash        string                  `json:"checkpoint_alignment_hash,omitempty"`
	EventHash                      string                  `json:"event_hash,omitempty"`
	BodyDigest                     string                  `json:"body_digest"`
	Kind                           string                  `json:"kind"`
	EventType                      string                  `json:"event_type"`
	TimestampUnixNano              int64                   `json:"timestamp_unix_nano"`
	Sequence                       uint64                  `json:"sequence"`
	Origin                         string                  `json:"origin"`
	SourceAgent                    string                  `json:"source_agent,omitempty"`
	Lane                           string                  `json:"lane,omitempty"`
	Clear                          bool                    `json:"clear,omitempty"`
	Payload                        []byte                  `json:"payload"`
}

type remoteReplayBatchDigestBinding struct {
	ProtocolVersion     uint16                         `json:"protocol_version"`
	StreamID            string                         `json:"stream_id"`
	StreamEpoch         string                         `json:"stream_epoch"`
	PredecessorCursor   string                         `json:"predecessor_cursor"`
	PredecessorPosition uint64                         `json:"predecessor_position"`
	Cursor              string                         `json:"cursor"`
	CursorDigest        string                         `json:"cursor_digest"`
	Position            uint64                         `json:"position"`
	Events              []remoteReplayBatchDigestEvent `json:"events"`
}

func RemoteReplayBatchDigest(delivery RemoteInboundDeliveryV2) (string, error) {
	if delivery.ProtocolVersion != 1 || len(delivery.Events) < 2 || len(delivery.Events) > RemoteReplayBatchMaxEvents ||
		delivery.StagedCheckpoint != nil || delivery.BatchEventCount != uint16(len(delivery.Events)) {
		return "", ErrRemoteReplayBatchInvalid
	}
	events := make([]remoteReplayBatchDigestEvent, len(delivery.Events))
	for index, event := range delivery.Events {
		events[index] = remoteReplayBatchDigestEvent{
			ProjectID: event.ProjectID, ProjectAuthorizationGeneration: event.ProjectAuthorizationGeneration,
			AccessGeneration: event.AccessGeneration, AccessSetHash: event.AccessSetHash,
			SecurityBarrierID: event.SecurityBarrierID, SecurityGeneration: event.SecurityGeneration,
			KeyMode: event.KeyMode, KeyVersion: event.KeyVersion,
			CheckpointCoverage: event.CheckpointCoverage, CheckpointGeneration: event.CheckpointGeneration,
			NamespaceID: event.NamespaceID, BranchID: event.BranchID, ArtifactID: event.ArtifactID,
			EventID: event.EventID, ParentHash: event.ParentHash, CheckpointAlignmentHash: event.CheckpointAlignmentHash,
			EventHash: event.EventHash, BodyDigest: event.BodyDigest, Kind: event.Kind, EventType: event.Type,
			TimestampUnixNano: event.Timestamp.UnixNano(), Sequence: event.Sequence, Origin: event.Origin,
			SourceAgent: event.SourceAgent, Lane: event.Lane, Clear: event.Clear, Payload: append([]byte(nil), event.Bytes...),
		}
	}
	preimage, err := json.Marshal(remoteReplayBatchDigestBinding{
		ProtocolVersion: delivery.ProtocolVersion, StreamID: delivery.StreamID, StreamEpoch: delivery.StreamEpoch,
		PredecessorCursor: delivery.PredecessorCursor, PredecessorPosition: delivery.PredecessorPosition,
		Cursor: delivery.Cursor, CursorDigest: delivery.CursorDigest, Position: delivery.Position, Events: events,
	})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(append([]byte("aplexica/redaction-safe-replay-batch/v1\x00"), preimage...))
	return hex.EncodeToString(digest[:]), nil
}

type RemoteBatchMaterializationEntryV1 struct {
	Kind             string `json:"kind"`
	ArtifactID       string `json:"artifact_id"`
	CanonicalEventID string `json:"canonical_event_id"`
	CanonicalHash    string `json:"canonical_hash"`
}

func EncodeRemoteBatchMaterializationPlan(entries []RemoteBatchMaterializationEntryV1) (string, string, error) {
	encoded, err := json.Marshal(entries)
	if err != nil || len(encoded) == 0 || len(encoded) > RemoteBatchMaterializationMaxBytes {
		return "", "", ErrRemoteReplayBatchInvalid
	}
	digest := sha256.Sum256(encoded)
	return base64.RawURLEncoding.EncodeToString(encoded), hex.EncodeToString(digest[:]), nil
}

func DecodeRemoteBatchMaterializationPlan(plan, digest string) ([]RemoteBatchMaterializationEntryV1, error) {
	encoded, err := base64.RawURLEncoding.DecodeString(plan)
	if err != nil || len(encoded) == 0 || len(encoded) > RemoteBatchMaterializationMaxBytes {
		return nil, ErrRemoteReplayBatchInvalid
	}
	want := sha256.Sum256(encoded)
	if digest != hex.EncodeToString(want[:]) {
		return nil, ErrRemoteReplayBatchInvalid
	}
	var entries []RemoteBatchMaterializationEntryV1
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&entries); err != nil || len(entries) > RemoteReplayBatchMaxEvents {
		return nil, ErrRemoteReplayBatchInvalid
	}
	canonical, err := json.Marshal(entries)
	if err != nil || !bytes.Equal(canonical, encoded) {
		return nil, ErrRemoteReplayBatchInvalid
	}
	return entries, nil
}
