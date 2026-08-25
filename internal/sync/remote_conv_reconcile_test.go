package syncd

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/plugin/proto"
	"github.com/stretchr/testify/require"
)

func convPayload(t *testing.T, events ...acf.ConversationEvent) json.RawMessage {
	t.Helper()
	return convPayloadState(t, events, nil)
}

func convPayloadState(t *testing.T, events []acf.ConversationEvent, attachments []acf.Attachment) json.RawMessage {
	t.Helper()
	p, err := json.Marshal(acf.ConversationPayload{
		Format: acf.ConversationFormatV1, Events: events, Attachments: attachments,
	})
	require.NoError(t, err)
	return p
}

// seedConversation writes an artifact + one full-payload create event and
// returns the artifact id and the head event.
func seedConversation(t *testing.T, store *acf.Store, deviceID string, events ...acf.ConversationEvent) (string, acf.Event) {
	t.Helper()
	artID := acf.NewID()
	now := time.Now().UTC()
	require.NoError(t, store.WriteArtifact(acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion, ArtifactID: artID,
		Kind: acf.KindConversation, Scope: acf.ScopeGlobal,
		Name: "conv", CreatedAt: now, UpdatedAt: now,
	}))
	ev := acf.Event{
		EventID: acf.NewID(), ArtifactID: artID, Type: acf.EventTypeCreate,
		Timestamp:  now,
		Provenance: acf.Provenance{DeviceID: deviceID, SourceAgent: "claude-code"},
		Payload:    convPayload(t, events...),
	}
	require.NoError(t, store.AppendEvent(acf.KindConversation, ev))
	head, ok, err := store.LastEvent(acf.KindConversation, artID)
	require.NoError(t, err)
	require.True(t, ok)
	return artID, head
}

// seedConversationFixture fixes every hash-bearing field for ordering tests.
func seedConversationFixture(t *testing.T, store *acf.Store, deviceID, artID, eventID string, now time.Time, events ...acf.ConversationEvent) acf.Event {
	t.Helper()
	require.NoError(t, store.WriteArtifact(acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion, ArtifactID: artID,
		Kind: acf.KindConversation, Scope: acf.ScopeGlobal,
		Name: "conv", CreatedAt: now, UpdatedAt: now,
	}))
	require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
		EventID: eventID, ArtifactID: artID, Type: acf.EventTypeCreate,
		Timestamp:  now,
		Provenance: acf.Provenance{DeviceID: deviceID, SourceAgent: "claude-code"},
		Payload:    convPayload(t, events...),
	}))
	head, ok, err := store.LastEvent(acf.KindConversation, artID)
	require.NoError(t, err)
	require.True(t, ok)
	return head
}

func localTurns(t *testing.T, store *acf.Store, artID string) []acf.TextTurn {
	t.Helper()
	head, ok, err := materializedConversationHead(store, artID)
	require.NoError(t, err)
	require.True(t, ok)
	p, err := acf.DecodeConversationPayload(head)
	require.NoError(t, err)
	return acf.ExtractTextTurns(p.Events)
}

func wireConversation(t *testing.T, local testDevice, artID, origin string, ts time.Time, events ...acf.ConversationEvent) proto.RemoteEvent {
	t.Helper()
	return wireConversationPayloadWithTags(t, local, artID, origin, ts, convPayload(t, events...), nil)
}

func wireConversationPayload(t *testing.T, local testDevice, artID, origin string, ts time.Time, payload json.RawMessage) proto.RemoteEvent {
	t.Helper()
	return wireConversationPayloadWithTags(t, local, artID, origin, ts, payload, nil)
}

func wireConversationPayloadWithTags(t *testing.T, local testDevice, artID, origin string, ts time.Time, payload json.RawMessage, tags []string) proto.RemoteEvent {
	t.Helper()
	ev := acf.Event{
		EventID: acf.NewID(), ArtifactID: artID, Type: acf.EventTypeUpdate,
		Timestamp:  ts,
		ParentHash: "unknown-remote-parent",
		Provenance: acf.Provenance{DeviceID: origin, SourceAgent: "claude-code"},
		Payload:    payload,
		EventTags:  tags,
	}
	sealed, err := sealEnvelope(ev, acf.ScopeGlobal, nil, []recipient{{deviceID: local.id, pub: local.pub}})
	require.NoError(t, err)
	return proto.RemoteEvent{
		ArtifactID: artID, EventID: ev.EventID, Kind: string(acf.KindConversation),
		Type: string(ev.Type), Timestamp: ts, ParentHash: ev.ParentHash,
		Bytes: sealed, Origin: origin,
	}
}

// Divergent turn sets must UNION, not last-writer-win, and the merged head
// must publish so the peer converges.
func TestImportInbound_ConversationDivergence_UnionMergesAndPublishes(t *testing.T) {
	local := newTestDevice(t, "device-A")
	pub := &stubRemotePublisher{}
	o, store := newStoreOrch(t, pub, local)

	t0 := time.Now().UTC().Add(-time.Minute)
	artID, head := seedConversation(t, store, local.id,
		turnEv("user", "q1", t0), turnEv("assistant", "a1", t0.Add(time.Second)))

	// Local continuation (device A types q2a).
	require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
		EventID: acf.NewID(), ArtifactID: artID, Type: acf.EventTypeUpdate,
		Timestamp: t0.Add(2 * time.Second), ParentHash: head.Hash,
		Provenance: acf.Provenance{DeviceID: local.id, SourceAgent: "claude-code"},
		Payload: convPayload(t,
			turnEv("user", "q1", t0), turnEv("assistant", "a1", t0.Add(time.Second)),
			turnEv("user", "q2a-local", t0.Add(2*time.Second))),
	}))
	eventsBefore, err := store.ReadEvents(acf.KindConversation, artID)
	require.NoError(t, err)

	// Concurrent peer continuation (device B typed q2b) arrives with an
	// unknown parent and a NEWER wall clock — the old code deleted the local
	// chain and adopted it (silent loss of q2a-local).
	inbound := wireConversation(t, local, artID, "device-B", t0.Add(3*time.Second),
		turnEv("user", "q1", t0), turnEv("assistant", "a1", t0.Add(time.Second)),
		turnEv("user", "q2b-remote", t0.Add(3*time.Second)))
	outcomes := o.ImportInboundResults([]proto.RemoteEvent{inbound})
	require.Equal(t, []ImportOutcome{ImportApplied}, outcomes)

	turns := localTurns(t, store, artID)
	require.Equal(t, []acf.TextTurn{
		{Role: "user", Text: "q1"}, {Role: "assistant", Text: "a1"},
		{Role: "user", Text: "q2a-local"}, {Role: "user", Text: "q2b-remote"},
	}, turns, "both sides' turns must survive")

	eventsAfter, err := store.ReadEvents(acf.KindConversation, artID)
	require.NoError(t, err)
	require.Greater(t, len(eventsAfter), len(eventsBefore), "merge must APPEND, never delete the chain")

	require.GreaterOrEqual(t, pub.Count(), 1, "the merged head must publish so the peer converges")
}

func TestImportInbound_LegacyAssistantEdgeEchoConvergesAndStaysClean(t *testing.T) {
	local := newTestDevice(t, "device-A")
	pub := &stubRemotePublisher{}
	o, store := newStoreOrch(t, pub, local)

	t0 := time.Date(2026, 7, 18, 20, 10, 54, 0, time.UTC)
	clean := []acf.ConversationEvent{
		turnEv("user", "what is capital of Poland", t0),
		turnEv("assistant", "Warsaw.", t0),
		turnEv("user", "how many people live in warsaw?", t0),
		turnEv("assistant", "About 1.87 million.", t0.Add(time.Minute)),
	}
	dirty := append([]acf.ConversationEvent{turnEv("assistant", "Warsaw.", t0)}, clean...)
	dirty = append(dirty, turnEv("assistant", "About 1.87 million.", t0.Add(2*time.Minute)))
	artID, _ := seedConversation(t, store, local.id, dirty...)

	inboundClean := wireConversation(t, local, artID, "device-B", t0.Add(3*time.Minute), clean...)
	require.Equal(t, []ImportOutcome{ImportApplied}, o.ImportInboundResults([]proto.RemoteEvent{inboundClean}))
	require.Equal(t, acf.ExtractTextTurns(clean), localTurns(t, store, artID))

	// A retained dirty head can arrive again after the first reconcile round.
	// It must not restore either assistant echo.
	inboundDirty := wireConversation(t, local, artID, "device-B", t0.Add(4*time.Minute), dirty...)
	beforeCorrectivePublish := pub.Count()
	require.Equal(t, []ImportOutcome{ImportApplied}, o.ImportInboundResults([]proto.RemoteEvent{inboundDirty}))
	require.Equal(t, acf.ExtractTextTurns(clean), localTurns(t, store, artID))
	require.Greater(t, pub.Count(), beforeCorrectivePublish,
		"a clean peer must publish a fresh corrective head so the retained dirty origin converges")
}

