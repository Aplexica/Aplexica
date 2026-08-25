//go:build windows

package filelock

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/aplexica/aplexica/internal/privatefs"
	"golang.org/x/sys/windows"
)

func openAndLock(path string, timeout time.Duration) (*os.File, error) {
	deadline := time.Now().Add(timeout)
	for {
		root, err := privatefs.OpenRoot(filepath.Dir(path), privatefs.DirPolicy{
			Access:        privatefs.AccessPrivate,
			RepairOwned:   true,
			AllowExisting: true,
		})
		if err != nil {
			return nil, err
		}
		f, err := root.OpenAppendRegularRepair(filepath.Base(path))
		closeErr := root.Close()
		if err != nil {
			return nil, err
		}
		if closeErr != nil {
			_ = f.Close()
			return nil, closeErr
		}
		var ov windows.Overlapped
		err = windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &ov)
		if err == nil {
			return f, nil
		}
		_ = f.Close()
		if !errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return nil, err
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("filelock: timeout")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func unlock(f *os.File) error {
	var ov windows.Overlapped
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &ov)
}
