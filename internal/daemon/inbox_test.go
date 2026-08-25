package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/plugin/proto"
	"github.com/aplexica/aplexica/internal/securityepoch"
	"github.com/aplexica/aplexica/internal/securityerr"
	"github.com/stretchr/testify/require"
)

func TestInboundInboxOpaqueBoundsAllowCloudCursorWithoutWideningDeliveryID(t *testing.T) {
	require.True(t, validOpaqueDeliveryValue(strings.Repeat("d", proto.MaxDeliveryIDBytes), proto.MaxDeliveryIDBytes))
	require.False(t, validOpaqueDeliveryValue(strings.Repeat("d", proto.MaxDeliveryIDBytes+1), proto.MaxDeliveryIDBytes))
	require.True(t, validOpaqueDeliveryValue(strings.Repeat("c", proto.MaxDurableCursorBytes), proto.MaxDurableCursorBytes))
	require.False(t, validOpaqueDeliveryValue(strings.Repeat("c", proto.MaxDurableCursorBytes+1), proto.MaxDurableCursorBytes))
}

func TestInboundInboxAdmitsCloudCursorAtProtocolBound(t *testing.T) {
	inbox := &InboundInbox{Root: filepath.Join(t.TempDir(), "inbox")}
	delivery := proto.RemoteInboundDeliveryV2{
		DeliveryID: "delivery-max-cursor",
		Cursor:     strings.Repeat("c", proto.MaxDurableCursorBytes),
		Events:     []proto.RemoteEvent{{EventID: "event-1"}},
	}
	_, cached, err := inbox.Admit(delivery, securityepoch.SecurityEpoch{})
	require.NoError(t, err)
	require.Nil(t, cached)

	delivery.DeliveryID = "delivery-over-cursor"
	delivery.Cursor += "c"
	_, _, err = inbox.Admit(delivery, securityepoch.SecurityEpoch{})
	require.Error(t, err)
}

func TestInboundInboxDurablyDeduplicatesTerminalAck(t *testing.T) {
	inbox := &InboundInbox{Root: filepath.Join(t.TempDir(), "inbox")}
	epoch := securityepoch.SecurityEpoch{CoordinatorGeneration: 1, AccessGeneration: 1, AccessSetHash: sha256.Sum256([]byte("access")), BarrierID: sha256.Sum256([]byte("barrier")), KeyMode: "recipient-wrap-v2"}
	delivery := proto.RemoteInboundDeliveryV2{DeliveryID: "delivery-1", Cursor: "cursor-1", Events: []proto.RemoteEvent{{EventID: "event-1", AccessGeneration: 1, AccessSetHash: epoch.AccessSetHash, SecurityBarrierID: epoch.BarrierID, SecurityGeneration: 1, KeyMode: "recipient-wrap-v2"}}}
	admission, cached, err := inbox.Admit(delivery, epoch)
	if err != nil || cached != nil {
		t.Fatalf("admit = %+v, %v", cached, err)
	}
	pending, err := inbox.PendingIDs()
	if err != nil || len(pending) != 1 || pending[0] != delivery.DeliveryID {
		t.Fatalf("pending = %v, %v", pending, err)
	}
	ack := proto.RemoteInboundAckV2{DeliveryID: delivery.DeliveryID, NextCursor: delivery.Cursor, Outcomes: []proto.RemoteInboundEventOutcomeV2{{Index: 0, Disposition: "accepted", ReasonCode: "durable"}}}
	if err := inbox.Complete(delivery.DeliveryID, admission.InputSHA256, ack); err != nil {
		t.Fatal(err)
	}
	_, cached, err = inbox.Admit(delivery, epoch)
	if err != nil || cached == nil || cached.NextCursor != delivery.Cursor {
		t.Fatalf("cached ack = %+v, %v", cached, err)
	}
	delivery.Events[0].EventID = "substituted"
	if _, _, err := inbox.Admit(delivery, epoch); err == nil {
		t.Fatal("delivery id replay with substituted input accepted")
	}
}

