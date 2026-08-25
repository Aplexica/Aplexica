//go:build unix

package daemon

import (
	"fmt"
	"os"
	"syscall"
)

func validateDeadletterInPlaceFile(f *os.File) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() {
		return fmt.Errorf("not a regular file")
	}
	if stat.Nlink != 1 {
		return fmt.Errorf("link count is %d, want 1", stat.Nlink)
	}
	if int(stat.Uid) != os.Geteuid() {
		return fmt.Errorf("owner mismatch")
	}
	return nil
}
