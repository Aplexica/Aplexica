package syncd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
	"github.com/aplexica/aplexica/internal/plugin/proto"
	"github.com/aplexica/aplexica/internal/syncgate"
	"github.com/stretchr/testify/require"
)

type rejectingFinalizeExportAdapter struct {
	adapter.Adapter
	calls int
}

func (a *rejectingFinalizeExportAdapter) Export(context.Context, *acf.Store, string, string) error {
	a.calls++
	return errors.New("injected native export failure")
}

type failOnceConversationTarget struct {
	fakeConvSource
	mu        sync.Mutex
	attempts  int
	successes int
}

func (f *failOnceConversationTarget) MaterializeConversationSession(art acf.Artifact, _ acf.Event, _ string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attempts++
	if f.attempts == 1 {
		return "", false, errors.New("injected first materialization failure")
	}
	f.successes++
	return filepath.Join("/tmp/fake-target", string(art.Kind), art.ArtifactID+".session"), true, nil
}

func (f *failOnceConversationTarget) counts() (attempts, successes int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.attempts, f.successes
}

// TestImportInbound_MaterializesNativeFile verifies that an inbound memory
// artifact is written to a local agent's native file AND does NOT produce an
// outbound publish (the imported event is remote-origin; the materialised
// native write is recursion-guard-suppressed, so it never bounces back).
func TestImportInbound_MaterializesNativeFile(t *testing.T) {
	root := realTempDir(t)
	watched := filepath.Join(root, "proj")
	require.NoError(t, os.MkdirAll(watched, 0o755))
	// VCS marker so a project-scoped memory materialises into the project dir.
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
		QuietPeriod:          80 * time.Millisecond,
		GuardWindow:          2 * time.Second,
		RemoteEventPublisher: pub,
		LocalDeviceID:        local.id,
		RecipientResolver:    staticResolver{recipients: []Recipient{{DeviceID: local.id, PubKey: local.pub}}},
		DeviceKeyProvider:    fixedKeyProvider{priv: local.priv},
	})
	require.NoError(t, err)
	defer orch.Close()

	// Run the watcher so the recursion guard's suppression of the materialised
	// write is exercised end-to-end (a guard-marked native write must not
	// re-import + re-publish).
	go orch.Run(ctx)
	time.Sleep(120 * time.Millisecond)

	// Build a memory event as a PEER (claude-code) authored it, project-scoped
	// to the watched dir, and seal it for THIS device.
	artID := acf.NewID()
	payload, _ := json.Marshal(acf.MemoryPayload{Format: "markdown", Content: "# synced from peer\n"})
	peerEvent := acf.Event{
		EventID:    acf.NewID(),
		ArtifactID: artID,
		Type:       acf.EventTypeCreate,
		Timestamp:  time.Now().UTC(),
		Provenance: acf.Provenance{DeviceID: "peer-device", SourceAgent: "claude-code"},
		Payload:    payload,
	}
	sealed, err := sealEnvelope(peerEvent, acf.ScopeGlobal, nil, []recipient{{deviceID: local.id, pub: local.pub}})
	require.NoError(t, err)

	orch.ImportInbound([]proto.RemoteEvent{{
		ArtifactID: artID,
		EventID:    peerEvent.EventID,
		Kind:       string(acf.KindMemory),
		Type:       string(acf.EventTypeCreate),
		Timestamp:  peerEvent.Timestamp,
		Bytes:      sealed,
		Origin:     "peer-device",
	}})

	// The artifact is global-scope, so it materialises to the cross-agent
	// global native locations under HomeDir (root): codex -> ~/.codex/AGENTS.md,
	// kilo -> ~/.config/kilo/AGENTS.md. We assert at least one native file
	// appears anywhere under root (other than the canonical store).
	require.Eventually(t, func() bool {
		return countNativeMemoryFiles(t, root) > 0
	}, 5*time.Second, 50*time.Millisecond, "inbound artifact never materialised to a native file")

	// Give any (incorrectly) un-suppressed re-import a chance to publish, then
	// assert NOTHING was published outbound: a materialised inbound artifact
	// must not bounce back to the relay.
	time.Sleep(500 * time.Millisecond)
	require.Equal(t, 0, pub.Count(), "materialised inbound artifact must not re-publish outbound")
}

func TestInboundCanonicalEvidenceRequiresExactWireAndCanonicalEvent(t *testing.T) {
	root := realTempDir(t)
	store := &acf.Store{Root: filepath.Join(root, "store")}
	require.NoError(t, store.Init())
	orch, err := NewOrchestrator(Config{Dir: root, Store: store})
	require.NoError(t, err)
	defer orch.Close()

	artifactID, stored := seedArtifact(t, store, acf.KindMemory, "peer-device")
	wire := proto.RemoteEvent{
		Kind: string(acf.KindMemory), ArtifactID: artifactID, EventID: stored.EventID,
		EventHash: stored.Hash, Lane: LaneLive,
	}
	evidence, err := orch.CanonicalEvidenceForInbound(wire)
	require.NoError(t, err)
	require.Equal(t, stored.EventID, evidence.EventID)
	require.Equal(t, stored.Hash, evidence.EventHash)
	require.NoError(t, orch.FinalizeInboundCanonicalEvidence(evidence))

	tamperedWire := wire
	tamperedWire.EventHash = strings.Repeat("0", 64)
	_, err = orch.CanonicalEvidenceForInbound(tamperedWire)
	require.Error(t, err)
	tamperedCanonical := evidence
	tamperedCanonical.EventHash = strings.Repeat("0", 64)
	require.Error(t, orch.FinalizeInboundCanonicalEvidence(tamperedCanonical))
}

func TestDedupedTerminalEvidenceRepairsCanonicalArtifactMetadata(t *testing.T) {
	root := realTempDir(t)
	store := &acf.Store{Root: filepath.Join(root, "store")}
	require.NoError(t, store.Init())
	orch, err := NewOrchestrator(Config{Dir: root, Store: store})
	require.NoError(t, err)
	defer orch.Close()

	artifactID, stored := seedArtifact(t, store, acf.KindMemory, "peer-device")
	artifact, err := store.ReadArtifact(acf.KindMemory, artifactID)
	require.NoError(t, err)
	artifact.HeadEventHash = ""
	artifact.BranchHeads = nil
	artifact.EventCount = 0
	require.NoError(t, store.WriteArtifact(artifact), "model crash after visible event bytes but before append metadata")

	wire := proto.RemoteEvent{
		Kind: string(acf.KindMemory), ArtifactID: artifactID, EventID: stored.EventID,
		EventHash: stored.Hash, Lane: LaneLive,
	}
	evidence, err := orch.CanonicalEvidenceForTerminalInbound(wire, ImportDeduped)
	require.NoError(t, err)
	require.Equal(t, proto.InboundFinalizeCanonicalMaterialize, evidence.FinalizeKind)
	require.Equal(t, stored.EventID, evidence.EventID)
	repaired, err := store.ReadArtifact(acf.KindMemory, artifactID)
	require.NoError(t, err)
	require.Equal(t, stored.Hash, repaired.HeadEventHash)
	require.Equal(t, stored.Hash, repaired.BranchHeads[acf.MainBranch])
	require.Equal(t, uint64(1), repaired.EventCount)
}

