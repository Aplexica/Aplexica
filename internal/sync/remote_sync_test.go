package syncd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
	"github.com/aplexica/aplexica/internal/adapter/claudecode"
	"github.com/aplexica/aplexica/internal/adapter/codex"
	"github.com/aplexica/aplexica/internal/keys"
	"github.com/aplexica/aplexica/internal/plugin/proto"
	"github.com/aplexica/aplexica/internal/project"
	"github.com/aplexica/aplexica/internal/secrets"
	"github.com/aplexica/aplexica/internal/syncrules"
	"github.com/stretchr/testify/require"
)

// testDevice is a synthetic device identity (id + X25519 keypair) used to drive
// the encrypted outbound/inbound paths in tests.
type testDevice struct {
	id   string
	priv [keys.X25519KeySize]byte
	pub  [keys.X25519KeySize]byte
}

func newTestDevice(t *testing.T, id string) testDevice {
	t.Helper()
	priv, pub, err := keys.NewDeviceKey()
	require.NoError(t, err)
	return testDevice{id: id, priv: priv, pub: pub}
}

// fixedKeyProvider satisfies DeviceKeyProvider with a fixed private key.
type fixedKeyProvider struct {
	priv [keys.X25519KeySize]byte
}

func (p fixedKeyProvider) Private() ([keys.X25519KeySize]byte, error) { return p.priv, nil }

// staticResolver satisfies RecipientResolver returning a fixed recipient set
// (the encrypted-path equivalent of "these devices may decrypt").
type staticResolver struct {
	recipients []Recipient
}

func (r staticResolver) Recipients(_ string) ([]Recipient, error) { return r.recipients, nil }

// newStoreOrch builds a minimal orchestrator backed by a real on-disk store.
// The local device's keypair drives both the inbound decrypt (DeviceKeyProvider)
// and is included in the recipient set so the sender can decrypt its own
// outbound (RecipientResolver). extraRecipients adds peer devices the outbound
// envelope is also sealed for.
func newStoreOrch(t *testing.T, pub RemoteEventPublisher, local testDevice, extraRecipients ...Recipient) (*Orchestrator, *acf.Store) {
	t.Helper()
	root := realTempDir(t)
	watched := filepath.Join(root, "proj")
	require.NoError(t, os.MkdirAll(watched, 0o755))
	adapters, store, _ := buildAllThreeAdapters(t, root)

	recipients := append([]Recipient{{DeviceID: local.id, PubKey: local.pub}}, extraRecipients...)
	o, err := NewOrchestrator(Config{
		Dir:                  watched,
		Adapters:             adapters,
		Store:                store,
		QuietPeriod:          50 * time.Millisecond,
		GuardWindow:          time.Second,
		RemoteEventPublisher: pub,
		LocalDeviceID:        local.id,
		RecipientResolver:    staticResolver{recipients: recipients},
		DeviceKeyProvider:    fixedKeyProvider{priv: local.priv},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = o.Close() })
	return o, store
}

// seedArtifact writes an artifact + one genesis event directly to the store
// and returns the artifact id. provenanceDevice is stamped on the event so
// tests can exercise the local-vs-remote origin discrimination.
func seedArtifact(t *testing.T, store *acf.Store, kind acf.Kind, provenanceDevice string) (string, acf.Event) {
	t.Helper()
	id := acf.NewID()
	now := time.Now().UTC()
	require.NoError(t, store.WriteArtifact(acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion,
		ArtifactID:       id,
		Kind:             kind,
		Scope:            acf.ScopeGlobal,
		CreatedAt:        now,
		UpdatedAt:        now,
	}))
	payload, _ := json.Marshal(acf.MemoryPayload{Format: "markdown", Content: "hello"})
	ev := acf.Event{
		EventID:    acf.NewID(),
		ArtifactID: id,
		Type:       acf.EventTypeCreate,
		Timestamp:  now,
		Provenance: acf.Provenance{DeviceID: provenanceDevice, SourceAgent: "test"},
		Payload:    payload,
		ParentHash: "",
	}
	require.NoError(t, store.AppendEvent(kind, ev))
	// Re-read so the test sees the stored Hash.
	events, err := store.ReadEvents(kind, id)
	require.NoError(t, err)
	require.Len(t, events, 1)
	return id, events[0]
}

func syncTestConversationTurn(role, text string) acf.ConversationEvent {
	return acf.ConversationEvent{
		Type:    acf.EventTypeTurn,
		Role:    role,
		Content: []acf.ContentBlock{{Type: "text", Text: text}},
	}
}

func encodeSyncTestConversationPayload(t *testing.T, format string, events []acf.ConversationEvent) json.RawMessage {
	t.Helper()
	payload, err := acf.EncodePayload(acf.ConversationPayload{Format: format, Events: events})
	require.NoError(t, err)
	return payload
}

func seedConversationWithDelta(t *testing.T, store *acf.Store, provenanceDevice string) (string, acf.Event, acf.Event) {
	t.Helper()
	id := acf.NewID()
	now := time.Now().UTC()
	require.NoError(t, store.WriteArtifact(acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion,
		ArtifactID:       id,
		Kind:             acf.KindConversation,
		Scope:            acf.ScopeGlobal,
		CreatedAt:        now,
		UpdatedAt:        now,
	}))
	create := acf.Event{
		EventID:    acf.NewID(),
		ArtifactID: id,
		Type:       acf.EventTypeCreate,
		Timestamp:  now,
		Provenance: acf.Provenance{DeviceID: provenanceDevice, SourceAgent: "codex"},
		Payload: encodeSyncTestConversationPayload(t, acf.ConversationFormatV1, []acf.ConversationEvent{
			syncTestConversationTurn("user", "What is the Capital of China?"),
		}),
	}
	require.NoError(t, store.AppendEvent(acf.KindConversation, create))
	events, err := store.ReadEvents(acf.KindConversation, id)
	require.NoError(t, err)
	create = events[0]

	update := acf.Event{
		EventID:    acf.NewID(),
		ArtifactID: id,
		Type:       acf.EventTypeUpdate,
		Timestamp:  now.Add(time.Second),
		Provenance: acf.Provenance{DeviceID: provenanceDevice, SourceAgent: "codex"},
		Payload: encodeSyncTestConversationPayload(t, acf.ConversationDeltaFormatV1, []acf.ConversationEvent{
			syncTestConversationTurn("assistant", "The capital of China is **Beijing**."),
		}),
		ParentHash: create.Hash,
	}
	require.NoError(t, store.AppendEvent(acf.KindConversation, update))
	events, err = store.ReadEvents(acf.KindConversation, id)
	require.NoError(t, err)
	return id, events[0], events[1]
}

func appendConversationFork(t *testing.T, store *acf.Store, deviceID, artID, branch string, parent acf.Event) acf.Event {
	t.Helper()
	now := time.Now().UTC()
	originAgent := parent.Provenance.SourceAgent
	if originAgent == "" {
		originAgent = "test"
	}
	require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
		EventID:          acf.NewID(),
		ArtifactID:       artID,
		Type:             acf.EventTypeForkOuter,
		Branch:           branch,
		Timestamp:        now,
		ParentHash:       parent.Hash,
		ForkSourceBranch: normalizeBranchName(parent.Branch),
		ForkFromEventID:  parent.EventID,
		ForkOriginAgent:  originAgent,
		Provenance:       acf.Provenance{DeviceID: deviceID, SourceAgent: "codex"},
	}))
	events, err := store.ReadEvents(acf.KindConversation, artID)
	require.NoError(t, err)
	return events[len(events)-1]
}

func appendConversationBranchUpdate(t *testing.T, store *acf.Store, deviceID, artID, branch string, forkParent acf.Event, text string) acf.Event {
	t.Helper()
	fork := appendConversationFork(t, store, deviceID, artID, branch, forkParent)
	return appendConversationBranchUpdateFromHead(t, store, deviceID, artID, branch, fork, text)
}

func appendConversationBranchUpdateFromHead(t *testing.T, store *acf.Store, deviceID, artID, branch string, parent acf.Event, text string) acf.Event {
	t.Helper()
	now := time.Now().UTC()
	payload, err := acf.EncodePayload(acf.ConversationPayload{
		Format: acf.ConversationDeltaFormatV1,
		Events: []acf.ConversationEvent{syncTestConversationTurn("user", text)},
	})
	require.NoError(t, err)
	require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
		EventID:    acf.NewID(),
		ArtifactID: artID,
		Type:       acf.EventTypeUpdate,
		Branch:     branch,
		Timestamp:  now,
		ParentHash: parent.Hash,
		Provenance: acf.Provenance{DeviceID: deviceID, SourceAgent: "codex"},
		Payload:    payload,
	}))
	events, err := store.ReadEvents(acf.KindConversation, artID)
	require.NoError(t, err)
	return events[len(events)-1]
}

// laneEvents filters the captured outbound events by lane.
func laneEvents(pub *stubRemotePublisher, lane string) []OutboundEvent {
	pub.mu.Lock()
	defer pub.mu.Unlock()
	var out []OutboundEvent
	for _, e := range pub.events {
		if e.Lane == lane {
			out = append(out, e)
		}
	}
	return out
}

// TestForwardCommitted_PublishesEncrypted verifies a locally-authored committed
// event is sealed into a ciphertext envelope (no plaintext leak) and forwarded.
// Non-conversation kinds publish exactly ONE lane=live full-state event — the
// two-lane split (aligned-chains design rule 5) applies to conversations only.
func TestForwardCommitted_PublishesEncrypted(t *testing.T) {
	pub := &stubRemotePublisher{}
	local := newTestDevice(t, "this-device")
	o, store := newStoreOrch(t, pub, local)
	id, ev := seedArtifact(t, store, acf.KindMemory, "this-device")

	o.forwardCommitted(id)

	require.Equal(t, 1, pub.Count(), "memory artifacts publish exactly one event")
	pub.mu.Lock()
	defer pub.mu.Unlock()
	got := pub.events[0]
	require.Equal(t, id, got.ArtifactID)
	require.Equal(t, ev.EventID, got.EventID)
	require.Equal(t, "memory", got.Kind)
	require.Equal(t, string(acf.EventTypeCreate), got.Type)
	require.Equal(t, acf.MainBranch, got.BranchID)
	require.Equal(t, uint64(1), got.Sequence)
	require.Equal(t, "this-device", got.Origin)
	require.Equal(t, ev.Provenance.SourceAgent, got.SourceAgent)
	require.Equal(t, LaneLive, got.Lane, "non-conversation kinds stay single-lane live")

	// ZERO-KNOWLEDGE: the bytes must be a ciphertext envelope (valid JSON,
	// no plaintext "hello" marker), decryptable only with the device key.
	require.True(t, json.Valid(got.Bytes), "bytes must be a JSON envelope")
	require.NotContains(t, string(got.Bytes), "hello", "outbound bytes leak plaintext")
	decoded, _, _, err := openEnvelope(got.Bytes, local.id, local.priv)
	require.NoError(t, err)
	require.Equal(t, ev.EventID, decoded.EventID)
}

