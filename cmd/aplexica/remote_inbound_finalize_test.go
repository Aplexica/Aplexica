package main

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/aplexica/aplexica/internal/daemon"
	"github.com/aplexica/aplexica/internal/plugin/proto"
	"github.com/aplexica/aplexica/internal/securityepoch"
	syncd "github.com/aplexica/aplexica/internal/sync"
	"github.com/stretchr/testify/require"
)

func completedFinalizeFixture(t *testing.T) (*daemon.InboundInbox, *daemon.DurableCursorStore, proto.RemoteInboundFinalizeEvidenceV1) {
	t.Helper()
	root := t.TempDir()
	inbox := &daemon.InboundInbox{Root: filepath.Join(root, "inbox")}
	cursors := &daemon.DurableCursorStore{Root: filepath.Join(root, "cursors")}
	delivery := durableInboundTestDelivery("cursor-1")
	admission, cached, err := inbox.AdmitDurable(delivery, securityepoch.SecurityEpoch{}, "device-1")
	require.NoError(t, err)
	require.Nil(t, cached)
	evidence := durableInboundTestFinalizeEvidence(delivery)
	ack := proto.RemoteInboundAckV2{
		DeliveryID: delivery.DeliveryID, NextCursor: delivery.Cursor, NextCursorDigest: delivery.CursorDigest,
		NextPosition: delivery.Position, Outcomes: []proto.RemoteInboundEventOutcomeV2{{Index: 0, Disposition: "accepted", ReasonCode: "durable"}},
		FinalizeEvidence: &evidence,
	}
	require.NoError(t, inbox.CompleteDurable(delivery.DeliveryID, admission.InputSHA256, ack))
	_, err = cursors.CompareAndSwap(
		daemon.DurableCursorKey{RemoteIdentity: "device-1", StreamID: delivery.StreamID, StreamEpoch: delivery.StreamEpoch},
		nil,
		daemon.DurableCursorState{Cursor: delivery.Cursor, CursorDigest: delivery.CursorDigest, Position: delivery.Position},
	)
	require.NoError(t, err)
	return inbox, cursors, evidence
}

func completedNoopFinalizeFixture(t *testing.T) (*daemon.InboundInbox, *daemon.DurableCursorStore, proto.RemoteInboundFinalizeEvidenceV1) {
	t.Helper()
	root := t.TempDir()
	inbox := &daemon.InboundInbox{Root: filepath.Join(root, "inbox")}
	cursors := &daemon.DurableCursorStore{Root: filepath.Join(root, "cursors")}
	delivery := durableInboundTestDelivery("cursor-noop-1")
	admission, cached, err := inbox.AdmitDurable(delivery, securityepoch.SecurityEpoch{}, "device-1")
	require.NoError(t, err)
	require.Nil(t, cached)
	evidence := durableInboundTestFinalizeEvidence(delivery)
	evidence.FinalizeKind = proto.InboundFinalizeAuthenticatedNoop
	evidence.CanonicalEventID = ""
	evidence.CanonicalHash = ""
	evidence.NoopReason = proto.InboundFinalizeNoopNotRecipient
	evidence.AuthenticatedHeaderDigest = strings.Repeat("d", sha256.Size*2)
	evidence.AuthenticatedSignerIdentity = "device-2:" + strings.Repeat("e", sha256.Size*2)
	ack := proto.RemoteInboundAckV2{
		DeliveryID: delivery.DeliveryID, NextCursor: delivery.Cursor, NextCursorDigest: delivery.CursorDigest,
		NextPosition: delivery.Position, Outcomes: []proto.RemoteInboundEventOutcomeV2{{Index: 0, Disposition: "accepted", ReasonCode: "authenticated-noop"}},
		FinalizeEvidence: &evidence,
	}
	require.NoError(t, inbox.CompleteDurable(delivery.DeliveryID, admission.InputSHA256, ack))
	_, err = cursors.CompareAndSwap(
		daemon.DurableCursorKey{RemoteIdentity: "device-1", StreamID: delivery.StreamID, StreamEpoch: delivery.StreamEpoch}, nil,
		daemon.DurableCursorState{Cursor: delivery.Cursor, CursorDigest: delivery.CursorDigest, Position: delivery.Position},
	)
	require.NoError(t, err)
	return inbox, cursors, evidence
}