func TestFinalizeInboundCanonicalEvidenceRetriesWhenEligibleNativeExportFails(t *testing.T) {
	root := realTempDir(t)
	adapters, store, _ := buildAllThreeAdapters(t, root)
	rejecting := &rejectingFinalizeExportAdapter{Adapter: adapters[0]}
	orch, err := NewOrchestrator(Config{Dir: root, Store: store, Adapters: []adapter.Adapter{rejecting}})
	require.NoError(t, err)
	defer orch.Close()

	artifactID, stored := seedArtifact(t, store, acf.KindMemory, "peer-device")
	err = orch.FinalizeInboundCanonicalEvidence(InboundCanonicalEvidence{
		FinalizeKind: proto.InboundFinalizeCanonicalMaterialize,
		Kind:         acf.KindMemory, ArtifactID: artifactID, EventID: stored.EventID, EventHash: stored.Hash,
	})
	require.ErrorIs(t, err, ErrInboundNativeMaterialization)
	require.Equal(t, 1, rejecting.calls, "strict finalize must attempt the eligible export exactly once per request")
}

func TestTerminalNonConversationConflictUsesExactSurvivingCanonicalHeadEvidence(t *testing.T) {
	root := realTempDir(t)
	store := &acf.Store{Root: filepath.Join(root, "store")}
	require.NoError(t, store.Init())
	orch, err := NewOrchestrator(Config{Dir: root, Store: store})
	require.NoError(t, err)
	defer orch.Close()

	artifactID, ancestor := seedArtifact(t, store, acf.KindMemory, "local-device")
	localPayload, err := json.Marshal(acf.MemoryPayload{Format: "markdown", Content: "local sibling"})
	require.NoError(t, err)
	localSibling := acf.Event{
		EventID: acf.NewID(), ArtifactID: artifactID, Type: acf.EventTypeUpdate,
		Timestamp: time.Now().UTC(), ParentHash: ancestor.Hash,
		Provenance: acf.Provenance{DeviceID: "local-device", SourceAgent: "codex"}, Payload: localPayload,
	}
	require.NoError(t, store.AppendEvent(acf.KindMemory, localSibling))
	localHead, ok, err := store.LastEvent(acf.KindMemory, artifactID)
	require.NoError(t, err)
	require.True(t, ok)

	remotePayload, err := json.Marshal(acf.MemoryPayload{Format: "markdown", Content: "remote sibling"})
	require.NoError(t, err)
	remoteSibling := acf.Event{
		EventID: acf.NewID(), ArtifactID: artifactID, Type: acf.EventTypeUpdate,
		Timestamp: time.Now().UTC(), ParentHash: ancestor.Hash,
		Provenance: acf.Provenance{DeviceID: "peer-device", SourceAgent: "claude-code"}, Payload: remotePayload,
	}
	remoteSibling.Hash, err = acf.ComputeHash(remoteSibling)
	require.NoError(t, err)
	require.Error(t, orch.recordInboundConflictWithDurability(acf.KindMemory, remoteSibling, true),
		"durable consumption must not discard a genuine sibling when no conflict sidecar is available")
	require.NoError(t, orch.recordInboundConflict(acf.KindMemory, remoteSibling))
	exists, err := store.HasEventID(acf.KindMemory, artifactID, remoteSibling.EventID)
	require.NoError(t, err)
	require.False(t, exists, "conflict recording must preserve, not append over, the surviving local chain")

	conflictingWire := proto.RemoteEvent{
		Kind: string(acf.KindMemory), ArtifactID: artifactID, EventID: remoteSibling.EventID,
		EventHash: remoteSibling.Hash, Lane: LaneLive,
	}
	_, err = orch.CanonicalEvidenceForInbound(conflictingWire)
	require.Error(t, err, "a non-appended sibling must not masquerade as the exact wire event")

	evidence, err := orch.CanonicalEvidenceForTerminalInbound(conflictingWire, ImportApplied)
	require.NoError(t, err)
	require.Equal(t, localHead.EventID, evidence.EventID)
	require.Equal(t, localHead.Hash, evidence.EventHash)
	require.NoError(t, orch.FinalizeInboundCanonicalEvidence(evidence))

	_, err = orch.CanonicalEvidenceForTerminalInbound(conflictingWire, ImportRetryable)
	require.Error(t, err, "a nonterminal outcome must never gain current-head evidence")
	conflictingWire.Kind = string(acf.KindConversation)
	_, err = orch.CanonicalEvidenceForTerminalInbound(conflictingWire, ImportApplied)
	require.Error(t, err, "conversation replay/redaction must retain strict event evidence")
}

func TestTerminalRetainedConversationNoOpAndMergeUseSurvivingCanonicalHeadEvidence(t *testing.T) {
	devA, devB := newTestDevice(t, "device-A"), newTestDevice(t, "device-B")
	pubA, pubB := &stubRemotePublisher{}, &stubRemotePublisher{}
	oA, storeA := newStoreOrch(t, pubA, devA, Recipient{DeviceID: devB.id, PubKey: devB.pub})
	oB, storeB := newStoreOrch(t, pubB, devB, Recipient{DeviceID: devA.id, PubKey: devA.pub})
	t0 := time.Now().UTC().Add(-time.Minute)
	artifactID, _ := seedConversation(t, storeA, devA.id, turnEv("user", "base", t0))
	require.True(t, oA.forwardCommitted(artifactID))
	retainedA := 0
	require.Equal(t, []ImportOutcome{ImportApplied}, deliverLane(t, pubA, oB, LaneRetained, &retainedA))
	liveA := len(laneEvents(pubA, LaneLive))

	// Once the next live delta has chained verbatim, its retained companion is
	// a content-equal no-op whose derived retained wire id is intentionally not
	// appended to B's log.
	appendConversationDelta(t, storeA, devA.id, artifactID, "from-A", t0.Add(time.Second))
	require.True(t, oA.forwardCommitted(artifactID))
	require.Equal(t, []ImportOutcome{ImportApplied}, importWires(t, oB, takeLane(pubA, LaneLive, &liveA)))
	noOpOutbound := takeLane(pubA, LaneRetained, &retainedA)
	require.Len(t, noOpOutbound, 1)
	noOpWire := wireFromOutbound(noOpOutbound[0])
	noOpOutcome := oB.ImportInboundCanonicalResults([]proto.RemoteEvent{noOpWire})
	require.Equal(t, []ImportOutcome{ImportDeduped}, noOpOutcome)
	_, err := oB.CanonicalEvidenceForInbound(noOpWire)
	require.Error(t, err, "content dedupe must not pretend the retained wire id was appended")
	noOpEvidence, err := oB.CanonicalEvidenceForTerminalInbound(noOpWire, noOpOutcome[0])
	require.NoError(t, err)
	noOpHead, ok, err := storeB.LastEvent(acf.KindConversation, artifactID)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, noOpHead.EventID, noOpEvidence.EventID)
	require.Equal(t, noOpHead.Hash, noOpEvidence.EventHash)

	// Concurrent continuation on B makes A's next retained state diverged. B
	// records a locally-authored union merge, so terminal evidence must bind
	// that exact merge head rather than the absent peer wire id.
	appendConversationDelta(t, storeB, devB.id, artifactID, "from-B", t0.Add(2*time.Second))
	appendConversationDelta(t, storeA, devA.id, artifactID, "from-A-2", t0.Add(3*time.Second))
	require.True(t, oA.forwardCommitted(artifactID))
	mergeOutbound := takeLane(pubA, LaneRetained, &retainedA)
	require.Len(t, mergeOutbound, 1)
	mergeWire := wireFromOutbound(mergeOutbound[0])
	mergeOutcome := oB.ImportInboundCanonicalResults([]proto.RemoteEvent{mergeWire})
	require.Equal(t, []ImportOutcome{ImportApplied}, mergeOutcome)
	_, err = oB.CanonicalEvidenceForInbound(mergeWire)
	require.Error(t, err, "a locally-authored union merge must not masquerade as the peer wire event")
	mergeEvidence, err := oB.CanonicalEvidenceForTerminalInbound(mergeWire, mergeOutcome[0])
	require.NoError(t, err)
	mergeHead, ok, err := storeB.LastEvent(acf.KindConversation, artifactID)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, mergeHead.EventID, mergeEvidence.EventID)
	require.Equal(t, mergeHead.Hash, mergeEvidence.EventHash)

	liveWire := mergeWire
	liveWire.Lane = LaneLive
	_, err = oB.CanonicalEvidenceForTerminalInbound(liveWire, ImportApplied)
	require.Error(t, err, "a missing live conversation event must never gain retained-reconcile fallback evidence")
}

