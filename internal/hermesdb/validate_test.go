package hermesdb

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValidate_ValidHermesDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := InitTestDB(path)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	require.NoError(t, Validate(path), "a freshly-initialized Hermes schema must validate")
}

func TestValidate_ZeroByteFile_IsNotHermesDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	require.NoError(t, os.WriteFile(path, nil, 0o644)) // the dev-Mac case

	err := Validate(path)
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrNotHermesDB), "0-byte file must report ErrNotHermesDB, got %v", err)
}

func TestValidate_MissingFile_IsNotHermesDB(t *testing.T) {
	err := Validate(filepath.Join(t.TempDir(), "nope.db"))
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrNotHermesDB), "missing file must report ErrNotHermesDB, got %v", err)
}

func TestValidate_WrongSchema_IsNotHermesDB(t *testing.T) {
	// A SQLite DB whose sessions table lacks the core 'id' column must be
	// rejected as an incompatible Hermes database.
	path := newSchemaDB(t, `CREATE TABLE sessions (started_at REAL);
		CREATE TABLE messages (role TEXT, timestamp REAL);`)

	err := Validate(path)
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrNotHermesDB), "wrong schema must report ErrNotHermesDB, got %v", err)
}
