package hermeswatch

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter/hermes"
	"github.com/aplexica/aplexica/internal/hermesdb"
	"github.com/stretchr/testify/require"
)

// FIX B: TickOutbound must invoke OnImported with the imported conversation ids
// so the daemon can fan them out to sibling agents and forward them to the relay
// (the hermeswatch import path bypasses handleEvent's fan-out/forward tail). A
// no-op tick (no DB change) must NOT invoke it.
func TestWatcher_TickOutbound_InvokesOnImported(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.db")
	db, err := hermesdb.InitTestDB(dbPath)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO sessions (id, source, started_at) VALUES ('s1','cli',100.0)`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO messages (session_id, role, content, timestamp) VALUES ('s1','user','hi',110.0)`)
	require.NoError(t, err)
	db.Close()

	storeRoot := filepath.Join(t.TempDir(), "store")
	store := &acf.Store{Root: storeRoot}
	require.NoError(t, store.Init())
	a := &hermes.Adapter{HomeDir: t.TempDir(), DeviceID: "test-dev"}

	var gotIDs []string
	calls := 0
	w := &Watcher{
		Adapter:   a,
		Store:     store,
		DBPath:    dbPath,
		Interval:  5 * time.Second,
		Direction: DirectionOutbound,
		OnImported: func(_ context.Context, ids []string) {
			calls++
			gotIDs = append(gotIDs, ids...)
		},
	}
	ids, err := w.TickOutbound(context.Background())
	require.NoError(t, err)
	require.Len(t, ids, 1)
	require.Equal(t, ids, gotIDs, "OnImported must fire with the imported conversation ids")
	require.Equal(t, 1, calls)

	_, err = w.TickOutbound(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, calls, "OnImported must NOT fire when nothing was imported")
}

func TestWatcher_Tick_NewSessionExported(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.db")
	db, err := hermesdb.InitTestDB(dbPath)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO sessions (id, source, started_at) VALUES ('s1','cli',100.0)`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO messages (session_id, role, content, timestamp) VALUES ('s1','user','hi',110.0)`)
	require.NoError(t, err)
	db.Close()

	storeRoot := filepath.Join(t.TempDir(), "store")
	store := &acf.Store{Root: storeRoot}
	require.NoError(t, store.Init())

	a := &hermes.Adapter{HomeDir: t.TempDir(), DeviceID: "test-dev"}

	w := &Watcher{
		Adapter:   a,
		Store:     store,
		DBPath:    dbPath,
		Interval:  5 * time.Second,
		Direction: DirectionOutbound,
	}
	ids, err := w.Tick(context.Background())
	require.NoError(t, err)
	require.Len(t, ids, 1, "first tick should export the seeded session")

	// HWM should now be at the last message timestamp.
	require.InDelta(t, 110.0, w.HWM(), 0.001)

	// Second tick with no DB change should return zero IDs.
	ids2, err := w.Tick(context.Background())
	require.NoError(t, err)
	require.Empty(t, ids2, "second tick is a no-op when DB has not changed")
}

func TestWatcher_Tick_MessageAppendTriggersUpdate(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.db")
	db, err := hermesdb.InitTestDB(dbPath)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO sessions (id, source, started_at) VALUES ('s1','cli',100.0)`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO messages (session_id, role, content, timestamp) VALUES ('s1','user','first',110.0)`)
	require.NoError(t, err)
	db.Close()

	storeRoot := filepath.Join(t.TempDir(), "store")
	store := &acf.Store{Root: storeRoot}
	require.NoError(t, store.Init())

	a := &hermes.Adapter{HomeDir: t.TempDir(), DeviceID: "test-dev"}

	w := &Watcher{
		Adapter:   a,
		Store:     store,
		DBPath:    dbPath,
		Interval:  5 * time.Second,
		Direction: DirectionOutbound,
	}

	ids1, err := w.Tick(context.Background())
	require.NoError(t, err)
	require.Len(t, ids1, 1)
	firstID := ids1[0]

	// Append a new message
	db2, err := hermesdb.OpenRW(dbPath)
	require.NoError(t, err)
	_, err = db2.Exec(`INSERT INTO messages (session_id, role, content, timestamp) VALUES ('s1','assistant','second',200.0)`)
	require.NoError(t, err)
	db2.Close()

	ids2, err := w.Tick(context.Background())
	require.NoError(t, err)
	require.Len(t, ids2, 1, "second tick should detect the new message and re-export the session")
	require.Equal(t, firstID, ids2[0], "identity reconciliation: same artifact ID, appended update event")

	// Verify: artifact has 2 events (create + update).
	events, err := store.ReadEvents(acf.KindConversation, firstID)
	require.NoError(t, err)
	require.Len(t, events, 2)
	require.Equal(t, acf.EventTypeCreate, events[0].Type)
	require.Equal(t, acf.EventTypeUpdate, events[1].Type)
}

