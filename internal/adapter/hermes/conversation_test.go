package hermes

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/hermesdb"
	"github.com/aplexica/aplexica/internal/retention"
	"github.com/stretchr/testify/require"
)

func TestImportConversationsFromDB_OneArtifactPerSession(t *testing.T) {
	srcPath := filepath.Join(t.TempDir(), "src.db")
	src, err := hermesdb.InitTestDB(srcPath)
	require.NoError(t, err)
	_, err = src.Exec(`INSERT INTO sessions (id, source, started_at, title) VALUES ('s1','cli',100.0,'first')`)
	require.NoError(t, err)
	_, err = src.Exec(`INSERT INTO sessions (id, source, started_at, title) VALUES ('s2','cli',200.0,'second')`)
	require.NoError(t, err)
	_, err = src.Exec(`INSERT INTO messages (session_id, role, content, timestamp) VALUES ('s1','user','hi',101.0)`)
	require.NoError(t, err)
	src.Close()

	storeRoot := filepath.Join(t.TempDir(), "store")
	store := &acf.Store{Root: storeRoot}
	require.NoError(t, store.Init())

	a := newTestAdapter(t)
	ids, err := a.ImportConversationsFromDB(context.Background(), store, srcPath, 0)
	require.NoError(t, err)
	require.Len(t, ids, 2)

	for _, id := range ids {
		art, err := store.ReadArtifact(acf.KindConversation, id)
		require.NoError(t, err)
		require.Equal(t, acf.KindConversation, art.Kind)
		require.Equal(t, acf.ScopeGlobal, art.Scope)
		require.Contains(t, art.SourcePath, "#session=")
	}
}

func TestRoundTrip_HermesToHermes(t *testing.T) {
	srcPath := filepath.Join(t.TempDir(), "src.db")
	src, err := hermesdb.InitTestDB(srcPath)
	require.NoError(t, err)
	_, err = src.Exec(`INSERT INTO sessions (id, source, started_at) VALUES ('sX','cli',100.0)`)
	require.NoError(t, err)
	_, err = src.Exec(`INSERT INTO messages (session_id, role, content, timestamp) VALUES ('sX','user','question',101.0)`)
	require.NoError(t, err)
	_, err = src.Exec(`INSERT INTO messages (session_id, role, content, timestamp) VALUES ('sX','assistant','answer',102.0)`)
	require.NoError(t, err)
	src.Close()

	storeRoot := filepath.Join(t.TempDir(), "store")
	store := &acf.Store{Root: storeRoot}
	require.NoError(t, store.Init())

	a := newTestAdapter(t)
	ids, err := a.ImportConversationsFromDB(context.Background(), store, srcPath, 0)
	require.NoError(t, err)
	require.Len(t, ids, 1)

	dstPath := filepath.Join(t.TempDir(), "dst.db")
	dst, err := hermesdb.InitTestDB(dstPath)
	require.NoError(t, err)
	dst.Close()

	require.NoError(t, a.ExportConversationsToDB(context.Background(), store, ids[0], dstPath))

	got, err := hermesdb.ListSessions(dstPath, 0)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "sX", got[0].Session.ID)
	require.Len(t, got[0].Messages, 2)
	require.Equal(t, "question", *got[0].Messages[0].Content)
	require.Equal(t, "answer", *got[0].Messages[1].Content)
}

