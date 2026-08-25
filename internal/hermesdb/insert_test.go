package hermesdb

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// readSessionRow fetches one sessions row by id for assertion.
func readSessionRow(t *testing.T, path, id string) SessionRow {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	defer db.Close()
	var s SessionRow
	row := db.QueryRow(`SELECT id, source, title, started_at, ended_at, message_count FROM sessions WHERE id = ?`, id)
	require.NoError(t, row.Scan(&s.ID, &s.Source, &s.Title, &s.StartedAt, &s.EndedAt, &s.MessageCount))
	return s
}

// TestInsertSession_UpsertsOwnSessionMetadata: re-inserting a session the
// daemon itself wrote refreshes its metadata (title/message_count/ended_at).
// Before this, INSERT OR IGNORE froze the first write forever — the 49
// sessions exported before titles existed rendered as "—" rows in hermes'
// /resume with no way to heal them.
func TestInsertSession_UpsertsOwnSessionMetadata(t *testing.T) {
	path := filepath.Join(t.TempDir(), "h.db")
	db, err := InitTestDB(path)
	require.NoError(t, err)
	db.Close()

	bare := SessionBundle{Session: SessionRow{ID: "s1", Source: "aplexica:canonical-import", StartedAt: 100}}
	require.NoError(t, InsertSession(path, bare))

	title := "↪ Codex: what is my name?"
	ended := 102.0
	rich := SessionBundle{Session: SessionRow{
		ID: "s1", Source: "aplexica:canonical-import", StartedAt: 100,
		Title: &title, MessageCount: 3, EndedAt: &ended,
	}}
	require.NoError(t, InsertSession(path, rich))

	got := readSessionRow(t, path, "s1")
	require.NotNil(t, got.Title)
	require.Equal(t, title, *got.Title)
	require.Equal(t, int64(3), got.MessageCount)
	require.NotNil(t, got.EndedAt)
}

// Native hermes rows must never be mutated by a colliding canonical insert.
func TestInsertSession_NeverMutatesNativeRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "h.db")
	db, err := InitTestDB(path)
	require.NoError(t, err)
	db.Close()

	nativeTitle := "my real hermes chat"
	native := SessionBundle{Session: SessionRow{ID: "sX", Source: "cli", StartedAt: 50, Title: &nativeTitle}}
	require.NoError(t, InsertSession(path, native))

	intruderTitle := "↪ Codex: overwrite attempt"
	intruder := SessionBundle{Session: SessionRow{ID: "sX", Source: "aplexica:canonical-import", StartedAt: 50, Title: &intruderTitle, MessageCount: 9}}
	require.NoError(t, InsertSession(path, intruder))

	got := readSessionRow(t, path, "sX")
	require.Equal(t, nativeTitle, *got.Title, "different source on the existing row must block the update")
	require.Equal(t, "cli", got.Source)
}

// TestInsertSession_DisambiguatesDuplicateTitles: the live hermes schema
// enforces CREATE UNIQUE INDEX ... ON sessions(title) WHERE title IS NOT
// NULL. Two synced sessions whose first user message is identical (e.g.
// repeated "Reply with exactly: OK" test runs) derived the same title and
// the second INSERT failed the whole export. On a title collision the
// insert retries once with a deterministic short session-id suffix.
func TestInsertSession_DisambiguatesDuplicateTitles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "h.db")
	db, err := InitTestDB(path)
	require.NoError(t, err)
	_, err = db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_sessions_title_unique ON sessions(title) WHERE title IS NOT NULL`)
	require.NoError(t, err)
	db.Close()

	title := "↪ Codex: Reply with exactly: OK"
	t1 := title
	a := SessionBundle{Session: SessionRow{ID: "s-aaa111", Source: "aplexica:canonical-import", StartedAt: 100, Title: &t1}}
	require.NoError(t, InsertSession(path, a))

	t2 := title
	b := SessionBundle{Session: SessionRow{ID: "s-bbb222", Source: "aplexica:canonical-import", StartedAt: 200, Title: &t2}}
	require.NoError(t, InsertSession(path, b), "a colliding title must disambiguate, not fail the export")

	got := readSessionRow(t, path, "s-bbb222")
	require.NotNil(t, got.Title)
	require.NotEqual(t, title, *got.Title)
	require.Contains(t, *got.Title, title, "the clean title stays as the prefix")
}

// countMessages returns the number of message rows for a session.
func countMessages(t *testing.T, path, sessionID string) int {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	defer db.Close()
	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM messages WHERE session_id = ?`, sessionID).Scan(&n))
	return n
}

