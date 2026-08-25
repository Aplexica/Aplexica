package syncd

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
	"github.com/aplexica/aplexica/internal/syncgate"
	"github.com/aplexica/aplexica/internal/syncrules"
	"github.com/stretchr/testify/require"
)

// fakeConvSource is a minimal adapter that only authors conversations: it is
// never a fan-out target (it implements neither ConversationSessionTarget nor
// ConversationDocTarget), so backfillConversations treats it purely as the
// "primary" (source) agent that the cap excludes.
type fakeConvSource struct{ name string }

func (f *fakeConvSource) Name() string    { return f.name }
func (f *fakeConvSource) Version() string { return "test" }
func (f *fakeConvSource) Import(context.Context, *acf.Store, string) ([]string, error) {
	return nil, nil
}
func (f *fakeConvSource) Export(context.Context, *acf.Store, string, string) error { return nil }
func (f *fakeConvSource) NativePath(acf.Artifact, string) (string, bool, error) {
	return "", false, nil
}
func (f *fakeConvSource) HandlesFormat(acf.Kind, string) bool { return false }
func (f *fakeConvSource) Capabilities() adapter.Capabilities {
	return adapter.Capabilities{Name: f.name}
}
func (f *fakeConvSource) Discover() (adapter.Discovery, error) { return adapter.Discovery{}, nil }

// fakeConvTarget is a ConversationSessionTarget that counts how many
// conversations get materialized into it. It always "supports" the
// materialization so the cap — not a per-payload opt-out — is the only thing
// that bounds the count.
type fakeConvTarget struct {
	fakeConvSource
	mu    sync.Mutex
	count int
}

func (f *fakeConvTarget) MaterializeConversationSession(art acf.Artifact, _ acf.Event, _ string) (string, bool, error) {
	f.mu.Lock()
	f.count++
	f.mu.Unlock()
	// A non-empty, unique path so the orchestrator's guard.Mark + success
	// bookkeeping runs exactly as it would for a real materialization.
	return filepath.Join("/tmp/fake-target", string(art.Kind), art.ArtifactID+".session"), true, nil
}

func (f *fakeConvTarget) materialized() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.count
}

// seedConversations writes n conversation artifacts authored by sourceAgent,
// each with a single create event. Timestamps ascend with i, so artifact i+1
// is newer than artifact i (backfillConversations materializes newest-first).
func seedConversations(t *testing.T, store *acf.Store, sourceAgent string, n int) {
	seedConversationsWithOrigin(t, store, sourceAgent, "", "conv", n)
}

func seedConversationsFromDevice(t *testing.T, store *acf.Store, sourceAgent, deviceID string, n int) {
	seedConversationsWithOrigin(t, store, sourceAgent, deviceID, "conv", n)
}

func seedInboundConversations(t *testing.T, store *acf.Store, sourceAgent, deviceID string, n int) {
	seedConversationsWithOrigin(t, store, sourceAgent, deviceID, "", n)
}

func seedConversationsWithOrigin(t *testing.T, store *acf.Store, sourceAgent, deviceID, name string, n int) {
	t.Helper()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < n; i++ {
		ts := base.Add(time.Duration(i) * time.Minute)
		id := acf.NewID()
		remoteOriginDeviceID := ""
		remoteSourceAgent := ""
		if name == "" && deviceID != "" {
			remoteOriginDeviceID = deviceID
			remoteSourceAgent = sourceAgent
		}
		require.NoError(t, store.WriteArtifact(acf.Artifact{
			AcfSchemaVersion:     acf.SchemaVersion,
			ArtifactID:           id,
			Kind:                 acf.KindConversation,
			Scope:                acf.ScopeGlobal,
			Name:                 name,
			CreatedAt:            ts,
			UpdatedAt:            ts,
			RemoteOriginDeviceID: remoteOriginDeviceID,
			RemoteSourceAgent:    remoteSourceAgent,
		}))
		payload, _ := json.Marshal(acf.ConversationPayload{Format: "test.session", Content: "{}"})
		require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
			EventID:    acf.NewID(),
			ArtifactID: id,
			Type:       acf.EventTypeCreate,
			Timestamp:  ts,
			Provenance: acf.Provenance{DeviceID: deviceID, SourceAgent: sourceAgent},
			Payload:    payload,
			ParentHash: "",
		}))
	}
}

