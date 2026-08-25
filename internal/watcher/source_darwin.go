//go:build darwin

package watcher

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsevents"
)

// darwinFSEventsSource implements Source via macOS's FSEvents framework,
// supplemented by a stat-polling layer for sub-second precision per
// BRD-03 §4.3. FSEvents delivers events with a configurable latency
// (default coalescing window ~100-1000ms); stat polling fills the gap
// when the user is actively interacting with a file we care about.
//
// Non-recursive in v0.5.1 — FSEvents is inherently recursive, so we
// filter events whose parent isn't the watched dir.
type darwinFSEventsSource struct {
	dir       string
	recursive bool
	stream    *fsevents.EventStream
	events    chan Event
	errors    chan error
	done      chan struct{}
	wg        sync.WaitGroup // tracks background goroutines

	// stat-poll state
	pollCadence time.Duration
	mu          sync.Mutex
	known       map[string]fileFingerprint // path -> last observed (size, mtime)
}

// darwinStatPollCadence is how often the stat-polling supplement scans a flat
// watched directory for changes FSEvents may have dropped entirely.
//
// It is a BACKSTOP, not the fast path: FSEvents (50ms latency) already
// delivers sub-second change notifications per BRD-03 §4.3, and the
// Debouncer downstream gates every change behind a 500ms quiet period — so
// polling faster than the quiet period yields no lower end-to-end latency.
// A tight cadence only multiplies the scan cost: under a recursive watch of
// a large agent history (hundreds of directories), each tick walks every
// watched dir. At 100ms that was ~10 full-tree scans/sec and held ~0.4 of a
// core busy at idle; 1s keeps the daemon near-idle while still catching a
// dropped FSEvents event well inside a second poll. Each scan reads only
// directory-entry metadata (size + mtime), so its cost is O(file count), not
// O(file size).
const darwinStatPollCadence = 1 * time.Second

// Recursive FSEvents roots are usually agent histories with thousands of files
// under date/project subdirectories. FSEvents itself remains the real-time
// path; the recursive stat poll is only a last-ditch safety net, and the daemon
// also runs a focused native-root scanner for near-real-time catch-up.
const darwinRecursiveStatPollCadence = 1 * time.Minute

// New returns the macOS-native Source on darwin.
func New(dir string) (Source, error) {
	return newDarwinFSEventsSource(dir)
}

func newDarwinFSEventsSource(dir string) (Source, error) {
	return newDarwinFSEventsSourceMode(dir, false)
}

func newDarwinRecursiveFSEventsSource(dir string) (Source, error) {
	return newDarwinFSEventsSourceMode(dir, true)
}

func newDarwinFSEventsSourceMode(dir string, recursive bool) (Source, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("watcher/darwin: resolve dir: %w", err)
	}
	// FSEvents emits paths in their canonical (symlink-resolved) form. On
	// macOS, /var, /tmp, /etc are symlinks to /private/var, /private/tmp,
	// /private/etc. If the caller supplies a path under one of these (e.g.
	// t.TempDir() returns /var/folders/...), our stored s.dir would not
	// match the parent of any FSEvents-emitted event path and the
	// filepath.Dir(ev.Path) != s.dir filter in runEventLoop would drop
	// every event. EvalSymlinks normalizes to match what FSEvents emits.
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("watcher/darwin: resolve symlinks: %w", err)
	}
	abs = resolved
	if _, err := os.Stat(abs); err != nil {
		return nil, fmt.Errorf("watcher/darwin: stat dir: %w", err)
	}

	pollCadence := darwinStatPollCadence
	if recursive {
		pollCadence = darwinRecursiveStatPollCadence
	}

	s := &darwinFSEventsSource{
		dir:         abs,
		recursive:   recursive,
		events:      make(chan Event, 64),
		errors:      make(chan error, 8),
		done:        make(chan struct{}),
		pollCadence: pollCadence,
		known:       map[string]fileFingerprint{},
	}

	// Do NOT set Device — when Device is set FSEvents returns paths relative
	// to the device root, which breaks our filepath.Dir filtering. Omitting
	// Device causes FSEvents to use absolute paths directly.
	s.stream = &fsevents.EventStream{
		Paths:   []string{abs},
		Latency: 50 * time.Millisecond,
		Flags:   fsevents.FileEvents | fsevents.WatchRoot,
	}
	if err := s.stream.Start(); err != nil {
		return nil, fmt.Errorf("watcher/darwin: start stream: %w", err)
	}

	// Seed the fingerprint cache with the current directory state so the
	// first poll doesn't fire spurious OpChange events for pre-existing files.
	s.seedKnownFingerprints()

	s.wg.Add(2)
	go s.runEventLoop()
	go s.runStatPolling()
	return s, nil
}

