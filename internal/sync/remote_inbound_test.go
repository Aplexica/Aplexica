package syncd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/conflicts"
	"github.com/aplexica/aplexica/internal/identity"
	"github.com/aplexica/aplexica/internal/keys"
	"github.com/aplexica/aplexica/internal/plugin/proto"
	"github.com/stretchr/testify/require"
)

type fixedVerifiedRosterProvider struct {
	snapshot RosterSnapshot
}

func (p fixedVerifiedRosterProvider) Current(context.Context, string, string) (RosterSnapshot, error) {
	return p.snapshot, nil
}

type fixedV2IdentityProvider struct {
	identity keys.DeviceIdentity
}

func (p fixedV2IdentityProvider) Identity() (keys.DeviceIdentity, error) {
	return p.identity, nil
}

func v2RemoteEventForTest(t *testing.T, event acf.Event, kind acf.Kind, header EventHeaderV2, roster identity.VerifiedRoster, device keys.DeviceIdentity, barrier [32]byte) proto.RemoteEvent {
	t.Helper()
	wireBytes, err := SealEnvelopeV2(event, acf.ScopeGlobal, nil, header, roster, device)
	require.NoError(t, err)
	bodyDigest := sha256.Sum256(wireBytes)
	return proto.RemoteEvent{
		NamespaceID: header.Routing.NamespaceID, BranchID: header.Routing.BranchID,
		ArtifactID: event.ArtifactID, EventID: header.Routing.WireEventID, ParentHash: event.ParentHash,
		EventHash: event.Hash, BodyDigest: hex.EncodeToString(bodyDigest[:]), Kind: string(kind), Type: string(event.Type),
		Timestamp: event.Timestamp, Bytes: wireBytes, Sequence: header.Routing.Sequence,
		Origin: event.Provenance.DeviceID, SourceAgent: event.Provenance.SourceAgent, Lane: header.Routing.Lane,
		AccessGeneration: roster.Manifest.Manifest.AccessGeneration, AccessSetHash: roster.Manifest.Manifest.AccessSetHash,
		SecurityBarrierID: barrier, SecurityGeneration: 1, KeyMode: "recipient-wrap-v2",
	}
}

func newV2InboundOrchestratorForTest(t *testing.T, roster identity.VerifiedRoster, device keys.DeviceIdentity, barrier [32]byte, localDeviceID string) (*Orchestrator, *acf.Store) {
	t.Helper()
	root := realTempDir(t)
	watched := filepath.Join(root, "proj")
	require.NoError(t, os.MkdirAll(watched, 0o755))
	adapters, store, _ := buildAllThreeAdapters(t, root)
	o, err := NewOrchestrator(Config{
		Dir: watched, Adapters: adapters, Store: store, QuietPeriod: 50 * time.Millisecond, GuardWindow: time.Second,
		LocalDeviceID: localDeviceID, DeviceKeyProvider: fixedKeyProvider{priv: device.WrapPrivate}, RequireEnvelopeV2: true,
		VerifiedRosterProvider: fixedVerifiedRosterProvider{snapshot: RosterSnapshot{
			Roster: roster, BarrierID: barrier, KeyMode: "recipient-wrap-v2", CoordinatorGeneration: 1,
		}},
		V2IdentityProvider: fixedV2IdentityProvider{identity: device},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = o.Close() })
	return o, store
}

