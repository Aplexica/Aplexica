package project

import (
	"fmt"
	"os"
	"syscall"
)

// platformProjectIdentity deliberately uses metadata-only lstat checks on
// macOS. Opening a directory below a privacy-protected location (Documents,
// Desktop, or Downloads) from a background LaunchAgent can be suspended by
// TCC indefinitely while the identical open succeeds from Terminal. Registry
// reads happen on the daemon's startup path, so an unbounded open prevents the
// control socket and tray from ever becoming healthy.
//
// The registry's security property is unchanged: authorization is bound to
// the directory's device/inode identity, symlinks are rejected, and the name
// is re-measured to detect replacement during the check. Callers never retain
// or use the directory descriptor returned by the Unix implementation, so a
// metadata-only identity check is the appropriate macOS primitive and avoids
// requesting content access to the protected directory.
func platformProjectIdentity(path string, expected os.FileInfo) (FileIdentity, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return FileIdentity{}, fmt.Errorf("project: inspect directory identity: %w", err)
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.IsDir() || expected == nil || !os.SameFile(expected, before) {
		return FileIdentity{}, fmt.Errorf("project: directory changed while measuring identity")
	}
	after, err := os.Lstat(path)
	if err != nil || after.Mode()&os.ModeSymlink != 0 || !after.IsDir() || !os.SameFile(before, after) {
		return FileIdentity{}, fmt.Errorf("project: directory changed while measuring identity")
	}
	stat, ok := after.Sys().(*syscall.Stat_t)
	if !ok {
		return FileIdentity{}, fmt.Errorf("project: directory identity unavailable")
	}
	return FileIdentity{Platform: "unix", UnixDevice: uint64(stat.Dev), UnixInode: uint64(stat.Ino)}, nil
}
