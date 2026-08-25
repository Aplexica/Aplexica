package daemon

import (
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aplexica/aplexica/internal/privatefs"
)

// LogRetentionDays is how many days of aplexicad-*.log.gz (and legacy
// aplexicad-*.log) archive files are kept. Archives older than this are deleted
// by NewLogger on each call and by (*RotatingLogger).Rotate on each rotation.
const LogRetentionDays = 30

// stableLogName is the fixed filename of the live daemon log. It never
// carries a date — the readers ("aplexica daemon logs", "aplexica doctor
// --log") open this exact path. Rotation stages it under a private name,
// reopens a fresh copy, then installs a dated gzip archive.
const stableLogName = "aplexicad.log"

// RotatingLogger wraps a slog.Logger whose underlying file can be
// re-opened via Rotate() in response to e.g. SIGHUP or the daily
// midnight scheduler. The slog.Handler is re-created on each rotation
// because Go's slog.JSONHandler binds to a single io.Writer at
// construction time.
//
// The embedded *slog.Logger promotes the standard Info/Warn/Error/Debug
// (and With/WithGroup/Log/LogAttrs) methods, so RotatingLogger is a
// drop-in replacement for *slog.Logger at call sites that only use
// those.
//
// The level field is a slog.LevelVar so SetLevel can swap the log
// threshold live (used by SIGHUP config reload — see ApplyReload). It is
// preserved across Rotate() calls because openCurrent passes &r.level by
// pointer to the new handler.
//
// openDay records the local calendar day the currently-open stable file
// began covering. Rotate stamps the archive with that day so a file that
// rolls over at midnight is named for the day it actually covered, not
// the day the rotation runs.
type RotatingLogger struct {
	mu      sync.Mutex
	dir     string
	file    *os.File
	openDay time.Time
	level   slog.LevelVar
	*slog.Logger
	root *privatefs.Root
}
type LogDirOptions struct {
	Path           string
	IsDefaultOwned bool
}

// archiveLogName returns the legacy uncompressed dated archive filename for a
// given day, e.g. aplexicad-2026-06-21.log. It remains the compatibility name
// recognized during startup migration and retention.
func archiveLogName(day time.Time) string {
	return fmt.Sprintf("aplexicad-%s.log", day.Format("2006-01-02"))
}

// archiveLogGzipName is the durable archive name used by new rotations.
func archiveLogGzipName(day time.Time) string {
	return archiveLogName(day) + ".gz"
}

const (
	logGzipTempPrefix = ".aplexica-log-gzip-"
	logPendingPrefix  = ".aplexicad-"
	logPendingMarker  = "-pending-"
)

// NewLogger opens (or creates) the stable JSON-line log file
// aplexicad.log under dir and returns a *RotatingLogger writing to it.
// The returned io.Closer flushes and closes the underlying file when
// called; the caller must close it during daemon shutdown.
//
// The live file always has the fixed name aplexicad.log (FR-10.3 /
// stable-file scheme); the readers open that exact path. Daily rotation
// at local midnight writes an aplexicad-YYYY-MM-DD.log.gz archive and reopens
// a fresh aplexicad.log — see (*RotatingLogger).Rotate and
// StartMidnightRotation. Closed legacy .log archives are migrated to .log.gz
// at startup. Archives older than LogRetentionDays (30 days, local-zone date)
// are purged at startup on a best-effort basis — migration/purge failures don't
// block opening the live log file and leave the uncompressed source intact.
func NewLogger(dir string) (*RotatingLogger, io.Closer, error) {
	return NewLoggerWithOptions(LogDirOptions{Path: dir, IsDefaultOwned: true})
}
func NewLoggerWithOptions(opts LogDirOptions) (*RotatingLogger, io.Closer, error) {
	dir := opts.Path
	if err := privatefs.EnsureDir(dir, privatefs.DirPolicy{Access: privatefs.AccessPrivate, RepairOwned: opts.IsDefaultOwned, AllowExisting: opts.IsDefaultOwned}); err != nil {
		return nil, nil, fmt.Errorf("daemon: secure log dir: %w", err)
	}
	root, err := privatefs.OpenRoot(dir, privatefs.DirPolicy{Access: privatefs.AccessPrivate, RepairOwned: opts.IsDefaultOwned, AllowExisting: true})
	if err != nil {
		return nil, nil, err
	}
	if opts.IsDefaultOwned {
		if err := root.HardenPrivateTree(); err != nil {
			root.Close()
			return nil, nil, err
		}
	}

	// Purge archives older than LogRetentionDays. Best-effort — errors here
	// are silently ignored so we never block opening the log file.
	purgeOldLogsRoot(root, LogRetentionDays)
	// Compress every closed legacy archive, including a pending file left by an
	// interrupted rotation. The live stable file is deliberately not a candidate.
	// Each source remains authoritative until its gzip replacement is synced and
	// atomically installed, so startup can safely retry after a crash.
	_ = compressClosedLogArchivesRoot(root)
	// A very old pending rotation becomes a dated archive only during migration;
	// run retention again so it is not kept for one extra daemon lifetime.
	purgeOldLogsRoot(root, LogRetentionDays)

	rl := &RotatingLogger{dir: dir, root: root}
	if err := rl.openCurrent(); err != nil {
		root.Close()
		return nil, nil, err
	}
	return rl, rl, nil
}

