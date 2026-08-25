//go:build unix

package privatefs

import (
	"fmt"
	"os"
)

func HardenOwnedPrivateSocket(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSocket == 0 || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("privatefs: expected socket node")
	}
	return os.Chmod(path, 0o600)
}
