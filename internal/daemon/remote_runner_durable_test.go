package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/plugin/proto"
	"github.com/stretchr/testify/require"
)

type remoteSyncNegotiatorStub struct {
	mu              sync.Mutex
	calls           int
	resumeCalls     int
	negotiateParams []proto.RemoteNegotiateSyncV1Params
	negotiate       func(context.Context, int) (proto.RemoteNegotiateSyncV1Result, error)
	resume          func(context.Context, proto.RemoteResumeCursorV1Params) (proto.RemoteResumeCursorV1Result, error)
}

type multiRemoteSyncNegotiatorStub struct {
	*remoteSyncNegotiatorStub
	mu           sync.Mutex
	resumeCalls  int
	resumeParams []proto.RemoteResumeCursorsV1Params
	resume       func(context.Context, proto.RemoteResumeCursorsV1Params) (proto.RemoteResumeCursorsV1Result, error)
}

func (stub *multiRemoteSyncNegotiatorStub) ResumeCursorsV1(ctx context.Context, params proto.RemoteResumeCursorsV1Params) (proto.RemoteResumeCursorsV1Result, error) {
	stub.mu.Lock()
	stub.resumeCalls++
	captured := proto.RemoteResumeCursorsV1Params{Cursors: append([]proto.RemoteResumeCursorV1Params(nil), params.Cursors...)}
	stub.resumeParams = append(stub.resumeParams, captured)
	stub.mu.Unlock()
	if stub.resume != nil {
		return stub.resume(ctx, params)
	}
	result := proto.RemoteResumeCursorsV1Result{Accepted: true, Cursors: make([]proto.RemoteResumeCursorV1Result, len(params.Cursors))}
	for index, cursor := range params.Cursors {
		result.Cursors[index] = proto.RemoteResumeCursorV1Result{
			Accepted: true, StreamID: cursor.StreamID, StreamEpoch: cursor.StreamEpoch,
			CursorPresent: cursor.CursorPresent, Cursor: cursor.Cursor, CursorDigest: cursor.CursorDigest, Position: cursor.Position,
			PendingFinalizeEvidence: cursor.PendingFinalizeEvidence,
		}
	}
	return result, nil
}

func (stub *multiRemoteSyncNegotiatorStub) multiResumeCallCount() int {
	stub.mu.Lock()
	defer stub.mu.Unlock()
	return stub.resumeCalls
}

func (stub *multiRemoteSyncNegotiatorStub) lastMultiResumeParams() proto.RemoteResumeCursorsV1Params {
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if len(stub.resumeParams) == 0 {
		return proto.RemoteResumeCursorsV1Params{}
	}
	params := stub.resumeParams[len(stub.resumeParams)-1]
	params.Cursors = append([]proto.RemoteResumeCursorV1Params(nil), params.Cursors...)
	return params
}

func (stub *remoteSyncNegotiatorStub) NegotiateSyncV1(ctx context.Context, params proto.RemoteNegotiateSyncV1Params) (proto.RemoteNegotiateSyncV1Result, error) {
	stub.mu.Lock()
	stub.calls++
	call := stub.calls
	captured := params
	if params.PendingFinalizeEvidence != nil {
		evidence := *params.PendingFinalizeEvidence
		captured.PendingFinalizeEvidence = &evidence
	}
	stub.negotiateParams = append(stub.negotiateParams, captured)
	stub.mu.Unlock()
	if stub.negotiate == nil {
		return proto.RemoteNegotiateSyncV1Result{}, errors.New("unexpected negotiation")
	}
	return stub.negotiate(ctx, call)
}

func (stub *remoteSyncNegotiatorStub) ResumeCursorV1(ctx context.Context, params proto.RemoteResumeCursorV1Params) (proto.RemoteResumeCursorV1Result, error) {
	stub.mu.Lock()
	stub.resumeCalls++
	stub.mu.Unlock()
	if stub.resume != nil {
		return stub.resume(ctx, params)
	}
	return proto.RemoteResumeCursorV1Result{
		Accepted: true, StreamID: params.StreamID, StreamEpoch: params.StreamEpoch,
		CursorPresent: params.CursorPresent, Cursor: params.Cursor, CursorDigest: params.CursorDigest, Position: params.Position,
		PendingFinalizeEvidence: params.PendingFinalizeEvidence,
	}, nil
}

func (stub *remoteSyncNegotiatorStub) resumeCallCount() int {
	stub.mu.Lock()
	defer stub.mu.Unlock()
	return stub.resumeCalls
}

func (stub *remoteSyncNegotiatorStub) callCount() int {
	stub.mu.Lock()
	defer stub.mu.Unlock()
	return stub.calls
}

func (stub *remoteSyncNegotiatorStub) lastNegotiationParams() proto.RemoteNegotiateSyncV1Params {
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if len(stub.negotiateParams) == 0 {
		return proto.RemoteNegotiateSyncV1Params{}
	}
	return stub.negotiateParams[len(stub.negotiateParams)-1]
}

func durableRefreshManifestForTest() proto.RemotePluginManifestUnsignedV1 {
	return proto.RemotePluginManifestUnsignedV1{Capabilities: []string{proto.CapabilityDurableDeltaSyncV1}}
}

func validDurableNegotiation(mode string) proto.RemoteNegotiateSyncV1Result {
	return proto.RemoteNegotiateSyncV1Result{
		SelectedProtocol:        1,
		Mode:                    mode,
		ServerCapabilities:      []string{proto.CapabilityDurableCursorResumeV1, proto.CapabilityDurableDeltaSyncV1, proto.CapabilityInboundAckV2, proto.CapabilityInboundFinalizeV1, proto.CapabilityRedactionSafeBatchV1, proto.CapabilityStagedCheckpointV1},
		AllActiveDevicesCapable: true,
		CheckpointReady:         true,
		FeatureGateEnabled:      true,
		StreamID:                "opaque-stream",
		StreamEpoch:             "epoch-1",
		MaxEventBytes:           4 << 20,
		MaxPageEvents:           128,
		MaxPageBytes:            8 << 20,
	}
}

func validMultiStreamNegotiation(mode string) proto.RemoteNegotiateSyncV1Result {
	result := validDurableNegotiation(mode)
	result.ServerCapabilities = append(result.ServerCapabilities, proto.CapabilityDurableMultiStreamV1)
	result.MinAvailableCursor = "account-min"
	result.Streams = []proto.RemoteStreamDescriptorV1{
		{
			NamespaceID: "", StreamID: result.StreamID, StreamEpoch: result.StreamEpoch,
			MaxEventBytes: result.MaxEventBytes, MaxPageEvents: result.MaxPageEvents, MaxPageBytes: result.MaxPageBytes,
			MinAvailableCursor: "account-min", TipCursor: "account-tip", CheckpointReady: true,
		},
		{
			NamespaceID: "namespace-a", StreamID: "namespace-stream-a", StreamEpoch: "namespace-epoch-a",
			MaxEventBytes: result.MaxEventBytes, MaxPageEvents: result.MaxPageEvents, MaxPageBytes: result.MaxPageBytes,
			MinAvailableCursor: "namespace-min-a", TipCursor: "namespace-tip-a", CheckpointReady: true,
		},
		{
			NamespaceID: "namespace-b", StreamID: "namespace-stream-b", StreamEpoch: "namespace-epoch-b",
			MaxEventBytes: result.MaxEventBytes, MaxPageEvents: result.MaxPageEvents, MaxPageBytes: result.MaxPageBytes,
			MinAvailableCursor: "namespace-min-b", TipCursor: "namespace-tip-b", CheckpointReady: true,
		},
	}
	return result
}

