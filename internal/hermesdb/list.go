package hermesdb

import (
	"database/sql"
	"fmt"
	"strings"
)

// ListSessions returns all sessions with started_at > sinceUnixSeconds.
// Pass 0 to get every session. Messages are loaded eagerly per session,
// ordered by timestamp ASC then id ASC (insertion order).
//
// Column sets adapt to the installed Hermes schema (see columns.go): an older
// state.db missing columns we model, or a newer one with extras, both work.
func ListSessions(dbPath string, sinceUnixSeconds float64) ([]SessionBundle, error) {
	db, err := OpenRO(dbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	sessCols, msgCols, err := liveColumns(db)
	if err != nil {
		return nil, err
	}

	// Drain sessions first, THEN load messages. OpenRO pins MaxOpenConns=1,
	// so holding the sessions rows iterator while calling loadMessages
	// (which opens a second query) deadlocks waiting for a connection.
	rows, err := db.Query(`SELECT `+strings.Join(sessCols, ", ")+` FROM sessions
		WHERE started_at > ? ORDER BY started_at ASC, id ASC`, sinceUnixSeconds)
	if err != nil {
		return nil, fmt.Errorf("hermesdb: query sessions: %w", err)
	}
	var sessions []SessionRow
	for rows.Next() {
		var s SessionRow
		if err := rows.Scan(bindColumns(sessionScanDest(&s), sessCols)...); err != nil {
			rows.Close()
			return nil, fmt.Errorf("hermesdb: scan session: %w", err)
		}
		sessions = append(sessions, s)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("hermesdb: iterate sessions: %w", err)
	}
	rows.Close()

	out := make([]SessionBundle, 0, len(sessions))
	for _, s := range sessions {
		msgs, err := loadMessages(db, s.ID, msgCols)
		if err != nil {
			return nil, err
		}
		out = append(out, SessionBundle{Session: s, Messages: msgs})
	}
	return out, nil
}

func loadMessages(db *sql.DB, sessionID string, msgCols []string) ([]MessageRow, error) {
	rows, err := db.Query(`SELECT `+strings.Join(msgCols, ", ")+` FROM messages
		WHERE session_id = ? ORDER BY timestamp ASC, id ASC`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("hermesdb: query messages for %s: %w", sessionID, err)
	}
	defer rows.Close()
	var out []MessageRow
	for rows.Next() {
		var m MessageRow
		if err := rows.Scan(bindColumns(messageScanDest(&m), msgCols)...); err != nil {
			return nil, fmt.Errorf("hermesdb: scan message: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("hermesdb: iterate messages: %w", err)
	}
	if out == nil {
		out = []MessageRow{} // canonicalize JSON: empty slice, not nil
	}
	return out, nil
}