func TestExportConversationsToDB_SkipsHiddenOnlyThread(t *testing.T) {
	storeRoot := filepath.Join(t.TempDir(), "store")
	store := &acf.Store{Root: storeRoot}
	require.NoError(t, store.Init())

	now := time.Now().UTC()
	id := acf.NewID()
	require.NoError(t, store.WriteArtifact(acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion,
		ArtifactID:       id,
		Kind:             acf.KindConversation,
		Scope:            acf.ScopeGlobal,
		Name:             "command-only",
		CreatedAt:        now,
		UpdatedAt:        now,
	}))
	payload, err := acf.EncodePayload(acf.ConversationPayload{
		Format: acf.ConversationFormatV1,
		Events: []acf.ConversationEvent{
			{Type: acf.EventTypeTurn, Timestamp: now, Role: "user", Content: []acf.ContentBlock{{Type: "text", Text: "<command-name>/model</command-name>"}}},
			{Type: acf.EventTypeTurn, Timestamp: now, Role: "user", Content: []acf.ContentBlock{{Type: "text", Text: "<local-command-stdout>Set model to Opus 4.8</local-command-stdout>"}}},
		},
	})
	require.NoError(t, err)
	require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
		EventID:    acf.NewID(),
		ArtifactID: id,
		Type:       acf.EventTypeCreate,
		Timestamp:  now,
		Provenance: acf.Provenance{SourceAgent: "claude-code"},
		Payload:    payload,
	}))

	dstPath := filepath.Join(t.TempDir(), "dst.db")
	dst, err := hermesdb.InitTestDB(dstPath)
	require.NoError(t, err)
	dst.Close()

	a := newTestAdapter(t)
	require.NoError(t, a.ExportConversationsToDB(context.Background(), store, id, dstPath))
	got, err := hermesdb.ListSessions(dstPath, 0)
	require.NoError(t, err)
	require.Empty(t, got, "hidden command-only artifacts must not create blank Hermes sessions")
}

// A conversation MATERIALIZED into Hermes (source=canonical-import, session id ==
// the canonical thread id) that the user then CONTINUES — by resuming it in
// Hermes and adding a turn — must have the new turn APPENDED to the ORIGINAL
// conversation, so it propagates back to every other agent/device. The old
// blanket canonical-import skip stranded such continuations in the Hermes DB
// forever (reached neither sibling agents nor the relay). Regression for that.
func TestImportConversationsFromDB_ContinuedCanonicalImportSession_Appends(t *testing.T) {
	storeRoot := filepath.Join(t.TempDir(), "store")
	store := &acf.Store{Root: storeRoot}
	require.NoError(t, store.Init())
	a := newTestAdapter(t)

	// Seed a canonical conversation artifact (stand-in for a synced thread): import
	// a native hermes session, capturing the canonical artifact id it mints.
	seedPath := filepath.Join(t.TempDir(), "seed.db")
	seed, err := hermesdb.InitTestDB(seedPath)
	require.NoError(t, err)
	_, err = seed.Exec(`INSERT INTO sessions (id, source, started_at, title) VALUES ('orig','cli',100.0,'q')`)
	require.NoError(t, err)
	_, err = seed.Exec(`INSERT INTO messages (session_id, role, content, timestamp) VALUES ('orig','user','what is 60+60',101.0)`)
	require.NoError(t, err)
	_, err = seed.Exec(`INSERT INTO messages (session_id, role, content, timestamp) VALUES ('orig','assistant','120',102.0)`)
	require.NoError(t, err)
	seed.Close()
	ids, err := a.ImportConversationsFromDB(context.Background(), store, seedPath, 0)
	require.NoError(t, err)
	require.Len(t, ids, 1)
	threadID := ids[0]

	// Materialized-then-continued session: id == the canonical thread id,
	// source=canonical-import, original two turns PLUS a new continuation.
	contPath := filepath.Join(t.TempDir(), "cont.db")
	cont, err := hermesdb.InitTestDB(contPath)
	require.NoError(t, err)
	_, err = cont.Exec(`INSERT INTO sessions (id, source, started_at, title) VALUES (?, 'aplexica:canonical-import', 100.0, 'q')`, threadID)
	require.NoError(t, err)
	for _, m := range []struct {
		role, content string
		ts            float64
	}{
		{"user", "what is 60+60", 101.0}, {"assistant", "120", 102.0},
		{"user", "what is 70+70", 103.0}, {"assistant", "140", 104.0},
	} {
		_, err = cont.Exec(`INSERT INTO messages (session_id, role, content, timestamp) VALUES (?,?,?,?)`, threadID, m.role, m.content, m.ts)
		require.NoError(t, err)
	}
	cont.Close()

	contIDs, err := a.ImportConversationsFromDB(context.Background(), store, contPath, 0)
	require.NoError(t, err)
	require.Equal(t, []string{threadID}, contIDs,
		"a continued canonical-import session must append to the ORIGINAL thread, not be skipped")

	// Exactly one conversation artifact (no fork/duplicate), and it now carries
	// all four turns including the 70+70 continuation.
	arts, err := store.ListArtifacts(acf.KindConversation)
	require.NoError(t, err)
	require.Len(t, arts, 1, "continuation must reconcile to the original artifact, not fork a duplicate")

	dstPath := filepath.Join(t.TempDir(), "dst.db")
	dst, err := hermesdb.InitTestDB(dstPath)
	require.NoError(t, err)
	dst.Close()
	require.NoError(t, a.ExportConversationsToDB(context.Background(), store, threadID, dstPath))
	got, err := hermesdb.ListSessions(dstPath, 0)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Len(t, got[0].Messages, 4, "the continued thread must carry all four turns")
	require.Equal(t, "what is 70+70", *got[0].Messages[2].Content)
}

