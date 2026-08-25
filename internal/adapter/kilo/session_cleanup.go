package kilo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const kiloCleanupTimeout = 30 * time.Second

// cleanupImportedKiloSession removes rows that an older materialization of the
// same deterministic Aplexica session left behind. Kilo's public import command
// upserts the rows present in an interchange file but does not delete rows that
// are absent from it. Cleanup runs only after a successful import, so a crash
// before or during this function leaves a complete (if temporarily stale)
// session; the SQLite transaction makes the cleanup itself all-or-nothing.
func (a *Adapter) cleanupImportedKiloSession(doc kiloExportFile) error {
	ctx, cancel := context.WithTimeout(context.Background(), kiloCleanupTimeout)
	defer cancel()

	candidates := a.existingKiloDBCandidatesNewest()
	var candidateErrs []error
	for _, dbPath := range candidates {
		found, err := cleanupKiloImportedSessionDB(ctx, dbPath, doc)
		if found {
			if err != nil {
				return fmt.Errorf("kilo: exact imported-session cleanup failed: %s: %w", dbPath, err)
			}
			return nil
		}
		if err != nil {
			candidateErrs = append(candidateErrs, fmt.Errorf("%s: %w", dbPath, err))
		}
	}
	if len(candidateErrs) > 0 {
		return fmt.Errorf("kilo: exact imported-session cleanup failed: %w", errors.Join(candidateErrs...))
	}
	return fmt.Errorf("kilo: exact imported-session cleanup could not locate session %s", doc.Info.ID)
}

type kiloDBCandidate struct {
	path    string
	modTime time.Time
}

// existingKiloDBCandidatesNewest puts the database most likely touched by the
// just-completed CLI import first. Multiple historical data roots can coexist
// after a Kilo migration; cleanup stops after the first database containing the
// complete imported session rather than mutating an inactive historical copy.
func (a *Adapter) existingKiloDBCandidatesNewest() []string {
	var candidates []kiloDBCandidate
	for _, path := range a.kiloDBCandidates() {
		info, err := os.Lstat(path)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			continue
		}
		candidates = append(candidates, kiloDBCandidate{path: path, modTime: info.ModTime()})
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].modTime.After(candidates[j].modTime)
	})
	out := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		out = append(out, candidate.path)
	}
	return out
}

