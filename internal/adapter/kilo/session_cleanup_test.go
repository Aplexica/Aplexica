package kilo

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/stretchr/testify/require"
)

func TestCleanupKiloImportedSessionDB_PrunesOnlyObsoleteGeneratedRows(t *testing.T) {
	dbPath := initKiloConversationTestDB(t)
	doc := kiloCleanupTestDocument()
	seedKiloCleanupDocument(t, dbPath, doc)

	seed, ok := kiloCleanupSeed(doc.Info.ID)
	require.True(t, ok)
	staleMessage := "msg_aplx" + seed + "000004"
	stalePart := "prt_aplx" + seed + "000004"
	staleRootMessage := "msg_aplxroot" + seed
	staleRootPart := "prt_aplxroot" + seed
	desiredMessage := doc.Messages[0].Info["id"].(string)
	staleDesiredPart := "prt_aplx" + seed + "999999"

	insertCleanupMessage(t, dbPath, staleMessage, doc.Info.ID)
	insertCleanupPart(t, dbPath, stalePart, staleMessage, doc.Info.ID)
	insertCleanupMessage(t, dbPath, staleRootMessage, doc.Info.ID)
	insertCleanupPart(t, dbPath, staleRootPart, staleRootMessage, doc.Info.ID)
	insertCleanupPart(t, dbPath, staleDesiredPart, desiredMessage, doc.Info.ID)
	insertCleanupMessage(t, dbPath, "msg_native_keep", doc.Info.ID)
	insertCleanupPart(t, dbPath, "prt_native_keep", "msg_native_keep", doc.Info.ID)

	// A native user's session is outside the exact deterministic target and
	// must remain byte-for-byte untouched.
	insertCleanupSession(t, dbPath, "ses_native", "native", "7.4.11")
	insertCleanupMessage(t, dbPath, "msg_native_other", "ses_native")
	insertCleanupPart(t, dbPath, "prt_native_other", "msg_native_other", "ses_native")

	found, err := cleanupKiloImportedSessionDB(context.Background(), dbPath, doc)
	require.NoError(t, err)
	require.True(t, found)

	messageIDs, partIDs := readCleanupIDs(t, dbPath, doc.Info.ID)
	desiredMessages, desiredParts, err := kiloCleanupDesiredIDs(doc)
	require.NoError(t, err)
	for id := range desiredMessages {
		require.Contains(t, messageIDs, id)
	}
	for id := range desiredParts {
		require.Contains(t, partIDs, id)
	}
	require.Contains(t, messageIDs, "msg_native_keep")
	require.Contains(t, partIDs, "prt_native_keep")
	for _, id := range []string{staleMessage, staleRootMessage} {
		require.NotContains(t, messageIDs, id)
	}
	for _, id := range []string{stalePart, staleRootPart, staleDesiredPart} {
		require.NotContains(t, partIDs, id)
	}

	otherMessages, otherParts := readCleanupIDs(t, dbPath, "ses_native")
	require.Equal(t, map[string]struct{}{"msg_native_other": {}}, otherMessages)
	require.Equal(t, map[string]struct{}{"prt_native_other": {}}, otherParts)
}

func TestCleanupKiloImportedSessionDB_RejectsNonOwnedSession(t *testing.T) {
	dbPath := initKiloConversationTestDB(t)
	doc := kiloCleanupTestDocument()
	insertCleanupSession(t, dbPath, doc.Info.ID, doc.Info.Slug, "7.4.11")
	staleID := "msg_aplx" + mustKiloCleanupSeed(t, doc.Info.ID) + "000009"
	insertCleanupMessage(t, dbPath, staleID, doc.Info.ID)

	found, err := cleanupKiloImportedSessionDB(context.Background(), dbPath, doc)
	require.ErrorContains(t, err, "ownership metadata")
	require.True(t, found, "the exact session was located even though its ownership gate failed")
	messages, _ := readCleanupIDs(t, dbPath, doc.Info.ID)
	require.Contains(t, messages, staleID, "a non-owned session must not be mutated")
}

