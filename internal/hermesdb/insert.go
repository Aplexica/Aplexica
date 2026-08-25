package hermesdb

import (
	"database/sql"
	"fmt"
	"strings"
)

// AplexicaCanonicalImportSource identifies sessions materialized by Aplexica.
// It is intentionally owned by hermesdb because safe reconciliation must
// verify the existing database row before removing any stale internal rows.
const AplexicaCanonicalImportSource = "aplexica:canonical-import"

// InsertSession writes b into the DB at dbPath. Idempotent:
//   - sessions row: INSERT OR IGNORE on PK (id)
//   - messages: dedupe on (session_id, timestamp, role, content) plus the
//     tool-call columns (tool_calls/tool_call_id/tool_name) so parallel
//     same-timestamp tool calls are not collapsed
//
// Wraps the whole operation in a single transaction. Column sets adapt to the
// installed Hermes schema (see columns.go): fields whose columns are absent
// from the live schema are silently dropped rather than hard-failing the write.
func InsertSession(dbPath string, b SessionBundle) error {
	return insertSession(dbPath, b, nil, false)
}

// InsertPortableSession writes a canonical cross-agent projection and repairs
// stale Aplexica-generated rows left by older adapters. obsolete contains the
// exact rows produced by the former full projection. Repair is deliberately
// conservative: cleanup runs only when the existing row is Aplexica-owned and
// its visible user/assistant text sequence either already equals the incoming
// projection or does so after removing proven historical identities. When
// history has expired, only bounded non-portable rows at/before the incoming
// head timestamp are eligible; newer Hermes rows are always preserved.
func InsertPortableSession(dbPath string, b SessionBundle, obsolete []MessageRow) error {
	return insertSession(dbPath, b, obsolete, true)
}

