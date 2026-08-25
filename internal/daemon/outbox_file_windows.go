//go:build windows

package daemon

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

func validateDeadletterInPlaceFile(f *os.File) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("not a regular file")
	}
	var byHandle windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(f.Fd()), &byHandle); err != nil {
		return err
	}
	if byHandle.NumberOfLinks != 1 {
		return fmt.Errorf("link count is %d, want 1", byHandle.NumberOfLinks)
	}
	return nil
}
