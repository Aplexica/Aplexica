package secrets

import (
	"compress/gzip"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"sync"
	"time"

	"github.com/aplexica/aplexica/internal/filelock"
	"github.com/aplexica/aplexica/internal/privatefs"
)

const (
	secretAuditLiveName       = "secrets-audit.jsonl"
	secretAuditRotateMaxBytes = int64(8 << 20)
	secretAuditRetention      = 30 * 24 * time.Hour
	secretAuditGzipOSUnknown  = byte(255)
	secretAuditFileMode       = os.FileMode(0o600)
	secretAuditLockName       = ".secrets-audit.lock"
	secretAuditLockTimeout    = 30 * time.Second
	secretAuditCleanupLimit   = 4096
	// Older Windows releases could publish one cumulative gzip snapshot per
	// audit append when removing the open live pathname failed. The upgrade
	// migration is bounded independently from ordinary retention cleanup.
	secretAuditLegacyPrefixCleanupLimit = 16_384
	secretAuditLegacyPrefixMaxLiveBytes = int64(64 << 20)
)

var (
	secretAuditTempName    = regexp.MustCompile(`^\.secrets-audit-[0-9]{1,10}\.jsonl\.gz\.tmp$`)
	secretAuditArchiveName = regexp.MustCompile(
		`^secrets-audit-[0-9]{8}T[0-9]{6}\.[0-9]{9}Z-([0-9a-f]{64})\.jsonl\.gz$`,
	)
)

// The process mutex prevents needless local lock contention. The file lock in
// withSecretAuditLock is the authoritative serialization primitive across the
// daemon, tray, helper, and any overlapping upgrade process.
var secretAuditMu sync.Mutex

type secretAuditEntry struct {
	At     string `json:"at"`
	Op     string `json:"op"`
	Scope  string `json:"scope"`
	Ref    string `json:"ref"`
	Result string `json:"result"`
}

// auditDir returns the directory that holds secrets-audit.jsonl. The secrets
// Root is ~/.aplexica/secrets, so the sibling logs dir is ~/.aplexica/logs.
func (s *Store) auditDir() string {
	return filepath.Join(filepath.Dir(s.Root), "logs")
}

// MaintainAuditLog applies rotation and retention without recording a secret
// operation. The daemon runs this after startup so an oversized audit log from
// an older release is compressed on upgrade even when no secret is accessed.
// Normal audit writes perform the same checks before every append.
func (s *Store) MaintainAuditLog() error {
	if s.Root == "" {
		return nil
	}
	return maintainSecretAuditLog(s.auditDir(), time.Now().UTC())
}

func maintainSecretAuditLog(logsDir string, now time.Time) error {
	return withSecretAuditLock(logsDir, func() error {
		return maintainSecretAuditLogLocked(logsDir, now)
	})
}

