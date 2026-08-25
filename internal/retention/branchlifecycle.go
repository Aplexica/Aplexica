// Package retention — branch auto-archival lifecycle (BRD-04 §4.3.1 /
// FR-04.14). Walks every artifact's branch index and flips Archived=true
// on branches whose most recent event is older than the configured
// staleness threshold.
//
// Archival is metadata-only — no events are deleted, the events file
// is untouched, and archived branches remain accessible via
// `aplexica branch list --include-archived`. Users can revive a branch
// with `aplexica branch unarchive` (FR-04.15).

package retention

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
)

// DoNotArchiveTag is the per-branch tag that exempts a branch from
// auto-archival (FR-04.15).
const DoNotArchiveTag = "do-not-archive"

// AutoArchiveResult summarises one pass.
type AutoArchiveResult struct {
	Inspected int      // branches inspected across all artifacts
	Archived  []string // "<kind>/<artifactId>:<branch>" for newly-archived branches
}

// TickAutoArchive walks every artifact of every kind and flips the
// Archived flag on branches whose most recent event is older than
// staleAfter. Returns the result summary plus any error encountered
// while iterating; per-artifact errors are logged but don't abort the
// pass (best-effort retention contract).
//
// staleAfter <= 0 disables the pass — used by tests and by operators
// who set branches.auto_archive_after_days=0.
func TickAutoArchive(ctx context.Context, store *acf.Store, staleAfter time.Duration, now time.Time) (AutoArchiveResult, error) {
	var res AutoArchiveResult
	if staleAfter <= 0 {
		return res, nil
	}
	cutoff := now.Add(-staleAfter)
	kinds := []acf.Kind{acf.KindMemory, acf.KindSkill, acf.KindTool, acf.KindConversation}
	for _, k := range kinds {
		arts, err := store.ListArtifacts(k)
		if err != nil {
			// Surface the error so the caller can log it but keep going.
			return res, err
		}
		for _, a := range arts {
			select {
			case <-ctx.Done():
				return res, ctx.Err()
			default:
			}
			bi, err := store.RefreshBranchIndex(k, a.ArtifactID)
			if err != nil {
				// Best-effort: skip on read errors.
				continue
			}
			changed := false
			for name, info := range bi.Branches {
				if name == acf.MainBranch {
					continue
				}
				if info.Archived || info.MergedInto != "" {
					continue
				}
				if hasDoNotArchive(info.Tags) {
					continue
				}
				res.Inspected++
				if !info.LastEventAt.IsZero() && info.LastEventAt.Before(cutoff) {
					info.Archived = true
					info.ArchivedAt = now
					info.ArchiveReason = "auto:stale"
					res.Archived = append(res.Archived, string(k)+"/"+a.ArtifactID+":"+name)
					changed = true
				}
			}
			if changed {
				if err := store.WriteBranchIndex(bi); err != nil {
					continue
				}
			}
		}
	}
	return res, nil
}

func hasDoNotArchive(tags []string) bool {
	for _, t := range tags {
		if t == DoNotArchiveTag {
			return true
		}
	}
	return false
}

// AutoArchiveRunner owns the per-tick state for the branch auto-archive
// loop. SetThreshold + SetInterval are safe to call concurrently with
// Tick / Run; the next pass uses the snapshot of values at the start of
// the call.
type AutoArchiveRunner struct {
	store *acf.Store

	mu        sync.RWMutex
	stale     time.Duration
	interval  time.Duration
	lastTick  AutoArchiveResult
	lastError error
}

// NewAutoArchiveRunner constructs an AutoArchiveRunner. staleAfter <= 0
// disables auto-archive; interval <= 0 defaults to 24h.
func NewAutoArchiveRunner(store *acf.Store, staleAfter, interval time.Duration) *AutoArchiveRunner {
	if interval <= 0 {
		interval = defaultAutoArchiveInterval
	}
	return &AutoArchiveRunner{store: store, stale: staleAfter, interval: interval}
}

// SetThreshold hot-reloads the staleness threshold. <=0 disables the
// pass; subsequent Tick calls become no-ops.
func (r *AutoArchiveRunner) SetThreshold(staleAfter time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stale = staleAfter
}

// SetInterval hot-reloads the loop interval.
func (r *AutoArchiveRunner) SetInterval(interval time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if interval <= 0 {
		interval = defaultAutoArchiveInterval
	}
	r.interval = interval
}

// Threshold returns the current staleness threshold.
func (r *AutoArchiveRunner) Threshold() time.Duration {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.stale
}

// Interval returns the current loop interval.
func (r *AutoArchiveRunner) Interval() time.Duration {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.interval
}

// LastTick reports the most recent pass's summary plus any error
// encountered. Suitable for `aplexica status` / Prometheus metrics.
func (r *AutoArchiveRunner) LastTick() (AutoArchiveResult, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.lastTick, r.lastError
}

// Tick performs one auto-archive pass and records the result.
func (r *AutoArchiveRunner) Tick(ctx context.Context, now time.Time) (AutoArchiveResult, error) {
	r.mu.RLock()
	stale := r.stale
	r.mu.RUnlock()
	res, err := TickAutoArchive(ctx, r.store, stale, now)
	r.mu.Lock()
	r.lastTick = res
	r.lastError = err
	r.mu.Unlock()
	return res, err
}

// Run blocks until ctx is cancelled, performing a pass on each interval tick.
// Per-tick errors are swallowed (best-effort retention) but stashed in LastTick
// for observation.
func (r *AutoArchiveRunner) Run(ctx context.Context) error {
	if r.Threshold() <= 0 {
		// Auto-archive disabled — block on ctx without polling.
		<-ctx.Done()
		if errors.Is(ctx.Err(), context.Canceled) {
			return nil
		}
		return ctx.Err()
	}
	t := time.NewTicker(r.Interval())
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			_, _ = r.Tick(ctx, time.Now())
			// Resnap interval in case it was hot-reloaded.
			t.Reset(r.Interval())
		}
	}
}

// defaultAutoArchiveInterval mirrors the [branches].auto_archive_interval
// shipped default. This package can't import internal/config (which
// imports nothing else from this side of the tree), so the value is
// duplicated here. Keep the two in sync.
const defaultAutoArchiveInterval = 24 * time.Hour

// fileMtime helper used in tests to backdate a branch-index file so
// the next pass treats it as stale without needing to wait calendar
// time. Exported for test packages only — the function name's lower
// case makes it package-private; tests live in this package.
func fileMtime(path string) (time.Time, error) {
	st, err := os.Stat(path)
	if err != nil {
		return time.Time{}, err
	}
	return st.ModTime(), nil
}

// touchEventFile is a test helper for backdating event-log files.
func touchEventFile(p string, t time.Time) error {
	if err := os.Chtimes(p, t, t); err != nil {
		return err
	}
	return nil
}

// branchIndexPath mirrors Store.branchIndexPath for the test helpers.
func branchIndexPath(storeRoot string, k acf.Kind, id string) string {
	return filepath.Join(storeRoot, "branches", kindDir(k), id+".json")
}

func kindDir(k acf.Kind) string {
	switch k {
	case acf.KindMemory:
		return "memories"
	default:
		return string(k) + "s"
	}
}