func TestRemoteRunnerBuildsAuthoritativeCursorHandoffAcrossPluginReplacement(t *testing.T) {
	root := filepath.Join(t.TempDir(), "cursors")
	store := &DurableCursorStore{Root: root}
	inbox := &InboundInbox{Root: filepath.Join(t.TempDir(), "inbox")}
	negotiated := validDurableNegotiation(proto.RemoteSyncModeShadow)
	key := DurableCursorKey{RemoteIdentity: "device-1", StreamID: negotiated.StreamID, StreamEpoch: negotiated.StreamEpoch}

	fresh := &RemoteRunner{DeviceID: "device-1", DurableCursorStore: store}
	params, err := fresh.authoritativeResumeCursorParams(negotiated)
	require.NoError(t, err)
	require.True(t, params.Authoritative)
	require.False(t, params.CursorPresent)
	require.True(t, validAuthoritativeResumeCursorResult(params, proto.RemoteResumeCursorV1Result{Accepted: true, StreamID: params.StreamID, StreamEpoch: params.StreamEpoch}))

	delivery := durableInboxTestDelivery(1)
	delivery.StreamID = negotiated.StreamID
	delivery.StreamEpoch = negotiated.StreamEpoch
	evidence := completeDurableInboxTestDelivery(t, inbox, delivery)
	state, err := store.CompareAndSwap(key, nil, DurableCursorState{Cursor: delivery.Cursor, CursorDigest: delivery.CursorDigest, Position: delivery.Position})
	require.NoError(t, err)

	replacement := &RemoteRunner{DeviceID: "device-1", DurableCursorStore: &DurableCursorStore{Root: root}, DurableInbox: &InboundInbox{Root: inbox.Root}}
	params, err = replacement.authoritativeResumeCursorParams(negotiated)
	require.NoError(t, err)
	require.True(t, params.CursorPresent)
	require.Equal(t, state.Cursor, params.Cursor)
	require.Equal(t, state.CursorDigest, params.CursorDigest)
	require.Equal(t, state.Position, params.Position)
	require.Equal(t, &evidence, params.PendingFinalizeEvidence, "plugin replacement must inherit the exact unfinalized obligation")
	require.True(t, validAuthoritativeResumeCursorResult(params, proto.RemoteResumeCursorV1Result{
		Accepted: true, StreamID: params.StreamID, StreamEpoch: params.StreamEpoch,
		CursorPresent: true, Cursor: params.Cursor, CursorDigest: params.CursorDigest, Position: params.Position,
		PendingFinalizeEvidence: params.PendingFinalizeEvidence,
	}))

	mismatch := proto.RemoteResumeCursorV1Result{Accepted: true, StreamID: params.StreamID, StreamEpoch: params.StreamEpoch, CursorPresent: true, Cursor: params.Cursor, CursorDigest: params.CursorDigest, Position: params.Position + 1}
	require.False(t, validAuthoritativeResumeCursorResult(params, mismatch))
	mismatchedEvidence := evidence
	mismatchedEvidence.CanonicalEventID = "substituted"
	mismatch = proto.RemoteResumeCursorV1Result{Accepted: true, StreamID: params.StreamID, StreamEpoch: params.StreamEpoch, CursorPresent: true, Cursor: params.Cursor, CursorDigest: params.CursorDigest, Position: params.Position, PendingFinalizeEvidence: &mismatchedEvidence}
	require.False(t, validAuthoritativeResumeCursorResult(params, mismatch), "plugin must echo the exact persisted obligation")

	_, err = inbox.MarkInboundFinalized(evidence)
	require.NoError(t, err)
	params, err = replacement.authoritativeResumeCursorParams(negotiated)
	require.NoError(t, err)
	require.Nil(t, params.PendingFinalizeEvidence, "a committed native-finalize marker clears the resume obligation")
}

func TestRemoteRunnerResumesExactCheckpointSeedWithoutInventingFinalizeEvidence(t *testing.T) {
	root := filepath.Join(t.TempDir(), "cursors")
	store := &DurableCursorStore{Root: root}
	inbox := &InboundInbox{Root: filepath.Join(t.TempDir(), "inbox")}
	negotiated := validDurableNegotiation(proto.RemoteSyncModeShadow)
	key := DurableCursorKey{RemoteIdentity: "device-1", StreamID: negotiated.StreamID, StreamEpoch: negotiated.StreamEpoch}
	cursor := "checkpoint-coverage-cursor-42"
	digest := sha256.Sum256([]byte(cursor))
	seeded, err := store.SeedFromCheckpoint(key, DurableCheckpointSeed{
		Cursor: cursor, CursorDigest: hex.EncodeToString(digest[:]), Position: 42,
		CheckpointEventID: "checkpoint-event-50", CheckpointEventHash: strings.Repeat("a", 64),
		CheckpointAlignmentHash: strings.Repeat("c", 64),
		CheckpointGeneration:    strings.Repeat("b", 64), CheckpointPosition: 50, CheckpointCoverage: 42,
	})
	require.NoError(t, err)

	replacement := &RemoteRunner{DeviceID: "device-1", DurableCursorStore: &DurableCursorStore{Root: root}, DurableInbox: inbox}
	params, err := replacement.authoritativeResumeCursorParams(negotiated)
	require.NoError(t, err)
	require.True(t, params.CursorPresent)
	require.Equal(t, seeded.Cursor, params.Cursor)
	require.Equal(t, seeded.CursorDigest, params.CursorDigest)
	require.Equal(t, seeded.Position, params.Position)
	require.Nil(t, params.PendingFinalizeEvidence,
		"an out-of-band authenticated checkpoint seed has no ordinary cloud ACK/native-finalize obligation")

	nextCursor := "ordinary-cursor-43"
	nextDigest := sha256.Sum256([]byte(nextCursor))
	_, err = store.CompareAndSwap(key, &seeded, DurableCursorState{
		Cursor: nextCursor, CursorDigest: hex.EncodeToString(nextDigest[:]), Position: 43,
	})
	require.NoError(t, err)
	_, err = replacement.authoritativeResumeCursorParams(negotiated)
	require.ErrorIs(t, err, ErrInboundFinalizeEvidenceNotFound,
		"missing inbox evidence for a normal delivery must still fail closed")
}

func exactPluralResumeResult(params proto.RemoteResumeCursorsV1Params) proto.RemoteResumeCursorsV1Result {
	result := proto.RemoteResumeCursorsV1Result{Accepted: true, Cursors: make([]proto.RemoteResumeCursorV1Result, len(params.Cursors))}
	for index, cursor := range params.Cursors {
		result.Cursors[index] = proto.RemoteResumeCursorV1Result{
			Accepted: true, StreamID: cursor.StreamID, StreamEpoch: cursor.StreamEpoch,
			CursorPresent: cursor.CursorPresent, Cursor: cursor.Cursor, CursorDigest: cursor.CursorDigest, Position: cursor.Position,
			PendingFinalizeEvidence: cursor.PendingFinalizeEvidence,
		}
	}
	return result
}

func TestRemoteRunnerBuildsAtomicMultistreamCursorHandoffAcrossRestart(t *testing.T) {
	cursorRoot := filepath.Join(t.TempDir(), "cursors")
	inboxRoot := filepath.Join(t.TempDir(), "inbox")
	store := &DurableCursorStore{Root: cursorRoot}
	inbox := &InboundInbox{Root: inboxRoot}
	negotiated := validMultiStreamNegotiation(proto.RemoteSyncModeDurableRead)
	namespace := negotiated.Streams[1]

	delivery := durableInboxTestDelivery(1)
	delivery.StreamID = namespace.StreamID
	delivery.StreamEpoch = namespace.StreamEpoch
	delivery.Events[0].NamespaceID = namespace.NamespaceID
	evidence := completeDurableInboxTestDelivery(t, inbox, delivery)
	key := DurableCursorKey{RemoteIdentity: "device-1", StreamID: namespace.StreamID, StreamEpoch: namespace.StreamEpoch}
	_, err := store.CompareAndSwap(key, nil, DurableCursorState{Cursor: delivery.Cursor, CursorDigest: delivery.CursorDigest, Position: delivery.Position})
	require.NoError(t, err)

	restarted := &RemoteRunner{
		DeviceID: "device-1", DurableCursorStore: &DurableCursorStore{Root: cursorRoot}, DurableInbox: &InboundInbox{Root: inboxRoot},
	}
	params, err := restarted.authoritativeResumeCursorsParams(negotiated)
	require.NoError(t, err)
	require.Len(t, params.Cursors, len(negotiated.Streams))
	require.Equal(t, negotiated.Streams[0].StreamID, params.Cursors[0].StreamID)
	require.False(t, params.Cursors[0].CursorPresent)
	require.Equal(t, namespace.StreamID, params.Cursors[1].StreamID)
	require.True(t, params.Cursors[1].CursorPresent)
	require.Equal(t, delivery.Cursor, params.Cursors[1].Cursor)
	require.Equal(t, &evidence, params.Cursors[1].PendingFinalizeEvidence, "only the matching namespace stream carries terminal evidence")
	require.Nil(t, params.Cursors[2].PendingFinalizeEvidence)

	exact := exactPluralResumeResult(params)
	require.True(t, validAuthoritativeResumeCursorsResult(params, exact))

	partial := exact
	partial.Cursors = partial.Cursors[:len(partial.Cursors)-1]
	require.False(t, validAuthoritativeResumeCursorsResult(params, partial), "partial echo must not activate any stream")

	reordered := exactPluralResumeResult(params)
	reordered.Cursors[0], reordered.Cursors[1] = reordered.Cursors[1], reordered.Cursors[0]
	require.False(t, validAuthoritativeResumeCursorsResult(params, reordered), "echo order is generation-bound")

	duplicate := exactPluralResumeResult(params)
	duplicate.Cursors[2] = duplicate.Cursors[1]
	require.False(t, validAuthoritativeResumeCursorsResult(params, duplicate))

	changed := exactPluralResumeResult(params)
	changed.Cursors[1].Position++
	require.False(t, validAuthoritativeResumeCursorsResult(params, changed))
}