func TestInboundInboxDurablyQuarantinesPreAdmissionTerminalDelivery(t *testing.T) {
	inbox := &InboundInbox{Root: filepath.Join(t.TempDir(), "inbox")}
	epochHash := sha256.Sum256([]byte("claimed-access"))
	barrier := sha256.Sum256([]byte("claimed-barrier"))
	delivery := proto.RemoteInboundDeliveryV2{
		DeliveryID: "legacy-delivery-1",
		Cursor:     "legacy-cursor-1",
		Events: []proto.RemoteEvent{{
			EventID: "legacy-event-1", Bytes: json.RawMessage(`{"v":1,"ct":"opaque"}`),
			AccessGeneration: 1, AccessSetHash: epochHash, SecurityBarrierID: barrier,
			SecurityGeneration: 1, KeyMode: "recipient-wrap-v2",
		}},
	}
	ack := proto.RemoteInboundAckV2{
		DeliveryID: delivery.DeliveryID,
		NextCursor: delivery.Cursor,
		Outcomes: []proto.RemoteInboundEventOutcomeV2{{
			Index: 0, Disposition: "quarantined", ReasonCode: "envelope-version-below-minimum",
		}},
	}
	got, err := inbox.QuarantineTerminal(delivery, ack)
	require.NoError(t, err)
	require.Equal(t, ack, *got)

	// Exact redelivery returns the durable terminal acknowledgement and never
	// appears in the pending-admission list.
	cached, err := inbox.QuarantineTerminal(delivery, ack)
	require.NoError(t, err)
	require.Equal(t, ack, *cached)
	pending, err := inbox.PendingIDs()
	require.NoError(t, err)
	require.Empty(t, pending)

	changed := delivery
	changed.Events = append([]proto.RemoteEvent(nil), delivery.Events...)
	changed.Events[0].Bytes = json.RawMessage(`{"v":1,"ct":"different"}`)
	_, err = inbox.QuarantineTerminal(changed, ack)
	require.ErrorIs(t, err, securityerr.ErrMetadataMismatch)
}

func TestInboundInboxRejectsRetryablePreAdmissionQuarantineRecord(t *testing.T) {
	inbox := &InboundInbox{Root: filepath.Join(t.TempDir(), "inbox")}
	delivery := proto.RemoteInboundDeliveryV2{DeliveryID: "delivery-1", Cursor: "cursor-1", Events: []proto.RemoteEvent{{EventID: "event-1"}}}
	ack := proto.RemoteInboundAckV2{DeliveryID: delivery.DeliveryID, Outcomes: []proto.RemoteInboundEventOutcomeV2{{Index: 0, Disposition: "retryable", ReasonCode: "temporary"}}}
	_, err := inbox.QuarantineTerminal(delivery, ack)
	require.ErrorIs(t, err, securityerr.ErrMetadataMismatch)
}

func durableInboxTestDelivery(position uint64) proto.RemoteInboundDeliveryV2 {
	predecessor := "signed-cursor-" + string(rune('a'+position-1))
	cursor := "signed-cursor-" + string(rune('a'+position))
	digest := sha256.Sum256([]byte(cursor))
	body := json.RawMessage(`{}`)
	bodyDigest := sha256.Sum256(body)
	return proto.RemoteInboundDeliveryV2{
		DeliveryID: "durable-delivery-" + string(rune('a'+position)), Cursor: cursor,
		ProtocolVersion: 1, StreamID: "stream-1", StreamEpoch: "epoch-1",
		PredecessorCursor: predecessor, PredecessorPosition: position - 1, Position: position,
		CursorDigest: hex.EncodeToString(digest[:]),
		Events: []proto.RemoteEvent{{
			NamespaceID: "namespace-1", BranchID: "main", Kind: "memory", ArtifactID: "artifact-1",
			EventID: "event-" + string(rune('a'+position)), EventHash: strings.Repeat(string(rune('a'+position)), sha256.Size*2),
			BodyDigest: hex.EncodeToString(bodyDigest[:]), Type: "update", Timestamp: time.Unix(0, int64(position)).UTC(),
			Sequence: position, Origin: "device-2", SourceAgent: "codex", Lane: "live", Bytes: body,
		}},
	}
}

