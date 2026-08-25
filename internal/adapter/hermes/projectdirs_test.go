package hermes

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/aplexica/aplexica/internal/hermesdb"
	"github.com/stretchr/testify/require"
)

func TestProjectDirs_ReadsCwdFromStateDB(t *testing.T) {
	home := t.TempDir()
	dbPath := filepath.Join(home, ".hermes", "state.db")
	require.NoError(t, os.MkdirAll(filepath.Dir(dbPath), 0o755))
	db, err := hermesdb.InitTestDB(dbPath)
	require.NoError(t, err)
	_, err = db.Exec(`ALTER TABLE sessions ADD COLUMN cwd TEXT`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO sessions (id, source, started_at, cwd) VALUES ('old','cli',100.0,'/Users/testuser/repo')`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO sessions (id, source, started_at, cwd) VALUES ('new','cli',500.0,'/Users/testuser/repo')`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO sessions (id, source, started_at, cwd) VALUES ('other','cli',300.0,'/Users/testuser/other')`)
	require.NoError(t, err)
	db.Close()

	got, err := (&Adapter{HomeDir: home}).ProjectDirs()
	require.NoError(t, err)
	m := map[string]string{}
	for _, p := range got {
		m[p.Path] = p.LastActive.UTC().Format("15:04:05")
	}
	require.Equal(t, "00:08:20", m["/Users/testuser/repo"], "newest session wins per cwd")
	require.Equal(t, "00:05:00", m["/Users/testuser/other"])
	require.Len(t, got, 2)
}
