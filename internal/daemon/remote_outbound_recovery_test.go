package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/plugin/proto"
	syncd "github.com/aplexica/aplexica/internal/sync"
	"github.com/stretchr/testify/require"
)

type recoveryPolicyClient struct {
	*fakePublishClient
	required   bool
	negotiated proto.RemoteNegotiateSyncV1Result
}

func (c *recoveryPolicyClient) DurableReceiptRequired() bool { return c.required }
func (c *recoveryPolicyClient) SyncNegotiation() proto.RemoteNegotiateSyncV1Result {
	return c.negotiated
}

type fakeOutboundRecoverySource struct {
	calls int
	fn    func(syncd.RemoteRecoveryRequest, func(syncd.OutboundEvent) error) (syncd.RemoteRecoveryResult, error)
}

func (s *fakeOutboundRecoverySource) RebuildRemoteOutbound(_ context.Context, request syncd.RemoteRecoveryRequest, appendDurable func(syncd.OutboundEvent) error) (syncd.RemoteRecoveryResult, error) {
	s.calls++
	return s.fn(request, appendDurable)
}

func daemonRecoveryEvent(id string, generation uint64, body string) proto.RemoteEvent {
	accessHash := sha256.Sum256([]byte("access-set"))
	barrier := sha256.Sum256([]byte("security-barrier"))
	bytes := json.RawMessage(body)
	return proto.RemoteEvent{
		AccessGeneration: generation, AccessSetHash: accessHash,
		SecurityGeneration: generation, SecurityBarrierID: barrier,
		KeyMode: "recipient-wrap-v2", KeyVersion: 0,
		BranchID: "main", ArtifactID: "artifact-1", EventID: id,
		EventHash: watermarkTestDigest("hash-" + id), BodyDigest: sealedBodyDigest(bytes),
		Kind: "memory", Type: "update", Timestamp: time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC),
		Bytes: bytes, Sequence: 2, Origin: "device-a", SourceAgent: "codex", Lane: syncd.LaneLive,
	}
}

func outboundForRecoveryEvent(event proto.RemoteEvent, body string) syncd.OutboundEvent {
	return syncd.OutboundEvent{
		ProjectID: event.ProjectID, ProjectAuthorizationGeneration: event.ProjectAuthorizationGeneration,
		AccessGeneration: event.AccessGeneration, AccessSetHash: event.AccessSetHash,
		SecurityGeneration: event.SecurityGeneration, SecurityBarrierID: event.SecurityBarrierID,
		KeyMode: event.KeyMode, KeyVersion: event.KeyVersion,
		NamespaceID: event.NamespaceID, BranchID: event.BranchID, ArtifactID: event.ArtifactID,
		EventID: event.EventID, ParentHash: event.ParentHash, EventHash: event.EventHash,
		Kind: event.Kind, Type: event.Type, Timestamp: event.Timestamp,
		Bytes: []byte(body), Sequence: event.Sequence, Origin: event.Origin,
		SourceAgent: event.SourceAgent, Lane: event.Lane,
	}
}

func newRecoveryTestAdapter(t *testing.T, mode string) (*RemotePublishAdapter, *recoveryPolicyClient) {
	t.Helper()
	client := &recoveryPolicyClient{
		fakePublishClient: &fakePublishClient{}, required: true,
		negotiated: proto.RemoteNegotiateSyncV1Result{
			Mode: mode, StreamID: "stream-1", StreamEpoch: "epoch-1",
		},
	}
	return newTestPublishAdapterWithOutbox(t, client), client
}

func TestRemoteOutboundRecoveryRequeuesExactPersistedCiphertextThenClearsFromWatermark(t *testing.T) {
	adapter, _ := newRecoveryTestAdapter(t, proto.RemoteSyncModeDeltaRequired)
	original := daemonRecoveryEvent("event-1", 1, `{"sealed":"original"}`)
	resealed := outboundForRecoveryEvent(original, `{"sealed":"randomized-again"}`)
	require.NoError(t, adapter.outbox.Append(original))
	require.NoError(t, adapter.outbox.RequireCanonicalRecovery(original))

	source := &fakeOutboundRecoverySource{fn: func(request syncd.RemoteRecoveryRequest, appendDurable func(syncd.OutboundEvent) error) (syncd.RemoteRecoveryResult, error) {
		require.Empty(t, request.Anchors)
		require.NoError(t, appendDurable(resealed))
		return syncd.RemoteRecoveryResult{Rebuilt: 1}, nil
	}}
	adapter.SetRecoverySource(source)
	adapter.recoverDirtyOnce(context.Background())
	queued := <-adapter.queue
	require.JSONEq(t, string(original.Bytes), string(queued.Bytes), "randomized reseal must not replace exact retry bytes")
	dirty, err := adapter.outbox.mutations.IsDirty("")
	require.NoError(t, err)
	require.True(t, dirty)

	watermark := watermarkFixture(7)
	watermark.Key = DurablePublishWatermarkKey{StreamID: "stream-1", StreamEpoch: "epoch-1", ArtifactID: original.ArtifactID, BranchID: original.BranchID}
	watermark.CanonicalEventID = original.EventID
	watermark.CanonicalEventHash = original.EventHash
	watermark.RecipientFingerprint = hex.EncodeToString(original.AccessSetHash[:])
	watermark.AccessGeneration = original.AccessGeneration
	watermark.SecurityGeneration = original.SecurityGeneration
	watermark.SecurityBarrier = hex.EncodeToString(original.SecurityBarrierID[:])
	watermark.KeyMode = original.KeyMode
	watermark.KeyVersion = original.KeyVersion
	_, err = adapter.watermarks.Advance(watermark)
	require.NoError(t, err)
	source.fn = func(request syncd.RemoteRecoveryRequest, _ func(syncd.OutboundEvent) error) (syncd.RemoteRecoveryResult, error) {
		require.Len(t, request.Anchors, 1)
		return syncd.RemoteRecoveryResult{Complete: true}, nil
	}
	adapter.recoverDirtyOnce(context.Background())
	dirty, err = adapter.outbox.mutations.IsDirty("")
	require.NoError(t, err)
	require.False(t, dirty)
}