// Rotate archives the current stable log file under a local-dated .log.gz name,
// runs the 30-day retention purge, and reopens a fresh aplexicad.log.
// Safe to call concurrently with logging — logging goroutines see the
// new handler after Rotate returns; in-flight writes to the old handler
// land in the old (now closed) file (slog absorbs the resulting EBADF).
//
// The archive name uses the local calendar day the rotated file covered:
// the recorded open day if known, otherwise the previous local day. This
// keeps a file that rolls over at midnight stamped for the day it
// actually covered. The closed file first moves to a private, unique pending
// name and the new live file is opened before compression. Compression writes
// and fsyncs a private temporary file, atomically installs the .log.gz, and only
// then removes the pending source. A crash therefore leaves either the source,
// the durable gzip, or both; startup migration safely finishes either state.
//
// Use this from the midnight scheduler, a SIGHUP handler, or after
// external `mv` rotation.
func (r *RotatingLogger) Rotate() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.file != nil {
		_ = r.file.Close()
		r.file = nil
	}

	day := r.openDay
	if day.IsZero() {
		// Unknown open day (e.g. external mv): attribute to the previous
		// local day, since rotation runs at/after the covered boundary.
		day = time.Now().Local().AddDate(0, 0, -1)
	}
	pending, stageErr := stageCurrentLogRoot(r.root, day)
	if stageErr != nil {
		// The stable path was not moved, so reopening it resumes append mode.
		return errors.Join(stageErr, r.openCurrent())
	}

	// Restore the live logger before doing potentially substantial compression.
	// The unique pending source is closed and remains recoverable on any error.
	openErr := r.openCurrent()
	purgeOldLogsRoot(r.root, LogRetentionDays)
	compressErr := compressClosedLogArchivesRoot(r.root)
	purgeOldLogsRoot(r.root, LogRetentionDays)
	if pending != "" && compressErr != nil {
		compressErr = fmt.Errorf("daemon: compress rotated log %s: %w", pending, compressErr)
	}
	return errors.Join(openErr, compressErr)
}

// Close implements io.Closer. Idempotent — second Close is a no-op.
func (r *RotatingLogger) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.file == nil {
		return nil
	}
	err := r.file.Close()
	r.file = nil
	if e := r.root.Close(); err == nil {
		err = e
	}
	return err
}

// SetLevel updates the live slog handler level. Safe to call from any
// goroutine. Used by the SIGHUP config reload path (see reload.go) to
// honor a changed LogLevel without restarting the daemon. The level
// applies to the current handler AND any subsequent handler created by
// Rotate (openCurrent passes &r.level to the new handler).
func (r *RotatingLogger) SetLevel(lvl slog.Level) {
	r.level.Set(lvl)
}

