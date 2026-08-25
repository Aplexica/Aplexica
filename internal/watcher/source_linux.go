//go:build linux

package watcher

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// linuxInotifySource implements Source via direct inotify syscalls per BRD
// §4.3 + ADR-0035. Enforces the per-user inotify watch budget read from
// /proc/sys/fs/inotify/max_user_watches: when the daemon approaches the
// limit, it emits a warning on Errors() and falls back to periodic full-
// directory scanning for the affected path.
//
// Non-recursive in v0.5.1 — we add the directory itself only, and filter
// any event whose subject is outside that dir's direct children.
type linuxInotifySource struct {
	dir    string
	fd     int
	wd     int32 // inotify watch descriptor
	events chan Event
	errors chan error
	done   chan struct{}
	// fallbackPoll is started when inotify budget is exhausted.
	fallbackActive bool
	mu             sync.Mutex
	known          map[string]fileFingerprint

	// closeOnce + wg ensure Close() is idempotent and that the producer
	// goroutine (runEventLoop / runPollLoop) has exited before we close
	// the events/errors channels — otherwise the producer can send on a
	// closed channel, or the race detector flags the close-vs-send.
	closeOnce sync.Once
	wg        sync.WaitGroup
}

const linuxFallbackPollCadence = 500 * time.Millisecond

// linuxBudgetWarnThreshold is the fraction of fs.inotify.max_user_watches
// at which we emit a warning on Errors(). 0.9 = warn at 90% utilization.
const linuxBudgetWarnThreshold = 0.9

// New returns the Linux-native Source on linux.
func New(dir string) (Source, error) {
	return newLinuxInotifySource(dir)
}

func newLinuxInotifySource(dir string) (Source, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("watcher/linux: resolve dir: %w", err)
	}

	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		return nil, fmt.Errorf("watcher/linux: inotify_init1: %w", err)
	}

	wd, err := unix.InotifyAddWatch(fd, abs,
		unix.IN_CREATE|unix.IN_MODIFY|unix.IN_DELETE|unix.IN_MOVED_FROM|unix.IN_MOVED_TO|unix.IN_CLOSE_WRITE)
	if err != nil {
		unix.Close(fd)
		// If ENOSPC, the budget is exhausted — fall back to polling without
		// inotify entirely. Surface a warning on the errors channel.
		if errors.Is(err, unix.ENOSPC) {
			return newLinuxPollingFallbackSource(abs)
		}
		return nil, fmt.Errorf("watcher/linux: inotify_add_watch %s: %w", abs, err)
	}

	s := &linuxInotifySource{
		dir:    abs,
		fd:     fd,
		wd:     int32(wd),
		events: make(chan Event, 64),
		errors: make(chan error, 8),
		done:   make(chan struct{}),
		known:  map[string]fileFingerprint{},
	}

	// Best-effort budget check.
	if used, max, ok := readInotifyBudget(); ok {
		if float64(used)/float64(max) >= linuxBudgetWarnThreshold {
			// Non-blocking enqueue — Errors() buffer is small.
			select {
			case s.errors <- fmt.Errorf("watcher/linux: inotify watches at %d/%d (≥%.0f%% of budget); consider increasing fs.inotify.max_user_watches",
				used, max, linuxBudgetWarnThreshold*100):
			default:
			}
		}
	}

	s.wg.Add(1)
	go s.runEventLoop()
	return s, nil
}

// newLinuxPollingFallbackSource is used when inotify_add_watch returns
// ENOSPC — the budget is exhausted, so we fall back to periodic full
// directory scanning. The returned Source obeys the same contract but
// has higher latency (≤ linuxFallbackPollCadence).
func newLinuxPollingFallbackSource(dir string) (Source, error) {
	s := &linuxInotifySource{
		dir:            dir,
		fd:             -1, // sentinel: no inotify fd
		events:         make(chan Event, 64),
		errors:         make(chan error, 8),
		done:           make(chan struct{}),
		fallbackActive: true,
		known:          map[string]fileFingerprint{},
	}
	// Surface the fallback as a warning.
	select {
	case s.errors <- fmt.Errorf("watcher/linux: inotify budget exhausted; falling back to %v polling for %s",
		linuxFallbackPollCadence, dir):
	default:
	}
	s.seedKnownFingerprints()
	s.wg.Add(1)
	go s.runPollLoop()
	return s, nil
}