// TestInsertSession_PreservesParallelToolCallsSameTimestamp: a single
// assistant turn that fans out parallel tool calls emits several rows sharing
// (timestamp, role) with empty content; they differ only in the tool-call
// columns. The dedup probe must keep them distinct rather than collapsing them
// to one (which silently dropped inbound tool calls — BRD-02 §5.4 round-trip).
func TestInsertSession_PreservesParallelToolCallsSameTimestamp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "h.db")
	db, err := InitTestDB(path)
	require.NoError(t, err)
	db.Close()

	ts := 1500.0
	empty := ""
	tc1, id1, tn1 := `[{"id":"a","name":"read"}]`, "a", "read"
	tc2, id2, tn2 := `[{"id":"b","name":"write"}]`, "b", "write"
	bundle := SessionBundle{
		Session: SessionRow{ID: "s-par", Source: "aplexica:canonical-import", StartedAt: 1000},
		Messages: []MessageRow{
			{Role: "assistant", Timestamp: ts, Content: &empty, ToolCalls: &tc1, ToolCallID: &id1, ToolName: &tn1},
			{Role: "assistant", Timestamp: ts, Content: &empty, ToolCalls: &tc2, ToolCallID: &id2, ToolName: &tn2},
		},
	}
	require.NoError(t, InsertSession(path, bundle))
	require.Equal(t, 2, countMessages(t, path, "s-par"),
		"both parallel tool-call rows at the same timestamp must survive")

	// Re-inserting the same bundle must stay idempotent (no duplicates).
	require.NoError(t, InsertSession(path, bundle))
	require.Equal(t, 2, countMessages(t, path, "s-par"),
		"re-insert must remain idempotent after the dedup-key fix")
}

func TestInsertPortableSession_PrunesOwnedInternalsOnExactVisibleMatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "h.db")
	db, err := InitTestDB(path)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	question, answer := "what is capital of France?", "Paris."
	harness, empty, calls, callID := "<permissions instructions>private harness", "", `[{"id":"call-1"}]`, "call-1"
	polluted := SessionBundle{
		Session: SessionRow{ID: "managed", Source: AplexicaCanonicalImportSource, StartedAt: 100, MessageCount: 5},
		Messages: []MessageRow{
			{Role: "user", Content: &question, Timestamp: 100},
			{Role: "system", Content: &harness, Timestamp: 100.5},
			{Role: "assistant", Content: &answer, Timestamp: 101},
			{Role: "assistant", Content: &empty, ToolCalls: &calls, Timestamp: 101.5},
			{Role: "tool", Content: &harness, ToolCallID: &callID, Timestamp: 102},
		},
	}
	require.NoError(t, InsertSession(path, polluted))

	portable := SessionBundle{
		Session: SessionRow{ID: "managed", Source: AplexicaCanonicalImportSource, StartedAt: 100, MessageCount: 2},
		Messages: []MessageRow{
			{Role: "user", Content: &question, Timestamp: 100},
			{Role: "assistant", Content: &answer, Timestamp: 101},
		},
	}
	require.NoError(t, InsertPortableSession(path, portable, []MessageRow{
		polluted.Messages[1], polluted.Messages[3], polluted.Messages[4],
	}))

	bundles, err := ListSessions(path, 0)
	require.NoError(t, err)
	require.Len(t, bundles, 1)
	require.Equal(t, portable.Messages, bundles[0].Messages)
	require.Equal(t, int64(2), bundles[0].Session.MessageCount)
}