func TestRemoteRunnerMultistreamResumeFailsClosedOnPartialEchoAndCancellation(t *testing.T) {
	negotiated := validMultiStreamNegotiation(proto.RemoteSyncModeDurableRead)
	runner := &RemoteRunner{DeviceID: "device-1", DurableCursorStore: &DurableCursorStore{Root: filepath.Join(t.TempDir(), "cursors")}}
	base := &remoteSyncNegotiatorStub{}
	remote := &multiRemoteSyncNegotiatorStub{remoteSyncNegotiatorStub: base}
	remote.resume = func(_ context.Context, params proto.RemoteResumeCursorsV1Params) (proto.RemoteResumeCursorsV1Result, error) {
		result := exactPluralResumeResult(params)
		result.Cursors = result.Cursors[:len(result.Cursors)-1]
		return result, nil
	}
	selected, err := runner.resumeNegotiatedMultiStream(context.Background(), remote, negotiated, nil, nil, true)
	require.ErrorContains(t, err, "atomically install exact")
	require.Equal(t, proto.RemoteSyncModeLegacy, selected.Mode)
	require.Equal(t, 1, remote.multiResumeCallCount())
	require.Equal(t, 0, base.resumeCallCount(), "plural failure must not degrade to a sequence of singular installs")

	cancelledBase := &remoteSyncNegotiatorStub{}
	cancelled := &multiRemoteSyncNegotiatorStub{remoteSyncNegotiatorStub: cancelledBase}
	cancelled.resume = func(ctx context.Context, _ proto.RemoteResumeCursorsV1Params) (proto.RemoteResumeCursorsV1Result, error) {
		<-ctx.Done()
		return proto.RemoteResumeCursorsV1Result{}, ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	selected, err = runner.resumeNegotiatedMultiStream(ctx, cancelled, negotiated, nil, nil, true)
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, proto.RemoteSyncModeLegacy, selected.Mode, "a cancelled plugin generation cannot install a later cursor set")
	require.Equal(t, 1, cancelled.multiResumeCallCount())
}

func TestRemoteRunnerShadowKeepsSingularResumeForOldPlugin(t *testing.T) {
	remote := &remoteSyncNegotiatorStub{negotiate: func(_ context.Context, _ int) (proto.RemoteNegotiateSyncV1Result, error) {
		return validDurableNegotiation(proto.RemoteSyncModeShadow), nil
	}}
	manifest := proto.RemotePluginManifestUnsignedV1{Capabilities: []string{
		proto.CapabilityDurableCursorResumeV1,
		proto.CapabilityDurableDeltaSyncV1,
		proto.CapabilityInboundFinalizeV1,
	}}
	runner := &RemoteRunner{DeviceID: "device-1", DurableCursorStore: &DurableCursorStore{Root: filepath.Join(t.TempDir(), "cursors")}}
	selected, err := runner.negotiateRemoteSyncV1(context.Background(), remote, manifest)
	require.NoError(t, err)
	require.Equal(t, proto.RemoteSyncModeShadow, selected.Mode)
	require.Equal(t, 1, remote.resumeCallCount(), "old shadow plugin keeps the singular compatibility handoff")
}

func TestRemoteRunnerShadowMultistreamAtomicallyResumesPendingNamespaceFinalize(t *testing.T) {
	root := t.TempDir()
	store := &DurableCursorStore{Root: filepath.Join(root, "cursors")}
	inbox := &InboundInbox{Root: filepath.Join(root, "inbox")}
	negotiated := validMultiStreamNegotiation(proto.RemoteSyncModeShadow)
	negotiated.CheckpointReady = false
	for index := range negotiated.Streams {
		negotiated.Streams[index].CheckpointReady = false
	}
	namespace := negotiated.Streams[1]
	delivery := durableInboxTestDelivery(1)
	delivery.StreamID = namespace.StreamID
	delivery.StreamEpoch = namespace.StreamEpoch
	delivery.Events[0].NamespaceID = namespace.NamespaceID
	evidence := completeDurableInboxTestDelivery(t, inbox, delivery)
	negotiated.PendingFinalizeEvidence = &evidence
	_, err := store.CompareAndSwap(
		DurableCursorKey{RemoteIdentity: evidence.RemoteIdentity, StreamID: evidence.StreamID, StreamEpoch: evidence.StreamEpoch}, nil,
		DurableCursorState{Cursor: evidence.Cursor, CursorDigest: evidence.CursorDigest, Position: evidence.Position},
	)
	require.NoError(t, err)

	base := &remoteSyncNegotiatorStub{negotiate: func(context.Context, int) (proto.RemoteNegotiateSyncV1Result, error) {
		return negotiated, nil
	}}
	remote := &multiRemoteSyncNegotiatorStub{remoteSyncNegotiatorStub: base}
	manifest := proto.RemotePluginManifestUnsignedV1{Capabilities: []string{
		proto.CapabilityDurableCursorResumeV1,
		proto.CapabilityDurableDeltaSyncV1,
		proto.CapabilityDurableMultiStreamV1,
		proto.CapabilityInboundFinalizeV1,
	}}
	runner := &RemoteRunner{DeviceID: evidence.RemoteIdentity, DurableCursorStore: store, DurableInbox: inbox}
	selected, err := runner.negotiateRemoteSyncV1(context.Background(), remote, manifest)
	require.NoError(t, err)
	require.Equal(t, proto.RemoteSyncModeShadow, selected.Mode)
	require.Equal(t, 1, remote.multiResumeCallCount())
	require.Zero(t, base.resumeCallCount(), "a descriptor roster must never be partially installed through singular resume")
	params := remote.lastMultiResumeParams()
	require.Len(t, params.Cursors, len(negotiated.Streams))
	require.Equal(t, &evidence, params.Cursors[1].PendingFinalizeEvidence)
	require.Nil(t, params.Cursors[0].PendingFinalizeEvidence)
	require.Nil(t, params.Cursors[2].PendingFinalizeEvidence)
}

func TestRemoteRunnerMultistreamTerminalRecoveryRejectsNamespaceRemap(t *testing.T) {
	for _, alreadyFinalized := range []bool{false, true} {
		name := "daemon pending evidence"
		if alreadyFinalized {
			name = "plugin finalized proposal"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			store := &DurableCursorStore{Root: filepath.Join(root, "cursors")}
			inbox := &InboundInbox{Root: filepath.Join(root, "inbox")}
			candidate := validMultiStreamNegotiation(proto.RemoteSyncModeShadow)
			namespace := candidate.Streams[1]
			delivery := durableInboxTestDelivery(1)
			delivery.StreamID = namespace.StreamID
			delivery.StreamEpoch = namespace.StreamEpoch
			delivery.Events[0].NamespaceID = namespace.NamespaceID
			evidence := completeDurableInboxTestDelivery(t, inbox, delivery)
			_, err := store.CompareAndSwap(
				DurableCursorKey{RemoteIdentity: evidence.RemoteIdentity, StreamID: evidence.StreamID, StreamEpoch: evidence.StreamEpoch}, nil,
				DurableCursorState{Cursor: evidence.Cursor, CursorDigest: evidence.CursorDigest, Position: evidence.Position},
			)
			require.NoError(t, err)
			if alreadyFinalized {
				_, err = inbox.MarkInboundFinalized(evidence)
				require.NoError(t, err)
			}
			candidate.Streams = append([]proto.RemoteStreamDescriptorV1(nil), candidate.Streams...)
			candidate.Streams[1].NamespaceID = "namespace-aa"
			candidate.PendingFinalizeEvidence = &evidence
			base := &remoteSyncNegotiatorStub{negotiate: func(context.Context, int) (proto.RemoteNegotiateSyncV1Result, error) {
				return candidate, nil
			}}
			remote := &multiRemoteSyncNegotiatorStub{remoteSyncNegotiatorStub: base}
			manifest := proto.RemotePluginManifestUnsignedV1{Capabilities: []string{
				proto.CapabilityDurableCursorResumeV1,
				proto.CapabilityDurableDeltaSyncV1,
				proto.CapabilityDurableMultiStreamV1,
				proto.CapabilityInboundFinalizeV1,
			}}
			runner := &RemoteRunner{DeviceID: evidence.RemoteIdentity, DurableCursorStore: store, DurableInbox: inbox}
			selected, err := runner.negotiateRemoteSyncV1(context.Background(), remote, manifest)
			require.ErrorIs(t, err, ErrRemoteFinalizeRecoveryBlocked)
			require.Equal(t, proto.RemoteSyncModeLegacy, selected.Mode)
			require.Zero(t, remote.multiResumeCallCount(), "namespace-remapped terminal evidence must be rejected before cursor installation")
			require.Zero(t, base.resumeCallCount())
		})
	}
}