func TestDurableInboundAuthenticatedNoopFinalizeNeverFansOutAndIsRestartIdempotent(t *testing.T) {
	inbox, cursors, evidence := completedNoopFinalizeFixture(t)
	var gate sync.Mutex
	materialized := 0
	materialize := func(syncd.InboundCanonicalEvidence) error { materialized++; return nil }
	negotiated := durableInboundTestNegotiation(proto.RemoteSyncModeDurableRead)

	result := handleDurableInboundFinalize(&gate, inbox, cursors, "device-1", negotiated,
		proto.RemoteInboundFinalizeV1Params{Evidence: evidence}, materialize)
	require.True(t, result.Accepted, "unexpected finalize result: %+v", result)
	require.True(t, result.NoopFinalized)
	require.False(t, result.Materialized)
	require.False(t, result.AlreadyFinalized)
	require.Empty(t, result.ReasonCode)
	require.Zero(t, materialized, "authenticated no-op finalization must never invoke native fan-out")

	result = handleDurableInboundFinalize(&gate, &daemon.InboundInbox{Root: inbox.Root}, &daemon.DurableCursorStore{Root: cursors.Root},
		"device-1", negotiated, proto.RemoteInboundFinalizeV1Params{Evidence: evidence}, materialize)
	require.True(t, result.Accepted)
	require.True(t, result.AlreadyFinalized)
	require.False(t, result.Materialized)
	require.False(t, result.NoopFinalized)
	require.Zero(t, materialized)

	tampered := evidence
	tampered.NoopReason = "retained_clear"
	result = handleDurableInboundFinalize(&gate, inbox, cursors, "device-1", negotiated,
		proto.RemoteInboundFinalizeV1Params{Evidence: tampered}, materialize)
	require.False(t, result.Accepted)
	require.False(t, result.Materialized)
	require.False(t, result.NoopFinalized)
	require.False(t, result.AlreadyFinalized)
}

func TestDurableInboundFinalizeOccursOnlyOnExactPostAckRequestAndIsRestartIdempotent(t *testing.T) {
	inbox, cursors, evidence := completedFinalizeFixture(t)
	var gate sync.Mutex
	materialized := 0
	materialize := func(got syncd.InboundCanonicalEvidence) error {
		materialized++
		require.Equal(t, evidence.ArtifactID, got.ArtifactID)
		require.Equal(t, evidence.CanonicalEventID, got.EventID)
		require.Equal(t, evidence.CanonicalHash, got.EventHash)
		return nil
	}
	require.Zero(t, materialized, "terminal canonical ACK alone must not materialize before plugin cloud ACK")

	result := handleDurableInboundFinalize(&gate, inbox, cursors, "device-1", durableInboundTestNegotiation(proto.RemoteSyncModeDurableRead), proto.RemoteInboundFinalizeV1Params{Evidence: evidence}, materialize)
	require.True(t, result.Accepted)
	require.True(t, result.Materialized)
	require.False(t, result.AlreadyFinalized)
	require.Equal(t, 1, materialized)

	// Fresh store handles model a daemon restart after the native-finalized
	// marker committed but before the JSON-RPC result reached the plugin.
	restartedInbox := &daemon.InboundInbox{Root: inbox.Root}
	restartedCursors := &daemon.DurableCursorStore{Root: cursors.Root}
	result = handleDurableInboundFinalize(&gate, restartedInbox, restartedCursors, "device-1", durableInboundTestNegotiation(proto.RemoteSyncModeDurableRead), proto.RemoteInboundFinalizeV1Params{Evidence: evidence}, materialize)
	require.True(t, result.Accepted)
	require.True(t, result.AlreadyFinalized)
	require.False(t, result.Materialized)
	require.Equal(t, 1, materialized, "exact retry after restart must not fan out twice")

	persisted, err := restartedCursors.Load(daemon.DurableCursorKey{RemoteIdentity: "device-1", StreamID: evidence.StreamID, StreamEpoch: evidence.StreamEpoch})
	require.NoError(t, err)
	require.Equal(t, evidence.Position, persisted.Position, "finalize must never advance the cursor")
}

