package hermesdb

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestInitTestDB_RoundTrip(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.db")
	db, err := InitTestDB(dbPath)
	require.NoError(t, err)
	defer db.Close()

	// Insert a session + message, confirm FTS5 trigger fires.
	_, err = db.Exec(`INSERT INTO sessions (id, source, started_at) VALUES (?, ?, ?)`,
		"s1", "test", 100.0)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO messages (session_id, role, content, timestamp) VALUES (?, ?, ?, ?)`,
		"s1", "user", "hello world", 101.0)
	require.NoError(t, err)

	var n int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM messages_fts WHERE messages_fts MATCH 'hello'`).Scan(&n))
	require.Equal(t, 1, n, "FTS5 trigger must populate messages_fts on INSERT")
}

func TestListSessions_RoundTrip(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.db")
	src, err := InitTestDB(dbPath)
	require.NoError(t, err)

	// Seed two sessions, one with messages.
	mustExec(t, src, `INSERT INTO sessions (id, source, started_at, title) VALUES (?, ?, ?, ?)`,
		"sess-A", "cli", 1000.0, "Alpha")
	mustExec(t, src, `INSERT INTO sessions (id, source, started_at) VALUES (?, ?, ?)`,
		"sess-B", "gateway", 2000.0)
	mustExec(t, src, `INSERT INTO messages (session_id, role, content, timestamp) VALUES (?, ?, ?, ?)`,
		"sess-A", "user", "hi", 1001.0)
	mustExec(t, src, `INSERT INTO messages (session_id, role, content, tool_calls, timestamp) VALUES (?, ?, ?, ?, ?)`,
		"sess-A", "assistant", "ok", `[{"name":"bash"}]`, 1002.0)
	src.Close()

	got, err := ListSessions(dbPath, 0)
	require.NoError(t, err)
	require.Len(t, got, 2)
	// Stable ordering: by started_at ASC.
	require.Equal(t, "sess-A", got[0].Session.ID)
	require.Equal(t, "Alpha", *got[0].Session.Title)
	require.Len(t, got[0].Messages, 2)
	require.Equal(t, "user", got[0].Messages[0].Role)
	require.Equal(t, "assistant", got[0].Messages[1].Role)
	require.NotNil(t, got[0].Messages[1].ToolCalls)
	require.Equal(t, `[{"name":"bash"}]`, *got[0].Messages[1].ToolCalls)
	require.Equal(t, "sess-B", got[1].Session.ID)
	require.Len(t, got[1].Messages, 0)

	// Insert into a fresh DB and confirm round-trip.
	dstPath := filepath.Join(t.TempDir(), "dst.db")
	dst, err := InitTestDB(dstPath)
	require.NoError(t, err)
	dst.Close()
	for _, b := range got {
		require.NoError(t, InsertSession(dstPath, b))
	}
	got2, err := ListSessions(dstPath, 0)
	require.NoError(t, err)
	require.Equal(t, got, got2, "round-trip must be lossless")
}

func TestInsertSession_Idempotent(t *testing.T) {
	srcPath := filepath.Join(t.TempDir(), "src.db")
	src, err := InitTestDB(srcPath)
	require.NoError(t, err)
	mustExec(t, src, `INSERT INTO sessions (id, source, started_at) VALUES (?, ?, ?)`,
		"s1", "cli", 100.0)
	mustExec(t, src, `INSERT INTO messages (session_id, role, content, timestamp) VALUES (?, ?, ?, ?)`,
		"s1", "user", "msg", 101.0)
	src.Close()

	bundles, err := ListSessions(srcPath, 0)
	require.NoError(t, err)
	require.Len(t, bundles, 1)

	dstPath := filepath.Join(t.TempDir(), "dst.db")
	dst, err := InitTestDB(dstPath)
	require.NoError(t, err)
	dst.Close()

	// Insert three times; rows must not duplicate.
	require.NoError(t, InsertSession(dstPath, bundles[0]))
	require.NoError(t, InsertSession(dstPath, bundles[0]))
	require.NoError(t, InsertSession(dstPath, bundles[0]))

	after, err := ListSessions(dstPath, 0)
	require.NoError(t, err)
	require.Len(t, after, 1)
	require.Len(t, after[0].Messages, 1, "InsertSession must dedupe messages")
}

func TestListSessions_SinceFilter(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.db")
	db, err := InitTestDB(dbPath)
	require.NoError(t, err)
	mustExec(t, db, `INSERT INTO sessions (id, source, started_at) VALUES ('old', 'cli', 100.0)`)
	mustExec(t, db, `INSERT INTO sessions (id, source, started_at) VALUES ('new', 'cli', 500.0)`)
	db.Close()

	bundles, err := ListSessions(dbPath, 300.0)
	require.NoError(t, err)
	require.Len(t, bundles, 1)
	require.Equal(t, "new", bundles[0].Session.ID)
}

// TestOpenRO_BusyTimeout guards against spurious SQLITE_BUSY on the hot poll
// path: OpenRO reads a state.db owned by a live Hermes process, so a brief
// exclusive-lock window (e.g. a WAL checkpoint) must make the read wait, not
// fail immediately. The connection must carry a non-zero busy_timeout, matching
// OpenRW.
func TestOpenRO_BusyTimeout(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := InitTestDB(path)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	ro, err := OpenRO(path)
	require.NoError(t, err)
	defer ro.Close()

	var ms int
	require.NoError(t, ro.QueryRow(`PRAGMA busy_timeout`).Scan(&ms))
	require.Equal(t, busyTimeoutMS, ms, "OpenRO must set busy_timeout to avoid spurious SQLITE_BUSY")
}

func mustExec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	_, err := db.Exec(q, args...)
	require.NoError(t, err)
}