func TestRemoteRunnerDescriptorChangeDoesNotReuseUnknownGeneration(t *testing.T) {
	first := validMultiStreamNegotiation(proto.RemoteSyncModeDurableRead)
	runner := &RemoteRunner{DeviceID: "device-1", DurableCursorStore: &DurableCursorStore{Root: filepath.Join(t.TempDir(), "cursors")}}
	runner.setSyncNegotiation(first)

	oldNamespace := first.Streams[1]
	_, err := runner.FetchV2(context.Background(), proto.RemoteFetchV2Params{StreamID: oldNamespace.StreamID, StreamEpoch: oldNamespace.StreamEpoch})
	require.ErrorIs(t, err, ErrRemoteReconnecting, "known namespace reaches the current plugin generation")
	_, err = runner.FetchV2(context.Background(), proto.RemoteFetchV2Params{StreamID: "unknown", StreamEpoch: "unknown"})
	require.ErrorIs(t, err, ErrRemoteNotConfigured)
	_, err = runner.FetchParentV1(context.Background(), proto.RemoteFetchParentV1Params{StreamID: oldNamespace.StreamID, StreamEpoch: oldNamespace.StreamEpoch, NamespaceID: oldNamespace.NamespaceID})
	require.ErrorIs(t, err, ErrRemoteReconnecting)
	_, err = runner.FetchParentV1(context.Background(), proto.RemoteFetchParentV1Params{StreamID: oldNamespace.StreamID, StreamEpoch: oldNamespace.StreamEpoch, NamespaceID: "namespace-b"})
	require.ErrorIs(t, err, ErrRemoteNotConfigured, "a valid stream cannot be relabeled as another namespace")
	_, err = runner.AckV2(context.Background(), proto.RemoteAckV2Params{StreamID: oldNamespace.StreamID, StreamEpoch: oldNamespace.StreamEpoch})
	require.ErrorIs(t, err, ErrRemoteReconnecting)
	_, err = runner.AckV2(context.Background(), proto.RemoteAckV2Params{StreamID: "unknown", StreamEpoch: "unknown"})
	require.ErrorIs(t, err, ErrRemoteNotConfigured)

	second := validMultiStreamNegotiation(proto.RemoteSyncModeDurableRead)
	second.Streams = append([]proto.RemoteStreamDescriptorV1(nil), second.Streams...)
	second.Streams[1].StreamID = "namespace-stream-a-next"
	second.Streams[1].StreamEpoch = "namespace-epoch-a-next"
	runner.setSyncNegotiation(second)
	_, err = runner.FetchV2(context.Background(), proto.RemoteFetchV2Params{StreamID: oldNamespace.StreamID, StreamEpoch: oldNamespace.StreamEpoch})
	require.ErrorIs(t, err, ErrRemoteNotConfigured, "old descriptor is rejected immediately after an atomic generation swap")
	params, err := runner.authoritativeResumeCursorsParams(second)
	require.NoError(t, err)
	require.Equal(t, "namespace-stream-a-next", params.Cursors[1].StreamID)
	require.False(t, params.Cursors[1].CursorPresent, "a changed epoch cannot inherit the old stream cursor")
}

func TestRemoteRunnerAuthoritativeCursorHandoffFailsClosed(t *testing.T) {
	negotiated := validDurableNegotiation(proto.RemoteSyncModeShadow)
	_, err := (&RemoteRunner{DeviceID: "device-1"}).authoritativeResumeCursorParams(negotiated)
	require.ErrorIs(t, err, ErrRemoteNotConfigured)
	_, err = (&RemoteRunner{DurableCursorStore: &DurableCursorStore{Root: filepath.Join(t.TempDir(), "cursors")}}).authoritativeResumeCursorParams(negotiated)
	require.ErrorIs(t, err, ErrRemoteNotConfigured)

	absent := proto.RemoteResumeCursorV1Params{Authoritative: true, StreamID: "stream", StreamEpoch: "epoch"}
	require.False(t, validAuthoritativeResumeCursorResult(absent, proto.RemoteResumeCursorV1Result{Accepted: true, Cursor: "plugin-stale-cursor"}))
	absent.PendingFinalizeEvidence = &proto.RemoteInboundFinalizeEvidenceV1{DeliveryID: "impossible-without-cursor"}
	require.False(t, validAuthoritativeResumeCursorResult(absent, proto.RemoteResumeCursorV1Result{Accepted: true, StreamID: absent.StreamID, StreamEpoch: absent.StreamEpoch, PendingFinalizeEvidence: absent.PendingFinalizeEvidence}))
}

func TestRemoteSyncNegotiationRefreshesBeforePluginFreshnessExpiryAndFallsBack(t *testing.T) {
	require.Less(t, remoteSyncNegotiationRefreshInterval, 5*time.Minute)
	require.Greater(t, remoteSyncNegotiationRefreshInterval, remoteSyncNegotiationTimeout)

	runner := &RemoteRunner{}
	runner.setSyncNegotiation(validDurableNegotiation(proto.RemoteSyncModeShadow))
	stub := &remoteSyncNegotiatorStub{negotiate: func(context.Context, int) (proto.RemoteNegotiateSyncV1Result, error) {
		return proto.RemoteNegotiateSyncV1Result{}, errors.New("control plane temporarily unavailable")
	}}
	ctx, cancel := context.WithCancel(context.Background())
	ticks := make(chan time.Time, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		runner.refreshRemoteSyncNegotiation(ctx, stub, durableRefreshManifestForTest(), ticks)
	}()
	ticks <- time.Now()
	require.Eventually(t, func() bool {
		return stub.callCount() == 1 && runner.SyncNegotiation().Mode == proto.RemoteSyncModeLegacy
	}, time.Second, 10*time.Millisecond, "a failed refresh must immediately disable durable methods")
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("refresh loop did not stop after cancellation")
	}
}

func TestRemoteSyncNegotiationRefreshCancellationInterruptsInflightRPC(t *testing.T) {
	started := make(chan struct{})
	var once sync.Once
	stub := &remoteSyncNegotiatorStub{negotiate: func(ctx context.Context, _ int) (proto.RemoteNegotiateSyncV1Result, error) {
		once.Do(func() { close(started) })
		<-ctx.Done()
		return proto.RemoteNegotiateSyncV1Result{}, ctx.Err()
	}}
	runner := &RemoteRunner{}
	ctx, cancel := context.WithCancel(context.Background())
	ticks := make(chan time.Time, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		runner.refreshRemoteSyncNegotiation(ctx, stub, durableRefreshManifestForTest(), ticks)
	}()
	ticks <- time.Now()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("refresh RPC did not start")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("in-flight refresh RPC ignored run cancellation")
	}
	require.Equal(t, proto.RemoteSyncModeLegacy, runner.SyncNegotiation().Mode)
}

func TestRemoteSyncNegotiationRefreshStopsBeforePluginReplacement(t *testing.T) {
	runner := &RemoteRunner{}
	oldResult := validDurableNegotiation(proto.RemoteSyncModeShadow)
	oldResult.StreamEpoch = "old-plugin-epoch"
	oldRemote := &remoteSyncNegotiatorStub{negotiate: func(context.Context, int) (proto.RemoteNegotiateSyncV1Result, error) { return oldResult, nil }}
	oldCtx, cancelOld := context.WithCancel(context.Background())
	oldTicks := make(chan time.Time, 1)
	oldDone := make(chan struct{})
	go func() {
		defer close(oldDone)
		runner.refreshRemoteSyncNegotiation(oldCtx, oldRemote, durableRefreshManifestForTest(), oldTicks)
	}()
	oldTicks <- time.Now()
	require.Eventually(t, func() bool { return runner.SyncNegotiation().StreamEpoch == oldResult.StreamEpoch }, time.Second, 10*time.Millisecond)
	cancelOld()
	<-oldDone

	newResult := validDurableNegotiation(proto.RemoteSyncModeShadow)
	newResult.StreamEpoch = "replacement-plugin-epoch"
	newRemote := &remoteSyncNegotiatorStub{negotiate: func(context.Context, int) (proto.RemoteNegotiateSyncV1Result, error) { return newResult, nil }}
	newCtx, cancelNew := context.WithCancel(context.Background())
	newTicks := make(chan time.Time, 1)
	newDone := make(chan struct{})
	go func() {
		defer close(newDone)
		runner.refreshRemoteSyncNegotiation(newCtx, newRemote, durableRefreshManifestForTest(), newTicks)
	}()
	newTicks <- time.Now()
	require.Eventually(t, func() bool { return runner.SyncNegotiation().StreamEpoch == newResult.StreamEpoch }, time.Second, 10*time.Millisecond)

	oldTicks <- time.Now()
	require.Equal(t, 1, oldRemote.callCount(), "a cancelled plugin generation must never renegotiate or overwrite its replacement")
	cancelNew()
	<-newDone
}

func TestRemoteSyncNegotiationRequiresSignedManifestCapability(t *testing.T) {
	stub := &remoteSyncNegotiatorStub{negotiate: func(context.Context, int) (proto.RemoteNegotiateSyncV1Result, error) {
		return validDurableNegotiation(proto.RemoteSyncModeShadow), nil
	}}
	result, err := (&RemoteRunner{}).negotiateRemoteSyncV1(context.Background(), stub, proto.RemotePluginManifestUnsignedV1{})
	require.NoError(t, err)
	require.Equal(t, proto.RemoteSyncModeLegacy, result.Mode)
	require.Zero(t, stub.callCount(), "unsigned runtime claims must never be queried or trusted")
}