func TestWatcher_Run_ExitsOnContextCancel(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.db")
	db, err := hermesdb.InitTestDB(dbPath)
	require.NoError(t, err)
	db.Close()

	storeRoot := filepath.Join(t.TempDir(), "store")
	store := &acf.Store{Root: storeRoot}
	require.NoError(t, store.Init())

	a := &hermes.Adapter{HomeDir: t.TempDir(), DeviceID: "test-dev"}

	w := &Watcher{
		Adapter:   a,
		Store:     store,
		DBPath:    dbPath,
		Interval:  50 * time.Millisecond,
		Direction: DirectionOutbound,
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	time.Sleep(200 * time.Millisecond) // let it tick a few times
	cancel()

	select {
	case err := <-done:
		require.NoError(t, err, "Run must return nil on context cancellation")
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit within 2s of context cancellation")
	}
}

func TestWatcher_TickInbound_InsertsBundleIntoTargetDB(t *testing.T) {
	// SETUP: source DB has 1 session. Outbound tick exports it to the store.
	srcPath := filepath.Join(t.TempDir(), "src.db")
	src, err := hermesdb.InitTestDB(srcPath)
	require.NoError(t, err)
	_, err = src.Exec(`INSERT INTO sessions (id, source, started_at, title) VALUES ('inbound-test','cli',100.0,'Inbound')`)
	require.NoError(t, err)
	_, err = src.Exec(`INSERT INTO messages (session_id, role, content, timestamp) VALUES ('inbound-test','user','hello',101.0)`)
	require.NoError(t, err)
	src.Close()

	storeRoot := filepath.Join(t.TempDir(), "store")
	store := &acf.Store{Root: storeRoot}
	require.NoError(t, store.Init())

	a := &hermes.Adapter{HomeDir: t.TempDir(), DeviceID: "test-dev"}

	// Use the source DB as the outbound source first to populate the store.
	wOut := &Watcher{
		Adapter:  a,
		Store:    store,
		DBPath:   srcPath,
		Interval: 5 * time.Second,
	}
	_, err = wOut.TickOutbound(context.Background())
	require.NoError(t, err)

	// Now point the inbound watcher at a fresh dst DB.
	dstPath := filepath.Join(t.TempDir(), "dst.db")
	dstInit, err := hermesdb.InitTestDB(dstPath)
	require.NoError(t, err)
	dstInit.Close()

	wIn := &Watcher{
		Adapter:  a,
		Store:    store,
		DBPath:   dstPath,
		Interval: 5 * time.Second,
	}
	ids, err := wIn.TickInbound(context.Background())
	require.NoError(t, err)
	require.Len(t, ids, 1, "first inbound tick should materialize the artifact into dst.db")

	// Verify the session landed in the destination DB.
	got, err := hermesdb.ListSessions(dstPath, 0)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "inbound-test", got[0].Session.ID)
	require.Len(t, got[0].Messages, 1)
	require.Equal(t, "hello", *got[0].Messages[0].Content)

	// Second inbound tick with no store changes → no new IDs (seenHeads cache hit).
	ids2, err := wIn.TickInbound(context.Background())
	require.NoError(t, err)
	require.Empty(t, ids2, "second inbound tick is a no-op when no artifact has changed")
}

