package kilo

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/aplexica/aplexica/internal/adapter"
	_ "modernc.org/sqlite"
)

// ProjectDirs reports working directories Kilo has recorded in its current
// SQLite session store. Kilo stores this at <XDG_DATA_HOME>/kilo/kilo.db
// (macOS: ~/Library/Application Support/kilo/kilo.db).
func (a *Adapter) ProjectDirs() ([]adapter.ProjectPresence, error) {
	byPath := map[string]adapter.ProjectPresence{}
	for _, dbPath := range a.kiloDBCandidates() {
		presences, err := readKiloDBProjectDirs(dbPath)
		if err != nil {
			continue
		}
		for _, cur := range presences {
			if existing, ok := byPath[cur.Path]; ok {
				cur = adapter.NewerPresence(existing, cur)
			}
			byPath[cur.Path] = cur
		}
	}
	out := make([]adapter.ProjectPresence, 0, len(byPath))
	for _, v := range byPath {
		out = append(out, v)
	}
	return out, nil
}

func readKiloDBProjectDirs(dbPath string) ([]adapter.ProjectPresence, error) {
	if _, err := os.Stat(dbPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	db, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.Query(`SELECT directory, MAX(COALESCE(time_updated, time_created, 0)) FROM session
		WHERE directory IS NOT NULL AND directory != ''
		GROUP BY directory`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []adapter.ProjectPresence
	for rows.Next() {
		var dir string
		var ms int64
		if err := rows.Scan(&dir, &ms); err != nil {
			return nil, err
		}
		out = append(out, adapter.ProjectPresence{
			Path:       dir,
			LastActive: kiloMillis(ms),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func kiloMillis(ms int64) time.Time {
	if ms <= 0 {
		return time.Time{}
	}
	return time.Unix(ms/1000, (ms%1000)*int64(time.Millisecond)).UTC()
}

func (a *Adapter) kiloDBCandidates() []string {
	if a.HomeDir == "" {
		return nil
	}
	roots := []string{
		filepath.Join(a.HomeDir, "Library", "Application Support", "kilo"),
		filepath.Join(a.HomeDir, ".local", "share", "kilo"),
	}
	if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
		roots = append([]string{filepath.Join(xdg, "kilo")}, roots...)
	}
	out := make([]string, 0, len(roots))
	seen := map[string]struct{}{}
	for _, root := range roots {
		p := filepath.Join(root, "kilo.db")
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	return out
}