func TestDurableInboundAuthenticatedNoopEvidenceBindsNotRecipientAndRejectsDurableClear(t *testing.T) {
	roster, device := signedTestRoster(t)
	var barrier [32]byte
	barrier[0] = 1
	event := acf.Event{
		EventID: acf.NewID(), ArtifactID: acf.NewID(), Type: acf.EventTypeCreate, Timestamp: time.Now().UTC(),
		Provenance: acf.Provenance{DeviceID: "device-a", SourceAgent: "codex"},
		Payload:    json.RawMessage(`{"format":"markdown","content":"authenticated no-op"}`),
	}
	event.Hash, _ = acf.ComputeHash(event)
	header := NewEventHeaderV2(event, acf.KindMemory, "", event.EventID, LaneLive, 1, roster, barrier)
	wire := v2RemoteEventForTest(t, event, acf.KindMemory, header, roster, device, barrier)

	// device-b is deliberately absent from the signed roster's wrap set. The
	// immutable envelope is still signature-authenticated, so durable receive
	// may own it as an explicit no-op without fabricating a canonical append.
	o, store := newV2InboundOrchestratorForTest(t, roster, device, barrier, "device-b")
	require.Equal(t, []ImportOutcome{ImportSkipped}, o.ImportInboundCanonicalResults([]proto.RemoteEvent{wire}))
	evidence, err := o.CanonicalEvidenceForTerminalInbound(wire, ImportSkipped)
	require.NoError(t, err)
	require.Equal(t, proto.InboundFinalizeAuthenticatedNoop, evidence.FinalizeKind)
	require.Equal(t, proto.InboundFinalizeNoopNotRecipient, evidence.NoopReason)
	require.True(t, validLowerHexSHA256(evidence.AuthenticatedHeaderDigest))
	require.True(t, strings.HasPrefix(evidence.AuthenticatedSigner, "device-a:"))
	stored, err := store.ReadEvents(acf.KindMemory, event.ArtifactID)
	require.NoError(t, err)
	require.Empty(t, stored, "authenticated no-op must not create canonical state")

	// Durable non-materialising receive binds the cloud-visible EventHash to the
	// signed canonical header even when this device cannot decrypt the body.
	tamperedHash := wire
	tamperedHash.EventHash = strings.Repeat("0", sha256.Size*2)
	require.Equal(t, []ImportOutcome{ImportRejected}, o.ImportInboundCanonicalResults([]proto.RemoteEvent{tamperedHash}))
	_, err = o.CanonicalEvidenceForTerminalInbound(tamperedHash, ImportSkipped)
	require.Error(t, err)
	// The legacy/immediate-materialisation path intentionally preserves its
	// pre-cutover behavior; EventHash is not an authority there.
	require.Equal(t, []ImportOutcome{ImportSkipped}, o.ImportInboundResults([]proto.RemoteEvent{tamperedHash}))

	clearEvent := acf.Event{
		EventID: acf.NewID(), ArtifactID: acf.NewID(), Type: acf.EventTypeRedaction, Timestamp: time.Now().UTC(),
		Provenance: acf.Provenance{DeviceID: "device-a", SourceAgent: "codex"},
	}
	clearHeader := NewEventHeaderV2(clearEvent, acf.KindConversation, "", RetainedWireEventID(clearEvent.EventID, "device-a"), LaneRetained, 2, roster, barrier)
	clearHeader.Purpose = "retained-clear"
	clearHeader.Routing.Clear = true
	clearHeader.Canonical = CanonicalMetadataV2{}
	clearBytes, err := SealRetainedClearV2(clearHeader, roster, device)
	require.NoError(t, err)
	clearDigest := sha256.Sum256(clearBytes)
	clearWire := proto.RemoteEvent{
		BranchID: clearHeader.Routing.BranchID, ArtifactID: clearEvent.ArtifactID, EventID: clearHeader.Routing.WireEventID,
		ParentHash: clearEvent.ParentHash, BodyDigest: hex.EncodeToString(clearDigest[:]), Kind: string(acf.KindConversation),
		Type: string(clearEvent.Type), Timestamp: clearEvent.Timestamp, Bytes: clearBytes, Sequence: 2,
		Origin: "device-a", SourceAgent: "codex", Lane: LaneRetained, Clear: true,
		AccessGeneration: roster.Manifest.Manifest.AccessGeneration, AccessSetHash: roster.Manifest.Manifest.AccessSetHash,
		SecurityBarrierID: barrier, SecurityGeneration: 1, KeyMode: "recipient-wrap-v2",
	}
	clearOrchestrator, _ := newV2InboundOrchestratorForTest(t, roster, device, barrier, "device-a")
	require.Equal(t, []ImportOutcome{ImportRejected}, clearOrchestrator.ImportInboundCanonicalResults([]proto.RemoteEvent{tamperedHash}),
		"a local recipient must also reject non-conversation outer EventHash substitution")
	require.Equal(t, []ImportOutcome{ImportRejected}, clearOrchestrator.ImportInboundCanonicalResults([]proto.RemoteEvent{clearWire}),
		"retained-slot Clear is legacy MQTT transport state, never a durable cloud event")
	_, err = clearOrchestrator.CanonicalEvidenceForTerminalInbound(clearWire, ImportSkipped)
	require.Error(t, err, "durable Clear must never yield terminal finalize evidence")
	require.Equal(t, []ImportOutcome{ImportSkipped}, clearOrchestrator.ImportInboundResults([]proto.RemoteEvent{clearWire}),
		"the existing immediate/legacy MQTT retained-slot Clear behavior must remain intact")

	tamperedClear := clearWire
	tamperedClear.Origin = "device-b"
	require.Equal(t, []ImportOutcome{ImportRejected}, clearOrchestrator.ImportInboundCanonicalResults([]proto.RemoteEvent{tamperedClear}))

	retainedEvent := acf.Event{
		EventID: acf.NewID(), ArtifactID: acf.NewID(), Type: acf.EventTypeCreate, Timestamp: time.Now().UTC(),
		Provenance:  acf.Provenance{DeviceID: "device-a", SourceAgent: "codex"},
		Payload:     json.RawMessage(`{"format":"aplexica-conversation-v1","events":[]}`),
		AlignedHead: strings.Repeat("a", sha256.Size*2),
	}
	retainedEvent.AlignedEventID = retainedEvent.EventID
	retainedEvent.Hash, _ = acf.ComputeHash(retainedEvent)
	retainedHeader := NewEventHeaderV2(retainedEvent, acf.KindConversation, "", RetainedWireEventID(retainedEvent.EventID, "device-a"), LaneRetained, 3, roster, barrier)
	retainedWire := v2RemoteEventForTest(t, retainedEvent, acf.KindConversation, retainedHeader, roster, device, barrier)
	legacyRetained := retainedWire
	require.NoError(t, validateAuthenticatedInboundOuter(legacyRetained, retainedHeader, true),
		"legacy MQTT retained records may omit the additive outer alignment")
	retainedWire.CheckpointCoverage = 2
	retainedWire.CheckpointGeneration = strings.Repeat("b", sha256.Size*2)
	retainedWire.CheckpointAlignmentHash = retainedHeader.Canonical.AlignedHead
	require.NoError(t, validateAuthenticatedInboundOuter(retainedWire, retainedHeader, true))
	require.Empty(t, retainedWire.ParentHash, "a checkpoint may have no canonical predecessor")

	tamperedAlignment := retainedWire
	tamperedAlignment.CheckpointAlignmentHash = strings.Repeat("c", sha256.Size*2)
	require.Equal(t, []ImportOutcome{ImportRejected}, clearOrchestrator.ImportInboundCanonicalResults([]proto.RemoteEvent{tamperedAlignment}),
		"outer checkpoint alignment must equal the signed Canonical.AlignedHead")
	missingAlignment := retainedWire
	missingAlignment.CheckpointAlignmentHash = ""
	require.Equal(t, []ImportOutcome{ImportRejected}, clearOrchestrator.ImportInboundCanonicalResults([]proto.RemoteEvent{missingAlignment}),
		"durable checkpoint metadata requires an explicit authenticated alignment")

	liveWithAlignment := wire
	liveWithAlignment.CheckpointAlignmentHash = retainedHeader.Canonical.AlignedHead
	require.Equal(t, []ImportOutcome{ImportRejected}, o.ImportInboundCanonicalResults([]proto.RemoteEvent{liveWithAlignment}),
		"live/tombstone events must not carry checkpoint alignment")

	retainedWire.EventHash = strings.Repeat("1", sha256.Size*2)
	require.Equal(t, []ImportOutcome{ImportRejected}, clearOrchestrator.ImportInboundCanonicalResults([]proto.RemoteEvent{retainedWire}),
		"retained conversation outer EventHash substitution must fail before reconcile")
}