func TestWatcher_TickInbound_InsertsPayloadBearingBaselineIntoTargetDB(t *testing.T) {
	storeRoot := filepath.Join(t.TempDir(), "store")
	store := &acf.Store{Root: storeRoot}
	require.NoError(t, store.Init())

	id := acf.NewID()
	now := time.Now().UTC()
	payload, err := acf.EncodePayload(acf.ConversationPayload{
		Format: acf.ConversationFormatV1,
		Events: []acf.ConversationEvent{
			{
				Type:      acf.EventTypeTurn,
				Role:      "user",
				Timestamp: now,
				Content:   []acf.ContentBlock{{Type: "text", Text: "what is the distance to moon?"}},
			},
			{
				Type:      acf.EventTypeTurn,
				Role:      "assistant",
				Timestamp: now.Add(time.Second),
				Content:   []acf.ContentBlock{{Type: "text", Text: "about 384,400 km"}},
			},
		},
	})
	require.NoError(t, err)
	require.NoError(t, store.AdoptBaseline(acf.KindConversation, acf.Event{
		EventID:        acf.NewID(),
		ArtifactID:     id,
		Type:           acf.EventTypeBaseline,
		Timestamp:      now,
		Payload:        payload,
		AlignedHead:    "origin-head-hash",
		AlignedEventID: "origin-head-event",
		Provenance: acf.Provenance{
			DeviceID:       "origin-device",
			SourceAgent:    "claude-code",
			AgentVersion:   acf.UnknownAgentVersion,
			AdapterVersion: "test",
		},
	}))

	dstPath := filepath.Join(t.TempDir(), "dst.db")
	dstInit, err := hermesdb.InitTestDB(dstPath)
	require.NoError(t, err)
	dstInit.Close()

	a := &hermes.Adapter{HomeDir: t.TempDir(), DeviceID: "test-dev"}
	wIn := &Watcher{Adapter: a, Store: store, DBPath: dstPath, Interval: 5 * time.Second}
	ids, err := wIn.TickInbound(context.Background())
	require.NoError(t, err)
	require.Equal(t, []string{id}, ids, "payload-bearing baseline events must materialize into Hermes")

	got, err := hermesdb.ListSessions(dstPath, 0)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, id, got[0].Session.ID)
	require.Len(t, got[0].Messages, 2)
	require.Equal(t, "what is the distance to moon?", *got[0].Messages[0].Content)
}

func TestWatcher_TickInbound_DetectsHeadChange(t *testing.T) {
	// SETUP: as above, populate the store via outbound tick.
	srcPath := filepath.Join(t.TempDir(), "src.db")
	src, err := hermesdb.InitTestDB(srcPath)
	require.NoError(t, err)
	_, err = src.Exec(`INSERT INTO sessions (id, source, started_at) VALUES ('head-change','cli',100.0)`)
	require.NoError(t, err)
	_, err = src.Exec(`INSERT INTO messages (session_id, role, content, timestamp) VALUES ('head-change','user','first',101.0)`)
	require.NoError(t, err)
	src.Close()

	storeRoot := filepath.Join(t.TempDir(), "store")
	store := &acf.Store{Root: storeRoot}
	require.NoError(t, store.Init())

	a := &hermes.Adapter{HomeDir: t.TempDir(), DeviceID: "test-dev"}

	wOut := &Watcher{Adapter: a, Store: store, DBPath: srcPath, Interval: 5 * time.Second}
	_, err = wOut.TickOutbound(context.Background())
	require.NoError(t, err)

	dstPath := filepath.Join(t.TempDir(), "dst.db")
	dstInit, err := hermesdb.InitTestDB(dstPath)
	require.NoError(t, err)
	dstInit.Close()

	wIn := &Watcher{Adapter: a, Store: store, DBPath: dstPath, Interval: 5 * time.Second}
	_, err = wIn.TickInbound(context.Background())
	require.NoError(t, err)

	// Mutate the source: append a message. Then re-run outbound → ACF has a new event.
	srcRW, err := hermesdb.OpenRW(srcPath)
	require.NoError(t, err)
	_, err = srcRW.Exec(`INSERT INTO messages (session_id, role, content, timestamp) VALUES ('head-change','assistant','second',200.0)`)
	require.NoError(t, err)
	srcRW.Close()
	_, err = wOut.TickOutbound(context.Background())
	require.NoError(t, err)

	// Inbound tick should now detect the head change and re-insert.
	ids, err := wIn.TickInbound(context.Background())
	require.NoError(t, err)
	require.Len(t, ids, 1, "inbound tick should re-materialize when head event hash changed")

	// dst.db should now have BOTH messages.
	got, err := hermesdb.ListSessions(dstPath, 0)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Len(t, got[0].Messages, 2)
}