func TestRemoteOutboundRecoveryConcurrentMutationDefeatsCompletionCAS(t *testing.T) {
	adapter, _ := newRecoveryTestAdapter(t, proto.RemoteSyncModeDeltaPreferred)
	first := daemonRecoveryEvent("event-1", 1, `{"sealed":"first"}`)
	second := daemonRecoveryEvent("event-2", 1, `{"sealed":"second"}`)
	require.NoError(t, adapter.outbox.RequireCanonicalRecovery(first))
	source := &fakeOutboundRecoverySource{fn: func(_ syncd.RemoteRecoveryRequest, _ func(syncd.OutboundEvent) error) (syncd.RemoteRecoveryResult, error) {
		require.NoError(t, adapter.outbox.RequireCanonicalRecovery(second))
		return syncd.RemoteRecoveryResult{Complete: true}, nil
	}}
	adapter.SetRecoverySource(source)
	adapter.recoverDirtyOnce(context.Background())
	markers, err := adapter.outbox.mutations.ListDirty()
	require.NoError(t, err)
	require.Len(t, markers, 1)
	require.Equal(t, first.EventID, markers[0].Marker.TargetEventID, "concurrent work must retain the earliest missing target")
	require.GreaterOrEqual(t, markers[0].Marker.MutationGeneration, uint64(2))
}

func TestRemoteOutboundRecoveryRepairsOutboxOverflowAfterCapacityFrees(t *testing.T) {
	adapter, _ := newRecoveryTestAdapter(t, proto.RemoteSyncModeDeltaRequired)
	adapter.outbox.maxEntries = 1
	first := daemonRecoveryEvent("event-1", 1, `{"sealed":"first"}`)
	second := daemonRecoveryEvent("event-2", 1, `{"sealed":"second"}`)
	require.NoError(t, adapter.outbox.Append(first))
	_, _, err := adapter.outbox.AppendForPublish(second)
	require.Error(t, err)
	markers, err := adapter.outbox.mutations.ListDirty()
	require.NoError(t, err)
	require.Len(t, markers, 1)
	require.Equal(t, second.EventID, markers[0].Marker.TargetEventID)

	require.NoError(t, adapter.outbox.Remove(first.EventID))
	source := &fakeOutboundRecoverySource{fn: func(request syncd.RemoteRecoveryRequest, appendDurable func(syncd.OutboundEvent) error) (syncd.RemoteRecoveryResult, error) {
		require.Equal(t, second.EventID, request.Target.EventID)
		require.NoError(t, appendDurable(outboundForRecoveryEvent(second, string(second.Bytes))))
		return syncd.RemoteRecoveryResult{Rebuilt: 1}, nil
	}}
	adapter.SetRecoverySource(source)
	adapter.recoverDirtyOnce(context.Background())
	require.Equal(t, second.EventID, (<-adapter.queue).EventID)
	require.True(t, outboxHas(t, adapter, second.EventID))
	dirty, err := adapter.outbox.mutations.IsDirty("")
	require.NoError(t, err)
	require.True(t, dirty, "cloud watermark must prove the rebuilt event before marker cleanup")
}

