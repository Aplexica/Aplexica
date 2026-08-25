// Package daemon provides the building blocks for Aplexica's background
// sync daemon: a PID-based lock file (lock.go), structured per-day JSON
// logging (log.go), and a Unix-domain-socket control endpoint with a
// line-delimited JSON protocol (control.go).
//
// Higher-level integration with the syncd.Orchestrator lives in
// cmd/aplexica/cmd_daemon.go.
package daemon

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/aplexica/aplexica/internal/privatefs"
)

const maxPIDFileBytes = 64

// Lock is a PID-file-backed exclusive lock. Acquire returns a Lock when
// the daemon is the sole holder; subsequent Acquires fail until the
// holding process releases or exits (in which case the lock is "stale"
// and the next Acquire takes over).
type Lock struct {
	path string
	root *privatefs.Root
	rel  string
}

// Acquire takes the lock at path. The parent directory is created if
// missing (mode 0o700, matching the daemon's state-dir convention).
//
// Stale-lock semantics: if path already exists and contains a PID that
// is not currently running, Acquire deletes the stale file and proceeds
// to take the lock itself. This matches launchd/systemd behavior — a
// crashed daemon doesn't permanently lock itself out.
//
// Returns an error if the lock is held by a live process.
func Acquire(path string) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("daemon: mkdir lock dir: %w", err)
	}
	root, err := privatefs.OpenRoot(filepath.Dir(path), privatefs.DirPolicy{
		Access:        privatefs.AccessPrivate,
		RepairOwned:   true,
		AllowExisting: true,
	})
	if err != nil {
		return nil, fmt.Errorf("daemon: secure lock dir: %w", err)
	}
	keepRoot := false
	defer func() {
		if !keepRoot {
			_ = root.Close()
		}
	}()
	rel := filepath.Base(path)

	// Best-effort stale-lock check before attempting O_EXCL create.
	if existing, err := readPIDRoot(root, rel); err == nil {
		if !pidAlive(existing) {
			_ = root.RemoveRegular(rel)
		}
	}

	f, err := root.CreateExclusive(rel, privatefs.FilePolicy{RequirePrivateParent: true, RejectWritableByOthers: true})
	if err != nil {
		// Try one more time after a stale-check race.
		if existing, rerr := readPIDRoot(root, rel); rerr == nil && !pidAlive(existing) {
			_ = root.RemoveRegular(rel)
			f, err = root.CreateExclusive(rel, privatefs.FilePolicy{RequirePrivateParent: true, RejectWritableByOthers: true})
		}
		if err != nil {
			holderPID, _ := readPIDRoot(root, rel)
			return nil, fmt.Errorf("daemon: lock %s held by pid %d", path, holderPID)
		}
	}
	defer f.Close()

	if _, err := fmt.Fprintf(f, "%d", os.Getpid()); err != nil {
		_ = root.RemoveRegular(rel)
		return nil, fmt.Errorf("daemon: write pid: %w", err)
	}
	keepRoot = true
	return &Lock{path: path, root: root, rel: rel}, nil
}

// Release removes the lock file. Safe to call multiple times; only the
// first call removes the file.
func (l *Lock) Release() error {
	if l == nil || l.path == "" {
		return nil
	}
	path := l.path
	l.path = ""
	root := l.root
	l.root = nil
	rel := l.rel
	l.rel = ""
	if root == nil {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("daemon: release lock: %w", err)
		}
		return nil
	}
	err := root.RemoveRegular(rel)
	closeErr := root.Close()
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("daemon: release lock: %w", err)
	}
	return closeErr
}

func readPIDRoot(root *privatefs.Root, rel string) (int, error) {
	f, err := root.OpenReadRegularRepair(rel)
	if err != nil {
		return 0, err
	}
	b, err := io.ReadAll(io.LimitReader(f, maxPIDFileBytes))
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return 0, fmt.Errorf("invalid pid file contents: %w", err)
	}
	return pid, nil
}

// pidAlive reports whether pid is currently running. Implementation
// lives in lock_unix.go (signal-0 probe) and lock_windows.go
// (OpenProcess + GetExitCodeProcess) — Go's os.Process.Signal accepts
// only os.Kill on Windows, so the Unix "kill -0" idiom doesn't carry.