func TestForwardCommitted_InactiveProjectCannotPublish(t *testing.T) {
	pub := &stubRemotePublisher{}
	local := newTestDevice(t, "this-device")
	o, store := newStoreOrch(t, pub, local)
	id, _ := seedArtifact(t, store, acf.KindMemory, "this-device")
	artifact, err := store.ReadArtifact(acf.KindMemory, id)
	require.NoError(t, err)
	artifact.Scope = acf.ScopeProject
	artifact.Project = &project.ProjectInfo{ID: "inactive-project", Path: "/untrusted/wire/path", VCS: "none"}
	require.NoError(t, store.WriteArtifact(artifact))

	registry, err := project.NewRegistry(filepath.Join(realTempDir(t), "projects.json"))
	require.NoError(t, err)
	missing := filepath.Join(realTempDir(t), "missing-project")
	require.NoError(t, registry.Add(project.Entry{ID: "inactive-project", Path: missing, VCS: "none", Inactive: true}))
	o.cfg.ProjectRegistry = registry

	require.False(t, o.forwardCommitted(id))
	require.Zero(t, pub.Count(), "inactive project authority must be rejected before remote publication")
}

// TestForwardCommitted_UsesCloudOriginOverNativeProvenance covers the paired
// daemon path where adapters may still stamp host-native provenance (for
// example "test-host.localdomain"). The relay origin must be the cloud device id
// so self-echoes do not later teach the loop guard that the local hostname is a
// remote device.
func TestForwardCommitted_UsesCloudOriginOverNativeProvenance(t *testing.T) {
	pub := &stubRemotePublisher{}
	local := newTestDevice(t, "cloud-device")
	o, store := newStoreOrch(t, pub, local)
	id, _ := seedArtifact(t, store, acf.KindMemory, "test-host.localdomain")

	o.forwardCommitted(id)

	require.Equal(t, 1, pub.Count())
	pub.mu.Lock()
	defer pub.mu.Unlock()
	require.Equal(t, "cloud-device", pub.events[0].Origin)
}

// TestForwardCommitted_MaterializesConversationDeltaForRemote: committing a
// conversation delta head publishes TWO events (aligned-chains design rule 5).
// This test pins the lane=retained half — the envelope still carries a FULL
// self-contained conversation payload (this test's original guarantee), now
// stamped with the AlignedHead/AlignedEventID a receiver adopts a baseline
// from. The lane=live half is pinned by
// TestForwardCommitted_ConversationDeltaLiveLaneVerbatim.
func TestForwardCommitted_MaterializesConversationDeltaForRemote(t *testing.T) {
	pub := &stubRemotePublisher{}
	local := newTestDevice(t, "this-device")
	o, store := newStoreOrch(t, pub, local)
	id, _, update := seedConversationWithDelta(t, store, local.id)

	require.True(t, o.forwardCommitted(id))

	require.Equal(t, 2, pub.Count(), "a conversation delta head publishes lane=live + lane=retained")
	retained := laneEvents(pub, LaneRetained)
	require.Len(t, retained, 1)
	got := retained[0]
	require.Equal(t, id, got.ArtifactID)
	require.Equal(t, RetainedWireEventID(update.EventID, local.id), got.EventID,
		"retained wire EventID must be the origin-scoped head+-r-<dev8> — DISTINCT from the live lane so the outbox persists both")
	require.Equal(t, update.ParentHash, got.ParentHash)
	require.Equal(t, update.Hash, got.CheckpointAlignmentHash,
		"checkpoint alignment must come from the retained event's signed Canonical.AlignedHead")
	require.NotEqual(t, got.ParentHash, got.CheckpointAlignmentHash,
		"canonical predecessor and covered checkpoint head are independent")
	require.Equal(t, "conversation", got.Kind)

	decoded, _, _, err := openEnvelope(got.Bytes, local.id, local.priv)
	require.NoError(t, err)
	require.Equal(t, update.EventID, decoded.EventID,
		"the SEALED body keeps the head's real EventID; only the wire/outbox id is suffixed")
	require.Equal(t, update.Hash, decoded.AlignedHead,
		"retained event must name the origin head hash for baseline adoption")
	require.Equal(t, update.EventID, decoded.AlignedEventID,
		"retained event must name the origin head event id for the re-align tiebreak")
	var payload acf.ConversationPayload
	require.NoError(t, json.Unmarshal(decoded.Payload, &payload))
	require.Equal(t, acf.ConversationFormatV1, payload.Format,
		"retained envelope must carry a full conversation payload, not a bare delta")
	require.Equal(t, []acf.TextTurn{
		{Role: "user", Text: "What is the Capital of China?"},
		{Role: "assistant", Text: "The capital of China is **Beijing**."},
	}, acf.ExtractTextTurns(payload.Events))
}

// TestForwardCommitted_ConversationDeltaLiveLaneVerbatim: the lane=live event
// must be the stored head event VERBATIM — the compact original
// ConversationDeltaFormatV1 delta — so a receiver whose head bookkeeping
// matches ParentHash can append it natively and, by acf.ComputeHash
// determinism, recompute the identical hash (chains stay aligned).
func TestForwardCommitted_ConversationDeltaLiveLaneVerbatim(t *testing.T) {
	pub := &stubRemotePublisher{}
	local := newTestDevice(t, "this-device")
	o, store := newStoreOrch(t, pub, local)
	id, _, update := seedConversationWithDelta(t, store, local.id)

	require.True(t, o.forwardCommitted(id))

	live := laneEvents(pub, LaneLive)
	require.Len(t, live, 1)
	got := live[0]
	require.Equal(t, id, got.ArtifactID)
	require.Equal(t, update.EventID, got.EventID)
	require.Equal(t, update.ParentHash, got.ParentHash)
	require.Empty(t, got.CheckpointAlignmentHash, "live deltas never carry checkpoint alignment")
	require.Less(t, len(got.Bytes), 64*1024,
		"a one-turn delta must stay small on the live lane regardless of history size")

	decoded, _, _, err := openEnvelope(got.Bytes, local.id, local.priv)
	require.NoError(t, err)
	require.Equal(t, update, decoded, "live lane must carry the stored head event VERBATIM")
	var payload acf.ConversationPayload
	require.NoError(t, json.Unmarshal(decoded.Payload, &payload))
	require.Equal(t, acf.ConversationDeltaFormatV1, payload.Format,
		"live lane must preserve the delta format, never materialize")
}

func TestForwardCommitted_NonMainConversationPublishesBranchScopedCheckpoint(t *testing.T) {
	pub := &stubRemotePublisher{}
	local := newTestDevice(t, "this-device")
	o, store := newStoreOrch(t, pub, local)
	id, create := seedConversation(t, store, local.id, turnEv("user", "root", time.Now().UTC()))
	update := appendConversationBranchUpdate(t, store, local.id, id, "review", create, "branch turn")

	require.True(t, o.forwardCommitted(id))

	live := laneEvents(pub, LaneLive)
	require.Len(t, live, 1)
	got := live[0]
	require.Equal(t, "review", got.BranchID)
	require.Equal(t, update.EventID, got.EventID)
	decoded, _, _, err := openEnvelope(got.Bytes, local.id, local.priv)
	require.NoError(t, err)
	require.Equal(t, update.EventID, decoded.EventID)
	require.Equal(t, "review", decoded.Branch)

	retained := laneEvents(pub, LaneRetained)
	require.Len(t, retained, 1, "every conversation branch needs its own recoverable checkpoint")
	require.Equal(t, "review", retained[0].BranchID)
	checkpoint, _, _, err := openEnvelope(retained[0].Bytes, local.id, local.priv)
	require.NoError(t, err)
	require.Equal(t, "review", checkpoint.Branch)
	require.Equal(t, update.Hash, checkpoint.AlignedHead)
	var payload acf.ConversationPayload
	require.NoError(t, json.Unmarshal(checkpoint.Payload, &payload))
	require.Equal(t, []acf.TextTurn{{Role: "user", Text: "root"}, {Role: "user", Text: "branch turn"}}, acf.ExtractTextTurns(payload.Events),
		"the branch checkpoint must contain only its projected ancestry, never unrelated main history")
}

func TestEndToEnd_NonMainConversationLiveBranchRoundTrips(t *testing.T) {
	devA := newTestDevice(t, "device-A")
	devB := newTestDevice(t, "device-B")
	pubA, pubB := &stubRemotePublisher{}, &stubRemotePublisher{}
	oA, storeA := newStoreOrch(t, pubA, devA, Recipient{DeviceID: devB.id, PubKey: devB.pub})
	oB, storeB := newStoreOrch(t, pubB, devB, Recipient{DeviceID: devA.id, PubKey: devA.pub})

	t0 := time.Now().UTC()
	artID, create := seedConversation(t, storeA, devA.id, turnEv("user", "root", t0))
	require.True(t, oA.forwardCommitted(artID))
	liveCursor := 0
	require.Equal(t, []ImportOutcome{ImportApplied}, importWires(t, oB, takeLane(pubA, LaneLive, &liveCursor)))

	fork := appendConversationFork(t, storeA, devA.id, artID, "review", create)
	require.True(t, oA.forwardCommitted(artID))
	require.Equal(t, []ImportOutcome{ImportApplied}, importWires(t, oB, takeLane(pubA, LaneLive, &liveCursor)))

	update := appendConversationBranchUpdateFromHead(t, storeA, devA.id, artID, "review", fork, "branch turn")
	require.True(t, oA.forwardCommitted(artID))
	require.Equal(t, []ImportOutcome{ImportApplied}, importWires(t, oB, takeLane(pubA, LaneLive, &liveCursor)))

	projected, err := storeB.ProjectEventsForBranch(acf.KindConversation, artID, "review", acf.BranchProjectionOpts{})
	require.NoError(t, err)
	require.Len(t, projected, 3)
	require.Equal(t, []string{create.EventID, fork.EventID, update.EventID}, []string{
		projected[0].EventID,
		projected[1].EventID,
		projected[2].EventID,
	})
	payload, _, ok, err := storeB.ProjectConversationPayloadForBranch(artID, "review", acf.BranchProjectionOpts{})
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, []acf.TextTurn{
		{Role: "user", Text: "root"},
		{Role: "user", Text: "branch turn"},
	}, acf.ExtractTextTurns(payload.Events))
	require.Equal(t, 0, pubB.Count(), "receiver must not bounce remote branch events back out")
}