// openCurrent opens the stable aplexicad.log file and rebuilds the
// embedded *slog.Logger to point at it. Caller MUST hold r.mu. The open
// day is recorded (local) so a later Rotate can stamp the archive for the
// day this file covered.
//
// The handler's level is bound to &r.level (a slog.LevelVar) so
// SetLevel takes effect immediately on this handler and on any handler
// created by a subsequent Rotate(). The level defaults to slog.LevelInfo
// (the zero value of slog.LevelVar).
func (r *RotatingLogger) openCurrent() error {
	f, err := r.root.OpenAppendRegular(stableLogName)
	if err != nil {
		return fmt.Errorf("daemon: open log file: %w", err)
	}
	r.file = f
	r.openDay = time.Now().Local()
	handler := slog.NewJSONHandler(f, &slog.HandlerOptions{
		Level: &r.level,
	})
	r.Logger = slog.New(handler)
	return nil
}

// stageCurrentLogRoot moves the closed stable log to a unique private pending
// name. A placeholder allocated through privatefs makes the destination
// collision-free without escaping the retained root. Empty logs are still
// staged; the compression pass removes them without creating an empty archive.
func stageCurrentLogRoot(root *privatefs.Root, day time.Time) (string, error) {
	existing, err := root.OpenReadRegular(stableLogName)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("daemon: inspect live log for rotation: %w", err)
	}
	if err := existing.Close(); err != nil {
		return "", fmt.Errorf("daemon: close live log inspection handle: %w", err)
	}

	pattern := fmt.Sprintf(
		"%s%s%s%020d-",
		logPendingPrefix,
		day.Format("2006-01-02"),
		logPendingMarker,
		time.Now().UnixNano(),
	)
	placeholder, pending, err := root.CreateTemp(".", pattern)
	if err != nil {
		return "", fmt.Errorf("daemon: allocate pending log archive: %w", err)
	}
	if err := placeholder.Close(); err != nil {
		_ = root.RemoveRegular(pending)
		return "", fmt.Errorf("daemon: close pending log placeholder: %w", err)
	}
	if err := root.Rename(stableLogName, pending); err != nil {
		_ = root.RemoveRegular(pending)
		return "", fmt.Errorf("daemon: stage live log for compression: %w", err)
	}
	return pending, nil
}

type closedLogSource struct {
	name   string
	target string
	legacy bool
}

// compressClosedLogArchivesRoot migrates all closed dated .log archives and
// resumes unique pending rotations. It intentionally ignores aplexicad.log.
// Sources are ordered per day so a legacy archive precedes newer pending
// rotations; gzip concatenated members preserve that daily order while keeping
// repeat rotations cheap (the existing compressed bytes are copied verbatim).
func compressClosedLogArchivesRoot(root *privatefs.Root) error {
	entries, err := root.ReadDir(".")
	if err != nil {
		return err
	}
	var sources []closedLogSource
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasPrefix(name, logGzipTempPrefix) {
			// A gzip temp is never authoritative: its source is removed only
			// after the final archive has been synced and installed.
			_ = root.RemoveRegular(name)
			continue
		}
		if day, ok := legacyArchiveDay(name); ok {
			sources = append(sources, closedLogSource{
				name: name, target: archiveLogGzipName(day), legacy: true,
			})
			continue
		}
		if day, ok := pendingArchiveDay(name); ok {
			sources = append(sources, closedLogSource{
				name: name, target: archiveLogGzipName(day),
			})
		}
	}
	sort.Slice(sources, func(i, j int) bool {
		if sources[i].target != sources[j].target {
			return sources[i].target < sources[j].target
		}
		if sources[i].legacy != sources[j].legacy {
			return sources[i].legacy
		}
		return sources[i].name < sources[j].name
	})

	var result error
	for _, source := range sources {
		if err := compressClosedLogSourceRoot(root, source.name, source.target); err != nil {
			result = errors.Join(result, fmt.Errorf("%s: %w", source.name, err))
		}
	}
	return result
}

