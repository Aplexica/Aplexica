package filelock

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type Lock struct{ f *os.File }

func Acquire(path string, timeout time.Duration) (*Lock, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("filelock: path must be absolute")
	}
	f, err := openAndLock(path, timeout)
	if err != nil {
		return nil, err
	}
	return &Lock{f: f}, nil
}

func (l *Lock) Close() error {
	if l == nil || l.f == nil {
		return nil
	}
	err := unlock(l.f)
	closeErr := l.f.Close()
	l.f = nil
	if err == nil {
		err = closeErr
	}
	return err
}
