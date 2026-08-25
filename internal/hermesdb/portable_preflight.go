package hermesdb

import (
	"database/sql"
	"fmt"
)

// PortableRepairNeedsHistory reports whether an existing Aplexica-owned
// Hermes session has visible row identities that differ from the incoming
// portable projection. Exact rows need no historical replay: the bounded
// InsertPortableSession fallback can remove stale non-visible system/tool
// internals. A divergent role/content/timestamp sequence may include old
// assistant commentary or retimestamped rows, so the exporter must load the
// compacted canonical history once to derive precise obsolete identities.
//
// This is only a read optimization. InsertPortableSession repeats ownership
// and equality guards transactionally before deleting anything.
func PortableRepairNeedsHistory(dbPath string, incoming SessionBundle) (bool, error) {
	if incoming.Session.Source != AplexicaCanonicalImportSource {
		return false, nil
	}
	for _, message := range incoming.Messages {
		if !isPortableTextMessage(message) {
			return false, nil
		}
	}
	db, err := OpenRO(dbPath)
	if err != nil {
		return false, err
	}
	defer db.Close()
	sessCols, msgCols, err := liveColumns(db)
	if err != nil {
		return false, err
	}
	if !containsColumn(sessCols, "source") || !containsColumn(msgCols, "content") ||
		!containsColumn(msgCols, "timestamp") || !containsColumn(msgCols, "role") {
		return false, nil
	}
	var source string
	err = db.QueryRow(`SELECT source FROM sessions WHERE id = ?`, incoming.Session.ID).Scan(&source)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("hermesdb: inspect portable preflight session %s: %w", incoming.Session.ID, err)
	}
	if source != AplexicaCanonicalImportSource {
		return false, nil
	}
	existing, err := loadVisibleMessageRows(db, incoming.Session.ID, msgCols)
	if err != nil {
		return false, err
	}
	if len(existing) != len(incoming.Messages) {
		return true, nil
	}
	for i := range existing {
		if existing[i].Timestamp != incoming.Messages[i].Timestamp ||
			existing[i].Role != incoming.Messages[i].Role ||
			stringPointerValue(existing[i].Content) != stringPointerValue(incoming.Messages[i].Content) {
			return true, nil
		}
	}
	return false, nil
}
