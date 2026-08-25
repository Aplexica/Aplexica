//go:build unix && !linux && !darwin

package privatefs

import "golang.org/x/sys/unix"

func (r *Root) installNoReplace(oldRel, newRel string) error {
	fd := int(r.dir.Fd())
	if err := unix.Linkat(fd, oldRel, fd, newRel, 0); err != nil {
		return err
	}
	return unix.Unlinkat(fd, oldRel, 0)
}