// countNativeMemoryFiles counts agent-native memory files (AGENTS.md / CLAUDE.md)
// anywhere under root, excluding the canonical store dir.
func countNativeMemoryFiles(t *testing.T, root string) int {
	t.Helper()
	storeDir := filepath.Join(root, "store")
	n := 0
	_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if len(p) >= len(storeDir) && p[:len(storeDir)] == storeDir {
			return nil
		}
		base := filepath.Base(p)
		if base == "AGENTS.md" || base == "CLAUDE.md" {
			n++
		}
		return nil
	})
	return n
}

type guardProbeSessionTarget struct {
	fakeConvSource
	dest          string
	writeBody     string
	guard         *RecursionGuard
	sawSuppressed bool
	materialized  int
}

func (g *guardProbeSessionTarget) ConversationSessionPath(acf.Artifact, acf.Event, string) (string, bool, error) {
	return g.dest, true, nil
}

func (g *guardProbeSessionTarget) MaterializeConversationSession(acf.Artifact, acf.Event, string) (string, bool, error) {
	g.materialized++
	g.sawSuppressed = g.guard != nil && g.guard.Suppressed(g.dest)
	if g.writeBody != "" {
		if err := os.MkdirAll(filepath.Dir(g.dest), 0o755); err != nil {
			return "", false, err
		}
		if err := os.WriteFile(g.dest, []byte(g.writeBody), 0o644); err != nil {
			return "", false, err
		}
	}
	return g.dest, true, nil
}

func TestConversationSessionMaterialization_RewritesGeneratedSourceButNotOriginalSource(t *testing.T) {
	root := realTempDir(t)
	store := &acf.Store{Root: filepath.Join(root, "store")}
	require.NoError(t, store.Init())

	original := filepath.Join(root, "native-original.jsonl")
	generated := filepath.Join(root, "generated", "thread.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(generated), 0o755))
	require.NoError(t, os.WriteFile(generated, []byte("native continuation\n"), 0o644))
	src := &guardProbeSessionTarget{
		fakeConvSource: fakeConvSource{name: "src"},
		dest:           generated,
		writeBody:      "canonical union\n",
	}
	tgt := &guardProbeSessionTarget{
		fakeConvSource: fakeConvSource{name: "tgt"},
		dest:           filepath.Join(root, "target", "thread.jsonl"),
		writeBody:      "canonical union\n",
	}
	orch, err := NewOrchestrator(Config{
		Dir: root, Adapters: []adapter.Adapter{src, tgt}, Store: store, GuardWindow: time.Second,
	})
	require.NoError(t, err)
	defer orch.Close()
	src.guard = orch.guard

	now := time.Now().UTC()
	artID := acf.NewID()
	require.NoError(t, store.WriteArtifact(acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion,
		ArtifactID:       artID,
		Kind:             acf.KindConversation,
		Scope:            acf.ScopeGlobal,
		Name:             "thread",
		SourcePath:       original,
		CreatedAt:        now,
		UpdatedAt:        now,
	}))
	payload, err := acf.EncodePayload(acf.ConversationPayload{
		Format: acf.ConversationFormatV1,
		Events: []acf.ConversationEvent{{
			Type: acf.EventTypeTurn, Role: "user", Timestamp: now,
			Content: []acf.ContentBlock{{Type: "text", Text: "preserve both writers"}},
		}},
	})
	require.NoError(t, err)
	require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
		EventID: acf.NewID(), ArtifactID: artID, Type: acf.EventTypeCreate,
		Timestamp: now, Provenance: acf.Provenance{SourceAgent: "src"}, Payload: payload,
	}))

	// A normal native import must not create a second session in its source
	// adapter. A continuation imported from an Aplexica-generated session has a
	// different original SourcePath and must rematerialize the canonical union
	// into that source adapter. The concrete Codex/Claude materializers preserve
	// active inodes or branch safely; this fake only verifies orchestrator routing.
	orch.fanOut(context.Background(), src, []string{artID}, root, original, false, nil)
	require.Zero(t, src.materialized)
	orch.recordDestHash(generated)
	orch.fanOut(context.Background(), src, []string{artID}, root, generated, false, nil)
	require.Equal(t, 1, src.materialized)
	require.Positive(t, tgt.materialized, "other agents must still receive the canonical conversation")
	data, err := os.ReadFile(generated)
	require.NoError(t, err)
	require.Equal(t, "canonical union\n", string(data))
}

func TestConversationSessionMaterialization_PreGuardsPlannedPath(t *testing.T) {
	root := realTempDir(t)
	store := &acf.Store{Root: filepath.Join(root, "store")}
	require.NoError(t, store.Init())

	src := &fakeConvSource{name: "src"}
	tgt := &guardProbeSessionTarget{
		fakeConvSource: fakeConvSource{name: "tgt"},
		dest:           filepath.Join(root, "native", "remote-session.jsonl"),
	}
	orch, err := NewOrchestrator(Config{
		Dir:         root,
		Adapters:    []adapter.Adapter{src, tgt},
		Store:       store,
		GuardWindow: time.Second,
	})
	require.NoError(t, err)
	defer orch.Close()
	tgt.guard = orch.guard

	now := time.Now().UTC()
	artID := acf.NewID()
	require.NoError(t, store.WriteArtifact(acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion,
		ArtifactID:       artID,
		Kind:             acf.KindConversation,
		Scope:            acf.ScopeGlobal,
		Name:             "Germany probe",
		CreatedAt:        now,
		UpdatedAt:        now,
	}))
	payload, _ := json.Marshal(acf.ConversationPayload{
		Format: acf.ConversationFormatV1,
		Events: []acf.ConversationEvent{
			{
				Type:      acf.EventTypeTurn,
				Role:      "user",
				Timestamp: now,
				Content:   []acf.ContentBlock{{Type: "text", Text: "What is the capital of Germany?"}},
			},
			{
				Type:      acf.EventTypeTurn,
				Role:      "assistant",
				Timestamp: now.Add(time.Second),
				Content:   []acf.ContentBlock{{Type: "text", Text: "Berlin."}},
			},
		},
	})
	require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
		EventID:    acf.NewID(),
		ArtifactID: artID,
		Type:       acf.EventTypeCreate,
		Timestamp:  now,
		Provenance: acf.Provenance{SourceAgent: "src"},
		Payload:    payload,
	}))

	orch.fanOut(context.Background(), src, []string{artID}, root, filepath.Join(root, "source.jsonl"), false, nil)

	require.True(t, tgt.sawSuppressed, "native conversation session path must be guard-marked before the materializer writes")
	require.True(t, orch.guard.Suppressed(tgt.dest), "planned session path should remain guarded after materialization")
}

