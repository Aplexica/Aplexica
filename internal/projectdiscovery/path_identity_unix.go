//go:build unix

package projectdiscovery

import (
	"fmt"
	"os"
	"syscall"
)

func fileIdentity(info os.FileInfo) (FileIdentity, error) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Dev == 0 || st.Ino == 0 {
		return FileIdentity{}, fmt.Errorf("projectdiscovery: unavailable directory identity")
	}
	return FileIdentity{Platform: "unix", UnixDevice: uint64(st.Dev), UnixInode: uint64(st.Ino)}, nil
}

func fileIdentityFromHandle(f *os.File, _ os.FileInfo) (FileIdentity, error) {
	info, err := f.Stat()
	if err != nil {
		return FileIdentity{}, err
	}
	return fileIdentity(info)
}
