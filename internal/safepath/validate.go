// Package safepath contains platform-independent validation for dynamic path
// components and archive names. It does not open files; security-sensitive
// callers combine these checks with privatefs retained-root operations.
package safepath

import (
	"fmt"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"unicode/utf8"

	"github.com/aplexica/aplexica/internal/securityerr"
	"golang.org/x/text/unicode/norm"
)

const (
	MaxStoreComponentBytes = 255
	MaxArchiveNameBytes    = 1024
	MaxArchiveSegments     = 64
	minPrintableCodePoint  = rune(0x20)
	deleteCodePoint        = rune(0x7f)
)

var windowsReservedNames = map[string]struct{}{
	"aux": {}, "clock$": {}, "con": {}, "nul": {}, "prn": {},
	"com1": {}, "com2": {}, "com3": {}, "com4": {}, "com5": {},
	"com6": {}, "com7": {}, "com8": {}, "com9": {},
	"lpt1": {}, "lpt2": {}, "lpt3": {}, "lpt4": {}, "lpt5": {},
	"lpt6": {}, "lpt7": {}, "lpt8": {}, "lpt9": {},
}

// ValidateStoreComponent validates one dynamic filesystem component using the
// union of Unix and Windows restrictions. It therefore returns the same result
// on every supported platform.
func ValidateStoreComponent(component string) error {
	return validateComponent(component, MaxStoreComponentBytes)
}

// ValidateNativeComponent validates one component copied from an agent-owned
// native tree. Native rollback snapshots preserve names that are legal on the
// current host even when they are not portable store identifiers. In
// particular, historical Codex rollout filenames contain ':' on macOS. A
// Windows host still applies the complete Windows restriction set so a native
// path can never become an alternate data stream or volume-qualified name.
func ValidateNativeComponent(component string) error {
	if runtime.GOOS == "windows" {
		return validateComponent(component, MaxStoreComponentBytes)
	}
	return validateUnixNativeComponent(component, MaxStoreComponentBytes)
}

// ValidateArchiveName validates a slash-separated archive name before any
// body is read. Allowed top-level shapes and duplicate/prefix collisions are
// enforced by the bundle planner; this function enforces the general name
// invariants shared by every archive reader.
func ValidateArchiveName(name string) error {
	return validateArchiveName(name, ValidateStoreComponent)
}

// ValidateNativeArchiveName applies the same traversal, depth, and encoding
// checks as ValidateArchiveName while permitting names that are valid in a
// native rollback snapshot on the current host. Native snapshots are tied to
// their recorded source roots; they are not ACF's cross-platform store format.
func ValidateNativeArchiveName(name string) error {
	return validateArchiveName(name, ValidateNativeComponent)
}

func validateArchiveName(name string, validate func(string) error) error {
	if len(name) == 0 || len(name) > MaxArchiveNameBytes || !utf8.ValidString(name) || !norm.NFC.IsNormalString(name) {
		return unsafeIdentifier("archive name encoding or length")
	}
	if strings.HasPrefix(name, "/") || strings.HasPrefix(name, "//") || strings.ContainsRune(name, '\\') {
		return pathEscape("absolute or platform path")
	}
	if path.Clean(name) != name {
		return pathEscape("non-canonical archive name")
	}
	segments := strings.Split(name, "/")
	if len(segments) > MaxArchiveSegments {
		return unsafeIdentifier("too many archive path segments")
	}
	for _, segment := range segments {
		if err := validate(segment); err != nil {
			return fmt.Errorf("safepath: archive component: %w", err)
		}
	}
	return nil
}

func validateUnixNativeComponent(component string, maxBytes int) error {
	if len(component) == 0 || len(component) > maxBytes || !utf8.ValidString(component) || !norm.NFC.IsNormalString(component) {
		return unsafeIdentifier("component encoding or length")
	}
	if component == "." || component == ".." {
		return pathEscape("dot component")
	}
	// Backslash is a legal Unix filename byte, but native snapshot manifests
	// use slash-separated paths and may be encrypted for later download. Keep
	// both separator forms unambiguous while allowing ':' on Unix.
	if strings.ContainsAny(component, "/\\") {
		return pathEscape("separator component")
	}
	for _, r := range component {
		if r == 0 || r < minPrintableCodePoint || r == deleteCodePoint {
			return unsafeIdentifier("control character")
		}
	}
	return nil
}

// Within reports whether candidate is lexically within root after absolute
// cleaning. It is useful for policy checks, but does not replace retained-root
// no-follow operations where an attacker can race filesystem objects.
func Within(root, candidate string) bool {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	candidateAbs, err := filepath.Abs(candidate)
	if err != nil {
		return false
	}
	rootAbs = filepath.Clean(rootAbs)
	candidateAbs = filepath.Clean(candidateAbs)
	if !strings.EqualFold(filepath.VolumeName(rootAbs), filepath.VolumeName(candidateAbs)) {
		return false
	}
	rel, err := filepath.Rel(rootAbs, candidateAbs)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel))
}

func validateComponent(component string, maxBytes int) error {
	if len(component) == 0 || len(component) > maxBytes || !utf8.ValidString(component) || !norm.NFC.IsNormalString(component) {
		return unsafeIdentifier("component encoding or length")
	}
	if component == "." || component == ".." {
		return pathEscape("dot component")
	}
	if strings.ContainsAny(component, "/\\:") || filepath.VolumeName(component) != "" {
		return pathEscape("separator or volume component")
	}
	if strings.HasSuffix(component, " ") || strings.HasSuffix(component, ".") {
		return unsafeIdentifier("windows trailing alias")
	}
	for _, r := range component {
		if r == 0 || r < minPrintableCodePoint || r == deleteCodePoint {
			return unsafeIdentifier("control character")
		}
	}
	base := component
	if dot := strings.IndexByte(base, '.'); dot >= 0 {
		base = base[:dot]
	}
	if _, reserved := windowsReservedNames[strings.ToLower(base)]; reserved {
		return unsafeIdentifier("windows reserved name")
	}
	return nil
}

func unsafeIdentifier(category string) error {
	return fmt.Errorf("safepath: %s: %w", category, securityerr.ErrUnsafeIdentifier)
}

func pathEscape(category string) error {
	return fmt.Errorf("safepath: %s: %w", category, securityerr.ErrPathEscape)
}