// TestExportConversationsToDB_AfterSnapshotAndPrune covers the confirmed P1
// retention regression: once retention.CreateSnapshot + PruneArtifact run on a
// main-only conversation, every pre-snapshot create/update event moves into the
// .compacted layer and the ACTIVE log holds only the snapshot event. A reader
// that walks just the active log must still re-materialize the session into a
// fresh ~/.hermes/state.db. Without the fix the active log yields no exportable
// payload (the snapshot is the only active event) and the session silently
// fails to materialize (0 sessions).
func TestExportConversationsToDB_AfterSnapshotAndPrune(t *testing.T) {
	srcPath := filepath.Join(t.TempDir(), "src.db")
	src, err := hermesdb.InitTestDB(srcPath)
	require.NoError(t, err)
	_, err = src.Exec(`INSERT INTO sessions (id, source, started_at) VALUES ('sP','cli',100.0)`)
	require.NoError(t, err)
	_, err = src.Exec(`INSERT INTO messages (session_id, role, content, timestamp) VALUES ('sP','user','survives prune',101.0)`)
	require.NoError(t, err)
	src.Close()

	storeRoot := filepath.Join(t.TempDir(), "store")
	store := &acf.Store{Root: storeRoot}
	require.NoError(t, store.Init())

	a := newTestAdapter(t)
	ids, err := a.ImportConversationsFromDB(context.Background(), store, srcPath, 0)
	require.NoError(t, err)
	require.Len(t, ids, 1)
	id := ids[0]

	// Snapshot, then prune. After this the active log is snapshot-only and the
	// create event lives in .compacted (this is the exact production sequence
	// the retention engine runs on a main-only conversation).
	_, err = retention.CreateSnapshot(context.Background(), store, acf.KindConversation, id)
	require.NoError(t, err)
	moved, _, err := retention.PruneArtifact(context.Background(), store, acf.KindConversation, id, time.Now().Add(-time.Hour))
	require.NoError(t, err)
	require.Equal(t, 1, moved, "the single pre-snapshot create event must move to .compacted")

	active, err := store.ReadEvents(acf.KindConversation, id)
	require.NoError(t, err)
	require.Len(t, active, 1, "active log is snapshot-only after prune")
	require.Equal(t, acf.EventType(acf.EventTypeSnapshot), active[0].Type)

	dstPath := filepath.Join(t.TempDir(), "dst.db")
	dst, err := hermesdb.InitTestDB(dstPath)
	require.NoError(t, err)
	dst.Close()

	require.NoError(t, a.ExportConversationsToDB(context.Background(), store, id, dstPath),
		"export must still materialize the session after snapshot+prune")

	got, err := hermesdb.ListSessions(dstPath, 0)
	require.NoError(t, err)
	require.Len(t, got, 1, "the pruned conversation must still materialize into a fresh DB (>0 sessions)")
	require.Len(t, got[0].Messages, 1)
	require.Equal(t, "survives prune", *got[0].Messages[0].Content)
}