func TestRemoteRepublishBookkeepingIsBranchScoped(t *testing.T) {
	o := &Orchestrator{}
	const (
		artifactID = "019f0000-0000-7000-8000-00000000beef"
		fp         = "device-a\ndevice-b"
	)

	o.markRepublishedLocalRemoteHead(artifactID, acf.MainBranch, "hash-main", fp)
	require.False(t, o.shouldRepublishLocalRemoteHead(artifactID, acf.MainBranch, "hash-main", fp))
	require.True(t, o.shouldRepublishLocalRemoteHead(artifactID, "review", "hash-review", fp),
		"a published main head must not suppress a branch head")

	o = &Orchestrator{}
	o.markRetainedOversized(artifactID, "review", "hash-review")
	require.False(t, o.shouldBackfillLocalRemoteHead(artifactID, "review", "hash-review", fp))
	require.True(t, o.shouldBackfillLocalRemoteHead(artifactID, acf.MainBranch, "hash-main", fp),
		"a branch oversized marker must not block main")
	o.clearRetainedOversized(artifactID, "review")
	require.True(t, o.shouldBackfillLocalRemoteHead(artifactID, "review", "hash-review", fp))

	o = &Orchestrator{}
	o.markBackfillAttempted(artifactID, "review")
	require.False(t, o.shouldBackfillLocalRemoteHead(artifactID, "review", "hash-review", fp))
	require.True(t, o.shouldBackfillLocalRemoteHead(artifactID, acf.MainBranch, "hash-main", fp),
		"a branch backfill attempt must not burn main")
	o.clearBackfillAttempted(artifactID, "review")
	require.True(t, o.shouldBackfillLocalRemoteHead(artifactID, "review", "hash-review", fp))
}

// TestForwardCommitted_OversizedLiveSkipsToRetainedOnly: a conversation head
// whose SEALED size exceeds the live-lane cap publishes ONLY the retained
// event — the daemon would just dead-letter an over-cap live event, and a
// retained state that still fits the transport budget carries the full state.
func TestForwardCommitted_OversizedLiveSkipsToRetainedOnly(t *testing.T) {
	restore := remotePublishLiveMaxBytes
	remotePublishLiveMaxBytes = 1 << 10
	t.Cleanup(func() { remotePublishLiveMaxBytes = restore })

	pub := &stubRemotePublisher{}
	local := newTestDevice(t, "this-device")
	o, store := newStoreOrch(t, pub, local)
	// A single-turn create whose payload (~9 KB, below the envelope gzip
	// threshold) seals well above the shrunken 1 KiB live cap.
	id, head := seedConversation(t, store, local.id,
		turnEv("user", incompressibleHexText(9<<10), time.Now().UTC()))

	require.True(t, o.forwardCommitted(id), "retained lane alone still counts as published")

	require.Equal(t, 1, pub.Count(), "over-cap live lane is skipped entirely; retained still ships")
	pub.mu.Lock()
	got := pub.events[0]
	pub.mu.Unlock()
	require.Equal(t, LaneRetained, got.Lane)
	require.Equal(t, RetainedWireEventID(head.EventID, local.id), got.EventID)

	decoded, _, _, err := openEnvelope(got.Bytes, local.id, local.priv)
	require.NoError(t, err)
	require.Equal(t, head.Hash, decoded.AlignedHead)
	require.Equal(t, head.EventID, decoded.AlignedEventID)
	var payload acf.ConversationPayload
	require.NoError(t, json.Unmarshal(decoded.Payload, &payload))
	require.Equal(t, acf.ConversationFormatV1, payload.Format)
}

func TestDeferLargeRetainedBaseline_RateLimitsSameRoster(t *testing.T) {
	restoreThreshold := largeMaterializeThreshold
	largeMaterializeThreshold = 1
	t.Cleanup(func() { largeMaterializeThreshold = restoreThreshold })

	source := filepath.Join(t.TempDir(), "large.jsonl")
	require.NoError(t, os.WriteFile(source, []byte("large"), 0o600))
	o := &Orchestrator{cfg: Config{Store: &acf.Store{Root: t.TempDir()}}}
	art := acf.Artifact{ArtifactID: acf.NewID(), Kind: acf.KindConversation, SourcePath: source}
	head := acf.Event{Type: acf.EventTypeUpdate, Branch: acf.MainBranch}

	require.False(t, o.deferLargeRetainedBaseline(art, head, "roster-a"), "first large baseline is reserved immediately")
	require.True(t, o.deferLargeRetainedBaseline(art, head, "roster-a"), "same roster is rate-limited")
	art.HeadEventHash = "new-head"
	require.True(t, o.deferLargeRetainedBaseline(art, head, "roster-a"), "new heads coalesce until the existing interval is due")
	require.Equal(t, "new-head", o.remoteLargeRetainedAttempts[remoteHeadKey(art.ArtifactID, head.Branch)].headHash,
		"the eventual retained retry must target the newest coalesced head")
	require.False(t, o.deferLargeRetainedBaseline(art, head, "roster-b"), "a changed recipient roster bypasses the interval")

	head.Type = acf.EventTypeRedaction
	require.False(t, o.deferLargeRetainedBaseline(art, head, "roster-b"), "redaction must clear the retained slot immediately")
}

func TestRepublishLocalRemoteHeads_DeferredLargeBaselineDoesNotSweepEveryMinute(t *testing.T) {
	restoreThreshold := largeMaterializeThreshold
	restoreInterval := largeRetainedBaselineMinInterval
	largeMaterializeThreshold = 1
	largeRetainedBaselineMinInterval = time.Hour
	t.Cleanup(func() {
		largeMaterializeThreshold = restoreThreshold
		largeRetainedBaselineMinInterval = restoreInterval
	})

	pub := &stubRemotePublisher{}
	local := newTestDevice(t, "this-device")
	o, store := newStoreOrch(t, pub, local)
	id, _, head := seedConversationWithDelta(t, store, local.id)
	source := filepath.Join(t.TempDir(), "large.jsonl")
	require.NoError(t, os.WriteFile(source, []byte("large"), 0o600))
	art, err := store.ReadArtifact(acf.KindConversation, id)
	require.NoError(t, err)
	art.SourcePath = source
	require.NoError(t, store.WriteArtifact(art))

	require.False(t, o.deferLargeRetainedBaseline(art, head, local.id), "reserve the in-flight retained attempt")
	require.True(t, o.forwardCommitted(id), "the committing path still publishes the compact live lane")
	require.Equal(t, 1, pub.Count(), "the deferred pass publishes only the compact live lane")

	n, err := o.RepublishLocalRemoteHeads(context.Background())
	require.NoError(t, err)
	require.Zero(t, n, "the same head must stay suppressed until the retained retry cadence is due")
	require.Equal(t, 1, pub.Count())
}

// A conversation whose SEALED retained baseline exceeds
// remotePublishRetainedMaxBytes has no transport path (the daemon would only
// dead-letter it — design rule 6's acknowledged residual). forwardCommitted
// must handle it HONESTLY: keep publishing the live lane, refuse the retained
// lane at the source, surface remote.outbound_oversized with
// retained_too_large=true (the status surface's "peers cannot baseline this
// conversation" signal), and mark the artifact so the sweep machinery does
// not spin on it (see the republish/backfill test below).
func TestForwardCommitted_OversizedRetainedSkipsLaneAndNotifies(t *testing.T) {
	restore := remotePublishRetainedMaxBytes
	remotePublishRetainedMaxBytes = 1 << 10
	t.Cleanup(func() { remotePublishRetainedMaxBytes = restore })

	pub := &stubRemotePublisher{}
	local := newTestDevice(t, "this-device")
	o, store := newStoreOrch(t, pub, local)
	bus := &capturingBus{}
	o.cfg.EventPublisher = bus

	// ~9 KB single-turn payload (below the envelope gzip threshold) seals
	// well above the shrunken 1 KiB retained cap; the live lane (4 MB cap)
	// still carries the verbatim head.
	id, head := seedConversation(t, store, local.id,
		turnEv("user", incompressibleHexText(9<<10), time.Now().UTC()))

	require.True(t, o.forwardCommitted(id), "the live lane must still publish")
	require.Len(t, laneEvents(pub, LaneLive), 1)
	require.Empty(t, laneEvents(pub, LaneRetained),
		"an over-cap retained seal must be refused at the source, not handed to the transport to dead-letter")

	body := bus.lastBody("remote.outbound_oversized")
	require.NotNil(t, body, "the oversized condition must surface on the bus")
	require.Equal(t, true, body["retained_too_large"],
		"the notify body must state that the RETAINED lane (the recovery baseline) is what is too large")
	require.Equal(t, id, body["artifact_id"])
	require.Equal(t, RetainedWireEventID(head.EventID, local.id), body["event_id"])
	require.Equal(t, remotePublishRetainedMaxBytes, body["limit"])
}

func TestForwardCommitted_OversizedRetainedUsesAuthenticatedStagedCapability(t *testing.T) {
	restore := remotePublishRetainedMaxBytes
	remotePublishRetainedMaxBytes = 1 << 10
	t.Cleanup(func() { remotePublishRetainedMaxBytes = restore })

	pub := &stubRemotePublisher{largeRetained: true}
	local := newTestDevice(t, "this-device")
	o, store := newStoreOrch(t, pub, local)
	bus := &capturingBus{}
	o.cfg.EventPublisher = bus
	id, _ := seedConversation(t, store, local.id,
		turnEv("user", incompressibleHexText(13<<10), time.Now().UTC()))

	require.True(t, o.forwardCommitted(id))
	require.Len(t, laneEvents(pub, LaneLive), 1)
	retained := laneEvents(pub, LaneRetained)
	require.Len(t, retained, 1)
	require.Greater(t, len(retained[0].Bytes), remotePublishRetainedMaxBytes)
	require.Nil(t, bus.lastBody("remote.outbound_oversized"))
}

func TestForwardCommitted_RetainedMaterializeFailureStaysRepublishEligible(t *testing.T) {
	restoreInterval := largeRetainedBaselineMinInterval
	largeRetainedBaselineMinInterval = time.Hour
	t.Cleanup(func() { largeRetainedBaselineMinInterval = restoreInterval })

	pub := &stubRemotePublisher{}
	local := newTestDevice(t, "this-device")
	o, store := newStoreOrch(t, pub, local)
	now := time.Now().UTC()
	artifactID := acf.NewID()
	require.NoError(t, store.WriteArtifact(acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion,
		ArtifactID:       artifactID,
		Kind:             acf.KindConversation,
		Scope:            acf.ScopeGlobal,
		CreatedAt:        now,
		UpdatedAt:        now,
	}))
	head := acf.Event{
		EventID:    acf.NewID(),
		ArtifactID: artifactID,
		Type:       acf.EventTypeCreate,
		Timestamp:  now,
		Provenance: acf.Provenance{DeviceID: local.id, SourceAgent: "codex"},
		Payload:    json.RawMessage(`{"format":"conversation.v1","events":"not-an-array"}`),
	}
	require.NoError(t, store.AppendEvent(acf.KindConversation, head))
	events, err := store.ReadEvents(acf.KindConversation, artifactID)
	require.NoError(t, err)
	head = events[0]

	require.True(t, o.forwardCommitted(artifactID), "the live lane may still publish even when retained materialization fails")
	require.Len(t, laneEvents(pub, LaneLive), 1)
	require.Empty(t, laneEvents(pub, LaneRetained))
	require.False(t,
		o.shouldRepublishLocalRemoteHead(artifactID, head.Branch, head.Hash, recipientsFingerprint([]recipient{{deviceID: local.id}})),
		"a failed retained baseline must not spin in every one-minute safety sweep")

	key := remoteHeadKey(artifactID, head.Branch)
	o.mu.Lock()
	attempt := o.remoteLargeRetainedAttempts[key]
	attempt.at = time.Now().Add(-2 * largeRetainedBaselineMinInterval)
	o.remoteLargeRetainedAttempts[key] = attempt
	o.mu.Unlock()
	require.True(t,
		o.shouldRepublishLocalRemoteHead(artifactID, head.Branch, head.Hash, recipientsFingerprint([]recipient{{deviceID: local.id}})),
		"the failed retained baseline remains eligible when its bounded retry cadence is due")
}

