package main

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/daemon"
	"github.com/aplexica/aplexica/internal/plugin/proto"
	"github.com/aplexica/aplexica/internal/securityepoch"
	syncd "github.com/aplexica/aplexica/internal/sync"
	"github.com/stretchr/testify/require"
)

func durableInboundTestNegotiation(mode string) proto.RemoteNegotiateSyncV1Result {
	return proto.RemoteNegotiateSyncV1Result{
		SelectedProtocol:   1,
		Mode:               mode,
		ServerCapabilities: []string{proto.CapabilityDurableDeltaSyncV1, proto.CapabilityInboundAckV2, proto.CapabilityInboundFinalizeV1},
		FeatureGateEnabled: true,
		StreamID:           "stream-1",
		StreamEpoch:        "epoch-1",
	}
}

func durableInboundTestMultiNegotiation(mode string) proto.RemoteNegotiateSyncV1Result {
	negotiated := durableInboundTestNegotiation(mode)
	negotiated.ServerCapabilities = append(negotiated.ServerCapabilities, proto.CapabilityDurableMultiStreamV1)
	negotiated.Streams = []proto.RemoteStreamDescriptorV1{
		{NamespaceID: "", StreamID: negotiated.StreamID, StreamEpoch: negotiated.StreamEpoch},
		{NamespaceID: "namespace-1", StreamID: "namespace-stream-1", StreamEpoch: "namespace-epoch-1"},
	}
	return negotiated
}

func durableInboundTestDelivery(cursor string) proto.RemoteInboundDeliveryV2 {
	digest := sha256.Sum256([]byte(cursor))
	body := []byte(`{}`)
	bodyDigest := sha256.Sum256(body)
	return proto.RemoteInboundDeliveryV2{
		DeliveryID:        "delivery-1",
		Cursor:            cursor,
		ProtocolVersion:   1,
		StreamID:          "stream-1",
		StreamEpoch:       "epoch-1",
		PredecessorCursor: "cursor-position-zero",
		Position:          1,
		CursorDigest:      hex.EncodeToString(digest[:]),
		Events: []proto.RemoteEvent{{
			NamespaceID: "namespace-1",
			BranchID:    "main",
			ArtifactID:  "artifact-1",
			EventID:     "event-1",
			EventHash:   strings.Repeat("a", sha256.Size*2),
			BodyDigest:  hex.EncodeToString(bodyDigest[:]),
			Kind:        "memory",
			Type:        "update",
			Timestamp:   time.Unix(0, 1).UTC(),
			Sequence:    1,
			Origin:      "device-2",
			SourceAgent: "codex",
			Lane:        "live",
			Bytes:       body,
		}},
	}
}

func durableInboundTestSuccessor(previousCursor, cursor string, position uint64) proto.RemoteInboundDeliveryV2 {
	delivery := durableInboundTestDelivery(cursor)
	delivery.PredecessorCursor = previousCursor
	delivery.PredecessorPosition = position - 1
	delivery.Position = position
	delivery.Events[0].EventID = "event-successor"
	return delivery
}

func durableInboundTestFinalizeEvidence(delivery proto.RemoteInboundDeliveryV2) proto.RemoteInboundFinalizeEvidenceV1 {
	event := delivery.Events[0]
	return proto.RemoteInboundFinalizeEvidenceV1{
		ProtocolVersion: delivery.ProtocolVersion, FinalizeKind: proto.InboundFinalizeCanonicalMaterialize,
		RemoteIdentity: "device-1", DeliveryID: delivery.DeliveryID,
		StreamID: delivery.StreamID, StreamEpoch: delivery.StreamEpoch, Cursor: delivery.Cursor,
		CursorDigest: delivery.CursorDigest, Position: delivery.Position, NamespaceID: event.NamespaceID,
		BranchID: event.BranchID, Kind: event.Kind, ArtifactID: event.ArtifactID, WireEventID: event.EventID,
		WireEventHash: event.EventHash, BodyDigest: event.BodyDigest, ParentHash: event.ParentHash,
		CheckpointAlignmentHash: event.CheckpointAlignmentHash,
		EventType:               event.Type, TimestampUnixNano: event.Timestamp.UnixNano(), Sequence: event.Sequence,
		Origin: event.Origin, SourceAgent: event.SourceAgent, Lane: event.Lane, Clear: event.Clear,
		CanonicalEventID: event.EventID,
		CanonicalHash:    strings.Repeat("c", sha256.Size*2),
	}
}

