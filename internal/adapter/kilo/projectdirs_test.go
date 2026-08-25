package kilo

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/stretchr/testify/require"
)

func TestProjectDirs_ReadsCurrentKiloDBSessionDirectories(t *testing.T) {
	home := t.TempDir()
	dbPath := filepath.Join(home, ".local", "share", "kilo", "kilo.db")
	require.NoError(t, os.MkdirAll(filepath.Dir(dbPath), 0o755))
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	_, err = db.Exec(`CREATE TABLE session (
		id TEXT PRIMARY KEY,
		directory TEXT NOT NULL,
		time_created INTEGER,
		time_updated INTEGER
	)`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO session (id, directory, time_created, time_updated) VALUES
		('old', '/Users/testuser/repo', 1000, 1000),
		('new', '/Users/testuser/repo', 1000, 5000),
		('other', '/Users/testuser/other', 2000, 2000)`)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	got, err := (&Adapter{HomeDir: home}).ProjectDirs()
	require.NoError(t, err)
	m := map[string]int64{}
	for _, p := range got {
		m[p.Path] = p.LastActive.Unix()
	}
	require.Equal(t, int64(5), m["/Users/testuser/repo"], "newest session wins per directory")
	require.Equal(t, int64(2), m["/Users/testuser/other"])
	require.Len(t, got, 2)
}