func TestConversationSessionMaterialization_RecordsDestHashForGuardEscape(t *testing.T) {
	root := realTempDir(t)
	store := &acf.Store{Root: filepath.Join(root, "store")}
	require.NoError(t, store.Init())

	src := &fakeConvSource{name: "src"}
	tgt := &guardProbeSessionTarget{
		fakeConvSource: fakeConvSource{name: "tgt"},
		dest:           filepath.Join(root, "native", "remote-session.jsonl"),
		writeBody:      "remote materialized session\n",
	}
	orch, err := NewOrchestrator(Config{
		Dir:         root,
		Adapters:    []adapter.Adapter{src, tgt},
		Store:       store,
		GuardWindow: 5 * time.Second,
	})
	require.NoError(t, err)
	defer orch.Close()
	tgt.guard = orch.guard

	now := time.Now().UTC()
	artID := acf.NewID()
	require.NoError(t, store.WriteArtifact(acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion,
		ArtifactID:       artID,
		Kind:             acf.KindConversation,
		Scope:            acf.ScopeGlobal,
		Name:             "fast switch probe",
		CreatedAt:        now,
		UpdatedAt:        now,
	}))
	payload, err := json.Marshal(acf.ConversationPayload{
		Format: acf.ConversationFormatV1,
		Events: []acf.ConversationEvent{
			{
				Type:      acf.EventTypeTurn,
				Role:      "user",
				Timestamp: now,
				Content:   []acf.ContentBlock{{Type: "text", Text: "What is the distance to Moon?"}},
			},
		},
	})
	require.NoError(t, err)
	require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
		EventID:    acf.NewID(),
		ArtifactID: artID,
		Type:       acf.EventTypeCreate,
		Timestamp:  now,
		Provenance: acf.Provenance{SourceAgent: "src"},
		Payload:    payload,
	}))

	orch.fanOut(context.Background(), src, []string{artID}, root, filepath.Join(root, "source.jsonl"), false, nil)
	require.True(t, orch.guard.Suppressed(tgt.dest), "materialized session path should still be inside the recursion guard")
	require.False(t, orch.destChangedUnderUs(tgt.dest), "freshly materialized bytes should be recorded as the guard baseline")

	require.NoError(t, os.WriteFile(tgt.dest, []byte("remote materialized session\nlocal continuation\n"), 0o644))
	require.True(t, orch.destChangedUnderUs(tgt.dest),
		"a user continuation inside the guard window must be treated as a real edit, not an echo")
}

// P0 clobber fix: if the planned session file changed since the orchestrator
// last wrote/imported it (a user continuation whose import hasn't landed yet),
// materialization must be DEFERRED — never overwrite un-imported turns.
func TestConversationSessionMaterialization_DefersWhenDestEditedUnderUs(t *testing.T) {
	root := realTempDir(t)
	store := &acf.Store{Root: filepath.Join(root, "store")}
	require.NoError(t, store.Init())

	src := &fakeConvSource{name: "src"}
	tgt := &guardProbeSessionTarget{
		fakeConvSource: fakeConvSource{name: "tgt"},
		dest:           filepath.Join(root, "native", "remote-session.jsonl"),
		writeBody:      "canonical state v1\n",
	}
	orch, err := NewOrchestrator(Config{
		Dir:         root,
		Adapters:    []adapter.Adapter{src, tgt},
		Store:       store,
		GuardWindow: 5 * time.Second,
	})
	require.NoError(t, err)
	defer orch.Close()
	tgt.guard = orch.guard

	now := time.Now().UTC()
	artID := acf.NewID()
	require.NoError(t, store.WriteArtifact(acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion,
		ArtifactID:       artID,
		Kind:             acf.KindConversation,
		Scope:            acf.ScopeGlobal,
		Name:             "clobber probe",
		CreatedAt:        now,
		UpdatedAt:        now,
	}))
	payload, err := json.Marshal(acf.ConversationPayload{
		Format: acf.ConversationFormatV1,
		Events: []acf.ConversationEvent{{
			Type: acf.EventTypeTurn, Role: "user", Timestamp: now,
			Content: []acf.ContentBlock{{Type: "text", Text: "first question"}},
		}},
	})
	require.NoError(t, err)
	require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
		EventID: acf.NewID(), ArtifactID: artID, Type: acf.EventTypeCreate,
		Timestamp: now, Provenance: acf.Provenance{SourceAgent: "src"}, Payload: payload,
	}))

	// First materialization: writes v1 and records the dest fingerprint.
	orch.fanOut(context.Background(), src, []string{artID}, root, filepath.Join(root, "source.jsonl"), false, nil)
	require.False(t, orch.destChangedUnderUs(tgt.dest))

	// User continues the session before the import lands.
	userEdit := "canonical state v1\nuser continuation NOT yet imported\n"
	require.NoError(t, os.WriteFile(tgt.dest, []byte(userEdit), 0o644))

	// A second canonical event arrives (e.g. inbound from the peer).
	tgt.writeBody = "canonical state v2\n"
	last, ok, err := store.LastEvent(acf.KindConversation, artID)
	require.NoError(t, err)
	require.True(t, ok)
	payload2, err := json.Marshal(acf.ConversationPayload{
		Format: acf.ConversationFormatV1,
		Events: []acf.ConversationEvent{{
			Type: acf.EventTypeTurn, Role: "user", Timestamp: now,
			Content: []acf.ContentBlock{{Type: "text", Text: "first question"}},
		}, {
			Type: acf.EventTypeTurn, Role: "assistant", Timestamp: now.Add(time.Second),
			Content: []acf.ContentBlock{{Type: "text", Text: "peer answer"}},
		}},
	})
	require.NoError(t, err)
	require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
		EventID: acf.NewID(), ArtifactID: artID, Type: acf.EventTypeUpdate,
		Timestamp: now.Add(time.Second), ParentHash: last.Hash,
		Provenance: acf.Provenance{SourceAgent: "src"}, Payload: payload2,
	}))

	orch.fanOut(context.Background(), src, []string{artID}, root, filepath.Join(root, "source.jsonl"), false, nil)

	got, err := os.ReadFile(tgt.dest)
	require.NoError(t, err)
	require.Equal(t, userEdit, string(got),
		"materialization must be deferred while an un-imported user edit sits in the file")

	// Once the pending edit has been imported (import path re-records the
	// fingerprint), the next fan-out cycle materializes normally.
	orch.recordDestHash(tgt.dest)
	orch.fanOut(context.Background(), src, []string{artID}, root, filepath.Join(root, "source.jsonl"), false, nil)
	got, err = os.ReadFile(tgt.dest)
	require.NoError(t, err)
	require.Equal(t, "canonical state v2\n", string(got))
}