func TestBindDurableInboundCursorKeepsMetadataAbsentDeliveryLegacy(t *testing.T) {
	delivery := durableInboundTestDelivery("cursor-1")
	delivery.ProtocolVersion, delivery.StreamID, delivery.StreamEpoch, delivery.Position, delivery.CursorDigest = 0, "", "", 0, ""
	delivery.PredecessorCursor = ""

	binding, err := bindDurableInboundCursor("device-1", durableInboundTestNegotiation(proto.RemoteSyncModeDeltaPreferred), delivery)
	require.NoError(t, err)
	require.Nil(t, binding, "metadata-free MQTT inbound-v2 must keep legacy behavior during overlap")
}

func TestBindDurableInboundCursorAcceptsCloudCursorBound(t *testing.T) {
	maximum := durableInboundTestDelivery(strings.Repeat("c", proto.MaxDurableCursorBytes))
	binding, err := bindDurableInboundCursor("device-1", durableInboundTestNegotiation(proto.RemoteSyncModeDurableRead), maximum)
	require.NoError(t, err)
	require.Equal(t, maximum.Cursor, binding.next.Cursor)

	over := durableInboundTestDelivery(strings.Repeat("c", proto.MaxDurableCursorBytes+1))
	binding, err = bindDurableInboundCursor("device-1", durableInboundTestNegotiation(proto.RemoteSyncModeDurableRead), over)
	require.ErrorIs(t, err, errDurableInboundMetadata)
	require.Nil(t, binding)
}

func TestBindDurableInboundCursorAcceptsOnlyExactNegotiatedNamespaceStream(t *testing.T) {
	negotiated := durableInboundTestMultiNegotiation(proto.RemoteSyncModeDurableRead)
	delivery := durableInboundTestDelivery("namespace-cursor-1")
	delivery.StreamID = negotiated.Streams[1].StreamID
	delivery.StreamEpoch = negotiated.Streams[1].StreamEpoch

	binding, err := bindDurableInboundCursor("device-1", negotiated, delivery)
	require.NoError(t, err)
	require.Equal(t, delivery.StreamID, binding.key.StreamID)
	require.Equal(t, delivery.StreamEpoch, binding.key.StreamEpoch)

	wrongNamespace := delivery
	wrongNamespace.Events = append([]proto.RemoteEvent(nil), delivery.Events...)
	wrongNamespace.Events[0].NamespaceID = "namespace-2"
	binding, err = bindDurableInboundCursor("device-1", negotiated, wrongNamespace)
	require.ErrorIs(t, err, errDurableInboundMetadata)
	require.Nil(t, binding)

	unknown := delivery
	unknown.StreamID = "unknown-stream"
	binding, err = bindDurableInboundCursor("device-1", negotiated, unknown)
	require.ErrorIs(t, err, errDurableInboundMetadata)
	require.Nil(t, binding)

	unsignedRuntime := negotiated
	unsignedRuntime.ServerCapabilities = []string{proto.CapabilityDurableDeltaSyncV1, proto.CapabilityInboundAckV2, proto.CapabilityInboundFinalizeV1}
	binding, err = bindDurableInboundCursor("device-1", unsignedRuntime, delivery)
	require.ErrorIs(t, err, errDurableInboundMetadata)
	require.Nil(t, binding)
}