// TestExportConversationsToDB_LegacyPayloadlessSnapshotAndPrune isolates
// DEFENSE (1): a snapshot written BEFORE the FR-02.32 change carries NO payload.
// After prune the active log is that payload-less snapshot alone — so the root
// fix (payload-bearing snapshot) does NOT apply and the ONLY thing that can
// re-materialize the session is the fallback to ReadEventsIncludingCompacted.
// This proves defense-in-depth holds for legacy snapshots already in the wild.
func TestExportConversationsToDB_LegacyPayloadlessSnapshotAndPrune(t *testing.T) {
	storeRoot := filepath.Join(t.TempDir(), "store")
	store := &acf.Store{Root: storeRoot}
	require.NoError(t, store.Init())

	now := time.Now().UTC()
	id := acf.NewID()
	require.NoError(t, store.WriteArtifact(acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion,
		ArtifactID:       id,
		Kind:             acf.KindConversation,
		Scope:            acf.ScopeGlobal,
		Name:             "legacy-snap",
		CreatedAt:        now,
		UpdatedAt:        now,
	}))

	canonical, err := acf.EncodePayload(acf.ConversationPayload{
		Format: acf.ConversationFormatV1,
		Events: EncodeBundleAsCanonical(hermesdb.SessionBundle{
			Session:  hermesdb.SessionRow{ID: "ignored", Source: "cli", StartedAt: 100.0},
			Messages: []hermesdb.MessageRow{{Role: "user", Content: ptrString("legacy survives"), Timestamp: 101.0}},
		}),
	})
	require.NoError(t, err)
	require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
		EventID: acf.NewID(), ArtifactID: id, Type: acf.EventTypeCreate, Timestamp: now, Payload: canonical,
	}))

	// Append a PAYLOAD-LESS snapshot by hand (the pre-FR-02.32 shape).
	head, err := store.ReadArtifact(acf.KindConversation, id)
	require.NoError(t, err)
	require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
		EventID:       acf.NewID(),
		ArtifactID:    id,
		Type:          acf.EventTypeSnapshot,
		Timestamp:     now.Add(time.Second),
		ParentHash:    head.HeadEventHash,
		SnapshotState: "sha256:deadbeef",
		Payload:       nil,
	}))

	// Prune: the create event moves to .compacted, leaving the payload-less
	// snapshot alone in the active log.
	moved, _, err := retention.PruneArtifact(context.Background(), store, acf.KindConversation, id, time.Now().Add(-time.Hour))
	require.NoError(t, err)
	require.Equal(t, 1, moved)

	active, err := store.ReadEvents(acf.KindConversation, id)
	require.NoError(t, err)
	require.Len(t, active, 1)
	require.Equal(t, acf.EventType(acf.EventTypeSnapshot), active[0].Type)
	// A nil-payload snapshot serializes to `"payload":null` and reads back as
	// the 4-byte literal `null` (Event.Payload has no omitempty), so it has no
	// real body — acf.HasPayload distinguishes that from a genuine payload.
	require.False(t, acf.HasPayload(active[0].Payload), "this snapshot is intentionally payload-less (legacy shape)")

	dstPath := filepath.Join(t.TempDir(), "dst.db")
	dst, err := hermesdb.InitTestDB(dstPath)
	require.NoError(t, err)
	dst.Close()

	a := newTestAdapter(t)
	require.NoError(t, a.ExportConversationsToDB(context.Background(), store, id, dstPath),
		"the compacted-layer fallback must re-materialize even a payload-less snapshot")

	got, err := hermesdb.ListSessions(dstPath, 0)
	require.NoError(t, err)
	require.Len(t, got, 1, "fallback to ReadEventsIncludingCompacted must materialize the session")
	require.Len(t, got[0].Messages, 1)
	require.Equal(t, "legacy survives", *got[0].Messages[0].Content)
}