// compressClosedLogSourceRoot appends source as one named gzip member to the
// day's archive. The target is built privately, fsynced, and atomically
// replaced before source is removed. The member name is an idempotency marker:
// after a crash between replacement and source removal, startup recognizes the
// already-committed member and removes the duplicate source without appending
// it again.
func compressClosedLogSourceRoot(root *privatefs.Root, sourceName, targetName string) error {
	source, err := root.OpenReadRegular(sourceName)
	if err != nil {
		return err
	}
	defer source.Close()
	sourceInfo, err := source.Stat()
	if err != nil {
		return err
	}
	if sourceInfo.Size() == 0 {
		if err := source.Close(); err != nil {
			return err
		}
		return root.RemoveRegular(sourceName)
	}
	sourceDigest, err := digestLogSource(source)
	if err != nil {
		return err
	}
	sourceID := gzipMemberID{name: sourceName, digest: sourceDigest}

	var existing *os.File
	existing, err = root.OpenReadRegular(targetName)
	if err == nil {
		memberIDs, validateErr := gzipMemberIDs(existing)
		err = validateErr
		if err != nil {
			_ = existing.Close()
			return fmt.Errorf("validate existing gzip archive: %w", err)
		}
		if _, committed := memberIDs[sourceID]; committed {
			if err := existing.Close(); err != nil {
				return err
			}
			if err := source.Close(); err != nil {
				return err
			}
			return root.RemoveRegular(sourceName)
		}
		if _, err := existing.Seek(0, io.SeekStart); err != nil {
			_ = existing.Close()
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	} else {
		existing = nil
	}

	temp, tempName, err := root.CreateTemp(".", logGzipTempPrefix)
	if err != nil {
		if existing != nil {
			_ = existing.Close()
		}
		return err
	}
	installed := false
	defer func() {
		_ = temp.Close()
		if !installed {
			_ = root.RemoveRegular(tempName)
		}
	}()

	if existing != nil {
		_, err = io.Copy(temp, existing)
		closeErr := existing.Close()
		if err == nil {
			err = closeErr
		}
		if err != nil {
			return err
		}
	}
	zw, err := gzip.NewWriterLevel(temp, gzip.BestSpeed)
	if err != nil {
		return err
	}
	zw.Name = sourceName
	zw.ModTime = sourceInfo.ModTime()
	writtenHash := sha256.New()
	_, copyErr := io.Copy(zw, io.TeeReader(source, writtenHash))
	closeErr := zw.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	var writtenDigest [sha256.Size]byte
	copy(writtenDigest[:], writtenHash.Sum(nil))
	if writtenDigest != sourceDigest {
		return fmt.Errorf("daemon: closed log changed during compression")
	}
	if err := verifyLogSourceSnapshot(root, sourceName, sourceInfo); err != nil {
		return err
	}
	if err := temp.Sync(); err != nil {
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := root.Replace(tempName, targetName, ""); err != nil {
		return err
	}
	installed = true
	// Recheck after the atomic install as well. If a same-user legacy writer
	// changed or replaced the source in the narrow commit window, retain it; the
	// next pass sees a different digest and appends that new content separately.
	if err := verifyLogSourceSnapshot(root, sourceName, sourceInfo); err != nil {
		return err
	}
	if err := source.Close(); err != nil {
		return err
	}
	return root.RemoveRegular(sourceName)
}

type gzipMemberID struct {
	name   string
	digest [sha256.Size]byte
}

func digestLogSource(f *os.File) ([sha256.Size]byte, error) {
	var digest [sha256.Size]byte
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return digest, err
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return digest, err
	}
	copy(digest[:], h.Sum(nil))
	_, err := f.Seek(0, io.SeekStart)
	return digest, err
}

func verifyLogSourceSnapshot(root *privatefs.Root, sourceName string, before os.FileInfo) error {
	current, err := root.OpenReadRegular(sourceName)
	if err != nil {
		return fmt.Errorf("daemon: closed log changed during compression: %w", err)
	}
	defer current.Close()
	after, err := current.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return fmt.Errorf("daemon: closed log changed during compression")
	}
	return nil
}

