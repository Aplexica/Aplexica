//go:build windows

package privatefs

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

func syncDirectoryHandle(f *os.File) error {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(f.Fd()), &info); err != nil {
		return err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 || info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return fmt.Errorf("privatefs: sync target is not a real directory")
	}
	// Windows documents FlushFileBuffers only for handles with GENERIC_WRITE
	// and does not list directory handles as supported. File contents are
	// flushed before metadata installation; validating the retained directory
	// handle is the strongest portable directory-sync boundary on Windows.
	return nil
}