// TestExportConversationsToDB_CorruptChainStillErrors proves the fallback does
// NOT mask genuine corruption: an active log whose chain is broken (and has no
// .compacted layer to repair it) must still fail with a clear "event log
// invalid" error, exactly as before the prune-resilience fallback was added.
func TestExportConversationsToDB_CorruptChainStillErrors(t *testing.T) {
	storeRoot := filepath.Join(t.TempDir(), "store")
	store := &acf.Store{Root: storeRoot}
	require.NoError(t, store.Init())

	now := time.Now().UTC()
	id := acf.NewID()
	require.NoError(t, store.WriteArtifact(acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion, ArtifactID: id, Kind: acf.KindConversation,
		Scope: acf.ScopeGlobal, Name: "corrupt", CreatedAt: now, UpdatedAt: now,
	}))
	canonical, err := acf.EncodePayload(acf.ConversationPayload{
		Format: acf.ConversationFormatV1,
		Events: EncodeBundleAsCanonical(hermesdb.SessionBundle{
			Session:  hermesdb.SessionRow{ID: "ignored", Source: "cli", StartedAt: 100.0},
			Messages: []hermesdb.MessageRow{{Role: "user", Content: ptrString("x"), Timestamp: 101.0}},
		}),
	})
	require.NoError(t, err)
	// Write a create event with a BOGUS non-empty ParentHash DIRECTLY to the
	// events file (store.AppendEvent would reject it). A genesis event must have
	// ParentHash "", so VerifyChain fails. The event's own Hash is set correctly
	// so the failure is specifically the chain break, not a hash mismatch. No
	// .compacted file exists, so the fallback cannot repair it.
	corrupt := acf.Event{
		EventID: acf.NewID(), ArtifactID: id, Type: acf.EventTypeCreate, Timestamp: now,
		Payload: canonical, ParentHash: "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
	}
	corrupt.Hash, err = acf.ComputeHash(corrupt)
	require.NoError(t, err)
	line, err := json.Marshal(corrupt)
	require.NoError(t, err)
	eventsPath := filepath.Join(storeRoot, "events", "conversations", id+".jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(eventsPath), 0o755))
	require.NoError(t, os.WriteFile(eventsPath, append(line, '\n'), 0o644))

	dstPath := filepath.Join(t.TempDir(), "dst.db")
	dst, err := hermesdb.InitTestDB(dstPath)
	require.NoError(t, err)
	dst.Close()

	a := newTestAdapter(t)
	err = a.ExportConversationsToDB(context.Background(), store, id, dstPath)
	require.Error(t, err, "a genuinely corrupt chain must still error")
	require.Contains(t, err.Error(), "event log invalid",
		"corruption must surface as 'event log invalid', not the generic no-payload message")
}