func TestBindDurableInboundCursorFailsClosed(t *testing.T) {
	validDelivery := durableInboundTestDelivery("cursor-1")
	validNegotiation := durableInboundTestNegotiation(proto.RemoteSyncModeDurableRead)

	tests := []struct {
		name        string
		identity    string
		negotiation proto.RemoteNegotiateSyncV1Result
		delivery    proto.RemoteInboundDeliveryV2
	}{
		{name: "missing identity", negotiation: validNegotiation, delivery: validDelivery},
		{name: "legacy negotiation", identity: "device-1", negotiation: durableInboundTestNegotiation(proto.RemoteSyncModeLegacy), delivery: validDelivery},
		{name: "shadow negotiation", identity: "device-1", negotiation: durableInboundTestNegotiation(proto.RemoteSyncModeShadow), delivery: validDelivery},
		{name: "stale stream", identity: "device-1", negotiation: func() proto.RemoteNegotiateSyncV1Result { v := validNegotiation; v.StreamID = "stream-2"; return v }(), delivery: validDelivery},
		{name: "stale epoch", identity: "device-1", negotiation: func() proto.RemoteNegotiateSyncV1Result { v := validNegotiation; v.StreamEpoch = "epoch-2"; return v }(), delivery: validDelivery},
		{name: "inbound acknowledgement capability absent", identity: "device-1", negotiation: func() proto.RemoteNegotiateSyncV1Result {
			v := validNegotiation
			v.ServerCapabilities = []string{proto.CapabilityDurableDeltaSyncV1}
			return v
		}(), delivery: validDelivery},
		{name: "partial metadata", identity: "device-1", negotiation: validNegotiation, delivery: func() proto.RemoteInboundDeliveryV2 { v := validDelivery; v.CursorDigest = ""; return v }()},
		{name: "wrong protocol", identity: "device-1", negotiation: validNegotiation, delivery: func() proto.RemoteInboundDeliveryV2 { v := validDelivery; v.ProtocolVersion = 2; return v }()},
		{name: "missing position", identity: "device-1", negotiation: validNegotiation, delivery: func() proto.RemoteInboundDeliveryV2 { v := validDelivery; v.Position = 0; return v }()},
		{name: "genesis without signed predecessor cursor", identity: "device-1", negotiation: validNegotiation, delivery: func() proto.RemoteInboundDeliveryV2 {
			v := validDelivery
			v.PredecessorCursor = ""
			return v
		}()},
		{name: "successor missing predecessor", identity: "device-1", negotiation: validNegotiation, delivery: func() proto.RemoteInboundDeliveryV2 {
			v := validDelivery
			v.Position = 2
			return v
		}()},
		{name: "successor non-adjacent position", identity: "device-1", negotiation: validNegotiation, delivery: func() proto.RemoteInboundDeliveryV2 {
			v := durableInboundTestSuccessor("cursor-1", "cursor-3", 3)
			v.PredecessorPosition = 1
			return v
		}()},
		{name: "digest mismatch", identity: "device-1", negotiation: validNegotiation, delivery: func() proto.RemoteInboundDeliveryV2 {
			v := validDelivery
			v.CursorDigest = string(make([]byte, 64))
			return v
		}()},
		{name: "batched delivery", identity: "device-1", negotiation: validNegotiation, delivery: func() proto.RemoteInboundDeliveryV2 {
			v := validDelivery
			v.Events = append(v.Events, v.Events[0])
			return v
		}()},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			binding, err := bindDurableInboundCursor(test.identity, test.negotiation, test.delivery)
			require.ErrorIs(t, err, errDurableInboundMetadata)
			require.Nil(t, binding)
		})
	}

	binding, err := bindDurableInboundCursor("device-1", validNegotiation, validDelivery)
	require.NoError(t, err)
	require.Equal(t, daemon.DurableCursorKey{RemoteIdentity: "device-1", StreamID: "stream-1", StreamEpoch: "epoch-1"}, binding.key)
	require.Equal(t, validDelivery.PredecessorCursor, binding.predecessor.Cursor)
	require.Equal(t, uint64(0), binding.predecessor.Position)
	require.Equal(t, daemon.DurableCursorState{Cursor: validDelivery.Cursor, CursorDigest: validDelivery.CursorDigest, Position: 1}, binding.next)
}