func TestInsertPortableSession_PrunesOwnedLegacyAdjacentAssistantEchoConflict(t *testing.T) {
	path := filepath.Join(t.TempDir(), "h.db")
	db, err := InitTestDB(path)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	q1, a1 := "what is capital of France?", "The capital of France is Paris."
	q2, a2 := "how many people live in Paris?", "Paris has about 2.1 million residents."
	clean := SessionBundle{
		Session: SessionRow{ID: "managed-france", Source: AplexicaCanonicalImportSource, StartedAt: 100, MessageCount: 4},
		Messages: []MessageRow{
			{Role: "user", Content: &q1, Timestamp: 100},
			{Role: "assistant", Content: &a1, Timestamp: 101},
			{Role: "user", Content: &q2, Timestamp: 102},
			{Role: "assistant", Content: &a2, Timestamp: 103},
		},
	}
	polluted := SessionBundle{
		Session: SessionRow{ID: clean.Session.ID, Source: AplexicaCanonicalImportSource, StartedAt: 100, MessageCount: 7},
		Messages: []MessageRow{
			clean.Messages[0],
			clean.Messages[1],
			{Role: "assistant", Content: &a1, Timestamp: 101.5},
			clean.Messages[2],
			clean.Messages[3],
			{Role: "assistant", Content: &a2, Timestamp: 200},
			{Role: "user", Content: &q2, Timestamp: 201},
		},
	}
	require.NoError(t, InsertSession(path, polluted))
	require.NoError(t, InsertPortableSession(path, clean, nil))

	bundles, err := ListSessions(path, 0)
	require.NoError(t, err)
	require.Len(t, bundles, 1)
	require.Equal(t, clean.Messages, bundles[0].Messages,
		"the exact owned UAAUAAU legacy projection must converge to canonical UAUA")
	require.Equal(t, int64(len(clean.Messages)), bundles[0].Session.MessageCount)
}

func TestInsertPortableSession_PreservesNearMissVisibleEcho(t *testing.T) {
	path := filepath.Join(t.TempDir(), "h.db")
	db, err := InitTestDB(path)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	q1, a1, q2, a2 := "q1", "a1", "q2", "a2"
	localRepeat := "a2 with a local edit"
	clean := SessionBundle{
		Session: SessionRow{ID: "near-miss", Source: AplexicaCanonicalImportSource, StartedAt: 100},
		Messages: []MessageRow{
			{Role: "user", Content: &q1, Timestamp: 100},
			{Role: "assistant", Content: &a1, Timestamp: 101},
			{Role: "user", Content: &q2, Timestamp: 102},
			{Role: "assistant", Content: &a2, Timestamp: 103},
		},
	}
	existing := SessionBundle{
		Session: SessionRow{ID: clean.Session.ID, Source: AplexicaCanonicalImportSource, StartedAt: 100},
		Messages: []MessageRow{
			clean.Messages[0], clean.Messages[1],
			{Role: "assistant", Content: &a1, Timestamp: 101.5},
			clean.Messages[2], clean.Messages[3],
			{Role: "assistant", Content: &localRepeat, Timestamp: 200},
			{Role: "user", Content: &q2, Timestamp: 201},
		},
	}
	require.NoError(t, InsertSession(path, existing))
	require.NoError(t, InsertPortableSession(path, clean, nil))

	bundles, err := ListSessions(path, 0)
	require.NoError(t, err)
	require.Len(t, bundles, 1)
	require.Equal(t, existing.Messages, bundles[0].Messages,
		"any role/content near miss must preserve the entire visible continuation")
}

func TestInsertPortableSession_PreservesUnsyncedVisibleContinuation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "h.db")
	db, err := InitTestDB(path)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	question, answer := "what is capital of France?", "Paris."
	harness, empty, calls := "private harness", "", `[{"id":"call-1"}]`
	continuation := "how many people live in Paris?"
	existing := SessionBundle{
		Session: SessionRow{ID: "continued", Source: AplexicaCanonicalImportSource, StartedAt: 100},
		Messages: []MessageRow{
			{Role: "user", Content: &question, Timestamp: 100},
			{Role: "system", Content: &harness, Timestamp: 100.5},
			{Role: "assistant", Content: &answer, Timestamp: 101},
			{Role: "assistant", Content: &empty, ToolCalls: &calls, Timestamp: 101.5},
			{Role: "user", Content: &continuation, Timestamp: 102},
		},
	}
	require.NoError(t, InsertSession(path, existing))

	portable := SessionBundle{
		Session: SessionRow{ID: "continued", Source: AplexicaCanonicalImportSource, StartedAt: 100},
		Messages: []MessageRow{
			{Role: "user", Content: &question, Timestamp: 100},
			{Role: "assistant", Content: &answer, Timestamp: 101},
		},
	}
	require.NoError(t, InsertPortableSession(path, portable, []MessageRow{
		existing.Messages[1], existing.Messages[3],
	}))

	bundles, err := ListSessions(path, 0)
	require.NoError(t, err)
	require.Len(t, bundles, 1)
	require.Equal(t, existing.Messages, bundles[0].Messages,
		"a visible local continuation must block all cleanup of existing rows")
	require.Equal(t, int64(len(existing.Messages)), bundles[0].Session.MessageCount)
}

