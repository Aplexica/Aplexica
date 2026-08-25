package project

import (
	"runtime"
	"strings"
)

// registryPathKey mirrors the platform path-comparison policy used by project
// discovery. Windows and default macOS volumes are case-insensitive; folding
// both active and not-yet-existing paths prevents two spellings from becoming
// separate authorization records for the same future directory. A false
// collision on an explicitly case-sensitive macOS volume fails closed.
func registryPathKey(path string) string {
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		return strings.ToLower(path)
	}
	return path
}