// runBackfill builds an orchestrator with a fake source + counting target,
// seeds n conversations, applies the cap, runs RefanOutAll, and returns how
// many conversations were materialized into the target.
func runBackfill(t *testing.T, n int, caps map[string]int) int {
	t.Helper()
	root := realTempDir(t)
	store := &acf.Store{Root: filepath.Join(root, "store")}
	require.NoError(t, store.Init())

	src := &fakeConvSource{name: "src"}
	tgt := &fakeConvTarget{fakeConvSource: fakeConvSource{name: "tgt"}}
	seedConversations(t, store, "src", n)

	orch, err := NewOrchestrator(Config{
		Dir:      root,
		Adapters: []adapter.Adapter{src, tgt},
		Store:    store,
		// Both source and target enabled — fan-out gating is bidirectional.
		SyncGate: syncgate.New(syncgate.Config{Agents: map[string]bool{
			"src": true,
			"tgt": true,
		}}),
	})
	require.NoError(t, err)
	defer orch.Close()

	if caps != nil {
		orch.SetConvBackfill(caps)
	}

	_, err = orch.RefanOutAll(context.Background())
	require.NoError(t, err)
	return tgt.materialized()
}

// TestBackfillConversations_CapsTargetAtN is the end-to-end proof that
// enabling an agent's sync no longer floods it with another agent's entire
// conversation history: the target receives at most its configured cap.
func TestBackfillConversations_CapsTargetAtN(t *testing.T) {
	// 50 conversations available, cap of 5 → exactly 5 materialized.
	got := runBackfill(t, 50, map[string]int{"tgt": 5})
	require.Equal(t, 5, got, "target must receive exactly its cap, not the full history")
}

func TestBackfillConversations_CapAboveAvailableMaterializesAll(t *testing.T) {
	// Cap of 15 but only 5 conversations exist → all 5 (cap is a ceiling).
	got := runBackfill(t, 5, map[string]int{"tgt": 15})
	require.Equal(t, 5, got)
}

func TestBackfillConversations_NegativeCapIsUnlimited(t *testing.T) {
	// "all" is expressed as a negative cap → every conversation materializes.
	got := runBackfill(t, 23, map[string]int{"tgt": -1})
	require.Equal(t, 23, got)
}

func TestBackfillConversations_DefaultCapWhenUnset(t *testing.T) {
	// No cap configured for "tgt" → DefaultConvBackfill (10) applies.
	got := runBackfill(t, 50, nil)
	require.Equal(t, DefaultConvBackfill, got)
}

func TestBackfillConversations_ZeroCapMaterializesNone(t *testing.T) {
	// An explicit cap of 0 means "seed nothing" for that agent.
	got := runBackfill(t, 50, map[string]int{"tgt": 0})
	require.Equal(t, 0, got)
}

func TestBackfillConversations_RoutingDeniedNewestDoesNotConsumeCap(t *testing.T) {
	root := realTempDir(t)
	store := &acf.Store{Root: filepath.Join(root, "store")}
	require.NoError(t, store.Init())
	seedConversations(t, store, "src", 2)
	conversations, err := store.ListArtifacts(acf.KindConversation)
	require.NoError(t, err)
	require.Len(t, conversations, 2)
	sort.Slice(conversations, func(i, j int) bool { return conversations[i].UpdatedAt.Before(conversations[j].UpdatedAt) })
	conversations[0].Tags = []string{"allowed"}
	conversations[1].Tags = []string{"denied"}
	require.NoError(t, store.WriteArtifact(conversations[0]))
	require.NoError(t, store.WriteArtifact(conversations[1]))

	eng, err := syncrules.New([]syncrules.Rule{{
		Name:  "allowed-history",
		Match: syncrules.MatchSpec{Tag: []string{"allowed"}, Type: []string{"conversation"}},
		Route: syncrules.RouteSpec{
			Agents:              []string{"tgt"},
			HistoricalSyncDepth: map[string]int{"tgt": 1},
		},
	}})
	require.NoError(t, err)
	source := &fakeConvSource{name: "src"}
	target := &fakeConvTarget{fakeConvSource: fakeConvSource{name: "tgt"}}
	orch, err := NewOrchestrator(Config{
		Dir:         root,
		Adapters:    []adapter.Adapter{source, target},
		Store:       store,
		RulesEngine: eng,
	})
	require.NoError(t, err)
	defer orch.Close()
	orch.SetConvBackfill(map[string]int{"tgt": 1})

	require.Equal(t, 2, orch.backfillConversations(context.Background()))
	require.Equal(t, 1, target.materialized(),
		"the newest denied conversation must leave cap room for the newest allowed one")
}