// The origin-side spin guard for the oversized-retained residual: while the
// head is unchanged, the republish sweep and the backfill trickle must SKIP a
// known-oversized artifact (each attempt re-materializes and re-seals
// hundreds of MB only to be refused again — e.g. on every roster change or
// reconnect). A head change re-arms the attempt: the state may have shrunk
// (redaction, compaction), and if it now fits the retained lane publishes and
// the mark clears.
func TestRepublishAndBackfill_SkipOversizedRetainedUntilHeadChange(t *testing.T) {
	restore := remotePublishRetainedMaxBytes
	remotePublishRetainedMaxBytes = 1 << 10
	t.Cleanup(func() { remotePublishRetainedMaxBytes = restore })

	pub := &stubRemotePublisher{}
	local := newTestDevice(t, "this-device")
	o, store := newStoreOrch(t, pub, local)

	t0 := time.Now().UTC().Add(-time.Minute)
	// The transcript must stay over the 1 KiB test cap AFTER the pre-seal
	// gzip pass (envelopeCompressThreshold is 4 KiB): a repetitive string
	// would compress under the cap and dissolve the oversized premise, so
	// seed high-entropy text that keep-only-if-smaller cannot shrink.
	id, _ := seedConversation(t, store, local.id,
		turnEv("user", incompressibleHexText(9<<10), t0))
	require.True(t, o.forwardCommitted(id))
	require.Empty(t, laneEvents(pub, LaneRetained))
	before := pub.Count()

	// A roster change normally forces a re-seal of an UNCHANGED head — but a
	// known-oversized head must be skipped, not re-refused on every pass.
	peer := newTestDevice(t, "peer-device")
	o.SetRecipientResolver(staticResolver{recipients: []Recipient{
		{DeviceID: local.id, PubKey: local.pub},
		{DeviceID: peer.id, PubKey: peer.pub},
	}})
	n, err := o.RepublishLocalRemoteHeads(context.Background())
	require.NoError(t, err)
	require.Zero(t, n, "the republish sweep must skip a head whose retained seal is known over-cap")
	require.Equal(t, before, pub.Count(), "no lane may republish for the skipped artifact")

	n, err = o.BackfillLocalRemoteHeads(context.Background())
	require.NoError(t, err)
	require.Zero(t, n, "the backfill trickle must skip it too")
	require.Equal(t, before, pub.Count())

	// Head change + the state now fits (cap restored): the commit attempts the
	// retained lane again and publishes it, clearing the mark…
	remotePublishRetainedMaxBytes = restore
	appendConversationDelta(t, store, local.id, id, "q2", t0.Add(2*time.Second))
	require.True(t, o.forwardCommitted(id))
	require.Len(t, laneEvents(pub, LaneRetained), 1,
		"a post-head-change commit must attempt the retained lane again")

	// …so the artifact stays eligible for roster-change re-seals, and the
	// earlier skip must NOT have burnt its once-per-run backfill attempt.
	peer2 := newTestDevice(t, "peer-2")
	o.SetRecipientResolver(staticResolver{recipients: []Recipient{
		{DeviceID: local.id, PubKey: local.pub},
		{DeviceID: peer2.id, PubKey: peer2.pub},
	}})
	n, err = o.BackfillLocalRemoteHeads(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, n, "the oversized skip must not burn the once-per-run backfill attempt")
	require.Len(t, laneEvents(pub, LaneRetained), 2)
}

// TestForwardCommitted_RedactionPublishesRetainedClear: a redaction leaves the
// artifact with no retainable state, but the broker's retained slot still
// serves the LAST pre-redaction full state to future subscribers — a newly-
// paired or long-offline device would adopt it and resurrect redacted content.
// The redaction commit must therefore publish a lane=retained CLEAR (empty
// Bytes, Clear=true — the plugin maps it to an MQTT retained-clear publish)
// alongside the verbatim live redaction event.
func TestForwardCommitted_RedactionPublishesRetainedClear(t *testing.T) {
	pub := &stubRemotePublisher{}
	local := newTestDevice(t, "this-device")
	o, store := newStoreOrch(t, pub, local)

	t0 := time.Now().UTC().Add(-time.Minute)
	artID, head := seedConversation(t, store, local.id, turnEv("user", "secret-q", t0))
	require.True(t, o.forwardCommitted(artID))
	require.Len(t, laneEvents(pub, LaneRetained), 1, "pre-redaction commit retains full state")

	red := acf.Event{
		EventID: acf.NewID(), ArtifactID: artID, Type: acf.EventTypeRedaction,
		Timestamp: t0.Add(time.Second), ParentHash: head.Hash,
		Provenance: acf.Provenance{DeviceID: local.id, SourceAgent: "claude-code"},
	}
	require.NoError(t, store.AppendEvent(acf.KindConversation, red))
	require.True(t, o.forwardCommitted(artID))

	live := laneEvents(pub, LaneLive)
	require.Equal(t, red.EventID, live[len(live)-1].EventID,
		"the redaction itself still propagates on the live lane")

	retained := laneEvents(pub, LaneRetained)
	require.Len(t, retained, 2, "the redaction commit must publish a retained-slot CLEAR")
	clear := retained[1]
	require.True(t, clear.Clear, "the retained slot must be cleared, not left serving pre-redaction state")
	require.Empty(t, clear.Bytes, "a clear carries no body (ids only — zero-knowledge)")
	require.Equal(t, RetainedWireEventID(red.EventID, local.id), clear.EventID)
	require.Equal(t, string(acf.EventTypeRedaction), clear.Type)
	require.Equal(t, artID, clear.ArtifactID)
}

// A conversation log that never carried a payload (nothing was ever retained)
// must NOT publish a clear — the clear is redaction-gated.
func TestForwardCommitted_NeverMaterializedConversationSkipsClear(t *testing.T) {
	pub := &stubRemotePublisher{}
	local := newTestDevice(t, "this-device")
	o, store := newStoreOrch(t, pub, local)

	artID := acf.NewID()
	now := time.Now().UTC()
	require.NoError(t, store.WriteArtifact(acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion, ArtifactID: artID,
		Kind: acf.KindConversation, Scope: acf.ScopeGlobal,
		Name: "conv", CreatedAt: now, UpdatedAt: now,
	}))
	// A payload-less amendment is skipped by the materializer: ok=false with
	// no redaction barrier in the log.
	require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
		EventID: acf.NewID(), ArtifactID: artID, Type: acf.EventTypeAmendment,
		Timestamp:  now,
		Provenance: acf.Provenance{DeviceID: local.id, SourceAgent: "claude-code"},
	}))

	require.True(t, o.forwardCommitted(artID), "the live lane still publishes")
	require.Empty(t, laneEvents(pub, LaneRetained),
		"never-materialized state has nothing retained, so nothing to clear")
}

