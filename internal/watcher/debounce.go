// Package watcher implements the inbound side of Aplexica's sync pipeline:
// a filesystem-events wrapper (Watcher) feeding a per-path quiet-period
// debouncer (Debouncer) that calls a user-supplied callback when a path
// has settled. The package is platform-neutral via github.com/fsnotify/
// fsnotify; per-platform optimizations (per ADR-0035) are deferred to a
// later milestone.
package watcher

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"sync"
	"time"
)

const maxConcurrentEvaluations = 4

// Debouncer fires a callback for a path after a configurable quiet period
// with no further events for that path. Within the quiet period, any
// additional Notify call for the same path resets the timer. When the
// timer expires, the file's content is hashed and the callback fires
// only when the hash differs from the last-known hash for that path
// (or when there is no last-known hash). This implements the OSS default
// behavior from ADR-0031: 500 ms quiet period + always-on hash dedup.
//
// The 250 ms coalesce window from ADR-0031 is automatically satisfied:
// because the quiet period resets on every event, any burst of events
// closer together than the quiet period collapses into a single callback.
//
// Safe for concurrent Notify calls. Stop cancels any pending timers and
// must be called when the debouncer is no longer needed.
type Debouncer struct {
	quietPeriod time.Duration
	onSettled   func(path string) bool

	mu       sync.Mutex
	timers   map[string]*time.Timer
	lastHash map[string]string
	stopped  bool
	// inflight counts evaluate callbacks that have fired and may be running
	// the full onSettled pipeline (import + fan-out + store writes). Stop
	// waits for them so callers (orchestrator Close, test cleanup) get a
	// drained pipeline instead of racing in-flight file writes — on Windows
	// that race surfaced as TempDir RemoveAll "file in use" / "directory
	// not empty" CI failures.
	inflight sync.WaitGroup

	// evaluateSlots prevents a startup burst of per-path timers from hashing
	// hundreds of files concurrently before the orchestrator's import gate can
	// apply. Four small hash workers preserve responsiveness while bounding
	// memory to at most four maxHashBytes reads.
	evaluateSlots chan struct{}
}

// NewDebouncer returns a Debouncer whose callback reports no commit status:
// every settle is treated as committed, so the file's content hash is always
// recorded for dedup. Suitable for callers (the `aplexica watch` CLI, tests)
// that do not need failed-settle retry semantics. Daemon callers that must NOT
// permanently dedup-suppress a path whose import failed use
// NewDebouncerWithCommit instead. quiet < 0 is treated as 0 (callback fires
// asynchronously on next scheduler tick).
func NewDebouncer(quiet time.Duration, onSettled func(path string)) *Debouncer {
	return NewDebouncerWithCommit(quiet, func(path string) bool {
		onSettled(path)
		return true
	})
}

// NewDebouncerWithCommit returns a Debouncer whose callback returns whether the
// settle COMMITTED (imported/handled to a durable terminal state). The content
// hash used for dedup is recorded ONLY when the callback reports a commit, so a
// settle that did NOT commit — e.g. a transient import failure during the
// unstable window just after a daemon restart — is retried on the path's next
// event instead of being silently dedup-suppressed forever.
//
// This closes the watcher-side half of the "freshly-written conversation
// stranded out of the store" bug: evaluate() previously recorded lastHash
// BEFORE the callback ran and regardless of its outcome, so a single failed
// settle poisoned the path's dedup entry until its bytes changed again.
func NewDebouncerWithCommit(quiet time.Duration, onSettled func(path string) bool) *Debouncer {
	return &Debouncer{
		quietPeriod:   quiet,
		onSettled:     onSettled,
		timers:        map[string]*time.Timer{},
		lastHash:      map[string]string{},
		evaluateSlots: make(chan struct{}, maxConcurrentEvaluations),
	}
}

// Notify records an event for path. If no event arrives for path within
// the quiet period, the debouncer will hash the file at path and (if
// the hash differs from the last-known hash) invoke onSettled(path).
//
// Multiple Notify calls for the same path within the quiet period
// collapse into a single delayed evaluation.
func (d *Debouncer) Notify(path string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stopped {
		return
	}
	if existing, ok := d.timers[path]; ok {
		existing.Stop()
	}
	d.timers[path] = time.AfterFunc(d.quietPeriod, func() {
		d.evaluate(path)
	})
}

// SetQuietPeriod updates the per-path quiet window used for FUTURE Notify
// calls. Timers already running with the prior deadline are NOT restarted
// — they fire on their original schedule. Safe for concurrent callers.
//
// This is the live-setter half of the v0.27.x SIGHUP config-reload story:
// the daemon's SIGHUP handler calls SetQuietPeriod when the operator edits
// `quiet` in config.json so the change takes effect without a restart.
// The "in-flight timers retain their original deadline" semantics is the
// only safe option without restarting them — which would race with any
// concurrent Notify and risk losing events.
func (d *Debouncer) SetQuietPeriod(p time.Duration) {
	d.mu.Lock()
	d.quietPeriod = p
	d.mu.Unlock()
}

