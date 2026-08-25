package watcher

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// RecursiveSource is a Source that watches a directory tree. Platforms with a
// cheap native recursive source can provide one through newPlatformRecursiveSource;
// otherwise RecursiveSource composes one underlying per-directory Source for
// every directory in the tree.
//
// Implements the Source interface. Conformance with the contract from
// source.go is exercised by TestRecursiveSource_Conformance_* in the test
// file.
//
// v0.6.0 limitations (deferred to later milestones):
//   - Symlink-target validation is naive — filepath.WalkDir follows
//     symlinks unless the user's filesystem prevents it.
//   - No per-directory filtering (hidden dirs, vendor dirs like
//     node_modules) — those are watched too. The cost is duplicated work,
//     not correctness.
type RecursiveSource struct {
	root string

	mu       sync.Mutex
	children map[string]Source // dir → its Source
	closed   bool

	events chan Event
	errors chan error
	done   chan struct{}
	wg     sync.WaitGroup
}

// NewRecursiveSource walks dir, opens one Source per directory found,
// and returns a Source that aggregates them. Callers consume Events()
// and Errors() exactly as they would with any other Source.
//
// On error during the initial walk, every Source created so far is
// closed and the error is returned. Errors are wrapped with "watcher/
// recursive: " prefix.
func NewRecursiveSource(dir string) (*RecursiveSource, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("watcher/recursive: resolve dir: %w", err)
	}
	rs := &RecursiveSource{
		root:     abs,
		children: map[string]Source{},
		events:   make(chan Event, 256),
		errors:   make(chan error, 32),
		done:     make(chan struct{}),
	}

	if src, ok, err := newPlatformRecursiveSource(abs); err != nil {
		return nil, fmt.Errorf("watcher/recursive: open %s: %w", abs, err)
	} else if ok {
		rs.children[abs] = src
		rs.wg.Add(1)
		go rs.fanFrom(src)
	} else {
		if err := rs.walkAndAttach(abs); err != nil {
			rs.closeAllChildren()
			return nil, err
		}
	}

	return rs, nil
}

func (rs *RecursiveSource) Add(dir string) error {
	// V0.6.0 does not extend the watched root after construction.
	// The dynamic-subdir handling (Tasks 2 and 3) attaches new dirs
	// internally as they appear; external Add() is reserved for future use.
	return fmt.Errorf("watcher/recursive: Add not supported in v0.6.0 (recursive watching auto-discovers via the root passed to NewRecursiveSource)")
}

func (rs *RecursiveSource) Events() <-chan Event { return rs.events }
func (rs *RecursiveSource) Errors() <-chan error { return rs.errors }

// Close stops every underlying Source and closes the aggregated channels.
// Safe to call multiple times.
func (rs *RecursiveSource) Close() error {
	rs.mu.Lock()
	if rs.closed {
		rs.mu.Unlock()
		return nil
	}
	rs.closed = true
	close(rs.done)
	children := rs.children
	rs.children = map[string]Source{}
	rs.mu.Unlock()

	for _, src := range children {
		_ = src.Close()
	}
	rs.wg.Wait()
	close(rs.events)
	close(rs.errors)
	return nil
}

// walkAndAttach walks the directory tree rooted at start and attaches
// one Source per directory found. The root itself is attached too.
func (rs *RecursiveSource) walkAndAttach(start string) error {
	return filepath.WalkDir(start, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("watcher/recursive: walk %s: %w", path, err)
		}
		if !d.IsDir() {
			return nil
		}
		// Prune dependency/VCS caches (node_modules, .git, …). Returning
		// SkipDir for the start dir itself stops the whole walk, so a dynamic
		// attach triggered by a newly-created node_modules/ attaches nothing.
		if SkipWalkDir(d.Name()) {
			return fs.SkipDir
		}
		return rs.attachDir(path)
	})
}