// newStoreOrchWithConflicts is newStoreOrch plus a conflict store, so inbound
// divergence detection (P1-5) is active.
func newStoreOrchWithConflicts(t *testing.T, pub RemoteEventPublisher, local testDevice, cs *conflicts.Store) (*Orchestrator, *acf.Store) {
	t.Helper()
	root := realTempDir(t)
	watched := filepath.Join(root, "proj")
	require.NoError(t, os.MkdirAll(watched, 0o755))
	adapters, store, _ := buildAllThreeAdapters(t, root)

	o, err := NewOrchestrator(Config{
		Dir:                  watched,
		Adapters:             adapters,
		Store:                store,
		QuietPeriod:          50 * time.Millisecond,
		GuardWindow:          time.Second,
		RemoteEventPublisher: pub,
		LocalDeviceID:        local.id,
		RecipientResolver:    staticResolver{recipients: []Recipient{{DeviceID: local.id, PubKey: local.pub}}},
		DeviceKeyProvider:    fixedKeyProvider{priv: local.priv},
		ConflictStore:        cs,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = o.Close() })
	return o, store
}

// sealedInbound builds an inbound wire event sealed for local with an explicit
// ParentHash / timestamp / content so chain-gap vs genuine-divergence can be
// driven precisely.
func sealedInbound(t *testing.T, local testDevice, kind acf.Kind, artID, parentHash, content, origin string, ts time.Time) proto.RemoteEvent {
	t.Helper()
	payload, _ := json.Marshal(acf.MemoryPayload{Format: "markdown", Content: content})
	ev := acf.Event{
		EventID:    acf.NewID(),
		ArtifactID: artID,
		Type:       acf.EventTypeUpdate,
		Timestamp:  ts,
		Provenance: acf.Provenance{DeviceID: origin, SourceAgent: "codex"},
		Payload:    payload,
		ParentHash: parentHash,
	}
	sealed, err := sealEnvelope(ev, acf.ScopeGlobal, nil, []recipient{{deviceID: local.id, pub: local.pub}})
	require.NoError(t, err)
	return proto.RemoteEvent{
		ArtifactID: artID,
		EventID:    ev.EventID,
		ParentHash: parentHash,
		Kind:       string(kind),
		Type:       string(acf.EventTypeUpdate),
		Timestamp:  ts,
		Bytes:      sealed,
		Origin:     origin,
	}
}

func sealedConversationInbound(t *testing.T, local testDevice, artID, parentHash, origin string, ts time.Time, format string, events []acf.ConversationEvent) proto.RemoteEvent {
	t.Helper()
	ev := acf.Event{
		EventID:    acf.NewID(),
		ArtifactID: artID,
		Type:       acf.EventTypeUpdate,
		Timestamp:  ts,
		Provenance: acf.Provenance{DeviceID: origin, SourceAgent: "codex"},
		Payload:    encodeSyncTestConversationPayload(t, format, events),
		ParentHash: parentHash,
	}
	sealed, err := sealEnvelope(ev, acf.ScopeGlobal, nil, []recipient{{deviceID: local.id, pub: local.pub}})
	require.NoError(t, err)
	return proto.RemoteEvent{
		ArtifactID: artID,
		EventID:    ev.EventID,
		ParentHash: parentHash,
		Kind:       string(acf.KindConversation),
		Type:       string(acf.EventTypeUpdate),
		Timestamp:  ts,
		Bytes:      sealed,
		Origin:     origin,
	}
}