func TestImportInbound_MaterializesConversationWhenRemoteSourceDisabledLocally(t *testing.T) {
	root := realTempDir(t)
	store := &acf.Store{Root: filepath.Join(root, "store")}
	require.NoError(t, store.Init())

	local := newTestDevice(t, "windows-device")
	src := &fakeConvSource{name: "codex"}
	tgt := &fakeConvTarget{fakeConvSource: fakeConvSource{name: "claude-code"}}
	orch, err := NewOrchestrator(Config{
		Dir:               root,
		Adapters:          []adapter.Adapter{src, tgt},
		Store:             store,
		SyncGate:          syncgate.New(syncgate.Config{Agents: map[string]bool{"claude-code": true}}),
		LocalDeviceID:     local.id,
		DeviceKeyProvider: fixedKeyProvider{priv: local.priv},
	})
	require.NoError(t, err)
	defer orch.Close()

	now := time.Now().UTC()
	artID := acf.NewID()
	payload, err := json.Marshal(acf.ConversationPayload{
		Format: acf.ConversationFormatV1,
		Events: []acf.ConversationEvent{
			{
				Type:      acf.EventTypeTurn,
				Role:      "user",
				Timestamp: now,
				Content:   []acf.ContentBlock{{Type: "text", Text: "What is the capital of Iceland?"}},
			},
			{
				Type:      acf.EventTypeTurn,
				Role:      "assistant",
				Timestamp: now.Add(time.Second),
				Content:   []acf.ContentBlock{{Type: "text", Text: "Reykjavik."}},
			},
		},
	})
	require.NoError(t, err)
	remoteHead := acf.Event{
		EventID:    acf.NewID(),
		ArtifactID: artID,
		Type:       acf.EventTypeUpdate,
		Timestamp:  now,
		Provenance: acf.Provenance{DeviceID: "mac-device", SourceAgent: "codex"},
		Payload:    payload,
		ParentHash: "missing-parent",
	}
	sealed, err := sealEnvelope(remoteHead, acf.ScopeGlobal, nil, []recipient{{deviceID: local.id, pub: local.pub}})
	require.NoError(t, err)

	outcomes := orch.ImportInboundResults([]proto.RemoteEvent{{
		ArtifactID: artID,
		EventID:    remoteHead.EventID,
		Kind:       string(acf.KindConversation),
		Type:       string(remoteHead.Type),
		Timestamp:  remoteHead.Timestamp,
		ParentHash: remoteHead.ParentHash,
		Bytes:      sealed,
		Origin:     "mac-device",
	}})

	require.Equal(t, []ImportOutcome{ImportApplied}, outcomes)
	require.Equal(t, 1, tgt.materialized(),
		"inbound cloud materialization must not require the remote source agent to be enabled locally")
}

func TestImportInbound_BlockedTargetMaterializesAfterSafetyClearWithoutRewritingHealthyTarget(t *testing.T) {
	root := realTempDir(t)
	store := &acf.Store{Root: filepath.Join(root, "store")}
	require.NoError(t, store.Init())

	local := newTestDevice(t, "receiving-device")
	src := &fakeConvSource{name: "codex"}
	healthy := &fakeConvTarget{fakeConvSource: fakeConvSource{name: "claude-code"}}
	blocked := &failOnceConversationTarget{fakeConvSource: fakeConvSource{name: "hermes"}}
	blocker := NewAdapterBlocker(map[string]string{"hermes": "native safety backup verification pending"})
	orch, err := NewOrchestrator(Config{
		Dir:            root,
		Adapters:       []adapter.Adapter{src, healthy, blocked},
		Store:          store,
		AdapterBlocker: blocker,
		SyncGate: syncgate.New(syncgate.Config{Agents: map[string]bool{
			"claude-code": true,
			"hermes":      true,
		}}),
		LocalDeviceID:     local.id,
		DeviceKeyProvider: fixedKeyProvider{priv: local.priv},
	})
	require.NoError(t, err)
	defer orch.Close()

	now := time.Now().UTC()
	artID := acf.NewID()
	payload, err := json.Marshal(acf.ConversationPayload{
		Format: acf.ConversationFormatV1,
		Events: []acf.ConversationEvent{{
			Type: acf.EventTypeTurn, Role: "user", Timestamp: now,
			Content: []acf.ContentBlock{{Type: "text", Text: "deferred safety fan-out"}},
		}},
	})
	require.NoError(t, err)
	remoteHead := acf.Event{
		EventID: acf.NewID(), ArtifactID: artID, Type: acf.EventTypeCreate, Timestamp: now,
		Provenance: acf.Provenance{DeviceID: "peer-device", SourceAgent: "codex"}, Payload: payload,
	}
	sealed, err := sealEnvelope(remoteHead, acf.ScopeGlobal, nil, []recipient{{deviceID: local.id, pub: local.pub}})
	require.NoError(t, err)

	require.Equal(t, []ImportOutcome{ImportApplied}, orch.ImportInboundResults([]proto.RemoteEvent{{
		ArtifactID: artID, EventID: remoteHead.EventID, Kind: string(acf.KindConversation),
		Type: string(remoteHead.Type), Timestamp: remoteHead.Timestamp, Bytes: sealed, Origin: "peer-device",
	}}))
	require.Equal(t, 1, healthy.materialized(), "an unblocked sibling must receive the live inbound event immediately")
	attempts, successes := blocked.counts()
	require.Equal(t, 0, attempts, "startup safety must withhold the blocked target")
	require.Equal(t, 0, successes)

	blocker.Clear("hermes")
	require.Eventually(t, func() bool {
		_, successes = blocked.counts()
		return successes == 1
	}, 3*time.Second, 10*time.Millisecond, "the clear transition must retry and drain the exact missed artifact")
	time.Sleep(100 * time.Millisecond)
	require.Equal(t, 1, healthy.materialized(), "targeted retry must not rewrite a healthy sibling")
	attempts, successes = blocked.counts()
	require.Equal(t, 2, attempts, "a transient native failure must be retried")
	require.Equal(t, 1, successes)
	blocker.Clear("hermes")
	time.Sleep(100 * time.Millisecond)
	attemptsAfter, successesAfter := blocked.counts()
	require.Equal(t, attempts, attemptsAfter, "duplicate Clear calls must not duplicate the target")
	require.Equal(t, successes, successesAfter)
}