func TestDurableInboundFinalizeDrainsExactPreexistingObligationAfterShadowRollback(t *testing.T) {
	inbox, cursors, evidence := completedFinalizeFixture(t)
	var gate sync.Mutex
	materialized := 0
	materialize := func(syncd.InboundCanonicalEvidence) error { materialized++; return nil }
	shadow := durableInboundTestNegotiation(proto.RemoteSyncModeShadow)

	result := handleDurableInboundFinalize(&gate, inbox, cursors, "device-1", shadow,
		proto.RemoteInboundFinalizeV1Params{Evidence: evidence}, materialize)
	require.True(t, result.Accepted)
	require.True(t, result.Materialized)
	require.Equal(t, 1, materialized)

	tampered := evidence
	tampered.CanonicalHash = strings.Repeat("0", 64)
	result = handleDurableInboundFinalize(&gate, inbox, cursors, "device-1", shadow,
		proto.RemoteInboundFinalizeV1Params{Evidence: tampered}, materialize)
	require.False(t, result.Accepted, "shadow rollback may drain only exact daemon-owned evidence")
	require.Equal(t, 1, materialized)

	delivery := durableInboundTestSuccessor(evidence.Cursor, "cursor-2", 2)
	_, err := bindDurableInboundCursor("device-1", shadow, delivery)
	require.Error(t, err, "shadow rollback must never admit a new durable delivery")
}

func TestDurableInboundFinalizeAcceptsExactNamespaceStreamAndRejectsUnknownGeneration(t *testing.T) {
	inbox, cursors, evidence := completedFinalizeFixture(t)
	negotiated := durableInboundTestMultiNegotiation(proto.RemoteSyncModeDurableRead)
	negotiated.Streams[1].StreamID = evidence.StreamID
	negotiated.Streams[1].StreamEpoch = evidence.StreamEpoch
	negotiated.StreamID = "account-stream"
	negotiated.StreamEpoch = "account-epoch"
	negotiated.Streams[0].StreamID = negotiated.StreamID
	negotiated.Streams[0].StreamEpoch = negotiated.StreamEpoch

	var gate sync.Mutex
	materialized := 0
	result := handleDurableInboundFinalize(&gate, inbox, cursors, "device-1", negotiated,
		proto.RemoteInboundFinalizeV1Params{Evidence: evidence}, func(syncd.InboundCanonicalEvidence) error {
			materialized++
			return nil
		})
	require.True(t, result.Accepted)
	require.Equal(t, 1, materialized)

	wrongNamespaceEvidence := evidence
	wrongNamespaceEvidence.NamespaceID = "namespace-2"
	result = handleDurableInboundFinalize(&gate, inbox, cursors, "device-1", negotiated,
		proto.RemoteInboundFinalizeV1Params{Evidence: wrongNamespaceEvidence}, func(syncd.InboundCanonicalEvidence) error {
			materialized++
			return nil
		})
	require.False(t, result.Accepted)
	require.Equal(t, 1, materialized)

	unknown := negotiated
	unknown.Streams = append([]proto.RemoteStreamDescriptorV1(nil), negotiated.Streams...)
	unknown.Streams[1].StreamEpoch = "replaced-epoch"
	result = handleDurableInboundFinalize(&gate, inbox, cursors, "device-1", unknown,
		proto.RemoteInboundFinalizeV1Params{Evidence: evidence}, func(syncd.InboundCanonicalEvidence) error {
			materialized++
			return nil
		})
	require.False(t, result.Accepted)
	require.Equal(t, 1, materialized)
}

