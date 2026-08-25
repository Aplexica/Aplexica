// Disk-pressure emergency mode for the canonical store. Implements
// BRD-03 §4.8.2: when the store crosses a configured "high watermark"
// the daemon force-snapshots every artifact so a subsequent prune pass
// can free space. Pure best-effort — symlinks aren't followed and
// per-file / per-artifact errors are skipped so a single bad inode
// can't disable the entire emergency mode.
//
// v0.34.0 ships the primitives (CheckPressure + ForceSnapshotsAll).
// The daemon wiring (periodic check + WARN log + ForceSnapshotsAll
// call) lives in cmd/aplexica/cmd_daemon.go.
package retention

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/aplexica/aplexica/internal/acf"
)

// StoreSize reports the disk footprint of a canonical store, split by
// structural area so the honest accounting (reclaimable-by-retention vs
// pinned) is derivable from one walk without ever parsing an event log. The
// per-area fields always sum to Bytes.
type StoreSize struct {
	Bytes int64
	// EventLogBytes is the active per-artifact event logs
	// (events/<kind>/<id>.jsonl). Append-only: pruning only RELOCATES
	// pre-snapshot events into the .compacted layer inside the same store, so
	// no retention pass can free these bytes — they stay pinned until a
	// compaction feature (gated, not shipped) can truncate them.
	EventLogBytes int64
	// CompactedBytes is the .compacted gz segments
	// (events/.compacted/<kind>/<id>.jsonl.gz). Retention grace-deletes these
	// whole files once past the grace window — the one store area whose bytes
	// are structurally reclaimable.
	CompactedBytes int64
	// BlobBytes is the content-addressed blob store (blobs/**). Pinned in
	// this accounting: blobs referenced by live artifact heads must survive,
	// and unreferenced blobs are collected by the periodic sweep after its
	// grace window, so what persists is presumed live. Deciding per-blob
	// liveness here would require replaying every conversation log — exactly
	// the cost this walk-time accounting exists to avoid — so the split is
	// conservative (it may under-report reclaimable, never over-report).
	BlobBytes int64
	// OtherBytes is everything else: artifact head metadata (acf/**), branch
	// indexes, catalogs, locks. Pinned — deleting any of it breaks the store.
	OtherBytes int64
}

// ReclaimableBytes is the portion of the store retention could actually free
// without new features: the grace-deletable .compacted segments. Everything
// else is pinned — see the per-field docs for why each area cannot be freed.
func (s StoreSize) ReclaimableBytes() int64 { return s.CompactedBytes }

// PinnedBytes is the portion of the store no retention pass can legally free:
// active event logs, live artifact metadata, and blobs presumed referenced by
// live heads. When PinnedBytes alone meets the high watermark, the watermark
// is unreachable no matter how hard retention works.
func (s StoreSize) PinnedBytes() int64 { return s.Bytes - s.CompactedBytes }

// addFile classifies one regular file's size into the store-area field its
// path (relative to the store root) belongs to, and into the total.
func (s *StoreSize) addFile(rel string, size int64) {
	s.Bytes += size
	parts := strings.Split(filepath.ToSlash(rel), "/")
	switch {
	case parts[0] == "events" && len(parts) > 1 && parts[1] == ".compacted":
		s.CompactedBytes += size
	case parts[0] == "events":
		s.EventLogBytes += size
	case parts[0] == "blobs":
		s.BlobBytes += size
	default:
		s.OtherBytes += size
	}
}

// CheckPressure walks the store root and returns the classified byte totals +
// whether the high watermark is exceeded. Best-effort: symlinks are not
// followed, unreadable files are skipped silently (the walker's per-entry err
// is swallowed). The only error returned is from filepath.Walk itself, which
// would mean the root doesn't exist or some other terminal walk failure.
//
// highWatermarkBytes <= 0 makes "exceeded" always false (the daemon
// treats this as "feature disabled").
func CheckPressure(storeRoot string, highWatermarkBytes int64) (StoreSize, bool, error) {
	var size StoreSize
	err := filepath.Walk(storeRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // skip unreadable
		}
		if info.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(storeRoot, path)
		if rerr != nil {
			rel = path // classify as "other"; the total still counts it
		}
		size.addFile(rel, info.Size())
		return nil
	})
	if err != nil {
		return StoreSize{}, false, fmt.Errorf("retention: walk store for pressure check: %w", err)
	}
	if highWatermarkBytes <= 0 {
		return size, false, nil
	}
	return size, size.Bytes >= highWatermarkBytes, nil
}

// ForceSnapshotsAll snapshots every artifact in the store (across all
// four kinds). Used by the disk-pressure emergency mode per BRD-03 §4.8.2
// — when the store exceeds the high watermark, the engine forces
// snapshots so subsequent PruneArtifact runs can free space.
//
// An artifact whose head event is ALREADY an EventTypeSnapshot is skipped
// (it has accumulated nothing new to encode since its last snapshot), the
// same load-bearing guard as the time-based path in TickTimeBasedSnapshots.
// Without it a repeated gc/pressure pass over an idle store would append a
// fresh redundant snapshot every pass, growing the event log without bound.
// (The single-artifact `aplexica snapshot <id>` force path stays a force —
// CreateSnapshot's own contract is unchanged; the guard lives here, at the
// force-all caller.)
//
// Returns the number of snapshots successfully created. Per-artifact
// errors (reading events or CreateSnapshot) are ignored (best-effort under
// pressure: one bad artifact must not block the rest of the cleanup pass).
// The iteration order is memory → skill → tool → conversation; callers
// should treat that as arbitrary.
func ForceSnapshotsAll(ctx context.Context, store *acf.Store) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, ctx.Err()
	}
	var n int
	for _, k := range []acf.Kind{acf.KindMemory, acf.KindSkill, acf.KindTool, acf.KindConversation} {
		arts, err := store.ListArtifacts(k)
		if err != nil {
			return n, fmt.Errorf("retention: list %s: %w", k, err)
		}
		for _, a := range arts {
			latest, found, rerr := store.LastEvent(k, a.ArtifactID)
			if rerr != nil || !found {
				continue // unreadable or empty — skip (best-effort, see godoc)
			}
			if isCheckpointEvent(latest) {
				continue // head is already a checkpoint (snapshot/baseline) — nothing new to encode
			}
			if _, err := CreateSnapshot(ctx, store, k, a.ArtifactID); err == nil {
				n++
			}
			// Per-artifact errors are intentionally swallowed — see godoc.
		}
	}
	return n, nil
}