// TestExportConversationsToDB_RedactionHeadDoesNotResurrect proves a redaction
// is authoritative: when the latest mutating event is a redaction, export must
// drop the session and must NOT fall back to the compacted layer to resurrect a
// pre-redaction payload.
func TestExportConversationsToDB_RedactionHeadDoesNotResurrect(t *testing.T) {
	storeRoot := filepath.Join(t.TempDir(), "store")
	store := &acf.Store{Root: storeRoot}
	require.NoError(t, store.Init())

	now := time.Now().UTC()
	id := acf.NewID()
	require.NoError(t, store.WriteArtifact(acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion, ArtifactID: id, Kind: acf.KindConversation,
		Scope: acf.ScopeGlobal, Name: "redacted", CreatedAt: now, UpdatedAt: now,
	}))
	canonical, err := acf.EncodePayload(acf.ConversationPayload{
		Format: acf.ConversationFormatV1,
		Events: EncodeBundleAsCanonical(hermesdb.SessionBundle{
			Session:  hermesdb.SessionRow{ID: "ignored", Source: "cli", StartedAt: 100.0},
			Messages: []hermesdb.MessageRow{{Role: "user", Content: ptrString("secret"), Timestamp: 101.0}},
		}),
	})
	require.NoError(t, err)
	require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
		EventID: acf.NewID(), ArtifactID: id, Type: acf.EventTypeCreate, Timestamp: now, Payload: canonical,
	}))
	head, err := store.ReadArtifact(acf.KindConversation, id)
	require.NoError(t, err)
	require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
		EventID: acf.NewID(), ArtifactID: id, Type: acf.EventTypeRedaction,
		Timestamp: now.Add(time.Second), ParentHash: head.HeadEventHash,
	}))

	dstPath := filepath.Join(t.TempDir(), "dst.db")
	dst, err := hermesdb.InitTestDB(dstPath)
	require.NoError(t, err)
	dst.Close()

	a := newTestAdapter(t)
	err = a.ExportConversationsToDB(context.Background(), store, id, dstPath)
	require.Error(t, err, "a redacted artifact has nothing to export")
	require.Contains(t, err.Error(), "nothing to export")

	got, lerr := hermesdb.ListSessions(dstPath, 0)
	require.NoError(t, lerr)
	require.Len(t, got, 0, "redaction must not resurrect the pre-redaction payload into the DB")
}

// TestExportConversationsToDB_MixedFormatLog_ExportsLatest covers an artifact
// whose event log MIXES formats: it was first imported as a foreign
// claude-code.session.jsonl conversation and later updated with canonical
// acf.conversation.v1 events. Export must materialize from the latest decodable
// payload
// and must NOT choke on the historical foreign event, which only the hermes
// adapter's caller would otherwise retry forever.
func TestExportConversationsToDB_MixedFormatLog_ExportsLatest(t *testing.T) {
	storeRoot := filepath.Join(t.TempDir(), "store")
	store := &acf.Store{Root: storeRoot}
	require.NoError(t, store.Init())

	now := time.Now().UTC()
	id := acf.NewID()
	require.NoError(t, store.WriteArtifact(acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion,
		ArtifactID:       id,
		Kind:             acf.KindConversation,
		Scope:            acf.ScopeProject,
		Name:             "mixed-format",
		SourcePath:       "/tmp/claude.jsonl",
		CreatedAt:        now,
		UpdatedAt:        now,
	}))

	// create: a FOREIGN claude-code.session.jsonl payload hermes cannot decode.
	foreign, err := acf.EncodePayload(acf.ConversationPayload{
		Format:  "claude-code.session.jsonl",
		Content: "raw claude jsonl bytes hermes cannot parse",
	})
	require.NoError(t, err)
	require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
		EventID: acf.NewID(), ArtifactID: id, Type: acf.EventTypeCreate, Timestamp: now, Payload: foreign,
	}))

	// A superseded payload may even be valid JSON that cannot decode into a
	// ConversationPayload. Candidate collection and latest-state replay must
	// ignore it once a later full canonical payload replaces it.
	head, err := store.ReadArtifact(acf.KindConversation, id)
	require.NoError(t, err)
	require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
		EventID:    acf.NewID(),
		ArtifactID: id,
		Type:       acf.EventTypeUpdate,
		Timestamp:  now.Add(500 * time.Millisecond),
		ParentHash: head.HeadEventHash,
		Payload:    json.RawMessage(`{"format":{"not":"a string"}}`),
	}))

	// update: the artifact later switched to canonical acf.conversation.v1.
	head, err = store.ReadArtifact(acf.KindConversation, id)
	require.NoError(t, err)
	canonical, err := acf.EncodePayload(acf.ConversationPayload{
		Format: acf.ConversationFormatV1,
		Events: EncodeBundleAsCanonical(hermesdb.SessionBundle{
			Session:  hermesdb.SessionRow{ID: "ignored", Source: "cli", StartedAt: 100.0},
			Messages: []hermesdb.MessageRow{{Role: "user", Content: ptrString("hello canonical"), Timestamp: 101.0}},
		}),
	})
	require.NoError(t, err)
	require.NoError(t, store.AppendEvent(acf.KindConversation, acf.Event{
		EventID:    acf.NewID(),
		ArtifactID: id,
		Type:       acf.EventTypeUpdate,
		Timestamp:  now.Add(time.Second),
		ParentHash: head.HeadEventHash,
		Payload:    canonical,
	}))

	dstPath := filepath.Join(t.TempDir(), "dst.db")
	dst, err := hermesdb.InitTestDB(dstPath)
	require.NoError(t, err)
	dst.Close()

	a := newTestAdapter(t)
	require.NoError(t, a.ExportConversationsToDB(context.Background(), store, id, dstPath),
		"export must succeed from the latest canonical payload despite the foreign historical event")

	got, err := hermesdb.ListSessions(dstPath, 0)
	require.NoError(t, err)
	require.Len(t, got, 1, "the canonical latest payload must materialize into the destination DB")
	require.Len(t, got[0].Messages, 1)
	require.Equal(t, "hello canonical", *got[0].Messages[0].Content)
}

