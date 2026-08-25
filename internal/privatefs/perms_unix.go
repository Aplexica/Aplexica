//go:build unix

package privatefs

import (
	"fmt"
	"os"
	"syscall"
)

// regularReadOpenFlags keeps special files from blocking before descriptor
// validation can reject them. O_NONBLOCK has no effect on regular-file reads,
// but makes a hostile FIFO fail closed instead of hanging a startup integrity
// check while open waits for a writer.
func regularReadOpenFlags() int { return os.O_RDONLY | syscall.O_NONBLOCK }

func validateOrRepairDir(path string, info os.FileInfo, policy DirPolicy) error {
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("privatefs: root is not a real directory")
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(st.Uid) != os.Geteuid() {
		return fmt.Errorf("privatefs: directory owner mismatch")
	}
	bad := info.Mode().Perm() & 0o077
	if policy.Access == AccessIntegrityOnly {
		bad = info.Mode().Perm() & 0o022
	}
	if bad != 0 {
		if policy.Access == AccessPrivate && policy.RepairOwned {
			if err := os.Chmod(path, info.Mode().Perm()&0o700); err != nil {
				return err
			}
			return nil
		}
		return fmt.Errorf("privatefs: unsafe directory permissions %04o", info.Mode().Perm())
	}
	return nil
}

func validateDirInfo(info os.FileInfo, policy DirPolicy) error {
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("privatefs: expected directory")
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(st.Uid) != os.Geteuid() {
		return fmt.Errorf("privatefs: directory owner mismatch")
	}
	bad := info.Mode().Perm() & 0o077
	if policy.Access == AccessIntegrityOnly {
		bad = info.Mode().Perm() & 0o022
	}
	if bad != 0 {
		return fmt.Errorf("privatefs: unsafe directory permissions")
	}
	return nil
}

func validateRegularFile(f *os.File, writable bool) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("privatefs: expected regular file")
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(st.Uid) != os.Geteuid() {
		return ErrUnsafeFileIdentity
	}
	if st.Nlink == 0 {
		return ErrOpenedFileUnlinked
	}
	if st.Nlink != 1 {
		return ErrUnsafeFileIdentity
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("privatefs: unsafe file permissions")
	}
	return nil
}

func validateIntegrityFile(f *os.File) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || st.Nlink != 1 || (int(st.Uid) != os.Geteuid() && st.Uid != 0) || info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("privatefs: unsafe integrity file")
	}
	return nil
}

func ValidateRootHandle(r *os.Root, policy DirPolicy) error {
	info, err := r.Stat(".")
	if err != nil {
		return err
	}
	return validateDirInfo(info, policy)
}

func openRetainedDirectory(path string) (*os.File, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("privatefs: retain root: %w", err)
	}
	return f, nil
}
func validateRegularDirectoryHandle(f *os.File, policy DirPolicy) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	return validateDirInfo(info, policy)
}

func validateRepairNode(info os.FileInfo, dir bool) error {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(st.Uid) != os.Geteuid() {
		return fmt.Errorf("privatefs: node owner mismatch")
	}
	if dir {
		if !info.IsDir() {
			return fmt.Errorf("privatefs: expected directory")
		}
	} else if !info.Mode().IsRegular() {
		return fmt.Errorf("privatefs: unsafe private file identity")
	} else if st.Nlink == 0 {
		return ErrOpenedFileUnlinked
	} else if st.Nlink != 1 {
		return fmt.Errorf("privatefs: unsafe private file identity")
	}
	return nil
}

func validateRepairHandle(f *os.File, dir bool) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	return validateRepairNode(info, dir)
}

func hardenHandle(f *os.File, dir bool) error {
	if dir {
		return f.Chmod(0o700)
	}
	return f.Chmod(0o600)
}

func (r *Root) hardenRelativeNode(_ string, f *os.File, dir bool) error {
	return hardenHandle(f, dir)
}