// TestImportInbound_ChainGapStillSelfHeals verifies the deliberate 1ea1f5d
// self-heal is preserved: an inbound event whose ParentHash references history
// this device never received (a genuine chain gap) is adopted as a fresh
// baseline, and NO conflict is recorded.
func TestImportInbound_ChainGapStillSelfHeals(t *testing.T) {
	local := newTestDevice(t, "this-device")
	cs := &conflicts.Store{Root: filepath.Join(realTempDir(t), "conflicts")}
	require.NoError(t, cs.Init())
	pub := &stubRemotePublisher{}
	o, store := newStoreOrchWithConflicts(t, pub, local, cs)

	id, _ := seedArtifact(t, store, acf.KindMemory, "peer")

	wire := sealedInbound(t, local, acf.KindMemory, id, "unknown-parent-hash-not-in-log", "full state from peer", "peer", time.Now().UTC().Add(time.Hour))
	o.ImportInbound([]proto.RemoteEvent{wire})

	_, err := cs.Get(id)
	require.Error(t, err, "chain-gap self-heal must NOT record a conflict")

	stored, err := store.ReadEvents(acf.KindMemory, id)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(stored), 1)
	require.Contains(t, string(stored[len(stored)-1].Payload), "full state from peer",
		"inbound full-state snapshot must be adopted")
}

func TestImportInbound_ConversationDeltaMissingParentIsRetryable(t *testing.T) {
	local := newTestDevice(t, "this-device")
	pub := &stubRemotePublisher{}
	o, store := newStoreOrch(t, pub, local)

	artID := acf.NewID()
	wire := sealedConversationInbound(t, local, artID, "missing-parent", "peer", time.Now().UTC(),
		acf.ConversationDeltaFormatV1,
		[]acf.ConversationEvent{syncTestConversationTurn("assistant", "The capital of China is **Beijing**.")})

	outcomes := o.ImportInboundResults([]proto.RemoteEvent{wire})

	require.Equal(t, []ImportOutcome{ImportRetryable}, outcomes,
		"a bare conversation delta must wait for its base instead of becoming a broken genesis")
	stored, err := store.ReadEvents(acf.KindConversation, artID)
	require.NoError(t, err)
	require.Len(t, stored, 0, "missing-parent conversation delta must not be appended")
}

func TestImportInbound_MaterializedConversationUpdateSelfHealsChainGap(t *testing.T) {
	local := newTestDevice(t, "this-device")
	pub := &stubRemotePublisher{}
	o, store := newStoreOrch(t, pub, local)

	artID := acf.NewID()
	wire := sealedConversationInbound(t, local, artID, "missing-parent", "peer", time.Now().UTC(),
		acf.ConversationFormatV1,
		[]acf.ConversationEvent{
			syncTestConversationTurn("user", "What is the Capital of China?"),
			syncTestConversationTurn("assistant", "The capital of China is **Beijing**."),
		})

	outcomes := o.ImportInboundResults([]proto.RemoteEvent{wire})

	require.Equal(t, []ImportOutcome{ImportApplied}, outcomes)
	stored, err := store.ReadEvents(acf.KindConversation, artID)
	require.NoError(t, err)
	require.Len(t, stored, 1)
	p, ok, err := acf.MaterializedConversationPayload(stored)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, []acf.TextTurn{
		{Role: "user", Text: "What is the Capital of China?"},
		{Role: "assistant", Text: "The capital of China is **Beijing**."},
	}, acf.ExtractTextTurns(p.Events))
}

// TestImportInbound_GenuineDivergenceRecordsConflict is the P1-5 fix: two real
// edits off a shared ancestor must NOT delete the local chain — the daemon
// records a conflict and keeps both (BRD-04 §4.2/§5.7).
func TestImportInbound_GenuineDivergenceRecordsConflict(t *testing.T) {
	local := newTestDevice(t, "this-device")
	cs := &conflicts.Store{Root: filepath.Join(realTempDir(t), "conflicts")}
	require.NoError(t, cs.Init())
	pub := &stubRemotePublisher{}
	o, store := newStoreOrchWithConflicts(t, pub, local, cs)

	id, e0 := seedArtifact(t, store, acf.KindMemory, "this-device")

	// Local edit off the shared ancestor E0 -> local head moves to E1a.
	p1, _ := json.Marshal(acf.MemoryPayload{Format: "markdown", Content: "local unsynced edit"})
	e1a := acf.Event{
		EventID:    acf.NewID(),
		ArtifactID: id,
		Type:       acf.EventTypeUpdate,
		Timestamp:  e0.Timestamp.Add(time.Second),
		Provenance: acf.Provenance{DeviceID: "this-device", SourceAgent: "claude-code"},
		Payload:    p1,
		ParentHash: e0.Hash,
	}
	require.NoError(t, store.AppendEvent(acf.KindMemory, e1a))

	// Inbound edit off the SAME ancestor E0 (present locally), newer timestamp:
	// genuine divergence, NOT a chain gap.
	wire := sealedInbound(t, local, acf.KindMemory, id, e0.Hash, "remote concurrent edit", "peer", e0.Timestamp.Add(2*time.Second))
	o.ImportInbound([]proto.RemoteEvent{wire})

	stored, err := store.ReadEvents(acf.KindMemory, id)
	require.NoError(t, err)
	foundLocal := false
	for _, e := range stored {
		if e.EventID == e1a.EventID {
			foundLocal = true
		}
	}
	require.True(t, foundLocal, "the unsynced local edit must NOT be deleted on genuine divergence")

	c, err := cs.Get(id)
	require.NoError(t, err, "genuine divergence must record a conflict")
	require.Len(t, c.Heads, 2, "conflict must capture both heads")
}

