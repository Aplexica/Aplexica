package project

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"strings"
)

// pathDerivedID returns the stable non-VCS project ID per BRD-02
// §4.13.3 item 3:
//
//	local:<sha256(absolute-path)[:6]>:<dirname>
//
// The 6-hex-char prefix makes IDs short while keeping cross-device
// collision risk negligible (two different paths would need a 24-bit
// hash collision AND the same dirname). The dirname is appended for
// human readability — when a user sees "local:a1b2c3:sample-project"
// they know which directory it refers to without consulting the
// registry.
func pathDerivedID(absPath string) string {
	sum := sha256.Sum256([]byte(absPath))
	short := hex.EncodeToString(sum[:])[:6]
	dirname := filepath.Base(absPath)
	dirname = sanitizeDirname(dirname)
	if dirname == "" {
		dirname = "root"
	}
	return "local:" + short + ":" + dirname
}

// PathDerivedID is the exported entry point to pathDerivedID. The web
// API's manual-registration fallback uses it to produce the identical
// ID project.Detect would derive for a non-VCS folder, for the rare
// case where Detect itself errors (path-resolution failure).
func PathDerivedID(absPath string) string {
	return pathDerivedID(absPath)
}

// sanitizeDirname normalizes a directory basename into a slug
// suitable for inclusion in a project ID. Lowercases, collapses
// runs of non-alphanumeric to single hyphens, trims leading/trailing
// hyphens.
func sanitizeDirname(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	prevDash := false
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			prevDash = false
		} else if !prevDash {
			b.WriteRune('-')
			prevDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}
