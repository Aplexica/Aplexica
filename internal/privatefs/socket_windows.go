//go:build windows

package privatefs

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

// HardenOwnedPrivateSocket protects the filesystem node backing an AF_UNIX
// listener. Windows ignores POSIX chmod bits, so the socket must receive the
// same current-user/SYSTEM/Administrators protected DACL as private files.
func HardenOwnedPrivateSocket(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("privatefs: expected socket node")
	}
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	owned, err := ownerIsCurrent(sd)
	if err != nil || !owned {
		return fmt.Errorf("privatefs: socket owner mismatch")
	}
	if err := repairNamedPrivate(path); err != nil {
		return err
	}
	sd, err = windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	return validatePrivateDescriptor(sd, AccessPrivate)
}