func TestInboundInboxPersistsStagedCheckpointDescriptorWithoutLargeBody(t *testing.T) {
	inbox := &InboundInbox{Root: filepath.Join(t.TempDir(), "inbox")}
	delivery := durableInboxTestDelivery(2)
	event := &delivery.Events[0]
	event.Bytes = nil
	event.Lane = "retained"
	event.CheckpointCoverage = 1
	event.CheckpointGeneration = strings.Repeat("c", 64)
	event.CheckpointAlignmentHash = strings.Repeat("d", 64)
	event.BodyDigest = strings.Repeat("b", 64)
	staged := &proto.RemoteStagedFileV1{
		ProtocolVersion: proto.RemoteStagedTransferProtocolV1, TransferID: strings.Repeat("a", 64),
		SealedBytes: proto.MaxSealedEventBytes + 17, BodyDigest: event.BodyDigest,
		StreamID: delivery.StreamID, StreamEpoch: delivery.StreamEpoch,
	}
	staged.BindingDigest = proto.RemoteStagedBindingDigest(*event, *staged)
	delivery.StagedCheckpoint = staged

	admission, cached, err := inbox.AdmitDurable(delivery, securityepoch.SecurityEpoch{}, "device-1")
	require.NoError(t, err)
	require.Nil(t, cached)
	require.Equal(t, staged.BodyDigest, admission.DurableBodyDigest)

	name, err := inboxName(delivery.DeliveryID)
	require.NoError(t, err)
	info, err := os.Stat(filepath.Join(inbox.Root, name))
	require.NoError(t, err)
	require.Less(t, info.Size(), int64(32<<10), "descriptor-only inbox record must stay small")
	root, err := inbox.root()
	require.NoError(t, err)
	record, err := readInboxRecord(root, name)
	require.NoError(t, err)
	require.NoError(t, root.Close())
	require.NotNil(t, record.Delivery)
	require.Equal(t, "null", string(record.Delivery.Events[0].Bytes), "frozen event JSON carries only the null payload sentinel")
	require.Equal(t, staged, record.Delivery.StagedCheckpoint)

	restarted := &InboundInbox{Root: inbox.Root}
	replayed, replayedAck, err := restarted.AdmitDurable(delivery, securityepoch.SecurityEpoch{}, "device-1")
	require.NoError(t, err)
	require.Nil(t, replayedAck)
	require.Equal(t, admission, replayed)

	tampered := delivery
	tampered.Events = append([]proto.RemoteEvent(nil), delivery.Events...)
	clone := *staged
	clone.BindingDigest = strings.Repeat("f", 64)
	tampered.StagedCheckpoint = &clone
	_, _, err = restarted.AdmitDurable(tampered, securityepoch.SecurityEpoch{}, "device-1")
	require.ErrorIs(t, err, securityerr.ErrMetadataMismatch)
}