func TestRemoteOutboundRecoveryAuthorityConflictPersistsMandatoryObligationAndOldBytes(t *testing.T) {
	adapter, _ := newRecoveryTestAdapter(t, proto.RemoteSyncModeDeltaRequired)
	old := daemonRecoveryEvent("event-1", 1, `{"sealed":"old-generation"}`)
	current := daemonRecoveryEvent("event-1", 2, `{"sealed":"current-generation"}`)
	require.NoError(t, adapter.outbox.Append(old))
	require.NoError(t, adapter.outbox.RequireCanonicalRecovery(current))
	source := &fakeOutboundRecoverySource{fn: func(_ syncd.RemoteRecoveryRequest, appendDurable func(syncd.OutboundEvent) error) (syncd.RemoteRecoveryResult, error) {
		err := appendDurable(outboundForRecoveryEvent(current, `{"sealed":"new-random-seal"}`))
		return syncd.RemoteRecoveryResult{}, err
	}}
	adapter.SetRecoverySource(source)
	adapter.recoverDirtyOnce(context.Background())

	entries, err := adapter.outbox.List()
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.JSONEq(t, string(old.Bytes), string(entries[0].Event.Bytes), "conflict must preserve old exact retry authority")
	obligations, err := adapter.checkpointObligations.List()
	require.NoError(t, err)
	require.Len(t, obligations, 1)
	require.Equal(t, "pending-recovery-authority-conflict", obligations[0].Reason)
	markers, err := adapter.outbox.mutations.ListDirty()
	require.NoError(t, err)
	require.Len(t, markers, 1)
	require.NotZero(t, markers[0].Marker.ReasonFlags&rescanReasonCheckpoint)
	adapter.recoverDirtyOnce(context.Background())
	require.Equal(t, 1, source.calls, "checkpoint-marked scope must not retry live delta reconstruction")
}

func TestRemoteOutboundRecoveryDeadTargetPersistsMandatoryObligation(t *testing.T) {
	adapter, _ := newRecoveryTestAdapter(t, proto.RemoteSyncModeDeltaRequired)
	event := daemonRecoveryEvent("event-dead", 1, `{"sealed":"dead"}`)
	require.NoError(t, adapter.outbox.Append(event))
	require.NoError(t, adapter.outbox.Deadletter(event.EventID))
	require.NoError(t, adapter.outbox.RequireCanonicalRecovery(event))
	source := &fakeOutboundRecoverySource{fn: func(_ syncd.RemoteRecoveryRequest, appendDurable func(syncd.OutboundEvent) error) (syncd.RemoteRecoveryResult, error) {
		err := appendDurable(outboundForRecoveryEvent(event, `{"sealed":"new"}`))
		return syncd.RemoteRecoveryResult{}, err
	}}
	adapter.SetRecoverySource(source)
	adapter.recoverDirtyOnce(context.Background())
	obligations, err := adapter.checkpointObligations.List()
	require.NoError(t, err)
	require.Len(t, obligations, 1)
	require.Equal(t, "terminal-outbox-state", obligations[0].Reason)
}

func TestRemoteOutboundRecoveryMissingNonTargetSealRequiresCheckpoint(t *testing.T) {
	adapter, _ := newRecoveryTestAdapter(t, proto.RemoteSyncModeDeltaRequired)
	target := daemonRecoveryEvent("event-target", 1, `{"sealed":"target"}`)
	earlier := daemonRecoveryEvent("event-earlier", 1, `{"sealed":"earlier"}`)
	require.NoError(t, adapter.outbox.RequireCanonicalRecovery(target))
	source := &fakeOutboundRecoverySource{fn: func(_ syncd.RemoteRecoveryRequest, appendDurable func(syncd.OutboundEvent) error) (syncd.RemoteRecoveryResult, error) {
		err := appendDurable(outboundForRecoveryEvent(earlier, string(earlier.Bytes)))
		return syncd.RemoteRecoveryResult{}, err
	}}
	adapter.SetRecoverySource(source)
	adapter.recoverDirtyOnce(context.Background())
	obligations, err := adapter.checkpointObligations.List()
	require.NoError(t, err)
	require.Len(t, obligations, 1)
	require.Equal(t, "exact-seal-authority-unavailable", obligations[0].Reason)
	require.False(t, outboxHas(t, adapter, earlier.EventID), "unbound randomized reseal must not become retry authority")
}

func TestRemoteOutboundRecoveryDisabledOutsideDeltaReceiptModes(t *testing.T) {
	for _, mode := range []string{proto.RemoteSyncModeLegacy, proto.RemoteSyncModeShadow, proto.RemoteSyncModeDurableRead} {
		t.Run(mode, func(t *testing.T) {
			adapter, _ := newRecoveryTestAdapter(t, mode)
			event := daemonRecoveryEvent("event-1", 1, `{"sealed":"event"}`)
			require.NoError(t, adapter.outbox.RequireCanonicalRecovery(event))
			source := &fakeOutboundRecoverySource{fn: func(syncd.RemoteRecoveryRequest, func(syncd.OutboundEvent) error) (syncd.RemoteRecoveryResult, error) {
				return syncd.RemoteRecoveryResult{}, errors.New("must not run")
			}}
			adapter.SetRecoverySource(source)
			adapter.recoverDirtyOnce(context.Background())
			require.Zero(t, source.calls)
			dirty, err := adapter.outbox.mutations.IsDirty("")
			require.NoError(t, err)
			require.True(t, dirty)
		})
	}
}
