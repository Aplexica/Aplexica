package syncd

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
	"github.com/aplexica/aplexica/internal/adapter/claudecode"
	"github.com/aplexica/aplexica/internal/adapter/codex"
	"github.com/aplexica/aplexica/internal/adapter/hermes"
	"github.com/aplexica/aplexica/internal/hermesdb"
	"github.com/aplexica/aplexica/internal/secrets"
	"github.com/stretchr/testify/require"
)

// importCountingAdapter wraps a real adapter and counts Import calls — the
// expensive parse+canonical-encode step we want a restart to skip.
type importCountingAdapter struct {
	adapter.Adapter
	imports *int32
}

func (a importCountingAdapter) Import(ctx context.Context, store *acf.Store, path string) ([]string, error) {
	atomic.AddInt32(a.imports, 1)
	return a.Adapter.Import(ctx, store, path)
}

// postImportBlockingAdapter lets a test append to the source after the wrapped
// adapter has committed its snapshot but before handleEvent records the scan
// cache fingerprint. That is the exact window in which a fresh post-import stat
// used to hide a later assistant answer permanently.
type postImportBlockingAdapter struct {
	adapter.Adapter
	entered chan struct{}
	release chan struct{}
	blocked int32
}

func (a *postImportBlockingAdapter) Import(ctx context.Context, store *acf.Store, path string) ([]string, error) {
	ids, err := a.Adapter.Import(ctx, store, path)
	if atomic.CompareAndSwapInt32(&a.blocked, 0, 1) {
		close(a.entered)
		<-a.release
	}
	return ids, err
}

// A daemon restart must NOT re-import (re-encode) files that are unchanged
// since the previous run. The startup InitialScan persists a per-file
// fingerprint under the store root; a fresh orchestrator over the same store
// loads it and skips unchanged files entirely — but still imports a file that
// changed while it was down.
func TestOrchestrator_InitialScan_SkipsUnchangedAcrossRestart(t *testing.T) {
	tmp := realTempDir(t)
	storeRoot := filepath.Join(tmp, "store")
	secRoot := filepath.Join(tmp, "sec")
	watchDir := filepath.Join(tmp, "proj")
	require.NoError(t, os.MkdirAll(watchDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(watchDir, "CLAUDE.md"), []byte("memory body"), 0o644))

	var imports int32
	newOrch := func() *Orchestrator {
		store := &acf.Store{Root: storeRoot}
		require.NoError(t, store.Init())
		ss := &secrets.Store{Root: secRoot}
		require.NoError(t, ss.Init())
		cc := claudecode.New()
		cc.HomeDir = tmp
		cc.SecretsStore = ss
		orch, err := NewOrchestrator(Config{
			Dir:         watchDir,
			Adapters:    []adapter.Adapter{importCountingAdapter{Adapter: cc, imports: &imports}},
			Store:       store,
			QuietPeriod: 50 * time.Millisecond,
			GuardWindow: time.Second,
		})
		require.NoError(t, err)
		return orch
	}

	ctx := context.Background()

	// Boot 1: empty cache → the pre-existing file is imported.
	orch1 := newOrch()
	require.NoError(t, orch1.InitialScan(ctx))
	require.GreaterOrEqual(t, atomic.LoadInt32(&imports), int32(1), "first scan must import the file")
	require.NoError(t, orch1.Close())

	// Boot 2: a fresh orchestrator over the SAME store loads the persisted
	// fingerprints and skips the unchanged file — zero new imports.
	atomic.StoreInt32(&imports, 0)
	orch2 := newOrch()
	require.NoError(t, orch2.InitialScan(ctx))
	require.Equal(t, int32(0), atomic.LoadInt32(&imports),
		"a restart over unchanged files must skip the expensive re-import entirely")

	// A real change while "down" must still be picked up on the next scan.
	require.NoError(t, os.WriteFile(filepath.Join(watchDir, "CLAUDE.md"), []byte("memory body — edited, now longer"), 0o644))
	require.NoError(t, orch2.InitialScan(ctx))
	require.Equal(t, int32(1), atomic.LoadInt32(&imports),
		"a file changed since the last import must be re-imported")
	require.NoError(t, orch2.Close())
}