func TestImportConversationsFromDB_IdentityReconciliation(t *testing.T) {
	srcPath := filepath.Join(t.TempDir(), "src.db")
	src, err := hermesdb.InitTestDB(srcPath)
	require.NoError(t, err)
	_, err = src.Exec(`INSERT INTO sessions (id, source, started_at) VALUES ('rec','cli',100.0)`)
	require.NoError(t, err)
	src.Close()

	storeRoot := filepath.Join(t.TempDir(), "store")
	store := &acf.Store{Root: storeRoot}
	require.NoError(t, store.Init())

	a := newTestAdapter(t)

	// First import
	ids1, err := a.ImportConversationsFromDB(context.Background(), store, srcPath, 0)
	require.NoError(t, err)
	require.Len(t, ids1, 1)

	// Modify the DB so the second import is not a no-op, then re-import → update event.
	db2, err := hermesdb.OpenRW(srcPath)
	require.NoError(t, err)
	_, err = db2.Exec(`INSERT INTO messages (session_id, role, content, timestamp) VALUES ('rec','user','m',200.0)`)
	require.NoError(t, err)
	db2.Close()

	ids2, err := a.ImportConversationsFromDB(context.Background(), store, srcPath, 0)
	require.NoError(t, err)
	require.Len(t, ids2, 1)
	require.Equal(t, ids1[0], ids2[0], "identity reconciliation: same session must reuse artifact ID")

	events, err := store.ReadEvents(acf.KindConversation, ids1[0])
	require.NoError(t, err)
	require.Len(t, events, 2)
	require.Equal(t, acf.EventTypeCreate, events[0].Type)
	require.Equal(t, acf.EventTypeUpdate, events[1].Type)
}

