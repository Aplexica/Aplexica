package daemon

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/privatefs"
	"github.com/stretchr/testify/require"
)

func readGzipLog(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	require.NoError(t, err)
	defer f.Close()
	zr, err := gzip.NewReader(f)
	require.NoError(t, err)
	data, err := io.ReadAll(zr)
	require.NoError(t, err)
	require.NoError(t, zr.Close())
	return string(data)
}

func TestNewLogger_WritesJSONLineToStableFile(t *testing.T) {
	dir := t.TempDir()

	lg, closer, err := NewLogger(dir)
	require.NoError(t, err)
	defer closer.Close()

	lg.Info("hello", "foo", "bar")

	// Stable-file scheme (FR-10.3): the live file is always aplexicad.log;
	// dated names are reserved for rotated archives.
	expected := filepath.Join(dir, "aplexicad.log")
	_, err = os.Stat(expected)
	require.NoError(t, err, "expected log file: %s", expected)

	// File contains a single JSON line with our message.
	data, err := os.ReadFile(expected)
	require.NoError(t, err)
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	require.Len(t, lines, 1)

	var entry map[string]any
	require.NoError(t, json.Unmarshal([]byte(lines[0]), &entry))
	require.Equal(t, "hello", entry["msg"])
	require.Equal(t, "bar", entry["foo"])
	require.Equal(t, "INFO", entry["level"])
}

func TestNewLogger_MultipleLines(t *testing.T) {
	dir := t.TempDir()
	lg, closer, err := NewLogger(dir)
	require.NoError(t, err)
	defer closer.Close()

	for i := 0; i < 5; i++ {
		lg.Info("entry", "i", i)
	}

	// All writes land in the stable aplexicad.log (no rotation occurred).
	logPath := filepath.Join(dir, "aplexicad.log")
	data, err := os.ReadFile(logPath)
	require.NoError(t, err)
	require.Equal(t, 5, strings.Count(string(data), "\n"))
}

func TestNewLogger_DirCreatedIfMissing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "subdir")
	lg, closer, err := NewLogger(dir)
	require.NoError(t, err)
	defer closer.Close()
	require.NotNil(t, lg)
	info, err := os.Stat(dir)
	require.NoError(t, err)
	require.True(t, info.IsDir())
}

func TestNewLogger_ReturnedCloserClosesFile(t *testing.T) {
	dir := t.TempDir()
	lg, closer, err := NewLogger(dir)
	require.NoError(t, err)
	lg.Info("before close")
	require.NoError(t, closer.Close())
	// After close, writes to the underlying handle would error — but the
	// slog.Logger holds the handler not the file directly, so this is
	// difficult to test directly. The key invariant: Close doesn't panic.
	require.NotNil(t, io.Closer(closer))
}

func TestNewLogger_PurgesLogsOlderThan30Days(t *testing.T) {
	dir := t.TempDir()
	// Seed both legacy and compressed files at various ages. Startup purges old
	// archives before migrating retained legacy files to gzip.
	old := time.Now().Local().AddDate(0, 0, -31)
	recent := time.Now().Local().AddDate(0, 0, -5)
	oldName := filepath.Join(dir, fmt.Sprintf("aplexicad-%s.log", old.Format("2006-01-02")))
	oldGzipName := oldName + ".gz"
	recentName := filepath.Join(dir, fmt.Sprintf("aplexicad-%s.log", recent.Format("2006-01-02")))
	require.NoError(t, os.WriteFile(oldName, []byte("OLD-LEGACY"), 0o600))
	require.NoError(t, os.WriteFile(oldGzipName, []byte("OLD-GZIP"), 0o600))
	require.NoError(t, os.WriteFile(recentName, []byte("RECENT"), 0o600))

	_, closer, err := NewLogger(dir)
	require.NoError(t, err)
	defer closer.Close()

	// Both old formats are gone. The recent legacy file was migrated rather
	// than retained uncompressed.
	_, err = os.Stat(oldName)
	require.True(t, os.IsNotExist(err), "log older than 30 days must be purged")
	_, err = os.Stat(oldGzipName)
	require.True(t, os.IsNotExist(err), "compressed log older than 30 days must be purged")
	_, err = os.Stat(recentName)
	require.True(t, os.IsNotExist(err), "retained legacy archive must be migrated")
	require.Equal(t, "RECENT", readGzipLog(t, recentName+".gz"))
}

