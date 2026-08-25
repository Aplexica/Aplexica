package watcher

import (
	"context"
	"sync"
)

// Watcher drives a Source's event stream into a Debouncer. v0.5.1 splits the
// "watch a directory" responsibility cleanly: Source owns the platform-specific
// OS-level wiring (FSEvents on macOS, inotify on Linux, ReadDirectoryChangesW
// on Windows, fsnotify elsewhere), and Watcher owns the platform-neutral
// "feed events to the debouncer + plumb errors through" logic.
//
// Non-recursive in v0.5.1 (one directory only). Recursive watching is a
// v0.6.0 concern.
type Watcher struct {
	src       Source
	debouncer *Debouncer
	OnError   func(error)
	// ownsSource is true when the Watcher created its own Source (NewWatcher)
	// and is therefore responsible for Closing it. False when the caller
	// supplied a Source via NewWatcherWithSource (caller manages lifetime).
	ownsSource bool
	// closeOnce makes Close idempotent: UnwatchFolder may Close a watcher that
	// the orchestrator's shutdown loop also Closes, and a second src.Close on
	// some platforms errors. closeErr caches the first (and only) result.
	closeOnce sync.Once
	closeErr  error
}

// NewWatcher creates a Watcher for dir using the platform-default Source.
// Callers who want a custom Source (e.g., for testing) should use
// NewWatcherWithSource instead.
func NewWatcher(dir string, debouncer *Debouncer) (*Watcher, error) {
	src, err := New(dir)
	if err != nil {
		return nil, err
	}
	return &Watcher{src: src, debouncer: debouncer, ownsSource: true}, nil
}

// NewWatcherWithSource creates a Watcher backed by a user-supplied Source.
// The Source's lifetime is the caller's responsibility — Close on the returned
// Watcher will NOT close the Source.
func NewWatcherWithSource(src Source, debouncer *Debouncer) *Watcher {
	return &Watcher{src: src, debouncer: debouncer, ownsSource: false}
}

// Run processes events until ctx is cancelled. Returns when ctx.Done fires.
// Caller still must call Close to release the underlying Source (when owned).
func (w *Watcher) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-w.src.Events():
			if !ok {
				return
			}
			w.debouncer.Notify(ev.Path)
		case err, ok := <-w.src.Errors():
			if !ok {
				return
			}
			if w.OnError != nil {
				w.OnError(err)
			}
		}
	}
}

// Close releases the underlying Source if the Watcher owns it. Safe to call
// multiple times. When the Source was supplied via NewWatcherWithSource,
// Close is a no-op and the caller must Close the Source themselves.
func (w *Watcher) Close() error {
	if !w.ownsSource {
		return nil
	}
	w.closeOnce.Do(func() {
		w.closeErr = w.src.Close()
	})
	return w.closeErr
}
