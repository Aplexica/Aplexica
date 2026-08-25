package watcher

import (
	"fmt"
	"path/filepath"
	"sync"

	"github.com/fsnotify/fsnotify"
)

// newFsnotifySource returns a Source backed by github.com/fsnotify/fsnotify.
// Used on platforms without a native watcher implementation
// and as the fallback for non-V1 platforms (source_default.go).
func newFsnotifySource(dir string) (Source, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("watcher/fsnotify: resolve dir: %w", err)
	}
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("watcher/fsnotify: new: %w", err)
	}
	if err := w.Add(abs); err != nil {
		w.Close()
		return nil, fmt.Errorf("watcher/fsnotify: add %s: %w", abs, err)
	}
	s := &fsnotifySource{
		dir:    abs,
		w:      w,
		events: make(chan Event, 64),
		errors: make(chan error, 8),
		done:   make(chan struct{}),
	}
	go s.loop()
	return s, nil
}

type fsnotifySource struct {
	dir    string
	w      *fsnotify.Watcher
	events chan Event
	errors chan error
	done   chan struct{}
	// closeOnce makes Close() idempotent: a non-atomic check-then-close on
	// done lets two concurrent Close calls both close(done) -> panic
	// "close of closed channel". Mirrors the native Sources (source_linux.go
	// / source_windows.go).
	closeOnce sync.Once
}

func (s *fsnotifySource) Add(dir string) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	if err := s.w.Add(abs); err != nil {
		return fmt.Errorf("watcher/fsnotify: add %s: %w", abs, err)
	}
	s.dir = abs
	return nil
}

func (s *fsnotifySource) Events() <-chan Event { return s.events }
func (s *fsnotifySource) Errors() <-chan error { return s.errors }

func (s *fsnotifySource) Close() error {
	var err error
	s.closeOnce.Do(func() {
		close(s.done)
		err = s.w.Close()
	})
	return err
}

func (s *fsnotifySource) loop() {
	defer close(s.events)
	defer close(s.errors)
	for {
		select {
		case <-s.done:
			return
		case ev, ok := <-s.w.Events:
			if !ok {
				return
			}
			if filepath.Dir(ev.Name) != s.dir {
				continue
			}
			if ev.Op == fsnotify.Chmod {
				continue
			}
			op := OpChange
			if ev.Op&fsnotify.Remove != 0 || ev.Op&fsnotify.Rename != 0 {
				op = OpRemove
			}
			select {
			case s.events <- Event{Path: ev.Name, Op: op}:
			case <-s.done:
				return
			}
		case err, ok := <-s.w.Errors:
			if !ok {
				return
			}
			select {
			case s.errors <- err:
			case <-s.done:
				return
			}
		}
	}
}
