//go:build !darwin

package watcher

func newPlatformRecursiveSource(dir string) (Source, bool, error) {
	return nil, false, nil
}
