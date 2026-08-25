package nativebackup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// MaxPreRestoreSnapshots bounds reversible undo history. A restore first
// reduces existing history to MaxPreRestoreSnapshots-1 and then adds its new
// undo point, so ordinary successful restores never exceed this limit.
const MaxPreRestoreSnapshots = 3

type preRestoreCandidate struct {
	name      string
	path      string
	createdAt time.Time
	preserve  bool
}

// PrunePreRestoreHistory removes incomplete pre-restore trees and then keeps at
// most keep complete undo snapshots. preservePath, when it names a snapshot
// directly under backupsRoot, is always retained and counts toward keep. This
// lets a user restore from an older undo point without the pre-allocation sweep
// deleting the source being read.
//
// Callers must serialize this operation with restore creation across processes.
// Every deletion is constrained to a direct child whose exact ID parses as a
// pre-restore snapshot; links and other object types are never traversed.
func PrunePreRestoreHistory(ctx context.Context, backupsRoot string, keep int, preservePath string) (int, error) {
	if keep < 0 {
		return 0, fmt.Errorf("nativebackup: invalid pre-restore retention %d", keep)
	}
	rootAbs, err := filepath.Abs(backupsRoot)
	if err != nil {
		return 0, err
	}
	rootAbs = filepath.Clean(rootAbs)
	preserveAbs := ""
	if preservePath != "" {
		if candidate, absErr := filepath.Abs(preservePath); absErr == nil {
			candidate = filepath.Clean(candidate)
			if filepath.Dir(candidate) == rootAbs {
				if kind, ok := SnapshotKindFromID(filepath.Base(candidate)); ok && kind == "pre-restore" {
					preserveAbs = candidate
				}
			}
		}
	}
	entries, err := os.ReadDir(rootAbs)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("nativebackup: enumerate pre-restore history: %w", err)
	}

	removed := 0
	var candidates []preRestoreCandidate
	var cleanupErrs []error
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return removed, err
		}
		kind, ok := SnapshotKindFromID(entry.Name())
		if !ok || kind != "pre-restore" || !entry.IsDir() {
			continue
		}
		path := filepath.Join(rootAbs, entry.Name())
		info, statErr := os.Lstat(path)
		if statErr != nil {
			cleanupErrs = append(cleanupErrs, fmt.Errorf("inspect %s: %w", entry.Name(), statErr))
			continue
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		manifest, manifestErr := ReadSnapshotManifestContext(ctx, path)
		if manifestErr == nil {
			// A manifest is written last, but it is not by itself proof that every
			// payload survived a crash or later filesystem damage. Before this tree
			// is allowed to displace an older undo point, require the complete signed
			// inventory to exist with the declared regular-file types and sizes.
			// Restore still performs the expensive digest/authentication proof before
			// accepting bytes; retention deliberately avoids rehashing full history.
			manifestErr = verifySnapshotInventoryOnly(ctx, path, manifest)
		}
		if manifestErr != nil {
			// A tree without a strict manifest and complete structural inventory is
			// not a restorable undo point. It is the residue produced by a crash,
			// cancellation, or truncated/missing payload after snapshot creation.
			if path == preserveAbs {
				// The caller intends to restore this exact tree. Fail before the
				// retention phase can delete any valid fallback: an invalid requested
				// restore must never make recovery options worse merely by being tried.
				return removed, fmt.Errorf("nativebackup: preserved pre-restore snapshot %s is incomplete: %w", entry.Name(), manifestErr)
			}
			if removeErr := os.RemoveAll(path); removeErr != nil {
				cleanupErrs = append(cleanupErrs, fmt.Errorf("remove incomplete pre-restore snapshot %s: %w", entry.Name(), removeErr))
			} else {
				removed++
			}
			continue
		}
		createdAt := manifest.CreatedAt
		if createdAt.IsZero() {
			createdAt = info.ModTime().UTC()
		}
		candidates = append(candidates, preRestoreCandidate{
			name:      entry.Name(),
			path:      path,
			createdAt: createdAt,
			preserve:  path == preserveAbs,
		})
	}

	sort.Slice(candidates, func(i, j int) bool {
		if !candidates[i].createdAt.Equal(candidates[j].createdAt) {
			return candidates[i].createdAt.After(candidates[j].createdAt)
		}
		return candidates[i].name > candidates[j].name
	})
	kept := 0
	for _, candidate := range candidates {
		if candidate.preserve {
			kept++
		}
	}
	for _, candidate := range candidates {
		if candidate.preserve {
			continue
		}
		if kept < keep {
			kept++
			continue
		}
		if err := ctx.Err(); err != nil {
			return removed, err
		}
		if removeErr := os.RemoveAll(candidate.path); removeErr != nil {
			cleanupErrs = append(cleanupErrs, fmt.Errorf("remove expired pre-restore snapshot %s: %w", candidate.name, removeErr))
			continue
		}
		removed++
	}
	if len(cleanupErrs) > 0 {
		return removed, fmt.Errorf("nativebackup: pre-restore cleanup incomplete: %w", errors.Join(cleanupErrs...))
	}
	return removed, nil
}