func maintainSecretAuditLogLocked(logsDir string, now time.Time) error {
	if err := pruneRedundantLegacySecretAuditPrefixesLocked(logsDir); err != nil {
		return err
	}
	if err := pruneSecretAuditArchivesLocked(logsDir, now); err != nil {
		return err
	}
	livePath := filepath.Join(logsDir, secretAuditLiveName)
	if info, err := lstatSecretAuditRegular(livePath); err == nil {
		if info.Size() >= secretAuditRotateMaxBytes {
			if err := rotateSecretAuditLogLocked(livePath); err != nil {
				return err
			}
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	return nil
}

type legacySecretAuditPrefix struct {
	path   string
	info   os.FileInfo
	bytes  int64
	digest string
}

// pruneRedundantLegacySecretAuditPrefixesLocked reclaims cumulative gzip
// snapshots created by the pre-v1.0.37 Windows rotation bug. It deletes an
// archive only when its content-addressed name is the SHA-256 of an exact
// prefix of the still-authoritative live log with the exact gzip ISIZE. The
// live inode is retained and revalidated until every deletion is durable, so a
// crash at any boundary leaves at least one complete copy. Correct independent
// audit segments never match a live prefix and remain under normal retention.
func pruneRedundantLegacySecretAuditPrefixesLocked(logsDir string) error {
	livePath := filepath.Join(logsDir, secretAuditLiveName)
	liveInfo, err := lstatSecretAuditRegular(livePath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if liveInfo.Size() == 0 || liveInfo.Size() > secretAuditLegacyPrefixMaxLiveBytes {
		return nil
	}

	dir, err := os.Open(logsDir)
	if err != nil {
		return err
	}
	entries, readErr := dir.ReadDir(secretAuditLegacyPrefixCleanupLimit + 1)
	closeErr := dir.Close()
	if readErr != nil && readErr != io.EOF {
		return readErr
	}
	if closeErr != nil {
		return closeErr
	}
	if len(entries) > secretAuditLegacyPrefixCleanupLimit {
		return fmt.Errorf("secrets: legacy audit prefix cleanup exceeds bounded entry limit")
	}

	byLength := make(map[int64][]legacySecretAuditPrefix)
	lengths := make([]int64, 0)
	for _, entry := range entries {
		match := secretAuditArchiveName.FindStringSubmatch(entry.Name())
		if len(match) != 2 {
			continue
		}
		path := filepath.Join(logsDir, entry.Name())
		info, infoErr := lstatSecretAuditRegular(path)
		if infoErr != nil {
			return infoErr
		}
		uncompressed, sizeErr := secretAuditGzipISIZE(path, info)
		if sizeErr != nil || uncompressed <= 0 || uncompressed > liveInfo.Size() {
			continue
		}
		if _, exists := byLength[uncompressed]; !exists {
			lengths = append(lengths, uncompressed)
		}
		byLength[uncompressed] = append(byLength[uncompressed], legacySecretAuditPrefix{
			path: path, info: info, bytes: uncompressed, digest: match[1],
		})
	}
	if len(lengths) == 0 {
		return nil
	}
	sort.Slice(lengths, func(i, j int) bool { return lengths[i] < lengths[j] })

	live, err := os.Open(livePath)
	if err != nil {
		return err
	}
	defer live.Close()
	hash := sha256.New()
	position := int64(0)
	remove := make([]legacySecretAuditPrefix, 0)
	for _, length := range lengths {
		if _, err := io.CopyN(hash, live, length-position); err != nil {
			return fmt.Errorf("secrets: hash legacy audit live prefix: %w", err)
		}
		position = length
		digest := fmt.Sprintf("%x", hash.Sum(nil))
		for _, candidate := range byLength[length] {
			if candidate.digest == digest {
				remove = append(remove, candidate)
			}
		}
	}
	after, err := live.Stat()
	if err != nil || !os.SameFile(liveInfo, after) || after.Size() != liveInfo.Size() || !after.ModTime().Equal(liveInfo.ModTime()) {
		return fmt.Errorf("secrets: audit live file changed during legacy prefix cleanup")
	}
	for _, candidate := range remove {
		current, statErr := lstatSecretAuditRegular(candidate.path)
		if statErr != nil || !os.SameFile(candidate.info, current) || current.Size() != candidate.info.Size() {
			return fmt.Errorf("secrets: audit archive changed during legacy prefix cleanup")
		}
		if err := os.Remove(candidate.path); err != nil {
			return err
		}
	}
	if len(remove) != 0 {
		return syncSecretAuditDirectory(logsDir)
	}
	return nil
}

func secretAuditGzipISIZE(path string, info os.FileInfo) (int64, error) {
	if info == nil || info.Size() < 4 {
		return 0, fmt.Errorf("secrets: audit archive is too short")
	}
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	var trailer [4]byte
	if _, err := f.ReadAt(trailer[:], info.Size()-int64(len(trailer))); err != nil {
		return 0, err
	}
	return int64(binary.LittleEndian.Uint32(trailer[:])), nil
}

// audit appends one record to ~/.aplexica/logs/secrets-audit.jsonl describing a
// secret operation (BRD-09 §4.2: "Every secret-value rotation and every secret
// read/write produces a local audit log entry"). Best-effort: a logging
// failure never blocks or fails the operation. The secret VALUE is NEVER
// recorded — only the op, scope, a non-sensitive reference, and the result.
//
//   - op:    read | write | rotate | delete | sync-toggle
//   - scope: per-artifact | global
//   - ref:   "<artifactId>/<key>" or "<artifactId>" (per-artifact) | "<name>" (global)
func (s *Store) audit(op, scope, ref string, opErr error) {
	if s.Root == "" {
		return
	}
	result := "ok"
	if opErr != nil {
		result = "error"
	}
	now := time.Now().UTC()
	data, err := json.Marshal(secretAuditEntry{
		At:     now.Format(time.RFC3339Nano),
		Op:     op,
		Scope:  scope,
		Ref:    ref,
		Result: result,
	})
	if err != nil {
		return
	}

	_ = appendSecretAuditRecord(s.auditDir(), append(data, '\n'), now)
}

func appendSecretAuditRecord(logsDir string, record []byte, now time.Time) error {
	return withSecretAuditLock(logsDir, func() error {
		return appendSecretAuditRecordLocked(logsDir, record, now)
	})
}

func appendSecretAuditRecordLocked(logsDir string, record []byte, now time.Time) (err error) {
	if err := pruneSecretAuditArchivesLocked(logsDir, now); err != nil {
		return err
	}
	livePath := filepath.Join(logsDir, secretAuditLiveName)
	if info, err := lstatSecretAuditRegular(livePath); err == nil {
		if info.Size() > 0 &&
			(info.Size() >= secretAuditRotateMaxBytes || info.Size()+int64(len(record)) > secretAuditRotateMaxBytes) {
			// Rotation is best-effort independently of the audit record. If gzip
			// or rename fails, preserve the oversized live file and still append
			// this operation below rather than silently dropping its audit entry.
			_ = rotateSecretAuditLogLocked(livePath)
		}
	} else if !os.IsNotExist(err) {
		return err
	}

	root, err := privatefs.OpenRoot(logsDir, privatefs.DirPolicy{
		Access:      privatefs.AccessPrivate,
		RepairOwned: true,
	})
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := root.Close(); err == nil {
			err = closeErr
		}
	}()
	f, err := root.OpenAppendRegularRepair(secretAuditLiveName)
	if err != nil {
		return err
	}
	_, err = f.Write(record)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	return err
}

// rotateSecretAuditLog streams the current live log through gzip BestSpeed to
// a private sibling temp file, fsyncs it, then atomically publishes the closed
// archive before truncating the verified sole-link live inode. A crash can
// leave the live source or a temp file, but can never publish a partial gzip or
// lose the only complete copy. Truncation avoids Windows sharing violations on
// pathname deletion; the content-derived name makes a retry after
// publish-before-truncate reuse the same segment instead of multiplying
// duplicates.
func rotateSecretAuditLog(livePath string) (err error) {
	if filepath.Base(livePath) != secretAuditLiveName {
		return fmt.Errorf("secrets: invalid audit live path")
	}
	return withSecretAuditLock(filepath.Dir(livePath), func() error {
		return rotateSecretAuditLogLocked(livePath)
	})
}

func rotateSecretAuditLogLocked(livePath string) (err error) {
	pathInfo, err := lstatSecretAuditRegular(livePath)
	if err != nil {
		return err
	}
	if pathInfo.Size() == 0 {
		return nil
	}
	source, err := os.Open(livePath)
	if err != nil {
		return err
	}
	sourceInfo, err := source.Stat()
	if err != nil {
		_ = source.Close()
		return err
	}
	if !sourceInfo.Mode().IsRegular() || !os.SameFile(pathInfo, sourceInfo) {
		_ = source.Close()
		return fmt.Errorf("secrets: unsafe audit live file")
	}

	dir := filepath.Dir(livePath)
	temp, err := os.CreateTemp(dir, ".secrets-audit-*.jsonl.gz.tmp")
	if err != nil {
		_ = source.Close()
		return err
	}
	tempPath := temp.Name()
	removeTemp := true
	defer func() {
		_ = source.Close()
		_ = temp.Close()
		if removeTemp {
			_ = os.Remove(tempPath)
		}
	}()
	if err := temp.Chmod(secretAuditFileMode); err != nil {
		return err
	}

	uncompressedHash := sha256.New()
	gzipWriter, err := gzip.NewWriterLevel(temp, gzip.BestSpeed)
	if err != nil {
		return err
	}
	gzipWriter.Header.ModTime = sourceInfo.ModTime().UTC()
	gzipWriter.Header.OS = secretAuditGzipOSUnknown
	written, copyErr := io.Copy(gzipWriter, io.TeeReader(source, uncompressedHash))
	closeGzipErr := gzipWriter.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeGzipErr != nil {
		return closeGzipErr
	}
	afterInfo, err := source.Stat()
	if err != nil {
		return err
	}
	pathInfo, err = lstatSecretAuditRegular(livePath)
	if err != nil {
		return err
	}
	if !os.SameFile(sourceInfo, afterInfo) || !os.SameFile(afterInfo, pathInfo) ||
		sourceInfo.Size() != written || afterInfo.Size() != written || pathInfo.Size() != written {
		return fmt.Errorf("secrets: audit log changed during rotation")
	}
	if err := temp.Sync(); err != nil {
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := source.Close(); err != nil {
		return err
	}

	digest := fmt.Sprintf("%x", uncompressedHash.Sum(nil))
	stamp := sourceInfo.ModTime().UTC().Format("20060102T150405.000000000Z")
	archivePath := filepath.Join(dir, "secrets-audit-"+stamp+"-"+digest+".jsonl.gz")
	if _, statErr := lstatSecretAuditRegular(archivePath); statErr == nil {
		// This is the deterministic retry path after a crash between archive
		// publication and live-file removal. Authenticate the existing archive
		// against the still-authoritative live source before treating it as the
		// committed copy; a corrupt or pre-created deterministic pathname must
		// never authorize deletion of the audit log.
		if err := validateSecretAuditArchive(archivePath, digest, written); err != nil {
			return fmt.Errorf("secrets: validate existing audit archive: %w", err)
		}
		if err := os.Remove(tempPath); err != nil {
			return err
		}
		removeTemp = false
	} else if !os.IsNotExist(statErr) {
		return statErr
	} else {
		if err := os.Rename(tempPath, archivePath); err != nil {
			return err
		}
		removeTemp = false
	}
	if err := os.Chmod(archivePath, secretAuditFileMode); err != nil {
		return err
	}
	if err := os.Chtimes(archivePath, sourceInfo.ModTime(), sourceInfo.ModTime()); err != nil {
		return err
	}
	if err := syncSecretAuditDirectory(dir); err != nil {
		return err
	}

	// Verify that the pathname still identifies exactly the bytes archived, then
	// reopen it through privatefs before truncation. OpenReadWriteRegularRepair
	// rejects links/hardlinks/special files and returns a non-append descriptor;
	// truncating that verified inode avoids a Windows pathname-delete failure
	// creating another full archive on every subsequent audit append.
	currentInfo, err := lstatSecretAuditRegular(livePath)
	if err != nil {
		return err
	}
	if !os.SameFile(sourceInfo, currentInfo) || currentInfo.Size() != written {
		return fmt.Errorf("secrets: audit log changed before rotation commit")
	}
	root, err := privatefs.OpenRoot(dir, privatefs.DirPolicy{Access: privatefs.AccessPrivate, RepairOwned: true})
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := root.Close(); err == nil {
			err = closeErr
		}
	}()
	live, err := root.OpenReadWriteRegularRepair(secretAuditLiveName)
	if err != nil {
		return err
	}
	liveInfo, err := live.Stat()
	if err != nil || !os.SameFile(sourceInfo, liveInfo) || liveInfo.Size() != written {
		_ = live.Close()
		return fmt.Errorf("secrets: audit log changed before rotation truncate")
	}
	if err := live.Truncate(0); err != nil {
		_ = live.Close()
		return err
	}
	if err := live.Sync(); err != nil {
		_ = live.Close()
		return err
	}
	return live.Close()
}

func lstatSecretAuditRegular(path string) (os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("secrets: unsafe non-regular audit path: %s", filepath.Base(path))
	}
	return info, nil
}

func validateSecretAuditArchive(path, wantDigest string, wantBytes int64) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("archive is not a regular file")
	}
	zr, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	h := sha256.New()
	written, copyErr := io.Copy(h, zr)
	closeErr := zr.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if written != wantBytes || fmt.Sprintf("%x", h.Sum(nil)) != wantDigest {
		return fmt.Errorf("archive content does not match live audit log")
	}
	return nil
}

