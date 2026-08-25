package watcher

// Op describes the kind of filesystem event observed by a Source.
type Op uint8

const (
	// OpChange is a unified event type: create, write, or rename-in.
	// v0.5.1 does not distinguish among these — the Debouncer reads the
	// file at the path and the actual change is determined by content
	// hash. A finer-grained taxonomy would add complexity without value
	// at this layer.
	OpChange Op = iota
	// OpRemove indicates the path was deleted or moved out. The Debouncer
	// will fail to hash it and drop the callback.
	OpRemove
)

// Event is a filesystem event observed by a Source.
type Event struct {
	Path string
	Op   Op
}

// Source is a directory-level filesystem notification stream. Implementations
// MUST satisfy the conformance contract defined in source_test.go:
//
//   - After New(dir) succeeds, every subsequent OpChange/OpRemove event for
//     a file directly inside dir MUST surface on Events() within a bounded
//     time (typically <100ms on a non-loaded system).
//   - Events for files in subdirectories of dir MUST NOT surface
//     (non-recursive semantics; recursion is a Watcher-layer concern).
//   - Close() MUST be safe to call multiple times and MUST close both
//     Events() and Errors() channels (after they drain).
//   - Errors() carries non-fatal errors from the underlying OS API
//     (e.g., inotify queue overflow, FSEvents stream drop).
type Source interface {
	Add(dir string) error
	Events() <-chan Event
	Errors() <-chan error
	Close() error
}
