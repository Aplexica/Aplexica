//go:build !windows

package nativebackup

func transientCloudKeyringLoadError(error) bool {
	return false
}