func TestRepublishLocalRemoteHeads_MaterializesConversationDeltaForRemote(t *testing.T) {
	pub := &stubRemotePublisher{}
	local := newTestDevice(t, "this-device")
	o, store := newStoreOrch(t, pub, local)
	id, _, update := seedConversationWithDelta(t, store, local.id)

	n, err := o.RepublishLocalRemoteHeads(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.Equal(t, 2, pub.Count(), "republish goes through the same two-lane conversation path")
	require.Len(t, laneEvents(pub, LaneLive), 1, "verbatim delta republished on the live lane")

	retained := laneEvents(pub, LaneRetained)
	require.Len(t, retained, 1)
	got := retained[0]
	require.Equal(t, id, got.ArtifactID)
	require.Equal(t, RetainedWireEventID(update.EventID, local.id), got.EventID)

	decoded, _, _, err := openEnvelope(got.Bytes, local.id, local.priv)
	require.NoError(t, err)
	require.Equal(t, update.Hash, decoded.AlignedHead)
	require.Equal(t, update.EventID, decoded.AlignedEventID)
	var payload acf.ConversationPayload
	require.NoError(t, json.Unmarshal(decoded.Payload, &payload))
	require.Equal(t, acf.ConversationFormatV1, payload.Format)
	require.Equal(t, []acf.TextTurn{
		{Role: "user", Text: "What is the Capital of China?"},
		{Role: "assistant", Text: "The capital of China is **Beijing**."},
	}, acf.ExtractTextTurns(payload.Events))
}

func TestRepublishLocalRemoteHeads_SkipsPeerAuthoredHeadsAfterRestart(t *testing.T) {
	pub := &stubRemotePublisher{}
	local := newTestDevice(t, "this-device")
	o, store := newStoreOrch(t, pub, local)
	localID, _ := seedArtifact(t, store, acf.KindMemory, local.id)
	peerID, _ := seedArtifact(t, store, acf.KindMemory, "peer-device")
	unattributedID, _ := seedArtifact(t, store, acf.KindMemory, "")

	n, err := o.RepublishLocalRemoteHeads(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.Equal(t, 1, pub.Count())

	pub.mu.Lock()
	got := pub.events[0]
	pub.mu.Unlock()
	require.Equal(t, localID, got.ArtifactID)
	require.NotEqual(t, peerID, got.ArtifactID)
	require.NotEqual(t, unattributedID, got.ArtifactID)
}

func TestRepublishLocalRemoteHeads_RecoversLegacyLocalResolutionWithoutDeviceID(t *testing.T) {
	pub := &stubRemotePublisher{}
	local := newTestDevice(t, "this-device")
	o, store := newStoreOrch(t, pub, local)
	id, create := seedArtifact(t, store, acf.KindMemory, local.id)

	payload, err := json.Marshal(acf.MemoryPayload{Format: "markdown", Content: "resolved"})
	require.NoError(t, err)
	require.NoError(t, store.AppendEvent(acf.KindMemory, acf.Event{
		EventID:    acf.NewID(),
		ArtifactID: id,
		Type:       acf.EventTypeResolution,
		Timestamp:  time.Now().UTC(),
		Provenance: acf.Provenance{SourceAgent: "aplexica:web-resolve"},
		Payload:    payload,
		ParentHash: create.Hash,
	}))

	n, err := o.RepublishLocalRemoteHeads(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, n,
		"an unattributed legacy resolution extending this device's head must be repaired")
	require.Equal(t, 1, pub.Count())
}

func TestRepublishLocalRemoteHeads_SkipsLegacyPeerResolutionWithoutDeviceID(t *testing.T) {
	pub := &stubRemotePublisher{}
	local := newTestDevice(t, "this-device")
	o, store := newStoreOrch(t, pub, local)
	id, create := seedArtifact(t, store, acf.KindMemory, "peer-device")

	payload, err := json.Marshal(acf.MemoryPayload{Format: "markdown", Content: "resolved"})
	require.NoError(t, err)
	require.NoError(t, store.AppendEvent(acf.KindMemory, acf.Event{
		EventID:    acf.NewID(),
		ArtifactID: id,
		Type:       acf.EventTypeResolution,
		Timestamp:  time.Now().UTC(),
		Provenance: acf.Provenance{SourceAgent: "aplexica:web-resolve"},
		Payload:    payload,
		ParentHash: create.Hash,
	}))

	n, err := o.RepublishLocalRemoteHeads(context.Background())
	require.NoError(t, err)
	require.Zero(t, n,
		"an unattributed legacy resolution extending a peer head must remain ineligible")
	require.Zero(t, pub.Count())
}

func TestRepublishLocalRemoteHeads_SkipsOldHeads(t *testing.T) {
	pub := &stubRemotePublisher{}
	local := newTestDevice(t, "this-device")
	o, store := newStoreOrch(t, pub, local)
	oldID, old := seedArtifact(t, store, acf.KindMemory, local.id)
	recentID, _ := seedArtifact(t, store, acf.KindMemory, local.id)

	old.EventID = acf.NewID()
	old.Type = acf.EventTypeUpdate
	old.ParentHash = old.Hash
	old.Hash = ""
	old.Timestamp = time.Now().Add(-(remoteRepublishRecentWindow + time.Minute)).UTC()
	require.NoError(t, store.AppendEvent(acf.KindMemory, old))

	n, err := o.RepublishLocalRemoteHeads(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.Equal(t, 1, pub.Count())

	pub.mu.Lock()
	got := pub.events[0]
	pub.mu.Unlock()
	require.Equal(t, recentID, got.ArtifactID)
	require.NotEqual(t, oldID, got.ArtifactID)
}

func TestRepublishLocalRemoteHeads_OnlyPublishesChangedHeads(t *testing.T) {
	pub := &stubRemotePublisher{}
	local := newTestDevice(t, "this-device")
	o, store := newStoreOrch(t, pub, local)
	id, create := seedArtifact(t, store, acf.KindMemory, local.id)

	n, err := o.RepublishLocalRemoteHeads(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.Equal(t, 1, pub.Count())

	n, err = o.RepublishLocalRemoteHeads(context.Background())
	require.NoError(t, err)
	require.Equal(t, 0, n)
	require.Equal(t, 1, pub.Count(), "unchanged head should not be resent")

	payload, err := json.Marshal(acf.MemoryPayload{Format: "markdown", Content: "hello again"})
	require.NoError(t, err)
	update := acf.Event{
		EventID:    acf.NewID(),
		ArtifactID: id,
		Type:       acf.EventTypeUpdate,
		Timestamp:  time.Now().UTC(),
		Provenance: acf.Provenance{DeviceID: local.id, SourceAgent: "test"},
		Payload:    payload,
		ParentHash: create.Hash,
	}
	require.NoError(t, store.AppendEvent(acf.KindMemory, update))

	n, err = o.RepublishLocalRemoteHeads(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.Equal(t, 2, pub.Count(), "changed head should be resent")
}

func TestRepublishLocalRemoteHeads_SeedsExistingHeadsAsAlreadySent(t *testing.T) {
	pub := &stubRemotePublisher{}
	local := newTestDevice(t, "this-device")
	root := realTempDir(t)
	watched := filepath.Join(root, "proj")
	require.NoError(t, os.MkdirAll(watched, 0o755))
	adapters, store, _ := buildAllThreeAdapters(t, root)
	id, create := seedArtifact(t, store, acf.KindMemory, local.id)

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
	})
	require.NoError(t, err)
	defer o.Close()

	n, err := o.RepublishLocalRemoteHeads(context.Background())
	require.NoError(t, err)
	require.Equal(t, 0, n)
	require.Equal(t, 0, pub.Count(), "pre-existing heads must not replay after daemon restart")

	payload, err := json.Marshal(acf.MemoryPayload{Format: "markdown", Content: "hello again"})
	require.NoError(t, err)
	update := acf.Event{
		EventID:    acf.NewID(),
		ArtifactID: id,
		Type:       acf.EventTypeUpdate,
		Timestamp:  time.Now().UTC(),
		Provenance: acf.Provenance{DeviceID: local.id, SourceAgent: "test"},
		Payload:    payload,
		ParentHash: create.Hash,
	}
	require.NoError(t, store.AppendEvent(acf.KindMemory, update))

	n, err = o.RepublishLocalRemoteHeads(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.Equal(t, 1, pub.Count(), "a changed head still republishes")
}

func TestRepublishLocalRemoteHeads_RepairsSeededRecentConversationBaseline(t *testing.T) {
	pub := &stubRemotePublisher{}
	local := newTestDevice(t, "this-device")
	root := realTempDir(t)
	watched := filepath.Join(root, "proj")
	require.NoError(t, os.MkdirAll(watched, 0o755))
	adapters, store, _ := buildAllThreeAdapters(t, root)
	memoryID, _ := seedArtifact(t, store, acf.KindMemory, local.id)
	conversationID, _, update := seedConversationWithDelta(t, store, local.id)

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
	})
	require.NoError(t, err)
	defer o.Close()

	n, err := o.RepublishLocalRemoteHeads(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, n, "recent startup-seeded conversations get one fast retained repair publish")
	require.Equal(t, 2, pub.Count(), "conversation repair publishes live + retained")
	require.NotContains(t, publishedArtifactIDs(pub), memoryID, "non-conversation startup seeds must keep no-replay behavior")

	retained := laneEvents(pub, LaneRetained)
	require.Len(t, retained, 1)
	require.Equal(t, conversationID, retained[0].ArtifactID)
	require.Equal(t, RetainedWireEventID(update.EventID, local.id), retained[0].EventID)

	n, err = o.RepublishLocalRemoteHeads(context.Background())
	require.NoError(t, err)
	require.Equal(t, 0, n, "the repaired conversation dedups after the retained lane is handled")
	require.Equal(t, 2, pub.Count())
}

func TestRepublishLocalRemoteHeads_SkipsRevokedProjectBeforeReadingConversationLog(t *testing.T) {
	pub := &stubRemotePublisher{}
	local := newTestDevice(t, "this-device")
	root := realTempDir(t)
	watched := filepath.Join(root, "proj")
	require.NoError(t, os.MkdirAll(watched, 0o755))
	adapters, store, _ := buildAllThreeAdapters(t, root)
	conversationID, _, _ := seedConversationWithDelta(t, store, local.id)

	art, err := store.ReadArtifact(acf.KindConversation, conversationID)
	require.NoError(t, err)
	art.Scope = acf.ScopeProject
	art.Project = &project.ProjectInfo{ID: "revoked-project", Path: watched, VCS: "none"}
	require.NoError(t, store.WriteArtifact(art))

	registry, err := project.NewRegistry(filepath.Join(root, "projects.json"))
	require.NoError(t, err)
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
		ProjectRegistry:      registry,
	})
	require.NoError(t, err)
	defer o.Close()

	// Prove the authorization gate runs from artifact metadata, before the
	// startup-seeded conversation repair path opens or parses the append log.
	eventPath := filepath.Join(store.Root, "events", "conversations", conversationID+".jsonl")
	require.NoError(t, os.WriteFile(eventPath, []byte("damaged legacy history\n"), 0o600))

	n, err := o.RepublishLocalRemoteHeads(context.Background())
	require.NoError(t, err)
	require.Zero(t, n)
	require.Zero(t, pub.Count())
}

func TestRepublishLocalRemoteHeads_DedupesAlignedBaselineBookkeepingHead(t *testing.T) {
	pub := &stubRemotePublisher{}
	local := newTestDevice(t, "this-device")
	root := realTempDir(t)
	watched := filepath.Join(root, "proj")
	require.NoError(t, os.MkdirAll(watched, 0o755))
	adapters, store, _ := buildAllThreeAdapters(t, root)

	id := acf.NewID()
	now := time.Now().UTC()
	require.NoError(t, store.WriteArtifact(acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion,
		ArtifactID:       id,
		Kind:             acf.KindConversation,
		Scope:            acf.ScopeGlobal,
		CreatedAt:        now,
		UpdatedAt:        now,
	}))
	baseline := acf.Event{
		EventID:    acf.NewID(),
		ArtifactID: id,
		Type:       acf.EventTypeBaseline,
		Timestamp:  now,
		Provenance: acf.Provenance{DeviceID: local.id, SourceAgent: "codex"},
		Payload: encodeSyncTestConversationPayload(t, acf.ConversationFormatV1, []acf.ConversationEvent{
			syncTestConversationTurn("user", "aligned recovery"),
		}),
		AlignedHead:    "origin-aligned-head",
		AlignedEventID: acf.NewID(),
	}
	require.NoError(t, store.AdoptBaseline(acf.KindConversation, baseline))
	art, err := store.ReadArtifact(acf.KindConversation, id)
	require.NoError(t, err)
	last, ok, err := store.LastEvent(acf.KindConversation, id)
	require.NoError(t, err)
	require.True(t, ok)
	require.NotEqual(t, art.HeadEventHash, last.Hash, "fixture must exercise aligned bookkeeping rather than an ordinary append head")

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
	})
	require.NoError(t, err)
	defer o.Close()

	n, err := o.RepublishLocalRemoteHeads(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.Equal(t, 2, pub.Count(), "first repair publishes live and retained lanes")

	n, err = o.RepublishLocalRemoteHeads(context.Background())
	require.NoError(t, err)
	require.Zero(t, n, "the aligned metadata head must dedupe after the repair")
	require.Equal(t, 2, pub.Count(), "an unchanged aligned baseline must not enter a one-minute republish loop")
}

func TestRepublishLocalRemoteHeads_DefersLargeSeededConversationUntilChange(t *testing.T) {
	restore := remoteSeededConversationRepairMaxLogBytes
	remoteSeededConversationRepairMaxLogBytes = 1
	t.Cleanup(func() { remoteSeededConversationRepairMaxLogBytes = restore })

	pub := &stubRemotePublisher{}
	local := newTestDevice(t, "this-device")
	root := realTempDir(t)
	watched := filepath.Join(root, "proj")
	require.NoError(t, os.MkdirAll(watched, 0o755))
	adapters, store, _ := buildAllThreeAdapters(t, root)
	conversationID, _, update := seedConversationWithDelta(t, store, local.id)

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
	})
	require.NoError(t, err)
	defer o.Close()

	n, err := o.RepublishLocalRemoteHeads(context.Background())
	require.NoError(t, err)
	require.Zero(t, n, "an oversized unchanged startup seed must not be replayed by the one-minute repair sweep")
	require.Zero(t, pub.Count())

	appendConversationDelta(t, store, local.id, conversationID, "new turn", update.Timestamp.Add(time.Second))
	n, err = o.RepublishLocalRemoteHeads(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, n, "a real head change must publish regardless of the startup repair size gate")
}

