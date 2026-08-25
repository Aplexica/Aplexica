//go:build unix

package project

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

type registryLock struct{ f *os.File }

func acquireRegistryLock(path string, timeout time.Duration) (*registryLock, error) {
	deadline := time.Now().Add(timeout)
	for {
		f, err := openRegistryLockNoFollow(path)
		if err != nil {
			if errors.Is(err, unix.ELOOP) {
				return nil, fmt.Errorf("project: unsafe registry lock object")
			}
			return nil, fmt.Errorf("project: open registry lock: %w", err)
		}
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
			if err := f.Chmod(0o600); err != nil {
				_ = f.Close()
				return nil, err
			}
			return &registryLock{f: f}, nil
		} else if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			_ = f.Close()
			return nil, fmt.Errorf("project: lock registry: %w", err)
		}
		_ = f.Close()
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("project: registry lock timeout")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func openRegistryLockNoFollow(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	if f == nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("retain registry lock descriptor")
	}
	opened, openedErr := f.Stat()
	named, namedErr := os.Lstat(path)
	if openedErr != nil || namedErr != nil || !opened.Mode().IsRegular() || named.Mode()&os.ModeSymlink != 0 ||
		!named.Mode().IsRegular() || !os.SameFile(opened, named) {
		_ = f.Close()
		return nil, fmt.Errorf("unsafe registry lock object")
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

func (l *registryLock) release() { _ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN); _ = l.f.Close() }
