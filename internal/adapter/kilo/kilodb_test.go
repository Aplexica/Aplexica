package kilo

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/stretchr/testify/require"
)

func TestListKiloSessionBundles_ReadsMessagesAndParts(t *testing.T) {
	dbPath := initKiloConversationTestDB(t)
	insertKiloSession(t, dbPath, kiloTestSession{
		ID:          "old",
		ProjectID:   "proj-old",
		Slug:        "old-chat",
		Directory:   "/tmp/old",
		Title:       "Old Chat",
		TimeCreated: 100,
		TimeUpdated: 900,
	})
	insertKiloSession(t, dbPath, kiloTestSession{
		ID:          "s1",
		ProjectID:   "proj-s1",
		Slug:        "project-chat",
		Directory:   "/tmp/project",
		Title:       "Project Chat",
		TimeCreated: 1000,
		TimeUpdated: 2000,
	})
	insertKiloMessage(t, dbPath, "m1", "s1", 1100, `{"role":"user","time":{"created":1100}}`)
	insertKiloMessage(t, dbPath, "m2", "s1", 1200, `{"role":"assistant","time":{"created":1200},"parentID":"m1","modelID":"kilo-auto/frontier","providerID":"kilo","path":{"cwd":"/tmp/project","root":"/tmp/project"},"agent":"code","mode":"code","cost":0,"tokens":{"input":1,"output":2,"reasoning":0,"cache":{"read":0,"write":0}}}`)
	insertKiloPart(t, dbPath, "p1", "m1", "s1", 1101, `{"type":"text","text":"hello","time":{"start":1101,"end":1102}}`)
	insertKiloPart(t, dbPath, "p2", "m2", "s1", 1201, `{"type":"step-start","snapshot":"abc"}`)
	insertKiloPart(t, dbPath, "p3", "m2", "s1", 1202, `{"type":"text","text":"hi back","time":{"start":1202,"end":1203}}`)

	got, err := listKiloSessionBundles(dbPath, 1000)
	require.NoError(t, err)
	require.Len(t, got, 1)

	require.Equal(t, "s1", got[0].Session.ID)
	require.Equal(t, "Project Chat", got[0].Session.Title)
	require.Equal(t, "/tmp/project", got[0].Session.Directory)
	require.Len(t, got[0].Messages, 2)
	require.Equal(t, "user", got[0].Messages[0].Message.Role)
	require.Equal(t, "assistant", got[0].Messages[1].Message.Role)
	require.Equal(t, "text", got[0].Messages[0].Parts[0].Type)
	require.Equal(t, "step-start", got[0].Messages[1].Parts[0].Type)
	require.Equal(t, "text", got[0].Messages[1].Parts[1].Type)
}

type kiloTestSession struct {
	ID          string
	ProjectID   string
	ParentID    string
	Slug        string
	Directory   string
	Title       string
	TimeCreated int64
	TimeUpdated int64
}

func initKiloConversationTestDB(t *testing.T) string {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "kilo.db")
	require.NoError(t, os.MkdirAll(filepath.Dir(dbPath), 0o755))
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.Exec(`CREATE TABLE session (
		id TEXT PRIMARY KEY,
		project_id TEXT NOT NULL,
		parent_id TEXT,
		slug TEXT NOT NULL,
		directory TEXT NOT NULL,
		path TEXT,
		title TEXT NOT NULL,
		version TEXT NOT NULL,
		share_url TEXT,
		revert TEXT,
		permission TEXT,
		workspace_id TEXT,
		agent TEXT,
		model TEXT,
		time_created INTEGER NOT NULL,
		time_updated INTEGER NOT NULL,
		time_compacting INTEGER,
		time_archived INTEGER
	);
	CREATE TABLE message (
		id TEXT PRIMARY KEY,
		session_id TEXT NOT NULL,
		time_created INTEGER NOT NULL,
		time_updated INTEGER NOT NULL,
		data TEXT NOT NULL
	);
	CREATE TABLE part (
		id TEXT PRIMARY KEY,
		message_id TEXT NOT NULL,
		session_id TEXT NOT NULL,
		time_created INTEGER NOT NULL,
		time_updated INTEGER NOT NULL,
		data TEXT NOT NULL
	);`)
	require.NoError(t, err)
	return dbPath
}

func insertKiloSession(t *testing.T, dbPath string, s kiloTestSession) {
	t.Helper()
	if s.ProjectID == "" {
		s.ProjectID = "proj"
	}
	if s.Slug == "" {
		s.Slug = s.ID
	}
	if s.Directory == "" {
		s.Directory = "/tmp/project"
	}
	if s.Title == "" {
		s.Title = s.ID
	}
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	defer db.Close()
	_, err = db.Exec(`INSERT INTO session
		(id, project_id, parent_id, slug, directory, title, version, time_created, time_updated)
		VALUES (?, ?, ?, ?, ?, ?, '0.0.0-test', ?, ?)`,
		s.ID, s.ProjectID, nullString(s.ParentID), s.Slug, s.Directory, s.Title, s.TimeCreated, s.TimeUpdated)
	require.NoError(t, err)
}

func insertKiloMessage(t *testing.T, dbPath, id, sessionID string, ts int64, data string) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	defer db.Close()
	_, err = db.Exec(`INSERT INTO message (id, session_id, time_created, time_updated, data)
		VALUES (?, ?, ?, ?, ?)`, id, sessionID, ts, ts, data)
	require.NoError(t, err)
}

func insertKiloPart(t *testing.T, dbPath, id, messageID, sessionID string, ts int64, data string) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	defer db.Close()
	_, err = db.Exec(`INSERT INTO part (id, message_id, session_id, time_created, time_updated, data)
		VALUES (?, ?, ?, ?, ?, ?)`, id, messageID, sessionID, ts, ts, data)
	require.NoError(t, err)
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