// TestRepublishLocalRemoteHeads_RepublishesWhenRecipientSetChanges pins the
// roster-recovery re-seal: a retained conversation baseline sealed while the
// recipient roster was degraded (self-only resolver fallback) is useless to a
// peer — it can never decrypt it. When the roster RECOVERS or CHANGES, the
// republish pass must re-seal + re-publish the SAME head (unchanged hash) for
// the new recipient set; the dedup index therefore keys on the recipient-set
// fingerprint alongside the head hash. A baseline sealed self-only during a
// degraded roster must be resealed when a peer becomes available.
func TestRepublishLocalRemoteHeads_RepublishesWhenRecipientSetChanges(t *testing.T) {
	pub := &stubRemotePublisher{}
	local := newTestDevice(t, "this-device")
	// newStoreOrch wires a SELF-ONLY resolver: the degraded roster.
	o, store := newStoreOrch(t, pub, local)
	id, _, update := seedConversationWithDelta(t, store, local.id)

	n, err := o.RepublishLocalRemoteHeads(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.Equal(t, 2, pub.Count(), "live + retained sealed against the self-only roster")

	// Roster recovers: a peer device appears (same head hash on the artifact).
	peer := newTestDevice(t, "peer-device")
	o.SetRecipientResolver(staticResolver{recipients: []Recipient{
		{DeviceID: local.id, PubKey: local.pub},
		{DeviceID: peer.id, PubKey: peer.pub},
	}})

	n, err = o.RepublishLocalRemoteHeads(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, n,
		"an unchanged head last sealed for a different roster must republish when the recipient set changes")
	require.Equal(t, 4, pub.Count())

	// The re-published retained baseline must now open with the PEER's key.
	retained := laneEvents(pub, LaneRetained)
	require.Len(t, retained, 2)
	got := retained[1]
	require.Equal(t, id, got.ArtifactID)
	require.Equal(t, RetainedWireEventID(update.EventID, local.id), got.EventID)
	decoded, _, _, err := openEnvelope(got.Bytes, peer.id, peer.priv)
	require.NoError(t, err, "the re-sealed retained baseline must be decryptable by the recovered peer")
	require.Equal(t, update.Hash, decoded.AlignedHead)

	// A third pass with the SAME roster dedups again — no republish flood.
	n, err = o.RepublishLocalRemoteHeads(context.Background())
	require.NoError(t, err)
	require.Equal(t, 0, n, "same head + same roster must not resend")
	require.Equal(t, 4, pub.Count())
}

// seedArtifactAt is seedArtifact with an explicit event timestamp, so backfill
// tests can build artifacts older than the recent republish window.
func seedArtifactAt(t *testing.T, store *acf.Store, kind acf.Kind, provenanceDevice string, ts time.Time) string {
	t.Helper()
	id := acf.NewID()
	require.NoError(t, store.WriteArtifact(acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion,
		ArtifactID:       id,
		Kind:             kind,
		Scope:            acf.ScopeGlobal,
		CreatedAt:        ts,
		UpdatedAt:        ts,
	}))
	payload, _ := json.Marshal(acf.MemoryPayload{Format: "markdown", Content: "hello"})
	require.NoError(t, store.AppendEvent(kind, acf.Event{
		EventID:    acf.NewID(),
		ArtifactID: id,
		Type:       acf.EventTypeCreate,
		Timestamp:  ts,
		Provenance: acf.Provenance{DeviceID: provenanceDevice, SourceAgent: "test"},
		Payload:    payload,
	}))
	return id
}

// publishedArtifactIDs returns the artifact ids of all captured outbound
// events, in publish order.
func publishedArtifactIDs(pub *stubRemotePublisher) []string {
	pub.mu.Lock()
	defer pub.mu.Unlock()
	ids := make([]string, 0, len(pub.events))
	for _, e := range pub.events {
		ids = append(ids, e.ArtifactID)
	}
	return ids
}

// TestBackfillLocalRemoteHeads_PublishesNeverBaselinedOldestFirst pins the
// slow retained-baseline trickle: artifacts that predate the daemon run
// (startup-seeded dedup entries) and fall OUTSIDE the recent republish window
// are never touched by RepublishLocalRemoteHeads, so an old conversation may
// never get a retained baseline. BackfillLocalRemoteHeads
// must publish them oldest-first, capped per pass, skipping peer-authored
// heads, and never re-publish an artifact it already handled.
func TestBackfillLocalRemoteHeads_PublishesNeverBaselinedOldestFirst(t *testing.T) {
	restore := remoteRepublishBackfillMaxHeads
	remoteRepublishBackfillMaxHeads = 2
	t.Cleanup(func() { remoteRepublishBackfillMaxHeads = restore })

	pub := &stubRemotePublisher{}
	local := newTestDevice(t, "this-device")
	root := realTempDir(t)
	watched := filepath.Join(root, "proj")
	require.NoError(t, os.MkdirAll(watched, 0o755))
	adapters, store, _ := buildAllThreeAdapters(t, root)

	// Seed BEFORE the orchestrator exists (startup-seeded index entries), all
	// older than the recent republish window.
	base := time.Now().UTC()
	oldest := seedArtifactAt(t, store, acf.KindMemory, local.id, base.Add(-72*time.Hour))
	middle := seedArtifactAt(t, store, acf.KindMemory, local.id, base.Add(-48*time.Hour))
	newest := seedArtifactAt(t, store, acf.KindMemory, local.id, base.Add(-25*time.Hour))
	peer := seedArtifactAt(t, store, acf.KindMemory, "peer-device", base.Add(-70*time.Hour))
	unattributed := seedArtifactAt(t, store, acf.KindMemory, "", base.Add(-69*time.Hour))

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
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = o.Close() })

	// The existing newest-first recent sweep must stay untouched: seeded +
	// outside the window means it publishes nothing.
	n, err := o.RepublishLocalRemoteHeads(context.Background())
	require.NoError(t, err)
	require.Equal(t, 0, n)
	require.Equal(t, 0, pub.Count())

	// Backfill pass 1: the two OLDEST never-baselined local heads.
	n, err = o.BackfillLocalRemoteHeads(context.Background())
	require.NoError(t, err)
	require.Equal(t, 2, n)
	require.Equal(t, []string{oldest, middle}, publishedArtifactIDs(pub),
		"backfill must trickle oldest-first")

	// Backfill pass 2: the remaining local head; the peer-authored head is
	// never republished (it is not ours to baseline).
	n, err = o.BackfillLocalRemoteHeads(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.Equal(t, []string{oldest, middle, newest}, publishedArtifactIDs(pub))
	require.NotContains(t, publishedArtifactIDs(pub), peer)
	require.NotContains(t, publishedArtifactIDs(pub), unattributed)

	// Backfill pass 3: everything converged — nothing left to publish.
	n, err = o.BackfillLocalRemoteHeads(context.Background())
	require.NoError(t, err)
	require.Equal(t, 0, n)
	require.Equal(t, 3, pub.Count())
}

// TestBackfillLocalRemoteHeads_SkipsDegradedRosterWithoutBurningAttempts: a
// pass while the roster is unresolvable (nil/empty resolver) must be a clean
// no-op — publishing would only drop every candidate (never plaintext) AND
// burn their once-per-run attempt budget, permanently starving them of a
// baseline for this daemon run. Once the roster recovers the same artifacts
// must still backfill.
func TestBackfillLocalRemoteHeads_SkipsDegradedRosterWithoutBurningAttempts(t *testing.T) {
	pub := &stubRemotePublisher{}
	local := newTestDevice(t, "this-device")
	root := realTempDir(t)
	watched := filepath.Join(root, "proj")
	require.NoError(t, os.MkdirAll(watched, 0o755))
	adapters, store, _ := buildAllThreeAdapters(t, root)
	id := seedArtifactAt(t, store, acf.KindMemory, local.id, time.Now().UTC().Add(-48*time.Hour))

	o, err := NewOrchestrator(Config{
		Dir:                  watched,
		Adapters:             adapters,
		Store:                store,
		QuietPeriod:          50 * time.Millisecond,
		GuardWindow:          time.Second,
		RemoteEventPublisher: pub,
		LocalDeviceID:        local.id,
		// No RecipientResolver: the roster is unresolvable (degraded).
		DeviceKeyProvider: fixedKeyProvider{priv: local.priv},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = o.Close() })

	n, err := o.BackfillLocalRemoteHeads(context.Background())
	require.NoError(t, err)
	require.Equal(t, 0, n, "a degraded roster must skip the pass entirely")
	require.Equal(t, 0, pub.Count())

	// Roster recovers: the artifact must still be in line.
	o.SetRecipientResolver(staticResolver{recipients: []Recipient{{DeviceID: local.id, PubKey: local.pub}}})
	n, err = o.BackfillLocalRemoteHeads(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, n, "the degraded pass must not have burnt the artifact's attempt")
	require.Equal(t, []string{id}, publishedArtifactIDs(pub))
}

// TestBackfillLocalRemoteHeads_ReSealsWhenRosterChanges closes the long-tail
// half of the roster-recovery hole: an OLD artifact (outside the recent
// republish window, so RepublishLocalRemoteHeads never touches it) whose
// baseline was backfilled while the roster was degraded (self-only resolver
// fallback) must be re-sealed and re-published by a later backfill pass once
// the roster recovers — otherwise it stays peer-undecryptable until its next
// head change.
func TestBackfillLocalRemoteHeads_ReSealsWhenRosterChanges(t *testing.T) {
	pub := &stubRemotePublisher{}
	local := newTestDevice(t, "this-device")
	root := realTempDir(t)
	watched := filepath.Join(root, "proj")
	require.NoError(t, os.MkdirAll(watched, 0o755))
	adapters, store, _ := buildAllThreeAdapters(t, root)
	id := seedArtifactAt(t, store, acf.KindMemory, local.id, time.Now().UTC().Add(-48*time.Hour))

	o, err := NewOrchestrator(Config{
		Dir:                  watched,
		Adapters:             adapters,
		Store:                store,
		QuietPeriod:          50 * time.Millisecond,
		GuardWindow:          time.Second,
		RemoteEventPublisher: pub,
		LocalDeviceID:        local.id,
		// Degraded roster: the resolver's self-only fallback.
		RecipientResolver: staticResolver{recipients: []Recipient{{DeviceID: local.id, PubKey: local.pub}}},
		DeviceKeyProvider: fixedKeyProvider{priv: local.priv},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = o.Close() })

	n, err := o.BackfillLocalRemoteHeads(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, n, "backfill publishes the baseline sealed self-only")
	require.Equal(t, 1, pub.Count())

	// Roster recovers with a peer. The recent sweep cannot help — the head is
	// outside its window — so the backfill trickle must pick it up again.
	peer := newTestDevice(t, "peer-device")
	o.SetRecipientResolver(staticResolver{recipients: []Recipient{
		{DeviceID: local.id, PubKey: local.pub},
		{DeviceID: peer.id, PubKey: peer.pub},
	}})

	n, err = o.RepublishLocalRemoteHeads(context.Background())
	require.NoError(t, err)
	require.Equal(t, 0, n, "the recent sweep stays window-bounded (untouched)")

	n, err = o.BackfillLocalRemoteHeads(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, n, "backfill must re-seal an already-published head when the roster changed")
	require.Equal(t, 2, pub.Count())
	pub.mu.Lock()
	got := pub.events[1]
	pub.mu.Unlock()
	require.Equal(t, id, got.ArtifactID)
	_, _, _, err = openEnvelope(got.Bytes, peer.id, peer.priv)
	require.NoError(t, err, "the re-sealed baseline must be decryptable by the recovered peer")

	// Stable roster: nothing left to do.
	n, err = o.BackfillLocalRemoteHeads(context.Background())
	require.NoError(t, err)
	require.Equal(t, 0, n)
	require.Equal(t, 2, pub.Count())
}

