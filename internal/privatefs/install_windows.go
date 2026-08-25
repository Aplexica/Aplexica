//go:build windows

package privatefs

import (
	"errors"
	"fmt"
	"io/fs"
	"unsafe"

	"golang.org/x/sys/windows"
)

type fileRenameInformation struct {
	ReplaceIfExists uint32
	RootDirectory   windows.Handle
	FileNameLength  uint32
	FileName        [1]uint16
}

func (r *Root) installNoReplace(oldRel, newRel string) error {
	oldName, err := windows.NewNTUnicodeString(oldRel)
	if err != nil {
		return err
	}
	oa := &windows.OBJECT_ATTRIBUTES{
		Length:        uint32(unsafe.Sizeof(windows.OBJECT_ATTRIBUTES{})),
		RootDirectory: windows.Handle(r.dir.Fd()),
		ObjectName:    oldName,
		Attributes:    windows.OBJ_CASE_INSENSITIVE,
	}
	var iosb windows.IO_STATUS_BLOCK
	var allocation int64
	var source windows.Handle
	err = windows.NtCreateFile(&source, windows.FILE_GENERIC_READ|windows.DELETE, oa, &iosb,
		&allocation, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		windows.FILE_OPEN, windows.FILE_NON_DIRECTORY_FILE|windows.FILE_OPEN_REPARSE_POINT, 0, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(source)
	name, err := windows.UTF16FromString(newRel)
	if err != nil {
		return err
	}
	nameBytes := (len(name) - 1) * 2
	var dummy fileRenameInformation
	size := int(unsafe.Offsetof(dummy.FileName)) + nameBytes
	buf := make([]byte, size)
	info := (*fileRenameInformation)(unsafe.Pointer(&buf[0]))
	// FileRenameInformation (the NT API), unlike FileRenameInfoEx (the
	// Win32 API), accepts this root-relative FILE_RENAME_INFORMATION layout.
	// ReplaceIfExists deliberately remains zero: installing identity material
	// must never overwrite a winner created by another process.
	info.RootDirectory = windows.Handle(r.dir.Fd())
	info.FileNameLength = uint32(nameBytes)
	copy((*[windows.MAX_LONG_PATH]uint16)(unsafe.Pointer(&info.FileName[0]))[:nameBytes/2:nameBytes/2], name)
	err = windows.NtSetInformationFile(source, &iosb, &buf[0], uint32(len(buf)), windows.FileRenameInformation)
	if errors.Is(err, windows.STATUS_OBJECT_NAME_COLLISION) {
		return fmt.Errorf("privatefs: install destination exists: %w", fs.ErrExist)
	}
	return err
}