func TestDurableInboundFinalizeRejectsUnknownMismatchedNonterminalAndStaleEvidence(t *testing.T) {
	inbox, cursors, evidence := completedFinalizeFixture(t)
	var gate sync.Mutex
	materialized := 0
	materialize := func(syncd.InboundCanonicalEvidence) error { materialized++; return nil }
	negotiated := durableInboundTestNegotiation(proto.RemoteSyncModeDurableRead)

	for name, mutate := range map[string]func(*proto.RemoteInboundFinalizeEvidenceV1){
		"unknown delivery": func(value *proto.RemoteInboundFinalizeEvidenceV1) { value.DeliveryID = "unknown-delivery" },
		"mismatched event": func(value *proto.RemoteInboundFinalizeEvidenceV1) { value.CanonicalEventID = "substituted-event" },
		"wrong identity":   func(value *proto.RemoteInboundFinalizeEvidenceV1) { value.RemoteIdentity = "device-2" },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := evidence
			mutate(&candidate)
			result := handleDurableInboundFinalize(&gate, inbox, cursors, "device-1", negotiated, proto.RemoteInboundFinalizeV1Params{Evidence: candidate}, materialize)
			require.False(t, result.Accepted)
		})
	}
	require.Zero(t, materialized)

	// A terminal-looking request cannot use an inbox record whose canonical
	// import has not completed.
	pendingDelivery := durableInboundTestSuccessor(evidence.Cursor, "cursor-2", 2)
	pendingDelivery.DeliveryID = "pending-delivery"
	_, _, err := inbox.AdmitDurable(pendingDelivery, securityepoch.SecurityEpoch{}, "device-1")
	require.NoError(t, err)
	pendingEvidence := durableInboundTestFinalizeEvidence(pendingDelivery)
	result := handleDurableInboundFinalize(&gate, inbox, cursors, "device-1", negotiated, proto.RemoteInboundFinalizeV1Params{Evidence: pendingEvidence}, materialize)
	require.False(t, result.Accepted)

	// Once a later authenticated cursor exists, the older request is stale and
	// cannot materialize even though its terminal inbox record remains.
	key := daemon.DurableCursorKey{RemoteIdentity: "device-1", StreamID: evidence.StreamID, StreamEpoch: evidence.StreamEpoch}
	current, err := cursors.Load(key)
	require.NoError(t, err)
	_, err = cursors.CompareAndSwap(key, &current, daemon.DurableCursorState{Cursor: pendingDelivery.Cursor, CursorDigest: pendingDelivery.CursorDigest, Position: 2})
	require.NoError(t, err)
	result = handleDurableInboundFinalize(&gate, inbox, cursors, "device-1", negotiated, proto.RemoteInboundFinalizeV1Params{Evidence: evidence}, materialize)
	require.False(t, result.Accepted)
	require.Equal(t, "cursor-stale", result.ReasonCode)
	require.Zero(t, materialized)
}

func TestDurableInboundFinalizeEvidenceKeepsCheckpointAlignmentSeparateFromParent(t *testing.T) {
	delivery := durableInboundTestDelivery("cursor-checkpoint")
	event := &delivery.Events[0]
	event.Lane = syncd.LaneRetained
	event.ParentHash = ""
	event.CheckpointCoverage = 1
	event.CheckpointGeneration = strings.Repeat("b", sha256.Size*2)
	event.CheckpointAlignmentHash = strings.Repeat("a", sha256.Size*2)

	evidence := durableInboundTestFinalizeEvidence(delivery)
	require.True(t, validDurableInboundFinalizeEvidence(evidence))
	require.Empty(t, evidence.ParentHash)
	require.Equal(t, event.CheckpointAlignmentHash, evidence.CheckpointAlignmentHash)

	missing := evidence
	missing.CheckpointAlignmentHash = ""
	require.False(t, validDurableInboundFinalizeEvidence(missing))
	liveSmuggle := evidence
	liveSmuggle.Lane = syncd.LaneLive
	require.False(t, validDurableInboundFinalizeEvidence(liveSmuggle))

	live := durableInboundTestFinalizeEvidence(durableInboundTestDelivery("cursor-live"))
	require.True(t, validDurableInboundFinalizeEvidence(live))
	live.CheckpointAlignmentHash = strings.Repeat("c", sha256.Size*2)
	require.False(t, validDurableInboundFinalizeEvidence(live))
}