// TestBackfillLocalRemoteHeads_DeclinedPublishDoesNotWedgeTheLine: an artifact
// whose publish is persistently declined (route.remote=exclude) is attempted
// AT MOST ONCE per daemon run — it must not occupy the head of the
// oldest-first line forever and starve every younger artifact behind it.
func TestBackfillLocalRemoteHeads_DeclinedPublishDoesNotWedgeTheLine(t *testing.T) {
	restore := remoteRepublishBackfillMaxHeads
	remoteRepublishBackfillMaxHeads = 1
	t.Cleanup(func() { remoteRepublishBackfillMaxHeads = restore })

	eng, err := syncrules.New([]syncrules.Rule{{
		Name:  "private-local",
		Match: syncrules.MatchSpec{Kind: syncrules.MatchKindAny, Tag: []string{"private"}},
		Route: syncrules.RouteSpec{Remote: "exclude", Agents: []string{"*"}},
	}})
	require.NoError(t, err)

	pub := &stubRemotePublisher{}
	local := newTestDevice(t, "this-device")
	root := realTempDir(t)
	watched := filepath.Join(root, "proj")
	require.NoError(t, os.MkdirAll(watched, 0o755))
	adapters, store, _ := buildAllThreeAdapters(t, root)

	base := time.Now().UTC()
	// The OLDEST artifact is remote-excluded; a younger one is allowed.
	excludedID := acf.NewID()
	require.NoError(t, store.WriteArtifact(acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion,
		ArtifactID:       excludedID,
		Kind:             acf.KindMemory,
		Scope:            acf.ScopeGlobal,
		Tags:             []string{"private"},
		CreatedAt:        base.Add(-72 * time.Hour),
		UpdatedAt:        base.Add(-72 * time.Hour),
	}))
	payload, _ := json.Marshal(acf.MemoryPayload{Format: "markdown", Content: "secret"})
	require.NoError(t, store.AppendEvent(acf.KindMemory, acf.Event{
		EventID:    acf.NewID(),
		ArtifactID: excludedID,
		Type:       acf.EventTypeCreate,
		Timestamp:  base.Add(-72 * time.Hour),
		Provenance: acf.Provenance{DeviceID: local.id, SourceAgent: "test"},
		Payload:    payload,
	}))
	allowedID := seedArtifactAt(t, store, acf.KindMemory, local.id, base.Add(-48*time.Hour))

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
		RulesEngine:          eng,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = o.Close() })

	// Pass 1 spends its single slot on the excluded artifact — declined.
	n, err := o.BackfillLocalRemoteHeads(context.Background())
	require.NoError(t, err)
	require.Equal(t, 0, n)
	require.Equal(t, 0, pub.Count())

	// Pass 2 must move PAST the declined artifact to the allowed one.
	n, err = o.BackfillLocalRemoteHeads(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, n, "a declined artifact must not wedge the oldest-first line")
	require.Equal(t, []string{allowedID}, publishedArtifactIDs(pub))

	// Pass 3: nothing left; the excluded artifact is not retried this run.
	n, err = o.BackfillLocalRemoteHeads(context.Background())
	require.NoError(t, err)
	require.Equal(t, 0, n)
	require.Equal(t, 1, pub.Count())
}

func TestMarkRemoteOrigin_IgnoresLocalCloudIdentity(t *testing.T) {
	local := newTestDevice(t, "this-device")
	o, _ := newStoreOrch(t, &stubRemotePublisher{}, local)

	o.markRemoteOrigin("this-device")

	require.False(t, o.isRemoteOrigin("this-device"))
}

// TestForwardCommitted_DropsWhenNoRecipients verifies the zero-knowledge gate:
// with no resolvable recipients, the outbound is DROPPED, never sent plaintext.
func TestForwardCommitted_DropsWhenNoRecipients(t *testing.T) {
	pub := &stubRemotePublisher{}
	local := newTestDevice(t, "this-device")
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
		RecipientResolver:    staticResolver{recipients: nil}, // empty
		DeviceKeyProvider:    fixedKeyProvider{priv: local.priv},
	})
	require.NoError(t, err)
	defer o.Close()
	id, _ := seedArtifact(t, store, acf.KindMemory, "this-device")

	o.forwardCommitted(id)
	require.Equal(t, 0, pub.Count(), "must DROP outbound when no recipients (never plaintext)")
}

// TestForwardCommitted_UsesPersistentArtifactCounter proves the outbound hot
// path does not count every newline in the event log. The extra physical line
// deliberately leaves artifact metadata unchanged; sequence must follow the
// authenticated append counter (1), not the raw file's two lines.
func TestForwardCommitted_UsesPersistentArtifactCounter(t *testing.T) {
	pub := &stubRemotePublisher{}
	local := newTestDevice(t, "this-device")
	o, store := newStoreOrch(t, pub, local)
	id, ev := seedArtifact(t, store, acf.KindMemory, local.id)

	b, err := json.Marshal(ev)
	require.NoError(t, err)
	eventsPath := filepath.Join(store.Root, "events", "memories", id+".jsonl")
	f, err := os.OpenFile(eventsPath, os.O_APPEND|os.O_WRONLY, 0)
	require.NoError(t, err)
	_, writeErr := f.Write(append(b, '\n'))
	require.NoError(t, writeErr)
	require.NoError(t, f.Close())

	require.True(t, o.forwardCommitted(id))
	pub.mu.Lock()
	require.Len(t, pub.events, 1)
	sequence := pub.events[0].Sequence
	pub.mu.Unlock()
	require.Equal(t, uint64(1), sequence)
}

// TestForwardCommitted_SkipsRemoteOrigin is the loop-prevention test: an event
// authored on a device the orchestrator has marked as a remote origin must NOT
// be forwarded back out.
func TestForwardCommitted_SkipsRemoteOrigin(t *testing.T) {
	pub := &stubRemotePublisher{}
	local := newTestDevice(t, "this-device")
	o, store := newStoreOrch(t, pub, local)
	o.markRemoteOrigin("peer-device")
	id, _ := seedArtifact(t, store, acf.KindMemory, "peer-device")

	o.forwardCommitted(id)

	require.Equal(t, 0, pub.Count(), "remote-origin event must not be re-published")
}

// TestEndToEnd_DeviceAtoB is the full cross-device E2E: device A seals an event
// for a recipient set including device B; device B's orchestrator imports it
// (decrypting with B's private key), lands it in B's store with A's origin
// preserved, and does NOT re-publish it (loop guard).
func TestEndToEnd_DeviceAtoB(t *testing.T) {
	devA := newTestDevice(t, "device-A")
	devB := newTestDevice(t, "device-B")

	// Device A: recipient set includes A and B.
	pubA := &stubRemotePublisher{}
	oA, storeA := newStoreOrch(t, pubA, devA, Recipient{DeviceID: devB.id, PubKey: devB.pub})
	id, evA := seedArtifact(t, storeA, acf.KindMemory, devA.id)
	oA.forwardCommitted(id)
	require.Equal(t, 1, pubA.Count())
	pubA.mu.Lock()
	out := pubA.events[0]
	pubA.mu.Unlock()

	// Convert the OutboundEvent into the wire RemoteEvent the plugin delivers.
	wire := proto.RemoteEvent{
		NamespaceID: out.NamespaceID,
		BranchID:    out.BranchID,
		ArtifactID:  out.ArtifactID,
		EventID:     out.EventID,
		ParentHash:  out.ParentHash,
		Kind:        out.Kind,
		Type:        out.Type,
		Timestamp:   out.Timestamp,
		Bytes:       out.Bytes,
		Sequence:    out.Sequence,
		Origin:      out.Origin,
	}

	// Device B: imports the inbound event, decrypting with B's key.
	pubB := &stubRemotePublisher{}
	oB, storeB := newStoreOrch(t, pubB, devB, Recipient{DeviceID: devA.id, PubKey: devA.pub})
	oB.ImportInbound([]proto.RemoteEvent{wire})

	stored, err := storeB.ReadEvents(acf.KindMemory, id)
	require.NoError(t, err)
	require.Len(t, stored, 1, "device B must have imported the event")
	require.Equal(t, evA.EventID, stored[0].EventID)
	require.Equal(t, devA.id, stored[0].Provenance.DeviceID, "origin device preserved across the relay")

	// Loop guard: B does not bounce A's event back out.
	require.True(t, oB.isRemoteOrigin(devA.id))
	oB.forwardCommitted(id)
	require.Equal(t, 0, pubB.Count(), "device B must not re-publish A's event")
}

// TestImportInbound_NonRecipientSkipped verifies an envelope sealed for OTHER
// devices (not this one) is skipped without error and does not land.
func TestImportInbound_NonRecipientSkipped(t *testing.T) {
	devOther := newTestDevice(t, "device-other")
	local := newTestDevice(t, "this-device")

	// Seal an event addressed ONLY to devOther.
	payload, _ := json.Marshal(acf.MemoryPayload{Format: "markdown", Content: "not for us"})
	artID := acf.NewID()
	ev := acf.Event{
		EventID:    acf.NewID(),
		ArtifactID: artID,
		Type:       acf.EventTypeCreate,
		Timestamp:  time.Now().UTC(),
		Provenance: acf.Provenance{DeviceID: "device-other"},
		Payload:    payload,
	}
	sealed, err := sealEnvelope(ev, acf.ScopeGlobal, nil, []recipient{{deviceID: devOther.id, pub: devOther.pub}})
	require.NoError(t, err)

	pub := &stubRemotePublisher{}
	o, store := newStoreOrch(t, pub, local)
	o.ImportInbound([]proto.RemoteEvent{{
		ArtifactID: artID,
		EventID:    ev.EventID,
		Kind:       string(acf.KindMemory),
		Type:       string(acf.EventTypeCreate),
		Timestamp:  ev.Timestamp,
		Bytes:      sealed,
		Origin:     "device-other",
	}})

	stored, err := store.ReadEvents(acf.KindMemory, artID)
	require.NoError(t, err)
	require.Len(t, stored, 0, "envelope not addressed to this device must be skipped")
}