func TestImportInbound_BlockedTargetDeferralSurvivesRestart(t *testing.T) {
	root := realTempDir(t)
	store := &acf.Store{Root: filepath.Join(root, "store")}
	require.NoError(t, store.Init())

	local := newTestDevice(t, "receiving-device")
	src := &fakeConvSource{name: "codex"}
	healthy := &fakeConvTarget{fakeConvSource: fakeConvSource{name: "claude-code"}}
	recovered := &fakeConvTarget{fakeConvSource: fakeConvSource{name: "hermes"}}
	blocked := NewAdapterBlocker(map[string]string{"hermes": "native safety backup verification pending"})
	config := func(blocker *AdapterBlocker) Config {
		return Config{
			Dir: root, Adapters: []adapter.Adapter{src, healthy, recovered}, Store: store,
			AdapterBlocker: blocker,
			SyncGate: syncgate.New(syncgate.Config{Agents: map[string]bool{
				"claude-code": true,
				"hermes":      true,
			}}),
			LocalDeviceID:     local.id,
			DeviceKeyProvider: fixedKeyProvider{priv: local.priv},
		}
	}

	orch, err := NewOrchestrator(config(blocked))
	require.NoError(t, err)
	now := time.Now().UTC()
	artID := acf.NewID()
	payload, err := json.Marshal(acf.ConversationPayload{
		Format: acf.ConversationFormatV1,
		Events: []acf.ConversationEvent{{
			Type: acf.EventTypeTurn, Role: "user", Timestamp: now,
			Content: []acf.ContentBlock{{Type: "text", Text: "persist this blocked target"}},
		}},
	})
	require.NoError(t, err)
	remoteHead := acf.Event{
		EventID: acf.NewID(), ArtifactID: artID, Type: acf.EventTypeCreate, Timestamp: now,
		Provenance: acf.Provenance{DeviceID: "peer-device", SourceAgent: "codex"}, Payload: payload,
	}
	sealed, err := sealEnvelope(remoteHead, acf.ScopeGlobal, nil, []recipient{{deviceID: local.id, pub: local.pub}})
	require.NoError(t, err)
	require.Equal(t, []ImportOutcome{ImportApplied}, orch.ImportInboundResults([]proto.RemoteEvent{{
		ArtifactID: artID, EventID: remoteHead.EventID, Kind: string(acf.KindConversation),
		Type: string(remoteHead.Type), Timestamp: remoteHead.Timestamp, Bytes: sealed, Origin: "peer-device",
	}}))
	require.Equal(t, 1, healthy.materialized())
	require.Equal(t, 0, recovered.materialized())
	queues, err := loadDeferredMaterializationQueues(store.Root)
	require.NoError(t, err)
	require.Contains(t, queues, "hermes", "blocked target must be durable before daemon shutdown")
	require.NoError(t, orch.Close())

	// A new process has no exact-ID queue. The persisted target marker must
	// trigger one target-only canonical reconciliation without waiting for a
	// new artifact event or rewriting the already-healthy sibling.
	orch, err = NewOrchestrator(config(NewAdapterBlocker(nil)))
	require.NoError(t, err)
	require.Eventually(t, func() bool { return recovered.materialized() == 1 }, 3*time.Second, 10*time.Millisecond)
	require.Equal(t, 1, healthy.materialized(), "restart recovery must remain target-only")
	require.Eventually(t, func() bool {
		queues, loadErr := loadDeferredMaterializationQueues(store.Root)
		return loadErr == nil && len(queues) == 0
	}, 3*time.Second, 10*time.Millisecond, "successful reconciliation must clear the durable marker")
	require.NoError(t, orch.Close())

	// Once cleared, another restart must not replay the target again.
	orch, err = NewOrchestrator(config(NewAdapterBlocker(nil)))
	require.NoError(t, err)
	time.Sleep(2 * deferredMaterializationRetryMin)
	require.Equal(t, 1, recovered.materialized())
	require.NoError(t, orch.Close())
}

func TestFinalizeInboundCanonicalEvidence_BlockedTargetStaysProtocolRetryableAndIsNotLegacyQueued(t *testing.T) {
	root := realTempDir(t)
	adapters, store, _ := buildAllThreeAdapters(t, root)
	target := &exportCountingAdapter{Adapter: adapters[0]}
	targetName := target.Name()
	blocker := NewAdapterBlocker(map[string]string{targetName: "native safety backup verification pending"})
	orch, err := NewOrchestrator(Config{
		Dir: root, Adapters: []adapter.Adapter{target}, Store: store,
		AdapterBlocker: blocker,
		SyncGate:       syncgate.New(syncgate.Config{Agents: map[string]bool{targetName: true}}),
		LocalDeviceID:  "receiving-device",
	})
	require.NoError(t, err)
	defer orch.Close()

	artifactID, stored := seedArtifact(t, store, acf.KindMemory, "peer-device")
	artifact, err := store.ReadArtifact(acf.KindMemory, artifactID)
	require.NoError(t, err)
	artifact.RemoteOriginDeviceID = "peer-device"
	artifact.RemoteSourceAgent = "codex"
	require.NoError(t, store.WriteArtifact(artifact))
	evidence := InboundCanonicalEvidence{
		FinalizeKind: proto.InboundFinalizeCanonicalMaterialize,
		Kind:         acf.KindMemory, ArtifactID: artifactID, EventID: stored.EventID, EventHash: stored.Hash,
	}

	require.ErrorIs(t, orch.FinalizeInboundCanonicalEvidence(evidence), ErrInboundNativeMaterialization,
		"durable finalize must remain retryable while its target is safety-blocked")
	blocker.Clear(targetName)
	time.Sleep(2 * deferredMaterializationRetryMin)
	require.Equal(t, 0, target.exports, "strict durable finalize must never enter the legacy clear-transition queue")
	require.NoError(t, orch.FinalizeInboundCanonicalEvidence(evidence))
	require.Equal(t, 1, target.exports)
}

func TestImportInboundCanonicalResults_DefersNativeMaterializationUntilFinalize(t *testing.T) {
	root := realTempDir(t)
	store := &acf.Store{Root: filepath.Join(root, "store")}
	require.NoError(t, store.Init())

	local := newTestDevice(t, "windows-device")
	src := &fakeConvSource{name: "codex"}
	tgt := &fakeConvTarget{fakeConvSource: fakeConvSource{name: "claude-code"}}
	orch, err := NewOrchestrator(Config{
		Dir:               root,
		Adapters:          []adapter.Adapter{src, tgt},
		Store:             store,
		SyncGate:          syncgate.New(syncgate.Config{Agents: map[string]bool{"claude-code": true}}),
		LocalDeviceID:     local.id,
		DeviceKeyProvider: fixedKeyProvider{priv: local.priv},
	})
	require.NoError(t, err)
	defer orch.Close()

	now := time.Now().UTC()
	artID := acf.NewID()
	payload, err := json.Marshal(acf.ConversationPayload{
		Format: acf.ConversationFormatV1,
		Events: []acf.ConversationEvent{{
			Type:      acf.EventTypeTurn,
			Role:      "user",
			Timestamp: now,
			Content:   []acf.ContentBlock{{Type: "text", Text: "durable delta"}},
		}},
	})
	require.NoError(t, err)
	remoteHead := acf.Event{
		EventID:    acf.NewID(),
		ArtifactID: artID,
		Type:       acf.EventTypeUpdate,
		Timestamp:  now,
		Provenance: acf.Provenance{DeviceID: "mac-device", SourceAgent: "codex"},
		Payload:    payload,
	}
	sealed, err := sealEnvelope(remoteHead, acf.ScopeGlobal, nil, []recipient{{deviceID: local.id, pub: local.pub}})
	require.NoError(t, err)
	wire := proto.RemoteEvent{
		ArtifactID: artID,
		EventID:    remoteHead.EventID,
		Kind:       string(acf.KindConversation),
		Type:       string(remoteHead.Type),
		Timestamp:  remoteHead.Timestamp,
		Bytes:      sealed,
		Origin:     "mac-device",
	}

	require.Equal(t, []ImportOutcome{ImportApplied}, orch.ImportInboundCanonicalResults([]proto.RemoteEvent{wire}))
	require.Equal(t, 0, tgt.materialized(), "canonical commit must not write native files before cloud ACK")
	stored, ok, err := store.LastEvent(acf.KindConversation, artID)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, remoteHead.EventID, stored.EventID)

	orch.MaterializeInboundArtifacts([]string{"", artID, artID})
	require.Equal(t, 1, tgt.materialized(), "finalize must deduplicate artifact ids within a request")
}

type slowProbeSessionTarget struct {
	fakeConvSource
	dest  string
	delay time.Duration
}

