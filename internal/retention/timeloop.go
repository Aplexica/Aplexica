package retention

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
)

// TickTimeBasedSnapshots walks every artifact in the store and creates a
// snapshot for any whose most-recent event is older than the per-kind
// maxAge AND whose latest event is NOT itself a snapshot.
//
// The "not a snapshot" gate is the load-bearing piece of BRD-03 §4.8.1's
// time-based trigger: an artifact that hasn't accumulated any new
// create/update/redact events since its last snapshot has nothing new to
// encode — replaying it would just produce the same materialized state.
// Skipping it keeps the event log from filling with no-op snapshots
// every tick (e.g. an idle memory file would otherwise grow one snapshot
// per tick forever).
//
// kinds with a missing key, zero, or negative threshold in maxAge are
// skipped entirely. Returns the IDs that were snapshotted in this tick
// (across all kinds, in the iteration order
// memory → skill → tool → conversation; the caller should treat order as
// arbitrary). Per-artifact errors are swallowed (best-effort, same as
// the orchestrator's event-count cadence path); only a list-artifacts
// failure is propagated as a Go error.
func TickTimeBasedSnapshots(ctx context.Context, store *acf.Store, maxAge map[acf.Kind]time.Duration) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var snapped []string
	now := time.Now().UTC()

	for _, kind := range []acf.Kind{acf.KindMemory, acf.KindSkill, acf.KindTool, acf.KindConversation} {
		threshold, ok := maxAge[kind]
		if !ok || threshold <= 0 {
			continue
		}
		arts, err := store.ListArtifacts(kind)
		if err != nil {
			return snapped, fmt.Errorf("retention: list %s: %w", kind, err)
		}
		for _, a := range arts {
			latest, found, err := store.LastEvent(kind, a.ArtifactID)
			if err != nil || !found {
				continue
			}
			if isCheckpointEvent(latest) {
				// Head is already a full-state checkpoint (snapshot or
				// aligned-chains baseline) — nothing new to encode.
				continue
			}
			if now.Sub(latest.Timestamp) < threshold {
				continue
			}
			if !AutomaticSnapshotAllowed(store, a) {
				continue
			}
			if _, err := CreateSnapshot(ctx, store, kind, a.ArtifactID); err != nil {
				continue
			}
			snapped = append(snapped, a.ArtifactID)
		}
	}
	return snapped, nil
}

// Runner owns the per-tick state for time-based snapshotting and exposes
// a thread-safe setter for the per-kind max-age map. This is the
// SIGHUP-friendly form added in v0.34.0; previously the daemon's
// goroutine captured the maxAge map by reference at startup, so live
// reconfiguration of SnapshotMaxAge* required a restart.
//
// The map is COPIED on both NewRunner and SetMaxAge so callers can mutate
// their original map without affecting the runner, and the runner's
// internal map can never be observed mid-mutation by Tick.
//
// The bare RunTimeBasedSnapshotter function below is preserved as a
// one-shot wrapper for callers (e.g. tests) that don't need hot-reload.
type Runner struct {
	store *acf.Store

	mu     sync.Mutex
	maxAge map[acf.Kind]time.Duration
}

// NewRunner constructs a Runner with a defensive copy of maxAge.
func NewRunner(store *acf.Store, maxAge map[acf.Kind]time.Duration) *Runner {
	cp := make(map[acf.Kind]time.Duration, len(maxAge))
	for k, v := range maxAge {
		cp[k] = v
	}
	return &Runner{store: store, maxAge: cp}
}

// SetMaxAge atomically replaces the per-kind max-age map with a defensive
// copy of next. Safe to call concurrently with Tick / Run. The next Tick
// observes the new map; an in-flight tick continues with the snapshot it
// took at the start of Tick.
func (r *Runner) SetMaxAge(next map[acf.Kind]time.Duration) {
	cp := make(map[acf.Kind]time.Duration, len(next))
	for k, v := range next {
		cp[k] = v
	}
	r.mu.Lock()
	r.maxAge = cp
	r.mu.Unlock()
}

// MaxAge returns a defensive copy of the current per-kind max-age map.
// Safe to call concurrently with SetMaxAge / Tick / Run.
func (r *Runner) MaxAge() map[acf.Kind]time.Duration {
	r.mu.Lock()
	cp := make(map[acf.Kind]time.Duration, len(r.maxAge))
	for k, v := range r.maxAge {
		cp[k] = v
	}
	r.mu.Unlock()
	return cp
}

// Tick runs one TickTimeBasedSnapshots cycle using the current MaxAge
// snapshot. Errors propagate unchanged from TickTimeBasedSnapshots.
func (r *Runner) Tick(ctx context.Context) ([]string, error) {
	return TickTimeBasedSnapshots(ctx, r.store, r.MaxAge())
}

// Run blocks until ctx is cancelled, calling Tick on every interval tick.
// Per-tick errors are swallowed (best-effort retention, same contract as
// RunTimeBasedSnapshotter).
//
// interval <= 0 is normalized to 1*time.Hour, the daemon-default in
// BRD-03 §4.8.1.
func (r *Runner) Run(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		interval = 1 * time.Hour
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			_, _ = r.Tick(ctx)
		}
	}
}

// RunTimeBasedSnapshotter is a one-shot wrapper around Runner.Run for
// callers that don't need hot-reload (tests, ad-hoc CLI invocations).
// New code should prefer NewRunner + Runner.Run so SnapshotMaxAge can be
// hot-reloaded via SetMaxAge — see v0.34.0 SIGHUP wiring in
// cmd/aplexica/cmd_daemon_unix.go.
//
// Per-tick errors are swallowed (the underlying TickTimeBasedSnapshots
// returns them only for list-artifacts failures, which we don't want to
// kill the daemon over — a transient FS error on one tick shouldn't
// disable the snapshotter for the rest of the process lifetime).
func RunTimeBasedSnapshotter(ctx context.Context, store *acf.Store, maxAge map[acf.Kind]time.Duration, interval time.Duration) error {
	return NewRunner(store, maxAge).Run(ctx, interval)
}
