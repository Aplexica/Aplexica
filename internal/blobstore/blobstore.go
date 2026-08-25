// Package blobstore is a content-addressed store for conversation
// attachment bytes (BRD-03 §4.8.3). It exists so the attachment-retention
// engine can bound store size WITHOUT rewriting hashed event payloads:
// the raw binary content lives here keyed by its own SHA-256, while the
// canonical event log carries only the content hash (a small, hashable
// reference). Eviction becomes "stop referencing a blob + GC the orphan",
// never "edit an event in place".
//
// Layout: <Root>/<AA>/<BB>/<hash>, a two-level shard taken from the first
// two hex bytes of the content hash. This keeps any single directory from
// holding the entire blob population (POSIX directory scaling) while
// staying trivial to compute from the hash alone.
//
// Writes are crash-safe and idempotent: the blob's bytes go down via
// atomicfile (tmp + fsync + rename) and Put then fsyncs the shard directory
// so the new directory entry is durable too (a directory fsync is best-effort
// on platforms that reject it, e.g. Windows). The same bytes always map to the
// same path, so a re-Put is a no-op once the file exists — and because the
// store is content-addressed, even a lost-then-recreated entry is benign.
// Deletes are idempotent too. GC is the only operation
// that removes data, and it is deliberately conservative (a caller-supplied
// live set plus a grace window guarding freshly-written orphans).
package blobstore

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/aplexica/aplexica/internal/atomicfile"
)

const (
	// shardBytes is how many leading bytes of the hex hash are consumed by
	// the on-disk shard prefix. Two bytes -> two nested directories
	// (<AA>/<BB>), each 256-wide, so the population spreads across up to
	// 65536 leaf directories before any single one grows large.
	shardBytes = 2

	// dirPerm / filePerm are the on-disk permission bits for shard
	// directories and blob files. Owner-writable, world-readable —
	// matching the canonical store's 0o755/0o644 convention.
	dirPerm  os.FileMode = 0o755
	filePerm os.FileMode = 0o644
)

// Store is a content-addressed blob store rooted at a single directory.
// The zero value is not usable; set Root to an existing-or-creatable path.
type Store struct {
	Root string
}

// hashHex returns the lowercase hex SHA-256 of raw — the canonical blob id.
func hashHex(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// hexCharsPerByte is the number of hex characters that encode one byte of
// the hash. The shard prefix consumes shardBytes bytes => shardHexChars hex
// characters, split into shardBytes single-byte directory segments.
const hexCharsPerByte = 2

// shardHexChars is the total hex-char width of the shard prefix.
const shardHexChars = shardBytes * hexCharsPerByte

// Path returns the on-disk path a blob with the given hash would occupy.
// It does NOT check existence. Short or malformed hashes (fewer than
// shardHexChars hex chars) fall back to a flat path under Root so the
// function never panics on bad input; well-formed SHA-256 hashes (64 hex
// chars) always shard into shardBytes nested single-byte directories.
func (s *Store) Path(hash string) string {
	if len(hash) < shardHexChars {
		return filepath.Join(s.Root, hash)
	}
	segments := make([]string, 0, shardBytes+2)
	segments = append(segments, s.Root)
	for i := 0; i < shardBytes; i++ {
		start := i * hexCharsPerByte
		segments = append(segments, hash[start:start+hexCharsPerByte])
	}
	segments = append(segments, hash)
	return filepath.Join(segments...)
}

// Has reports whether a blob with the given hash is present on disk.
func (s *Store) Has(hash string) bool {
	info, err := os.Stat(s.Path(hash))
	if err != nil {
		return false
	}
	return !info.IsDir()
}

// Put writes raw to the store keyed by hex(sha256(raw)) and returns that
// hash. The write is atomic (tmp + fsync + rename) and idempotent: if the
// blob already exists, Put returns the hash without rewriting. Concurrent
// Puts of identical bytes are safe — atomicfile renames over the
// destination and the content is byte-identical either way.
func (s *Store) Put(raw []byte) (string, error) {
	hash := hashHex(raw)
	if s.Has(hash) {
		return hash, nil
	}
	dest := s.Path(hash)
	shardDir := filepath.Dir(dest)
	if err := os.MkdirAll(shardDir, dirPerm); err != nil {
		return "", fmt.Errorf("blobstore: mkdir shard for %s: %w", hash, err)
	}
	if err := atomicfile.WriteFile(dest, raw, filePerm); err != nil {
		return "", fmt.Errorf("blobstore: write blob %s: %w", hash, err)
	}
	// atomicfile fsyncs the blob's *bytes* and renames atomically, but per its
	// own contract the rename's effect on the parent directory is the caller's
	// to make durable. Without this, a crash right after Put can leave the blob
	// bytes on disk yet the <AA>/<BB> directory entry unrecovered, so the blob
	// "vanishes" on reboot. fsync the shard directory so the new entry survives.
	if err := fsyncDir(shardDir); err != nil {
		return "", fmt.Errorf("blobstore: fsync shard dir for %s: %w", hash, err)
	}
	return hash, nil
}

// fsyncDir opens dir and fsyncs it so a rename/create inside it is durable
// across a crash. A directory fsync is not supported on every platform or
// filesystem (notably Windows, where opening a directory handle for sync is
// rejected); those known-benign cases are tolerated rather than failing an
// otherwise-successful Put. Genuine I/O errors are returned.
func fsyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	// Windows has no supported way to fsync a directory handle:
	// FlushFileBuffers on a directory returns ERROR_ACCESS_DENIED ("Access is
	// denied"), which maps to os.ErrPermission and is NOT one of the
	// EINVAL/ENOTSUP cases tolerated below. The blob's bytes were already
	// fsynced and atomically renamed by atomicfile.WriteFile, so dropping only
	// the directory-entry sync here is the documented best-effort behavior.
	// Skip it rather than failing an otherwise-durable Put.
	if runtime.GOOS == "windows" {
		return d.Close()
	}
	syncErr := d.Sync()
	closeErr := d.Close()
	if syncErr != nil {
		// EINVAL / ENOTSUP / "not supported" come back on platforms or
		// filesystems that don't allow syncing a directory handle; the data
		// fsync + atomic rename already happened, so degrade gracefully there.
		if errors.Is(syncErr, errors.ErrUnsupported) ||
			errors.Is(syncErr, os.ErrInvalid) ||
			errors.Is(syncErr, syscall.EINVAL) ||
			errors.Is(syncErr, syscall.ENOTSUP) {
			return nil
		}
		return syncErr
	}
	return closeErr
}