// cleanupKiloImportedSessionDB performs a narrowly scoped cleanup in one Kilo
// database. found=false means the session is not in this candidate database;
// found=true can accompany an error after the exact row is located so callers
// never mask an active-database failure with an older historical copy. Every
// destructive statement is constrained by the exact session id, and ownership
// is established from both session metadata and deterministic ids.
func cleanupKiloImportedSessionDB(ctx context.Context, dbPath string, doc kiloExportFile) (found bool, err error) {
	if err := validateKiloCleanupDocument(doc); err != nil {
		return false, err
	}
	info, err := os.Lstat(dbPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return false, fmt.Errorf("refusing non-regular Kilo database")
	}

	uriPath := filepath.ToSlash(dbPath)
	if filepath.VolumeName(dbPath) != "" && !strings.HasPrefix(uriPath, "/") {
		// SQLite URI filenames require /C:/... rather than an escaped
		// C:%5C... path on Windows. URL.String then emits file:///C:/....
		uriPath = "/" + uriPath
	}
	dsnURL := &url.URL{Scheme: "file", Path: uriPath}
	query := dsnURL.Query()
	query.Set("_txlock", "immediate")
	dsnURL.RawQuery = query.Encode()
	dsn := dsnURL.String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return false, err
	}
	defer db.Close()
	// Keep PRAGMA state, transaction, and cleanup statements on one connection.
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, `PRAGMA busy_timeout = 5000`); err != nil {
		return false, fmt.Errorf("set busy timeout: %w", err)
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return false, fmt.Errorf("begin cleanup transaction: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	var sessionID, slug, version string
	err = tx.QueryRowContext(ctx,
		`SELECT id, slug, version FROM session WHERE id = ?`, doc.Info.ID,
	).Scan(&sessionID, &slug, &version)
	if errors.Is(err, sql.ErrNoRows) {
		_ = tx.Rollback()
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read imported session: %w", err)
	}
	// From this point onward the candidate contains the exact deterministic
	// session. An error is terminal for this database search: continuing to an
	// older historical DB could hide a partial/unsafe active import and clean
	// the wrong copy instead.
	found = true
	if sessionID != doc.Info.ID || version != "aplexica-sync" || slug != doc.Info.Slug {
		return found, fmt.Errorf("refusing cleanup of session without exact Aplexica ownership metadata")
	}

	desiredMessages, desiredParts, err := kiloCleanupDesiredIDs(doc)
	if err != nil {
		return found, err
	}

	messageIDs, err := kiloSessionMessageIDs(ctx, tx, sessionID)
	if err != nil {
		return found, err
	}
	partRows, err := kiloSessionPartIDs(ctx, tx, sessionID)
	if err != nil {
		return found, err
	}

	// The CLI sometimes exits zero while dropping schema-invalid messages.
	// Refuse cleanup unless every desired row is present; stale rows are safer
	// than turning a partial import into an apparently valid exact projection.
	for id := range desiredMessages {
		if _, ok := messageIDs[id]; !ok {
			return found, fmt.Errorf("imported session is missing desired message %s", id)
		}
	}
	existingParts := make(map[string]struct{}, len(partRows))
	for _, part := range partRows {
		existingParts[part.id] = struct{}{}
	}
	for id := range desiredParts {
		if _, ok := existingParts[id]; !ok {
			return found, fmt.Errorf("imported session is missing desired part %s", id)
		}
	}

	staleMessages := make(map[string]struct{})
	for id := range messageIDs {
		if kiloOwnsMessageID(sessionID, id) {
			if _, keep := desiredMessages[id]; !keep {
				staleMessages[id] = struct{}{}
			}
		}
	}

	staleParts := make(map[string]struct{})
	for _, part := range partRows {
		_, staleMessage := staleMessages[part.messageID]
		if staleMessage {
			// A Kilo-native part attached to an obsolete generated message may
			// represent concurrent user work. Preserve the complete session and
			// let its DB import reconcile that work before retrying cleanup.
			if !kiloOwnsPartID(sessionID, part.id) {
				return found, fmt.Errorf("refusing cleanup: stale generated message %s has native part %s", part.messageID, part.id)
			}
			staleParts[part.id] = struct{}{}
			continue
		}
		if kiloOwnsMessageID(sessionID, part.messageID) && kiloOwnsPartID(sessionID, part.id) {
			if _, keep := desiredParts[part.id]; !keep {
				staleParts[part.id] = struct{}{}
			}
		}
	}

	for _, id := range sortedKiloIDs(staleParts) {
		if err := deleteExactKiloRow(ctx, tx, "part", sessionID, id); err != nil {
			return found, err
		}
	}
	for _, id := range sortedKiloIDs(staleMessages) {
		if err := deleteExactKiloRow(ctx, tx, "message", sessionID, id); err != nil {
			return found, err
		}
	}
	if err := tx.Commit(); err != nil {
		return found, fmt.Errorf("commit exact imported-session cleanup: %w", err)
	}
	return found, nil
}

func validateKiloCleanupDocument(doc kiloExportFile) error {
	if !strings.HasPrefix(doc.Info.ID, syncedSessionIDPrefix) ||
		len(strings.TrimPrefix(doc.Info.ID, syncedSessionIDPrefix)) != sessionIDSeedLen ||
		doc.Info.Version != "aplexica-sync" ||
		!strings.HasPrefix(doc.Info.Slug, "aplexica-") {
		return fmt.Errorf("refusing cleanup for a non-Aplexica Kilo document")
	}
	return nil
}

func kiloCleanupDesiredIDs(doc kiloExportFile) (map[string]struct{}, map[string]struct{}, error) {
	messages := make(map[string]struct{}, len(doc.Messages))
	parts := make(map[string]struct{}, len(doc.Messages))
	for _, message := range doc.Messages {
		id, ok := message.Info["id"].(string)
		if !ok || !kiloOwnsMessageID(doc.Info.ID, id) {
			return nil, nil, fmt.Errorf("refusing cleanup for invalid generated message id")
		}
		messages[id] = struct{}{}
		for _, part := range message.Parts {
			if part.SessionID != doc.Info.ID || part.MessageID != id || !kiloOwnsPartID(doc.Info.ID, part.ID) {
				return nil, nil, fmt.Errorf("refusing cleanup for invalid generated part id")
			}
			parts[part.ID] = struct{}{}
		}
	}
	return messages, parts, nil
}

func kiloOwnsMessageID(sessionID, id string) bool {
	seed, ok := kiloCleanupSeed(sessionID)
	if !ok {
		return false
	}
	if id == "msg_aplxroot"+seed {
		return true
	}
	return kiloIndexedGeneratedID(id, "msg_aplx"+seed)
}

func kiloOwnsPartID(sessionID, id string) bool {
	seed, ok := kiloCleanupSeed(sessionID)
	if !ok {
		return false
	}
	if id == "prt_aplxroot"+seed {
		return true
	}
	return kiloIndexedGeneratedID(id, "prt_aplx"+seed)
}

func kiloCleanupSeed(sessionID string) (string, bool) {
	if !strings.HasPrefix(sessionID, syncedSessionIDPrefix) {
		return "", false
	}
	seed := strings.TrimPrefix(sessionID, syncedSessionIDPrefix)
	if len(seed) != sessionIDSeedLen || len(seed) < msgIDSeedLen {
		return "", false
	}
	return seed[:msgIDSeedLen], true
}

func kiloIndexedGeneratedID(id, prefix string) bool {
	if !strings.HasPrefix(id, prefix) {
		return false
	}
	suffix := strings.TrimPrefix(id, prefix)
	if len(suffix) != generatedIDIndexLen {
		return false
	}
	for _, c := range suffix {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func kiloSessionMessageIDs(ctx context.Context, tx *sql.Tx, sessionID string) (map[string]struct{}, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id FROM message WHERE session_id = ?`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("list imported-session messages: %w", err)
	}
	defer rows.Close()
	out := make(map[string]struct{})
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan imported-session message: %w", err)
		}
		out[id] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read imported-session messages: %w", err)
	}
	return out, nil
}

type kiloPartID struct {
	id        string
	messageID string
}

func kiloSessionPartIDs(ctx context.Context, tx *sql.Tx, sessionID string) ([]kiloPartID, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, message_id FROM part WHERE session_id = ?`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("list imported-session parts: %w", err)
	}
	defer rows.Close()
	var out []kiloPartID
	for rows.Next() {
		var part kiloPartID
		if err := rows.Scan(&part.id, &part.messageID); err != nil {
			return nil, fmt.Errorf("scan imported-session part: %w", err)
		}
		out = append(out, part)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read imported-session parts: %w", err)
	}
	return out, nil
}

func deleteExactKiloRow(ctx context.Context, tx *sql.Tx, table, sessionID, id string) error {
	var query string
	switch table {
	case "message":
		query = `DELETE FROM message WHERE session_id = ? AND id = ?`
	case "part":
		query = `DELETE FROM part WHERE session_id = ? AND id = ?`
	default:
		return fmt.Errorf("unsupported Kilo cleanup table")
	}
	result, err := tx.ExecContext(ctx, query, sessionID, id)
	if err != nil {
		return fmt.Errorf("delete obsolete imported-session %s %s: %w", table, id, err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("verify obsolete imported-session %s %s deletion: %w", table, id, err)
	}
	if rows != 1 {
		return fmt.Errorf("obsolete imported-session %s %s deletion affected %d rows", table, id, rows)
	}
	return nil
}

func sortedKiloIDs(ids map[string]struct{}) []string {
	out := make([]string, 0, len(ids))
	for id := range ids {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}