func TestHandleEvent_AppendDuringImportIsNotCachedAsImported(t *testing.T) {
	tmp := realTempDir(t)
	store := &acf.Store{Root: filepath.Join(tmp, "store")}
	require.NoError(t, store.Init())

	cc := claudecode.New()
	cc.HomeDir = tmp
	cc.CanonicalConversations = true
	blocking := &postImportBlockingAdapter{
		Adapter: cc,
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	release := func() {
		select {
		case <-blocking.release:
		default:
			close(blocking.release)
		}
	}

	orch, err := NewOrchestrator(Config{
		Dir:      tmp,
		Adapters: []adapter.Adapter{blocking},
		Store:    store,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		release()
		_ = orch.Close()
	})

	sessionPath := filepath.Join(tmp, ".claude", "projects", "-proj", "sess-race.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(sessionPath), 0o755))
	require.NoError(t, os.WriteFile(sessionPath, []byte(raceSessionUserLine), 0o644))

	done := make(chan bool, 1)
	go func() { done <- orch.handleEvent(sessionPath) }()
	select {
	case <-blocking.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("adapter did not reach the post-import cache race window")
	}

	f, err := os.OpenFile(sessionPath, os.O_APPEND|os.O_WRONLY, 0)
	require.NoError(t, err)
	_, err = f.WriteString(raceSessionAssistantLines)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	release()

	select {
	case handled := <-done:
		require.True(t, handled)
	case <-time.After(5 * time.Second):
		t.Fatal("blocked import did not finish")
	}

	require.False(t, orch.scanCache.unchanged(sessionPath),
		"an answer appended after the adapter snapshot must remain eligible for import")
	require.True(t, orch.destChangedUnderUs(sessionPath),
		"bytes appended during import must remain protected from fan-out clobbering")
	require.True(t, orch.handleEvent(sessionPath), "the next pass must import the appended answer")
	require.True(t, orch.scanCache.unchanged(sessionPath),
		"the cache may mark the file unchanged only after the appended bytes are imported")

	artifacts := conversationArtifactsForSource(t, store, sessionPath)
	require.Len(t, artifacts, 1)
	events, err := store.ReadEvents(acf.KindConversation, artifacts[0].ArtifactID)
	require.NoError(t, err)
	payload, ok, err := acf.MaterializedConversationPayload(events)
	require.NoError(t, err)
	require.True(t, ok)

	var assistantTexts []string
	for _, event := range payload.Events {
		if event.Type != acf.EventTypeTurn || event.Role != "assistant" {
			continue
		}
		for _, block := range event.Content {
			if block.Type == "text" && block.Text != "" {
				assistantTexts = append(assistantTexts, block.Text)
			}
		}
	}
	require.Equal(t, []string{"Mercury has zero moons."}, assistantTexts)
}

func TestInitialScan_V4RepairsUnchangedNativeCodexLeakInHermes(t *testing.T) {
	root := realTempDir(t)
	store := &acf.Store{Root: filepath.Join(root, "store")}
	require.NoError(t, store.Init())
	deviceID := "local-device"
	now := time.Now().UTC()
	sessionsRoot := filepath.Join(root, ".codex", "sessions")
	dayDir := filepath.Join(
		sessionsRoot,
		now.Format("2006"), now.Format("01"), now.Format("02"),
	)
	require.NoError(t, os.MkdirAll(dayDir, 0o755))
	source := filepath.Join(dayDir, "rollout-native-leak.jsonl")
	raw := []byte(`{"timestamp":"2026-07-19T10:00:00Z","type":"session_meta","payload":{"id":"native-leak"}}
{"timestamp":"2026-07-19T10:00:01Z","type":"response_item","payload":{"type":"message","role":"developer","content":[{"type":"input_text","text":"private permissions and execution policy"}]}}
{"timestamp":"2026-07-19T10:00:02Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"what is capital of France?"}]}}
{"timestamp":"2026-07-19T10:00:03Z","type":"response_item","payload":{"type":"message","role":"assistant","phase":"commentary","content":[{"type":"output_text","text":"Searching private execution context."}]}}
{"timestamp":"2026-07-19T10:00:04Z","type":"response_item","payload":{"type":"function_call","name":"exec","arguments":"{}","call_id":"call-1"}}
{"timestamp":"2026-07-19T10:00:05Z","type":"response_item","payload":{"type":"function_call_output","call_id":"call-1","output":"private tool output"}}
{"timestamp":"2026-07-19T10:00:06Z","type":"response_item","payload":{"type":"message","role":"assistant","phase":"final_answer","content":[{"type":"output_text","text":"Paris."}]}}
`)
	require.NoError(t, os.WriteFile(source, raw, 0o600))

	base := time.Date(2026, 7, 19, 10, 0, 0, 0, time.UTC)
	legacy := []acf.ConversationEvent{
		{Type: acf.EventTypeTurn, Timestamp: base.Add(time.Second), Role: "system", Content: []acf.ContentBlock{{Type: "text", Text: "private permissions and execution policy"}}},
		{Type: acf.EventTypeTurn, Timestamp: base.Add(2 * time.Second), Role: "user", Content: []acf.ContentBlock{{Type: "text", Text: "what is capital of France?"}}},
		{Type: acf.EventTypeTurn, Timestamp: base.Add(3 * time.Second), Role: "assistant", Content: []acf.ContentBlock{{Type: "text", Text: "Searching private execution context."}}},
		{Type: acf.EventTypeToolCall, Timestamp: base.Add(4 * time.Second), CallID: "call-1", ToolName: "exec", Input: json.RawMessage(`{}`)},
		{Type: acf.EventTypeToolResult, Timestamp: base.Add(5 * time.Second), CallID: "call-1", Content: []acf.ContentBlock{{Type: "text", Text: "private tool output"}}},
		{Type: acf.EventTypeTurn, Timestamp: base.Add(6 * time.Second), Role: "assistant", Content: []acf.ContentBlock{{Type: "text", Text: "Paris."}}},
	}
	id := acf.NewID()
	require.NoError(t, store.WriteArtifact(acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion, ArtifactID: id,
		Kind: acf.KindConversation, Scope: acf.ScopeGlobal,
		Name: filepath.Base(source), SourcePath: source,
		CreatedAt: now, UpdatedAt: now,
	}))
	payload, err := acf.EncodePayload(acf.ConversationPayload{
		Format: acf.ConversationFormatV1, Events: legacy,
	})
	require.NoError(t, err)
	require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
		EventID: acf.NewID(), ArtifactID: id, Type: acf.EventTypeCreate,
		Timestamp: now, Payload: payload,
		Provenance: acf.Provenance{
			DeviceID: deviceID, SourceAgent: "codex", AdapterVersion: "0.9.2",
		},
	}))

	dbPath := filepath.Join(root, ".hermes", "state.db")
	require.NoError(t, os.MkdirAll(filepath.Dir(dbPath), 0o755))
	db, err := hermesdb.InitTestDB(dbPath)
	require.NoError(t, err)
	require.NoError(t, db.Close())
	hr := hermes.New()
	hr.HomeDir = root
	require.NoError(t, hr.ExportConversationsToDB(t.Context(), store, id, dbPath))
	before, err := hermesdb.ListSessions(dbPath, 0)
	require.NoError(t, err)
	require.Len(t, before, 1)
	require.Len(t, before[0].Messages, 3)
	require.Equal(t, "Searching private execution context.", *before[0].Messages[1].Content)

	sourceFP, ok := fingerprintPath(source)
	require.True(t, ok)
	v3, err := json.Marshal(importScanCacheDisk{
		Version: previousImportScanCacheSchemaVersion,
		Fingerprints: map[string]scanFP{
			source: sourceFP,
		},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(store.Root, importScanCacheName), v3, 0o600))

	cx := codex.New()
	cx.HomeDir = root
	cx.DeviceID = deviceID
	cx.CanonicalConversations = true
	watchDir := filepath.Join(root, "watched")
	require.NoError(t, os.MkdirAll(watchDir, 0o755))
	orch, err := NewOrchestrator(Config{
		Dir:            watchDir,
		RecursiveRoots: []string{sessionsRoot},
		RootsByAdapter: map[string][]string{"codex": {sessionsRoot}},
		Adapters:       []adapter.Adapter{cx},
		Store:          store,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, orch.Close()) })
	require.NoError(t, orch.InitialScan(t.Context()))

	current, ok, err := store.MaterializedConversationPayloadFromStore(id)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, []acf.TextTurn{
		{Role: "user", Text: "what is capital of France?"},
		{Role: "assistant", Text: "Paris."},
	}, acf.ExtractTextTurns(current.Events))
	require.Len(t, current.Events, 2)
	// Production performs the same export from the dedicated Hermes watcher;
	// conversation DBs are intentionally excluded from generic file fan-out.
	require.NoError(t, hr.ExportConversationsToDB(t.Context(), store, id, dbPath))
	after, err := hermesdb.ListSessions(dbPath, 0)
	require.NoError(t, err)
	require.Len(t, after, 1)
	require.Len(t, after[0].Messages, 2)
	require.Equal(t, "what is capital of France?", *after[0].Messages[0].Content)
	require.Equal(t, "Paris.", *after[0].Messages[1].Content)
	for _, message := range after[0].Messages {
		if message.Content != nil {
			require.NotContains(t, *message.Content, "private")
		}
	}
}
