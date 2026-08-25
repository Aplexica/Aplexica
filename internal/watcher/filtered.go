package watcher

import "sync"

// FilteredSource wraps a Source and forwards only events whose path passes
// the allow predicate. Used to watch a SINGLE FILE that lives in a directory
// the daemon must not watch wholesale (e.g. ~/.claude.json at the HOME root:
// a flat $HOME watch would be noisy and grant the owning adapter spurious
// path ownership over unrelated dotfiles).
type FilteredSource struct {
	inner  Source
	allow  func(path string) bool
	events chan Event
	done   chan struct{}
	// closeOnce makes Close() idempotent: a non-atomic check-then-close on
	// done lets two concurrent Close calls both close(done) -> panic
	// "close of closed channel". Mirrors the native Sources (source_linux.go
	// / source_windows.go).
	closeOnce sync.Once
}

// filteredEventBuffer matches the per-Source event buffering of the platform
// sources the filter wraps.
const filteredEventBuffer = 64

// NewFilteredSource starts forwarding from inner. Closing the returned
// source closes inner.
func NewFilteredSource(inner Source, allow func(path string) bool) *FilteredSource {
	fs := &FilteredSource{
		inner:  inner,
		allow:  allow,
		events: make(chan Event, filteredEventBuffer),
		done:   make(chan struct{}),
	}
	go fs.run()
	return fs
}

func (f *FilteredSource) run() {
	defer close(f.events)
	for {
		select {
		case <-f.done:
			return
		case ev, ok := <-f.inner.Events():
			if !ok {
				return
			}
			if !f.allow(ev.Path) {
				continue
			}
			select {
			case f.events <- ev:
			case <-f.done:
				return
			}
		}
	}
}

func (f *FilteredSource) Events() <-chan Event { return f.events }
func (f *FilteredSource) Errors() <-chan error { return f.inner.Errors() }

func (f *FilteredSource) Add(dir string) error { return f.inner.Add(dir) }

func (f *FilteredSource) Close() error {
	var err error
	f.closeOnce.Do(func() {
		close(f.done)
		err = f.inner.Close()
	})
	return err
}
