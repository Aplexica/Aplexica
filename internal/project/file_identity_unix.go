//go:build unix && !darwin

package project

import (
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

func platformProjectIdentity(path string, expected os.FileInfo) (FileIdentity, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return FileIdentity{}, fmt.Errorf("project: open directory identity: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
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
	stat, ok := opened.Sys().(*syscall.Stat_t)
	if !ok {
		return FileIdentity{}, fmt.Errorf("project: directory identity unavailable")
	}
	return FileIdentity{Platform: "unix", UnixDevice: uint64(stat.Dev), UnixInode: uint64(stat.Ino)}, nil
}