func TestImportInbound_TaggedAdjacentAssistantEchoRepairConvergesAndStaysClean(t *testing.T) {
	local := newTestDevice(t, "device-A")
	pub := &stubRemotePublisher{}
	o, store := newStoreOrch(t, pub, local)

	t0 := time.Date(2026, 7, 18, 21, 47, 30, 0, time.UTC)
	clean := []acf.ConversationEvent{
		turnEv("user", "what is capital of France?", t0),
		turnEv("assistant", "Paris.", t0.Add(2*time.Second)),
		turnEv("user", "how many people live in Paris?", t0.Add(3*time.Second)),
		turnEv("assistant", "About 2.1 million.", t0.Add(4*time.Second)),
	}
	dirty := []acf.ConversationEvent{
		clean[0], turnEv("assistant", "Paris.", t0.Add(time.Second)),
		clean[1], clean[2], clean[3],
		turnEv("assistant", "About 2.1 million.", t0.Add(5*time.Second)),
		turnEv("user", "how many people live in Paris?", t0.Add(6*time.Second)),
	}
	artID, _ := seedConversation(t, store, local.id, dirty...)

	inboundClean := wireConversationPayloadWithTags(
		t, local, artID, "device-B", t0.Add(time.Minute), convPayloadState(t, clean, nil),
		[]string{acf.LegacyAdjacentAssistantEchoRepairEventTag},
	)
	require.Equal(t, []ImportOutcome{ImportApplied}, o.ImportInboundResults([]proto.RemoteEvent{inboundClean}))
	require.Equal(t, acf.ExtractTextTurns(clean), localTurns(t, store, artID))
	head, ok, err := store.LastEvent(acf.KindConversation, artID)
	require.NoError(t, err)
	require.True(t, ok)
	require.Contains(t, head.EventTags, acf.LegacyAdjacentAssistantEchoRepairEventTag,
		"the convergence head must retain the adapter-authenticated repair proof")

	// A retained dirty copy may arrive after the first reconcile round. The
	// tagged clean local head must publish another correction, not union the
	// adjacent answer back into the thread.
	beforeCorrectivePublish := pub.Count()
	inboundDirty := wireConversation(t, local, artID, "device-B", t0.Add(2*time.Minute), dirty...)
	require.Equal(t, []ImportOutcome{ImportApplied}, o.ImportInboundResults([]proto.RemoteEvent{inboundDirty}))
	require.Equal(t, acf.ExtractTextTurns(clean), localTurns(t, store, artID))
	require.Greater(t, pub.Count(), beforeCorrectivePublish)
}

func TestImportInbound_UntaggedAdjacentAssistantRepeatIsPreserved(t *testing.T) {
	local := newTestDevice(t, "device-A")
	o, store := newStoreOrch(t, &stubRemotePublisher{}, local)
	t0 := time.Date(2026, 7, 18, 21, 47, 30, 0, time.UTC)
	clean := []acf.ConversationEvent{
		turnEv("user", "q1", t0), turnEv("assistant", "same", t0.Add(2*time.Second)),
		turnEv("user", "q2", t0.Add(3*time.Second)), turnEv("assistant", "a2", t0.Add(4*time.Second)),
	}
	dirty := []acf.ConversationEvent{
		clean[0], turnEv("assistant", "same", t0.Add(time.Second)), clean[1], clean[2], clean[3],
	}
	artID, _ := seedConversation(t, store, local.id, dirty...)

	// The same structural payload without the adapter's repair tag is
	// ambiguous and must use the normal lossless union.
	inbound := wireConversation(t, local, artID, "device-B", t0.Add(time.Minute), clean...)
	require.Equal(t, []ImportOutcome{ImportApplied}, o.ImportInboundResults([]proto.RemoteEvent{inbound}))
	require.Len(t, localTurns(t, store, artID), len(dirty))
}

func TestImportInbound_SideBranchRepairTagCannotAuthorizeMainCleanup(t *testing.T) {
	local := newTestDevice(t, "device-A")
	o, store := newStoreOrch(t, &stubRemotePublisher{}, local)
	t0 := time.Date(2026, 7, 18, 21, 47, 30, 0, time.UTC)
	clean := []acf.ConversationEvent{
		turnEv("user", "q1", t0), turnEv("assistant", "same", t0.Add(2*time.Second)),
		turnEv("user", "q2", t0.Add(3*time.Second)), turnEv("assistant", "a2", t0.Add(4*time.Second)),
	}
	dirty := []acf.ConversationEvent{
		clean[0], turnEv("assistant", "same", t0.Add(time.Second)), clean[1], clean[2], clean[3],
	}
	artID, mainHead := seedConversation(t, store, local.id, clean...)
	fork := appendConversationFork(t, store, local.id, artID, "review", mainHead)
	require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
		EventID: acf.NewID(), ArtifactID: artID, Type: acf.EventTypeUpdate,
		Branch: "review", Timestamp: t0.Add(5 * time.Second), ParentHash: fork.Hash,
		Provenance: acf.Provenance{DeviceID: local.id, SourceAgent: "codex"},
		Payload:    convPayload(t, clean...),
		EventTags:  []string{acf.LegacyAdjacentAssistantEchoRepairEventTag},
	}))

	inbound := wireConversation(t, local, artID, "device-B", t0.Add(time.Minute), dirty...)
	require.Equal(t, []ImportOutcome{ImportApplied}, o.ImportInboundResults([]proto.RemoteEvent{inbound}))
	require.Len(t, localTurns(t, store, artID), len(dirty),
		"a tag on the unrelated side-branch tail must not authorize deleting a main-branch turn")
	main, ok, err := store.MaterializedConversationPayloadFromStore(artID)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, acf.ExtractTextTurns(dirty), acf.ExtractTextTurns(main.Events))
	last, ok, err := store.LastEvent(acf.KindConversation, artID)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, acf.MainBranch, normalizeBranchName(last.Branch))
	require.NotContains(t, last.EventTags, acf.LegacyAdjacentAssistantEchoRepairEventTag)
}

func TestImportInbound_TaggedCleanBaselineCorrectsDirtyPeerOnAlignedHead(t *testing.T) {
	local := newTestDevice(t, "device-A")
	o, store := newStoreOrch(t, &stubRemotePublisher{}, local)
	t0 := time.Date(2026, 7, 18, 21, 47, 30, 0, time.UTC)
	clean := []acf.ConversationEvent{
		turnEv("user", "q1", t0), turnEv("assistant", "a1", t0.Add(2*time.Second)),
		turnEv("user", "q2", t0.Add(3*time.Second)), turnEv("assistant", "a2", t0.Add(4*time.Second)),
	}
	dirty := []acf.ConversationEvent{
		clean[0], turnEv("assistant", "a1", t0.Add(time.Second)), clean[1], clean[2], clean[3],
		turnEv("assistant", "a2", t0.Add(5*time.Second)), turnEv("user", "q2", t0.Add(6*time.Second)),
	}
	artID := acf.NewID()
	require.NoError(t, store.WriteArtifact(acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion, ArtifactID: artID,
		Kind: acf.KindConversation, Scope: acf.ScopeGlobal, Name: "conv",
		CreatedAt: t0, UpdatedAt: t0,
	}))
	baseline := acf.Event{
		EventID: acf.NewID(), ArtifactID: artID, Type: acf.EventTypeBaseline,
		Branch: acf.MainBranch, Timestamp: t0,
		Provenance:  acf.Provenance{DeviceID: local.id, SourceAgent: "codex"},
		Payload:     convPayload(t, clean...),
		EventTags:   []string{acf.LegacyAdjacentAssistantEchoRepairEventTag},
		AlignedHead: "origin-aligned-head", AlignedEventID: acf.NewID(),
	}
	require.NoError(t, store.AdoptBaseline(acf.KindConversation, baseline))

	inbound := wireConversation(t, local, artID, "device-B", t0.Add(time.Minute), dirty...)
	require.Equal(t, []ImportOutcome{ImportApplied}, o.ImportInboundResults([]proto.RemoteEvent{inbound}))
	require.Equal(t, acf.ExtractTextTurns(clean), localTurns(t, store, artID))
	last, ok, err := store.LastEvent(acf.KindConversation, artID)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, acf.MainBranch, normalizeBranchName(last.Branch))
	require.Equal(t, baseline.AlignedHead, last.ParentHash,
		"the correction must chain on aligned bookkeeping, not the baseline wrapper hash")
	require.Contains(t, last.EventTags, acf.LegacyAdjacentAssistantEchoRepairEventTag)
}

