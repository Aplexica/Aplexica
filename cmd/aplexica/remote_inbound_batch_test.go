package main

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/daemon"
	"github.com/aplexica/aplexica/internal/plugin/proto"
	"github.com/aplexica/aplexica/internal/securityepoch"
	syncd "github.com/aplexica/aplexica/internal/sync"
	"github.com/stretchr/testify/require"
)

func durableInboundTestBatch(t *testing.T) proto.RemoteInboundDeliveryV2 {
	t.Helper()
	delivery := durableInboundTestDelivery("cursor-2")
	delivery.DeliveryID = "delivery-batch-1"
	delivery.Position = 2
	second := delivery.Events[0]
	second.EventID = "event-2"
	second.Sequence = 2
	second.Timestamp = time.Unix(0, 2).UTC()
	second.Bytes = []byte(`{"sealed":2}`)
	bodyDigest := sha256.Sum256(second.Bytes)
	second.BodyDigest = hex.EncodeToString(bodyDigest[:])
	second.EventHash = strings.Repeat("b", sha256.Size*2)
	delivery.Events = append(delivery.Events, second)
	delivery.BatchEventCount = uint16(len(delivery.Events))
	digest, err := proto.RemoteReplayBatchDigest(delivery)
	require.NoError(t, err)
	delivery.BatchDigest = digest
	return delivery
}

func durableInboundBatchNegotiation() proto.RemoteNegotiateSyncV1Result {
	negotiated := durableInboundTestNegotiation(proto.RemoteSyncModeDurableRead)
	negotiated.ServerCapabilities = append(negotiated.ServerCapabilities, proto.CapabilityRedactionSafeBatchV1)
	return negotiated
}

func batchEvidenceForTest(t *testing.T, delivery proto.RemoteInboundDeliveryV2) proto.RemoteInboundFinalizeEvidenceV1 {
	t.Helper()
	evidence, err := durableInboundBatchFinalizeEvidence(
		"device-1", delivery,
		[]syncd.ImportOutcome{syncd.ImportApplied, syncd.ImportApplied},
		func(event proto.RemoteEvent, _ syncd.ImportOutcome) (syncd.InboundCanonicalEvidence, error) {
			return syncd.InboundCanonicalEvidence{
				FinalizeKind: proto.InboundFinalizeCanonicalMaterialize,
				Kind:         acf.Kind(event.Kind), ArtifactID: event.ArtifactID,
				EventID: event.EventID, EventHash: event.EventHash,
			}, nil
		},
	)
	require.NoError(t, err)
	return evidence
}

func TestBindDurableInboundCursorRequiresExactRedactionBatchCapabilityAndDigest(t *testing.T) {
	delivery := durableInboundTestBatch(t)
	binding, err := bindDurableInboundCursor("device-1", durableInboundBatchNegotiation(), delivery)
	require.NoError(t, err)
	require.Equal(t, uint64(0), binding.predecessor.Position)
	require.Equal(t, uint64(2), binding.next.Position)

	_, err = bindDurableInboundCursor("device-1", durableInboundTestNegotiation(proto.RemoteSyncModeDurableRead), delivery)
	require.ErrorIs(t, err, errDurableInboundMetadata, "mixed-version negotiation must never accept a batch")

	tampered := delivery
	tampered.Events = append([]proto.RemoteEvent(nil), delivery.Events...)
	tampered.Events[0].Bytes = append([]byte(nil), delivery.Events[0].Bytes...)
	tampered.Events[0].Bytes[0] ^= 1
	_, err = bindDurableInboundCursor("device-1", durableInboundBatchNegotiation(), tampered)
	require.ErrorIs(t, err, errDurableInboundMetadata)
}

func TestDurableInboundBatchEvidenceKeepsOnlyTerminalArtifactState(t *testing.T) {
	delivery := durableInboundTestBatch(t)
	evidence := batchEvidenceForTest(t, delivery)
	require.Equal(t, proto.InboundFinalizeCanonicalBatch, evidence.FinalizeKind)
	require.Equal(t, delivery.BatchDigest, evidence.BatchDigest)
	entries, err := proto.DecodeRemoteBatchMaterializationPlan(evidence.BatchMaterializationPlan, evidence.BatchMaterializationDigest)
	require.NoError(t, err)
	require.Equal(t, []proto.RemoteBatchMaterializationEntryV1{{
		Kind: "memory", ArtifactID: "artifact-1", CanonicalEventID: "event-2", CanonicalHash: strings.Repeat("b", 64),
	}}, entries, "two replayed versions of one artifact must produce one terminal native write")
}

