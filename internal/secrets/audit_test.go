package secrets

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestStore_AuditLog: BRD-09 §4.2 requires every secret read/write/rotation/
// delete to produce a local audit entry in ~/.aplexica/logs/secrets-audit.jsonl
// — and the entry must NEVER contain the secret value.
func TestStore_AuditLog(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "secrets")
	s := &Store{Root: root}
	require.NoError(t, s.Init())

	const perArtifactValue = "super-secret-per-artifact-value"
	const globalValue = "another-super-secret-global-value"

	require.NoError(t, s.Put("art1", "TOKEN", perArtifactValue))
	_, err := s.Get("art1", "TOKEN")
	require.NoError(t, err)
	require.NoError(t, s.PutGlobal("API_KEY", globalValue))
	require.NoError(t, s.RotateGlobal("API_KEY", globalValue+"-v2"))
	require.NoError(t, s.DeleteGlobal("API_KEY"))

	auditPath := filepath.Join(tmp, "logs", "secrets-audit.jsonl")
	data, err := os.ReadFile(auditPath)
	require.NoError(t, err, "BRD-09 §4.2: secret ops must be audited to secrets-audit.jsonl")
	logStr := string(data)

	require.Contains(t, logStr, `"op":"write"`)
	require.Contains(t, logStr, `"op":"read"`)
	require.Contains(t, logStr, `"op":"rotate"`)
	require.Contains(t, logStr, `"op":"delete"`)

	// CRITICAL: the secret values must never be written to the audit log.
	require.NotContains(t, logStr, perArtifactValue)
	require.NotContains(t, logStr, globalValue)
	require.NotContains(t, logStr, "-v2")

	scanner := bufio.NewScanner(strings.NewReader(logStr))
	entries := 0
	for scanner.Scan() {
		var fields map[string]any
		require.NoError(t, json.Unmarshal(scanner.Bytes(), &fields))
		require.Len(t, fields, 5, "audit records must remain content-free metadata only")
		for _, key := range []string{"at", "op", "scope", "ref", "result"} {
			require.Contains(t, fields, key)
		}
		entries++
	}
	require.NoError(t, scanner.Err())
	require.Equal(t, 5, entries, "each secret operation must append exactly one audit record")
}

func TestStore_AuditLog_RotatesExistingLargeLiveFileCompressedOnNextWrite(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "secrets")
	logsDir := filepath.Join(tmp, "logs")
	require.NoError(t, os.MkdirAll(logsDir, 0o700))
	livePath := filepath.Join(logsDir, secretAuditLiveName)
	const legacyAuditBytes = int64(111 << 20)
	f, err := os.OpenFile(livePath, os.O_CREATE|os.O_WRONLY, 0o644)
	require.NoError(t, err)
	require.NoError(t, f.Truncate(legacyAuditBytes), "create the oversized live audit fixture")
	require.NoError(t, f.Close())

	s := &Store{Root: root}
	s.audit("read", "global", "API_KEY", nil)

	archives, err := filepath.Glob(filepath.Join(logsDir, "secrets-audit-*.jsonl.gz"))
	require.NoError(t, err)
	require.Len(t, archives, 1, "the oversized live log must close into one gzip segment on the next audit write")
	archiveInfo, err := os.Stat(archives[0])
	require.NoError(t, err)
	if runtime.GOOS != "windows" {
		require.Equal(t, os.FileMode(0o600), archiveInfo.Mode().Perm())
	}
	archive, err := os.Open(archives[0])
	require.NoError(t, err)
	gzipReader, err := gzip.NewReader(archive)
	require.NoError(t, err)
	uncompressedBytes, err := io.Copy(io.Discard, gzipReader)
	require.NoError(t, err)
	require.NoError(t, gzipReader.Close())
	require.NoError(t, archive.Close())
	require.Equal(t, legacyAuditBytes, uncompressedBytes)
	require.Less(t, archiveInfo.Size(), legacyAuditBytes/100,
		"the closed zero-filled regression fixture should demonstrate actual compression")

	live, err := os.ReadFile(livePath)
	require.NoError(t, err)
	liveInfo, err := os.Stat(livePath)
	require.NoError(t, err)
	if runtime.GOOS != "windows" {
		require.Equal(t, os.FileMode(0o600), liveInfo.Mode().Perm())
	}
	var entry secretAuditEntry
	require.NoError(t, json.Unmarshal(live, &entry))
	require.Equal(t, "read", entry.Op)
	require.Equal(t, "API_KEY", entry.Ref)
	require.NotContains(t, string(live), "secret-value")
}