func TestImportInbound_ConversationUnionPreservesAttachments(t *testing.T) {
	local := newTestDevice(t, "device-A")
	pub := &stubRemotePublisher{}
	o, store := newStoreOrch(t, pub, local)

	t0 := time.Now().UTC().Add(-time.Minute)
	events := []acf.ConversationEvent{
		turnEv("user", "q1", t0), turnEv("assistant", "a1", t0.Add(time.Second)),
	}
	localAttachment := acf.Attachment{
		Kind: "image", MimeType: "image/png", ContentHash: "local-hash", Bytes: 10, Filename: "local.png",
	}
	inboundAttachment := acf.Attachment{
		Kind: "file", MimeType: "text/plain", ContentHash: "inbound-hash", Bytes: 20, Filename: "peer.txt",
	}
	artID, head := seedConversation(t, store, local.id, events...)
	require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
		EventID: acf.NewID(), ArtifactID: artID, Type: acf.EventTypeUpdate,
		Timestamp: t0.Add(2 * time.Second), ParentHash: head.Hash,
		Provenance: acf.Provenance{DeviceID: local.id, SourceAgent: "claude-code"},
		Payload:    convPayloadState(t, events, []acf.Attachment{localAttachment}),
	}))

	inbound := wireConversationPayload(t, local, artID, "device-B", t0.Add(3*time.Second),
		convPayloadState(t, events, []acf.Attachment{inboundAttachment}))
	require.Equal(t, []ImportOutcome{ImportApplied}, o.ImportInboundResults([]proto.RemoteEvent{inbound}))

	materialized, ok, err := store.MaterializedConversationPayloadFromStore(artID)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t,
		conversationAttachmentKeys([]acf.Attachment{localAttachment, inboundAttachment}),
		conversationAttachmentKeys(materialized.Attachments),
		"neither peer's attachment metadata may be dropped by a full-state union")
	require.NotZero(t, pub.Count(), "the attachment union must publish for peer convergence")
}

// A stale redelivery (strict prefix) must be skipped even when its wall clock
// is NEWER than the local head — content, not clocks, decides staleness.
func TestImportInbound_ConversationStalePrefix_SkippedDespiteNewerClock(t *testing.T) {
	local := newTestDevice(t, "device-A")
	pub := &stubRemotePublisher{}
	o, store := newStoreOrch(t, pub, local)

	t0 := time.Now().UTC().Add(-time.Minute)
	artID, _ := seedConversation(t, store, local.id,
		turnEv("user", "q1", t0), turnEv("assistant", "a1", t0.Add(time.Second)),
		turnEv("user", "q2", t0.Add(2*time.Second)))

	inbound := wireConversation(t, local, artID, "device-B",
		time.Now().UTC().Add(time.Hour), // skewed-ahead clock
		turnEv("user", "q1", t0), turnEv("assistant", "a1", t0.Add(time.Second)))
	outcomes := o.ImportInboundResults([]proto.RemoteEvent{inbound})
	require.Equal(t, []ImportOutcome{ImportApplied}, outcomes)
	require.Len(t, localTurns(t, store, artID), 3, "stale prefix must not regress the thread")
}

// A genuine extension must fast-forward even when the sender's clock is BEHIND
// the local head (the case the old wall-clock guard silently dropped).
func TestImportInbound_ConversationExtends_FastForwardsDespiteOlderClock(t *testing.T) {
	local := newTestDevice(t, "device-A")
	pub := &stubRemotePublisher{}
	o, store := newStoreOrch(t, pub, local)

	t0 := time.Now().UTC().Add(-time.Minute)
	artID, _ := seedConversation(t, store, local.id,
		turnEv("user", "q1", t0), turnEv("assistant", "a1", t0.Add(time.Second)))

	inbound := wireConversation(t, local, artID, "device-B",
		t0.Add(-time.Hour), // sender clock far behind local head timestamp
		turnEv("user", "q1", t0), turnEv("assistant", "a1", t0.Add(time.Second)),
		turnEv("user", "q2-new", t0.Add(2*time.Second)))
	outcomes := o.ImportInboundResults([]proto.RemoteEvent{inbound})
	require.Equal(t, []ImportOutcome{ImportApplied}, outcomes)
	require.Len(t, localTurns(t, store, artID), 3,
		"a content-superset must apply regardless of clock skew")
}

// ---------------------------------------------------------------------------
// Aligned-chains delta sync (2026-07) — lane-aware inbound routing.
// ---------------------------------------------------------------------------

// mainHeadHash reads the artifact's main-branch head BOOKKEEPING (the value the
// aligned-chains invariant is asserted on — design rule 7), applying the same
// HeadEventHash-overridden-by-BranchHeads[main] precedence the store uses.
func mainHeadHash(t *testing.T, store *acf.Store, artID string) string {
	t.Helper()
	art, err := store.ReadArtifact(acf.KindConversation, artID)
	require.NoError(t, err)
	head := art.HeadEventHash
	if art.BranchHeads != nil {
		if h := art.BranchHeads[acf.MainBranch]; h != "" {
			head = h
		}
	}
	return head
}

// appendConversationDelta appends a one-turn ConversationDeltaFormatV1 update
// chained onto the artifact's current head bookkeeping (NOT the log tail — on
// a receiver that just adopted a baseline the two differ) and returns the
// stored head event.
func appendConversationDelta(t *testing.T, store *acf.Store, deviceID, artID, text string, ts time.Time) acf.Event {
	t.Helper()
	payload, err := acf.EncodePayload(acf.ConversationPayload{
		Format: acf.ConversationDeltaFormatV1,
		Events: []acf.ConversationEvent{turnEv("user", text, ts)},
	})
	require.NoError(t, err)
	require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
		EventID: acf.NewID(), ArtifactID: artID, Type: acf.EventTypeUpdate,
		Timestamp: ts, ParentHash: mainHeadHash(t, store, artID),
		Provenance: acf.Provenance{DeviceID: deviceID, SourceAgent: "claude-code"},
		Payload:    payload,
	}))
	head, ok, err := store.LastEvent(acf.KindConversation, artID)
	require.NoError(t, err)
	require.True(t, ok)
	return head
}

// wireFromOutbound converts a captured OutboundEvent into the wire
// proto.RemoteEvent the plugin would deliver, Lane included.
func wireFromOutbound(out OutboundEvent) proto.RemoteEvent {
	return proto.RemoteEvent{
		NamespaceID: out.NamespaceID, BranchID: out.BranchID,
		ArtifactID: out.ArtifactID, EventID: out.EventID,
		ParentHash: out.ParentHash, Kind: out.Kind, Type: out.Type,
		Timestamp: out.Timestamp, Bytes: out.Bytes,
		Sequence: out.Sequence, Origin: out.Origin, Lane: out.Lane,
	}
}

// takeLane snapshots the publisher's captured events on one lane from *cursor
// onward and advances the cursor past them. Symmetric exchanges must snapshot
// BOTH sides before importing either, so events published DURING the exchange
// (e.g. a union-merge) are not delivered until the next round.
func takeLane(from *stubRemotePublisher, lane string, cursor *int) []OutboundEvent {
	events := laneEvents(from, lane)
	out := events[*cursor:]
	*cursor = len(events)
	return out
}

// importWires imports the events one at a time and returns the outcomes.
func importWires(t *testing.T, to *Orchestrator, events []OutboundEvent) []ImportOutcome {
	t.Helper()
	var outcomes []ImportOutcome
	for _, out := range events {
		outcomes = append(outcomes, to.ImportInboundResults([]proto.RemoteEvent{wireFromOutbound(out)})...)
	}
	return outcomes
}

// deliverLane imports, in order, the publisher's captured events on ONE lane
// starting at *cursor, advancing the cursor. Returns the per-event outcomes.
func deliverLane(t *testing.T, from *stubRemotePublisher, to *Orchestrator, lane string, cursor *int) []ImportOutcome {
	t.Helper()
	return importWires(t, to, takeLane(from, lane, cursor))
}

