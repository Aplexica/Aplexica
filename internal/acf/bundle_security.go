package acf

import (
	"archive/tar"
	"encoding/hex"
	"fmt"
	"math"
	"path"
	"strings"

	"github.com/aplexica/aplexica/internal/safepath"
	"github.com/aplexica/aplexica/internal/securityerr"
)

// BundleLimits bounds every resource controlled by bundle input. Ordinary
// configuration may lower these values but must not raise the compiled hard
// ceilings without an explicit reviewed CLI policy.
type BundleLimits struct {
	MaxCompressedBytes   int64
	MaxSignatureBytes    int64
	MaxTrustedKeyBytes   int64
	MaxIdentityFileBytes int64
	MaxIdentities        int
	MaxMetaBytes         int64
	MaxPathBytes         int
	MaxEntries           int
	MaxEntryBytes        int64
	MaxTotalBytes        int64
	MaxSecretBytes       int64
	MaxBlobBytes         int64
}

func DefaultBundleLimits() BundleLimits {
	return BundleLimits{
		MaxCompressedBytes:   4 << 30,
		MaxSignatureBytes:    1 << 10,
		MaxTrustedKeyBytes:   256,
		MaxIdentityFileBytes: 64 << 10,
		MaxIdentities:        32,
		MaxMetaBytes:         1 << 20,
		MaxPathBytes:         safepath.MaxArchiveNameBytes,
		MaxEntries:           100_000,
		MaxEntryBytes:        256 << 20,
		MaxTotalBytes:        16 << 30,
		MaxSecretBytes:       16 << 20,
		MaxBlobBytes:         4 << 30,
	}
}

type bundlePathValidator struct {
	limits      BundleLimits
	entries     int
	totalBytes  int64
	files       map[string]struct{}
	directories map[string]struct{}
	impliedDirs map[string]struct{}
}

func newBundlePathValidator(limits BundleLimits) *bundlePathValidator {
	return &bundlePathValidator{
		limits:      limits,
		files:       make(map[string]struct{}),
		directories: make(map[string]struct{}),
		impliedDirs: make(map[string]struct{}),
	}
}

// validateHeader runs before a body read or target mutation. It rejects every
// link/special/unknown type, non-canonical name, duplicate/prefix collision,
// unsupported layout, and declared-size overflow.
func (v *bundlePathValidator) validateHeader(hdr *tar.Header) error {
	if hdr == nil {
		return invalidBundle("missing header", securityerr.ErrUnsafeIdentifier)
	}
	v.entries++
	if v.entries > v.limits.MaxEntries {
		return invalidBundle("entry count", securityerr.ErrLimitExceeded)
	}

	isDir := hdr.Typeflag == tar.TypeDir
	if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeRegA && !isDir {
		return invalidBundle("unsupported entry type", securityerr.ErrUnsafeFilesystemNode)
	}
	name := hdr.Name
	if isDir {
		name = strings.TrimSuffix(name, "/")
	}
	if name == "" || len(name) > v.limits.MaxPathBytes {
		return invalidBundle("entry path length", securityerr.ErrLimitExceeded)
	}
	if err := safepath.ValidateArchiveName(name); err != nil {
		return fmt.Errorf("acf: bundle name: %w", err)
	}
	if isDir {
		if !allowedBundleDirectory(name) {
			return invalidBundle("unsupported directory", securityerr.ErrUnsafeIdentifier)
		}
	} else if !allowedBundleFile(name) {
		return invalidBundle("unsupported file shape", securityerr.ErrUnsafeIdentifier)
	}
	if err := v.recordName(name, isDir); err != nil {
		return err
	}

	if isDir {
		if hdr.Size != 0 {
			return invalidBundle("directory body", securityerr.ErrUnsafeIdentifier)
		}
		return nil
	}
	if hdr.Size < 0 {
		return invalidBundle("negative entry size", securityerr.ErrLimitExceeded)
	}
	limit := v.limits.MaxEntryBytes
	switch {
	case name == "meta.json":
		limit = v.limits.MaxMetaBytes
	case strings.HasPrefix(name, "secrets/"):
		limit = v.limits.MaxSecretBytes
	case strings.HasPrefix(name, blobsDirName+"/"):
		limit = v.limits.MaxBlobBytes
	}
	if hdr.Size > limit {
		return invalidBundle("entry body size", securityerr.ErrLimitExceeded)
	}
	if hdr.Size > math.MaxInt64-v.totalBytes || v.totalBytes+hdr.Size > v.limits.MaxTotalBytes {
		return invalidBundle("aggregate body size", securityerr.ErrLimitExceeded)
	}
	v.totalBytes += hdr.Size
	return nil
}

