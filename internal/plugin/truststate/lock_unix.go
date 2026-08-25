//go:build !windows

package truststate

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
)

func lockCheckpointFile(file *os.File, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func unlockCheckpointFile(file *os.File) error {
	return syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
}
