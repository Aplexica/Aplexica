//go:build windows

package projectdiscovery

import (
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

type windowsFileIDInfo struct {
	VolumeSerialNumber uint64
	FileID             [16]byte
}

func fileIdentityFromHandle(f *os.File, info os.FileInfo) (FileIdentity, error) {
	if f == nil || info == nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return FileIdentity{}, fmt.Errorf("projectdiscovery: invalid Windows directory handle")
	}
	var id windowsFileIDInfo
	if err := windows.GetFileInformationByHandleEx(windows.Handle(f.Fd()), windows.FileIdInfo,
		(*byte)(unsafe.Pointer(&id)), uint32(unsafe.Sizeof(id))); err != nil {
		return FileIdentity{}, fmt.Errorf("projectdiscovery: read Windows file identity: %w", err)
	}
	if id.VolumeSerialNumber == 0 || id.FileID == ([16]byte{}) {
		return FileIdentity{}, fmt.Errorf("projectdiscovery: unavailable Windows file identity")
	}
	return FileIdentity{Platform: "windows", VolumeSerial: id.VolumeSerialNumber, WindowsFileID: id.FileID}, nil
}
