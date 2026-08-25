//go:build !darwin && !linux && !windows

package watcher

// New returns a fsnotify-backed Source for platforms without a native
// implementation in v0.5.1 (everything except darwin/linux/windows).
func New(dir string) (Source, error) {
	return newFsnotifySource(dir)
}
