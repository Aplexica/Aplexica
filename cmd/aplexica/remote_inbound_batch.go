package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"

	"github.com/aplexica/aplexica/internal/plugin/proto"
	syncd "github.com/aplexica/aplexica/internal/sync"
)

var errDurableInboundBatchEvidence = errors.New("remote: invalid durable inbound batch evidence")

type durableInboundBatchResultBinding struct {
	Index                     uint32 `json:"index"`
	Outcome                   uint8  `json:"outcome"`
	FinalizeKind              string `json:"finalize_kind"`
	Kind                      string `json:"kind"`
	ArtifactID                string `json:"artifact_id"`
	CanonicalEventID          string `json:"canonical_event_id,omitempty"`
	CanonicalHash             string `json:"canonical_hash,omitempty"`
	NoopReason                string `json:"noop_reason,omitempty"`
	AuthenticatedHeaderDigest string `json:"authenticated_header_digest,omitempty"`
	AuthenticatedSigner       string `json:"authenticated_signer,omitempty"`
}

func durableInboundBatchFinalizeEvidence(
	remoteIdentity string,
	delivery proto.RemoteInboundDeliveryV2,
	results []syncd.ImportOutcome,
	resolve func(proto.RemoteEvent, syncd.ImportOutcome) (syncd.InboundCanonicalEvidence, error),
	recoveries ...*durableGapRecoveryEvidence,
) (proto.RemoteInboundFinalizeEvidenceV1, error) {
	var recovery *durableGapRecoveryEvidence
	if len(recoveries) == 1 {
		recovery = recoveries[0]
	} else if len(recoveries) > 1 {
		return proto.RemoteInboundFinalizeEvidenceV1{}, errDurableInboundBatchEvidence
	}
	if remoteIdentity == "" || resolve == nil || len(delivery.Events) < 2 || len(delivery.Events) > proto.RemoteReplayBatchMaxEvents ||
		len(results) != len(delivery.Events) || delivery.BatchEventCount != uint16(len(delivery.Events)) || !validDurableInboundDigest(delivery.BatchDigest) {
		return proto.RemoteInboundFinalizeEvidenceV1{}, errDurableInboundBatchEvidence
	}
	computed, err := proto.RemoteReplayBatchDigest(delivery)
	if err != nil || computed != delivery.BatchDigest {
		return proto.RemoteInboundFinalizeEvidenceV1{}, errDurableInboundBatchEvidence
	}
	bindings := make([]durableInboundBatchResultBinding, len(results))
	terminal := make(map[string]proto.RemoteBatchMaterializationEntryV1, len(results))
	for index, result := range results {
		if result != syncd.ImportApplied && result != syncd.ImportDeduped && result != syncd.ImportSkipped {
			return proto.RemoteInboundFinalizeEvidenceV1{}, errDurableInboundBatchEvidence
		}
		resolveEvent := delivery.Events[index]
		resolveOutcome := result
		if recovery != nil {
			if covered, ok := recovery.covered[index]; ok {
				resolveEvent = covered.checkpoint.Event
				resolveOutcome = syncd.ImportApplied
			}
		}
		canonical, resolveErr := resolve(resolveEvent, resolveOutcome)
		if resolveErr != nil {
			return proto.RemoteInboundFinalizeEvidenceV1{}, resolveErr
		}
		bindings[index] = durableInboundBatchResultBinding{
			Index: uint32(index), Outcome: uint8(result), FinalizeKind: canonical.FinalizeKind,
			Kind: string(canonical.Kind), ArtifactID: canonical.ArtifactID,
			CanonicalEventID: canonical.EventID, CanonicalHash: canonical.EventHash,
			NoopReason: canonical.NoopReason, AuthenticatedHeaderDigest: canonical.AuthenticatedHeaderDigest,
			AuthenticatedSigner: canonical.AuthenticatedSigner,
		}
		if canonical.FinalizeKind == proto.InboundFinalizeCanonicalMaterialize {
			key := string(canonical.Kind) + "\x00" + canonical.ArtifactID
			terminal[key] = proto.RemoteBatchMaterializationEntryV1{
				Kind: string(canonical.Kind), ArtifactID: canonical.ArtifactID,
				CanonicalEventID: canonical.EventID, CanonicalHash: canonical.EventHash,
			}
		} else if canonical.FinalizeKind != proto.InboundFinalizeAuthenticatedNoop {
			return proto.RemoteInboundFinalizeEvidenceV1{}, errDurableInboundBatchEvidence
		}
	}
	resultPreimage, err := json.Marshal(bindings)
	if err != nil {
		return proto.RemoteInboundFinalizeEvidenceV1{}, err
	}
	resultDigest := sha256.Sum256(append([]byte("aplexica/redaction-safe-replay-batch-result/v1\x00"), resultPreimage...))
	keys := make([]string, 0, len(terminal))
	for key := range terminal {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	entries := make([]proto.RemoteBatchMaterializationEntryV1, 0, len(keys))
	for _, key := range keys {
		entries = append(entries, terminal[key])
	}
	plan, planDigest, err := proto.EncodeRemoteBatchMaterializationPlan(entries)
	if err != nil {
		return proto.RemoteInboundFinalizeEvidenceV1{}, err
	}
	finalizeKind := proto.InboundFinalizeAuthenticatedBatchNoop
	if len(entries) != 0 {
		finalizeKind = proto.InboundFinalizeCanonicalBatch
	}
	coveragePlan, coverageDigest, err := durableCheckpointCoveragePlan(delivery, recovery)
	if err != nil {
		return proto.RemoteInboundFinalizeEvidenceV1{}, err
	}
	return proto.RemoteInboundFinalizeEvidenceV1{
		ProtocolVersion: delivery.ProtocolVersion, FinalizeKind: finalizeKind,
		RemoteIdentity: remoteIdentity, DeliveryID: delivery.DeliveryID,
		StreamID: delivery.StreamID, StreamEpoch: delivery.StreamEpoch, Cursor: delivery.Cursor,
		CursorDigest: delivery.CursorDigest, Position: delivery.Position, NamespaceID: delivery.Events[0].NamespaceID,
		BatchEventCount: delivery.BatchEventCount, BatchDigest: delivery.BatchDigest,
		BatchResultDigest: hex.EncodeToString(resultDigest[:]), BatchMaterializationPlan: plan, BatchMaterializationDigest: planDigest,
		CheckpointCoveragePlan: coveragePlan, CheckpointCoverageDigest: coverageDigest,
	}, nil
}