func (v *bundlePathValidator) recordName(name string, isDir bool) error {
	if _, exists := v.files[name]; exists {
		return invalidBundle("duplicate or prefix collision", securityerr.ErrUnsafeIdentifier)
	}
	if _, exists := v.directories[name]; exists {
		return invalidBundle("duplicate or prefix collision", securityerr.ErrUnsafeIdentifier)
	}
	if _, exists := v.impliedDirs[name]; exists && !isDir {
		return invalidBundle("file replaces directory prefix", securityerr.ErrUnsafeIdentifier)
	}
	for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
		if _, file := v.files[parent]; file {
			return invalidBundle("directory descends from file", securityerr.ErrUnsafeIdentifier)
		}
		v.impliedDirs[parent] = struct{}{}
	}
	if isDir {
		v.directories[name] = struct{}{}
	} else {
		v.files[name] = struct{}{}
	}
	return nil
}

func allowedBundleFile(name string) bool {
	if name == "meta.json" {
		return true
	}
	parts := strings.Split(name, "/")
	if len(parts) < 2 {
		return false
	}
	switch parts[0] {
	case "acf":
		return len(parts) == 3 && knownKindDir(parts[1]) && validBundleLeaf(parts[2], ".json")
	case "events":
		if len(parts) == 3 {
			return knownKindDir(parts[1]) && validBundleLeaf(parts[2], ".jsonl")
		}
		return len(parts) == 4 && parts[1] == ".compacted" && knownKindDir(parts[2]) && validBundleLeaf(parts[3], ".jsonl.gz")
	case "eventTags", "branches":
		return len(parts) == 3 && knownKindDir(parts[1]) && validBundleLeaf(parts[2], ".json")
	case blobsDirName:
		return validBlobArchiveParts(parts)
	case "secrets":
		if len(parts) < 2 {
			return false
		}
		for _, component := range parts[1:] {
			if safepath.ValidateStoreComponent(component) != nil {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func allowedBundleDirectory(name string) bool {
	parts := strings.Split(name, "/")
	switch parts[0] {
	case "acf", "eventTags", "branches":
		return len(parts) == 1 || (len(parts) == 2 && knownKindDir(parts[1]))
	case "events":
		return len(parts) == 1 ||
			(len(parts) == 2 && (knownKindDir(parts[1]) || parts[1] == ".compacted")) ||
			(len(parts) == 3 && parts[1] == ".compacted" && knownKindDir(parts[2]))
	case blobsDirName:
		if len(parts) == 1 {
			return true
		}
		return (len(parts) == 2 || len(parts) == 3) && isLowerHex(parts[len(parts)-1], 2)
	case "secrets":
		if len(parts) == 1 {
			return true
		}
		for _, component := range parts[1:] {
			if safepath.ValidateStoreComponent(component) != nil {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func knownKindDir(value string) bool {
	switch value {
	case "memories", "skills", "tools", "conversations":
		return true
	default:
		return false
	}
}

func validBundleLeaf(leaf, suffix string) bool {
	if !strings.HasSuffix(leaf, suffix) {
		return false
	}
	component := strings.TrimSuffix(leaf, suffix)
	return component != "" && safepath.ValidateStoreComponent(component) == nil
}

func validBlobArchiveParts(parts []string) bool {
	if len(parts) != 4 || !isLowerHex(parts[1], 2) || !isLowerHex(parts[2], 2) || !isLowerHex(parts[3], 64) {
		return false
	}
	return parts[1] == parts[3][:2] && parts[2] == parts[3][2:4]
}

func isLowerHex(value string, size int) bool {
	if len(value) != size || strings.ToLower(value) != value {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded)*2 == size
}

func invalidBundle(category string, sentinel error) error {
	return fmt.Errorf("acf: bundle %s: %w", category, sentinel)
}
