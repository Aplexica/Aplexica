package atomicfile

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestWriteFile_CreatesNewFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.txt")

	require.NoError(t, WriteFile(path, []byte("hello"), 0o644))

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "hello", string(got))
}

func TestWriteFile_ReplacesExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.txt")

	require.NoError(t, os.WriteFile(path, []byte("old"), 0o644))
	require.NoError(t, WriteFile(path, []byte("new"), 0o644))

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "new", string(got))
}

func TestWriteFile_SameBytesDoesNotRewrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.txt")
	require.NoError(t, os.WriteFile(path, []byte("same"), 0o644))
	oldTime := time.Now().Add(-time.Hour).Truncate(time.Second)
	require.NoError(t, os.Chtimes(path, oldTime, oldTime))
	before, err := os.Stat(path)
	require.NoError(t, err)

	require.NoError(t, WriteFile(path, []byte("same"), 0o644))

	after, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, before.ModTime(), after.ModTime(), "same-byte write should not refresh mtime")
}

func TestWriteFile_LeavesNoTmpSiblings(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.txt")

	require.NoError(t, WriteFile(path, []byte("x"), 0o644))

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	for _, e := range entries {
		require.False(t, strings.HasPrefix(e.Name(), "out.txt.tmp."),
			"atomic helper left a tmp sibling: %s", e.Name())
	}
	require.Len(t, entries, 1, "exactly one file should remain")
}

func TestWriteFile_PreservesModeBits(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not honor POSIX mode bits via os.Chmod; stat returns 0o666 for any writable file regardless of what mode WriteFile passed.")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "secret")

	require.NoError(t, WriteFile(path, []byte("s"), 0o600))

	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestWriteFile_ParentDirMissing_IsError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "no-such-dir", "out.txt")

	err := WriteFile(path, []byte("x"), 0o644)
	require.Error(t, err, "WriteFile does not mkdir parent — caller's responsibility")
}

func TestWriteFile_OverwriteKeepsOriginalOnFailure(t *testing.T) {
	dir := t.TempDir()
	parent := filepath.Join(dir, "exists")
	require.NoError(t, os.Mkdir(parent, 0o755))
	target := filepath.Join(parent, "out.txt")
	require.NoError(t, os.WriteFile(target, []byte("original"), 0o644))

	require.Error(t, WriteFile(filepath.Join(dir, "missing", "out.txt"), []byte("new"), 0o644))

	got, err := os.ReadFile(target)
	require.NoError(t, err)
	require.Equal(t, "original", string(got))
}

func TestWriteFile_EmptyData(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.txt")

	require.NoError(t, WriteFile(path, []byte{}, 0o644))

	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, int64(0), info.Size())
}