func TestShadowPendingFinalizeHandoffRequiresSignedAndRuntimeCapability(t *testing.T) {
	root := t.TempDir()
	store := &DurableCursorStore{Root: filepath.Join(root, "cursors")}
	inbox := &InboundInbox{Root: filepath.Join(root, "inbox")}
	candidate := validDurableNegotiation(proto.RemoteSyncModeShadow)
	delivery := durableInboxTestDelivery(1)
	delivery.StreamID, delivery.StreamEpoch = candidate.StreamID, candidate.StreamEpoch
	evidence := completeDurableInboxTestDelivery(t, inbox, delivery)
	candidate.PendingFinalizeEvidence = &evidence
	_, err := store.CompareAndSwap(
		DurableCursorKey{RemoteIdentity: "device-1", StreamID: candidate.StreamID, StreamEpoch: candidate.StreamEpoch}, nil,
		DurableCursorState{Cursor: delivery.Cursor, CursorDigest: delivery.CursorDigest, Position: delivery.Position},
	)
	require.NoError(t, err)
	newRunner := func() *RemoteRunner {
		return &RemoteRunner{DeviceID: "device-1", DurableCursorStore: store, DurableInbox: inbox}
	}
	newRemote := func(runtime proto.RemoteNegotiateSyncV1Result) *remoteSyncNegotiatorStub {
		return &remoteSyncNegotiatorStub{negotiate: func(context.Context, int) (proto.RemoteNegotiateSyncV1Result, error) { return runtime, nil }}
	}
	unsignedFinalize := proto.RemotePluginManifestUnsignedV1{Capabilities: []string{
		proto.CapabilityDurableCursorResumeV1, proto.CapabilityDurableDeltaSyncV1,
	}}
	remote := newRemote(candidate)
	result, err := newRunner().negotiateRemoteSyncV1(context.Background(), remote, unsignedFinalize)
	require.Error(t, err)
	require.Equal(t, proto.RemoteSyncModeLegacy, result.Mode)
	require.Zero(t, remote.resumeCallCount(), "unsigned plugin must never receive the pending finalize obligation")

	signedFinalize := proto.RemotePluginManifestUnsignedV1{Capabilities: []string{
		proto.CapabilityDurableCursorResumeV1, proto.CapabilityDurableDeltaSyncV1, proto.CapabilityInboundFinalizeV1,
	}}
	remote = newRemote(candidate)
	result, err = newRunner().negotiateRemoteSyncV1(context.Background(), remote, signedFinalize)
	require.NoError(t, err)
	require.Equal(t, proto.RemoteSyncModeShadow, result.Mode)
	require.Equal(t, 1, remote.resumeCallCount(), "capable shadow plugin must drain the exact preexisting obligation")

	missingEcho := candidate
	missingEcho.PendingFinalizeEvidence = nil
	remote = newRemote(missingEcho)
	result, err = newRunner().negotiateRemoteSyncV1(context.Background(), remote, signedFinalize)
	require.ErrorIs(t, err, ErrRemoteFinalizeRecoveryBlocked)
	require.Equal(t, proto.RemoteSyncReasonTerminalFinalizeRecoveryStateInvalid, result.Reason)
	require.Zero(t, remote.resumeCallCount(), "daemon preflight evidence requires an exact candidate echo")

	withoutRuntimeFinalize := candidate
	withoutRuntimeFinalize.ServerCapabilities = []string{
		proto.CapabilityDurableCursorResumeV1, proto.CapabilityDurableDeltaSyncV1, proto.CapabilityInboundAckV2,
	}
	remote = newRemote(withoutRuntimeFinalize)
	result, err = newRunner().negotiateRemoteSyncV1(context.Background(), remote, signedFinalize)
	require.Error(t, err)
	require.Equal(t, proto.RemoteSyncModeLegacy, result.Mode)
	require.Zero(t, remote.resumeCallCount(), "runtime without finalize must never receive the obligation")
}

func TestRemoteSyncNegotiationPreflightsAndDrainsSameEpochAfterPluginStateLoss(t *testing.T) {
	root := t.TempDir()
	store := &DurableCursorStore{Root: filepath.Join(root, "cursors")}
	inbox := &InboundInbox{Root: filepath.Join(root, "inbox")}
	candidate := validDurableNegotiation(proto.RemoteSyncModeShadow)
	delivery := durableInboxTestDelivery(1)
	delivery.StreamID, delivery.StreamEpoch = candidate.StreamID, candidate.StreamEpoch
	evidence := completeDurableInboxTestDelivery(t, inbox, delivery)
	candidate.PendingFinalizeEvidence = &evidence
	_, err := store.CompareAndSwap(
		DurableCursorKey{RemoteIdentity: evidence.RemoteIdentity, StreamID: evidence.StreamID, StreamEpoch: evidence.StreamEpoch}, nil,
		DurableCursorState{Cursor: evidence.Cursor, CursorDigest: evidence.CursorDigest, Position: evidence.Position},
	)
	require.NoError(t, err)

	var resumed proto.RemoteResumeCursorV1Params
	remote := &remoteSyncNegotiatorStub{
		negotiate: func(context.Context, int) (proto.RemoteNegotiateSyncV1Result, error) { return candidate, nil },
		resume: func(_ context.Context, params proto.RemoteResumeCursorV1Params) (proto.RemoteResumeCursorV1Result, error) {
			resumed = params
			return proto.RemoteResumeCursorV1Result{
				Accepted: true, StreamID: params.StreamID, StreamEpoch: params.StreamEpoch,
				CursorPresent: params.CursorPresent, Cursor: params.Cursor, CursorDigest: params.CursorDigest, Position: params.Position,
				PendingFinalizeEvidence: params.PendingFinalizeEvidence,
			}, nil
		},
	}
	manifest := proto.RemotePluginManifestUnsignedV1{Capabilities: []string{
		proto.CapabilityDurableCursorResumeV1, proto.CapabilityDurableDeltaSyncV1, proto.CapabilityInboundFinalizeV1,
	}}
	runner := &RemoteRunner{DeviceID: evidence.RemoteIdentity, DurableCursorStore: store, DurableInbox: inbox}
	result, err := runner.negotiateRemoteSyncV1(context.Background(), remote, manifest)
	require.NoError(t, err)
	require.Equal(t, proto.RemoteSyncModeShadow, result.Mode)
	require.Equal(t, 1, remote.callCount())
	require.Equal(t, 1, remote.resumeCallCount())
	require.Equal(t, &evidence, remote.lastNegotiationParams().PendingFinalizeEvidence,
		"the exact old terminal evidence must be visible before the server selects an epoch")
	require.Equal(t, &evidence, resumed.PendingFinalizeEvidence,
		"authoritative resume must re-present the identical preflight evidence")

	rejecting := &remoteSyncNegotiatorStub{
		negotiate: func(context.Context, int) (proto.RemoteNegotiateSyncV1Result, error) { return candidate, nil },
		resume: func(_ context.Context, params proto.RemoteResumeCursorV1Params) (proto.RemoteResumeCursorV1Result, error) {
			return proto.RemoteResumeCursorV1Result{
				Accepted: true, StreamID: params.StreamID, StreamEpoch: params.StreamEpoch,
				CursorPresent: params.CursorPresent, Cursor: params.Cursor, CursorDigest: params.CursorDigest, Position: params.Position,
				// Omitting PendingFinalizeEvidence proves that a partial resume
				// cannot activate even drain-only shadow mode.
			}, nil
		},
	}
	result, err = runner.negotiateRemoteSyncV1(context.Background(), rejecting, manifest)
	require.ErrorIs(t, err, ErrRemoteFinalizeRecoveryBlocked)
	require.Equal(t, proto.RemoteSyncModeLegacy, result.Mode)
	require.Equal(t, proto.RemoteSyncReasonTerminalFinalizeRecoveryStateInvalid, result.Reason)
	require.Equal(t, 1, rejecting.resumeCallCount())
}

func TestRemoteSyncNegotiationRepairsAdjacentCompletedCursorBeforePreflight(t *testing.T) {
	root := t.TempDir()
	store := &DurableCursorStore{Root: filepath.Join(root, "cursors")}
	inbox := &InboundInbox{Root: filepath.Join(root, "inbox")}
	candidate := validDurableNegotiation(proto.RemoteSyncModeShadow)
	first := durableInboxTestDelivery(1)
	first.StreamID, first.StreamEpoch = candidate.StreamID, candidate.StreamEpoch
	key := DurableCursorKey{RemoteIdentity: "device-1", StreamID: candidate.StreamID, StreamEpoch: candidate.StreamEpoch}
	_, err := store.CompareAndSwap(key, nil, DurableCursorState{
		Cursor: first.Cursor, CursorDigest: first.CursorDigest, Position: first.Position,
	})
	require.NoError(t, err)

	second := durableInboxTestDelivery(2)
	second.StreamID, second.StreamEpoch = candidate.StreamID, candidate.StreamEpoch
	evidence := completeDurableInboxTestDelivery(t, inbox, second)
	candidate.PendingFinalizeEvidence = &evidence
	// Simulate process death after CompleteDurable but before the successor
	// cursor CAS. No inbound redelivery/callback occurs before negotiation.

	var resumed proto.RemoteResumeCursorV1Params
	remote := &remoteSyncNegotiatorStub{
		negotiate: func(context.Context, int) (proto.RemoteNegotiateSyncV1Result, error) { return candidate, nil },
		resume: func(_ context.Context, params proto.RemoteResumeCursorV1Params) (proto.RemoteResumeCursorV1Result, error) {
			resumed = params
			return proto.RemoteResumeCursorV1Result{
				Accepted: true, StreamID: params.StreamID, StreamEpoch: params.StreamEpoch,
				CursorPresent: params.CursorPresent, Cursor: params.Cursor, CursorDigest: params.CursorDigest, Position: params.Position,
				PendingFinalizeEvidence: params.PendingFinalizeEvidence,
			}, nil
		},
	}
	manifest := proto.RemotePluginManifestUnsignedV1{Capabilities: []string{
		proto.CapabilityDurableCursorResumeV1, proto.CapabilityDurableDeltaSyncV1, proto.CapabilityInboundFinalizeV1,
	}}
	runner := &RemoteRunner{DeviceID: evidence.RemoteIdentity, DurableCursorStore: store, DurableInbox: inbox}
	result, err := runner.negotiateRemoteSyncV1(context.Background(), remote, manifest)
	require.NoError(t, err)
	require.Equal(t, proto.RemoteSyncModeShadow, result.Mode)
	require.Equal(t, &evidence, remote.lastNegotiationParams().PendingFinalizeEvidence)
	require.Equal(t, &evidence, resumed.PendingFinalizeEvidence)
	require.Equal(t, second.Cursor, resumed.Cursor)
	require.Equal(t, second.Position, resumed.Position)

	repaired, err := (&DurableCursorStore{Root: store.Root}).Load(key)
	require.NoError(t, err)
	require.Equal(t, second.Cursor, repaired.Cursor)
	require.Equal(t, second.CursorDigest, repaired.CursorDigest)
	require.Equal(t, second.Position, repaired.Position)
}