// Open returns a reader over the blob's bytes. The caller must Close it.
// Returns an error wrapping os.ErrNotExist when the blob is absent.
func (s *Store) Open(hash string) (io.ReadCloser, error) {
	f, err := os.Open(s.Path(hash))
	if err != nil {
		return nil, fmt.Errorf("blobstore: open blob %s: %w", hash, err)
	}
	return f, nil
}

// Delete removes a blob. It is idempotent: a missing blob is not an error.
func (s *Store) Delete(hash string) error {
	if err := os.Remove(s.Path(hash)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("blobstore: delete blob %s: %w", hash, err)
	}
	return nil
}

// PlanGCEntry is one blob PlanGC reports as collectible: its content hash
// (the leaf filename), the real on-disk path the walk found it at, and its
// on-disk size in bytes. Path is the authoritative delete target — it is the
// location actually walked, which for a mislocated file (one not at its
// canonical Path(Hash) shard position) differs from what re-sharding the
// basename would reconstruct.
type PlanGCEntry struct {
	Hash  string
	Path  string
	Bytes int64
}

// PlanGC walks every blob under Root and returns — WITHOUT deleting — the set
// of blobs GC would delete: those whose hash is NOT in live AND whose mtime is
// strictly before graceCutoff. The selection predicate is identical to GC, so
// a caller can render a dry-run report from PlanGC and then apply via GC with
// no divergence. Entries are returned in directory-walk order.
//
// Like GC, PlanGC tolerates a non-existent Root (nothing to collect) and
// treats any regular file under Root as a candidate.
func (s *Store) PlanGC(live map[string]bool, graceCutoff time.Time) ([]PlanGCEntry, error) {
	var entries []PlanGCEntry
	err := filepath.WalkDir(s.Root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			if errors.Is(walkErr, os.ErrNotExist) {
				return nil
			}
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		name := d.Name()
		if live[name] {
			return nil
		}
		info, infoErr := d.Info()
		if infoErr != nil {
			if errors.Is(infoErr, os.ErrNotExist) {
				return nil
			}
			return infoErr
		}
		if !info.ModTime().Before(graceCutoff) {
			return nil // within grace window — keep
		}
		entries = append(entries, PlanGCEntry{Hash: name, Path: path, Bytes: info.Size()})
		return nil
	})
	if err != nil {
		return entries, fmt.Errorf("blobstore: gc plan walk: %w", err)
	}
	return entries, nil
}

// GC walks every blob under Root and deletes any blob whose hash is NOT in
// live AND whose mtime is strictly before graceCutoff. Blobs in the live
// set are always kept; orphaned blobs newer than graceCutoff are kept too
// (the grace window protects a blob written between the live-set scan and
// the GC pass — e.g. a Put racing a concurrent eviction). Returns the
// number of blobs deleted.
//
// GC tolerates a non-existent Root (nothing to collect) and ignores
// stray non-blob files whose names are not valid leaf hashes only insofar
// as the live/grace test applies — naming is by directory walk, so any
// regular file under Root is a candidate. Callers should not place
// foreign files under Root.
func (s *Store) GC(live map[string]bool, graceCutoff time.Time) (int, error) {
	// GC = PlanGC + delete: it deletes exactly the blobs PlanGC selects, so the
	// dry-run plan and the applied result can never diverge (one walk, one
	// predicate). A delete error other than not-exist aborts the pass.
	entries, err := s.PlanGC(live, graceCutoff)
	if err != nil {
		return 0, err
	}
	var deleted int
	for _, e := range entries {
		// Delete the path the walk actually found, not a path reconstructed by
		// re-sharding the basename: a mislocated file (not at Path(Hash)) would
		// otherwise yield a no-op remove on a non-existent shard path. Count only
		// real deletes — a vanished file (raced/already gone) is ErrNotExist and
		// must not be reported as reclaimed.
		rmErr := os.Remove(e.Path)
		switch {
		case rmErr == nil:
			deleted++
		case errors.Is(rmErr, os.ErrNotExist):
			// already gone — not counted
		default:
			return deleted, fmt.Errorf("blobstore: gc remove %s: %w", e.Hash, rmErr)
		}
	}
	return deleted, nil
}