// countBaselines counts EventTypeBaseline events in the artifact's log.
func countBaselines(t *testing.T, store *acf.Store, artID string) int {
	t.Helper()
	events, err := store.ReadEvents(acf.KindConversation, artID)
	require.NoError(t, err)
	n := 0
	for _, e := range events {
		if e.Type == acf.EventTypeBaseline {
			n++
		}
	}
	return n
}

// Design rule 7 (alignment invariant): after adopting the origin's retained
// full state as a baseline, a receiver appends the origin's VERBATIM lane=live
// deltas natively — recomputing byte-identical hashes — so both stores' head
// bookkeeping stays EQUAL while only O(new turn) bytes cross the live lane.
func TestAlignedChains_DeltasChainNatively(t *testing.T) {
	devA, devB := newTestDevice(t, "device-A"), newTestDevice(t, "device-B")
	pubA, pubB := &stubRemotePublisher{}, &stubRemotePublisher{}
	oA, storeA := newStoreOrch(t, pubA, devA, Recipient{DeviceID: devB.id, PubKey: devB.pub})
	oB, storeB := newStoreOrch(t, pubB, devB, Recipient{DeviceID: devA.id, PubKey: devA.pub})

	t0 := time.Now().UTC().Add(-time.Minute)
	artID, _ := seedConversation(t, storeA, devA.id,
		turnEv("user", "q1", t0), turnEv("assistant", "a1", t0.Add(time.Second)))
	require.True(t, oA.forwardCommitted(artID))

	// B adopts A's retained full state as an aligned baseline.
	retainedCur := 0
	outcomes := deliverLane(t, pubA, oB, LaneRetained, &retainedCur)
	require.Equal(t, []ImportOutcome{ImportApplied}, outcomes)
	require.Equal(t, mainHeadHash(t, storeA, artID), mainHeadHash(t, storeB, artID),
		"adoption must align B's head bookkeeping with A's head hash")
	require.Equal(t, 1, countBaselines(t, storeB, artID))

	// From here on only lane=live deltas cross the wire — and they must chain
	// natively, with NO reconcile machinery on the bus.
	bus := &capturingBus{}
	oB.cfg.EventPublisher = bus
	liveCur := len(laneEvents(pubA, LaneLive)) // create's live: superseded by the adopted baseline

	appendConversationDelta(t, storeA, devA.id, artID, "q2", t0.Add(2*time.Second))
	require.True(t, oA.forwardCommitted(artID))
	appendConversationDelta(t, storeA, devA.id, artID, "q3", t0.Add(3*time.Second))
	require.True(t, oA.forwardCommitted(artID))

	live := takeLane(pubA, LaneLive, &liveCur)
	require.Len(t, live, 2)
	for _, out := range live {
		require.Less(t, len(out.Bytes), 64*1024,
			"a one-turn delta must stay small on the live lane regardless of history size")
	}
	outcomes = importWires(t, oB, live)
	require.Equal(t, []ImportOutcome{ImportApplied, ImportApplied}, outcomes,
		"verbatim deltas must chain natively")

	require.Equal(t, localTurns(t, storeA, artID), localTurns(t, storeB, artID))
	require.Len(t, localTurns(t, storeB, artID), 4)
	require.Equal(t, mainHeadHash(t, storeA, artID), mainHeadHash(t, storeB, artID),
		"aligned-chains invariant: head bookkeeping equal after live-only deltas")

	for _, kind := range []string{
		"remote.needs_baseline", "remote.inbound_merged",
		"remote.baseline_adopted", "remote.inbound_stale_skipped",
		"remote.hash_mismatch",
	} {
		require.False(t, bus.has(kind), "native chaining must not trigger reconcile: %s", kind)
	}
	require.Equal(t, 1, countBaselines(t, storeB, artID),
		"live deltas append verbatim; only the initial adoption is a baseline")
}

// A dropped live delta leaves the next one without its parent: the receiver
// must NOT retry-wedge — it defers (cursor-advancing), flags needs-baseline,
// and recovers + re-aligns from the origin's next lane=retained full state.
func TestAlignedChains_MissedDeltaRecoversViaRetained(t *testing.T) {
	devA, devB := newTestDevice(t, "device-A"), newTestDevice(t, "device-B")
	pubA, pubB := &stubRemotePublisher{}, &stubRemotePublisher{}
	oA, storeA := newStoreOrch(t, pubA, devA, Recipient{DeviceID: devB.id, PubKey: devB.pub})
	oB, storeB := newStoreOrch(t, pubB, devB, Recipient{DeviceID: devA.id, PubKey: devA.pub})

	t0 := time.Now().UTC().Add(-time.Minute)
	artID, _ := seedConversation(t, storeA, devA.id,
		turnEv("user", "q1", t0), turnEv("assistant", "a1", t0.Add(time.Second)))
	require.True(t, oA.forwardCommitted(artID))
	retainedCur := 0
	require.Equal(t, []ImportOutcome{ImportApplied}, deliverLane(t, pubA, oB, LaneRetained, &retainedCur))

	bus := &capturingBus{}
	oB.cfg.EventPublisher = bus

	appendConversationDelta(t, storeA, devA.id, artID, "q2", t0.Add(2*time.Second)) // live DROPPED
	require.True(t, oA.forwardCommitted(artID))
	appendConversationDelta(t, storeA, devA.id, artID, "q3", t0.Add(3*time.Second))
	require.True(t, oA.forwardCommitted(artID))

	// Deliver ONLY the second delta's live event: its parent (the dropped
	// delta) is unknown to B.
	live := laneEvents(pubA, LaneLive)
	outcomes := importWires(t, oB, live[len(live)-1:])
	require.Equal(t, []ImportOutcome{ImportDeferredNeedsBaseline}, outcomes,
		"an unknown-parent live delta must defer, not wedge the cursor")
	require.True(t, bus.has("remote.needs_baseline"), "the deferral must be visible on the bus")
	require.True(t, oB.needsBaselinePending(artID))
	eventsB, err := storeB.ReadEvents(acf.KindConversation, artID)
	require.NoError(t, err)
	require.Len(t, eventsB, 1, "the deferred delta must not be appended")

	// A's next retained (always published alongside the delta) recovers B.
	retained := laneEvents(pubA, LaneRetained)
	outcomes = importWires(t, oB, retained[len(retained)-1:])
	require.Equal(t, []ImportOutcome{ImportApplied}, outcomes)

	require.Equal(t, localTurns(t, storeA, artID), localTurns(t, storeB, artID))
	require.Len(t, localTurns(t, storeB, artID), 4, "the dropped delta's turn must be recovered")
	require.Equal(t, mainHeadHash(t, storeA, artID), mainHeadHash(t, storeB, artID),
		"baseline adoption must re-align the chains")
	require.False(t, oB.needsBaselinePending(artID), "adoption must clear needs-baseline")
}