// A conversation can go missing from the Hermes DB while seenHeads still
// records its head as "processed" — e.g. a prior tick observed the store
// artifact in a transient state (mid-rebase during cross-device sync) and
// cached the head without a durable export. Because seenHeads is persisted,
// a plain tick never retries and the conversation is permanently absent from
// Hermes. ReconcileInbound clears the cache so the next inbound pass re-exports
// it (InsertSession is content-hash idempotent, so re-export is otherwise a
// no-op).
func TestWatcher_ReconcileInbound_RecoversSessionMissingDespiteSeenCache(t *testing.T) {
	srcPath := filepath.Join(t.TempDir(), "src.db")
	src, err := hermesdb.InitTestDB(srcPath)
	require.NoError(t, err)
	_, err = src.Exec(`INSERT INTO sessions (id, source, started_at, title) VALUES ('reconcile-test','cli',100.0,'Reconcile')`)
	require.NoError(t, err)
	_, err = src.Exec(`INSERT INTO messages (session_id, role, content, timestamp) VALUES ('reconcile-test','user','hello',101.0)`)
	require.NoError(t, err)
	src.Close()

	storeRoot := filepath.Join(t.TempDir(), "store")
	store := &acf.Store{Root: storeRoot}
	require.NoError(t, store.Init())

	a := &hermes.Adapter{HomeDir: t.TempDir(), DeviceID: "test-dev"}

	wOut := &Watcher{Adapter: a, Store: store, DBPath: srcPath, Interval: 5 * time.Second}
	_, err = wOut.TickOutbound(context.Background())
	require.NoError(t, err)

	dstPath := filepath.Join(t.TempDir(), "dst.db")
	dstInit, err := hermesdb.InitTestDB(dstPath)
	require.NoError(t, err)
	dstInit.Close()

	wIn := &Watcher{Adapter: a, Store: store, DBPath: dstPath, Interval: 5 * time.Second}
	ids, err := wIn.TickInbound(context.Background())
	require.NoError(t, err)
	require.Len(t, ids, 1, "first inbound tick exports the conversation and caches its head")

	// Simulate the conversation vanishing from Hermes while the cache still
	// marks its head processed.
	rw, err := hermesdb.OpenRW(dstPath)
	require.NoError(t, err)
	_, err = rw.Exec(`DELETE FROM messages WHERE session_id='reconcile-test'`)
	require.NoError(t, err)
	_, err = rw.Exec(`DELETE FROM sessions WHERE id='reconcile-test'`)
	require.NoError(t, err)
	rw.Close()

	// A plain tick cannot recover it — seenHeads suppresses the re-export.
	ids2, err := wIn.TickInbound(context.Background())
	require.NoError(t, err)
	require.Empty(t, ids2, "plain tick is suppressed by the seenHeads cache")
	missing, err := hermesdb.ListSessions(dstPath, 0)
	require.NoError(t, err)
	require.Empty(t, missing, "documents the bug: the conversation stays missing from Hermes")

	// ReconcileInbound clears the cache and re-exports the missing conversation.
	ids3, err := wIn.ReconcileInbound(context.Background())
	require.NoError(t, err)
	require.Len(t, ids3, 1, "reconcile re-exports the missing conversation")
	recovered, err := hermesdb.ListSessions(dstPath, 0)
	require.NoError(t, err)
	require.Len(t, recovered, 1)
	require.Equal(t, "reconcile-test", recovered[0].Session.ID)
}