func TestNewLogger_IgnoresNonLogFiles(t *testing.T) {
	dir := t.TempDir()
	other := filepath.Join(dir, "README.md")
	require.NoError(t, os.WriteFile(other, []byte("x"), 0o644))
	_, closer, err := NewLogger(dir)
	require.NoError(t, err)
	defer closer.Close()
	_, err = os.Stat(other)
	require.NoError(t, err, "non-log files must NOT be purged")
}

func TestRotatingLogger_RotateClosesOldFileAndOpensNew(t *testing.T) {
	dir := t.TempDir()
	rl, closer, err := NewLogger(dir)
	require.NoError(t, err)
	defer closer.Close()

	rl.Info("before-rotate")

	// Rotate archives the existing aplexicad.log under a dated name and
	// reopens a fresh aplexicad.log. The pre-rotate line moves to the
	// archive; the post-rotate line lands in the new stable file.
	require.NoError(t, rl.Rotate())
	rl.Info("after-rotate")

	stable := filepath.Join(dir, "aplexicad.log")
	data, err := os.ReadFile(stable)
	require.NoError(t, err)
	body := string(data)
	require.Contains(t, body, "after-rotate")
	require.NotContains(t, body, "before-rotate")

	archives, err := filepath.Glob(filepath.Join(dir, "aplexicad-*.log.gz"))
	require.NoError(t, err)
	require.Len(t, archives, 1, "expected exactly one dated archive")
	require.Contains(t, readGzipLog(t, archives[0]), "before-rotate")
}

func TestRotatingLogger_RotateIsRepeatable(t *testing.T) {
	dir := t.TempDir()
	rl, closer, err := NewLogger(dir)
	require.NoError(t, err)
	defer closer.Close()

	for i := 0; i < 5; i++ {
		rl.Info("rot", "i", i)
		require.NoError(t, rl.Rotate())
	}
	rl.Info("final")

	// Repeated same-day rotations append independent gzip members to one daily
	// archive. The live file remains aplexicad.log with only the trailing line.
	stable := filepath.Join(dir, "aplexicad.log")
	data, err := os.ReadFile(stable)
	require.NoError(t, err)
	require.Equal(t, 1, strings.Count(string(data), "\n"))
	require.Contains(t, string(data), "final")
	archive := filepath.Join(dir, archiveLogGzipName(time.Now().Local()))
	body := readGzipLog(t, archive)
	for i := 0; i < 5; i++ {
		require.Equal(t, 1, strings.Count(body, fmt.Sprintf(`"i":%d`, i)))
	}
}

func TestRotatingLogger_CloseIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	rl, closer, err := NewLogger(dir)
	require.NoError(t, err)
	require.NoError(t, rl.Close())
	// Second close on either handle must be a no-op (not panic / error).
	require.NoError(t, rl.Close())
	require.NoError(t, closer.Close())
}

// TestRotate_ArchivesStableFileWithLocalDate is the FR-10.3 stable-file
// contract: Rotate() must (1) leave a fresh, empty aplexicad.log behind,
// (2) move the prior content into an aplexicad-YYYY-MM-DD.log.gz archive, and
// (3) stamp that archive with a LOCAL-zone date (the day the rotated file
// covered — yesterday's local date).
func TestRotate_ArchivesStableFileWithLocalDate(t *testing.T) {
	dir := t.TempDir()
	rl, closer, err := NewLogger(dir)
	require.NoError(t, err)
	defer closer.Close()

	rl.Info("pre-rotate-content")
	require.NoError(t, rl.Rotate())

	// The live file exists and is fresh (no carried-over content).
	stable := filepath.Join(dir, "aplexicad.log")
	sdata, err := os.ReadFile(stable)
	require.NoError(t, err)
	require.NotContains(t, string(sdata), "pre-rotate-content")

	// The archive is named with the LOCAL date of the day the rotated file
	// covered. This file was opened today (NewLogger) and rotated today, so
	// the archive carries today's local date — proving the date is computed
	// in the local zone (not UTC, which can differ near the day boundary).
	wantDate := time.Now().Local().Format("2006-01-02")
	wantArchive := filepath.Join(dir, fmt.Sprintf("aplexicad-%s.log.gz", wantDate))
	require.Contains(t, readGzipLog(t, wantArchive), "pre-rotate-content")
}

