package hermesdb

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/stretchr/testify/require"
)

// Shared FTS table + insert trigger. Every real Hermes schema carries an FTS5
// mirror of messages; including it here ensures InsertSession exercises the
// AFTER INSERT trigger path the way it does against a live Hermes DB.
const ftsAndTrigger = `
CREATE VIRTUAL TABLE IF NOT EXISTS messages_fts USING fts5(content);
CREATE TRIGGER IF NOT EXISTS messages_fts_insert AFTER INSERT ON messages BEGIN
    INSERT INTO messages_fts(rowid, content) VALUES (new.id, COALESCE(new.content, ''));
END;
`

// schemaCase is one supported Hermes schema "version": a full DDL snapshot of
// its sessions/messages tables plus flags describing which optional columns it
// carries. These are fixtures for the schema-compatibility regression: hermesdb
// must read and write any of them without hard-failing on a column the
// installed schema happens to lack (such as platform_message_id) or
// to carry beyond what we model.
type schemaCase struct {
	name string
	ddl  string

	hasHandoff           bool // sessions.handoff_* columns
	hasCodex             bool // messages.codex_*_items columns
	hasPlatformMessageID bool // messages.platform_message_id column
}

func schemaCases() []schemaCase {
	// Hermes v11 compatibility fixture (SCHEMA_VERSION = 11). Note: there is no
	// platform_message_id column in this schema; the adapter must not require it.
	const v11Sessions = `
CREATE TABLE sessions (
    id TEXT PRIMARY KEY, source TEXT NOT NULL, user_id TEXT, model TEXT,
    model_config TEXT, system_prompt TEXT, parent_session_id TEXT,
    started_at REAL NOT NULL, ended_at REAL, end_reason TEXT,
    message_count INTEGER DEFAULT 0, tool_call_count INTEGER DEFAULT 0,
    input_tokens INTEGER DEFAULT 0, output_tokens INTEGER DEFAULT 0,
    cache_read_tokens INTEGER DEFAULT 0, cache_write_tokens INTEGER DEFAULT 0,
    reasoning_tokens INTEGER DEFAULT 0, billing_provider TEXT, billing_base_url TEXT,
    billing_mode TEXT, estimated_cost_usd REAL, actual_cost_usd REAL,
    cost_status TEXT, cost_source TEXT, pricing_version TEXT, title TEXT,
    api_call_count INTEGER DEFAULT 0, handoff_state TEXT, handoff_platform TEXT,
    handoff_error TEXT
);`
	const v11Messages = `
CREATE TABLE messages (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id TEXT NOT NULL REFERENCES sessions(id),
    role TEXT NOT NULL, content TEXT, tool_call_id TEXT, tool_calls TEXT,
    tool_name TEXT, timestamp REAL NOT NULL, token_count INTEGER,
    finish_reason TEXT, reasoning TEXT, reasoning_content TEXT,
    reasoning_details TEXT, codex_reasoning_items TEXT, codex_message_items TEXT
);`

	// A hypothetical future/forked Hermes that adds platform_message_id (the
	// column our struct models). Proves we still read/write it when present.
	const futureMessages = `
CREATE TABLE messages (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id TEXT NOT NULL REFERENCES sessions(id),
    role TEXT NOT NULL, content TEXT, tool_call_id TEXT, tool_calls TEXT,
    tool_name TEXT, timestamp REAL NOT NULL, token_count INTEGER,
    finish_reason TEXT, reasoning TEXT, reasoning_content TEXT,
    reasoning_details TEXT, codex_reasoning_items TEXT, codex_message_items TEXT,
    platform_message_id TEXT
);`

	// An older Hermes predating the codex_* (messages) and handoff_* (sessions)
	// columns. Proves trailing-column gaps are tolerated on BOTH tables.
	const legacySessions = `
CREATE TABLE sessions (
    id TEXT PRIMARY KEY, source TEXT NOT NULL, user_id TEXT, model TEXT,
    model_config TEXT, system_prompt TEXT, parent_session_id TEXT,
    started_at REAL NOT NULL, ended_at REAL, end_reason TEXT,
    message_count INTEGER DEFAULT 0, tool_call_count INTEGER DEFAULT 0,
    input_tokens INTEGER DEFAULT 0, output_tokens INTEGER DEFAULT 0,
    cache_read_tokens INTEGER DEFAULT 0, cache_write_tokens INTEGER DEFAULT 0,
    reasoning_tokens INTEGER DEFAULT 0, billing_provider TEXT, billing_base_url TEXT,
    billing_mode TEXT, estimated_cost_usd REAL, actual_cost_usd REAL,
    cost_status TEXT, cost_source TEXT, pricing_version TEXT, title TEXT,
    api_call_count INTEGER DEFAULT 0
);`
	const legacyMessages = `
CREATE TABLE messages (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id TEXT NOT NULL REFERENCES sessions(id),
    role TEXT NOT NULL, content TEXT, tool_call_id TEXT, tool_calls TEXT,
    tool_name TEXT, timestamp REAL NOT NULL, token_count INTEGER,
    finish_reason TEXT, reasoning TEXT, reasoning_content TEXT,
    reasoning_details TEXT
);`

	return []schemaCase{
		{name: "v11_current", ddl: v11Sessions + v11Messages + ftsAndTrigger, hasHandoff: true, hasCodex: true, hasPlatformMessageID: false},
		{name: "future_platform_message_id", ddl: v11Sessions + futureMessages + ftsAndTrigger, hasHandoff: true, hasCodex: true, hasPlatformMessageID: true},
		{name: "legacy_pre_codex_pre_handoff", ddl: legacySessions + legacyMessages + ftsAndTrigger, hasHandoff: false, hasCodex: false, hasPlatformMessageID: false},
	}
}