// After a genuine divergence both sides union-merge to identical content under
// DIFFERENT heads. The retained exchange then re-aligns deterministically:
// EXACTLY ONE side adopts the peer's merge as a baseline (the smaller
// AlignedEventID wins — UUIDv7, string order is time order), both heads
// converge, and the exchange goes quiet with no turn lost.
func TestAlignedChains_DivergenceRealignsByEventIDTiebreak(t *testing.T) {
	devA, devB := newTestDevice(t, "device-A"), newTestDevice(t, "device-B")
	pubA, pubB := &stubRemotePublisher{}, &stubRemotePublisher{}
	oA, storeA := newStoreOrch(t, pubA, devA, Recipient{DeviceID: devB.id, PubKey: devB.pub})
	oB, storeB := newStoreOrch(t, pubB, devB, Recipient{DeviceID: devA.id, PubKey: devA.pub})

	// Aligned base: A creates, B adopts A's retained baseline.
	t0 := time.Now().UTC().Add(-time.Minute)
	artID, _ := seedConversation(t, storeA, devA.id,
		turnEv("user", "q1", t0), turnEv("assistant", "a1", t0.Add(time.Second)))
	require.True(t, oA.forwardCommitted(artID))
	retainedA, retainedB := 0, 0
	require.Equal(t, []ImportOutcome{ImportApplied}, deliverLane(t, pubA, oB, LaneRetained, &retainedA))
	require.Equal(t, mainHeadHash(t, storeA, artID), mainHeadHash(t, storeB, artID))
	liveA := len(laneEvents(pubA, LaneLive)) // create's live: superseded by the adoption
	liveB := 0

	// Concurrent continuations on BOTH devices off the aligned base.
	appendConversationDelta(t, storeA, devA.id, artID, "from-A", t0.Add(2*time.Second))
	require.True(t, oA.forwardCommitted(artID))
	appendConversationDelta(t, storeB, devB.id, artID, "from-B", t0.Add(3*time.Second))
	require.True(t, oB.forwardCommitted(artID))

	// Round 1 — cross-deliver live: neither delta extends the peer's moved
	// head, so BOTH sides defer needs-baseline.
	r1A, r1B := takeLane(pubA, LaneLive, &liveA), takeLane(pubB, LaneLive, &liveB)
	require.Equal(t, []ImportOutcome{ImportDeferredNeedsBaseline}, importWires(t, oB, r1A))
	require.Equal(t, []ImportOutcome{ImportDeferredNeedsBaseline}, importWires(t, oA, r1B))

	// Round 2 — cross-deliver retained: both classify convDiverged, append and
	// publish their (deterministic, identical-content) union merges.
	r2A, r2B := takeLane(pubA, LaneRetained, &retainedA), takeLane(pubB, LaneRetained, &retainedB)
	require.Equal(t, []ImportOutcome{ImportApplied}, importWires(t, oB, r2A))
	require.Equal(t, []ImportOutcome{ImportApplied}, importWires(t, oA, r2B))
	require.Equal(t, localTurns(t, storeA, artID), localTurns(t, storeB, artID),
		"union merges must be content-identical")
	require.NotEqual(t, mainHeadHash(t, storeA, artID), mainHeadHash(t, storeB, artID),
		"equal content still sits under different merge heads before the tiebreak")

	// The tiebreak winner is the merge with the SMALLER EventID.
	mergeA, ok, err := storeA.LastEvent(acf.KindConversation, artID)
	require.NoError(t, err)
	require.True(t, ok)
	mergeB, ok, err := storeB.LastEvent(acf.KindConversation, artID)
	require.NoError(t, err)
	require.True(t, ok)
	wantHead := mainHeadHash(t, storeA, artID)
	if mergeB.EventID < mergeA.EventID {
		wantHead = mainHeadHash(t, storeB, artID)
	}

	// Round 3 — cross-deliver the merges' retained: content equal, heads
	// differ → EXACTLY ONE side adopts.
	r3A, r3B := takeLane(pubA, LaneRetained, &retainedA), takeLane(pubB, LaneRetained, &retainedB)
	outB, outA := importWires(t, oB, r3A), importWires(t, oA, r3B)
	adopted := 0
	for _, oc := range append(append([]ImportOutcome{}, outB...), outA...) {
		switch oc {
		case ImportApplied:
			adopted++
		case ImportDeduped:
		default:
			t.Fatalf("unexpected tiebreak outcome %v", oc)
		}
	}
	require.Equal(t, 1, adopted, "exactly one side must adopt on the tiebreak")
	require.Equal(t, 2, countBaselines(t, storeA, artID)+countBaselines(t, storeB, artID),
		"B's setup adoption + exactly one tiebreak adoption")
	require.Equal(t, wantHead, mainHeadHash(t, storeA, artID), "smaller AlignedEventID must win")
	require.Equal(t, mainHeadHash(t, storeA, artID), mainHeadHash(t, storeB, artID),
		"tiebreak must re-align both heads")

	// Quiescence: drain everything still undelivered (the merges' live lane);
	// no new outbound publish may appear and no turn may be lost.
	countA, countB := pubA.Count(), pubB.Count()
	importWires(t, oB, takeLane(pubA, LaneLive, &liveA))
	importWires(t, oA, takeLane(pubB, LaneLive, &liveB))
	importWires(t, oB, takeLane(pubA, LaneRetained, &retainedA))
	importWires(t, oA, takeLane(pubB, LaneRetained, &retainedB))
	require.Equal(t, countA, pubA.Count(), "exchange must go quiet (no publish ping-pong)")
	require.Equal(t, countB, pubB.Count(), "exchange must go quiet (no publish ping-pong)")
	require.Equal(t, mainHeadHash(t, storeA, artID), mainHeadHash(t, storeB, artID))
	turns := localTurns(t, storeA, artID)
	require.Equal(t, turns, localTurns(t, storeB, artID))
	require.Len(t, turns, 4, "no turn lost: q1, a1, from-A, from-B")
}

// The lane=retained wire EventID is DISTINCT from the live lane's
// (RetainedWireEventID = head EventID + "-r") so the daemon's durable outbox —
// which keys files by EventID — persists BOTH lanes of one commit. Receivers
// never rely on EventID-log-dedupe for retained events: dedup is CONTENT-based
// (convEqual / the adopted baseline's recorded wire id). Pin every redelivery
// shape:
//
//  1. redelivery of the retained event B ADOPTED → deduped (the baseline
//     recorded the wire id, so the head fast-path catches it);
//  2. post-adoption redelivery of the LIVE lane of that same commit → a
//     benign ImportDeferredNeedsBaseline (the needs-baseline set is a HINT;
//     the next native live append clears it) — never a duplicate append;
//  3. after a native live append, redelivery of that commit's retained →
//     deduped via convEqual + same aligned head (content classification).
func TestAlignedChains_RetainedRedeliveryDedupes(t *testing.T) {
	devA, devB := newTestDevice(t, "device-A"), newTestDevice(t, "device-B")
	pubA, pubB := &stubRemotePublisher{}, &stubRemotePublisher{}
	oA, storeA := newStoreOrch(t, pubA, devA, Recipient{DeviceID: devB.id, PubKey: devB.pub})
	oB, storeB := newStoreOrch(t, pubB, devB, Recipient{DeviceID: devA.id, PubKey: devA.pub})
	_ = pubB

	t0 := time.Now().UTC().Add(-time.Minute)
	artID, createHead := seedConversation(t, storeA, devA.id,
		turnEv("user", "q1", t0), turnEv("assistant", "a1", t0.Add(time.Second)))
	require.True(t, oA.forwardCommitted(artID))

	live := laneEvents(pubA, LaneLive)
	retained := laneEvents(pubA, LaneRetained)
	require.Len(t, live, 1)
	require.Len(t, retained, 1)
	require.Equal(t, createHead.EventID, live[0].EventID,
		"the live lane carries the head's own EventID")
	require.Equal(t, RetainedWireEventID(createHead.EventID, devA.id), retained[0].EventID,
		"the retained lane must carry a DISTINCT, origin-scoped wire EventID (head + -r-<dev8>) so the outbox persists both lanes")

	// B adopts the retained baseline.
	require.Equal(t, []ImportOutcome{ImportApplied}, importWires(t, oB, retained))
	require.Equal(t, 1, countBaselines(t, storeB, artID))
	require.Equal(t, mainHeadHash(t, storeA, artID), mainHeadHash(t, storeB, artID))

	// (1) Redelivery of the adopted retained event → deduped, no new baseline.
	require.Equal(t, []ImportOutcome{ImportDeduped}, importWires(t, oB, retained),
		"a redelivered retained event must dedupe")
	require.Equal(t, 1, countBaselines(t, storeB, artID))

	// (2) The retained baseline records the deterministic derived wire ID, so
	// the canonical live ID remains distinct and safely defers for a baseline.
	require.Equal(t, []ImportOutcome{ImportDeferredNeedsBaseline}, importWires(t, oB, live))
	eventsB, err := storeB.ReadEvents(acf.KindConversation, artID)
	require.NoError(t, err)
	require.Len(t, eventsB, 1, "a post-adoption live redelivery must not append anything")
	require.True(t, oB.needsBaselinePending(artID))

	// (3) Next commit: the live delta chains natively (clearing the stale
	// hint), then the retained redelivery dedupes via CONTENT classification
	// (convEqual + same aligned head) — the "-r" id never log-dedupes it.
	deltaHead := appendConversationDelta(t, storeA, devA.id, artID, "q2", t0.Add(2*time.Second))
	require.True(t, oA.forwardCommitted(artID))
	live2 := laneEvents(pubA, LaneLive)[1:]
	require.Equal(t, []ImportOutcome{ImportApplied}, importWires(t, oB, live2))
	require.False(t, oB.needsBaselinePending(artID), "a native live append must clear the stale hint")

	retained2 := laneEvents(pubA, LaneRetained)[1:]
	require.Len(t, retained2, 1)
	require.Equal(t, RetainedWireEventID(deltaHead.EventID, devA.id), retained2[0].EventID)
	require.Equal(t, []ImportOutcome{ImportDeduped}, importWires(t, oB, retained2),
		"an already-chained commit's retained event must dedupe via convEqual")
	require.Equal(t, []ImportOutcome{ImportDeduped}, importWires(t, oB, retained2),
		"and redeliveries of it stay deduped")

	require.Equal(t, 1, countBaselines(t, storeB, artID),
		"content dedupe must never re-adopt")
	require.Equal(t, mainHeadHash(t, storeA, artID), mainHeadHash(t, storeB, artID))
	require.Equal(t, localTurns(t, storeA, artID), localTurns(t, storeB, artID))
	require.Len(t, localTurns(t, storeB, artID), 3)
}