func TestInboundV2AckFromResultsStopsDurableGapsAndSecurityFailures(t *testing.T) {
	delivery := durableInboundTestDelivery("cursor-1")

	legacyGap, terminal := inboundV2AckFromResults(delivery, []syncd.ImportOutcome{syncd.ImportDeferredNeedsBaseline}, false)
	require.True(t, terminal)
	require.Equal(t, "accepted", legacyGap.Outcomes[0].Disposition)
	require.Equal(t, "durable", legacyGap.Outcomes[0].ReasonCode)

	legacyRejected, terminal := inboundV2AckFromResults(delivery, []syncd.ImportOutcome{syncd.ImportRejected}, false)
	require.True(t, terminal)
	require.Equal(t, "quarantined", legacyRejected.Outcomes[0].Disposition)
	require.Equal(t, "authenticated-input-rejected", legacyRejected.Outcomes[0].ReasonCode)

	for name, result := range map[string]syncd.ImportOutcome{
		"missing parent":    syncd.ImportDeferredNeedsBaseline,
		"security failure":  syncd.ImportRejected,
		"transient failure": syncd.ImportRetryable,
	} {
		t.Run(name, func(t *testing.T) {
			ack, terminal := inboundV2AckFromResults(delivery, []syncd.ImportOutcome{result}, true)
			require.False(t, terminal)
			require.Equal(t, "retryable", ack.Outcomes[0].Disposition)
		})
	}
}

func TestInboundV2AckFromResultsUnknownOutcomeFailsClosed(t *testing.T) {
	delivery := durableInboundTestDelivery("cursor-1")
	ack, terminal := inboundV2AckFromResults(delivery, []syncd.ImportOutcome{syncd.ImportOutcome(255)}, true)
	require.False(t, terminal)
	require.Len(t, ack.Outcomes, 1)
	require.Equal(t, "retryable", ack.Outcomes[0].Disposition)
	require.Equal(t, "upgrade-required", ack.Outcomes[0].ReasonCode)
}

func TestCompletedInboxRedeliveryRepairsDurableCursorAfterRestart(t *testing.T) {
	root := t.TempDir()
	inboxRoot := filepath.Join(root, "inbox-v2")
	cursorRoot := filepath.Join(root, "durable-cursors")
	delivery := durableInboundTestDelivery("cursor-after-apply")
	inbox := &daemon.InboundInbox{Root: inboxRoot}

	admission, cached, err := inbox.AdmitDurable(delivery, securityepoch.SecurityEpoch{}, "device-1")
	require.NoError(t, err)
	require.Nil(t, cached)
	ack := proto.RemoteInboundAckV2{
		DeliveryID:       delivery.DeliveryID,
		NextCursor:       delivery.Cursor,
		NextCursorDigest: delivery.CursorDigest,
		NextPosition:     delivery.Position,
		Outcomes: []proto.RemoteInboundEventOutcomeV2{{
			Index:       0,
			Disposition: "accepted",
			ReasonCode:  "durable",
		}},
	}
	finalizeEvidence := durableInboundTestFinalizeEvidence(delivery)
	ack.FinalizeEvidence = &finalizeEvidence
	require.NoError(t, inbox.CompleteDurable(delivery.DeliveryID, admission.InputSHA256, ack))

	// Simulate a crash after canonical apply + terminal inbox commit, before
	// cursor persistence. A fresh inbox instance returns the exact terminal ACK.
	restartedInbox := &daemon.InboundInbox{Root: inboxRoot}
	_, cached, err = restartedInbox.AdmitDurable(delivery, securityepoch.SecurityEpoch{}, "device-1")
	require.NoError(t, err)
	require.Equal(t, ack, *cached)
	require.True(t, durableCachedAckSafe(delivery, *cached))

	binding, err := bindDurableInboundCursor("device-1", durableInboundTestNegotiation(proto.RemoteSyncModeDurableRead), delivery)
	require.NoError(t, err)
	store := &daemon.DurableCursorStore{Root: cursorRoot}
	_, err = store.Load(binding.key)
	require.ErrorIs(t, err, daemon.ErrDurableCursorNotFound)
	repair := &durableInboundRestartRepair{}
	require.NoError(t, repair.ensure(restartedInbox, store))

	persisted, err := (&daemon.DurableCursorStore{Root: cursorRoot}).Load(binding.key)
	require.NoError(t, err)
	require.Equal(t, delivery.Cursor, persisted.Cursor)
	require.Equal(t, delivery.CursorDigest, persisted.CursorDigest)
	require.NoError(t, repair.ensure(restartedInbox, &daemon.DurableCursorStore{Root: cursorRoot}), "second restart repair must be idempotent")
}