func (s *darwinFSEventsSource) Add(dir string) error {
	// Single-directory in v0.5.1; second Add returns an error to surface misuse.
	return fmt.Errorf("watcher/darwin: Source supports only the directory passed to New (v0.5.1 limitation)")
}

func (s *darwinFSEventsSource) Events() <-chan Event { return s.events }
func (s *darwinFSEventsSource) Errors() <-chan error { return s.errors }

func (s *darwinFSEventsSource) Close() error {
	select {
	case <-s.done:
		return nil
	default:
	}
	close(s.done)
	if s.stream != nil {
		s.stream.Stop()
	}
	// Wait for background goroutines to exit, then close channels.
	// This avoids the race between Close and goroutines still sending.
	go func() {
		s.wg.Wait()
		close(s.events)
		close(s.errors)
	}()
	return nil
}

func (s *darwinFSEventsSource) runEventLoop() {
	defer s.wg.Done()
	for {
		select {
		case <-s.done:
			return
		case msg, ok := <-s.stream.Events:
			if !ok {
				return
			}
			for _, ev := range msg {
				path, ok := s.eventPath(ev)
				if !ok {
					continue
				}
				op := OpChange
				if ev.Flags&fsevents.ItemRemoved != 0 {
					op = OpRemove
				}
				select {
				case s.events <- Event{Path: path, Op: op}:
				case <-s.done:
					return
				}
			}
		}
	}
}

func (s *darwinFSEventsSource) eventPath(ev fsevents.Event) (string, bool) {
	if !s.recursive {
		if filepath.Dir(ev.Path) != s.dir {
			return "", false
		}
		return ev.Path, true
	}
	if ev.Path != s.dir && !strings.HasPrefix(ev.Path, s.dir+string(filepath.Separator)) {
		return "", false
	}
	if ev.Flags&fsevents.ItemIsDir != 0 {
		return "", false
	}
	if ev.Flags&fsevents.ItemIsFile != 0 || ev.Flags&fsevents.ItemRemoved != 0 {
		return ev.Path, true
	}
	info, err := os.Stat(ev.Path)
	if err != nil || info.IsDir() {
		return "", false
	}
	return ev.Path, true
}

// runStatPolling is the FSEvents backstop. Every pollCadence it fingerprints
// every file directly in the watched dir (size + mtime) and emits OpChange /
// OpRemove for any fingerprint that changed or vanished since the last poll.
// This catches changes FSEvents dropped entirely; FSEvents remains the fast
// path for everything else.
func (s *darwinFSEventsSource) runStatPolling() {
	defer s.wg.Done()
	ticker := time.NewTicker(s.pollCadence)
	defer ticker.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-ticker.C:
			s.pollOnce()
		}
	}
}

func (s *darwinFSEventsSource) pollOnce() {
	current := s.fingerprint()

	s.mu.Lock()
	prev := s.known
	s.known = current
	s.mu.Unlock()

	for _, ev := range diffFingerprints(prev, current) {
		select {
		case s.events <- ev:
		case <-s.done:
			return
		}
	}
}

func (s *darwinFSEventsSource) seedKnownFingerprints() {
	known := s.fingerprint()
	s.mu.Lock()
	s.known = known
	s.mu.Unlock()
}

func (s *darwinFSEventsSource) fingerprint() map[string]fileFingerprint {
	if s.recursive {
		return fingerprintTree(s.dir)
	}
	return fingerprintDir(s.dir)
}