func TestInsertPortableSession_PrunesOnlyExactObsoleteRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "h.db")
	db, err := InitTestDB(path)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	question, answer := "what is capital of France?", "Paris."
	leaked, local := "old Codex harness", "local Hermes diagnostic"
	existing := SessionBundle{
		Session: SessionRow{ID: "precise", Source: AplexicaCanonicalImportSource, StartedAt: 100},
		Messages: []MessageRow{
			{Role: "user", Content: &question, Timestamp: 100},
			{Role: "system", Content: &leaked, Timestamp: 100.5},
			{Role: "system", Content: &local, Timestamp: 100.75},
			{Role: "assistant", Content: &answer, Timestamp: 101},
		},
	}
	require.NoError(t, InsertSession(path, existing))
	portable := SessionBundle{
		Session: SessionRow{ID: "precise", Source: AplexicaCanonicalImportSource, StartedAt: 100},
		Messages: []MessageRow{
			{Role: "user", Content: &question, Timestamp: 100},
			{Role: "assistant", Content: &answer, Timestamp: 101},
		},
	}
	require.NoError(t, InsertPortableSession(path, portable, []MessageRow{existing.Messages[1]}))

	bundles, err := ListSessions(path, 0)
	require.NoError(t, err)
	require.Len(t, bundles, 1)
	require.Equal(t, []MessageRow{existing.Messages[0], existing.Messages[2], existing.Messages[3]}, bundles[0].Messages,
		"exact-history repair must preserve unproven nonportable rows even before the incoming head")
	require.Equal(t, int64(3), bundles[0].Session.MessageCount)
}

func TestInsertPortableSession_NeverPrunesHermesOwnedSession(t *testing.T) {
	path := filepath.Join(t.TempDir(), "h.db")
	db, err := InitTestDB(path)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	question, system := "hello", "native system prompt"
	native := SessionBundle{
		Session: SessionRow{ID: "native", Source: "cli", StartedAt: 100},
		Messages: []MessageRow{
			{Role: "system", Content: &system, Timestamp: 99},
			{Role: "user", Content: &question, Timestamp: 100},
		},
	}
	require.NoError(t, InsertSession(path, native))

	portable := SessionBundle{
		Session:  SessionRow{ID: "native", Source: AplexicaCanonicalImportSource, StartedAt: 100},
		Messages: []MessageRow{{Role: "user", Content: &question, Timestamp: 100}},
	}
	require.NoError(t, InsertPortableSession(path, portable, []MessageRow{native.Messages[0]}))

	bundles, err := ListSessions(path, 0)
	require.NoError(t, err)
	require.Len(t, bundles, 1)
	require.Equal(t, "cli", bundles[0].Session.Source)
	require.Equal(t, native.Messages, bundles[0].Messages,
		"a colliding Aplexica projection must never clean a Hermes-owned row")
}