// QuietPeriod returns the current per-path quiet window. Primarily for tests
// and for the daemon's SIGHUP handler to log the effective post-reload value.
func (d *Debouncer) QuietPeriod() time.Duration {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.quietPeriod
}

// Pending returns the number of paths currently waiting for their quiet
// period to elapse. Exposed for the v0.44.0 PendingImports signal on
// daemon.StatusInfo (ADR-0159 Candidate A) — the orchestrator surfaces
// this to the control server so the tray indicator can render "active
// (N pending)" in its status header.
func (d *Debouncer) Pending() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.timers)
}

// Stop cancels every pending timer and BLOCKS until any already-fired
// callback finishes. After Stop, further Notify calls are silently
// ignored. Stop is safe to call multiple times.
func (d *Debouncer) Stop() {
	d.mu.Lock()
	d.stopped = true
	for _, t := range d.timers {
		t.Stop()
	}
	d.timers = map[string]*time.Timer{}
	d.mu.Unlock()
	// Outside the lock: an in-flight evaluate may need it to finish.
	// Registration happens under the lock before stopped is checked, so
	// no new callback can Add after this point.
	d.inflight.Wait()
}

// evaluate runs when the quiet-period timer for a path fires. It hashes
// the file content; if the hash differs from the last-known hash (or
// there is no last-known hash), the onSettled callback fires.
//
// If the file cannot be read (was deleted, permission denied, etc.) the
// callback does not fire — the event is implicitly dropped. A future
// milestone may surface this as a structured error; for v0.5.0 silent
// drop is acceptable because the next real change to the file will
// re-trigger evaluation.
func (d *Debouncer) evaluate(path string) {
	d.mu.Lock()
	if d.stopped {
		d.mu.Unlock()
		return
	}
	// Register BEFORE hashing/queueing. Stop sets stopped under the same lock,
	// so every timer callback that won the race is fully awaited, including
	// those waiting for a hash slot.
	d.inflight.Add(1)
	d.mu.Unlock()
	defer d.inflight.Done()

	if d.evaluateSlots != nil {
		d.evaluateSlots <- struct{}{}
		defer func() { <-d.evaluateSlots }()
	}
	hash, hashErr := hashFile(path)

	d.mu.Lock()
	// Always drop the timer entry on fire — whether or not the hash
	// succeeded. Without this, a Notify for a path that was deleted
	// before its quiet-period elapsed would leak a map entry forever
	// AND make Debouncer.Pending() over-report. Fixed alongside the
	// v0.44.0 PendingImports surfacing so the count stays accurate.
	delete(d.timers, path)
	if hashErr != nil {
		d.mu.Unlock()
		return
	}
	prev, hadPrev := d.lastHash[path]
	d.mu.Unlock()

	if hadPrev && prev == hash {
		return
	}
	// Record the content hash for dedup ONLY after the callback reports a
	// commit. If the settle did not commit (e.g. a transient import failure
	// during the unstable window just after a daemon restart), leave lastHash
	// untouched so the path's next event retries instead of being permanently
	// dedup-suppressed. See NewDebouncerWithCommit.
	if d.onSettled(path) {
		d.mu.Lock()
		d.lastHash[path] = hash
		d.mu.Unlock()
	}
}

// maxHashBytes caps the file size hashFile will read into memory for change
// detection. It mirrors the read-before-clobber guard in internal/sync's
// hashFileCapped (maxDestHashBytes = 8 MiB): native text artifacts
// (memory/skill/config files) are tiny, while a continuously-rewritten
// multi-MB/GB file the watcher sees — e.g. Codex's logs_*.sqlite-wal, or a
// large ~/.claude.json — must not be slurped whole on every settle just to
// dedup. Over-cap files are hashed from os.Stat metadata instead (see
// hashFile), so they still flow through to onSettled where handleEvent's
// ignore/size-cap filtering drops them.
const maxHashBytes = 8 << 20

func hashFile(path string) (string, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if fi.Size() > maxHashBytes {
		// Too large to read for a mere change-detection hash. Derive a cheap
		// fingerprint from size+mtime so dedup still suppresses no-op events,
		// while letting genuine changes (size/mtime move) re-fire onSettled.
		return metaHash(fi), nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// metaHash returns a content-independent fingerprint of fi derived from its
// size and modification time. Used by hashFile for over-cap files where a
// full-content read would be wasteful; size+mtime is sufficient to detect
// that an over-cap file changed between settles.
func metaHash(fi os.FileInfo) string {
	h := sha256.New()
	fmt.Fprintf(h, "aplexica-watcher-meta\x00%d\x00%d", fi.Size(), fi.ModTime().UnixNano())
	return hex.EncodeToString(h.Sum(nil))
}
