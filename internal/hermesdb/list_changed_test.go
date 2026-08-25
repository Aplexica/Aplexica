package hermesdb

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestListChangedSessions_NewSessionDetected(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.db")
	db, err := InitTestDB(dbPath)
	require.NoError(t, err)
	mustExec(t, db, `INSERT INTO sessions (id, source, started_at) VALUES ('A','cli',100.0)`)
	mustExec(t, db, `INSERT INTO sessions (id, source, started_at) VALUES ('B','cli',200.0)`)
	db.Close()

	bundles, hwm, err := ListChangedSessions(dbPath, 150.0)
	require.NoError(t, err)
	require.Len(t, bundles, 1)
	require.Equal(t, "B", bundles[0].Session.ID)
	require.InDelta(t, 200.0, hwm, 0.001)
}

func TestListChangedSessions_MessageAppendDetected(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.db")
	db, err := InitTestDB(dbPath)
	require.NoError(t, err)
	mustExec(t, db, `INSERT INTO sessions (id, source, started_at) VALUES ('A','cli',100.0)`)
	// Old message (before HWM)
	mustExec(t, db, `INSERT INTO messages (session_id, role, content, timestamp) VALUES ('A','user','old',150.0)`)
	// New message (after HWM)
	mustExec(t, db, `INSERT INTO messages (session_id, role, content, timestamp) VALUES ('A','assistant','new',300.0)`)
	db.Close()

	bundles, hwm, err := ListChangedSessions(dbPath, 200.0)
	require.NoError(t, err)
	require.Len(t, bundles, 1, "session A should be returned because a message timestamp > HWM exists")
	require.Equal(t, "A", bundles[0].Session.ID)
	require.Len(t, bundles[0].Messages, 2, "full message history must be returned, not just the new ones")
	require.InDelta(t, 300.0, hwm, 0.001)
}

func TestListChangedSessions_NoChangeReturnsEmpty(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.db")
	db, err := InitTestDB(dbPath)
	require.NoError(t, err)
	mustExec(t, db, `INSERT INTO sessions (id, source, started_at) VALUES ('A','cli',100.0)`)
	mustExec(t, db, `INSERT INTO messages (session_id, role, content, timestamp) VALUES ('A','user','m',110.0)`)
	db.Close()

	// HWM is past everything; nothing should come back.
	bundles, hwm, err := ListChangedSessions(dbPath, 500.0)
	require.NoError(t, err)
	require.Empty(t, bundles)
	require.InDelta(t, 500.0, hwm, 0.001, "HWM stays put when nothing new is observed")
}

func TestListChangedSessions_HWMAdvances(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.db")
	db, err := InitTestDB(dbPath)
	require.NoError(t, err)
	mustExec(t, db, `INSERT INTO sessions (id, source, started_at) VALUES ('A','cli',100.0)`)
	mustExec(t, db, `INSERT INTO messages (session_id, role, content, timestamp) VALUES ('A','user','m',110.0)`)
	mustExec(t, db, `INSERT INTO sessions (id, source, started_at) VALUES ('B','cli',250.0)`)
	mustExec(t, db, `INSERT INTO messages (session_id, role, content, timestamp) VALUES ('B','user','n',260.0)`)
	db.Close()

	// First full scan
	bundles, hwm, err := ListChangedSessions(dbPath, 0)
	require.NoError(t, err)
	require.Len(t, bundles, 2)
	require.InDelta(t, 260.0, hwm, 0.001, "HWM = max of all observed started_at and message timestamps")
}

func TestListChangedSessions_SessionWithoutMessagesUsesStartedAt(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.db")
	db, err := InitTestDB(dbPath)
	require.NoError(t, err)
	mustExec(t, db, `INSERT INTO sessions (id, source, started_at) VALUES ('A','cli',100.0)`)
	// No messages at all.
	db.Close()

	bundles, hwm, err := ListChangedSessions(dbPath, 50.0)
	require.NoError(t, err)
	require.Len(t, bundles, 1)
	require.InDelta(t, 100.0, hwm, 0.001)
}

// helper local to this file (mustExec already exists in hermesdb_test.go in same package)
var _ = sql.ErrNoRows // keep database/sql import used