// TestRecordInboundConflict_PreservesFullRemotePayload is the B3 fix: on a
// genuine remote-divergence inbound head mismatch, the inbound remote event is
// never appended to any local branch, so the only persisted copy of the remote
// head content used to be conflicts.Head.PayloadPreview (truncated to ~200
// chars). Resolution and side-by-side analysis reconstruct the winner payload by
// EventID lookup in the LOCAL store, so picking the remote head failed. The fix
// stores the full remote payload in the local-only conflict sidecar
// (Head.FullPayload) WITHOUT appending the remote event to the canonical log.
func TestRecordInboundConflict_PreservesFullRemotePayload(t *testing.T) {
	local := newTestDevice(t, "this-device")
	cs := &conflicts.Store{Root: filepath.Join(realTempDir(t), "conflicts")}
	require.NoError(t, cs.Init())
	pub := &stubRemotePublisher{}
	o, store := newStoreOrchWithConflicts(t, pub, local, cs)

	id, e0 := seedArtifact(t, store, acf.KindMemory, "this-device")

	// Inbound remote edit off the SAME ancestor E0 (present locally) whose
	// payload is LONGER than the 200-char preview cap and diverges from local.
	longContent := strings.Repeat("remote-divergent-content ", 40) // ~1000 chars
	remotePayload, err := json.Marshal(acf.MemoryPayload{Format: "markdown", Content: longContent})
	require.NoError(t, err)
	require.Greater(t, len(remotePayload), 200, "test payload must exceed the preview cap")
	ev := acf.Event{
		EventID:    acf.NewID(),
		ArtifactID: id,
		Type:       acf.EventTypeUpdate,
		Timestamp:  e0.Timestamp.Add(2 * time.Second),
		Provenance: acf.Provenance{DeviceID: "peer", SourceAgent: "codex"},
		Payload:    remotePayload,
		ParentHash: e0.Hash,
	}

	before, err := store.ReadEvents(acf.KindMemory, id)
	require.NoError(t, err)

	require.NoError(t, o.recordInboundConflictWithDurability(acf.KindMemory, ev, true),
		"the fsynced full-payload sidecar is durable ownership for cloud ACK")

	// The full remote payload must be preserved byte-for-byte in the conflict
	// sidecar's inbound head.
	c, err := cs.Get(id)
	require.NoError(t, err)
	require.Len(t, c.Heads, 2)
	var inbound *conflicts.Head
	for i := range c.Heads {
		if c.Heads[i].EventID == ev.EventID {
			inbound = &c.Heads[i]
		}
	}
	require.NotNil(t, inbound, "the inbound remote head must be recorded")
	// JSONEq, not byte-for-byte: the conflict store persists via MarshalIndent,
	// which re-indents the embedded RawMessage on disk. The invariant that
	// matters is that the FULL content survives (not truncated to the 200-char
	// preview), which JSONEq proves; the preview alone could never round-trip the
	// whole payload.
	require.JSONEq(t, string(ev.Payload), string(inbound.FullPayload),
		"the full remote payload must be preserved in the conflict sidecar")
	var got acf.MemoryPayload
	require.NoError(t, json.Unmarshal(inbound.FullPayload, &got))
	require.Equal(t, longContent, got.Content,
		"the full (untruncated) remote content must survive in FullPayload")
	require.Greater(t, len(inbound.FullPayload), len(inbound.PayloadPreview),
		"FullPayload must carry more than the truncated preview")

	// The remote event must NOT be appended to the canonical log: the hash chain
	// is untouched and the event count is unchanged.
	after, err := store.ReadEvents(acf.KindMemory, id)
	require.NoError(t, err)
	require.Len(t, after, len(before), "remote inbound event must NOT be appended to the canonical log")
	require.NoError(t, acf.VerifyChain(after), "canonical hash chain must remain intact")
	for _, e := range after {
		require.NotEqual(t, ev.EventID, e.EventID, "the remote event must not appear in the local log")
	}
}

