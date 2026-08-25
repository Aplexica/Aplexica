package hermesdb

import (
	"fmt"
	"strings"
)

// ListChangedSessions returns every session whose started_at is greater than
// sinceUnixSeconds OR whose most-recent message timestamp is greater than
// sinceUnixSeconds. Returned bundles contain the FULL message history for each
// session (not just the messages newer than the HWM) — Aplexica's import path
// is idempotent and reconstructs each artifact from the full bundle, so partial
// bundles would force complex merge logic on the import side.
//
// Second return value is the new high-water mark: the max of (started_at,
// max(messages.timestamp)) across all returned sessions. The caller advances
// its persistent HWM to max(oldHWM, newHWM). If no sessions matched, the
// returned HWM equals sinceUnixSeconds (caller's HWM does not regress).
//
// Connection-pool note: like ListSessions, this drains the outer sessions
// iterator BEFORE calling loadMessages — modernc.org/sqlite with
// SetMaxOpenConns(1) deadlocks otherwise.
func ListChangedSessions(dbPath string, sinceUnixSeconds float64) (bundles []SessionBundle, highWaterMark float64, err error) {
	db, err := OpenRO(dbPath)
	if err != nil {
		return nil, sinceUnixSeconds, err
	}
	defer db.Close()

	sessCols, msgCols, err := liveColumns(db)
	if err != nil {
		return nil, sinceUnixSeconds, err
	}

	// Compose: sessions where started_at > since OR there exists a message
	// with timestamp > since. Also pull the per-session MAX(messages.timestamp)
	// so we can compute the HWM in one pass. Session columns adapt to the live
	// schema; the computed max_msg_ts is appended after them.
	q := `SELECT ` + strings.Join(sessCols, ", ") + `,
		COALESCE((SELECT MAX(timestamp) FROM messages WHERE session_id = sessions.id), 0) AS max_msg_ts
		FROM sessions
		WHERE started_at > ?
		   OR EXISTS (SELECT 1 FROM messages WHERE session_id = sessions.id AND timestamp > ?)
		ORDER BY started_at ASC, id ASC`

	rows, err := db.Query(q, sinceUnixSeconds, sinceUnixSeconds)
	if err != nil {
		return nil, sinceUnixSeconds, fmt.Errorf("hermesdb: query changed sessions: %w", err)
	}

	type sessionPlus struct {
		row      SessionRow
		maxMsgTS float64
	}
	var rowsBuf []sessionPlus
	hwm := sinceUnixSeconds

	for rows.Next() {
		var s SessionRow
		var maxMsgTS float64
		dest := append(bindColumns(sessionScanDest(&s), sessCols), &maxMsgTS)
		if err := rows.Scan(dest...); err != nil {
			rows.Close()
			return nil, sinceUnixSeconds, fmt.Errorf("hermesdb: scan changed session: %w", err)
		}
		rowsBuf = append(rowsBuf, sessionPlus{row: s, maxMsgTS: maxMsgTS})
		if s.StartedAt > hwm {
			hwm = s.StartedAt
		}
		if maxMsgTS > hwm {
			hwm = maxMsgTS
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, sinceUnixSeconds, fmt.Errorf("hermesdb: iterate changed sessions: %w", err)
	}
	rows.Close()

	out := make([]SessionBundle, 0, len(rowsBuf))
	for _, sp := range rowsBuf {
		msgs, lerr := loadMessages(db, sp.row.ID, msgCols)
		if lerr != nil {
			return nil, sinceUnixSeconds, lerr
		}
		out = append(out, SessionBundle{Session: sp.row, Messages: msgs})
	}
	return out, hwm, nil
}