func TestCleanupKiloImportedSessionDB_MissingDesiredRowLeavesStaleRows(t *testing.T) {
	dbPath := initKiloConversationTestDB(t)
	doc := kiloCleanupTestDocument()
	insertCleanupSession(t, dbPath, doc.Info.ID, doc.Info.Slug, doc.Info.Version)
	staleID := "msg_aplx" + mustKiloCleanupSeed(t, doc.Info.ID) + "000009"
	insertCleanupMessage(t, dbPath, staleID, doc.Info.ID)

	found, err := cleanupKiloImportedSessionDB(context.Background(), dbPath, doc)
	require.ErrorContains(t, err, "missing desired message")
	require.True(t, found, "the exact session was located even though its import is partial")
	messages, _ := readCleanupIDs(t, dbPath, doc.Info.ID)
	require.Contains(t, messages, staleID, "a partial CLI import must not be destructively normalized")
}

func TestCleanupKiloImportedSessionDB_NativePartOnStaleMessageFailsClosed(t *testing.T) {
	dbPath := initKiloConversationTestDB(t)
	doc := kiloCleanupTestDocument()
	seedKiloCleanupDocument(t, dbPath, doc)
	seed := mustKiloCleanupSeed(t, doc.Info.ID)
	staleMessage := "msg_aplx" + seed + "000009"
	insertCleanupMessage(t, dbPath, staleMessage, doc.Info.ID)
	insertCleanupPart(t, dbPath, "prt_native_concurrent", staleMessage, doc.Info.ID)

	found, err := cleanupKiloImportedSessionDB(context.Background(), dbPath, doc)
	require.ErrorContains(t, err, "has native part")
	require.True(t, found, "the exact session was located even though cleanup failed closed")
	messages, parts := readCleanupIDs(t, dbPath, doc.Info.ID)
	require.Contains(t, messages, staleMessage)
	require.Contains(t, parts, "prt_native_concurrent")
}

func TestCleanupKiloImportedSessionDB_DeleteFailureRollsBackTransaction(t *testing.T) {
	dbPath := initKiloConversationTestDB(t)
	doc := kiloCleanupTestDocument()
	seedKiloCleanupDocument(t, dbPath, doc)
	seed := mustKiloCleanupSeed(t, doc.Info.ID)
	staleMessage := "msg_aplx" + seed + "000009"
	stalePart := "prt_aplx" + seed + "000009"
	insertCleanupMessage(t, dbPath, staleMessage, doc.Info.ID)
	insertCleanupPart(t, dbPath, stalePart, staleMessage, doc.Info.ID)

	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	_, err = db.Exec(fmt.Sprintf(`CREATE TRIGGER reject_cleanup
		BEFORE DELETE ON message WHEN OLD.id = %q
		BEGIN SELECT RAISE(ABORT, 'blocked'); END`, staleMessage))
	require.NoError(t, err)
	require.NoError(t, db.Close())

	found, err := cleanupKiloImportedSessionDB(context.Background(), dbPath, doc)
	require.ErrorContains(t, err, "blocked")
	require.True(t, found, "the exact session was located even though its transaction rolled back")
	messages, parts := readCleanupIDs(t, dbPath, doc.Info.ID)
	require.Contains(t, messages, staleMessage)
	require.Contains(t, parts, stalePart, "part deletion must roll back when the later message delete fails")
}