func TestInsertPortableSession_HistoryGoneFallbackPrunesBoundedInternals(t *testing.T) {
	path := filepath.Join(t.TempDir(), "h.db")
	db, err := InitTestDB(path)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	question, answer := "what is capital of France?", "Paris."
	harness, empty, calls, result, callID := "private harness", "", `[{"id":"call-1"}]`, "private output", "call-1"
	existing := SessionBundle{
		Session: SessionRow{ID: "history-gone", Source: AplexicaCanonicalImportSource, StartedAt: 100},
		Messages: []MessageRow{
			{Role: "user", Content: &question, Timestamp: 100},
			{Role: "system", Content: &harness, Timestamp: 100.25},
			{Role: "assistant", Content: &empty, ToolCalls: &calls, Timestamp: 100.5},
			{Role: "tool", Content: &result, ToolCallID: &callID, Timestamp: 100.75},
			{Role: "assistant", Content: &answer, Timestamp: 101},
		},
	}
	require.NoError(t, InsertSession(path, existing))
	portable := SessionBundle{
		Session: SessionRow{ID: "history-gone", Source: AplexicaCanonicalImportSource, StartedAt: 100},
		Messages: []MessageRow{
			{Role: "user", Content: &question, Timestamp: 100},
			{Role: "assistant", Content: &answer, Timestamp: 101},
		},
	}

	// nil means no exact historical identities remain (for example, retention
	// already grace-deleted the compacted canonical segment).
	require.NoError(t, InsertPortableSession(path, portable, nil))
	bundles, err := ListSessions(path, 0)
	require.NoError(t, err)
	require.Len(t, bundles, 1)
	require.Equal(t, portable.Messages, bundles[0].Messages)
	require.Equal(t, int64(2), bundles[0].Session.MessageCount)
}

func TestInsertPortableSession_HistoryGoneFallbackPreservesNewerRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "h.db")
	db, err := InitTestDB(path)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	question, answer := "what is capital of France?", "Paris."
	newerSystem, newerTool, callID := "new Hermes system context", "new Hermes tool output", "local-call"
	existing := SessionBundle{
		Session: SessionRow{ID: "newer", Source: AplexicaCanonicalImportSource, StartedAt: 100},
		Messages: []MessageRow{
			{Role: "user", Content: &question, Timestamp: 100},
			{Role: "assistant", Content: &answer, Timestamp: 101},
			{Role: "system", Content: &newerSystem, Timestamp: 102},
			{Role: "tool", Content: &newerTool, ToolCallID: &callID, Timestamp: 103},
		},
	}
	require.NoError(t, InsertSession(path, existing))
	portable := SessionBundle{
		Session: SessionRow{ID: "newer", Source: AplexicaCanonicalImportSource, StartedAt: 100},
		Messages: []MessageRow{
			{Role: "user", Content: &question, Timestamp: 100},
			{Role: "assistant", Content: &answer, Timestamp: 101},
		},
	}

	require.NoError(t, InsertPortableSession(path, portable, nil))
	bundles, err := ListSessions(path, 0)
	require.NoError(t, err)
	require.Len(t, bundles, 1)
	require.Equal(t, existing.Messages, bundles[0].Messages,
		"rows newer than the incoming portable head must survive history-gone cleanup")
	require.Equal(t, int64(len(existing.Messages)), bundles[0].Session.MessageCount)
}

func TestInsertPortableSession_HistoryGoneFallbackPreservesVisibleDivergence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "h.db")
	db, err := InitTestDB(path)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	question, answer := "what is capital of France?", "Paris."
	oldHarness, continuation := "old harness", "how many people live in Paris?"
	existing := SessionBundle{
		Session: SessionRow{ID: "diverged", Source: AplexicaCanonicalImportSource, StartedAt: 100},
		Messages: []MessageRow{
			{Role: "user", Content: &question, Timestamp: 100},
			{Role: "system", Content: &oldHarness, Timestamp: 100.5},
			{Role: "assistant", Content: &answer, Timestamp: 101},
			{Role: "user", Content: &continuation, Timestamp: 102},
		},
	}
	require.NoError(t, InsertSession(path, existing))
	portable := SessionBundle{
		Session: SessionRow{ID: "diverged", Source: AplexicaCanonicalImportSource, StartedAt: 100},
		Messages: []MessageRow{
			{Role: "user", Content: &question, Timestamp: 100},
			{Role: "assistant", Content: &answer, Timestamp: 101},
		},
	}

	require.NoError(t, InsertPortableSession(path, portable, nil))
	bundles, err := ListSessions(path, 0)
	require.NoError(t, err)
	require.Len(t, bundles, 1)
	require.Equal(t, existing.Messages, bundles[0].Messages,
		"a visible Hermes continuation must block the entire fallback cleanup")
	require.Equal(t, int64(len(existing.Messages)), bundles[0].Session.MessageCount)
}