func TestStore_AuditLog_RotationIsConcurrencySafe(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "secrets")
	logsDir := filepath.Join(tmp, "logs")
	require.NoError(t, os.MkdirAll(logsDir, 0o700))
	livePath := filepath.Join(logsDir, secretAuditLiveName)
	f, err := os.OpenFile(livePath, os.O_CREATE|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	require.NoError(t, f.Truncate(secretAuditRotateMaxBytes))
	require.NoError(t, f.Close())

	const writers = 128
	s := &Store{Root: root}
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.audit("read", "global", "concurrent-key", nil)
		}()
	}
	wg.Wait()

	archives, err := filepath.Glob(filepath.Join(logsDir, "secrets-audit-*.jsonl.gz"))
	require.NoError(t, err)
	require.Len(t, archives, 1, "only one goroutine may rotate the shared live segment")
	live, err := os.Open(livePath)
	require.NoError(t, err)
	defer live.Close()
	scanner := bufio.NewScanner(live)
	count := 0
	for scanner.Scan() {
		var entry secretAuditEntry
		require.NoError(t, json.Unmarshal(scanner.Bytes(), &entry))
		require.Equal(t, "concurrent-key", entry.Ref)
		count++
	}
	require.NoError(t, scanner.Err())
	require.Equal(t, writers, count, "every concurrent operation must append exactly one complete JSONL record")
}