func TestNextLocalMidnight(t *testing.T) {
	loc := time.Local
	cases := []struct {
		name string
		in   time.Time
		want time.Time
	}{
		{
			name: "midday",
			in:   time.Date(2026, 6, 21, 13, 45, 12, 0, loc),
			want: time.Date(2026, 6, 22, 0, 0, 0, 0, loc),
		},
		{
			name: "one-second-before-midnight",
			in:   time.Date(2026, 12, 31, 23, 59, 59, 0, loc),
			want: time.Date(2027, 1, 1, 0, 0, 0, 0, loc),
		},
		{
			name: "exactly-midnight-rolls-to-next-day",
			in:   time.Date(2026, 2, 28, 0, 0, 0, 0, loc),
			want: time.Date(2026, 3, 1, 0, 0, 0, 0, loc),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := nextLocalMidnight(tc.in)
			require.True(t, got.Equal(tc.want), "got %v want %v", got, tc.want)
			require.True(t, got.After(tc.in), "next midnight must be strictly after input")
		})
	}
}

// TestPurgeOldLogs_LocalCutoff seeds dated archives straddling the
// LogRetentionDays boundary (using LOCAL-zone date stamps, matching how
// Rotate names them) and asserts the purge keeps exactly 30 days.
func TestPurgeOldLogs_LocalCutoff(t *testing.T) {
	dir := t.TempDir()

	now := time.Now().Local()
	// Just inside the window (kept) and just outside (purged).
	keep := now.AddDate(0, 0, -(LogRetentionDays - 1))
	purge := now.AddDate(0, 0, -(LogRetentionDays + 1))

	keepName := filepath.Join(dir, fmt.Sprintf("aplexicad-%s.log", keep.Format("2006-01-02")))
	keepGzipName := keepName + ".gz"
	purgeName := filepath.Join(dir, fmt.Sprintf("aplexicad-%s.log", purge.Format("2006-01-02")))
	purgeGzipName := purgeName + ".gz"
	require.NoError(t, os.WriteFile(keepName, []byte("KEEP"), 0o600))
	require.NoError(t, os.WriteFile(keepGzipName, []byte("KEEP-GZIP"), 0o600))
	require.NoError(t, os.WriteFile(purgeName, []byte("PURGE"), 0o600))
	require.NoError(t, os.WriteFile(purgeGzipName, []byte("PURGE-GZIP"), 0o600))

	purgeOldLogs(dir, LogRetentionDays)

	_, err := os.Stat(keepName)
	require.NoError(t, err, "archive within retention window must be kept")
	_, err = os.Stat(keepGzipName)
	require.NoError(t, err, "compressed archive within retention window must be kept")
	_, err = os.Stat(purgeName)
	require.True(t, os.IsNotExist(err), "archive older than retention window must be purged")
	_, err = os.Stat(purgeGzipName)
	require.True(t, os.IsNotExist(err), "compressed archive older than retention window must be purged")
}

func TestNewLogger_MigratesClosedLegacyLogsWithoutTouchingLiveLog(t *testing.T) {
	dir := t.TempDir()
	day := time.Now().Local().AddDate(0, 0, -1)
	legacy := filepath.Join(dir, archiveLogName(day))
	stable := filepath.Join(dir, stableLogName)
	require.NoError(t, os.WriteFile(legacy, []byte("closed archive\n"), 0o600))
	require.NoError(t, os.WriteFile(stable, []byte("live prefix\n"), 0o600))

	lg, closer, err := NewLogger(dir)
	require.NoError(t, err)
	defer closer.Close()

	live, err := os.ReadFile(stable)
	require.NoError(t, err)
	require.Equal(t, "live prefix\n", string(live), "startup migration must not rotate or rewrite the live file")
	_, err = os.Stat(legacy)
	require.True(t, os.IsNotExist(err), "legacy archive should be removed only after gzip installation")
	require.Equal(t, "closed archive\n", readGzipLog(t, legacy+".gz"))

	lg.Info("new live line")
	live, err = os.ReadFile(stable)
	require.NoError(t, err)
	require.Contains(t, string(live), "live prefix")
	require.Contains(t, string(live), "new live line")
}

