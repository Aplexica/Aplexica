package hermesdb

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func portablePreflightBundle() SessionBundle {
	q := "question"
	a := "answer"
	return SessionBundle{
		Session: SessionRow{ID: "preflight-session", Source: AplexicaCanonicalImportSource, StartedAt: 100, MessageCount: 2},
		Messages: []MessageRow{
			{Role: "user", Content: &q, Timestamp: 100},
			{Role: "assistant", Content: &a, Timestamp: 101},
		},
	}
}

func TestPortableRepairNeedsHistory_UsesCompleteVisibleIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := InitTestDB(path)
	require.NoError(t, err)
	require.NoError(t, db.Close())
	incoming := portablePreflightBundle()
	require.NoError(t, InsertPortableSession(path, incoming, nil))

	needs, err := PortableRepairNeedsHistory(path, incoming)
	require.NoError(t, err)
	require.False(t, needs, "exact timestamp+role+content sequence needs no historical replay")

	db, err = OpenRW(path)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE messages SET timestamp = timestamp + 10 WHERE session_id = ?`, incoming.Session.ID)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	needs, err = PortableRepairNeedsHistory(path, incoming)
	require.NoError(t, err)
	require.True(t, needs,
		"equal text with legacy timestamps must load history so old visible identities are removed rather than duplicated")
}

func TestPortableRepairNeedsHistory_OnlyForOwnedVisibleDivergence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := InitTestDB(path)
	require.NoError(t, err)
	require.NoError(t, db.Close())
	incoming := portablePreflightBundle()
	require.NoError(t, InsertPortableSession(path, incoming, nil))

	db, err = OpenRW(path)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO messages (session_id, role, content, timestamp) VALUES (?, 'assistant', 'old commentary', 100.5)`, incoming.Session.ID)
	require.NoError(t, err)
	require.NoError(t, db.Close())
	needs, err := PortableRepairNeedsHistory(path, incoming)
	require.NoError(t, err)
	require.True(t, needs)

	db, err = OpenRW(path)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE sessions SET source = 'cli' WHERE id = ?`, incoming.Session.ID)
	require.NoError(t, err)
	require.NoError(t, db.Close())
	needs, err = PortableRepairNeedsHistory(path, incoming)
	require.NoError(t, err)
	require.False(t, needs, "Hermes-owned rows are never repair candidates")
}