func TestDurableInboundFinalizeCrashBeforeMarkerRetriesIdempotentMaterializer(t *testing.T) {
	inbox, cursors, evidence := completedFinalizeFixture(t)
	var gate sync.Mutex
	materialized := 0
	originalRoot := inbox.Root
	first := true
	materialize := func(syncd.InboundCanonicalEvidence) error {
		materialized++
		if first {
			first = false
			// Simulate loss of the finalize-marker fsync after native fan-out.
			inbox.Root = "relative-root-is-rejected"
		}
		return nil
	}
	result := handleDurableInboundFinalize(&gate, inbox, cursors, "device-1", durableInboundTestNegotiation(proto.RemoteSyncModeDurableRead), proto.RemoteInboundFinalizeV1Params{Evidence: evidence}, materialize)
	require.False(t, result.Accepted)
	require.Equal(t, "finalize-commit-failed", result.ReasonCode)
	require.Equal(t, 1, materialized)

	inbox.Root = originalRoot
	result = handleDurableInboundFinalize(&gate, inbox, cursors, "device-1", durableInboundTestNegotiation(proto.RemoteSyncModeDurableRead), proto.RemoteInboundFinalizeV1Params{Evidence: evidence}, materialize)
	require.True(t, result.Accepted)
	require.True(t, result.Materialized)
	require.Equal(t, 2, materialized, "crash-window retry may repeat only the idempotent native fan-out")
}

func TestDurableInboundFinalizeNativeFailureKeepsObligationPending(t *testing.T) {
	inbox, cursors, evidence := completedFinalizeFixture(t)
	var gate sync.Mutex
	result := handleDurableInboundFinalize(
		&gate, inbox, cursors, "device-1", durableInboundTestNegotiation(proto.RemoteSyncModeDurableRead),
		proto.RemoteInboundFinalizeV1Params{Evidence: evidence},
		func(syncd.InboundCanonicalEvidence) error { return syncd.ErrInboundNativeMaterialization },
	)
	require.False(t, result.Accepted)
	require.Equal(t, "native-materialization-retryable", result.ReasonCode)
	already, err := inbox.PrepareInboundFinalize(evidence)
	require.NoError(t, err)
	require.False(t, already, "native failure must not persist a terminal finalize marker")
}

func TestDurableInboundFinalizeBarrierBlocksSuccessorUntilPredecessorFinalized(t *testing.T) {
	inbox, cursors, evidence := completedFinalizeFixture(t)
	negotiated := durableInboundTestNegotiation(proto.RemoteSyncModeDurableRead)
	currentDelivery := durableInboundTestDelivery(evidence.Cursor)
	currentBinding, err := bindDurableInboundCursor("device-1", negotiated, currentDelivery)
	require.NoError(t, err)
	require.NoError(t, durableInboundFinalizeBarrier(inbox, cursors, currentBinding), "exact current redelivery remains repairable")

	successor := durableInboundTestSuccessor(evidence.Cursor, "cursor-2", 2)
	successorBinding, err := bindDurableInboundCursor("device-1", negotiated, successor)
	require.NoError(t, err)
	require.Error(t, durableInboundFinalizeBarrier(inbox, cursors, successorBinding), "successor must stop before predecessor native finalize")

	_, err = inbox.MarkInboundFinalized(evidence)
	require.NoError(t, err)
	require.NoError(t, durableInboundFinalizeBarrier(inbox, cursors, successorBinding), "durable finalize marker releases exactly the authenticated successor")
}