// Run's periodic reconcile (every ReconcileEvery ticks) recovers a session
// that is cached as "seen" but missing from the Hermes DB.
func TestWatcher_Run_PeriodicReconcileRecoversMissingSession(t *testing.T) {
	srcPath := filepath.Join(t.TempDir(), "src.db")
	src, err := hermesdb.InitTestDB(srcPath)
	require.NoError(t, err)
	_, err = src.Exec(`INSERT INTO sessions (id, source, started_at, title) VALUES ('run-reconcile','cli',100.0,'RunReconcile')`)
	require.NoError(t, err)
	_, err = src.Exec(`INSERT INTO messages (session_id, role, content, timestamp) VALUES ('run-reconcile','user','hi',101.0)`)
	require.NoError(t, err)
	src.Close()

	storeRoot := filepath.Join(t.TempDir(), "store")
	store := &acf.Store{Root: storeRoot}
	require.NoError(t, store.Init())

	a := &hermes.Adapter{HomeDir: t.TempDir(), DeviceID: "test-dev"}

	wOut := &Watcher{Adapter: a, Store: store, DBPath: srcPath, Interval: 5 * time.Second}
	_, err = wOut.TickOutbound(context.Background())
	require.NoError(t, err)

	dstPath := filepath.Join(t.TempDir(), "dst.db")
	dstInit, err := hermesdb.InitTestDB(dstPath)
	require.NoError(t, err)
	dstInit.Close()

	// Inbound-only watcher; export once so the head is cached, then delete the
	// session from the DB to recreate the "seen but missing" stuck state.
	wIn := &Watcher{Adapter: a, Store: store, DBPath: dstPath, Direction: DirectionInbound, Interval: 10 * time.Millisecond, ReconcileEvery: 2}
	_, err = wIn.TickInbound(context.Background())
	require.NoError(t, err)
	rw, err := hermesdb.OpenRW(dstPath)
	require.NoError(t, err)
	_, err = rw.Exec(`DELETE FROM messages WHERE session_id='run-reconcile'`)
	require.NoError(t, err)
	_, err = rw.Exec(`DELETE FROM sessions WHERE id='run-reconcile'`)
	require.NoError(t, err)
	rw.Close()

	ctx, cancel := context.WithCancel(context.Background())
	// Join the Run goroutine on the way out: cancel signals shutdown, then we
	// block until Run has fully returned. Without the join, a bare cancel only
	// *requests* a stop — the goroutine can still be mid-tick, writing SQLite
	// files under dstPath, when the test returns and t.TempDir's RemoveAll
	// cleanup runs ("directory not empty"). This defer runs before any
	// t.TempDir cleanup (test-body defers fire before t.Cleanup callbacks) and
	// also covers the require.Eventually timeout path (t.Fatal -> Goexit still
	// runs defers).
	done := make(chan struct{})
	go func() { defer close(done); _ = wIn.Run(ctx) }()
	defer func() {
		cancel()
		<-done
	}()

	require.Eventually(t, func() bool {
		got, lerr := hermesdb.ListSessions(dstPath, 0)
		return lerr == nil && len(got) == 1
	}, 2*time.Second, 10*time.Millisecond, "periodic reconcile should re-export the missing session")
}

func TestWatcher_TickInbound_IgnoresNonHermesFormat(t *testing.T) {
	storeRoot := filepath.Join(t.TempDir(), "store")
	store := &acf.Store{Root: storeRoot}
	require.NoError(t, store.Init())

	// Manually write a conversation artifact with a NON-hermes format.
	id := acf.NewID()
	now := time.Now().UTC()
	art := acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion,
		ArtifactID:       id,
		Kind:             acf.KindConversation,
		Scope:            acf.ScopeProject,
		Name:             "not-hermes",
		SourcePath:       "/tmp/fake.jsonl",
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	require.NoError(t, store.WriteArtifact(art))

	payload, err := acf.EncodePayload(acf.ConversationPayload{
		Format:  "claude-code.session.jsonl",
		Content: "fake jsonl bytes",
	})
	require.NoError(t, err)
	require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
		EventID:    acf.NewID(),
		ArtifactID: id,
		Type:       acf.EventTypeCreate,
		Timestamp:  now,
		Payload:    payload,
	}))

	dstPath := filepath.Join(t.TempDir(), "dst.db")
	dstInit, err := hermesdb.InitTestDB(dstPath)
	require.NoError(t, err)
	dstInit.Close()

	a := &hermes.Adapter{HomeDir: t.TempDir(), DeviceID: "test-dev"}
	w := &Watcher{Adapter: a, Store: store, DBPath: dstPath, Interval: 5 * time.Second}

	ids, err := w.TickInbound(context.Background())
	require.NoError(t, err)
	require.Empty(t, ids, "non-hermes conversation formats must be ignored by TickInbound")

	// Verify dst.db has no sessions.
	got, err := hermesdb.ListSessions(dstPath, 0)
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestWatcher_SaveLoadState_RoundTrip(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	storeRoot := filepath.Join(t.TempDir(), "store")
	store := &acf.Store{Root: storeRoot}
	require.NoError(t, store.Init())
	a := &hermes.Adapter{HomeDir: t.TempDir(), DeviceID: "test-dev"}

	w := &Watcher{Adapter: a, Store: store, DBPath: "/dev/null", StateFile: statePath}
	w.SetHWM(123.5)
	w.mu.Lock()
	w.seenHeads = map[string]string{"art-1": "hash-a", "art-2": "hash-b"}
	w.failedHeads = map[string]string{"art-3": "hash-c"}
	w.mu.Unlock()
	require.NoError(t, w.SaveState())

	w2 := &Watcher{Adapter: a, Store: store, DBPath: "/dev/null", StateFile: statePath}
	require.NoError(t, w2.LoadState())
	require.InDelta(t, 123.5, w2.HWM(), 0.001)
	w2.mu.Lock()
	require.Equal(t, "hash-a", w2.seenHeads["art-1"])
	require.Equal(t, "hash-b", w2.seenHeads["art-2"])
	require.Equal(t, "hash-c", w2.failedHeads["art-3"])
	w2.mu.Unlock()
}