func TestDurableInboundBatchFinalizeIsPostAckRestartIdempotentAndTamperSafe(t *testing.T) {
	delivery := durableInboundTestBatch(t)
	evidence := batchEvidenceForTest(t, delivery)
	inbox := &daemon.InboundInbox{Root: filepath.Join(t.TempDir(), "inbox")}
	admission, cached, err := inbox.AdmitDurable(delivery, securityepoch.SecurityEpoch{}, "device-1")
	require.NoError(t, err)
	require.Nil(t, cached)
	outcomes := make([]proto.RemoteInboundEventOutcomeV2, len(delivery.Events))
	for index := range outcomes {
		outcomes[index] = proto.RemoteInboundEventOutcomeV2{Index: uint32(index), Disposition: "accepted", ReasonCode: "durable"}
	}
	require.NoError(t, inbox.CompleteDurable(delivery.DeliveryID, admission.InputSHA256, proto.RemoteInboundAckV2{
		DeliveryID: delivery.DeliveryID, Outcomes: outcomes, NextCursor: delivery.Cursor,
		NextCursorDigest: delivery.CursorDigest, NextPosition: delivery.Position, FinalizeEvidence: &evidence,
	}))
	cursors := &daemon.DurableCursorStore{Root: filepath.Join(t.TempDir(), "cursors")}
	key := daemon.DurableCursorKey{RemoteIdentity: "device-1", StreamID: delivery.StreamID, StreamEpoch: delivery.StreamEpoch}
	_, err = cursors.Load(key)
	require.ErrorIs(t, err, daemon.ErrDurableCursorNotFound, "terminal inbox fsync must complete before the cursor is advanced separately")
	repair := &durableInboundRestartRepair{}
	require.NoError(t, repair.ensure(inbox, cursors), "restart must repair the complete batch span from terminal inbox evidence")
	repaired, err := cursors.Load(key)
	require.NoError(t, err)
	require.Equal(t, delivery.Position, repaired.Position)
	require.Equal(t, delivery.Cursor, repaired.Cursor)
	require.NoError(t, (&durableInboundRestartRepair{}).ensure(&daemon.InboundInbox{Root: inbox.Root}, cursors), "a second restart must be idempotent")

	var gate sync.Mutex
	var materialized []syncd.InboundCanonicalEvidence
	result := handleDurableInboundFinalize(&gate, inbox, cursors, "device-1", durableInboundBatchNegotiation(), proto.RemoteInboundFinalizeV1Params{Evidence: evidence}, func(value syncd.InboundCanonicalEvidence) error {
		materialized = append(materialized, value)
		return nil
	})
	require.True(t, result.Accepted)
	require.True(t, result.Materialized)
	require.Len(t, materialized, 1)
	require.Equal(t, "event-2", materialized[0].EventID)

	result = handleDurableInboundFinalize(&gate, &daemon.InboundInbox{Root: inbox.Root}, cursors, "device-1", durableInboundBatchNegotiation(), proto.RemoteInboundFinalizeV1Params{Evidence: evidence}, func(value syncd.InboundCanonicalEvidence) error {
		materialized = append(materialized, value)
		return nil
	})
	require.True(t, result.Accepted)
	require.True(t, result.AlreadyFinalized)
	require.Len(t, materialized, 1, "restart retry must not repeat a committed terminal materialization")

	tampered := evidence
	tampered.BatchMaterializationDigest = strings.Repeat("0", 64)
	result = handleDurableInboundFinalize(&gate, inbox, cursors, "device-1", durableInboundBatchNegotiation(), proto.RemoteInboundFinalizeV1Params{Evidence: tampered}, func(syncd.InboundCanonicalEvidence) error { return nil })
	require.False(t, result.Accepted)
	require.Equal(t, "metadata-invalid", result.ReasonCode)
}