// gzipMemberIDs validates every member (including checksums) and returns the
// original pending/source name plus a digest of its uncompressed content.
// Including content in the id prevents a later legacy writer from losing new
// data merely because it reused the same daily .log filename. Multistream(false)
// with a ByteReader leaves the buffered stream exactly at the next member.
func gzipMemberIDs(f *os.File) (map[gzipMemberID]struct{}, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	br := bufio.NewReader(f)
	ids := map[gzipMemberID]struct{}{}
	for {
		if _, err := br.Peek(1); errors.Is(err, io.EOF) {
			return ids, nil
		} else if err != nil {
			return nil, err
		}
		zr, err := gzip.NewReader(br)
		if err != nil {
			return nil, err
		}
		zr.Multistream(false)
		name := zr.Name
		h := sha256.New()
		_, copyErr := io.Copy(h, zr)
		closeErr := zr.Close()
		if copyErr != nil {
			return nil, copyErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
		if name != "" {
			var digest [sha256.Size]byte
			copy(digest[:], h.Sum(nil))
			ids[gzipMemberID{name: name, digest: digest}] = struct{}{}
		}
	}
}

func legacyArchiveDay(name string) (time.Time, bool) {
	if !strings.HasPrefix(name, "aplexicad-") || !strings.HasSuffix(name, ".log") {
		return time.Time{}, false
	}
	datePart := strings.TrimSuffix(strings.TrimPrefix(name, "aplexicad-"), ".log")
	if len(datePart) != len("2006-01-02") {
		return time.Time{}, false
	}
	day, err := time.ParseInLocation("2006-01-02", datePart, time.Local)
	return day, err == nil
}

func pendingArchiveDay(name string) (time.Time, bool) {
	if !strings.HasPrefix(name, logPendingPrefix) {
		return time.Time{}, false
	}
	rest := strings.TrimPrefix(name, logPendingPrefix)
	if len(rest) <= len("2006-01-02") || !strings.HasPrefix(rest[len("2006-01-02"):], logPendingMarker) {
		return time.Time{}, false
	}
	day, err := time.ParseInLocation("2006-01-02", rest[:len("2006-01-02")], time.Local)
	return day, err == nil
}

func archivedLogDay(name string) (time.Time, bool) {
	trimmed := strings.TrimSuffix(name, ".gz")
	if trimmed == name && !strings.HasSuffix(name, ".log") {
		return time.Time{}, false
	}
	return legacyArchiveDay(trimmed)
}

func purgeOldLogsRoot(root *privatefs.Root, keepDays int) {
	entries, err := root.ReadDir(".")
	if err != nil {
		return
	}
	cutoff := time.Now().Local().AddDate(0, 0, -keepDays)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if day, ok := archivedLogDay(name); ok && day.Before(cutoff) {
			_ = root.RemoveRegular(name)
		}
	}
}

// nextLocalMidnight returns the first instant of the next local calendar
// day after now (00:00:00 in now's location). It is exported-for-test via
// the package-internal tests and drives both the midnight scheduler and
// its assertions. Recomputing it each cycle (rather than adding a fixed
// 24h) keeps rotation aligned with wall-clock midnight across DST shifts.
func nextLocalMidnight(now time.Time) time.Time {
	y, m, d := now.Date()
	return time.Date(y, m, d+1, 0, 0, 0, 0, now.Location())
}

// StartMidnightRotation launches a goroutine that calls Rotate() at every
// local midnight until ctx is cancelled. Each cycle sleeps until the next
// local midnight (recomputed after every rotation so DST wall-clock
// shifts don't drift the boundary), then rotates. This is platform
// independent — it is the only rotation trigger on Windows, which has no
// SIGHUP. The goroutine exits cleanly on ctx.Done() (the timer is
// stopped, no leak).
func (r *RotatingLogger) StartMidnightRotation(ctx context.Context) {
	go func() {
		for {
			now := time.Now()
			timer := time.NewTimer(time.Until(nextLocalMidnight(now)))
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
				if err := r.Rotate(); err != nil {
					r.Error("midnight log rotation failed", "err", err)
				}
			}
		}
	}()
}

// purgeOldLogs removes aplexicad-YYYY-MM-DD.log.gz and legacy .log archive
// files in dir whose date part (parsed in the LOCAL zone) is older than
// keepDays. Errors are swallowed — the caller is best-effort and a corrupt
// directory must not block startup. The live aplexicad.log file does not match
// the dated names and is never purged.
func purgeOldLogs(dir string, keepDays int) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Local().AddDate(0, 0, -keepDays)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if day, ok := archivedLogDay(name); ok && day.Before(cutoff) {
			_ = os.Remove(filepath.Join(dir, name))
		}
	}
}
