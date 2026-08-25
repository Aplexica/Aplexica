//go:build darwin

package privatefs

import "golang.org/x/sys/unix"

func (r *Root) installNoReplace(oldRel, newRel string) error {
	fd := int(r.dir.Fd())
	return unix.RenameatxNp(fd, oldRel, fd, newRel, unix.RENAME_EXCL)
}