func TestCompletedInboxRedeliveryAdvancesAuthenticatedSuccessorAfterRestart(t *testing.T) {
	root := t.TempDir()
	inboxRoot := filepath.Join(root, "inbox-v2")
	cursorRoot := filepath.Join(root, "durable-cursors")
	negotiated := durableInboundTestNegotiation(proto.RemoteSyncModeDurableRead)
	store := &daemon.DurableCursorStore{Root: cursorRoot}

	first, err := bindDurableInboundCursor("device-1", negotiated, durableInboundTestDelivery("cursor-1"))
	require.NoError(t, err)
	require.NoError(t, advanceDurableInboundCursor(store, first, false))

	delivery := durableInboundTestSuccessor("cursor-1", "cursor-2", 2)
	binding, err := bindDurableInboundCursor("device-1", negotiated, delivery)
	require.NoError(t, err)
	inbox := &daemon.InboundInbox{Root: inboxRoot}
	admission, cached, err := inbox.AdmitDurable(delivery, securityepoch.SecurityEpoch{}, "device-1")
	require.NoError(t, err)
	require.Nil(t, cached)
	ack := proto.RemoteInboundAckV2{
		DeliveryID:       delivery.DeliveryID,
		NextCursor:       delivery.Cursor,
		NextCursorDigest: delivery.CursorDigest,
		NextPosition:     delivery.Position,
		Outcomes:         []proto.RemoteInboundEventOutcomeV2{{Index: 0, Disposition: "accepted", ReasonCode: "durable"}},
	}
	finalizeEvidence := durableInboundTestFinalizeEvidence(delivery)
	ack.FinalizeEvidence = &finalizeEvidence
	require.NoError(t, inbox.CompleteDurable(delivery.DeliveryID, admission.InputSHA256, ack))

	// Simulate restart after inbox completion while the cursor still names the
	// authenticated predecessor. The cached terminal ACK can repair the exact
	// +1 transition without re-importing canonical content.
	_, cached, err = (&daemon.InboundInbox{Root: inboxRoot}).AdmitDurable(delivery, securityepoch.SecurityEpoch{}, "device-1")
	require.NoError(t, err)
	require.NotNil(t, cached)
	require.True(t, durableCachedAckSafe(delivery, *cached))
	repair := &durableInboundRestartRepair{}
	require.NoError(t, repair.ensure(&daemon.InboundInbox{Root: inboxRoot}, &daemon.DurableCursorStore{Root: cursorRoot}))

	persisted, err := store.Load(binding.key)
	require.NoError(t, err)
	require.Equal(t, uint64(2), persisted.Position)
	require.Equal(t, delivery.Cursor, persisted.Cursor)
}