func newSchemaDB(t *testing.T, ddl string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := sql.Open("sqlite", "file:"+path)
	require.NoError(t, err)
	defer db.Close()
	_, err = db.Exec(ddl)
	require.NoError(t, err, "apply fixture schema")
	return path
}

func strptr(s string) *string { return &s }

// TestSchemaCompat_ReadWriteAcrossVersions is the regression for the
// "no such column: platform_message_id" defect. hermesdb must insert and list
// against every supported Hermes schema version, dropping columns absent from
// the live schema (on write) and returning them as nil (on read), while
// round-tripping the columns that do exist.
func TestSchemaCompat_ReadWriteAcrossVersions(t *testing.T) {
	for _, tc := range schemaCases() {
		t.Run(tc.name, func(t *testing.T) {
			path := newSchemaDB(t, tc.ddl)

			in := SessionBundle{
				Session: SessionRow{
					ID:           "sess-1",
					Source:       "cli",
					StartedAt:    100.0,
					HandoffState: strptr("active"), // set even when column absent
				},
				Messages: []MessageRow{
					{Role: "user", Content: strptr("hello"), Timestamp: 101.0,
						CodexMessageItems: strptr("[1]"), PlatformMessageID: strptr("pm-1")},
					{Role: "assistant", Content: strptr("hi there"), Timestamp: 102.0},
				},
			}

			// Write path: must not reference columns the schema lacks.
			require.NoError(t, InsertSession(path, in), "InsertSession must adapt to live columns")

			// Read paths: must not SELECT columns the schema lacks.
			listed, err := ListSessions(path, 0)
			require.NoError(t, err, "ListSessions must adapt to live columns")
			require.Len(t, listed, 1)

			changed, hwm, err := ListChangedSessions(path, 0)
			require.NoError(t, err, "ListChangedSessions must adapt to live columns")
			require.Len(t, changed, 1)
			require.InDelta(t, 102.0, hwm, 0.0001, "HWM = max message timestamp")

			got := listed[0]
			require.Equal(t, "sess-1", got.Session.ID)
			require.Equal(t, "cli", got.Session.Source)
			require.Len(t, got.Messages, 2)
			require.Equal(t, "user", got.Messages[0].Role)
			require.Equal(t, "hello", *got.Messages[0].Content)
			require.Equal(t, "assistant", got.Messages[1].Role)

			// Optional columns: round-trip when present, nil when absent.
			if tc.hasHandoff {
				require.NotNil(t, got.Session.HandoffState)
				require.Equal(t, "active", *got.Session.HandoffState)
			} else {
				require.Nil(t, got.Session.HandoffState, "absent column must read back nil")
			}
			if tc.hasCodex {
				require.NotNil(t, got.Messages[0].CodexMessageItems)
				require.Equal(t, "[1]", *got.Messages[0].CodexMessageItems)
			} else {
				require.Nil(t, got.Messages[0].CodexMessageItems, "absent column must read back nil")
			}
			if tc.hasPlatformMessageID {
				require.NotNil(t, got.Messages[0].PlatformMessageID)
				require.Equal(t, "pm-1", *got.Messages[0].PlatformMessageID)
			} else {
				require.Nil(t, got.Messages[0].PlatformMessageID, "absent column must read back nil")
			}
		})
	}
}