func (s *slowProbeSessionTarget) ConversationSessionPath(acf.Artifact, acf.Event, string) (string, bool, error) {
	return s.dest, true, nil
}

func (s *slowProbeSessionTarget) MaterializeConversationSession(art acf.Artifact, head acf.Event, _ string) (string, bool, error) {
	p, err := acf.DecodeConversationPayload(head)
	if err != nil {
		return "", false, err
	}
	body := fmt.Sprintf("turns=%d\n", len(p.Events))
	if len(p.Events) < 2 {
		// Widen the stale writer's read→write window so the newer head's
		// write lands inside it — the interleaving under test. The fresh
		// (2-turn) writer stays fast, so without per-artifact serialization
		// the stale body clobbers it.
		time.Sleep(s.delay)
	}
	if err := os.MkdirAll(filepath.Dir(s.dest), 0o755); err != nil {
		return "", false, err
	}
	if err := os.WriteFile(s.dest, []byte(body), 0o644); err != nil {
		return "", false, err
	}
	return s.dest, true, nil
}

// countingSessionTarget is a ConversationSessionPathTarget that writes
// "turns=N" for the head it received and counts every materialization, so
// the large-conversation debounce tests can pin exact write counts and the
// content written by the trailing-edge flush.
type countingSessionTarget struct {
	fakeConvSource
	dest string
	mu   sync.Mutex
	n    int
}

func (c *countingSessionTarget) ConversationSessionPath(acf.Artifact, acf.Event, string) (string, bool, error) {
	return c.dest, true, nil
}

func (c *countingSessionTarget) MaterializeConversationSession(_ acf.Artifact, head acf.Event, _ string) (string, bool, error) {
	p, err := acf.DecodeConversationPayload(head)
	if err != nil {
		return "", false, err
	}
	if err := os.MkdirAll(filepath.Dir(c.dest), 0o755); err != nil {
		return "", false, err
	}
	if err := os.WriteFile(c.dest, []byte(fmt.Sprintf("turns=%d\n", len(p.Events))), 0o644); err != nil {
		return "", false, err
	}
	c.mu.Lock()
	c.n++
	c.mu.Unlock()
	return c.dest, true, nil
}

func (c *countingSessionTarget) writes() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// docOnlyConvTarget implements ONLY ConversationDocTarget, so fanOut routes
// its conversation plan through the markdown-transcript pass (a session
// target would take precedence).
type docOnlyConvTarget struct {
	fakeConvSource
	dir string
}

func (d *docOnlyConvTarget) ConversationDocDir() (string, bool) { return d.dir, true }

// shrinkLargeMaterializeKnobs lowers the large-artifact threshold + debounce
// window so the tests exercise the coalescing path without multi-MB fixtures
// or 15 s sleeps. Restored via t.Cleanup (tests in this package do not run
// in parallel — same convention as remotePublishLiveMaxBytes overrides).
func shrinkLargeMaterializeKnobs(t *testing.T, threshold int, debounce time.Duration) {
	t.Helper()
	restoreThreshold, restoreDebounce := largeMaterializeThreshold, largeMaterializeDebounce
	largeMaterializeThreshold = threshold
	largeMaterializeDebounce = debounce
	t.Cleanup(func() {
		largeMaterializeThreshold, largeMaterializeDebounce = restoreThreshold, restoreDebounce
	})
}

// convGrowingAppender returns a helper that appends a full-payload head with
// n turns (the first turn padded to `pad` bytes so every head clears the
// shrunken large-artifact threshold when pad > threshold).
func convGrowingAppender(t *testing.T, store *acf.Store, artID string, now time.Time, pad int) func(n int) {
	t.Helper()
	parent := ""
	return func(n int) {
		evs := make([]acf.ConversationEvent, 0, n)
		for i := 0; i < n; i++ {
			text := fmt.Sprintf("t%d", i)
			if i == 0 && pad > 0 {
				text = strings.Repeat("x", pad)
			}
			evs = append(evs, turnEv("user", text, now.Add(time.Duration(i)*time.Second)))
		}
		p, err := json.Marshal(acf.ConversationPayload{Format: acf.ConversationFormatV1, Events: evs})
		require.NoError(t, err)
		ev := acf.Event{
			EventID: acf.NewID(), ArtifactID: artID,
			Type: acf.EventTypeUpdate, Timestamp: now.Add(time.Duration(n) * time.Second),
			ParentHash: parent, Provenance: acf.Provenance{SourceAgent: "src"}, Payload: p,
		}
		if parent == "" {
			ev.Type = acf.EventTypeCreate
		}
		require.NoError(t, store.AppendEvent(acf.KindConversation, ev))
		head, ok, err := store.LastEvent(acf.KindConversation, artID)
		require.NoError(t, err)
		require.True(t, ok)
		parent = head.Hash
	}
}

// Design rule 8 (aligned-chains): an artifact whose materialized head payload
// exceeds largeMaterializeThreshold must NOT rewrite its native session file
// on every fan-out — rapid dispatches coalesce into ONE trailing-edge write
// per largeMaterializeDebounce, and that write carries the NEWEST head.
func TestConversationSessionMaterialization_LargePayloadDebounced(t *testing.T) {
	shrinkLargeMaterializeKnobs(t, 1<<10, 500*time.Millisecond)

	root := realTempDir(t)
	store := &acf.Store{Root: filepath.Join(root, "store")}
	require.NoError(t, store.Init())
	src := &fakeConvSource{name: "src"}
	tgt := &countingSessionTarget{
		fakeConvSource: fakeConvSource{name: "tgt"},
		dest:           filepath.Join(root, "native", "large.jsonl"),
	}
	orch, err := NewOrchestrator(Config{Dir: root, Adapters: []adapter.Adapter{src, tgt}, Store: store, GuardWindow: time.Minute})
	require.NoError(t, err)
	t.Cleanup(func() { _ = orch.Close() })

	now := time.Now().UTC()
	artID := acf.NewID()
	require.NoError(t, store.WriteArtifact(acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion, ArtifactID: artID,
		Kind: acf.KindConversation, Scope: acf.ScopeGlobal, Name: "large",
		CreatedAt: now, UpdatedAt: now,
	}))
	appendHead := convGrowingAppender(t, store, artID, now, 2<<10) // every head > 1 KiB threshold

	// Three rapid fan-outs, each with a newer head.
	for n := 1; n <= 3; n++ {
		appendHead(n)
		orch.fanOut(context.Background(), src, []string{artID}, root, "", false, nil)
	}

	// Trailing edge: nothing written yet, and nothing guard-marked at
	// SCHEDULE time — the guard window must start at write time, or a real
	// agent edit landing during the pending window would be swallowed as an
	// echo of a write that hasn't happened.
	require.Equal(t, 0, tgt.writes(), "large-artifact fan-outs must defer to the trailing-edge flush")
	require.False(t, orch.guard.Suppressed(tgt.dest), "planned path must not be guard-marked at schedule time")

	// The flush writes exactly once, with the NEWEST head (3 turns).
	require.Eventually(t, func() bool { return tgt.writes() == 1 }, 5*time.Second, 20*time.Millisecond,
		"the debounce timer must flush exactly one write")
	got, err := os.ReadFile(tgt.dest)
	require.NoError(t, err)
	require.Equal(t, "turns=3\n", string(got), "the trailing-edge flush must write the newest head")
	require.True(t, orch.guard.Suppressed(tgt.dest), "the deferred write must be guard-marked at write time")

	// Cleared on fire: no ghost rewrites after the flush.
	time.Sleep(3 * largeMaterializeDebounce / 2)
	require.Equal(t, 1, tgt.writes(), "a fired flush must not re-run")

	// The next dispatch re-arms a fresh window and flushes the newer head.
	appendHead(4)
	orch.fanOut(context.Background(), src, []string{artID}, root, "", false, nil)
	require.Equal(t, 1, tgt.writes(), "a new dispatch after the flush must debounce again, not write immediately")
	require.Eventually(t, func() bool { return tgt.writes() == 2 }, 5*time.Second, 20*time.Millisecond)
	got, err = os.ReadFile(tgt.dest)
	require.NoError(t, err)
	require.Equal(t, "turns=4\n", string(got))
}