// A verbatim live append trusts recomputed-hash == origin-hash by ComputeHash
// determinism, but a determinism regression (a new acf.Event field without
// omitempty, JSON drift across versions) would misalign the chains SILENTLY.
// The importer must cross-check the recomputed hash against the wire-carried
// one: on mismatch the (durably applied) event still advances the cursor, but
// the mismatch is loudly visible (warn + remote.hash_mismatch) and the
// artifact is pre-flagged needs-baseline — the origin's next delta chains
// onto a hash this store will not have.
func TestAlignedChains_TamperedWireHashFlagsNeedsBaseline(t *testing.T) {
	devA, devB := newTestDevice(t, "device-A"), newTestDevice(t, "device-B")
	pubA, pubB := &stubRemotePublisher{}, &stubRemotePublisher{}
	oA, storeA := newStoreOrch(t, pubA, devA, Recipient{DeviceID: devB.id, PubKey: devB.pub})
	oB, storeB := newStoreOrch(t, pubB, devB, Recipient{DeviceID: devA.id, PubKey: devA.pub})

	t0 := time.Now().UTC().Add(-time.Minute)
	artID, _ := seedConversation(t, storeA, devA.id, turnEv("user", "q1", t0))
	require.True(t, oA.forwardCommitted(artID))
	retainedCur := 0
	require.Equal(t, []ImportOutcome{ImportApplied}, deliverLane(t, pubA, oB, LaneRetained, &retainedCur))

	bus := &capturingBus{}
	oB.cfg.EventPublisher = bus

	head := appendConversationDelta(t, storeA, devA.id, artID, "q2", t0.Add(2*time.Second))

	// Craft the live wire event VERBATIM except for a tampered carried Hash —
	// the observable shape of a determinism regression (recomputed != carried).
	// ComputeHash zeroes the Hash field, so the append itself still succeeds;
	// only the integrity claim differs.
	tampered := head
	tampered.Hash = "not-" + head.Hash
	sealed, err := sealEnvelope(tampered, acf.ScopeGlobal, nil, []recipient{{deviceID: devB.id, pub: devB.pub}})
	require.NoError(t, err)
	wire := proto.RemoteEvent{
		ArtifactID: artID, EventID: head.EventID, Kind: string(acf.KindConversation),
		Type: string(head.Type), Timestamp: head.Timestamp, ParentHash: head.ParentHash,
		Bytes: sealed, Origin: devA.id, Lane: LaneLive, Sequence: 2,
	}

	outcomes := oB.ImportInboundResults([]proto.RemoteEvent{wire})
	require.Equal(t, []ImportOutcome{ImportRejected}, outcomes,
		"a carried hash that does not authenticate the canonical event must not land")
	require.NotEqual(t, localTurns(t, storeA, artID), localTurns(t, storeB, artID))
	require.True(t, bus.has("remote.inbound_error"))
	require.False(t, oB.needsBaselinePending(artID))
}

// wireRetainedConversation builds a lane=retained wire event carrying a full
// conversation payload plus alignment metadata, shaped as a peer's
// forwardCommitted would emit it (wire EventID = the ORIGIN-scoped
// RetainedWireEventID of the advertised head EventID).
func wireRetainedConversation(t *testing.T, local testDevice, artID, origin, alignedHead, alignedEventID string, ts time.Time, events ...acf.ConversationEvent) proto.RemoteEvent {
	return wireRetainedConversationOnBranch(t, local, artID, origin, "", alignedHead, alignedEventID, ts, events...)
}

func wireRetainedConversationOnBranch(t *testing.T, local testDevice, artID, origin, branch, alignedHead, alignedEventID string, ts time.Time, events ...acf.ConversationEvent) proto.RemoteEvent {
	t.Helper()
	ev := acf.Event{
		EventID: alignedEventID, ArtifactID: artID, Type: acf.EventTypeUpdate,
		Timestamp:  ts,
		ParentHash: alignedHead,
		Branch:     branch,
		Provenance: acf.Provenance{DeviceID: origin, SourceAgent: "claude-code"},
		Payload:    convPayload(t, events...),
	}
	headHash, err := acf.ComputeHash(ev)
	require.NoError(t, err)
	ev.Hash = headHash
	ev.AlignedHead = headHash
	ev.AlignedEventID = alignedEventID
	ev.Hash, err = acf.ComputeHash(ev)
	require.NoError(t, err)
	sealed, err := sealEnvelope(ev, acf.ScopeGlobal, nil, []recipient{{deviceID: local.id, pub: local.pub}})
	require.NoError(t, err)
	return proto.RemoteEvent{
		ArtifactID: artID, EventID: RetainedWireEventID(alignedEventID, origin),
		Kind: string(acf.KindConversation), Type: string(ev.Type),
		Timestamp: ts, ParentHash: ev.ParentHash, BranchID: normalizeBranchName(branch),
		Bytes: sealed, Origin: origin, Lane: LaneRetained,
	}
}

func retainedAlignedHead(t *testing.T, local testDevice, wire proto.RemoteEvent) string {
	t.Helper()
	ev, _, _, err := openEnvelope(wire.Bytes, local.id, local.priv)
	require.NoError(t, err)
	return ev.AlignedHead
}

func TestAlignedChains_SideBranchCheckpointRestoresWithoutForkAncestry(t *testing.T) {
	local := newTestDevice(t, "device-local")
	o, store := newStoreOrch(t, &stubRemotePublisher{}, local)
	t0 := time.Now().UTC().Add(-time.Minute)
	artID, mainHead := seedConversation(t, store, local.id,
		turnEv("user", "main question", t0), turnEv("assistant", "main answer", t0.Add(time.Second)))

	// Gaps are branch-scoped. Restoring review must not conceal unrelated main
	// or experiment gaps on the same artifact.
	o.markNeedsBaseline(artID, acf.MainBranch, "gap-main")
	o.markNeedsBaseline(artID, "review", "gap-review")
	o.markNeedsBaseline(artID, "experiment", "gap-experiment")

	retained := wireRetainedConversationOnBranch(t, local, artID, "device-remote", "review",
		"remote-review-parent", acf.NewID(), t0.Add(2*time.Second),
		turnEv("user", "review question", t0), turnEv("assistant", "review answer", t0.Add(time.Second)))
	alignedReviewHead := retainedAlignedHead(t, local, retained)
	require.Equal(t, []ImportOutcome{ImportApplied}, o.ImportInboundResults([]proto.RemoteEvent{retained}))
	require.False(t, o.needsBaselineBranchPending(artID, "review"))
	require.True(t, o.needsBaselineBranchPending(artID, acf.MainBranch))
	require.True(t, o.needsBaselineBranchPending(artID, "experiment"))

	art, err := store.ReadArtifact(acf.KindConversation, artID)
	require.NoError(t, err)
	require.Equal(t, mainHead.Hash, art.HeadEventHash, "side checkpoint must not move main")
	require.Equal(t, alignedReviewHead, art.BranchHeads["review"])
	require.Equal(t, []acf.TextTurn{{Role: "user", Text: "main question"}, {Role: "assistant", Text: "main answer"}},
		localTurns(t, store, artID), "side checkpoint must not pollute main materialization")

	// The next exact origin delta chains directly onto the checkpoint's aligned
	// side head even though this receiver has no fork event for review.
	delta, err := acf.EncodePayload(acf.ConversationPayload{
		Format: acf.ConversationDeltaFormatV1,
		Events: []acf.ConversationEvent{turnEv("user", "review follow-up", t0.Add(3*time.Second))},
	})
	require.NoError(t, err)
	liveEvent := acf.Event{
		EventID:    acf.NewID(),
		ArtifactID: artID,
		Type:       acf.EventTypeUpdate,
		Branch:     "review",
		Timestamp:  t0.Add(3 * time.Second),
		ParentHash: alignedReviewHead,
		Provenance: acf.Provenance{DeviceID: "device-remote", SourceAgent: "claude-code"},
		Payload:    delta,
	}
	liveEvent.Hash, err = acf.ComputeHash(liveEvent)
	require.NoError(t, err)
	sealed, err := sealEnvelope(liveEvent, acf.ScopeGlobal, nil, []recipient{{deviceID: local.id, pub: local.pub}})
	require.NoError(t, err)
	live := proto.RemoteEvent{
		ArtifactID: artID, EventID: liveEvent.EventID, Kind: string(acf.KindConversation),
		Type: string(liveEvent.Type), Timestamp: liveEvent.Timestamp, ParentHash: liveEvent.ParentHash,
		BranchID: "review", Bytes: sealed, Origin: "device-remote", Lane: LaneLive,
	}
	require.Equal(t, []ImportOutcome{ImportApplied}, o.ImportInboundResults([]proto.RemoteEvent{live}))

	payload, projected, ok, err := store.ProjectConversationPayloadForBranch(artID, "review", acf.BranchProjectionOpts{})
	require.NoError(t, err)
	require.True(t, ok)
	require.Len(t, projected, 2, "side recovery root plus one native delta")
	require.Equal(t, []acf.TextTurn{
		{Role: "user", Text: "review question"},
		{Role: "assistant", Text: "review answer"},
		{Role: "user", Text: "review follow-up"},
	}, acf.ExtractTextTurns(payload.Events))
	all, err := store.ReadEvents(acf.KindConversation, artID)
	require.NoError(t, err)
	require.NoError(t, acf.VerifyChain(all))
	require.True(t, o.needsBaselinePending(artID), "unrelated branch gaps remain visible")
}

