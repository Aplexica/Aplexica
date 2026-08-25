package truststate

import (
	"fmt"
	"os"
	"time"

	"github.com/aplexica/aplexica/internal/privatefs"
)

const checkpointLockTimeout = 10 * time.Second

type checkpointLock struct {
	file *os.File
	root *privatefs.Root
}

func acquireCheckpointLock(rootPath string) (*checkpointLock, error) {
	root, err := privatefs.OpenRoot(rootPath, privatefs.DirPolicy{Access: privatefs.AccessPrivate})
	if err != nil {
		return nil, fmt.Errorf("open remote plugin checkpoint lock root: %w", err)
	}
	file, err := root.OpenAppendRegular(lockFilename)
	if err != nil {
		_ = root.Close()
		return nil, fmt.Errorf("open no-follow remote plugin checkpoint lock: %w", err)
	}
	if err := lockCheckpointFile(file, checkpointLockTimeout); err != nil {
		_ = file.Close()
		_ = root.Close()
		return nil, fmt.Errorf("lock remote plugin checkpoint: %w", err)
	}
	return &checkpointLock{file: file, root: root}, nil
}

func (lock *checkpointLock) Close() error {
	if lock == nil {
		return nil
	}
	var err error
	if lock.file != nil {
		err = unlockCheckpointFile(lock.file)
		if closeErr := lock.file.Close(); err == nil {
			err = closeErr
		}
	}
	if lock.root != nil {
		if closeErr := lock.root.Close(); err == nil {
			err = closeErr
		}
	}
	return err
}
