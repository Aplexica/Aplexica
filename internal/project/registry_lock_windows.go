//go:build windows

package project

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/aplexica/aplexica/internal/privatefs"
	"golang.org/x/sys/windows"
)

type registryLock struct{ f *os.File }

func acquireRegistryLock(path string, timeout time.Duration) (*registryLock, error) {
	deadline := time.Now().Add(timeout)
	for {
		f, err := openRegistryLockNoFollow(path)
		if err != nil {
			return nil, fmt.Errorf("project: open registry lock: %w", err)
		}
		var ov windows.Overlapped
		err = windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &ov)
		if err == nil {
			return &registryLock{f: f}, nil
		}
		_ = f.Close()
		if !errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return nil, fmt.Errorf("project: lock registry: %w", err)
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("project: registry lock timeout")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func openRegistryLockNoFollow(path string) (*os.File, error) {
	root, err := privatefs.OpenRoot(filepath.Dir(path), privatefs.DirPolicy{
		Access:        privatefs.AccessPrivate,
		RepairOwned:   true,
		AllowExisting: true,
	})
	if err != nil {
		return nil, err
	}
	defer root.Close()
	f, err := root.OpenAppendRegularRepair(filepath.Base(path))
	if err != nil {
		if info, statErr := os.Lstat(path); statErr == nil && info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("unsafe registry lock object: %w", err)
		}
		return nil, err
	}
	return f, nil
}

func (l *registryLock) release() {
	var ov windows.Overlapped
	_ = windows.UnlockFileEx(windows.Handle(l.f.Fd()), 0, 1, 0, &ov)
	_ = l.f.Close()
}