func TestWatcher_LoadState_DropsOldFormatGateCaches(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	require.NoError(t, os.WriteFile(statePath, []byte(`{
  "hwm": 123.5,
  "seen_heads": {
    "baseline-skipped-by-old-binary": "hash-a"
  },
  "failed_heads": {
    "terminal-failure": "hash-b"
  }
}`), 0o644))

	w := &Watcher{StateFile: statePath}
	require.NoError(t, w.LoadState())
	require.Equal(t, 123.5, w.hwm)
	require.Empty(t, w.seenHeads, "old seen_heads were written before baseline-aware filtering and must be re-evaluated")
	require.Empty(t, w.failedHeads, "old failed_heads may contain cancellation false positives and must be re-evaluated")
}

func TestWatcher_LoadState_MissingFileIsSilent(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "does-not-exist.json")
	storeRoot := filepath.Join(t.TempDir(), "store")
	store := &acf.Store{Root: storeRoot}
	require.NoError(t, store.Init())
	a := &hermes.Adapter{HomeDir: t.TempDir(), DeviceID: "test-dev"}

	w := &Watcher{Adapter: a, Store: store, DBPath: "/dev/null", StateFile: statePath}
	require.NoError(t, w.LoadState(), "missing state file must NOT error")
	require.Equal(t, float64(0), w.HWM())
}

func TestWatcher_SaveState_NoOpWhenStateFileEmpty(t *testing.T) {
	storeRoot := filepath.Join(t.TempDir(), "store")
	store := &acf.Store{Root: storeRoot}
	require.NoError(t, store.Init())
	a := &hermes.Adapter{HomeDir: t.TempDir(), DeviceID: "test-dev"}

	w := &Watcher{Adapter: a, Store: store, DBPath: "/dev/null"} // StateFile empty
	w.SetHWM(99.0)
	require.NoError(t, w.SaveState(), "no StateFile means SaveState is a silent no-op")
}

// TestWatcher_SetInterval covers the "Run not active" branch: the field is
// updated, and the no-channel-registered case must NOT panic. The Run-active
// branch (ticker re-arm on signal) is exercised indirectly by Run-exit and
// integration tests; verifying the goroutine swap deterministically would
// require a fakeClock injection that isn't worth the complexity for a field
// that operators set rarely (SIGHUP, not per-request).
func TestWatcher_SetInterval(t *testing.T) {
	w := &Watcher{Interval: 5 * time.Second}
	w.SetInterval(10 * time.Second)
	require.Equal(t, 10*time.Second, w.Interval)
	// And again — exercising the non-blocking send default branch path
	// is harmless when no channel is registered.
	w.SetInterval(2 * time.Second)
	require.Equal(t, 2*time.Second, w.Interval)
}

