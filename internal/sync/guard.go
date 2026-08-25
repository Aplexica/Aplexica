// Package syncd implements Aplexica's multi-agent sync orchestrator: given
// a directory and a set of installed adapters, watches the directory for
// filesystem events, imports each settled change via the primary adapter
// for its filename, and fans out the resulting artifact to every other
// installed adapter's native location for that artifact's kind. The
// recursion guard (RecursionGuard) prevents the orchestrator's own writes
// from feeding back as inbound events.
//
// Package name is `syncd` rather than `sync` to avoid shadowing the
// standard library's sync package; from imports it appears as `syncd`.
package syncd

import (
	"sync"
	"time"
)

// RecursionGuard tracks paths the orchestrator wrote recently. When the
// watcher's debouncer reports a settled event for one of these paths,
// the orchestrator suppresses the event (Suppressed returns true).
//
// Mechanism: in-process map[path]time.Time keyed by absolute path. Entries
// expire after window elapses. Safe for concurrent use.
//
// V0.7.0 limitation: in-process only — when the daemon restarts, the
// guard is empty. The BRD-03 §4.5 `causedBy` event-hash chain check (a
// second-line defense that survives restarts) is deferred; see ADR-0045
// (recursion-defense-v1-scope) for what V1 ships instead — this path
// guard plus destHashes (content idempotency) and remoteOrigins
// (cross-device origin set).
type RecursionGuard struct {
	window time.Duration

	mu    sync.Mutex
	marks map[string]guardMark
	seq   uint64
}

// guardMark is one path's suppression record. seq is a process-wide counter
// rather than a timestamp so Unmark can identify the exact mark it is
// withdrawing: coarse platform clocks (Windows ticks at ~15ms) make two marks
// of the same path indistinguishable by time alone.
type guardMark struct {
	at  time.Time
	seq uint64
}

// NewRecursionGuard returns a guard with the given suppression window.
// The BRD doesn't specify a target window; 5 seconds is a generous default
// that covers FS event delivery + debouncer quiet period (500ms) + adapter
// import + export round-trip. Callers tune via the constructor.
func NewRecursionGuard(window time.Duration) *RecursionGuard {
	return &RecursionGuard{
		window: window,
		marks:  map[string]guardMark{},
	}
}

// SetWindow updates the suppression window used for FUTURE Mark calls.
// Entries already in the guard map keep their original eviction time —
// Suppressed will continue to evict them at the OLD deadline, then any
// re-Mark of the same path will use the new window. Safe for concurrent
// callers.
//
// Live-setter half of the v0.27.x SIGHUP config-reload story: the daemon's
// SIGHUP handler calls SetWindow when the operator edits `guardWindow` in
// config.json so the change takes effect without a restart. Restarting
// in-flight entries with the new deadline would race with concurrent
// Mark/Suppressed and risk false-negatives during the swap.
func (g *RecursionGuard) SetWindow(w time.Duration) {
	g.mu.Lock()
	g.window = w
	g.mu.Unlock()
}

// Window returns the current suppression window. Primarily for tests and
// for the daemon's SIGHUP handler to log the effective post-reload value.
func (g *RecursionGuard) Window() time.Duration {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.window
}

// Mark records that path was written by the orchestrator at the current
// time. Within window, Suppressed will return true for path. The returned
// token identifies this mark for Unmark; callers that always write may
// discard it.
func (g *RecursionGuard) Mark(path string) uint64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.seq++
	g.marks[path] = guardMark{at: time.Now(), seq: g.seq}
	return g.seq
}

// Unmark withdraws the mark identified by token, but only when no later Mark
// has replaced it. It exists for guard-before-write callers whose write is
// then declined: a mark with no write behind it suppresses a genuine
// agent-side edit to that path for the whole window, and a retry loop that
// re-marks every few seconds can suppress it indefinitely. Callers must first
// establish that nothing was written — withdrawing the guard over bytes the
// orchestrator did write invites a self-echo re-import.
func (g *RecursionGuard) Unmark(path string, token uint64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if mark, ok := g.marks[path]; ok && mark.seq == token {
		delete(g.marks, path)
	}
}

// Suppressed reports whether path was recently marked. Also opportunistically
// evicts expired entries on every check (no separate sweep goroutine needed —
// the working set is bounded by activity, and stale entries are removed when
// they're looked up).
func (g *RecursionGuard) Suppressed(path string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()

	now := time.Now()
	mark, ok := g.marks[path]
	if !ok {
		return false
	}
	if now.Sub(mark.at) > g.window {
		delete(g.marks, path)
		return false
	}
	return true
}