func completeDurableInboxTestDelivery(t *testing.T, inbox *InboundInbox, delivery proto.RemoteInboundDeliveryV2) proto.RemoteInboundFinalizeEvidenceV1 {
	t.Helper()
	admission, cached, err := inbox.AdmitDurable(delivery, securityepoch.SecurityEpoch{}, "device-1")
	require.NoError(t, err)
	require.Nil(t, cached)
	event := delivery.Events[0]
	evidence := proto.RemoteInboundFinalizeEvidenceV1{
		ProtocolVersion: 1, FinalizeKind: proto.InboundFinalizeCanonicalMaterialize,
		RemoteIdentity: "device-1", DeliveryID: delivery.DeliveryID,
		StreamID: delivery.StreamID, StreamEpoch: delivery.StreamEpoch, Cursor: delivery.Cursor,
		CursorDigest: delivery.CursorDigest, Position: delivery.Position, NamespaceID: event.NamespaceID,
		BranchID: event.BranchID, Kind: event.Kind, ArtifactID: event.ArtifactID, WireEventID: event.EventID,
		WireEventHash: event.EventHash, BodyDigest: event.BodyDigest, ParentHash: event.ParentHash,
		CheckpointAlignmentHash: event.CheckpointAlignmentHash,
		EventType:               event.Type, TimestampUnixNano: event.Timestamp.UnixNano(), Sequence: event.Sequence,
		Origin: event.Origin, SourceAgent: event.SourceAgent, Lane: event.Lane, Clear: event.Clear,
		CanonicalEventID: "canonical-" + event.EventID, CanonicalHash: strings.Repeat("f", sha256.Size*2),
	}
	require.NoError(t, inbox.CompleteDurable(delivery.DeliveryID, admission.InputSHA256, proto.RemoteInboundAckV2{
		DeliveryID: delivery.DeliveryID, NextCursor: delivery.Cursor, NextCursorDigest: delivery.CursorDigest,
		NextPosition: delivery.Position, Outcomes: []proto.RemoteInboundEventOutcomeV2{{Index: 0, Disposition: "accepted", ReasonCode: "durable"}},
		FinalizeEvidence: &evidence,
	}))
	return evidence
}

func TestInboundInboxReturnsOnlyExactCurrentPendingFinalizeEvidence(t *testing.T) {
	inbox := &InboundInbox{Root: filepath.Join(t.TempDir(), "inbox")}
	delivery := durableInboxTestDelivery(1)
	evidence := completeDurableInboxTestDelivery(t, inbox, delivery)
	key := DurableCursorKey{RemoteIdentity: "device-1", StreamID: delivery.StreamID, StreamEpoch: delivery.StreamEpoch}
	state := DurableCursorState{Cursor: delivery.Cursor, CursorDigest: delivery.CursorDigest, Position: delivery.Position, Revision: 1}

	pending, err := inbox.PendingFinalizeEvidence(key, state)
	require.NoError(t, err)
	require.Equal(t, &evidence, pending)

	wrong := state
	wrong.Cursor = "different-cursor"
	_, err = inbox.PendingFinalizeEvidence(key, wrong)
	require.ErrorIs(t, err, securityerr.ErrMetadataMismatch)

	_, err = inbox.MarkInboundFinalized(evidence)
	require.NoError(t, err)
	retained, finalized, err := (&InboundInbox{Root: inbox.Root}).RetainedFinalizeEvidenceAtCursor(key, state)
	require.NoError(t, err)
	require.True(t, finalized)
	require.Equal(t, &evidence, retained, "finalized current evidence remains available to validate a plugin retry proposal")
	pending, err = (&InboundInbox{Root: inbox.Root}).PendingFinalizeEvidence(key, state)
	require.NoError(t, err)
	require.Nil(t, pending)
}

func TestInboundInboxBindsCheckpointAlignmentWithoutOverloadingParent(t *testing.T) {
	inbox := &InboundInbox{Root: filepath.Join(t.TempDir(), "inbox")}
	delivery := durableInboxTestDelivery(1)
	event := &delivery.Events[0]
	event.Lane = "retained"
	event.ParentHash = ""
	event.CheckpointCoverage = 1
	event.CheckpointGeneration = strings.Repeat("b", sha256.Size*2)
	event.CheckpointAlignmentHash = strings.Repeat("a", sha256.Size*2)

	evidence := completeDurableInboxTestDelivery(t, inbox, delivery)
	require.Empty(t, evidence.ParentHash)
	require.Equal(t, event.CheckpointAlignmentHash, evidence.CheckpointAlignmentHash)

	tampered := evidence
	tampered.CheckpointAlignmentHash = strings.Repeat("c", sha256.Size*2)
	_, err := inbox.PrepareInboundFinalize(tampered)
	require.ErrorIs(t, err, securityerr.ErrMetadataMismatch)
}

