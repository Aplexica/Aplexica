//go:build windows

package project

import (
	"golang.org/x/sys/windows"
)

func syncRegistryParent(path string) error {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	// FlushFileBuffers requires GENERIC_WRITE even when the handle names a
	// directory. The state directory is private and owned by this process's
	// user, so failure to obtain that right is a durability failure rather than
	// a condition we may silently ignore.
	handle, err := windows.CreateFile(name, windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(handle)
	return windows.FlushFileBuffers(handle)
}
