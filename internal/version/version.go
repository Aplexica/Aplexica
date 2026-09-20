// Package version exposes the daemon/CLI version string to other
// packages and the release toolchain.
//
// The default value is the in-source baseline; the release build
// overrides it via `-X github.com/aplexica/aplexica/internal/version.Version=…`
// ldflags (see the `builds` entries in .goreleaser.yaml). Local `go build`
// keeps the baseline.
//
// The baseline is not decoration. The `guard` job of
// .github/workflows/release.yml parses the literal below and refuses to build
// a release whose tag disagrees with it, because a plain `go build` of the
// tagged tree — which is what a contributor runs and what the updater compares
// against — reports this string and not the ldflag.
package version

import (
	"runtime/debug"
)

// ReadBuildInfo allows tests to substitute runtime/debug.ReadBuildInfo.
var ReadBuildInfo = debug.ReadBuildInfo

// Version is the canonical version string. Overridden by ldflags at
// release-build time; default is the in-source baseline used by local
// `go build` and CI test binaries.
var Version = "v1.0.77"

// GitCommit is the full source commit at build time. Overridden by
// release ldflags; default "unknown" for non-release builds.
var GitCommit = "unknown"

// BuildDate is the ISO-8601 build timestamp (commit date for
// reproducible releases). Overridden by release ldflags.
var BuildDate = "unknown"

// Modified reports whether the binary was built from a modified (dirty)
// VCS repository. Populated from vcs.modified build setting when available.
var Modified bool

// ReleaseTrain is empty for source, test, and ordinary Makefile builds. The
// GoReleaser configuration stamps the fixed value understood by the advisory
// updater. This is classification metadata, not a signature or trust root:
// downloaded release bytes still have to pass the documented KMS-backed
// cosign verification.
var ReleaseTrain = ""

func init() {
	initBuildInfo()
}

func initBuildInfo() {
	info, ok := ReadBuildInfo()
	if !ok || info == nil {
		return
	}
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			if GitCommit == "unknown" && setting.Value != "" {
				GitCommit = setting.Value
			}
		case "vcs.time":
			if BuildDate == "unknown" && setting.Value != "" {
				BuildDate = setting.Value
			}
		case "vcs.modified":
			Modified = setting.Value == "true"
		}
	}
}

// String returns the exact release identity suitable for `--version` output.
// Build provenance remains available in signed release evidence; it is not
// appended to the user-visible numeric release version.
func String() string {
	return "aplexica " + Version
}