func TestRemoteSyncNegotiationAcceptsExactAlreadyFinalizedPluginProposal(t *testing.T) {
	root := t.TempDir()
	store := &DurableCursorStore{Root: filepath.Join(root, "cursors")}
	inbox := &InboundInbox{Root: filepath.Join(root, "inbox")}
	candidate := validDurableNegotiation(proto.RemoteSyncModeShadow)
	delivery := durableInboxTestDelivery(1)
	delivery.StreamID, delivery.StreamEpoch = candidate.StreamID, candidate.StreamEpoch
	evidence := completeDurableInboxTestDelivery(t, inbox, delivery)
	_, err := store.CompareAndSwap(
		DurableCursorKey{RemoteIdentity: evidence.RemoteIdentity, StreamID: evidence.StreamID, StreamEpoch: evidence.StreamEpoch}, nil,
		DurableCursorState{Cursor: evidence.Cursor, CursorDigest: evidence.CursorDigest, Position: evidence.Position},
	)
	require.NoError(t, err)
	_, err = inbox.MarkInboundFinalized(evidence)
	require.NoError(t, err)
	candidate.PendingFinalizeEvidence = &evidence

	var resumed proto.RemoteResumeCursorV1Params
	remote := &remoteSyncNegotiatorStub{
		negotiate: func(context.Context, int) (proto.RemoteNegotiateSyncV1Result, error) { return candidate, nil },
		resume: func(_ context.Context, params proto.RemoteResumeCursorV1Params) (proto.RemoteResumeCursorV1Result, error) {
			resumed = params
			return proto.RemoteResumeCursorV1Result{
				Accepted: true, StreamID: params.StreamID, StreamEpoch: params.StreamEpoch,
				CursorPresent: params.CursorPresent, Cursor: params.Cursor, CursorDigest: params.CursorDigest, Position: params.Position,
				PendingFinalizeEvidence: params.PendingFinalizeEvidence,
			}, nil
		},
	}
	manifest := proto.RemotePluginManifestUnsignedV1{Capabilities: []string{
		proto.CapabilityDurableCursorResumeV1, proto.CapabilityDurableDeltaSyncV1, proto.CapabilityInboundFinalizeV1,
	}}
	runner := &RemoteRunner{DeviceID: evidence.RemoteIdentity, DurableCursorStore: store, DurableInbox: inbox}
	result, err := runner.negotiateRemoteSyncV1(context.Background(), remote, manifest)
	require.NoError(t, err)
	require.Equal(t, proto.RemoteSyncModeShadow, result.Mode)
	require.Nil(t, remote.lastNegotiationParams().PendingFinalizeEvidence,
		"already-finalized daemon state must not invent a preflight obligation")
	require.Equal(t, &evidence, resumed.PendingFinalizeEvidence,
		"the independently validated plugin proposal is promoted only into authoritative Resume")
	alreadyFinalized, err := inbox.PrepareInboundFinalize(*resumed.PendingFinalizeEvidence)
	require.NoError(t, err)
	require.True(t, alreadyFinalized, "the plugin's retry will receive AlreadyFinalized without rematerializing")
}

func TestRemoteSyncNegotiationRejectsUnprovenPluginFinalizeProposals(t *testing.T) {
	manifest := proto.RemotePluginManifestUnsignedV1{Capabilities: []string{
		proto.CapabilityDurableCursorResumeV1, proto.CapabilityDurableDeltaSyncV1, proto.CapabilityInboundFinalizeV1,
	}}

	t.Run("absent retained completion", func(t *testing.T) {
		root := t.TempDir()
		store := &DurableCursorStore{Root: filepath.Join(root, "cursors")}
		inbox := &InboundInbox{Root: filepath.Join(root, "inbox")}
		candidate := validDurableNegotiation(proto.RemoteSyncModeShadow)
		delivery := durableInboxTestDelivery(1)
		delivery.StreamID, delivery.StreamEpoch = candidate.StreamID, candidate.StreamEpoch
		temporaryInbox := &InboundInbox{Root: filepath.Join(root, "temporary-inbox")}
		evidence := completeDurableInboxTestDelivery(t, temporaryInbox, delivery)
		_, err := store.CompareAndSwap(
			DurableCursorKey{RemoteIdentity: evidence.RemoteIdentity, StreamID: evidence.StreamID, StreamEpoch: evidence.StreamEpoch}, nil,
			DurableCursorState{Cursor: evidence.Cursor, CursorDigest: evidence.CursorDigest, Position: evidence.Position},
		)
		require.NoError(t, err)
		candidate.PendingFinalizeEvidence = &evidence
		remote := &remoteSyncNegotiatorStub{negotiate: func(context.Context, int) (proto.RemoteNegotiateSyncV1Result, error) { return candidate, nil }}
		result, err := (&RemoteRunner{DeviceID: evidence.RemoteIdentity, DurableCursorStore: store, DurableInbox: inbox}).negotiateRemoteSyncV1(context.Background(), remote, manifest)
		require.ErrorIs(t, err, ErrRemoteFinalizeRecoveryBlocked)
		require.Equal(t, proto.RemoteSyncReasonTerminalFinalizeRecoveryStateInvalid, result.Reason)
		require.Zero(t, remote.resumeCallCount())
	})

	t.Run("unfinalized completion appeared during negotiation", func(t *testing.T) {
		root := t.TempDir()
		store := &DurableCursorStore{Root: filepath.Join(root, "cursors")}
		inbox := &InboundInbox{Root: filepath.Join(root, "inbox")}
		candidate := validDurableNegotiation(proto.RemoteSyncModeShadow)
		delivery := durableInboxTestDelivery(1)
		delivery.StreamID, delivery.StreamEpoch = candidate.StreamID, candidate.StreamEpoch
		var evidence proto.RemoteInboundFinalizeEvidenceV1
		remote := &remoteSyncNegotiatorStub{negotiate: func(context.Context, int) (proto.RemoteNegotiateSyncV1Result, error) {
			evidence = completeDurableInboxTestDelivery(t, inbox, delivery)
			_, err := store.CompareAndSwap(
				DurableCursorKey{RemoteIdentity: evidence.RemoteIdentity, StreamID: evidence.StreamID, StreamEpoch: evidence.StreamEpoch}, nil,
				DurableCursorState{Cursor: evidence.Cursor, CursorDigest: evidence.CursorDigest, Position: evidence.Position},
			)
			require.NoError(t, err)
			candidate.PendingFinalizeEvidence = &evidence
			return candidate, nil
		}}
		result, err := (&RemoteRunner{DeviceID: "device-1", DurableCursorStore: store, DurableInbox: inbox}).negotiateRemoteSyncV1(context.Background(), remote, manifest)
		require.ErrorIs(t, err, ErrRemoteFinalizeRecoveryBlocked)
		require.Equal(t, proto.RemoteSyncReasonTerminalFinalizeRecoveryStateInvalid, result.Reason)
		require.Zero(t, remote.resumeCallCount())
	})

	t.Run("full tuple mismatch", func(t *testing.T) {
		root := t.TempDir()
		store := &DurableCursorStore{Root: filepath.Join(root, "cursors")}
		inbox := &InboundInbox{Root: filepath.Join(root, "inbox")}
		candidate := validDurableNegotiation(proto.RemoteSyncModeShadow)
		delivery := durableInboxTestDelivery(1)
		delivery.StreamID, delivery.StreamEpoch = candidate.StreamID, candidate.StreamEpoch
		evidence := completeDurableInboxTestDelivery(t, inbox, delivery)
		_, err := store.CompareAndSwap(
			DurableCursorKey{RemoteIdentity: evidence.RemoteIdentity, StreamID: evidence.StreamID, StreamEpoch: evidence.StreamEpoch}, nil,
			DurableCursorState{Cursor: evidence.Cursor, CursorDigest: evidence.CursorDigest, Position: evidence.Position},
		)
		require.NoError(t, err)
		_, err = inbox.MarkInboundFinalized(evidence)
		require.NoError(t, err)
		substituted := evidence
		substituted.CanonicalHash = strings.Repeat("0", sha256.Size*2)
		candidate.PendingFinalizeEvidence = &substituted
		remote := &remoteSyncNegotiatorStub{negotiate: func(context.Context, int) (proto.RemoteNegotiateSyncV1Result, error) { return candidate, nil }}
		result, err := (&RemoteRunner{DeviceID: evidence.RemoteIdentity, DurableCursorStore: store, DurableInbox: inbox}).negotiateRemoteSyncV1(context.Background(), remote, manifest)
		require.ErrorIs(t, err, ErrRemoteFinalizeRecoveryBlocked)
		require.Equal(t, proto.RemoteSyncReasonTerminalFinalizeRecoveryStateInvalid, result.Reason)
		require.Zero(t, remote.resumeCallCount())

		wrongGeneration := candidate
		wrongGeneration.StreamEpoch = "substituted-epoch"
		wrongGeneration.PendingFinalizeEvidence = &evidence
		remote = &remoteSyncNegotiatorStub{negotiate: func(context.Context, int) (proto.RemoteNegotiateSyncV1Result, error) { return wrongGeneration, nil }}
		result, err = (&RemoteRunner{DeviceID: evidence.RemoteIdentity, DurableCursorStore: store, DurableInbox: inbox}).negotiateRemoteSyncV1(context.Background(), remote, manifest)
		require.ErrorIs(t, err, ErrRemoteFinalizeRecoveryBlocked)
		require.Equal(t, proto.RemoteSyncReasonTerminalFinalizeRecoveryStateInvalid, result.Reason)
		require.Zero(t, remote.resumeCallCount())
	})
}