func TestDurableInboundConflictNeverOverwritesEarlierCloudAckedSibling(t *testing.T) {
	local := newTestDevice(t, "this-device")
	conflictRoot := filepath.Join(realTempDir(t), "conflicts")
	cs := &conflicts.Store{Root: conflictRoot}
	require.NoError(t, cs.Init())
	o, store := newStoreOrchWithConflicts(t, &stubRemotePublisher{}, local, cs)
	id, ancestor := seedArtifact(t, store, acf.KindMemory, "this-device")

	localPayload, err := json.Marshal(acf.MemoryPayload{Format: "markdown", Content: "local sibling"})
	require.NoError(t, err)
	localSibling := acf.Event{
		EventID: acf.NewID(), ArtifactID: id, Type: acf.EventTypeUpdate,
		Timestamp: ancestor.Timestamp.Add(time.Second), ParentHash: ancestor.Hash,
		Provenance: acf.Provenance{DeviceID: local.id, SourceAgent: "claude-code"}, Payload: localPayload,
	}
	require.NoError(t, store.AppendEvent(acf.KindMemory, localSibling))

	makeRemote := func(content string, offset time.Duration) acf.Event {
		payload, marshalErr := json.Marshal(acf.MemoryPayload{Format: "markdown", Content: content})
		require.NoError(t, marshalErr)
		event := acf.Event{
			EventID: acf.NewID(), ArtifactID: id, Type: acf.EventTypeUpdate,
			Timestamp: ancestor.Timestamp.Add(offset), ParentHash: ancestor.Hash,
			Provenance: acf.Provenance{DeviceID: "peer-device", SourceAgent: "codex"}, Payload: payload,
		}
		event.Hash, marshalErr = acf.ComputeHash(event)
		require.NoError(t, marshalErr)
		return event
	}
	first := makeRemote("first remote sibling survives", 2*time.Second)
	second := makeRemote("second remote sibling must wait", 3*time.Second)
	require.NoError(t, o.recordInboundConflictWithDurability(acf.KindMemory, first, true))

	// Model daemon restart after the first sidecar reached disk and its cloud
	// cursor was ACKed. Exact redelivery remains idempotent through a fresh
	// ConflictStore handle, while a distinct successor cannot replace it.
	o.cfg.ConflictStore = &conflicts.Store{Root: conflictRoot}
	require.NoError(t, o.recordInboundConflictWithDurability(acf.KindMemory, first, true))
	require.ErrorIs(t, o.recordInboundConflictWithDurability(acf.KindMemory, second, true), ErrInboundUnresolvedConflict)

	persisted, err := o.cfg.ConflictStore.Get(id)
	require.NoError(t, err)
	var firstPayload, secondPayload json.RawMessage
	for _, head := range persisted.Heads {
		switch head.EventID {
		case first.EventID:
			firstPayload = head.FullPayload
		case second.EventID:
			secondPayload = head.FullPayload
		}
	}
	require.NotEmpty(t, firstPayload, "the earlier cloud-ACKed sibling must remain recoverable")
	require.JSONEq(t, string(first.Payload), string(firstPayload))
	require.Empty(t, secondPayload, "the blocked successor must not overwrite or merge implicitly")

	// Legacy MQTT conflict behavior is intentionally unchanged: it remains the
	// historical best-effort last-observed sidecar path.
	require.NoError(t, o.recordInboundConflictWithDurability(acf.KindMemory, second, false))
	legacy, err := o.cfg.ConflictStore.Get(id)
	require.NoError(t, err)
	foundSecond := false
	for _, head := range legacy.Heads {
		foundSecond = foundSecond || head.EventID == second.EventID
	}
	require.True(t, foundSecond)
}

// TestImportInbound_RejectsEmptyEventID is the P2-1 fix: an inbound event whose
// EventID is empty (a malformed/non-conformant plugin or relay redelivery)
// bypassed the EventID dedupe guard and was appended — and on redelivery churned
// the destructive rebase path. EventID is mandatory on the wire, so it must be
// rejected outright (best-effort: the stream continues).
func TestImportInbound_RejectsEmptyEventID(t *testing.T) {
	local := newTestDevice(t, "this-device")
	pub := &stubRemotePublisher{}
	o, store := newStoreOrch(t, pub, local)

	artID := acf.NewID()
	payload, _ := json.Marshal(acf.MemoryPayload{Format: "markdown", Content: "x"})
	ev := acf.Event{
		EventID:    "", // malformed: no event id in the sealed body
		ArtifactID: artID,
		Type:       acf.EventTypeCreate,
		Timestamp:  time.Now().UTC(),
		Provenance: acf.Provenance{DeviceID: "peer"},
		Payload:    payload,
	}
	sealed, err := sealEnvelope(ev, acf.ScopeGlobal, nil, []recipient{{deviceID: local.id, pub: local.pub}})
	require.NoError(t, err)
	wire := proto.RemoteEvent{
		ArtifactID: artID,
		EventID:    "", // mandatory wire field missing too
		Kind:       string(acf.KindMemory),
		Type:       string(acf.EventTypeCreate),
		Timestamp:  ev.Timestamp,
		Bytes:      sealed,
		Origin:     "peer",
	}

	o.ImportInbound([]proto.RemoteEvent{wire})
	o.ImportInbound([]proto.RemoteEvent{wire}) // redelivery must not churn

	stored, err := store.ReadEvents(acf.KindMemory, artID)
	require.NoError(t, err)
	require.Len(t, stored, 0, "empty-EventID event must be rejected, not appended")
}