// forcedBackfillHarness builds the same store/orchestrator shape as
// runBackfill but hands the pieces back so forced-backfill tests can drive
// the plan/start API directly.
func forcedBackfillHarness(t *testing.T, n int) (*Orchestrator, *fakeConvTarget) {
	t.Helper()
	root := realTempDir(t)
	store := &acf.Store{Root: filepath.Join(root, "store")}
	require.NoError(t, store.Init())

	src := &fakeConvSource{name: "src"}
	tgt := &fakeConvTarget{fakeConvSource: fakeConvSource{name: "tgt"}}
	seedConversations(t, store, "src", n)

	orch, err := NewOrchestrator(Config{
		Dir:      root,
		Adapters: []adapter.Adapter{src, tgt},
		Store:    store,
		SyncGate: syncgate.New(syncgate.Config{Agents: map[string]bool{
			"src": true,
			"tgt": true,
		}}),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = orch.Close() })
	return orch, tgt
}

// waitForcedBackfillDone polls until the background apply goroutine releases
// the single-flight flag (Close would also join it, but tests want to assert
// counts before shutdown).
func waitForcedBackfillDone(t *testing.T, orch *Orchestrator) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if !orch.forcedBackfillActive.Load() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("forced backfill did not finish within the deadline")
}

// TestForcedConversationBackfill_FullHistoryBeatsTheCap is the feature's
// core promise: `aplexica backfill --apply` materializes EVERYTHING locally,
// where the ordinary enable-time backfill stops at the recent-N cap.
func TestForcedConversationBackfill_FullHistoryBeatsTheCap(t *testing.T) {
	const n = 25
	orch, tgt := forcedBackfillHarness(t, n)

	plan, err := orch.StartForcedConversationBackfill(nil, -1)
	require.NoError(t, err)
	require.Equal(t, n, plan.Conversations)
	require.Equal(t, map[string]int{"tgt": n}, plan.PerAgent)
	require.Equal(t, []string{"tgt"}, plan.Targets)

	waitForcedBackfillDone(t, orch)
	require.Equal(t, n, tgt.materialized(),
		"a forced -1 depth must materialize the full history, not the recent-N cap")
}

// TestForcedConversationBackfill_DepthOverrideCaps proves --depth N caps the
// forced pass at the N most-recent conversations.
func TestForcedConversationBackfill_DepthOverrideCaps(t *testing.T) {
	orch, tgt := forcedBackfillHarness(t, 25)

	plan, err := orch.StartForcedConversationBackfill(nil, 5)
	require.NoError(t, err)
	require.Equal(t, map[string]int{"tgt": 5}, plan.PerAgent)

	waitForcedBackfillDone(t, orch)
	require.Equal(t, 5, tgt.materialized())
}

// TestForcedConversationBackfillPlan_DryRunWritesNothing pins the dry-run
// contract: full counts, zero materializations.
func TestForcedConversationBackfillPlan_DryRunWritesNothing(t *testing.T) {
	const n = 12
	orch, tgt := forcedBackfillHarness(t, n)

	plan, err := orch.ForcedConversationBackfillPlan(nil, -1)
	require.NoError(t, err)
	require.True(t, plan.DryRun)
	require.Equal(t, n, plan.Conversations)
	require.Equal(t, map[string]int{"tgt": n}, plan.PerAgent)
	require.Equal(t, 0, tgt.materialized(), "a dry run must not materialize anything")
}

// TestForcedConversationBackfill_RejectsBadRequests pins the request-shape
// errors: depth 0, an agent filter matching nothing, and double-start.
func TestForcedConversationBackfill_RejectsBadRequests(t *testing.T) {
	orch, _ := forcedBackfillHarness(t, 3)

	_, err := orch.ForcedConversationBackfillPlan(nil, 0)
	require.ErrorContains(t, err, "depth 0")

	_, err = orch.ForcedConversationBackfillPlan([]string{"no-such-agent"}, -1)
	require.ErrorContains(t, err, "no enabled conversation-capable agent")

	// A typo alongside a valid name must fail loudly, not silently narrow.
	_, err = orch.ForcedConversationBackfillPlan([]string{"tgt", "tgtt"}, -1)
	require.ErrorContains(t, err, `"tgtt" is not an enabled conversation-capable target`)

	// Double-start: hold the single-flight flag and prove the second start
	// reports the conflict.
	require.True(t, orch.forcedBackfillActive.CompareAndSwap(false, true))
	_, err = orch.StartForcedConversationBackfill(nil, -1)
	require.ErrorIs(t, err, ErrForcedBackfillRunning)
	orch.forcedBackfillActive.Store(false)
}
