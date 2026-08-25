//go:build windows

package project

import (
	"encoding/hex"
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

type windowsFileIDInfo struct {
	VolumeSerialNumber uint64
	FileID             [16]byte
}

func platformProjectIdentity(path string, expected os.FileInfo) (FileIdentity, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return FileIdentity{}, err
	}
	handle, err := windows.CreateFile(name, windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return FileIdentity{}, fmt.Errorf("project: open directory identity: %w", err)
	}
	file := os.NewFile(uintptr(handle), path)
	if file == nil {
		_ = windows.CloseHandle(handle)
		return FileIdentity{}, fmt.Errorf("project: retain directory identity handle")
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.IsDir() || expected == nil || !os.SameFile(expected, opened) {
		return FileIdentity{}, fmt.Errorf("project: directory changed while measuring identity")
	}
	named, err := os.Stat(path)
	if err != nil || !os.SameFile(opened, named) {
		return FileIdentity{}, fmt.Errorf("project: directory changed while measuring identity")
	}
	var identity windowsFileIDInfo
	if err := windows.GetFileInformationByHandleEx(handle, windows.FileIdInfo, (*byte)(unsafe.Pointer(&identity)), uint32(unsafe.Sizeof(identity))); err != nil {
		return FileIdentity{}, fmt.Errorf("project: read directory identity: %w", err)
	}
	return FileIdentity{Platform: "windows", VolumeSerial: identity.VolumeSerialNumber, WindowsFileID: hex.EncodeToString(identity.FileID[:])}, nil
}
