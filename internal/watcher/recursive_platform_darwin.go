//go:build darwin

package watcher

func newPlatformRecursiveSource(dir string) (Source, bool, error) {
	src, err := newDarwinRecursiveFSEventsSource(dir)
	return src, true, err
}