func insertSession(dbPath string, b SessionBundle, obsolete []MessageRow, portable bool) error {
	db, err := OpenRW(dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	// Introspect before opening the tx: with MaxOpenConns=1 a PRAGMA query
	// issued while a tx holds the single connection would deadlock.
	sessCols, msgCols, err := liveColumns(db)
	if err != nil {
		return err
	}

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("hermesdb: begin: %w", err)
	}
	defer tx.Rollback()

	s := b.Session
	if portable {
		if err := pruneStalePortableInternals(tx, s, b.Messages, obsolete, sessCols, msgCols); err != nil {
			return err
		}
	}
	// Upsert: a re-insert of a session refreshes its display metadata
	// (title / message_count / ended_at) — but ONLY when the existing
	// row has the SAME source, i.e. the daemon is re-exporting its own
	// previously-written session with richer metadata. A hermes-native
	// row colliding on id keeps its data untouched (different source →
	// the WHERE clause blocks the update). The SET list is built from
	// the live column set so older schemas missing one of the metadata
	// columns still work.
	var sets []string
	for _, c := range sessCols {
		switch c {
		case "title":
			sets = append(sets, c+" = COALESCE(excluded."+c+", sessions."+c+")")
		case "ended_at":
			sets = append(sets, c+" = CASE WHEN sessions."+c+" IS NULL THEN excluded."+c+
				" WHEN excluded."+c+" IS NULL THEN sessions."+c+
				" ELSE MAX(sessions."+c+", excluded."+c+") END")
		case "message_count", "tool_call_count":
			sets = append(sets, c+" = excluded."+c)
		}
	}
	stmt := `INSERT OR IGNORE INTO sessions (` + strings.Join(sessCols, ", ") + `) VALUES (` + placeholders(len(sessCols)) + `)`
	if len(sets) > 0 {
		stmt = `INSERT INTO sessions (` + strings.Join(sessCols, ", ") + `) VALUES (` + placeholders(len(sessCols)) + `)` +
			` ON CONFLICT(id) DO UPDATE SET ` + strings.Join(sets, ", ") +
			` WHERE sessions.source = excluded.source`
	}
	if _, err := tx.Exec(stmt, bindColumns(sessionScanDest(&s), sessCols)...); err != nil {
		// Hermes' live schema enforces UNIQUE(title) WHERE title IS NOT
		// NULL. Two synced sessions can derive the same title (identical
		// first user message); retry ONCE with a deterministic short
		// session-id suffix so the export succeeds and stays idempotent
		// across re-export passes.
		if s.Title != nil && strings.Contains(err.Error(), "sessions.title") {
			disambiguated := *s.Title + " · " + titleSuffix(s.ID)
			s.Title = &disambiguated
			if _, retryErr := tx.Exec(stmt, bindColumns(sessionScanDest(&s), sessCols)...); retryErr != nil {
				return fmt.Errorf("hermesdb: insert session %s (title disambiguated): %w", s.ID, retryErr)
			}
		} else {
			return fmt.Errorf("hermesdb: insert session %s: %w", s.ID, err)
		}
	}

	// Live-schema set, computed once, so the per-message dedup probe includes
	// only tool-call columns that actually exist on this Hermes schema.
	msgColSet := make(map[string]bool, len(msgCols))
	for _, c := range msgCols {
		msgColSet[c] = true
	}

	for _, m := range b.Messages {
		// Dedupe key: (session_id, timestamp, role, content) PLUS the tool-call
		// columns. A single assistant turn that fans out parallel tool calls
		// emits several rows sharing timestamp+role with empty content,
		// distinguished only by tool_calls/tool_call_id/tool_name; without
		// those in the key the 2nd..Nth rows were silently dropped (data loss
		// on inbound replication). No composite UNIQUE exists in the Hermes
		// schema, so we probe with a SELECT before INSERT.
		var content string
		if m.Content != nil {
			content = *m.Content
		}
		where := []string{"session_id = ?", "timestamp = ?", "role = ?", "COALESCE(content, '') = ?"}
		probeArgs := []any{s.ID, m.Timestamp, m.Role, content}
		for _, tc := range []struct {
			col string
			val *string
		}{
			{"tool_calls", m.ToolCalls},
			{"tool_call_id", m.ToolCallID},
			{"tool_name", m.ToolName},
		} {
			if !msgColSet[tc.col] {
				continue
			}
			v := ""
			if tc.val != nil {
				v = *tc.val
			}
			where = append(where, "COALESCE("+tc.col+", '') = ?")
			probeArgs = append(probeArgs, v)
		}
		var existing int
		if err := tx.QueryRow(
			`SELECT COUNT(*) FROM messages WHERE `+strings.Join(where, " AND "),
			probeArgs...,
		).Scan(&existing); err != nil {
			return fmt.Errorf("hermesdb: dedupe probe (session=%s ts=%f): %w", s.ID, m.Timestamp, err)
		}
		if existing > 0 {
			continue
		}
		// session_id is the FK (not a MessageRow field), so it leads the
		// column list and arg slice; the live message columns follow.
		cols := append([]string{"session_id"}, msgCols...)
		args := append([]any{s.ID}, bindColumns(messageScanDest(&m), msgCols)...)
		if _, err := tx.Exec(
			`INSERT INTO messages (`+strings.Join(cols, ", ")+`) VALUES (`+placeholders(len(cols))+`)`,
			args...,
		); err != nil {
			return fmt.Errorf("hermesdb: insert message (session=%s ts=%f): %w",
				s.ID, m.Timestamp, err)
		}
	}

	// A portable repair may intentionally preserve unknown/local Hermes rows.
	// Keep the UI's message count truthful instead of replacing it with the
	// smaller inbound projection count. This runs in the same transaction as
	// pruning/insertion, so the value describes the committed row set exactly.
	if portable && containsColumn(sessCols, "message_count") {
		var actual int64
		if err := tx.QueryRow(`SELECT COUNT(*) FROM messages WHERE session_id = ?`, s.ID).Scan(&actual); err != nil {
			return fmt.Errorf("hermesdb: count portable messages for session %s: %w", s.ID, err)
		}
		if _, err := tx.Exec(`UPDATE sessions SET message_count = ? WHERE id = ? AND source = ?`,
			actual, s.ID, AplexicaCanonicalImportSource); err != nil {
			return fmt.Errorf("hermesdb: update portable message count for session %s: %w", s.ID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("hermesdb: commit: %w", err)
	}
	return nil
}

// pruneStalePortableInternals removes rows that cannot be part of the portable
// visible transcript. The equality guard is the important safety property:
// a locally-added Hermes prompt or answer changes the visible sequence and
// blocks cleanup, so an inbound projection can never erase that continuation.
func pruneStalePortableInternals(
	tx *sql.Tx,
	incoming SessionRow,
	messages []MessageRow,
	obsolete []MessageRow,
	sessCols, msgCols []string,
) error {
	if incoming.Source != AplexicaCanonicalImportSource ||
		!containsColumn(sessCols, "source") || !containsColumn(msgCols, "content") {
		return nil
	}

	var existingSource string
	err := tx.QueryRow(`SELECT source FROM sessions WHERE id = ?`, incoming.ID).Scan(&existingSource)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return fmt.Errorf("hermesdb: inspect portable session %s: %w", incoming.ID, err)
	}
	if existingSource != AplexicaCanonicalImportSource {
		return nil
	}

	incomingVisible, portable := visibleTextSequence(messages)
	if !portable {
		// This API is also safe if accidentally given a richer legacy bundle:
		// preserve it exactly and skip the repair path.
		return nil
	}
	existingVisibleRows, err := loadVisibleMessageRows(tx, incoming.ID, msgCols)
	if err != nil {
		return err
	}
	msgColSet := make(map[string]bool, len(msgCols))
	for _, column := range msgCols {
		msgColSet[column] = true
	}
	// Releases before v1.0.39 could feed an Aplexica-owned generated session
	// back through the canonical thread and leave one of two exact visible echo
	// projections in Hermes:
	//
	//   clean:    U1 A1 U2 A2
	//   polluted: U1 A1 A1 U2 A2
	//   conflict: U1 A1 A1 U2 A2 A2 U2
	//
	// The canonical repair already authenticates these layouts before writing
	// its clean head. Hermes has no per-message provenance column, so ownership
	// of the session plus this complete role/content shape is the narrowest
	// durable deletion proof available. Remove the old visible projection as a
	// unit; the normal insert loop below recreates the four exact current rows.
	// Any different/reordered/local continuation blocks this path entirely.
	if legacyAdjacentAssistantEchoRows(messages, existingVisibleRows) {
		for _, message := range existingVisibleRows {
			where, args := messageIdentityWhere(incoming.ID, message, msgColSet)
			if _, err := tx.Exec(`DELETE FROM messages WHERE `+strings.Join(where, " AND "), args...); err != nil {
				return fmt.Errorf("hermesdb: prune owned legacy visible echo for session %s: %w", incoming.ID, err)
			}
		}
		// Continue through the ordinary obsolete/nonportable cleanup using the
		// post-repair visible projection. The rows themselves are inserted after
		// this function returns.
		existingVisibleRows = messages
	}
	if len(obsolete) > 0 {
		// Exact historical identities are the strongest cleanup evidence. Every
		// existing visible row must be either part of the current portable bundle
		// or a proven historical identity. This admits retimestamped replacements
		// and old assistant commentary while a local visible continuation blocks
		// the entire repair.
		if !visibleRowsCoveredByPortableReplacement(
			existingVisibleRows, messages, obsolete, msgColSet,
		) {
			return nil
		}
		for _, message := range obsolete {
			where, args := messageIdentityWhere(incoming.ID, message, msgColSet)
			if _, err := tx.Exec(`DELETE FROM messages WHERE `+strings.Join(where, " AND "), args...); err != nil {
				return fmt.Errorf("hermesdb: prune portable internal for session %s: %w", incoming.ID, err)
			}
		}
		return nil
	}

	// Once compacted canonical history has expired, exact identities no longer
	// exist to drive the migration. The fallback remains deliberately narrow:
	// this is an Aplexica-owned session with an exact visible-sequence match, and
	// it removes only non-portable rows no newer than the newest incoming turn.
	// A native system/tool row written after that boundary is an in-flight Hermes
	// continuation and must survive for the inbound watcher to import.
	if !sameVisibleTextSequence(existingVisibleRows, incomingVisible) {
		return nil
	}
	return pruneBoundedNonPortableRows(tx, incoming.ID, messages, msgCols)
}

func legacyAdjacentAssistantEchoRows(clean, polluted []MessageRow) bool {
	const (
		cleanTurns                    = 4
		adjacentEchoTurns             = 5
		materializedEchoTurns         = 7
		cleanSecondQuestionIndex      = 2
		cleanSecondAnswerIndex        = 3
		pollutedDuplicateAnswerIndex  = 2
		pollutedSecondQuestionIndex   = 3
		pollutedSecondAnswerIndex     = 4
		pollutedTrailingAnswerIndex   = 5
		pollutedTrailingQuestionIndex = 6
	)
	if len(clean) != cleanTurns ||
		(len(polluted) != adjacentEchoTurns && len(polluted) != materializedEchoTurns) {
		return false
	}
	for _, message := range clean {
		if !isPortableTextMessage(message) {
			return false
		}
	}
	for _, message := range polluted {
		if !isPortableTextMessage(message) {
			return false
		}
	}
	equal := func(a, b MessageRow) bool {
		return a.Role == b.Role && stringPointerValue(a.Content) == stringPointerValue(b.Content)
	}
	// U1,A1,A1,U2,A2.
	if !equal(polluted[0], clean[0]) || !equal(polluted[1], clean[1]) ||
		!equal(polluted[pollutedDuplicateAnswerIndex], clean[1]) ||
		!equal(polluted[pollutedSecondQuestionIndex], clean[cleanSecondQuestionIndex]) ||
		!equal(polluted[pollutedSecondAnswerIndex], clean[cleanSecondAnswerIndex]) {
		return false
	}
	if len(polluted) == adjacentEchoTurns {
		return true
	}
	// Exact materialized conflict suffix A2,U2.
	return equal(polluted[pollutedTrailingAnswerIndex], clean[cleanSecondAnswerIndex]) &&
		equal(polluted[pollutedTrailingQuestionIndex], clean[cleanSecondQuestionIndex])
}

type messageIdentity struct {
	timestamp  float64
	role       string
	content    string
	toolCalls  string
	toolCallID string
	toolName   string
}

func messageIdentityForColumns(message MessageRow, columns map[string]bool) messageIdentity {
	identity := messageIdentity{
		timestamp: message.Timestamp,
		role:      message.Role,
		content:   stringPointerValue(message.Content),
	}
	if columns["tool_calls"] {
		identity.toolCalls = stringPointerValue(message.ToolCalls)
	}
	if columns["tool_call_id"] {
		identity.toolCallID = stringPointerValue(message.ToolCallID)
	}
	if columns["tool_name"] {
		identity.toolName = stringPointerValue(message.ToolName)
	}
	return identity
}

func stringPointerValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func visibleRowsCoveredByPortableReplacement(
	existing []MessageRow,
	incoming []MessageRow,
	obsolete []MessageRow,
	columns map[string]bool,
) bool {
	allowed := make(map[messageIdentity]struct{}, len(incoming)+len(obsolete))
	for _, message := range incoming {
		allowed[messageIdentityForColumns(message, columns)] = struct{}{}
	}
	for _, message := range obsolete {
		allowed[messageIdentityForColumns(message, columns)] = struct{}{}
	}
	for _, message := range existing {
		if _, covered := allowed[messageIdentityForColumns(message, columns)]; !covered {
			return false
		}
	}
	return true
}

func pruneBoundedNonPortableRows(tx *sql.Tx, sessionID string, messages []MessageRow, msgCols []string) error {
	if len(messages) == 0 {
		return nil
	}
	newest := messages[0].Timestamp
	for _, message := range messages[1:] {
		if message.Timestamp > newest {
			newest = message.Timestamp
		}
	}

	portable := []string{
		"role IN ('user', 'assistant')",
		"content IS NOT NULL",
		"TRIM(content) <> ''",
	}
	for _, column := range []string{
		"tool_call_id", "tool_calls", "tool_name", "token_count", "finish_reason",
		"reasoning", "reasoning_content", "reasoning_details", "codex_reasoning_items",
		"codex_message_items", "platform_message_id",
	} {
		if containsColumn(msgCols, column) {
			portable = append(portable, column+" IS NULL")
		}
	}
	if _, err := tx.Exec(
		`DELETE FROM messages WHERE session_id = ? AND timestamp <= ? AND NOT (`+
			strings.Join(portable, " AND ")+`)`,
		sessionID, newest,
	); err != nil {
		return fmt.Errorf("hermesdb: prune bounded portable internals for session %s: %w", sessionID, err)
	}
	return nil
}

func messageIdentityWhere(sessionID string, message MessageRow, columns map[string]bool) ([]string, []any) {
	content := ""
	if message.Content != nil {
		content = *message.Content
	}
	where := []string{"session_id = ?", "timestamp = ?", "role = ?", "COALESCE(content, '') = ?"}
	args := []any{sessionID, message.Timestamp, message.Role, content}
	for _, field := range []struct {
		column string
		value  *string
	}{
		{"tool_calls", message.ToolCalls},
		{"tool_call_id", message.ToolCallID},
		{"tool_name", message.ToolName},
	} {
		if !columns[field.column] {
			continue
		}
		value := ""
		if field.value != nil {
			value = *field.value
		}
		where = append(where, "COALESCE("+field.column+", '') = ?")
		args = append(args, value)
	}
	return where, args
}

type visibleTextMessage struct {
	role    string
	content string
}

func visibleTextSequence(messages []MessageRow) ([]visibleTextMessage, bool) {
	out := make([]visibleTextMessage, 0, len(messages))
	for _, message := range messages {
		if !isPortableTextMessage(message) {
			return nil, false
		}
		out = append(out, visibleTextMessage{role: message.Role, content: *message.Content})
	}
	return out, true
}

func sameVisibleTextSequence(existing []MessageRow, incoming []visibleTextMessage) bool {
	if len(existing) != len(incoming) {
		return false
	}
	for i, message := range existing {
		if (visibleTextMessage{role: message.Role, content: stringPointerValue(message.Content)}) != incoming[i] {
			return false
		}
	}
	return true
}

type messageRowsQueryer interface {
	Query(query string, args ...any) (*sql.Rows, error)
}

func loadVisibleMessageRows(queryer messageRowsQueryer, sessionID string, msgCols []string) ([]MessageRow, error) {
	rows, err := queryer.Query(`SELECT `+strings.Join(msgCols, ", ")+` FROM messages
		WHERE session_id = ? AND role IN ('user', 'assistant')
		  AND content IS NOT NULL AND TRIM(content) <> ''
		ORDER BY timestamp ASC, id ASC`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("hermesdb: read visible messages for session %s: %w", sessionID, err)
	}
	defer rows.Close()

	var out []MessageRow
	for rows.Next() {
		var message MessageRow
		if err := rows.Scan(bindColumns(messageScanDest(&message), msgCols)...); err != nil {
			return nil, fmt.Errorf("hermesdb: scan visible message for session %s: %w", sessionID, err)
		}
		out = append(out, message)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("hermesdb: iterate visible messages for session %s: %w", sessionID, err)
	}
	return out, nil
}

func isVisibleTextMessage(role string, content *string) bool {
	return (role == "user" || role == "assistant") && content != nil && strings.TrimSpace(*content) != ""
}

func isPortableTextMessage(message MessageRow) bool {
	if !isVisibleTextMessage(message.Role, message.Content) {
		return false
	}
	return message.ToolCallID == nil && message.ToolCalls == nil && message.ToolName == nil &&
		message.TokenCount == nil && message.FinishReason == nil && message.Reasoning == nil &&
		message.ReasoningContent == nil && message.ReasoningDetails == nil &&
		message.CodexReasoningItems == nil && message.CodexMessageItems == nil &&
		message.PlatformMessageID == nil
}

func containsColumn(columns []string, want string) bool {
	for _, column := range columns {
		if column == want {
			return true
		}
	}
	return false
}

// titleSuffix returns a short stable discriminator for title-collision
// retries — the last 6 characters of the session id (the most-varying part
// of both UUIDs and hermes timestamp ids).
func titleSuffix(id string) string {
	const n = 6
	r := []rune(id)
	if len(r) <= n {
		return id
	}
	return string(r[len(r)-n:])
}