func TestDurableInboundFinalizeBarrierBlocksAnotherStreamUntilGlobalObligationFinalized(t *testing.T) {
	inbox, cursors, evidence := completedFinalizeFixture(t)
	negotiated := durableInboundTestMultiNegotiation(proto.RemoteSyncModeDurableRead)
	namespace := negotiated.Streams[1]
	delivery := durableInboundTestDelivery("namespace-cursor-1")
	delivery.StreamID = namespace.StreamID
	delivery.StreamEpoch = namespace.StreamEpoch
	delivery.Events[0].NamespaceID = namespace.NamespaceID
	binding, err := bindDurableInboundCursor("device-1", negotiated, delivery)
	require.NoError(t, err)
	require.Error(t, durableInboundFinalizeBarrier(inbox, cursors, binding),
		"a second stream must not create another terminal obligation before the first is native-finalized")

	_, err = inbox.MarkInboundFinalized(evidence)
	require.NoError(t, err)
	require.NoError(t, durableInboundFinalizeBarrier(inbox, cursors, binding),
		"the exact durable finalize marker releases the globally serialized next stream")
}

func TestDurableInboundFinalizeBarrierAllowsOnlyExactCheckpointSeedSuccessor(t *testing.T) {
	root := t.TempDir()
	inbox := &daemon.InboundInbox{Root: filepath.Join(root, "inbox")}
	cursors := &daemon.DurableCursorStore{Root: filepath.Join(root, "cursors")}
	key := daemon.DurableCursorKey{RemoteIdentity: "device-1", StreamID: "stream-1", StreamEpoch: "epoch-1"}
	seedCursor := "signed-checkpoint-coverage-42"
	seedDigest := sha256.Sum256([]byte(seedCursor))
	_, err := cursors.SeedFromCheckpoint(key, daemon.DurableCheckpointSeed{
		Cursor: seedCursor, CursorDigest: hex.EncodeToString(seedDigest[:]), Position: 42,
		CheckpointEventID: "checkpoint-event-50", CheckpointEventHash: strings.Repeat("a", 64),
		CheckpointAlignmentHash: strings.Repeat("c", 64),
		CheckpointGeneration:    strings.Repeat("b", 64), CheckpointPosition: 50, CheckpointCoverage: 42,
	})
	require.NoError(t, err)

	successor := durableInboundTestSuccessor(seedCursor, "cursor-43", 43)
	binding, err := bindDurableInboundCursor("device-1", durableInboundTestNegotiation(proto.RemoteSyncModeDurableRead), successor)
	require.NoError(t, err)
	require.NoError(t, durableInboundFinalizeBarrier(inbox, cursors, binding), "an exact authenticated checkpoint seed has no ordinary finalize obligation")

	ordinaryRoot := t.TempDir()
	ordinary := &daemon.DurableCursorStore{Root: filepath.Join(ordinaryRoot, "cursors")}
	ordinaryCursor := "ordinary-cursor-1"
	ordinaryDigest := sha256.Sum256([]byte(ordinaryCursor))
	_, err = ordinary.CompareAndSwap(key, nil, daemon.DurableCursorState{
		Cursor: ordinaryCursor, CursorDigest: hex.EncodeToString(ordinaryDigest[:]), Position: 1,
	})
	require.NoError(t, err)
	ordinarySuccessor := durableInboundTestSuccessor(ordinaryCursor, "ordinary-cursor-2", 2)
	ordinaryBinding, err := bindDurableInboundCursor("device-1", durableInboundTestNegotiation(proto.RemoteSyncModeDurableRead), ordinarySuccessor)
	require.NoError(t, err)
	require.Error(t, durableInboundFinalizeBarrier(&daemon.InboundInbox{Root: filepath.Join(ordinaryRoot, "inbox")}, ordinary, ordinaryBinding),
		"an ordinary cursor with missing inbox evidence must remain blocked")
}