// attachDir creates a fresh Source for dir (using the platform-default New)
// and starts a goroutine fanning its events and errors into the aggregated
// output channels. Safe to call multiple times — duplicates are ignored.
func (rs *RecursiveSource) attachDir(dir string) error {
	rs.mu.Lock()
	if rs.closed {
		rs.mu.Unlock()
		return fmt.Errorf("watcher/recursive: attach after close")
	}
	if _, ok := rs.children[dir]; ok {
		rs.mu.Unlock()
		return nil
	}
	src, err := New(dir)
	if err != nil {
		rs.mu.Unlock()
		return fmt.Errorf("watcher/recursive: open %s: %w", dir, err)
	}
	rs.children[dir] = src
	rs.mu.Unlock()

	rs.wg.Add(1)
	go rs.fanFrom(src)
	return nil
}

// fanFrom relays one child Source's events and errors into the aggregated
// channels until either the child closes its channels or the parent's done
// signal fires.
//
// Dynamic attachment: when an OpChange event refers to a path that is a
// directory, fanFrom walks-and-attaches the new subtree before forwarding
// the event. When an OpRemove event refers to a tracked directory, fanFrom
// detaches the child Source.
func (rs *RecursiveSource) fanFrom(src Source) {
	defer rs.wg.Done()
	for {
		select {
		case <-rs.done:
			return
		case ev, ok := <-src.Events():
			if !ok {
				return
			}
			switch ev.Op {
			case OpChange:
				if isDir(ev.Path) {
					if err := rs.walkAndAttach(ev.Path); err != nil {
						select {
						case rs.errors <- fmt.Errorf("watcher/recursive: attach new subtree %s: %w", ev.Path, err):
						case <-rs.done:
							return
						}
					}
					// Announce files already inside the just-attached
					// subtree. The typical creation sequence is mkdir
					// immediately followed by a file write (every agent
					// creates skills as <name>/SKILL.md this way); the
					// write lands BEFORE the new dir's Source starts
					// watching, so without this the file is silently
					// missed until its next touch.
					// Files the orchestrator itself just fanned out are
					// re-announced too — the recursion guard + destHashes
					// content check classify those as echoes and drop
					// them, exactly as for a real watch event.
					if !rs.announceFiles(ev.Path) {
						return
					}
				}
			case OpRemove:
				rs.detachIfTracked(ev.Path)
			}
			select {
			case rs.events <- ev:
			case <-rs.done:
				return
			}
		case err, ok := <-src.Errors():
			if !ok {
				return
			}
			select {
			case rs.errors <- err:
			case <-rs.done:
				return
			}
		}
	}
}

// announceFiles emits a synthetic OpChange for every regular file under dir
// (recursively, pruning the same cache dirs as walkAndAttach). Used after a
// dynamic subtree attach so files written before the new Sources started
// watching still reach the pipeline. Returns false when the source is done
// (caller should stop).
func (rs *RecursiveSource) announceFiles(dir string) bool {
	ok := true
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // best-effort: a vanished file is simply not announced
		}
		if d.IsDir() {
			if SkipWalkDir(d.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		select {
		case rs.events <- Event{Path: path, Op: OpChange}:
			return nil
		case <-rs.done:
			ok = false
			return fs.SkipAll
		}
	})
	return ok
}

// detachIfTracked closes the child Source for path (if one exists) and
// removes it from the children map. Safe to call for paths that aren't
// tracked — that's a no-op. Also recursively detaches descendants of
// path, since deleting a subdirectory implicitly deletes everything below it.
func (rs *RecursiveSource) detachIfTracked(path string) {
	rs.mu.Lock()
	if rs.closed {
		rs.mu.Unlock()
		return
	}
	toClose := []Source{}
	prefix := path + string(filepath.Separator)
	for dir, src := range rs.children {
		if dir == path || strings.HasPrefix(dir, prefix) {
			toClose = append(toClose, src)
			delete(rs.children, dir)
		}
	}
	rs.mu.Unlock()
	for _, src := range toClose {
		_ = src.Close()
	}
}

// closeAllChildren is the error-path cleanup helper used by
// NewRecursiveSource when the initial walk fails partway through.
func (rs *RecursiveSource) closeAllChildren() {
	rs.mu.Lock()
	children := rs.children
	rs.children = map[string]Source{}
	rs.mu.Unlock()
	for _, src := range children {
		_ = src.Close()
	}
}

// isDir reports whether path exists and is a directory. Errors (path
// doesn't exist, permission denied) return false.
func isDir(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return info.IsDir()
}
