//go:build windows

package secureexec

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/aplexica/aplexica/internal/privatefs"
	"golang.org/x/sys/windows"
)

func validatePlatformLaunchPath(string) error { return nil }

func validateInstalledRemotePluginPlatform(string, string, string) error { return nil }

func preparePlatformCommand(ctx context.Context, path string, expected [32]byte, input privatefs.TrustedInput, args []string) (*exec.Cmd, []*os.File, error) {
	resources, file, err := lockWindowsExecutablePath(path)
	if err != nil {
		return nil, resources, err
	}
	info, err := file.Stat()
	if err != nil || !os.SameFile(info, input.Info) {
		return nil, resources, errors.New("secureexec: executable identity changed before Windows launch lock")
	}
	hash := sha256.New()
	read, err := io.Copy(hash, io.LimitReader(file, maxExecutableBytes+1))
	if err != nil || read != int64(len(input.Bytes)) || read == 0 || read > maxExecutableBytes {
		return nil, resources, errors.New("secureexec: retained Windows executable has invalid size")
	}
	var actual [32]byte
	copy(actual[:], hash.Sum(nil))
	if actual != expected {
		return nil, resources, errors.New("secureexec: retained Windows executable digest mismatch")
	}
	current, err := os.Lstat(path)
	if err != nil || !os.SameFile(current, info) {
		return nil, resources, errors.New("secureexec: Windows executable pathname identity changed under launch lock")
	}
	return exec.CommandContext(ctx, path, args...), resources, nil
}

func lockWindowsExecutablePath(path string) ([]*os.File, *os.File, error) {
	volume := filepath.VolumeName(path)
	relative := strings.TrimPrefix(path, volume+string(filepath.Separator))
	parts := strings.Split(relative, string(filepath.Separator))
	if volume == "" || len(parts) == 0 {
		return nil, nil, errors.New("secureexec: Windows executable path is invalid")
	}
	resources := make([]*os.File, 0, len(parts))
	current := volume + string(filepath.Separator)
	for index, part := range parts {
		if part == "" || part == "." || part == ".." {
			return resources, nil, errors.New("secureexec: Windows executable path has an unsafe component")
		}
		current = filepath.Join(current, part)
		final := index == len(parts)-1
		file, err := openWindowsLaunchLock(current, !final)
		if err != nil {
			return resources, nil, err
		}
		resources = append(resources, file)
		if final {
			return resources, file, nil
		}
	}
	return resources, nil, errors.New("secureexec: Windows executable path has no final component")
}

func openWindowsLaunchLock(path string, directory bool) (*os.File, error) {
	pointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	desired := uint32(windows.FILE_READ_ATTRIBUTES | windows.SYNCHRONIZE)
	flags := uint32(windows.FILE_FLAG_OPEN_REPARSE_POINT)
	if directory {
		flags |= windows.FILE_FLAG_BACKUP_SEMANTICS
	} else {
		desired |= windows.GENERIC_READ
	}
	// Omitting FILE_SHARE_WRITE and FILE_SHARE_DELETE prevents in-place writes,
	// replacement, or ancestor rename until CreateProcess has resolved path.
	handle, err := windows.CreateFile(pointer, desired, windows.FILE_SHARE_READ, nil, windows.OPEN_EXISTING, flags, 0)
	if err != nil {
		return nil, fmt.Errorf("secureexec: acquire Windows launch lock for %s: %w", path, err)
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil || info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || (directory && info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0) || (!directory && info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0) {
		_ = windows.CloseHandle(handle)
		return nil, fmt.Errorf("secureexec: Windows launch path has an invalid type or reparse point: %s", path)
	}
	return os.NewFile(uintptr(handle), path), nil
}