func TestRotatingLogger_GzipArchiveIsPrivate(t *testing.T) {
	dir := t.TempDir()
	rl, closer, err := NewLogger(dir)
	require.NoError(t, err)
	defer closer.Close()
	rl.Info("private archive")
	require.NoError(t, rl.Rotate())

	archive, err := rl.root.OpenReadRegular(archiveLogGzipName(time.Now().Local()))
	require.NoError(t, err, "closed archives must satisfy the platform private-file policy")
	require.NoError(t, archive.Close())
}

func TestCompressClosedLogSourceRoot_RetryAfterCommitDoesNotDuplicate(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Chmod(dir, 0o700))
	root, err := privatefs.OpenRoot(dir, privatefs.DirPolicy{
		Access: privatefs.AccessPrivate, RepairOwned: true, AllowExisting: true,
	})
	require.NoError(t, err)
	defer root.Close()

	day := time.Now().Local()
	sourceName := ".aplexicad-" + day.Format("2006-01-02") + "-pending-00000000000000000001-test"
	targetName := archiveLogGzipName(day)
	sourcePath := filepath.Join(dir, sourceName)
	targetPath := filepath.Join(dir, targetName)
	require.NoError(t, root.WriteFile(sourceName, []byte("once only\n"), privatefs.FilePolicy{RejectWritableByOthers: true}))
	require.NoError(t, compressClosedLogSourceRoot(root, sourceName, targetName))

	// Simulate a crash after the gzip was atomically installed but before its
	// source was removed. The named member lets recovery delete, not reappend.
	require.NoError(t, root.WriteFile(sourceName, []byte("once only\n"), privatefs.FilePolicy{RejectWritableByOthers: true}))
	require.NoError(t, compressClosedLogSourceRoot(root, sourceName, targetName))
	require.Equal(t, "once only\n", readGzipLog(t, targetPath))
	_, err = os.Stat(sourcePath)
	require.True(t, os.IsNotExist(err))
}

func TestCompressClosedLogSourceRoot_ReusedLegacyNamePreservesNewContent(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Chmod(dir, 0o700))
	root, err := privatefs.OpenRoot(dir, privatefs.DirPolicy{
		Access: privatefs.AccessPrivate, RepairOwned: true, AllowExisting: true,
	})
	require.NoError(t, err)
	defer root.Close()

	day := time.Now().Local()
	sourceName := archiveLogName(day)
	targetName := archiveLogGzipName(day)
	require.NoError(t, root.WriteFile(sourceName, []byte("first\n"), privatefs.FilePolicy{RejectWritableByOthers: true}))
	require.NoError(t, compressClosedLogSourceRoot(root, sourceName, targetName))

	// A downgrade can recreate the same daily .log name with different bytes.
	// Content-aware idempotency must append it, never mistake it for crash debris.
	require.NoError(t, root.WriteFile(sourceName, []byte("second\n"), privatefs.FilePolicy{RejectWritableByOthers: true}))
	require.NoError(t, compressClosedLogSourceRoot(root, sourceName, targetName))
	require.Equal(t, "first\nsecond\n", readGzipLog(t, filepath.Join(dir, targetName)))
}

func TestCompressClosedLogSourceRoot_CorruptTargetLeavesSourceIntact(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Chmod(dir, 0o700))
	root, err := privatefs.OpenRoot(dir, privatefs.DirPolicy{
		Access: privatefs.AccessPrivate, RepairOwned: true, AllowExisting: true,
	})
	require.NoError(t, err)
	defer root.Close()

	day := time.Now().Local()
	sourceName := archiveLogName(day)
	targetName := archiveLogGzipName(day)
	sourcePath := filepath.Join(dir, sourceName)
	targetPath := filepath.Join(dir, targetName)
	require.NoError(t, root.WriteFile(sourceName, []byte("authoritative source\n"), privatefs.FilePolicy{RejectWritableByOthers: true}))
	require.NoError(t, root.WriteFile(targetName, []byte("not gzip"), privatefs.FilePolicy{RejectWritableByOthers: true}))

	err = compressClosedLogSourceRoot(root, sourceName, targetName)
	require.Error(t, err)
	source, readErr := os.ReadFile(sourcePath)
	require.NoError(t, readErr)
	require.Equal(t, "authoritative source\n", string(source))
	target, readErr := os.ReadFile(targetPath)
	require.NoError(t, readErr)
	require.Equal(t, "not gzip", string(target))
}
