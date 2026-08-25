package hermesdb

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ErrNotHermesDB marks a path that is not a usable Hermes state.db: missing,
// empty, or carrying a schema without the core sessions/messages columns. It
// is a PERMANENT condition (re-polling won't fix it), so callers use
// errors.Is(err, ErrNotHermesDB) to skip/disable a hermeswatch loop instead of
// retrying — and logging — on every tick. Transient failures (a locked DB, an
// I/O hiccup) do NOT wrap this sentinel and remain retryable.
var ErrNotHermesDB = errors.New("not a Hermes state.db")

// Validate reports whether dbPath is a usable Hermes state.db. It returns nil
// when the file exists, is non-empty, and exposes the core sessions/messages
// columns; otherwise it returns an error wrapping ErrNotHermesDB. It never
// creates the file. Use this as a pre-flight before starting a poll loop.
func Validate(dbPath string) error {
	abs, err := filepath.Abs(dbPath)
	if err != nil {
		return fmt.Errorf("hermesdb: resolve %s: %w", dbPath, err)
	}
	fi, err := os.Stat(abs)
	if err != nil {
		// Missing (or unstattable) → treat as "not a Hermes DB" so the caller
		// skips it the same way it skips a wrong-schema file.
		return fmt.Errorf("hermesdb: %s not present: %w", abs, errors.Join(err, ErrNotHermesDB))
	}
	if fi.IsDir() {
		return fmt.Errorf("hermesdb: %s is a directory: %w", abs, ErrNotHermesDB)
	}
	if fi.Size() == 0 {
		// A 0-byte file opens as an empty SQLite DB with no tables — the most
		// common stale-leftover case. Reject before opening.
		return fmt.Errorf("hermesdb: %s is empty: %w", abs, ErrNotHermesDB)
	}
	db, err := OpenRO(abs)
	if err != nil {
		return fmt.Errorf("hermesdb: open %s: %w", abs, err)
	}
	defer db.Close()
	if _, _, err := liveColumns(db); err != nil {
		// liveColumns' core-column failures wrap ErrNotHermesDB (see columns.go).
		return err
	}
	return nil
}
