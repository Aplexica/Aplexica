//go:build windows

package nativebackup

import (
	"errors"

	"golang.org/x/sys/windows"
)

func transientCloudKeyringLoadError(err error) bool {
	return errors.Is(err, windows.ERROR_SHARING_VIOLATION) ||
		errors.Is(err, windows.ERROR_LOCK_VIOLATION)
}
