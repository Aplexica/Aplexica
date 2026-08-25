package syncd

import (
	"sync"
	"time"
)

// QuarantineTracker is the BRD-03 FR-03.15 quarantine state machine.
// Each adapter accumulates timestamps of recent failures; when the
// number of failures within a sliding window exceeds the threshold,
// the adapter is marked quarantined. Quarantined adapters are
// skipped by the orchestrator (no Import, no Export) until the
// window clears.
//
// Default policy: 3 failures within 10 minutes → quarantine.
// `aplexica daemon restart` clears the quarantine state (in-memory).
type QuarantineTracker struct {
	mu         sync.Mutex
	threshold  int
	window     time.Duration
	failures   map[string][]time.Time
	quarantine map[string]time.Time // value: time when quarantine started
}

// NewQuarantineTracker returns a tracker with the FR-03.15 policy
// values. Threshold and window are tunable for tests (smaller window
// = faster quarantine in CI).
func NewQuarantineTracker(threshold int, window time.Duration) *QuarantineTracker {
	return &QuarantineTracker{
		threshold:  threshold,
		window:     window,
		failures:   map[string][]time.Time{},
		quarantine: map[string]time.Time{},
	}
}

// DefaultQuarantineTracker returns a tracker with the BRD-03 §6.15
// values: 3 failures in 10 minutes.
func DefaultQuarantineTracker() *QuarantineTracker {
	return NewQuarantineTracker(3, 10*time.Minute)
}

// RecordFailure appends `now` to the adapter's failure list (after
// pruning entries outside the window). If the post-prune count meets
// or exceeds the threshold, marks the adapter quarantined.
//
// Returns (justQuarantined=true) on the transition into quarantine
// so callers can log a single "quarantined" line per transition.
func (q *QuarantineTracker) RecordFailure(name string, now time.Time) (justQuarantined bool) {
	q.mu.Lock()
	defer q.mu.Unlock()

	q.pruneLocked(name, now)
	q.failures[name] = append(q.failures[name], now)

	if _, already := q.quarantine[name]; already {
		return false
	}
	if len(q.failures[name]) >= q.threshold {
		q.quarantine[name] = now
		return true
	}
	return false
}

// RecordSuccess clears the failure list for an adapter (a successful
// Import/Export is the signal that the adapter is healthy). Does NOT
// auto-unquarantine — explicit Clear / restart is required to
// resume an adapter that's been quarantined, per the BRD's "and
// continue running others" stance.
func (q *QuarantineTracker) RecordSuccess(name string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.failures, name)
}

// IsQuarantined reports whether the adapter is currently quarantined.
// If the quarantine window has elapsed, automatically clears it
// and reports false — operators get a self-healing recovery after
// the cool-down without needing a manual restart.
func (q *QuarantineTracker) IsQuarantined(name string, now time.Time) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	t, ok := q.quarantine[name]
	if !ok {
		return false
	}
	if now.Sub(t) > q.window {
		delete(q.quarantine, name)
		delete(q.failures, name)
		return false
	}
	return true
}

// Clear removes both the failure history and the quarantine flag for
// an adapter. Called by `aplexica adapters unquarantine` (a future
// CLI handle) and the daemon's restart path.
func (q *QuarantineTracker) Clear(name string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.failures, name)
	delete(q.quarantine, name)
}

// Snapshot returns the set of currently-quarantined adapter names
// (with self-healing for expired entries). Useful for status
// reports + metric updates.
func (q *QuarantineTracker) Snapshot(now time.Time) []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]string, 0, len(q.quarantine))
	for name, t := range q.quarantine {
		if now.Sub(t) > q.window {
			delete(q.quarantine, name)
			delete(q.failures, name)
			continue
		}
		out = append(out, name)
	}
	return out
}

// pruneLocked drops timestamps older than the window for the named
// adapter. Caller must hold q.mu.
func (q *QuarantineTracker) pruneLocked(name string, now time.Time) {
	cutoff := now.Add(-q.window)
	in := q.failures[name]
	out := in[:0]
	for _, t := range in {
		if t.After(cutoff) {
			out = append(out, t)
		}
	}
	if len(out) == 0 {
		delete(q.failures, name)
	} else {
		q.failures[name] = out
	}
}
