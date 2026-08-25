package privatefs

import (
	"fmt"
	"os"
	"path/filepath"
)

// HardenPrivateTree validates and narrows an operation-owned private tree.
// Links, special nodes, foreign owners, and hard-linked files fail closed.
func (r *Root) HardenPrivateTree() error {
	if err := r.validateTreeDir("."); err != nil {
		return err
	}
	return r.hardenDir(".")
}
func (r *Root) validateTreeDir(rel string) error {
	or, err := r.withRoot()
	if err != nil {
		return err
	}
	if rel != "." {
		if err := r.rejectLinks(rel, true); err != nil {
			return err
		}
	}
	// Reads the descriptors of rel's children below, so shared on rel for the
	// whole walk. Recursion into a child directory takes that child's exclusive
	// lock under this one: parent before child, per N2.
	defer rlockPrivateDir(filepath.Join(r.path, rel))()
	d, err := or.Open(rel)
	if err != nil {
		return err
	}
	err = validateRepairHandle(d, true)
	if err != nil {
		d.Close()
		return err
	}
	entries, err := d.ReadDir(-1)
	cerr := d.Close()
	if err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		child := entry.Name()
		if rel != "." {
			child = filepath.Join(rel, child)
		}
		li, err := or.Lstat(child)
		if err != nil {
			return err
		}
		if li.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("privatefs: link rejected during permission migration")
		}
		if li.IsDir() {
			if err := r.validateTreeDir(child); err != nil {
				return err
			}
			continue
		}
		if !li.Mode().IsRegular() {
			return fmt.Errorf("privatefs: special node rejected during permission migration")
		}
		f, err := or.OpenFile(child, os.O_RDONLY, 0)
		if err != nil {
			return err
		}
		fi, err := f.Stat()
		if err == nil && !os.SameFile(li, fi) {
			err = fmt.Errorf("privatefs: node identity changed during permission migration")
		}
		if err == nil {
			err = validateRepairHandle(f, false)
		}
		cerr = f.Close()
		if err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
	}
	return nil
}
func (r *Root) hardenDir(rel string) error {
	or, err := r.withRoot()
	if err != nil {
		return err
	}
	if rel != "." {
		if err := r.rejectLinks(rel, true); err != nil {
			return err
		}
	}
	d, err := or.Open(rel)
	if err != nil {
		return err
	}
	err = validateRepairHandle(d, true)
	if err == nil {
		// Takes exclusive(rel) internally and releases it on return, so the
		// shared hold for the child loop below is acquired after it, never
		// nested inside it (N1).
		err = r.hardenRelative(rel, d, true)
	}
	if err != nil {
		d.Close()
		return err
	}
	release := rlockPrivateDir(filepath.Join(r.path, rel))
	defer release()
	entries, err := d.ReadDir(-1)
	closeErr := d.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		child := entry.Name()
		if rel != "." {
			child = filepath.Join(rel, child)
		}
		li, err := or.Lstat(child)
		if err != nil {
			return err
		}
		if li.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("privatefs: link rejected during permission migration")
		}
		if li.IsDir() {
			if err := r.hardenDir(child); err != nil {
				return err
			}
			continue
		}
		if !li.Mode().IsRegular() {
			return fmt.Errorf("privatefs: special node rejected during permission migration")
		}
		f, err := or.OpenFile(child, os.O_RDONLY, 0)
		if err != nil {
			return err
		}
		fi, err := f.Stat()
		if err == nil && !os.SameFile(li, fi) {
			err = fmt.Errorf("privatefs: node identity changed during permission migration")
		}
		if err == nil {
			err = validateRepairHandle(f, false)
		}
		if err == nil {
			err = r.hardenRelative(child, f, false)
		}
		cerr := f.Close()
		if err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
	}
	return r.SyncDir(rel)
}
