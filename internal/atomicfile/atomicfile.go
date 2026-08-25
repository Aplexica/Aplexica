// Package atomicfile provides a crash-safe file writer: write-to-tmp,
// fsync, then atomic rename. Used by both the canonical store and the
// secrets store to replace the truncate-then-write semantics of
// os.WriteFile.
//
// What this protects against: a crash (power cut, OOM kill, kernel panic)
// in the middle of overwriting an existing file. With os.WriteFile, the
// destination is truncated to zero and then written; a crash mid-write
// leaves the destination half-overwritten or zero-length. With WriteFile
// from this package, the destination's bytes are either fully the old
// content or fully the new content — never an in-between state.
//
// What this does NOT protect against: bit-rot, intentional corruption,
// or non-atomic operations on multiple files. It is also not a
// replacement for fsync on the parent directory (callers that need
// crash-consistent directory entries must fsync the parent themselves).
package atomicfile

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
)

// WriteFile writes data to path crash-safely. It writes the bytes to a
// unique sibling .tmp.<random> file, fsyncs and closes it, then renames
// over path. The destination's parent directory must already exist;
// callers are responsible for MkdirAll.
//
// On any failure, the tmp sibling is best-effort removed and any
// pre-existing destination file is left untouched.
func WriteFile(path string, data []byte, mode os.FileMode) error {
	if existing, err := os.ReadFile(path); err == nil && bytes.Equal(existing, data) {
		if info, statErr := os.Stat(path); statErr == nil && info.Mode().Perm() != mode.Perm() {
			if chmodErr := os.Chmod(path, mode.Perm()); chmodErr != nil {
				return fmt.Errorf("atomicfile: chmod unchanged %s: %w", path, chmodErr)
			}
		}
		if err := protectPrivatePath(path, mode); err != nil {
			return fmt.Errorf("atomicfile: protect unchanged %s: %w", path, err)
		}
		return nil
	}

	suffix := make([]byte, 8)
	if _, err := rand.Read(suffix); err != nil {
		return fmt.Errorf("atomicfile: random suffix: %w", err)
	}
	tmpPath := path + ".tmp." + hex.EncodeToString(suffix)

	f, err := os.OpenFile(tmpPath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return fmt.Errorf("atomicfile: open tmp %s: %w", tmpPath, err)
	}
	if err := protectPrivateFileAtPath(f, tmpPath, mode); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("atomicfile: protect tmp %s: %w", tmpPath, err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("atomicfile: write tmp %s: %w", tmpPath, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("atomicfile: fsync tmp %s: %w", tmpPath, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("atomicfile: close tmp %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("atomicfile: rename %s -> %s: %w", tmpPath, path, err)
	}
	return nil
}