// The retained wire id derivation must be ORIGIN-SCOPED: head EventID + "-r-"
// + a short origin discriminator (first 8 chars of the origin device id), so
// two different origins can never collide on one wire id. The id stays opaque
// to receivers/transports; an empty origin (unpaired daemon) keeps the plain
// "-r" shape.
func TestRetainedWireEventID_OriginScoped(t *testing.T) {
	require.Equal(t, "evt-1-r", RetainedWireEventID("evt-1", ""),
		"no origin id keeps the plain -r shape")

	got := RetainedWireEventID("evt-1", "device-ABCD")
	require.True(t, strings.HasPrefix(got, "evt-1-r-"),
		"a retained wire id is a strict extension of the base EventID + -r suffix")
	require.Len(t, got, len("evt-1-r-")+retainedOriginDiscriminatorLen,
		"the origin discriminator is a fixed-width hash slice")
	require.Equal(t, got, RetainedWireEventID("evt-1", "device-ABCD"),
		"the discriminator is deterministic for a given origin")
	require.NotEqual(t,
		RetainedWireEventID("evt-1", "device-A"),
		RetainedWireEventID("evt-1", "device-B"),
		"different origins must never share a retained wire id")
	// Finding-1 fix: origins that share an 8-char LEADING group (e.g. UUIDv7
	// device ids paired in the same time window) must still differ — the
	// discriminator hashes the WHOLE id, not a raw prefix.
	require.NotEqual(t,
		RetainedWireEventID("evt-1", "aaaaaaaa-1111-1111-1111-111111111111"),
		RetainedWireEventID("evt-1", "aaaaaaaa-2222-2222-2222-222222222222"),
		"origins sharing an 8-char leading group must not collide")
}

// The 3+ device edge behind the origin scoping: two devices legacy-rebase-
// re-authored the SAME head EventID under different parents, and both publish
// retained events for it. With a shared "-r" wire id, a receiver that adopted
// one origin's baseline records that wire id as its log tail EventID — so the
// OTHER origin's different-hash retained event was dropped by the tail
// fast-path dedup BEFORE the reconcile tiebreak could run, leaving the
// receiver transiently misaligned (needs-baseline churn on that origin's live
// deltas) until the next real commit. Origin-scoped wire ids keep the two
// events distinct, so the fast path falls through and the tiebreak decides.
func TestAlignedChains_RetainedWireIDOriginScopedBreaksReauthoredHeadCollision(t *testing.T) {
	local := newTestDevice(t, "device-C")
	pub := &stubRemotePublisher{}
	o, store := newStoreOrch(t, pub, local)

	t0 := time.Now().UTC().Add(-time.Minute)
	turns := []acf.ConversationEvent{
		turnEv("user", "q1", t0), turnEv("assistant", "a1", t0.Add(time.Second)),
	}
	artID := acf.NewID()
	reauthoredID := acf.NewID() // the head EventID BOTH origins re-authored

	// Adopt device A's baseline first.
	fromA := wireRetainedConversation(t, local, artID, "device-A", "hash-bbb-A",
		reauthoredID, t0.Add(2*time.Second), turns...)
	headA := retainedAlignedHead(t, local, fromA)
	require.Equal(t, []ImportOutcome{ImportApplied}, o.ImportInboundResults([]proto.RemoteEvent{fromA}))
	require.Equal(t, 1, countBaselines(t, store, artID))
	require.Equal(t, headA, mainHeadHash(t, store, artID))

	// Device B re-authored the SAME EventID under a different parent → same
	// AlignedEventID, different hash, and — the fix — a DISTINCT wire id.
	var fromB proto.RemoteEvent
	var headB string
	for i := 0; i < 10000; i++ {
		fromB = wireRetainedConversation(t, local, artID, "device-B", fmt.Sprintf("parent-%d", i),
			reauthoredID, t0.Add(3*time.Second), turns...)
		headB = retainedAlignedHead(t, local, fromB)
		if headB < headA {
			break
		}
	}
	require.Less(t, headB, headA)
	require.NotEqual(t, fromA.EventID, fromB.EventID,
		"two origins re-authoring one head EventID must publish DISTINCT retained wire ids")

	// The event must reach the reconcile tiebreak (AlignedEventID sorts before
	// any adopted \"-r-\"-suffixed tail id → adopt), never die on the tail
	// fast-path dedup as the shared \"-r\" id did.
	require.Equal(t, []ImportOutcome{ImportApplied}, o.ImportInboundResults([]proto.RemoteEvent{fromB}),
		"the second origin's retained event must reconcile, not dedupe on the colliding wire id")
	require.Equal(t, headB, mainHeadHash(t, store, artID))
	require.Equal(t, []acf.TextTurn{
		{Role: "user", Text: "q1"}, {Role: "assistant", Text: "a1"},
	}, localTurns(t, store, artID), "no turn may be lost across the re-align")
}

// The re-align tiebreak had no arm for equal content under the SAME head
// EventID with DIFFERENT hashes (reachable via legacy-rebase histories where
// both devices re-authored an event with the same EventID under different
// parents): both sides deduped, neither adopted, and the artifact sat
// content-converged but hash-misaligned — every live delta deferring
// needs-baseline — until the next real commit. Deterministic fix: when the
// EventIDs tie, the SMALLER AlignedHead (plain string compare) wins. Any
// strict total order works — both devices evaluate the same predicate with
// the operands swapped and the hashes are unequal in this arm, so exactly one
// side sees "inbound < local" and adopts while the other dedupes.
func TestAlignedChains_TiebreakEqualEventIDDifferentHashUsesHashOrder(t *testing.T) {
	local := newTestDevice(t, "device-A")
	pub := &stubRemotePublisher{}
	o, store := newStoreOrch(t, pub, local)

	t0 := time.Date(2026, time.July, 20, 12, 0, 0, 0, time.UTC)
	turns := []acf.ConversationEvent{
		turnEv("user", "q1", t0), turnEv("assistant", "a1", t0.Add(time.Second)),
	}
	const artID = "0197f000-aaaa-7000-8000-000000000101"
	head := seedConversationFixture(t, store, local.id, artID,
		"0197f000-bbbb-7000-8000-000000000102", t0.Add(time.Minute), turns...)
	localHash := mainHeadHash(t, store, artID)

	// This fixed peer fixture hashes strictly LARGER than the fixed local head:
	// we win; dedupe, keep our head.
	larger := wireRetainedConversation(t, local, artID, "device-B",
		"larger-parent-9", head.EventID, t0.Add(2*time.Second), turns...)
	largerHash := retainedAlignedHead(t, local, larger)
	require.Greater(t, largerHash, localHash)
	require.Equal(t, []ImportOutcome{ImportDeduped}, o.ImportInboundResults([]proto.RemoteEvent{larger}))
	require.Equal(t, 0, countBaselines(t, store, artID))
	require.Equal(t, localHash, mainHeadHash(t, store, artID),
		"the larger inbound hash must NOT be adopted (the peer's mirror tiebreak adopts ours)")

	// This fixed peer fixture hashes strictly SMALLER than the fixed local head:
	// the peer wins; adopt its baseline and re-align.
	smaller := wireRetainedConversation(t, local, artID, "device-B",
		"smaller-parent-0", head.EventID, t0.Add(3*time.Second), turns...)
	smallerHash := retainedAlignedHead(t, local, smaller)
	require.Less(t, smallerHash, localHash)
	require.Equal(t, []ImportOutcome{ImportApplied}, o.ImportInboundResults([]proto.RemoteEvent{smaller}))
	require.Equal(t, 1, countBaselines(t, store, artID), "exactly one side adopts on the hash tiebreak")
	require.Equal(t, smallerHash, mainHeadHash(t, store, artID),
		"the smaller inbound hash must be adopted so both heads re-align")
	require.Equal(t, []acf.TextTurn{
		{Role: "user", Text: "q1"}, {Role: "assistant", Text: "a1"},
	}, localTurns(t, store, artID), "no turn may be lost across the re-align")
}