// Small conversations keep today's behavior: every fan-out materializes
// immediately — three dispatches, three writes, no timer involved.
func TestConversationSessionMaterialization_SmallPayloadImmediate(t *testing.T) {
	shrinkLargeMaterializeKnobs(t, 1<<10, 500*time.Millisecond)

	root := realTempDir(t)
	store := &acf.Store{Root: filepath.Join(root, "store")}
	require.NoError(t, store.Init())
	src := &fakeConvSource{name: "src"}
	tgt := &countingSessionTarget{
		fakeConvSource: fakeConvSource{name: "tgt"},
		dest:           filepath.Join(root, "native", "small.jsonl"),
	}
	orch, err := NewOrchestrator(Config{Dir: root, Adapters: []adapter.Adapter{src, tgt}, Store: store, GuardWindow: time.Minute})
	require.NoError(t, err)
	t.Cleanup(func() { _ = orch.Close() })

	now := time.Now().UTC()
	artID := acf.NewID()
	require.NoError(t, store.WriteArtifact(acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion, ArtifactID: artID,
		Kind: acf.KindConversation, Scope: acf.ScopeGlobal, Name: "small",
		CreatedAt: now, UpdatedAt: now,
	}))
	appendHead := convGrowingAppender(t, store, artID, now, 0) // tiny turns, well under 1 KiB

	for n := 1; n <= 3; n++ {
		appendHead(n)
		orch.fanOut(context.Background(), src, []string{artID}, root, "", false, nil)
		require.Equal(t, n, tgt.writes(), "small artifacts must materialize immediately on every fan-out")
	}
	got, err := os.ReadFile(tgt.dest)
	require.NoError(t, err)
	require.Equal(t, "turns=3\n", string(got))
}

// The markdown-transcript pass debounces large artifacts the same way as the
// native-session pass (Design rule 8 covers both materialization forms).
func TestConversationDocMaterialization_LargePayloadDebounced(t *testing.T) {
	shrinkLargeMaterializeKnobs(t, 1<<10, 500*time.Millisecond)

	root := realTempDir(t)
	store := &acf.Store{Root: filepath.Join(root, "store")}
	require.NoError(t, store.Init())
	src := &fakeConvSource{name: "src"}
	tgt := &docOnlyConvTarget{
		fakeConvSource: fakeConvSource{name: "tgt"},
		dir:            filepath.Join(root, "docs"),
	}
	orch, err := NewOrchestrator(Config{Dir: root, Adapters: []adapter.Adapter{src, tgt}, Store: store, GuardWindow: time.Minute})
	require.NoError(t, err)
	t.Cleanup(func() { _ = orch.Close() })

	now := time.Now().UTC()
	artID := acf.NewID()
	require.NoError(t, store.WriteArtifact(acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion, ArtifactID: artID,
		Kind: acf.KindConversation, Scope: acf.ScopeGlobal, Name: "large doc",
		CreatedAt: now, UpdatedAt: now,
	}))
	appendHead := convGrowingAppender(t, store, artID, now, 2<<10)

	for n := 1; n <= 3; n++ {
		appendHead(n)
		orch.fanOut(context.Background(), src, []string{artID}, root, "", false, nil)
	}

	docPath := filepath.Join(tgt.dir, conversationDocFilename("src", artID))
	_, statErr := os.Stat(docPath)
	require.True(t, os.IsNotExist(statErr), "large-artifact transcript must not be written before the trailing-edge flush")

	require.Eventually(t, func() bool {
		got, rerr := os.ReadFile(docPath)
		return rerr == nil && strings.Contains(string(got), "t2")
	}, 5*time.Second, 20*time.Millisecond, "the flush must render the transcript with the newest head (turn t2 present)")
}

// Two racing fan-outs of the same artifact must leave the NEWEST head in the
// native file — the per-artifact lock forces the second writer to re-read the
// head inside the lock.
func TestConversationSessionMaterialization_ConcurrentFanOutsKeepNewestHead(t *testing.T) {
	root := realTempDir(t)
	store := &acf.Store{Root: filepath.Join(root, "store")}
	require.NoError(t, store.Init())
	src := &fakeConvSource{name: "src"}
	tgt := &slowProbeSessionTarget{
		fakeConvSource: fakeConvSource{name: "tgt"},
		dest:           filepath.Join(root, "native", "race.jsonl"),
		delay:          100 * time.Millisecond,
	}
	orch, err := NewOrchestrator(Config{Dir: root, Adapters: []adapter.Adapter{src, tgt}, Store: store, GuardWindow: time.Second})
	require.NoError(t, err)
	defer orch.Close()

	now := time.Now().UTC()
	artID := acf.NewID()
	require.NoError(t, store.WriteArtifact(acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion, ArtifactID: artID,
		Kind: acf.KindConversation, Scope: acf.ScopeGlobal, Name: "race",
		CreatedAt: now, UpdatedAt: now,
	}))
	appendTurns := func(n int, parent string) string {
		evs := make([]acf.ConversationEvent, 0, n)
		for i := 0; i < n; i++ {
			evs = append(evs, turnEv("user", fmt.Sprintf("t%d", i), now.Add(time.Duration(i)*time.Second)))
		}
		p, err := json.Marshal(acf.ConversationPayload{Format: acf.ConversationFormatV1, Events: evs})
		require.NoError(t, err)
		ev := acf.Event{
			EventID: acf.NewID(), ArtifactID: artID,
			Type: acf.EventTypeUpdate, Timestamp: now.Add(time.Duration(n) * time.Second),
			ParentHash: parent, Provenance: acf.Provenance{SourceAgent: "src"}, Payload: p,
		}
		if parent == "" {
			ev.Type = acf.EventTypeCreate
		}
		require.NoError(t, store.AppendEvent(acf.KindConversation, ev))
		head, _, _ := store.LastEvent(acf.KindConversation, artID)
		return head.Hash
	}
	h1 := appendTurns(1, "")

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { // stale fan-out racing…
		defer wg.Done()
		orch.fanOut(context.Background(), src, []string{artID}, root, "", false, nil)
	}()
	go func() { // …a newer head + its fan-out
		defer wg.Done()
		time.Sleep(10 * time.Millisecond)
		appendTurns(2, h1)
		orch.fanOut(context.Background(), src, []string{artID}, root, "", false, nil)
	}()
	wg.Wait()

	got, err := os.ReadFile(tgt.dest)
	require.NoError(t, err)
	require.Equal(t, "turns=2\n", string(got), "the newest head must win the race")
}