func TestCleanupImportedKiloSession_ActivePartialDBErrorIsNotHiddenByOlderDB(t *testing.T) {
	home := t.TempDir()
	doc := kiloCleanupTestDocument()
	activeDB := filepath.Join(home, ".local", "share", "kilo", "kilo.db")
	olderDB := filepath.Join(home, "Library", "Application Support", "kilo", "kilo.db")
	createKiloCleanupTestDBAt(t, activeDB)
	createKiloCleanupTestDBAt(t, olderDB)

	// The newly modified active DB contains the exact session but the CLI
	// dropped desired rows. The older historical DB has a complete copy that
	// would be cleanable. The active error must be terminal; cleaning the old
	// copy would hide the real failure and leave Kilo's live projection stale.
	insertCleanupSession(t, activeDB, doc.Info.ID, doc.Info.Slug, doc.Info.Version)
	seed := mustKiloCleanupSeed(t, doc.Info.ID)
	activeStale := "msg_aplx" + seed + "000008"
	insertCleanupMessage(t, activeDB, activeStale, doc.Info.ID)

	seedKiloCleanupDocument(t, olderDB, doc)
	olderStale := "msg_aplx" + seed + "000009"
	insertCleanupMessage(t, olderDB, olderStale, doc.Info.ID)

	now := time.Now()
	require.NoError(t, os.Chtimes(olderDB, now.Add(-time.Hour), now.Add(-time.Hour)))
	require.NoError(t, os.Chtimes(activeDB, now, now))

	a := &Adapter{HomeDir: home}
	err := a.cleanupImportedKiloSession(doc)
	require.ErrorContains(t, err, "missing desired message")
	require.Contains(t, err.Error(), activeDB)

	activeMessages, _ := readCleanupIDs(t, activeDB, doc.Info.ID)
	require.Contains(t, activeMessages, activeStale)
	olderMessages, _ := readCleanupIDs(t, olderDB, doc.Info.ID)
	require.Contains(t, olderMessages, olderStale, "the inactive historical DB must not be cleaned after the active session was found")
}

func TestCleanupKiloImportedSessionDB_ConcurrentRetriesAreIdempotent(t *testing.T) {
	dbPath := initKiloConversationTestDB(t)
	doc := kiloCleanupTestDocument()
	seedKiloCleanupDocument(t, dbPath, doc)
	seed := mustKiloCleanupSeed(t, doc.Info.ID)
	staleMessage := "msg_aplx" + seed + "000009"
	stalePart := "prt_aplx" + seed + "000009"
	insertCleanupMessage(t, dbPath, staleMessage, doc.Info.ID)
	insertCleanupPart(t, dbPath, stalePart, staleMessage, doc.Info.ID)

	const workers = 8
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			found, err := cleanupKiloImportedSessionDB(context.Background(), dbPath, doc)
			if err == nil && !found {
				err = fmt.Errorf("session unexpectedly absent")
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	messages, parts := readCleanupIDs(t, dbPath, doc.Info.ID)
	require.NotContains(t, messages, staleMessage)
	require.NotContains(t, parts, stalePart)
}

func TestKiloGeneratedIDOwnership_IsExact(t *testing.T) {
	doc := kiloCleanupTestDocument()
	seed := mustKiloCleanupSeed(t, doc.Info.ID)
	require.True(t, kiloOwnsMessageID(doc.Info.ID, "msg_aplx"+seed+"000001"))
	require.True(t, kiloOwnsMessageID(doc.Info.ID, "msg_aplxroot"+seed))
	require.True(t, kiloOwnsPartID(doc.Info.ID, "prt_aplx"+seed+"000001"))
	require.True(t, kiloOwnsPartID(doc.Info.ID, "prt_aplxroot"+seed))
	for _, id := range []string{
		"msg_aplx" + seed,
		"msg_aplx" + seed + "00001",
		"msg_aplx" + seed + "000001extra",
		"msg_aplx" + seed + "native",
		"msg_native",
	} {
		require.False(t, kiloOwnsMessageID(doc.Info.ID, id), id)
	}
}

func kiloCleanupTestDocument() kiloExportFile {
	art := acf.Artifact{
		ArtifactID: "019f76da-6d22-7455-b3f5-15d5e13cbd94",
		CreatedAt:  time.Unix(1784400000, 0).UTC(),
		UpdatedAt:  time.Unix(1784400100, 0).UTC(),
	}
	turns := []acf.TextTurn{
		{Role: "user", Text: "what is capital of Poland"},
		{Role: "assistant", Text: "Warsaw."},
		{Role: "user", Text: "how many people live in warsaw?"},
		{Role: "assistant", Text: "About 1.87 million."},
	}
	return buildKiloExport(art, turns, "codex", "/Users/test")
}

func seedKiloCleanupDocument(t *testing.T, dbPath string, doc kiloExportFile) {
	t.Helper()
	insertCleanupSession(t, dbPath, doc.Info.ID, doc.Info.Slug, doc.Info.Version)
	for _, message := range doc.Messages {
		messageID := message.Info["id"].(string)
		insertCleanupMessage(t, dbPath, messageID, doc.Info.ID)
		for _, part := range message.Parts {
			insertCleanupPart(t, dbPath, part.ID, messageID, doc.Info.ID)
		}
	}
}

func insertCleanupSession(t *testing.T, dbPath, id, slug, version string) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	defer db.Close()
	_, err = db.Exec(`INSERT INTO session
		(id, project_id, slug, directory, title, version, time_created, time_updated)
		VALUES (?, 'global', ?, '/tmp', 'test', ?, 1, 1)`, id, slug, version)
	require.NoError(t, err)
}

