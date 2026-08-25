package adapter

import "time"

// ProjectPresence is one working directory an agent has been run in,
// with the most-recent session timestamp observed for it.
type ProjectPresence struct {
	Path       string
	LastActive time.Time
}

// ProjectDirSource is implemented by adapters that can report the
// working directories the agent has been run in, parsed from the
// agent's own session store. Optional — discovered via type assertion;
// absence means the adapter contributes no project dirs.
type ProjectDirSource interface {
	ProjectDirs() ([]ProjectPresence, error)
}

// NewerPresence returns whichever presence has the later LastActive.
func NewerPresence(a, b ProjectPresence) ProjectPresence {
	if b.LastActive.After(a.LastActive) {
		return b
	}
	return a
}
