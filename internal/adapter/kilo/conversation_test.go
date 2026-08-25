package kilo

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/stretchr/testify/require"
)

func TestImportConversationsFromDB_CreatesCanonicalProjectScopedArtifact(t *testing.T) {
	dbPath := initKiloConversationTestDB(t)
	projectDir := filepath.Join(t.TempDir(), "repo")
	require.NoError(t, makeGitRepo(projectDir))
	insertKiloSession(t, dbPath, kiloTestSession{
		ID:          "s1",
		ProjectID:   "proj-s1",
		Slug:        "project-chat",
		Directory:   projectDir,
		Title:       "Project Chat",
		TimeCreated: 1000,
		TimeUpdated: 2000,
	})
	insertKiloMessage(t, dbPath, "m1", "s1", 1100, `{"role":"user","time":{"created":1100}}`)
	insertKiloPart(t, dbPath, "p1", "m1", "s1", 1101, `{"type":"text","text":"hello","time":{"start":1101,"end":1102}}`)

	store := &acf.Store{Root: filepath.Join(t.TempDir(), "store")}
	require.NoError(t, store.Init())
	a := &Adapter{HomeDir: t.TempDir(), DeviceID: "test-dev"}

	ids, err := a.ImportConversationsFromDB(context.Background(), store, dbPath, 0)
	require.NoError(t, err)
	require.Len(t, ids, 1)

	art, err := store.ReadArtifact(acf.KindConversation, ids[0])
	require.NoError(t, err)
	require.Equal(t, acf.KindConversation, art.Kind)
	require.Equal(t, acf.ScopeProject, art.Scope)
	require.NotNil(t, art.Project)
	physicalProjectDir, err := filepath.EvalSymlinks(projectDir)
	require.NoError(t, err)
	require.Equal(t, physicalProjectDir, art.Project.Path)
	require.Equal(t, "git", art.Project.VCS)
	require.Equal(t, "Project Chat", art.Name)
	absDB, err := filepath.Abs(dbPath)
	require.NoError(t, err)
	require.Equal(t, absDB+"#session=s1", art.SourcePath)

	events, err := store.ReadEvents(acf.KindConversation, ids[0])
	require.NoError(t, err)
	require.Len(t, events, 1)
	payload, err := acf.DecodeConversationPayload(events[0])
	require.NoError(t, err)
	require.Equal(t, acf.ConversationFormatV1, payload.Format)
	require.Len(t, payload.Events, 1)
	require.Equal(t, acf.EventTypeTurn, payload.Events[0].Type)
	require.Equal(t, "hello", payload.Events[0].Content[0].Text)
}

func TestReadConversationSessionsIncludesAplexicaOwnedProjection(t *testing.T) {
	dbPath := initKiloConversationTestDB(t)
	sessionID := syncedSessionIDPrefix + "0123456789abcdef01234567"
	insertKiloSession(t, dbPath, kiloTestSession{
		ID: sessionID, ProjectID: "global", Directory: t.TempDir(),
		TimeCreated: 1000, TimeUpdated: 2000,
	})
	insertKiloMessage(t, dbPath, "m1", sessionID, 1100, `{"role":"user","time":{"created":1100}}`)
	insertKiloPart(t, dbPath, "p1", "m1", sessionID, 1101, `{"type":"text","text":"question"}`)
	insertKiloMessage(t, dbPath, "m2", sessionID, 1200, `{"role":"assistant","time":{"created":1200}}`)
	insertKiloPart(t, dbPath, "p2", "m2", sessionID, 1201, `{"type":"text","text":"answer"}`)

	sessions, err := ReadConversationSessions(context.Background(), dbPath, 1500)
	require.NoError(t, err)
	require.Len(t, sessions, 1)
	require.Equal(t, sessionID, sessions[0].SessionID)
	require.Equal(t, []acf.TextTurn{{Role: "user", Text: "question"}, {Role: "assistant", Text: "answer"}},
		acf.ExtractTextTurns(sessions[0].Events))
}

