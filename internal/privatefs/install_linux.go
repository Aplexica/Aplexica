//go:build linux

package privatefs

import "golang.org/x/sys/unix"

func (r *Root) installNoReplace(oldRel, newRel string) error {
	fd := int(r.dir.Fd())
	return unix.Renameat2(fd, oldRel, fd, newRel, unix.RENAME_NOREPLACE)
}