func createKiloCleanupTestDBAt(t *testing.T, dbPath string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(dbPath), 0o700))
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	defer db.Close()
	_, err = db.Exec(`CREATE TABLE session (
		id TEXT PRIMARY KEY, project_id TEXT NOT NULL, parent_id TEXT,
		slug TEXT NOT NULL, directory TEXT NOT NULL, path TEXT,
		title TEXT NOT NULL, version TEXT NOT NULL, share_url TEXT,
		revert TEXT, permission TEXT, workspace_id TEXT, agent TEXT, model TEXT,
		time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL,
		time_compacting INTEGER, time_archived INTEGER
	);
	CREATE TABLE message (
		id TEXT PRIMARY KEY, session_id TEXT NOT NULL,
		time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL, data TEXT NOT NULL
	);
	CREATE TABLE part (
		id TEXT PRIMARY KEY, message_id TEXT NOT NULL, session_id TEXT NOT NULL,
		time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL, data TEXT NOT NULL
	);`)
	require.NoError(t, err)
}

func insertCleanupMessage(t *testing.T, dbPath, id, sessionID string) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	defer db.Close()
	_, err = db.Exec(`INSERT INTO message (id, session_id, time_created, time_updated, data)
		VALUES (?, ?, 1, 1, '{"role":"user"}')`, id, sessionID)
	require.NoError(t, err)
}

func insertCleanupPart(t *testing.T, dbPath, id, messageID, sessionID string) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	defer db.Close()
	_, err = db.Exec(`INSERT INTO part (id, message_id, session_id, time_created, time_updated, data)
		VALUES (?, ?, ?, 1, 1, '{"type":"text","text":"x"}')`, id, messageID, sessionID)
	require.NoError(t, err)
}

func readCleanupIDs(t *testing.T, dbPath, sessionID string) (map[string]struct{}, map[string]struct{}) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	defer db.Close()
	read := func(query string) map[string]struct{} {
		rows, err := db.Query(query, sessionID)
		require.NoError(t, err)
		defer rows.Close()
		out := map[string]struct{}{}
		for rows.Next() {
			var id string
			require.NoError(t, rows.Scan(&id))
			out[id] = struct{}{}
		}
		require.NoError(t, rows.Err())
		return out
	}
	return read(`SELECT id FROM message WHERE session_id = ?`), read(`SELECT id FROM part WHERE session_id = ?`)
}

func mustKiloCleanupSeed(t *testing.T, sessionID string) string {
	t.Helper()
	seed, ok := kiloCleanupSeed(sessionID)
	require.True(t, ok)
	return seed
}