func TestImportConversationsFromDB_SkipsWhenUnchanged(t *testing.T) {
	srcPath := filepath.Join(t.TempDir(), "src.db")
	src, err := hermesdb.InitTestDB(srcPath)
	require.NoError(t, err)
	_, err = src.Exec(`INSERT INTO sessions (id, source, started_at) VALUES ('skip-test','cli',100.0)`)
	require.NoError(t, err)
	_, err = src.Exec(`INSERT INTO messages (session_id, role, content, timestamp) VALUES ('skip-test','user','m',101.0)`)
	require.NoError(t, err)
	src.Close()

	storeRoot := filepath.Join(t.TempDir(), "store")
	store := &acf.Store{Root: storeRoot}
	require.NoError(t, store.Init())

	a := newTestAdapter(t)

	// First import → create event
	ids1, err := a.ImportConversationsFromDB(context.Background(), store, srcPath, 0)
	require.NoError(t, err)
	require.Len(t, ids1, 1)
	id := ids1[0]

	events1, err := store.ReadEvents(acf.KindConversation, id)
	require.NoError(t, err)
	require.Len(t, events1, 1, "first import: one create event")
	require.Equal(t, acf.EventTypeCreate, events1[0].Type)

	// Second import with NO source-db change → no new event
	ids2, err := a.ImportConversationsFromDB(context.Background(), store, srcPath, 0)
	require.NoError(t, err)
	require.Len(t, ids2, 1, "the artifact ID is still returned (caller may want to track it)")
	require.Equal(t, id, ids2[0])

	events2, err := store.ReadEvents(acf.KindConversation, id)
	require.NoError(t, err)
	require.Len(t, events2, 1, "second import without changes: still one event (no spurious update)")

	// Third import AFTER appending a new message → update event
	dst, err := hermesdb.OpenRW(srcPath)
	require.NoError(t, err)
	_, err = dst.Exec(`INSERT INTO messages (session_id, role, content, timestamp) VALUES ('skip-test','assistant','m2',200.0)`)
	require.NoError(t, err)
	dst.Close()

	ids3, err := a.ImportConversationsFromDB(context.Background(), store, srcPath, 0)
	require.NoError(t, err)
	require.Len(t, ids3, 1)
	require.Equal(t, id, ids3[0])

	events3, err := store.ReadEvents(acf.KindConversation, id)
	require.NoError(t, err)
	require.Len(t, events3, 2, "third import WITH changes: create + update")
	require.Equal(t, acf.EventTypeUpdate, events3[1].Type)
}

// newTestAdapter builds an Adapter with HomeDir=<temp> and DeviceID="test-dev".
func newTestAdapter(t *testing.T) *Adapter {
	t.Helper()
	return &Adapter{
		HomeDir:  t.TempDir(),
		DeviceID: "test-dev",
	}
}

// TestImportConversationsFromDB_SkipsAplexicaWrittenSessions covers the
// E2E F5 echo finding: TickInbound exports a cross-agent conversation INTO
// hermes' DB with Source "aplexica:canonical-import"; the next TickOutbound
// then re-imported that very session as a NEW hermes-sourced artifact —
// duplicating every synced conversation in the canonical store. Sessions
// the daemon itself wrote must never round-trip back in.
func TestImportConversationsFromDB_SkipsAplexicaWrittenSessions(t *testing.T) {
	srcPath := filepath.Join(t.TempDir(), "src.db")
	src, err := hermesdb.InitTestDB(srcPath)
	require.NoError(t, err)
	_, err = src.Exec(`INSERT INTO sessions (id, source, started_at, title) VALUES ('real','cli',100.0,'native session')`)
	require.NoError(t, err)
	_, err = src.Exec(`INSERT INTO sessions (id, source, started_at, title) VALUES ('echo','aplexica:canonical-import',200.0,'exported by the daemon')`)
	require.NoError(t, err)
	src.Close()

	storeRoot := filepath.Join(t.TempDir(), "store")
	store := &acf.Store{Root: storeRoot}
	require.NoError(t, store.Init())

	a := newTestAdapter(t)
	ids, err := a.ImportConversationsFromDB(context.Background(), store, srcPath, 0)
	require.NoError(t, err)
	require.Len(t, ids, 1, "only the native session imports; the daemon-written one is an echo")

	art, err := store.ReadArtifact(acf.KindConversation, ids[0])
	require.NoError(t, err)
	require.Contains(t, art.SourcePath, "#session=real")
}