func TestImportConversationsFromDB_SkipsUnchangedSessionAndUpdatesChangedSession(t *testing.T) {
	dbPath := initKiloConversationTestDB(t)
	insertKiloSession(t, dbPath, kiloTestSession{
		ID:          "s1",
		ProjectID:   "proj-s1",
		Directory:   "/tmp/not-a-repo",
		Title:       "Project Chat",
		TimeCreated: 1000,
		TimeUpdated: 2000,
	})
	insertKiloMessage(t, dbPath, "m1", "s1", 1100, `{"role":"user","time":{"created":1100}}`)
	insertKiloPart(t, dbPath, "p1", "m1", "s1", 1101, `{"type":"text","text":"hello","time":{"start":1101,"end":1102}}`)

	store := &acf.Store{Root: filepath.Join(t.TempDir(), "store")}
	require.NoError(t, store.Init())
	a := &Adapter{HomeDir: t.TempDir(), DeviceID: "test-dev"}

	ids1, err := a.ImportConversationsFromDB(context.Background(), store, dbPath, 0)
	require.NoError(t, err)
	require.Len(t, ids1, 1)
	events1, err := store.ReadEvents(acf.KindConversation, ids1[0])
	require.NoError(t, err)
	require.Len(t, events1, 1)

	ids2, err := a.ImportConversationsFromDB(context.Background(), store, dbPath, 0)
	require.NoError(t, err)
	require.Equal(t, ids1, ids2)
	events2, err := store.ReadEvents(acf.KindConversation, ids1[0])
	require.NoError(t, err)
	require.Len(t, events2, 1)

	insertKiloPart(t, dbPath, "p2", "m1", "s1", 1103, `{"type":"text","text":"hello again","time":{"start":1103,"end":1104}}`)
	ids3, err := a.ImportConversationsFromDB(context.Background(), store, dbPath, 0)
	require.NoError(t, err)
	require.Equal(t, ids1, ids3)
	events3, err := store.ReadEvents(acf.KindConversation, ids1[0])
	require.NoError(t, err)
	require.Len(t, events3, 2)
	require.Equal(t, acf.EventTypeUpdate, events3[1].Type)
}

func TestImportConversationsFromDB_TimeUpdatedBumpDoesNotAppendEvent(t *testing.T) {
	dbPath := initKiloConversationTestDB(t)
	insertKiloSession(t, dbPath, kiloTestSession{
		ID:          "s1",
		ProjectID:   "proj-s1",
		Directory:   "/tmp/not-a-repo",
		Title:       "Project Chat",
		TimeCreated: 1000,
		TimeUpdated: 2000,
	})
	insertKiloMessage(t, dbPath, "m1", "s1", 1100, `{"role":"user","time":{"created":1100}}`)
	insertKiloPart(t, dbPath, "p1", "m1", "s1", 1101, `{"type":"text","text":"hello","time":{"start":1101,"end":1102}}`)

	store := &acf.Store{Root: filepath.Join(t.TempDir(), "store")}
	require.NoError(t, store.Init())
	a := &Adapter{HomeDir: t.TempDir(), DeviceID: "test-dev"}

	ids1, err := a.ImportConversationsFromDB(context.Background(), store, dbPath, 0)
	require.NoError(t, err)
	require.Len(t, ids1, 1)
	events1, err := store.ReadEvents(acf.KindConversation, ids1[0])
	require.NoError(t, err)
	require.Len(t, events1, 1)

	// Kilo bumps a session's time_updated on essentially any interaction, even
	// when no message/part content changed. A forced resync re-reads everything
	// (sinceMillis=0). The dedup must not append a second Update event whose only
	// delta is the embedded activity timestamp.
	bumpKiloSessionTimeUpdated(t, dbPath, "s1", 9999)

	ids2, err := a.ImportConversationsFromDB(context.Background(), store, dbPath, 0)
	require.NoError(t, err)
	require.Equal(t, ids1, ids2)
	events2, err := store.ReadEvents(acf.KindConversation, ids1[0])
	require.NoError(t, err)
	require.Len(t, events2, 1, "a pure time_updated bump with unchanged messages must not append an event")
}

func bumpKiloSessionTimeUpdated(t *testing.T, dbPath, sessionID string, timeUpdated int64) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	defer db.Close()
	res, err := db.Exec(`UPDATE session SET time_updated = ? WHERE id = ?`, timeUpdated, sessionID)
	require.NoError(t, err)
	n, err := res.RowsAffected()
	require.NoError(t, err)
	require.Equal(t, int64(1), n)
}

func makeGitRepo(path string) error {
	return os.MkdirAll(filepath.Join(path, ".git"), 0o755)
}