func TestInboundInboxPrunesCompletedDurableOnlyThroughExactPersistedCursor(t *testing.T) {
	root := t.TempDir()
	inbox := &InboundInbox{Root: filepath.Join(root, "inbox")}
	cursors := &DurableCursorStore{Root: filepath.Join(root, "cursors")}
	key := DurableCursorKey{RemoteIdentity: "device-1", StreamID: "stream-1", StreamEpoch: "epoch-1"}
	deliveries := []proto.RemoteInboundDeliveryV2{durableInboxTestDelivery(1), durableInboxTestDelivery(2), durableInboxTestDelivery(3)}
	for _, delivery := range deliveries {
		evidence := completeDurableInboxTestDelivery(t, inbox, delivery)
		_, err := inbox.MarkInboundFinalized(evidence)
		require.NoError(t, err)
	}
	first, err := cursors.CompareAndSwap(key, nil, DurableCursorState{Cursor: deliveries[0].Cursor, CursorDigest: deliveries[0].CursorDigest, Position: 1})
	require.NoError(t, err)
	_, err = cursors.CompareAndSwap(key, &first, DurableCursorState{Cursor: deliveries[1].Cursor, CursorDigest: deliveries[1].CursorDigest, Position: 2})
	require.NoError(t, err)

	removed, err := inbox.PruneCompletedDurable(cursors, key)
	require.NoError(t, err)
	require.Equal(t, 1, removed)
	remaining, err := inbox.CompletedDurable()
	require.NoError(t, err)
	require.Len(t, remaining, 2)
	require.Equal(t, uint64(2), remaining[0].Position, "the exact current record remains retry evidence")
	require.Equal(t, uint64(3), remaining[1].Position)

	removed, err = (&InboundInbox{Root: inbox.Root}).PruneCompletedDurable(&DurableCursorStore{Root: cursors.Root}, key)
	require.NoError(t, err)
	require.Zero(t, removed, "pruning remains restart-idempotent when no older completion remains")
}

func TestInboundInboxPruningFailsClosedWithoutExactEvidenceRecord(t *testing.T) {
	root := t.TempDir()
	inbox := &InboundInbox{Root: filepath.Join(root, "inbox")}
	cursors := &DurableCursorStore{Root: filepath.Join(root, "cursors")}
	key := DurableCursorKey{RemoteIdentity: "device-1", StreamID: "stream-1", StreamEpoch: "epoch-1"}
	firstDelivery, secondDelivery := durableInboxTestDelivery(1), durableInboxTestDelivery(2)
	firstEvidence := completeDurableInboxTestDelivery(t, inbox, firstDelivery)
	secondEvidence := completeDurableInboxTestDelivery(t, inbox, secondDelivery)
	_, err := inbox.MarkInboundFinalized(firstEvidence)
	require.NoError(t, err)
	_, err = inbox.MarkInboundFinalized(secondEvidence)
	require.NoError(t, err)
	first, err := cursors.CompareAndSwap(key, nil, DurableCursorState{Cursor: firstDelivery.Cursor, CursorDigest: firstDelivery.CursorDigest, Position: 1})
	require.NoError(t, err)
	_, err = cursors.CompareAndSwap(key, &first, DurableCursorState{Cursor: secondDelivery.Cursor, CursorDigest: secondDelivery.CursorDigest, Position: 2})
	require.NoError(t, err)
	exactName, err := inboxName(secondDelivery.DeliveryID)
	require.NoError(t, err)
	require.NoError(t, os.Remove(filepath.Join(inbox.Root, exactName)))

	_, err = inbox.PruneCompletedDurable(cursors, key)
	require.ErrorIs(t, err, securityerr.ErrMetadataMismatch)
	olderName, err := inboxName(firstDelivery.DeliveryID)
	require.NoError(t, err)
	_, statErr := os.Stat(filepath.Join(inbox.Root, olderName))
	require.NoError(t, statErr, "older completion must remain when exact cursor evidence is absent")
}