// An inbound retained-slot CLEAR (Clear=true, empty Bytes) is transport
// plumbing — the actual redaction arrives as a normal lane=live event — so a
// receiving daemon must reject an unsigned clear. V2 clear authority is a
// signed bodyless control envelope, never this legacy flag alone.
func TestImportInboundUnsignedRetainedClearIsRejected(t *testing.T) {
	local := newTestDevice(t, "device-A")
	pub := &stubRemotePublisher{}
	o, store := newStoreOrch(t, pub, local)
	bus := &capturingBus{}
	o.cfg.EventPublisher = bus

	t0 := time.Now().UTC().Add(-time.Minute)
	artID, _ := seedConversation(t, store, local.id, turnEv("user", "q1", t0))

	clear := proto.RemoteEvent{
		ArtifactID: artID, EventID: "peer-head-id" + retainedEventIDSuffix,
		Kind: string(acf.KindConversation), Type: string(acf.EventTypeRedaction),
		Timestamp: t0.Add(time.Second), Lane: LaneRetained,
		Origin: "device-B", Clear: true,
	}
	outcomes := o.ImportInboundResults([]proto.RemoteEvent{clear})
	require.Equal(t, []ImportOutcome{ImportRejected}, outcomes,
		"an unsigned clear must never be accepted as transport authority")

	events, err := store.ReadEvents(acf.KindConversation, artID)
	require.NoError(t, err)
	require.Len(t, events, 1, "a clear must not touch the local store")
	art, err := store.ReadArtifact(acf.KindConversation, artID)
	require.NoError(t, err)
	require.False(t, art.Tombstoned, "the clear is not the redaction; the live lane carries that")
	require.True(t, bus.has("remote.inbound_error"), "the rejected clear must be visible")
}

// Lane=="" is a legacy pre-lane event (old outbox replay, non-lane transport)
// and MUST keep the OLD reconcile behavior — content classification with the
// destructive fast-forward rebase — never the lane paths (no baseline
// adoption, no needs-baseline deferral).
func TestImportInbound_LanelessConversationKeepsLegacyReconcile(t *testing.T) {
	local := newTestDevice(t, "device-A")
	pub := &stubRemotePublisher{}
	o, store := newStoreOrch(t, pub, local)
	bus := &capturingBus{}
	o.cfg.EventPublisher = bus

	t0 := time.Now().UTC().Add(-time.Minute)
	artID, _ := seedConversation(t, store, local.id,
		turnEv("user", "q1", t0), turnEv("assistant", "a1", t0.Add(time.Second)))

	inbound := wireConversation(t, local, artID, "device-B", t0.Add(2*time.Second),
		turnEv("user", "q1", t0), turnEv("assistant", "a1", t0.Add(time.Second)),
		turnEv("user", "q2-remote", t0.Add(2*time.Second)))
	require.Empty(t, inbound.Lane, "wireConversation builds legacy pre-lane events")
	outcomes := o.ImportInboundResults([]proto.RemoteEvent{inbound})
	require.Equal(t, []ImportOutcome{ImportApplied}, outcomes)

	require.Len(t, localTurns(t, store, artID), 3,
		"legacy fast-forward must apply the inbound superset")
	cached, cachedOK, err := store.ValidatedCachedMaterializedConversationPayload(artID)
	require.NoError(t, err)
	require.True(t, cachedOK, "legacy full-state fast-forward must prime the validated fan-out cache")
	require.Len(t, acf.ExtractTextTurns(cached.Events), 3)
	require.Equal(t, 0, countBaselines(t, store, artID),
		"the legacy path must never adopt baselines")
	require.False(t, bus.has("remote.needs_baseline"),
		"the legacy path must never defer needs-baseline")
	require.False(t, o.needsBaselinePending(artID))
}

// Full two-device exchange: concurrent edits on a shared thread converge to
// the identical union on both devices within two delivery rounds.
func TestConversationMerge_ConvergesAcrossTwoDevices(t *testing.T) {
	devA := newTestDevice(t, "device-A")
	devB := newTestDevice(t, "device-B")
	pubA, pubB := &stubRemotePublisher{}, &stubRemotePublisher{}
	oA, storeA := newStoreOrch(t, pubA, devA, Recipient{DeviceID: devB.id, PubKey: devB.pub})
	oB, storeB := newStoreOrch(t, pubB, devB, Recipient{DeviceID: devA.id, PubKey: devA.pub})

	deliver := func(from *stubRemotePublisher, to *Orchestrator, since int) int {
		from.mu.Lock()
		events := append([]OutboundEvent(nil), from.events[since:]...)
		from.mu.Unlock()
		for _, out := range events {
			to.ImportInbound([]proto.RemoteEvent{{
				NamespaceID: out.NamespaceID, BranchID: out.BranchID,
				ArtifactID: out.ArtifactID, EventID: out.EventID,
				ParentHash: out.ParentHash, Kind: out.Kind, Type: out.Type,
				Timestamp: out.Timestamp, Bytes: out.Bytes,
				Sequence: out.Sequence, Origin: out.Origin,
			}})
		}
		return since + len(events)
	}

	// Round 0: A creates the thread and it replicates to B.
	t0 := time.Now().UTC().Add(-time.Minute)
	artID, _ := seedConversation(t, storeA, devA.id,
		turnEv("user", "q1", t0), turnEv("assistant", "a1", t0.Add(time.Second)))
	require.True(t, oA.forwardCommitted(artID))
	cursorA := deliver(pubA, oB, 0)

	// Concurrent local continuations on both devices.
	appendLocal := func(store *acf.Store, deviceID, text string, ts time.Time) {
		head, ok, err := store.LastEvent(acf.KindConversation, artID)
		require.NoError(t, err)
		require.True(t, ok)
		prev, err := acf.DecodeConversationPayload(head)
		require.NoError(t, err)
		require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
			EventID: acf.NewID(), ArtifactID: artID, Type: acf.EventTypeUpdate,
			Timestamp: ts, ParentHash: head.Hash,
			Provenance: acf.Provenance{DeviceID: deviceID, SourceAgent: "claude-code"},
			Payload:    convPayload(t, append(prev.Events, turnEv("user", text, ts))...),
		}))
	}
	appendLocal(storeA, devA.id, "from-A", t0.Add(2*time.Second))
	appendLocal(storeB, devB.id, "from-B", t0.Add(3*time.Second))
	require.True(t, oA.forwardCommitted(artID))
	require.True(t, oB.forwardCommitted(artID))

	// Round 1: cross-deliver the divergent updates (each side union-merges and
	// publishes its merge). Round 2: cross-deliver the merges (each side
	// classifies convEqual and goes quiet).
	cursorB := 0
	for round := 0; round < 2; round++ {
		cursorA = deliver(pubA, oB, cursorA)
		cursorB = deliver(pubB, oA, cursorB)
	}

	turnsA := localTurns(t, storeA, artID)
	turnsB := localTurns(t, storeB, artID)
	require.Equal(t, turnsA, turnsB, "both devices must converge on the identical thread")
	require.Len(t, turnsA, 4, "no turn may be lost: q1, a1, from-A, from-B")
	require.Equal(t, cursorA, deliver(pubA, oB, cursorA), "exchange must go quiet (no publish ping-pong)")
	require.Equal(t, cursorB, deliver(pubB, oA, cursorB))
}
