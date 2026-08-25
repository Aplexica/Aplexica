//go:build unix

package privatefs

import "os"

func syncDirectoryHandle(f *os.File) error { return f.Sync() }