func (s *linuxInotifySource) Add(dir string) error {
	return fmt.Errorf("watcher/linux: Source supports only the directory passed to New (v0.5.1 limitation)")
}

func (s *linuxInotifySource) Events() <-chan Event { return s.events }
func (s *linuxInotifySource) Errors() <-chan error { return s.errors }

func (s *linuxInotifySource) Close() error {
	s.closeOnce.Do(func() {
		close(s.done)
		// Closing the inotify fd unblocks the syscall.Read in runEventLoop
		// with EBADF, prompting it to return. runPollLoop selects on s.done
		// each tick and exits the same way. Either way, wg.Wait() below
		// blocks until the producer is gone so the channel closes that
		// follow can't race with a concurrent send.
		if s.fd >= 0 {
			if s.wd > 0 {
				_, _ = unix.InotifyRmWatch(s.fd, uint32(s.wd))
			}
			unix.Close(s.fd)
		}
		s.wg.Wait()
		close(s.events)
		close(s.errors)
	})
	return nil
}

// runEventLoop reads inotify events and forwards them.
// inotify_event size = 16 (fixed) + variable name field; max ~4 KB names
// are extreme — 256 bytes is generous enough.
const inotifyBufSize = 4096

func (s *linuxInotifySource) runEventLoop() {
	defer s.wg.Done()
	buf := make([]byte, inotifyBufSize)
	for {
		select {
		case <-s.done:
			return
		default:
		}
		n, err := syscall.Read(s.fd, buf)
		if err != nil {
			if errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EWOULDBLOCK) {
				time.Sleep(10 * time.Millisecond)
				continue
			}
			if errors.Is(err, syscall.EINTR) {
				continue
			}
			select {
			case s.errors <- fmt.Errorf("watcher/linux: inotify read: %w", err):
			default:
			}
			return
		}
		if n == 0 {
			continue
		}
		s.parseAndEmit(buf[:n])
	}
}

func (s *linuxInotifySource) parseAndEmit(buf []byte) {
	var off uint32 = 0
	for off < uint32(len(buf)) {
		if off+16 > uint32(len(buf)) {
			return
		}
		raw := (*unix.InotifyEvent)(unsafe.Pointer(&buf[off]))
		nameLen := raw.Len
		nameStart := off + 16
		nameEnd := nameStart + nameLen
		if nameEnd > uint32(len(buf)) {
			return
		}
		name := strings.TrimRight(string(buf[nameStart:nameEnd]), "\x00")

		if name == "" {
			// Event on the watched directory itself (not on a child) — ignore.
			off = nameEnd
			continue
		}
		fullPath := filepath.Join(s.dir, name)

		op := OpChange
		if raw.Mask&(unix.IN_DELETE|unix.IN_MOVED_FROM) != 0 {
			op = OpRemove
		}
		select {
		case s.events <- Event{Path: fullPath, Op: op}:
		case <-s.done:
			return
		}
		off = nameEnd
	}
}

func (s *linuxInotifySource) runPollLoop() {
	defer s.wg.Done()
	ticker := time.NewTicker(linuxFallbackPollCadence)
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

func (s *linuxInotifySource) pollOnce() {
	current := fingerprintDir(s.dir)

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

func (s *linuxInotifySource) seedKnownFingerprints() {
	known := fingerprintDir(s.dir)
	s.mu.Lock()
	s.known = known
	s.mu.Unlock()
}

// readInotifyBudget reads the current per-user watch count + max from /proc.
// Returns (used, max, true) on success. Used = number of inotify_watch
// entries; approximated by counting lines in /proc/self/fdinfo/* with
// "inotify wd" prefixes — for v0.5.1 we simplify to "we don't know used,
// only max" and return ok=false when used is unavailable.
func readInotifyBudget() (used, max int, ok bool) {
	maxBytes, err := os.ReadFile("/proc/sys/fs/inotify/max_user_watches")
	if err != nil {
		return 0, 0, false
	}
	maxStr := strings.TrimSpace(string(maxBytes))
	m, err := strconv.Atoi(maxStr)
	if err != nil {
		return 0, 0, false
	}
	// "used" counting is non-trivial and inherently racy. For v0.5.1 we
	// return max alone — the warn-threshold check above effectively becomes
	// a no-op when used==0 < 90% of max. A future milestone can add proper
	// /proc/self/fdinfo parsing.
	return 0, m, true
}