func TestRemoteSyncNegotiationBlocksOldFinalizeObligationFromNewEpoch(t *testing.T) {
	root := t.TempDir()
	store := &DurableCursorStore{Root: filepath.Join(root, "cursors")}
	inbox := &InboundInbox{Root: filepath.Join(root, "inbox")}
	delivery := durableInboxTestDelivery(1)
	delivery.StreamID, delivery.StreamEpoch = "old-stream", "old-epoch"
	evidence := completeDurableInboxTestDelivery(t, inbox, delivery)
	_, err := store.CompareAndSwap(
		DurableCursorKey{RemoteIdentity: evidence.RemoteIdentity, StreamID: evidence.StreamID, StreamEpoch: evidence.StreamEpoch}, nil,
		DurableCursorState{Cursor: evidence.Cursor, CursorDigest: evidence.CursorDigest, Position: evidence.Position},
	)
	require.NoError(t, err)

	newGeneration := validDurableNegotiation(proto.RemoteSyncModeShadow)
	newGeneration.StreamID, newGeneration.StreamEpoch = "new-stream", "new-epoch"
	newGeneration.PendingFinalizeEvidence = &evidence
	remote := &remoteSyncNegotiatorStub{negotiate: func(context.Context, int) (proto.RemoteNegotiateSyncV1Result, error) {
		return newGeneration, nil
	}}
	manifest := proto.RemotePluginManifestUnsignedV1{Capabilities: []string{
		proto.CapabilityDurableCursorResumeV1, proto.CapabilityDurableDeltaSyncV1, proto.CapabilityInboundFinalizeV1,
	}}
	runner := &RemoteRunner{DeviceID: evidence.RemoteIdentity, DurableCursorStore: store, DurableInbox: inbox}
	result, err := runner.negotiateRemoteSyncV1(context.Background(), remote, manifest)
	require.ErrorIs(t, err, ErrRemoteFinalizeRecoveryBlocked)
	require.Equal(t, proto.RemoteSyncModeLegacy, result.Mode)
	require.Equal(t, proto.RemoteSyncReasonTerminalFinalizeRecoveryBlocked, result.Reason)
	require.Equal(t, &evidence, remote.lastNegotiationParams().PendingFinalizeEvidence)
	require.Zero(t, remote.resumeCallCount(), "a new epoch must not activate while old terminal evidence remains")
}

func TestRemoteSyncNegotiationFailsClosedBeforeRPCForAmbiguousOrUnboundFinalizeEvidence(t *testing.T) {
	manifest := proto.RemotePluginManifestUnsignedV1{Capabilities: []string{
		proto.CapabilityDurableCursorResumeV1, proto.CapabilityDurableDeltaSyncV1, proto.CapabilityInboundFinalizeV1,
	}}

	t.Run("non-adjacent predecessor cursor", func(t *testing.T) {
		root := t.TempDir()
		inbox := &InboundInbox{Root: filepath.Join(root, "inbox")}
		store := &DurableCursorStore{Root: filepath.Join(root, "cursors")}
		first := durableInboxTestDelivery(1)
		_, err := store.CompareAndSwap(
			DurableCursorKey{RemoteIdentity: "device-1", StreamID: first.StreamID, StreamEpoch: first.StreamEpoch}, nil,
			DurableCursorState{Cursor: first.Cursor, CursorDigest: first.CursorDigest, Position: first.Position},
		)
		require.NoError(t, err)
		nonAdjacent := durableInboxTestDelivery(3)
		evidence := completeDurableInboxTestDelivery(t, inbox, nonAdjacent)
		remote := &remoteSyncNegotiatorStub{negotiate: func(context.Context, int) (proto.RemoteNegotiateSyncV1Result, error) {
			return validDurableNegotiation(proto.RemoteSyncModeShadow), nil
		}}
		runner := &RemoteRunner{DeviceID: evidence.RemoteIdentity, DurableCursorStore: store, DurableInbox: inbox}
		result, err := runner.negotiateRemoteSyncV1(context.Background(), remote, manifest)
		require.ErrorIs(t, err, ErrRemoteFinalizeRecoveryBlocked)
		require.Equal(t, proto.RemoteSyncModeLegacy, result.Mode)
		require.Zero(t, remote.callCount())
	})

	t.Run("multiple old epochs", func(t *testing.T) {
		root := t.TempDir()
		store := &DurableCursorStore{Root: filepath.Join(root, "cursors")}
		inbox := &InboundInbox{Root: filepath.Join(root, "inbox")}
		first := durableInboxTestDelivery(1)
		first.StreamID, first.StreamEpoch = "stream-old-a", "epoch-old-a"
		firstEvidence := completeDurableInboxTestDelivery(t, inbox, first)
		_, err := store.CompareAndSwap(
			DurableCursorKey{RemoteIdentity: firstEvidence.RemoteIdentity, StreamID: firstEvidence.StreamID, StreamEpoch: firstEvidence.StreamEpoch}, nil,
			DurableCursorState{Cursor: firstEvidence.Cursor, CursorDigest: firstEvidence.CursorDigest, Position: firstEvidence.Position},
		)
		require.NoError(t, err)

		second := durableInboxTestDelivery(1)
		second.DeliveryID = "durable-delivery-second"
		second.StreamID, second.StreamEpoch = "stream-old-b", "epoch-old-b"
		second.Events[0].EventID = "event-second"
		secondEvidence := completeDurableInboxTestDelivery(t, inbox, second)
		_, err = store.CompareAndSwap(
			DurableCursorKey{RemoteIdentity: secondEvidence.RemoteIdentity, StreamID: secondEvidence.StreamID, StreamEpoch: secondEvidence.StreamEpoch}, nil,
			DurableCursorState{Cursor: secondEvidence.Cursor, CursorDigest: secondEvidence.CursorDigest, Position: secondEvidence.Position},
		)
		require.NoError(t, err)

		remote := &remoteSyncNegotiatorStub{negotiate: func(context.Context, int) (proto.RemoteNegotiateSyncV1Result, error) {
			return validDurableNegotiation(proto.RemoteSyncModeShadow), nil
		}}
		runner := &RemoteRunner{DeviceID: firstEvidence.RemoteIdentity, DurableCursorStore: store, DurableInbox: inbox}
		result, err := runner.negotiateRemoteSyncV1(context.Background(), remote, manifest)
		require.ErrorIs(t, err, ErrRemoteFinalizeRecoveryBlocked)
		require.Equal(t, proto.RemoteSyncModeLegacy, result.Mode)
		require.Zero(t, remote.callCount())
	})
}

func TestShadowWithoutFinalizeObligationRemainsCompatibleAndRPCNeedsSignedRuntimeAuthority(t *testing.T) {
	candidate := validDurableNegotiation(proto.RemoteSyncModeShadow)
	manifestWithoutFinalize := proto.RemotePluginManifestUnsignedV1{Capabilities: []string{
		proto.CapabilityDurableCursorResumeV1, proto.CapabilityDurableDeltaSyncV1,
	}}
	remote := &remoteSyncNegotiatorStub{negotiate: func(context.Context, int) (proto.RemoteNegotiateSyncV1Result, error) { return candidate, nil }}
	runner := &RemoteRunner{DeviceID: "device-1", DurableCursorStore: &DurableCursorStore{Root: filepath.Join(t.TempDir(), "cursors")}}
	result, err := runner.negotiateRemoteSyncV1(context.Background(), remote, manifestWithoutFinalize)
	require.NoError(t, err)
	require.Equal(t, proto.RemoteSyncModeShadow, result.Mode)

	runner.setSyncNegotiation(result)
	require.False(t, runner.SignedInboundFinalizeReady(), "runtime claims without signed manifest authority must fail closed")
	runner.setSyncNegotiationWithManifest(result, proto.RemotePluginManifestUnsignedV1{Capabilities: []string{proto.CapabilityInboundFinalizeV1}})
	require.True(t, runner.SignedInboundFinalizeReady())
	result.ServerCapabilities = []string{proto.CapabilityDurableDeltaSyncV1, proto.CapabilityInboundAckV2}
	runner.setSyncNegotiationWithManifest(result, proto.RemotePluginManifestUnsignedV1{Capabilities: []string{proto.CapabilityInboundFinalizeV1}})
	require.False(t, runner.SignedInboundFinalizeReady(), "signed image without current runtime capability must fail closed")
}

