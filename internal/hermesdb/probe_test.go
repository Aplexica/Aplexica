package hermesdb

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

func TestModerncSqliteHasFTS5(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "probe.db")
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	defer db.Close()
	_, err = db.Exec(`CREATE VIRTUAL TABLE t USING fts5(x)`)
	require.NoError(t, err, "modernc.org/sqlite must be built with FTS5")
	_, err = db.Exec(`INSERT INTO t(x) VALUES ('hello world')`)
	require.NoError(t, err)
	var n int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM t WHERE x MATCH 'hello'`).Scan(&n))
	require.Equal(t, 1, n)
}