func TestWatcher_Tick_RunsBothDirections(t *testing.T) {
	srcPath := filepath.Join(t.TempDir(), "src.db")
	src, err := hermesdb.InitTestDB(srcPath)
	require.NoError(t, err)
	_, err = src.Exec(`INSERT INTO sessions (id, source, started_at) VALUES ('both','cli',100.0)`)
	require.NoError(t, err)
	_, err = src.Exec(`INSERT INTO messages (session_id, role, content, timestamp) VALUES ('both','user','m',101.0)`)
	require.NoError(t, err)
	src.Close()

	storeRoot := filepath.Join(t.TempDir(), "store")
	store := &acf.Store{Root: storeRoot}
	require.NoError(t, store.Init())

	a := &hermes.Adapter{HomeDir: t.TempDir(), DeviceID: "test-dev"}
	w := &Watcher{
		Adapter:   a,
		Store:     store,
		DBPath:    srcPath,
		Interval:  5 * time.Second,
		Direction: DirectionBoth,
	}
	ids, err := w.Tick(context.Background())
	require.NoError(t, err)
	require.NotEmpty(t, ids, "DirectionBoth tick should run both outbound (export new session) and inbound (no-op, source is the same DB)")
}

// countingLogger counts Error calls. TickInbound invokes the logger
// synchronously, so no locking is needed for single-threaded tests.
type countingLogger struct{ errors int }

func (l *countingLogger) Info(string, ...any)  {}
func (l *countingLogger) Error(string, ...any) { l.errors++ }

// TestWatcher_TickInbound_DoesNotRetryAfterPersistentFailure is the regression
// guard for the daemon-log flood: an artifact that passes the format gate (its
// latest payload IS a hermes format) but fails to export (malformed payload)
// must be logged ONCE and have its head cached, so subsequent ticks with an
// unchanged head neither re-attempt nor re-log it. Before the fix, such an
// artifact was retried — and its ERROR re-logged — on every ~5s tick forever.
func TestWatcher_TickInbound_DoesNotRetryAfterPersistentFailure(t *testing.T) {
	storeRoot := filepath.Join(t.TempDir(), "store")
	store := &acf.Store{Root: storeRoot}
	require.NoError(t, store.Init())

	id := acf.NewID()
	now := time.Now().UTC()
	require.NoError(t, store.WriteArtifact(acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion,
		ArtifactID:       id,
		Kind:             acf.KindConversation,
		Scope:            acf.ScopeProject,
		Name:             "broken",
		SourcePath:       "/tmp/broken.json",
		CreatedAt:        now,
		UpdatedAt:        now,
	}))

	// Latest payload declares a hermes format (so it passes isHermesBundleArtifact)
	// but carries invalid SessionBundle JSON, so ExportConversationsToDB fails.
	payload, err := acf.EncodePayload(acf.ConversationPayload{
		Format:  hermes.SessionBundleFormat,
		Content: "{ this is not valid session-bundle json",
	})
	require.NoError(t, err)
	require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
		EventID: acf.NewID(), ArtifactID: id, Type: acf.EventTypeCreate, Timestamp: now, Payload: payload,
	}))

	dstPath := filepath.Join(t.TempDir(), "dst.db")
	dstInit, err := hermesdb.InitTestDB(dstPath)
	require.NoError(t, err)
	dstInit.Close()

	lg := &countingLogger{}
	a := &hermes.Adapter{HomeDir: t.TempDir(), DeviceID: "test-dev"}
	w := &Watcher{Adapter: a, Store: store, DBPath: dstPath, Interval: 5 * time.Second, Logger: lg}

	// Tick 1: export fails → logged once, head cached, nothing written.
	ids, err := w.TickInbound(context.Background())
	require.NoError(t, err, "one artifact's export failure must not fail the whole tick")
	require.Empty(t, ids)
	require.Equal(t, 1, lg.errors, "the failure is logged exactly once")

	// Ticks 2 & 3 with unchanged head: NOT re-attempted, NOT re-logged.
	for i := 0; i < 2; i++ {
		ids, err = w.TickInbound(context.Background())
		require.NoError(t, err)
		require.Empty(t, ids)
	}
	require.Equal(t, 1, lg.errors,
		"an unchanged failing artifact must NOT be re-logged on every tick (anti-flood)")

	// A full inbound reconcile clears seenHeads to recover previously skipped
	// sessions, but unchanged terminal failures must remain suppressed. Before
	// failedHeads persisted terminal failures separately, this re-logged and
	// re-verified the same broken artifact on every reconcile sweep.
	ids, err = w.ReconcileInbound(context.Background())
	require.NoError(t, err)
	require.Empty(t, ids)
	require.Equal(t, 1, lg.errors,
		"an unchanged failing artifact must NOT be re-logged by full reconcile")
}