// TestImportInboundResults_OutcomeClassification is the B2 fix: ImportInboundResults
// must return one outcome per event IN ORDER so the remote-sync driver can advance
// its resume cursor only through durably-consumed / intentionally-dropped events
// and STOP at a transient failure (a Store/decrypt error). A corrupt envelope is a
// terminal decode failure (ImportRejected) — an immutable corrupt envelope can
// never recover and must not wedge the branch cursor ahead of later valid events.
// A good sibling is ImportApplied, an already-seen EventID is ImportDeduped, and
// an envelope sealed for another device is ImportSkipped.
func TestImportInboundResults_OutcomeClassification(t *testing.T) {
	local := newTestDevice(t, "this-device")
	other := newTestDevice(t, "other-device")
	pub := &stubRemotePublisher{}
	o, _ := newStoreOrch(t, pub, local)

	ts := time.Now().UTC()

	// (1) A good event we import first, so a re-import of it in the batch dedupes.
	dupArt := acf.NewID()
	dupWire := sealedInbound(t, local, acf.KindMemory, dupArt, "", "already imported", "peer", ts)
	pre := o.ImportInboundResults([]proto.RemoteEvent{dupWire})
	require.Len(t, pre, 1)
	require.Equal(t, ImportApplied, pre[0], "first import of the dup event must apply")

	// (2) A good sibling for a fresh artifact -> ImportApplied.
	goodWire := sealedInbound(t, local, acf.KindMemory, acf.NewID(), "", "fresh good state", "peer", ts.Add(time.Second))

	// (3) An envelope sealed for ANOTHER device -> not-a-recipient -> ImportSkipped.
	skipWire := sealedInbound(t, other, acf.KindMemory, acf.NewID(), "", "for other device", "peer", ts.Add(2*time.Second))

	// (4) A corrupt envelope: once the local key loaded, a decode/auth failure
	// (NOT not-a-recipient) is immutable -> ImportRejected.
	corruptWire := proto.RemoteEvent{
		ArtifactID: acf.NewID(),
		EventID:    acf.NewID(),
		Kind:       string(acf.KindMemory),
		Type:       string(acf.EventTypeUpdate),
		Timestamp:  ts.Add(3 * time.Second),
		Bytes:      []byte("not a valid sealed envelope"),
		Origin:     "peer",
	}

	outcomes := o.ImportInboundResults([]proto.RemoteEvent{goodWire, dupWire, skipWire, corruptWire})
	require.Len(t, outcomes, 4)
	require.Equal(t, ImportApplied, outcomes[0], "good sibling must apply")
	require.Equal(t, ImportDeduped, outcomes[1], "already-seen EventID must dedupe")
	require.Equal(t, ImportSkipped, outcomes[2], "envelope sealed for another device must skip")
	require.Equal(t, ImportRejected, outcomes[3], "corrupt-envelope decrypt failure must be terminal")
}

// TestImportInboundResults_V2CorruptEnvelopeIsTerminal protects branch
// availability after the mandatory v2 cutover. Once current roster/key state
// is available, an invalid signature or corrupt ciphertext is immutable and
// must be quarantined (ImportRejected), not retried forever ahead of later
// valid events.
func TestImportInboundResults_V2CorruptEnvelopeIsTerminal(t *testing.T) {
	roster, device := signedTestRoster(t)
	root := realTempDir(t)
	watched := filepath.Join(root, "proj")
	require.NoError(t, os.MkdirAll(watched, 0o755))
	adapters, store, _ := buildAllThreeAdapters(t, root)

	var barrier [32]byte
	barrier[0] = 1
	snapshot := RosterSnapshot{
		Roster:                roster,
		BarrierID:             barrier,
		KeyMode:               "recipient-wrap-v2",
		CoordinatorGeneration: 1,
	}
	o, err := NewOrchestrator(Config{
		Dir:                    watched,
		Adapters:               adapters,
		Store:                  store,
		QuietPeriod:            50 * time.Millisecond,
		GuardWindow:            time.Second,
		LocalDeviceID:          "device-a",
		DeviceKeyProvider:      fixedKeyProvider{priv: device.WrapPrivate},
		RequireEnvelopeV2:      true,
		VerifiedRosterProvider: fixedVerifiedRosterProvider{snapshot: snapshot},
		V2IdentityProvider:     fixedV2IdentityProvider{identity: device},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = o.Close() })

	payload, err := json.Marshal(acf.MemoryPayload{Format: "markdown", Content: "authenticated body"})
	require.NoError(t, err)
	ev := acf.Event{
		EventID:    acf.NewID(),
		ArtifactID: acf.NewID(),
		Type:       acf.EventTypeCreate,
		Timestamp:  time.Now().UTC(),
		Provenance: acf.Provenance{DeviceID: "device-a", SourceAgent: "claude-code"},
		Payload:    payload,
	}
	ev.Hash, err = acf.ComputeHash(ev)
	require.NoError(t, err)
	header := NewEventHeaderV2(ev, acf.KindMemory, "", ev.EventID, LaneLive, 1, roster, barrier)
	wireBytes, err := SealEnvelopeV2(ev, acf.ScopeGlobal, nil, header, roster, device)
	require.NoError(t, err)

	var envelope EventEnvelopeV2
	require.NoError(t, json.Unmarshal(wireBytes, &envelope))
	require.NotEmpty(t, envelope.BodyCiphertext)
	envelope.BodyCiphertext[0] ^= 0xff
	wireBytes, err = json.Marshal(envelope)
	require.NoError(t, err)

	wire := proto.RemoteEvent{
		BranchID:           acf.MainBranch,
		ArtifactID:         ev.ArtifactID,
		EventID:            ev.EventID,
		Kind:               string(acf.KindMemory),
		Type:               string(ev.Type),
		Timestamp:          ev.Timestamp,
		Bytes:              wireBytes,
		Sequence:           1,
		Origin:             "device-a",
		SourceAgent:        "claude-code",
		Lane:               LaneLive,
		AccessGeneration:   roster.Manifest.Manifest.AccessGeneration,
		AccessSetHash:      roster.Manifest.Manifest.AccessSetHash,
		SecurityBarrierID:  barrier,
		SecurityGeneration: 1,
		KeyMode:            "recipient-wrap-v2",
	}

	outcomes := o.ImportInboundResults([]proto.RemoteEvent{wire})
	require.Equal(t, []ImportOutcome{ImportRejected}, outcomes)
	stored, readErr := store.ReadEvents(acf.KindMemory, ev.ArtifactID)
	require.NoError(t, readErr)
	require.Empty(t, stored, "terminally rejected input must not mutate canonical state")
}

