package hermes

import (
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/aplexica/aplexica/internal/adapter"
	"github.com/aplexica/aplexica/internal/hermesdb"
)

// ProjectDirs reports working directories Hermes has recorded in state.db.
// Newer Hermes schemas include sessions.cwd; older DBs simply return no
// project presence.
func (a *Adapter) ProjectDirs() ([]adapter.ProjectPresence, error) {
	if a.HomeDir == "" {
		return nil, nil
	}
	dbPath := filepath.Join(a.HomeDir, ".hermes", "state.db")
	if _, err := os.Stat(dbPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	db, err := hermesdb.OpenRO(dbPath)
	if err != nil {
		return nil, nil
	}
	defer db.Close()

	rows, err := db.Query(`SELECT cwd, MAX(started_at) FROM sessions
		WHERE cwd IS NOT NULL AND cwd != ''
		GROUP BY cwd`)
	if err != nil {
		return nil, nil
	}
	defer rows.Close()

	var out []adapter.ProjectPresence
	for rows.Next() {
		var cwd string
		var ts float64
		if err := rows.Scan(&cwd, &ts); err != nil {
			return nil, err
		}
		out = append(out, adapter.ProjectPresence{
			Path:       cwd,
			LastActive: unixFloat(ts),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func unixFloat(ts float64) time.Time {
	sec := int64(ts)
	nsec := int64((ts - float64(sec)) * 1e9)
	return time.Unix(sec, nsec).UTC()
}