func TestCompletedInboxRepairNeverRegressesDifferentOpaqueCursor(t *testing.T) {
	root := filepath.Join(t.TempDir(), "durable-cursors")
	store := &daemon.DurableCursorStore{Root: root}
	currentDelivery := durableInboundTestDelivery("cursor-current")
	current, err := bindDurableInboundCursor("device-1", durableInboundTestNegotiation(proto.RemoteSyncModeDeltaPreferred), currentDelivery)
	require.NoError(t, err)
	require.NoError(t, advanceDurableInboundCursor(store, current, false))

	staleDelivery := durableInboundTestDelivery("cursor-stale")
	stale, err := bindDurableInboundCursor("device-1", durableInboundTestNegotiation(proto.RemoteSyncModeDeltaPreferred), staleDelivery)
	require.NoError(t, err)
	require.ErrorIs(t, advanceDurableInboundCursor(&daemon.DurableCursorStore{Root: root}, stale, true), daemon.ErrDurableCursorConflict)

	persisted, err := store.Load(current.key)
	require.NoError(t, err)
	require.Equal(t, current.next.Cursor, persisted.Cursor)
	require.Equal(t, current.next.CursorDigest, persisted.CursorDigest)
}

func TestDurableCursorAdvanceRequiresAuthenticatedAdjacencyForDifferentCursor(t *testing.T) {
	root := filepath.Join(t.TempDir(), "durable-cursors")
	store := &daemon.DurableCursorStore{Root: root}
	first, err := bindDurableInboundCursor("device-1", durableInboundTestNegotiation(proto.RemoteSyncModeDurableRead), durableInboundTestDelivery("cursor-1"))
	require.NoError(t, err)
	require.NoError(t, advanceDurableInboundCursor(store, first, false))

	secondDelivery := durableInboundTestSuccessor("cursor-1", "cursor-2", 2)
	second, err := bindDurableInboundCursor("device-1", durableInboundTestNegotiation(proto.RemoteSyncModeDurableRead), secondDelivery)
	require.NoError(t, err)
	require.Equal(t, first.next.Cursor, second.predecessor.Cursor)
	require.Equal(t, uint64(1), second.predecessor.Position)
	require.NoError(t, advanceDurableInboundCursor(store, second, false))

	persisted, err := store.Load(first.key)
	require.NoError(t, err)
	require.Equal(t, second.next.Cursor, persisted.Cursor)
	require.Equal(t, second.next.CursorDigest, persisted.CursorDigest)
	require.Equal(t, uint64(2), persisted.Position)

	skippedDelivery := durableInboundTestSuccessor("cursor-2", "cursor-4", 4)
	skipped, err := bindDurableInboundCursor("device-1", durableInboundTestNegotiation(proto.RemoteSyncModeDurableRead), skippedDelivery)
	require.NoError(t, err)
	require.ErrorIs(t, advanceDurableInboundCursor(store, skipped, false), daemon.ErrDurableCursorConflict)
}

func TestDurableCachedAckRejectsQuarantineAndCursorMismatch(t *testing.T) {
	delivery := durableInboundTestDelivery("cursor-1")
	ack := proto.RemoteInboundAckV2{
		DeliveryID:       delivery.DeliveryID,
		NextCursor:       delivery.Cursor,
		NextCursorDigest: delivery.CursorDigest,
		NextPosition:     delivery.Position,
		Outcomes:         []proto.RemoteInboundEventOutcomeV2{{Index: 0, Disposition: "accepted", ReasonCode: "durable"}},
	}
	finalizeEvidence := durableInboundTestFinalizeEvidence(delivery)
	ack.FinalizeEvidence = &finalizeEvidence
	require.True(t, durableCachedAckSafe(delivery, ack))
	ack.Outcomes[0].Disposition = "quarantined"
	require.False(t, durableCachedAckSafe(delivery, ack))
	ack.Outcomes[0].Disposition = "accepted"
	ack.NextCursor = "other"
	require.False(t, durableCachedAckSafe(delivery, ack))
	ack.NextCursor = delivery.Cursor
	ack.NextPosition++
	require.False(t, durableCachedAckSafe(delivery, ack))
}