func syncSecretAuditDirectory(path string) error {
	// Windows does not provide fsync semantics for directory handles. The
	// private, fully-synced temp plus atomic rename remains the supported commit
	// primitive there; Unix additionally persists the directory entry ordering.
	if runtime.GOOS == "windows" {
		return nil
	}
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	if err := dir.Sync(); err != nil {
		_ = dir.Close()
		return err
	}
	return dir.Close()
}

func pruneSecretAuditArchives(logsDir string, now time.Time) error {
	return withSecretAuditLock(logsDir, func() error {
		return pruneSecretAuditArchivesLocked(logsDir, now)
	})
}

func pruneSecretAuditArchivesLocked(logsDir string, now time.Time) error {
	dir, err := os.Open(logsDir)
	if err != nil {
		return err
	}
	entries, readErr := dir.ReadDir(secretAuditCleanupLimit)
	closeErr := dir.Close()
	if readErr != nil && readErr != io.EOF {
		return readErr
	}
	if closeErr != nil {
		return closeErr
	}
	cutoff := now.UTC().Add(-secretAuditRetention)
	remove := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		archive := secretAuditArchiveName.MatchString(name)
		crashTemp := secretAuditTempName.MatchString(name)
		if !archive && !crashTemp {
			continue
		}
		path := filepath.Join(logsDir, name)
		info, infoErr := lstatSecretAuditRegular(path)
		if infoErr != nil {
			return infoErr
		}
		if crashTemp || !info.ModTime().UTC().After(cutoff) {
			remove = append(remove, path)
		}
	}
	for _, path := range remove {
		if err := os.Remove(path); err != nil {
			return err
		}
	}
	if len(remove) > 0 {
		return syncSecretAuditDirectory(logsDir)
	}
	return nil
}

func withSecretAuditLock(logsDir string, fn func() error) (err error) {
	secretAuditMu.Lock()
	defer secretAuditMu.Unlock()

	if err := privatefs.EnsureDir(logsDir, privatefs.DirPolicy{
		Access:        privatefs.AccessPrivate,
		RepairOwned:   true,
		AllowExisting: true,
	}); err != nil {
		return err
	}
	lock, err := filelock.Acquire(filepath.Join(logsDir, secretAuditLockName), secretAuditLockTimeout)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := lock.Close(); err == nil {
			err = closeErr
		}
	}()
	return fn()
}