// TestImportInboundResults_V2LegacyRetainedTransportIDDedupes protects the
// upgrade path for retained records written before Lane was populated on the
// wire. Their authenticated transport ID already used the origin-scoped
// retained form, while the sealed canonical event kept its unsuffixed ID.
// AdoptBaseline stores the transport ID physically. A redelivery must compare
// that authenticated outer ID too; otherwise an event already on disk can be
// retried forever ahead of every newer relay record.
func TestImportInboundResults_V2LegacyRetainedTransportIDDedupes(t *testing.T) {
	roster, device := signedTestRoster(t)
	root := realTempDir(t)
	watched := filepath.Join(root, "proj")
	require.NoError(t, os.MkdirAll(watched, 0o755))
	adapters, store, _ := buildAllThreeAdapters(t, root)

	artifactID, localHead := seedArtifact(t, store, acf.KindConversation, "device-a")
	logicalEventID := acf.NewID()
	wireEventID := RetainedWireEventID(logicalEventID, "device-a")
	storedBaseline := acf.Event{
		EventID:        wireEventID,
		ArtifactID:     artifactID,
		Type:           acf.EventTypeBaseline,
		Timestamp:      time.Now().UTC(),
		Provenance:     acf.Provenance{DeviceID: "device-a", SourceAgent: "claude-code"},
		AlignedHead:    localHead.Hash,
		AlignedEventID: logicalEventID,
		Payload: encodeSyncTestConversationPayload(t, acf.ConversationFormatV1,
			[]acf.ConversationEvent{syncTestConversationTurn("assistant", "already stored")}),
		ParentHash: localHead.Hash,
	}
	require.NoError(t, store.AppendEvent(acf.KindConversation, storedBaseline))

	var barrier [32]byte
	barrier[0] = 1
	snapshot := RosterSnapshot{
		Roster:                roster,
		BarrierID:             barrier,
		KeyMode:               "recipient-wrap-v2",
		CoordinatorGeneration: 1,
	}
	o, err := NewOrchestrator(Config{
		Dir:                    watched,
		Adapters:               adapters,
		Store:                  store,
		QuietPeriod:            50 * time.Millisecond,
		GuardWindow:            time.Second,
		LocalDeviceID:          "device-a",
		DeviceKeyProvider:      fixedKeyProvider{priv: device.WrapPrivate},
		RequireEnvelopeV2:      true,
		VerifiedRosterProvider: fixedVerifiedRosterProvider{snapshot: snapshot},
		V2IdentityProvider:     fixedV2IdentityProvider{identity: device},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = o.Close() })

	// This body deliberately cannot extend the local chain. Without outer-ID
	// deduplication it reaches the legacy lane-empty reconcile path and becomes
	// retryable even though the corresponding retained baseline is already
	// durable under wireEventID.
	ev := acf.Event{
		EventID:    logicalEventID,
		ArtifactID: artifactID,
		Type:       acf.EventTypeUpdate,
		Timestamp:  storedBaseline.Timestamp,
		Provenance: acf.Provenance{DeviceID: "device-a", SourceAgent: "claude-code"},
		Payload: encodeSyncTestConversationPayload(t, acf.ConversationDeltaFormatV1,
			[]acf.ConversationEvent{syncTestConversationTurn("assistant", "legacy retained redelivery")}),
		ParentHash: "missing-legacy-parent",
	}
	ev.Hash, err = acf.ComputeHash(ev)
	require.NoError(t, err)
	header := NewEventHeaderV2(ev, acf.KindConversation, "", wireEventID, "", 1, roster, barrier)
	wireBytes, err := SealEnvelopeV2(ev, acf.ScopeGlobal, nil, header, roster, device)
	require.NoError(t, err)
	wire := proto.RemoteEvent{
		BranchID:           acf.MainBranch,
		ArtifactID:         artifactID,
		EventID:            wireEventID,
		ParentHash:         ev.ParentHash,
		Kind:               string(acf.KindConversation),
		Type:               string(ev.Type),
		Timestamp:          ev.Timestamp,
		Bytes:              wireBytes,
		Sequence:           1,
		Origin:             "device-a",
		SourceAgent:        "claude-code",
		AccessGeneration:   roster.Manifest.Manifest.AccessGeneration,
		AccessSetHash:      roster.Manifest.Manifest.AccessSetHash,
		SecurityBarrierID:  barrier,
		SecurityGeneration: 1,
		KeyMode:            "recipient-wrap-v2",
	}

	before, err := store.ReadEvents(acf.KindConversation, artifactID)
	require.NoError(t, err)
	outcomes := o.ImportInboundResults([]proto.RemoteEvent{wire})
	require.Equal(t, []ImportOutcome{ImportDeduped}, outcomes)
	after, err := store.ReadEvents(acf.KindConversation, artifactID)
	require.NoError(t, err)
	require.Equal(t, before, after, "dedupe must not mutate canonical state")
}