func TestStore_AuditLog_RotationIsCrossProcessSafe(t *testing.T) {
	tmp := t.TempDir()
	logsDir := filepath.Join(tmp, "logs")
	require.NoError(t, os.MkdirAll(logsDir, 0o700))
	livePath := filepath.Join(logsDir, secretAuditLiveName)
	f, err := os.OpenFile(livePath, os.O_CREATE|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	require.NoError(t, f.Truncate(secretAuditRotateMaxBytes))
	require.NoError(t, f.Close())

	const (
		processes        = 2
		writesPerProcess = 16
	)
	commands := make([]*exec.Cmd, 0, processes)
	outputs := make([]bytes.Buffer, processes)
	for i := 0; i < processes; i++ {
		cmd := exec.Command(os.Args[0], "-test.run=^TestSecretAuditAppendProcess$")
		cmd.Env = append(os.Environ(),
			"APLEXICA_TEST_SECRET_AUDIT_DIR="+logsDir,
			fmt.Sprintf("APLEXICA_TEST_SECRET_AUDIT_PROCESS=%d", i),
		)
		cmd.Stdout = &outputs[i]
		cmd.Stderr = &outputs[i]
		require.NoError(t, cmd.Start())
		commands = append(commands, cmd)
	}
	for i, cmd := range commands {
		require.NoErrorf(t, cmd.Wait(), "audit helper %d failed: %s", i, outputs[i].String())
	}

	archives, err := filepath.Glob(filepath.Join(logsDir, "secrets-audit-*.jsonl.gz"))
	require.NoError(t, err)
	require.Len(t, archives, 1, "only one process may rotate the shared live segment")
	live, err := os.Open(livePath)
	require.NoError(t, err)
	defer live.Close()
	scanner := bufio.NewScanner(live)
	count := 0
	for scanner.Scan() {
		var entry secretAuditEntry
		require.NoError(t, json.Unmarshal(scanner.Bytes(), &entry))
		require.Contains(t, entry.Ref, "process-")
		count++
	}
	require.NoError(t, scanner.Err())
	require.Equal(t, processes*writesPerProcess, count)
}

func TestSecretAuditAppendProcess(t *testing.T) {
	logsDir := os.Getenv("APLEXICA_TEST_SECRET_AUDIT_DIR")
	if logsDir == "" {
		return
	}
	process := os.Getenv("APLEXICA_TEST_SECRET_AUDIT_PROCESS")
	for i := 0; i < 16; i++ {
		record := []byte(fmt.Sprintf(
			`{"at":"2026-07-19T00:00:00Z","op":"read","scope":"global","ref":"process-%s-%d","result":"ok"}`+"\n",
			process, i,
		))
		require.NoError(t, appendSecretAuditRecord(logsDir, record, time.Now().UTC()))
	}
}

func TestStore_MaintainAuditLogRotatesOversizedLegacyLogWithoutAuditWrite(t *testing.T) {
	tmp := t.TempDir()
	logsDir := filepath.Join(tmp, "logs")
	require.NoError(t, os.MkdirAll(logsDir, 0o700))
	livePath := filepath.Join(logsDir, secretAuditLiveName)
	f, err := os.OpenFile(livePath, os.O_CREATE|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	require.NoError(t, f.Truncate(secretAuditRotateMaxBytes))
	require.NoError(t, f.Close())

	s := &Store{Root: filepath.Join(tmp, "secrets")}
	require.NoError(t, s.MaintainAuditLog())
	live, err := os.ReadFile(livePath)
	require.NoError(t, err)
	require.Empty(t, live, "maintenance must not append a synthetic audit record")
	archives, err := filepath.Glob(filepath.Join(logsDir, "secrets-audit-*.jsonl.gz"))
	require.NoError(t, err)
	require.Len(t, archives, 1)
	archive, err := os.Open(archives[0])
	require.NoError(t, err)
	defer archive.Close()
	zr, err := gzip.NewReader(archive)
	require.NoError(t, err)
	n, err := io.Copy(io.Discard, zr)
	require.NoError(t, err)
	require.NoError(t, zr.Close())
	require.Equal(t, secretAuditRotateMaxBytes, n)
}

func writeSecretAuditArchiveFixture(t *testing.T, logsDir string, at time.Time, content []byte) string {
	t.Helper()
	digest := sha256.Sum256(content)
	name := "secrets-audit-" + at.UTC().Format("20060102T150405.000000000Z") + "-" + fmt.Sprintf("%x", digest[:]) + ".jsonl.gz"
	path := filepath.Join(logsDir, name)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	zw := gzip.NewWriter(f)
	_, err = zw.Write(content)
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	require.NoError(t, f.Close())
	return path
}

func TestStore_MaintainAuditLogPrunesOnlyContentAddressedLivePrefixSnapshots(t *testing.T) {
	logsDir := t.TempDir()
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	first := []byte(`{"at":"2026-07-19T11:59:00Z","op":"read","scope":"global","ref":"one","result":"ok"}` + "\n")
	second := []byte(`{"at":"2026-07-19T11:59:01Z","op":"read","scope":"global","ref":"two","result":"ok"}` + "\n")
	liveContent := append(append([]byte(nil), first...), second...)
	livePath := filepath.Join(logsDir, secretAuditLiveName)
	require.NoError(t, os.WriteFile(livePath, liveContent, 0o600))
	prefixOne := writeSecretAuditArchiveFixture(t, logsDir, now.Add(-3*time.Second), first)
	prefixTwo := writeSecretAuditArchiveFixture(t, logsDir, now.Add(-2*time.Second), liveContent)
	independentContent := []byte(`{"at":"2026-07-18T00:00:00Z","op":"rotate","scope":"global","ref":"independent","result":"ok"}` + "\n")
	independent := writeSecretAuditArchiveFixture(t, logsDir, now.Add(-24*time.Hour), independentContent)

	require.NoError(t, maintainSecretAuditLog(logsDir, now))
	for _, redundant := range []string{prefixOne, prefixTwo} {
		_, err := os.Stat(redundant)
		require.ErrorIs(t, err, os.ErrNotExist)
	}
	_, err := os.Stat(independent)
	require.NoError(t, err, "an independent audit segment must remain")
	got, err := os.ReadFile(livePath)
	require.NoError(t, err)
	require.Equal(t, liveContent, got, "legacy reclamation must preserve the authoritative live log")
}

func TestStore_MaintainAuditLogKeepsArchiveWhoseNameDoesNotCommitItsLivePrefix(t *testing.T) {
	logsDir := t.TempDir()
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	live := []byte("authoritative live audit\n")
	require.NoError(t, os.WriteFile(filepath.Join(logsDir, secretAuditLiveName), live, 0o600))
	archive := writeSecretAuditArchiveFixture(t, logsDir, now.Add(-time.Second), []byte("different same-length data!\n"))

	require.NoError(t, maintainSecretAuditLog(logsDir, now))
	_, err := os.Stat(archive)
	require.NoError(t, err, "same-size non-prefix archive must not be reclaimed")
}

func TestStore_AuditLogRefusesCorruptExistingDeterministicArchive(t *testing.T) {
	tmp := t.TempDir()
	logsDir := filepath.Join(tmp, "logs")
	require.NoError(t, os.MkdirAll(logsDir, 0o700))
	livePath := filepath.Join(logsDir, secretAuditLiveName)
	content := []byte("authoritative audit rows\n")
	require.NoError(t, os.WriteFile(livePath, content, 0o600))
	info, err := os.Stat(livePath)
	require.NoError(t, err)
	digest := fmt.Sprintf("%x", sha256.Sum256(content))
	archivePath := filepath.Join(logsDir, "secrets-audit-"+
		info.ModTime().UTC().Format("20060102T150405.000000000Z")+"-"+digest+".jsonl.gz")
	require.NoError(t, os.WriteFile(archivePath, []byte("not gzip"), 0o600))

	err = rotateSecretAuditLog(livePath)
	require.Error(t, err)
	got, readErr := os.ReadFile(livePath)
	require.NoError(t, readErr, "the live audit source must remain authoritative")
	require.Equal(t, content, got)
}

func TestStore_AuditLogRotationRejectsHardlinkWithoutTruncatingOrMultiplyingArchives(t *testing.T) {
	tmp := t.TempDir()
	logsDir := filepath.Join(tmp, "logs")
	require.NoError(t, os.MkdirAll(logsDir, 0o700))
	livePath := filepath.Join(logsDir, secretAuditLiveName)
	content := []byte("authoritative audit rows must survive\n")
	require.NoError(t, os.WriteFile(livePath, content, 0o600))
	alias := filepath.Join(tmp, "unrelated-user-file")
	if err := os.Link(livePath, alias); err != nil {
		t.Skipf("hardlinks unavailable: %v", err)
	}

	for i := 0; i < 2; i++ {
		err := rotateSecretAuditLog(livePath)
		require.Error(t, err)
		for _, path := range []string{livePath, alias} {
			got, readErr := os.ReadFile(path)
			require.NoError(t, readErr)
			require.Equal(t, content, got, "a linked user file must never be truncated during space reclamation")
		}
	}
	archives, err := filepath.Glob(filepath.Join(logsDir, "secrets-audit-*.jsonl.gz"))
	require.NoError(t, err)
	require.Len(t, archives, 1,
		"a retry after a safe truncation refusal must reuse the deterministic archive")
}

func TestStore_AuditLog_PrunesOnlySegmentsOlderThanThirtyDays(t *testing.T) {
	tmp := t.TempDir()
	logsDir := filepath.Join(tmp, "logs")
	require.NoError(t, os.MkdirAll(logsDir, 0o700))
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	oldArchive := filepath.Join(logsDir, "secrets-audit-20260617T120000.000000000Z-"+strings.Repeat("a", 64)+".jsonl.gz")
	recentArchive := filepath.Join(logsDir, "secrets-audit-20260619T120000.000000000Z-"+strings.Repeat("b", 64)+".jsonl.gz")
	crashTemp := filepath.Join(logsDir, ".secrets-audit-1234567890.jsonl.gz.tmp")
	nearMiss := filepath.Join(logsDir, ".secrets-audit-stale.jsonl.gz.tmp")
	for _, path := range []string{oldArchive, recentArchive, crashTemp, nearMiss} {
		require.NoError(t, os.WriteFile(path, []byte("fixture"), 0o600))
	}
	require.NoError(t, os.Chtimes(oldArchive, now.Add(-31*24*time.Hour), now.Add(-31*24*time.Hour)))
	require.NoError(t, os.Chtimes(recentArchive, now.Add(-29*24*time.Hour), now.Add(-29*24*time.Hour)))
	record := []byte(`{"at":"2026-07-18T12:00:00Z","op":"read","scope":"global","ref":"key","result":"ok"}` + "\n")
	require.NoError(t, appendSecretAuditRecord(logsDir, record, now))

	_, err := os.Stat(oldArchive)
	require.ErrorIs(t, err, os.ErrNotExist)
	_, err = os.Stat(crashTemp)
	require.ErrorIs(t, err, os.ErrNotExist, "an exact regular crash temp is never authoritative and must be reclaimed immediately")
	_, err = os.Stat(recentArchive)
	require.NoError(t, err, "a segment younger than 30 days must be retained")
	_, err = os.Stat(nearMiss)
	require.NoError(t, err, "cleanup must not broaden the exact crash-temp pattern")
	_, err = os.Stat(filepath.Join(logsDir, secretAuditLiveName))
	require.NoError(t, err)
}

func TestStore_AuditLogCleanupRejectsMatchingSymlinkWithoutDeletingAnything(t *testing.T) {
	logsDir := t.TempDir()
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	safeTemp := filepath.Join(logsDir, ".secrets-audit-1.jsonl.gz.tmp")
	unsafeTemp := filepath.Join(logsDir, ".secrets-audit-2.jsonl.gz.tmp")
	target := filepath.Join(logsDir, "target")
	require.NoError(t, os.WriteFile(safeTemp, []byte("safe crash temp"), 0o600))
	require.NoError(t, os.WriteFile(target, []byte("must remain"), 0o600))
	if err := os.Symlink(target, unsafeTemp); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	err := pruneSecretAuditArchives(logsDir, now)
	require.Error(t, err)
	info, lstatErr := os.Lstat(unsafeTemp)
	require.NoError(t, lstatErr)
	require.NotZero(t, info.Mode()&os.ModeSymlink)
	_, err = os.Stat(safeTemp)
	require.NoError(t, err, "cleanup must validate the bounded batch before deleting any candidate")
	data, err := os.ReadFile(target)
	require.NoError(t, err)
	require.Equal(t, []byte("must remain"), data)
}

func TestStore_AuditLogCleanupRejectsMatchingNonRegularArchive(t *testing.T) {
	logsDir := t.TempDir()
	archive := filepath.Join(logsDir,
		"secrets-audit-20260617T120000.000000000Z-"+strings.Repeat("c", 64)+".jsonl.gz")
	require.NoError(t, os.Mkdir(archive, 0o700))

	err := pruneSecretAuditArchives(logsDir, time.Now().UTC())
	require.Error(t, err)
	info, statErr := os.Stat(archive)
	require.NoError(t, statErr)
	require.True(t, info.IsDir(), "a matching non-regular archive must be retained and rejected")
}

func TestStore_AuditLogCleanupIsBoundedPerPass(t *testing.T) {
	logsDir := t.TempDir()
	for i := 0; i < secretAuditCleanupLimit+1; i++ {
		name := fmt.Sprintf(".secrets-audit-%010d.jsonl.gz.tmp", i)
		require.NoError(t, os.WriteFile(filepath.Join(logsDir, name), []byte("temp"), 0o600))
	}

	require.NoError(t, pruneSecretAuditArchivesLocked(logsDir, time.Now().UTC()))
	entries, err := os.ReadDir(logsDir)
	require.NoError(t, err)
	require.Len(t, entries, 1, "one pass must inspect and delete at most the explicit cleanup bound")
	require.True(t, secretAuditTempName.MatchString(entries[0].Name()))
	require.NoError(t, pruneSecretAuditArchivesLocked(logsDir, time.Now().UTC()))
	entries, err = os.ReadDir(logsDir)
	require.NoError(t, err)
	require.Empty(t, entries)
}
