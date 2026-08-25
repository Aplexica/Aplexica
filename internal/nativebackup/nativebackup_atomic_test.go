package nativebackup

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestCopyFile_FailedCopyLeavesDestinationIntact: restore replays backup files
// back over LIVE native paths. A copy that fails partway (disk full, I/O error,
// SIGKILL) must not truncate/corrupt the live file — NFR-01.4 / FR-01.16
// require the restore target to stay self-consistent. We force a mid-copy read
// failure by using a directory as the source (read returns EISDIR) and assert
// the pre-existing destination is untouched and no temp file leaks. A read
// failure is now reported as a per-file skip rather than a fatal error, so one
// unreadable source never aborts the whole snapshot.
func TestCopyFile_FailedCopyLeavesDestinationIntact(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "live.db")
	const original = "ORIGINAL-IMPORTANT-DATA"
	require.NoError(t, os.WriteFile(dst, []byte(original), 0o644))

	srcDir := filepath.Join(dir, "srcdir")
	require.NoError(t, os.Mkdir(srcDir, 0o755))

	entry, skipped, err := copyFile(srcDir, dst, dir)
	require.NoError(t, err, "a read failure must be a per-file skip, not a fatal abort")
	require.Equal(t, FileEntry{}, entry, "no manifest entry for a file that failed to copy")
	require.NotNil(t, skipped, "the unreadable source must be recorded as skipped")

	got, rerr := os.ReadFile(dst)
	require.NoError(t, rerr)
	require.Equal(t, original, string(got),
		"a failed restore must leave the live file untouched, not truncate it")

	// No leftover temp files in the destination directory.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		require.NotContains(t, e.Name(), ".tmp", "temp file must be cleaned up on failure")
	}
}