func TestInboundInboxFinalizeIsExactRestartIdempotentAndUnfinalizedCannotPrune(t *testing.T) {
	root := t.TempDir()
	inbox := &InboundInbox{Root: filepath.Join(root, "inbox")}
	cursors := &DurableCursorStore{Root: filepath.Join(root, "cursors")}
	key := DurableCursorKey{RemoteIdentity: "device-1", StreamID: "stream-1", StreamEpoch: "epoch-1"}
	delivery := durableInboxTestDelivery(1)
	evidence := completeDurableInboxTestDelivery(t, inbox, delivery)
	_, err := cursors.CompareAndSwap(key, nil, DurableCursorState{Cursor: delivery.Cursor, CursorDigest: delivery.CursorDigest, Position: 1})
	require.NoError(t, err)

	already, err := (&InboundInbox{Root: inbox.Root}).PrepareInboundFinalize(evidence)
	require.NoError(t, err)
	require.False(t, already)
	removed, err := inbox.PruneCompletedDurable(cursors, key)
	require.NoError(t, err)
	require.Zero(t, removed, "unfinalized exact evidence must never be pruned")

	wrong := evidence
	wrong.CanonicalHash = strings.Repeat("0", sha256.Size*2)
	_, err = inbox.PrepareInboundFinalize(wrong)
	require.ErrorIs(t, err, securityerr.ErrMetadataMismatch)

	already, err = inbox.MarkInboundFinalized(evidence)
	require.NoError(t, err)
	require.False(t, already)
	already, err = (&InboundInbox{Root: inbox.Root}).PrepareInboundFinalize(evidence)
	require.NoError(t, err)
	require.True(t, already, "finalized marker must survive restart")
	already, err = (&InboundInbox{Root: inbox.Root}).MarkInboundFinalized(evidence)
	require.NoError(t, err)
	require.True(t, already, "exact finalize retry must be idempotent")
}

func TestInboundInboxBackpressuresAtBoundWithoutEvictingUnfinalizedEvidence(t *testing.T) {
	inbox := &InboundInbox{Root: filepath.Join(t.TempDir(), "inbox")}
	first := durableInboxTestDelivery(1)
	evidence := completeDurableInboxTestDelivery(t, inbox, first)
	pressure := filepath.Join(inbox.Root, "pressure.json")
	require.NoError(t, os.WriteFile(pressure, []byte("x"), 0o600))
	require.NoError(t, os.Truncate(pressure, inboxMaxBytes))

	second := durableInboxTestDelivery(2)
	second.PredecessorCursor = first.Cursor
	second.PredecessorPosition = 1
	_, _, err := inbox.AdmitDurable(second, securityepoch.SecurityEpoch{}, "device-1")
	require.ErrorIs(t, err, securityerr.ErrLimitExceeded, "bounded inbox must backpressure rather than evict terminal obligations")
	already, err := inbox.PrepareInboundFinalize(evidence)
	require.NoError(t, err)
	require.False(t, already, "unfinalized evidence must survive admission pressure")
}

func TestCompletedDurableIgnoresPendingCrashRecord(t *testing.T) {
	inbox := &InboundInbox{Root: filepath.Join(t.TempDir(), "inbox")}
	delivery := durableInboxTestDelivery(1)
	_, cached, err := inbox.AdmitDurable(delivery, securityepoch.SecurityEpoch{}, "device-1")
	require.NoError(t, err)
	require.Nil(t, cached)
	completed, err := (&InboundInbox{Root: inbox.Root}).CompletedDurable()
	require.NoError(t, err)
	require.Empty(t, completed)

	root, err := inbox.root()
	require.NoError(t, err)
	name, err := inboxName(delivery.DeliveryID)
	require.NoError(t, err)
	record, err := readInboxRecord(root, name)
	require.NoError(t, err)
	record.Admission.DurableCursorDigest = strings.Repeat("f", 64)
	require.NoError(t, writeInboxRecord(root, name, record))
	require.NoError(t, root.Close())
	_, err = (&InboundInbox{Root: inbox.Root}).CompletedDurable()
	require.ErrorIs(t, err, securityerr.ErrMetadataMismatch, "malformed pending durable metadata must still fail closed")
}
