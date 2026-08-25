//go:build !windows

package atomicfile

import "os"

func protectPrivateFileAtPath(_ *os.File, _ string, _ os.FileMode) error { return nil }
func protectPrivatePath(_ string, _ os.FileMode) error                   { return nil }