func TestValidateRemoteSyncNegotiationFailsClosed(t *testing.T) {
	valid := validDurableNegotiation(proto.RemoteSyncModeDeltaPreferred)
	require.Equal(t, proto.RemoteSyncModeLegacy, validateRemoteSyncNegotiation(valid, false, false, false, false).Mode, "unsigned capability cannot select durable mode")

	cases := []struct {
		name   string
		mutate func(*proto.RemoteNegotiateSyncV1Result)
	}{
		{"unknown mode", func(r *proto.RemoteNegotiateSyncV1Result) { r.Mode = "future" }},
		{"wrong protocol", func(r *proto.RemoteNegotiateSyncV1Result) { r.SelectedProtocol = 2 }},
		{"gate disabled", func(r *proto.RemoteNegotiateSyncV1Result) { r.FeatureGateEnabled = false }},
		{"server capability absent", func(r *proto.RemoteNegotiateSyncV1Result) { r.ServerCapabilities = nil }},
		{"inbound acknowledgement capability absent", func(r *proto.RemoteNegotiateSyncV1Result) {
			r.ServerCapabilities = []string{proto.CapabilityDurableDeltaSyncV1}
		}},
		{"stream absent", func(r *proto.RemoteNegotiateSyncV1Result) { r.StreamID = "" }},
		{"epoch absent", func(r *proto.RemoteNegotiateSyncV1Result) { r.StreamEpoch = "" }},
		{"device barrier absent", func(r *proto.RemoteNegotiateSyncV1Result) { r.AllActiveDevicesCapable = false }},
		{"checkpoint barrier absent", func(r *proto.RemoteNegotiateSyncV1Result) { r.CheckpointReady = false }},
		{"limits absent", func(r *proto.RemoteNegotiateSyncV1Result) { r.MaxPageBytes = 0 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			candidate := valid
			candidate.ServerCapabilities = append([]string(nil), valid.ServerCapabilities...)
			tc.mutate(&candidate)
			require.Equal(t, proto.RemoteSyncModeLegacy, validateRemoteSyncNegotiation(candidate, true, true, true, true).Mode)
		})
	}
}

func TestValidateRemoteSyncNegotiationAllowsShadowWithoutCutoverBarriers(t *testing.T) {
	candidate := validDurableNegotiation(proto.RemoteSyncModeShadow)
	candidate.AllActiveDevicesCapable = false
	candidate.CheckpointReady = false
	require.Equal(t, proto.RemoteSyncModeShadow, validateRemoteSyncNegotiation(candidate, true, false, false, false).Mode)

	multistream := validMultiStreamNegotiation(proto.RemoteSyncModeShadow)
	require.Equal(t, proto.RemoteSyncModeLegacy, validateRemoteSyncNegotiation(multistream, true, true, true, false).Mode,
		"unsigned descriptor authority must not enter even shadow state")
	require.Equal(t, proto.RemoteSyncModeShadow, validateRemoteSyncNegotiation(multistream, true, true, true, true).Mode)
}

func TestRemoteStreamDescriptorsRequireCanonicalCompleteAuthorizedSet(t *testing.T) {
	valid := validMultiStreamNegotiation(proto.RemoteSyncModeDurableRead)
	require.True(t, validRemoteStreamDescriptors(valid, true))

	tests := []struct {
		name   string
		mutate func(*proto.RemoteNegotiateSyncV1Result)
	}{
		{name: "account default is not first", mutate: func(candidate *proto.RemoteNegotiateSyncV1Result) {
			candidate.Streams[0], candidate.Streams[1] = candidate.Streams[1], candidate.Streams[0]
		}},
		{name: "namespace order differs", mutate: func(candidate *proto.RemoteNegotiateSyncV1Result) {
			candidate.Streams[1], candidate.Streams[2] = candidate.Streams[2], candidate.Streams[1]
		}},
		{name: "duplicate stream", mutate: func(candidate *proto.RemoteNegotiateSyncV1Result) {
			candidate.Streams[2].StreamID = candidate.Streams[1].StreamID
		}},
		{name: "duplicate namespace", mutate: func(candidate *proto.RemoteNegotiateSyncV1Result) {
			candidate.Streams[2].NamespaceID = candidate.Streams[1].NamespaceID
		}},
		{name: "singular compatibility mismatch", mutate: func(candidate *proto.RemoteNegotiateSyncV1Result) {
			candidate.Streams[0].StreamEpoch = "different"
		}},
		{name: "missing authenticated minimum", mutate: func(candidate *proto.RemoteNegotiateSyncV1Result) {
			candidate.Streams[1].MinAvailableCursor = ""
		}},
		{name: "missing authenticated tip", mutate: func(candidate *proto.RemoteNegotiateSyncV1Result) {
			candidate.Streams[1].TipCursor = ""
		}},
		{name: "checkpoint not ready", mutate: func(candidate *proto.RemoteNegotiateSyncV1Result) {
			candidate.Streams[1].CheckpointReady = false
		}},
		{name: "per-stream limit raises generation ceiling", mutate: func(candidate *proto.RemoteNegotiateSyncV1Result) {
			candidate.Streams[1].MaxPageEvents = candidate.MaxPageEvents + 1
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := valid
			candidate.Streams = append([]proto.RemoteStreamDescriptorV1(nil), valid.Streams...)
			test.mutate(&candidate)
			require.False(t, validRemoteStreamDescriptors(candidate, true))
		})
	}

	shadow := valid
	shadow.Streams = append([]proto.RemoteStreamDescriptorV1(nil), valid.Streams...)
	shadow.Streams[1].CheckpointReady = false
	shadow.Streams[1].MinAvailableCursor = ""
	shadow.Streams[1].TipCursor = ""
	require.True(t, validRemoteStreamDescriptors(shadow, false), "shadow may observe incomplete readiness but not activate it")

	tooMany := valid
	tooMany.Streams = make([]proto.RemoteStreamDescriptorV1, 129)
	require.False(t, validRemoteStreamDescriptors(tooMany, false))
}

func TestDurableCutoverRequiresSignedRuntimeFinalizeAndMultistreamCapabilities(t *testing.T) {
	candidate := validMultiStreamNegotiation(proto.RemoteSyncModeDurableRead)
	require.True(t, durableCutoverCapabilitiesReady(candidate, true, true, true, true, true))
	require.False(t, durableCutoverCapabilitiesReady(candidate, true, false, true, true, true), "unsigned finalize ABI must keep cutover dark")
	require.False(t, durableCutoverCapabilitiesReady(candidate, true, true, false, true, true), "unsigned multistream ABI must keep cutover dark")
	require.False(t, durableCutoverCapabilitiesReady(candidate, true, true, true, false, true), "unsigned replay-batch ABI must keep cutover dark")
	require.False(t, durableCutoverCapabilitiesReady(candidate, true, true, true, true, false), "unsigned staged-checkpoint ABI must keep cutover dark")
	candidate.ServerCapabilities = []string{proto.CapabilityDurableCursorResumeV1, proto.CapabilityDurableDeltaSyncV1, proto.CapabilityInboundAckV2}
	require.False(t, durableCutoverCapabilitiesReady(candidate, true, true, true, true, true), "server without finalize ABI must keep cutover dark")
}

func TestValidateRemoteSyncNegotiationEnforcesCompiledDeltaPreferredCeiling(t *testing.T) {
	require.Equal(t, proto.RemoteSyncModeDeltaPreferred, remoteSyncCompiledMaximumMode)
	for _, mode := range []string{proto.RemoteSyncModeDurableRead, proto.RemoteSyncModeDeltaPreferred} {
		t.Run(mode, func(t *testing.T) {
			candidate := validMultiStreamNegotiation(mode)
			got := validateRemoteSyncNegotiation(candidate, true, true, true, true, true, true)
			require.Equal(t, mode, got.Mode)
		})
	}
	required := validMultiStreamNegotiation(proto.RemoteSyncModeDeltaRequired)
	require.Equal(t, proto.RemoteSyncModeLegacy, validateRemoteSyncNegotiation(required, true, true, true, true, true, true).Mode,
		"delta-required must remain dark instead of being silently weakened")
}

func TestRemoteRunnerDurableReceiptPolicyOnlyForDeltaModes(t *testing.T) {
	runner := &RemoteRunner{}
	require.False(t, runner.DurableReceiptRequired())
	for _, mode := range []string{proto.RemoteSyncModeLegacy, proto.RemoteSyncModeShadow, proto.RemoteSyncModeDurableRead} {
		runner.setSyncNegotiation(validDurableNegotiation(mode))
		require.False(t, runner.DurableReceiptRequired(), mode)
	}
	for _, mode := range []string{proto.RemoteSyncModeDeltaPreferred, proto.RemoteSyncModeDeltaRequired} {
		runner.setSyncNegotiation(validDurableNegotiation(mode))
		require.True(t, runner.DurableReceiptRequired(), mode)
	}
}