// TestImportInbound_DedupsByEventID verifies redelivery of the same event id
// is idempotent (through the encrypted path).
func TestImportInbound_DedupsByEventID(t *testing.T) {
	local := newTestDevice(t, "this-device")
	peer := newTestDevice(t, "peer")
	pub := &stubRemotePublisher{}
	o, store := newStoreOrch(t, pub, local)

	artID := acf.NewID()
	payload, _ := json.Marshal(acf.MemoryPayload{Format: "markdown", Content: "x"})
	ev := acf.Event{
		EventID:    acf.NewID(),
		ArtifactID: artID,
		Type:       acf.EventTypeCreate,
		Timestamp:  time.Now().UTC(),
		Provenance: acf.Provenance{DeviceID: peer.id},
		Payload:    payload,
	}
	// Seal for the local device so it can decrypt.
	sealed, err := sealEnvelope(ev, acf.ScopeGlobal, nil, []recipient{{deviceID: local.id, pub: local.pub}})
	require.NoError(t, err)
	wire := proto.RemoteEvent{
		ArtifactID: artID,
		EventID:    ev.EventID,
		Kind:       string(acf.KindMemory),
		Type:       string(acf.EventTypeCreate),
		Timestamp:  ev.Timestamp,
		Bytes:      sealed,
		Origin:     peer.id,
	}

	o.ImportInbound([]proto.RemoteEvent{wire})
	o.ImportInbound([]proto.RemoteEvent{wire}) // redelivery

	// The reconnect fast path must prove the exact durable current identity is
	// enough to no-op before opening a large envelope. Corrupt bytes would fail
	// authentication if the implementation needlessly decrypted this duplicate.
	wire.Bytes = []byte("not-an-envelope")
	outcomes := o.ImportInboundResults([]proto.RemoteEvent{wire})
	require.Equal(t, []ImportOutcome{ImportDeduped}, outcomes,
		"current-head redelivery must dedup before decrypting its envelope")
	require.True(t, o.isRemoteOrigin(peer.id), "fast dedup must preserve the inbound loop guard")

	stored, err := store.ReadEvents(acf.KindMemory, artID)
	require.NoError(t, err)
	require.Len(t, stored, 1, "redelivered event must not double-append")
}

// TestSubscribeActiveNamespaces drives the namespace gate seam.
func TestSubscribeActiveNamespaces(t *testing.T) {
	sub := &stubSubscriber{}
	err := SubscribeActiveNamespaces(context.Background(), sub, []string{"ns-1", "", "ns-2"})
	require.NoError(t, err)
	require.Equal(t, []string{"ns-1", "ns-2"}, sub.subscribed, "empty namespace skipped")
}

type stubSubscriber struct {
	subscribed []string
}

func (s *stubSubscriber) Subscribe(_ context.Context, ns string) error {
	s.subscribed = append(s.subscribed, ns)
	return nil
}

// TestHandleEvent_ForwardsLocalEditOutbound is the end-to-end outbound test
// through the real watcher pipeline: editing a watched CLAUDE.md drives
// primaryImport -> fanOut -> forwardCommitted, so the publisher receives an
// ENCRYPTED OutboundEvent for the locally-committed memory artifact.
func TestHandleEvent_ForwardsLocalEditOutbound(t *testing.T) {
	root := realTempDir(t)
	watched := filepath.Join(root, "proj")
	require.NoError(t, os.MkdirAll(watched, 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(watched, ".git"), 0o755))
	adapters, store, _ := buildAllThreeAdapters(t, root)

	local := newTestDevice(t, "this-device")
	pub := &stubRemotePublisher{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	orch, err := NewOrchestrator(Config{
		Dir:                  watched,
		Adapters:             adapters,
		Store:                store,
		QuietPeriod:          100 * time.Millisecond,
		GuardWindow:          2 * time.Second,
		RemoteEventPublisher: pub,
		LocalDeviceID:        local.id,
		RecipientResolver:    staticResolver{recipients: []Recipient{{DeviceID: local.id, PubKey: local.pub}}},
		DeviceKeyProvider:    fixedKeyProvider{priv: local.priv},
	})
	require.NoError(t, err)
	defer orch.Close()

	go orch.Run(ctx)
	time.Sleep(150 * time.Millisecond)

	require.NoError(t, os.WriteFile(filepath.Join(watched, "CLAUDE.md"),
		[]byte("# local edit SENTINELXYZ\n"), 0o644))

	require.Eventually(t, func() bool {
		return pub.Count() >= 1
	}, 5*time.Second, 50*time.Millisecond, "outbound event never published for local edit")

	pub.mu.Lock()
	defer pub.mu.Unlock()
	got := pub.events[0]
	require.Equal(t, "memory", got.Kind)
	require.NotEmpty(t, got.ArtifactID)
	require.NotEmpty(t, got.EventID)
	require.NotEmpty(t, got.Bytes)
	// Ciphertext: the unique plaintext sentinel must not appear in the bytes.
	require.NotContains(t, string(got.Bytes), "SENTINELXYZ", "outbound bytes leak plaintext")
	dec, _, _, err := openEnvelope(got.Bytes, local.id, local.priv)
	require.NoError(t, err)
	require.Equal(t, got.EventID, dec.EventID)
}

type blockingConversationSessionAdapter struct {
	adapter.Adapter
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blockingConversationSessionAdapter) MaterializeConversationSession(acf.Artifact, acf.Event, string) (string, bool, error) {
	b.once.Do(func() { close(b.entered) })
	<-b.release
	return "", false, nil
}

func TestHandleEvent_ForwardsConversationBeforeSessionMaterialization(t *testing.T) {
	root := realTempDir(t)
	watched := filepath.Join(root, "proj")
	require.NoError(t, os.MkdirAll(watched, 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(watched, ".git"), 0o755))

	store := &acf.Store{Root: filepath.Join(root, "store")}
	require.NoError(t, store.Init())
	ss := &secrets.Store{Root: filepath.Join(root, "secrets")}
	require.NoError(t, ss.Init())

	cc := claudecode.New()
	cc.HomeDir = root
	cc.SecretsStore = ss

	cx := codex.New()
	cx.HomeDir = root
	cx.SecretsStore = ss
	blockingTarget := &blockingConversationSessionAdapter{
		Adapter: cx,
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	releaseBlocker := func() {
		select {
		case <-blockingTarget.release:
		default:
			close(blockingTarget.release)
		}
	}

	local := newTestDevice(t, "this-device")
	pub := &stubRemotePublisher{}
	ctx, cancel := context.WithCancel(context.Background())

	orch, err := NewOrchestrator(Config{
		Dir:                  watched,
		Adapters:             []adapter.Adapter{cc, blockingTarget},
		Store:                store,
		QuietPeriod:          100 * time.Millisecond,
		GuardWindow:          2 * time.Second,
		RemoteEventPublisher: pub,
		LocalDeviceID:        local.id,
		RecipientResolver:    staticResolver{recipients: []Recipient{{DeviceID: local.id, PubKey: local.pub}}},
		DeviceKeyProvider:    fixedKeyProvider{priv: local.priv},
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		releaseBlocker()
		cancel()
		orch.Close()
	})

	go orch.Run(ctx)
	time.Sleep(150 * time.Millisecond)

	session := strings.Join([]string{
		`{"type":"summary","summary":"test conversation","leafUuid":"conv-1"}`,
		`{"type":"user","sessionId":"conv-1","uuid":"u1","timestamp":"2026-06-29T16:34:12Z","message":{"role":"user","content":[{"type":"text","text":"What is the capital of Germany?"}]}}`,
		`{"type":"assistant","sessionId":"conv-1","uuid":"a1","parentUuid":"u1","timestamp":"2026-06-29T16:34:13Z","message":{"role":"assistant","content":[{"type":"text","text":"Berlin."}]}}`,
		"",
	}, "\n")
	require.NoError(t, os.WriteFile(filepath.Join(watched, "session-conv-1.jsonl"), []byte(session), 0o644))

	require.Eventually(t, func() bool {
		select {
		case <-blockingTarget.entered:
			return true
		default:
			return false
		}
	}, 5*time.Second, 50*time.Millisecond, "conversation materializer was not reached")

	require.Eventually(t, func() bool {
		return pub.Count() >= 1
	}, 500*time.Millisecond, 25*time.Millisecond,
		"conversation outbound publish must happen before session materialization returns")

	pub.mu.Lock()
	got := pub.events[0]
	pub.mu.Unlock()
	require.Equal(t, "conversation", got.Kind)
	require.NotEmpty(t, got.EventID)
	require.NotEmpty(t, got.Bytes)
	require.NotContains(t, string(got.Bytes), "Berlin", "outbound conversation bytes leak plaintext")
}

type capturingBus struct {
	mu     sync.Mutex
	kinds  []string
	bodies map[string][]any
}

func (c *capturingBus) Publish(kind string, body any) {
	c.mu.Lock()
	c.kinds = append(c.kinds, kind)
	if c.bodies == nil {
		c.bodies = map[string][]any{}
	}
	c.bodies[kind] = append(c.bodies[kind], body)
	c.mu.Unlock()
}

// lastBody returns the most recent body published under kind (nil when none,
// or when the body is not the map shape the orchestrator publishes).
func (c *capturingBus) lastBody(kind string) map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	list := c.bodies[kind]
	if len(list) == 0 {
		return nil
	}
	m, _ := list[len(list)-1].(map[string]any)
	return m
}

func (c *capturingBus) has(kind string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, k := range c.kinds {
		if k == kind {
			return true
		}
	}
	return false
}

// A non-conversation redelivery older than the local head is dropped by the
// wall-clock regression guard — that drop must be VISIBLE on the event bus.
func TestRebaseInbound_RegressionSkipIsSurfaced(t *testing.T) {
	local := newTestDevice(t, "this-device")
	pub := &stubRemotePublisher{}
	o, store := newStoreOrch(t, pub, local)
	bus := &capturingBus{}
	o.cfg.EventPublisher = bus // construction-time field; safe before Run

	id, _ := seedArtifact(t, store, acf.KindMemory, "peer-device")

	payload, err := json.Marshal(acf.MemoryPayload{Format: "markdown", Content: "stale snapshot"})
	require.NoError(t, err)
	stale := acf.Event{
		EventID: acf.NewID(), ArtifactID: id, Type: acf.EventTypeUpdate,
		Timestamp:  time.Now().UTC().Add(-24 * time.Hour),
		ParentHash: "unknown-parent",
		Provenance: acf.Provenance{DeviceID: "peer-device"},
		Payload:    payload,
	}
	require.NoError(t, o.rebaseInbound(acf.KindMemory, stale))
	require.True(t, bus.has("remote.inbound_regression_skipped"))
}

// incompressibleText returns n bytes of deterministic high-entropy printable
// text (keccak-free: chained SHA-256, hex-encoded — ~50% entropy per byte,
// far above any gzip/zstd break-even) for tests whose premise requires a
// payload the pre-seal compression pass cannot shrink.
func incompressibleHexText(n int) string {
	var b strings.Builder
	sum := sha256.Sum256([]byte("aplexica-incompressible-seed"))
	for b.Len() < n {
		sum = sha256.Sum256(sum[:])
		b.WriteString(hex.EncodeToString(sum[:]))
	}
	return b.String()[:n]
}
