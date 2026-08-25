package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// scanFile is the unit-of-test surface. The integration of allowlist
// loading + walk is exercised by the live `go run ./tools/magiclint`
// invocation in CI (which would itself fail if scanFile is wrong).

func writeGo(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, []byte(body), 0o644))
	return path
}

func TestScanFile_FlagsMagicNumeric(t *testing.T) {
	dir := t.TempDir()
	path := writeGo(t, dir, "x.go",
		`package x

func DaemonInterval() int { return 60 }
`)
	fs, err := scanFile(path)
	require.NoError(t, err)
	require.Len(t, fs, 1)
	require.Equal(t, "60", fs[0].Literal)
}

func TestScanFile_SkipsConstDecl(t *testing.T) {
	dir := t.TempDir()
	path := writeGo(t, dir, "x.go",
		`package x

const FooDays = 7
`)
	fs, err := scanFile(path)
	require.NoError(t, err)
	require.Empty(t, fs, "const decl literals are §10.4-exempt")
}

func TestScanFile_SkipsFixedArrayTypeLength(t *testing.T) {
	dir := t.TempDir()
	path := writeGo(t, dir, "x.go",
		`package x

type Digest [32]byte

func F(v [64 + 1]byte) [128]byte {
	return [128]byte{}
}
`)
	fs, err := scanFile(path)
	require.NoError(t, err)
	require.Empty(t, fs,
		"fixed array lengths are part of Go type identity, not runtime tunables")
}

func TestScanFile_ArrayLengthExemptionDoesNotHideRuntimeCapacity(t *testing.T) {
	dir := t.TempDir()
	path := writeGo(t, dir, "x.go",
		`package x

func F() []byte {
	return make([]byte, 4096)
}
`)
	fs, err := scanFile(path)
	require.NoError(t, err)
	require.Len(t, fs, 1)
	require.Equal(t, "4096", fs[0].Literal,
		"runtime allocation sizes must remain subject to FR-10.6")
}

func TestScanFile_SkipsStructuralByteAndBitPositions(t *testing.T) {
	dir := t.TempDir()
	path := writeGo(t, dir, "x.go",
		`package x

func F(buf []byte, item byte) byte {
	_ = buf[8:10]
	_ = buf[127]
	return (item >> 4) & 0x0f
}
`)
	fs, err := scanFile(path)
	require.NoError(t, err)
	require.Empty(t, fs,
		"byte offsets and bit fields describe a fixed data layout, not tunables")
}

func TestScanFile_StructuralExemptionsDoNotHideOrdinaryArithmetic(t *testing.T) {
	dir := t.TempDir()
	path := writeGo(t, dir, "x.go",
		`package x

func F(value int) int {
	return value + 99
}
`)
	fs, err := scanFile(path)
	require.NoError(t, err)
	require.Len(t, fs, 1)
	require.Equal(t, "99", fs[0].Literal)
}

func TestScanFile_SkipsTrivialNumerics(t *testing.T) {
	dir := t.TempDir()
	path := writeGo(t, dir, "x.go",
		`package x

func F() {
	x := 0
	y := 1
	z := 2
	_ = x + y + z
}
`)
	fs, err := scanFile(path)
	require.NoError(t, err)
	require.Empty(t, fs, "0/1/2 are universally trivial — must not be flagged")
}

func TestScanFile_SkipsFileModeStructField(t *testing.T) {
	dir := t.TempDir()
	path := writeGo(t, dir, "x.go",
		`package x

import "os"

type W struct {
	Mode os.FileMode
}

func New() W {
	return W{Mode: 0o644}
}
`)
	fs, err := scanFile(path)
	require.NoError(t, err)
	for _, f := range fs {
		require.NotEqual(t, "0644", f.Literal,
			"file-mode struct-field value 0o644 must be exempt; got finding %+v", f)
	}
}

func TestScanFile_SkipsOsExit(t *testing.T) {
	dir := t.TempDir()
	path := writeGo(t, dir, "x.go",
		`package x

import "os"

func F() { os.Exit(42) }
`)
	fs, err := scanFile(path)
	require.NoError(t, err)
	require.Empty(t, fs, "os.Exit(N) is exit-code-shaped, not a tunable")
}

func TestScanFile_FlagsTypicalTimeoutLiteral(t *testing.T) {
	dir := t.TempDir()
	path := writeGo(t, dir, "x.go",
		`package x

import "time"

func Run() {
	time.Sleep(time.Duration(300) * time.Second)
}
`)
	fs, err := scanFile(path)
	require.NoError(t, err)
	// 300 should be flagged; "Second" is an identifier, not a literal.
	require.NotEmpty(t, fs)
	found := false
	for _, f := range fs {
		if f.Literal == "300" {
			found = true
		}
	}
	require.True(t, found,
		"a hardcoded 300-second timeout must be flagged as a magic number")
}

func TestLoadAllowlist_HandlesCommentsAndTrailing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".magiclint-allow")
	body := `# header comment
foo.go:60:1
# trailing-comment style
bar.go:300:2 # legacy; tracked in #999

baz.go:1e9:1
`
	require.NoError(t, os.WriteFile(path, []byte(body), 0o644))

	got, err := loadAllowlist(path)
	require.NoError(t, err)
	require.Equal(t, 1, got["foo.go:60"])
	require.Equal(t, 2, got["bar.go:300"])
	require.Equal(t, 1, got["baz.go:1e9"])
	require.Equal(t, 3, len(got))
}

func TestLoadAllowlist_RejectsLegacyPathLineFormat(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".magiclint-allow")
	require.NoError(t, os.WriteFile(path, []byte("cmd/aplexica/cmd_daemon.go:492\n"), 0o644))

	_, err := loadAllowlist(path)
	require.Error(t, err, "a 2-part path:line entry must be rejected, not misread as path:value")
	require.Contains(t, err.Error(), "--init-allowlist",
		"the error must point at the migration path")
}

func TestLoadAllowlist_RejectsBadCount(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".magiclint-allow")
	for _, bad := range []string{"foo.go:60:0\n", "foo.go:60:-1\n", "foo.go:60:x\n"} {
		require.NoError(t, os.WriteFile(path, []byte(bad), 0o644))
		_, err := loadAllowlist(path)
		require.Error(t, err, "entry %q must be rejected", bad)
	}
}

func TestLoadAllowlist_MissingIsEmpty(t *testing.T) {
	got, err := loadAllowlist(filepath.Join(t.TempDir(), "never.allow"))
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestWriteAllowlist_RoundTrips(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".magiclint-allow")
	in := []finding{
		{Path: "foo.go", Line: 10, Literal: "60"},
		{Path: "foo.go", Line: 95, Literal: "60"}, // same value twice → count 2
		{Path: "bar.go", Line: 42, Literal: "300"},
	}
	entries, werr := writeAllowlist(path, in)
	require.NoError(t, werr)
	require.Equal(t, 2, entries, "3 findings group into 2 path:value:count entries")

	got, err := loadAllowlist(path)
	require.NoError(t, err)
	require.Equal(t, 2, got["foo.go:60"])
	require.Equal(t, 1, got["bar.go:300"])

	// Header comments must be there.
	body, _ := os.ReadFile(path)
	require.True(t, strings.HasPrefix(string(body), "#"))
}

func TestWriteAllowlist_PreservesReviewedHeaderRationale(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".magiclint-allow")
	header := "# reviewed baseline\n# keep this rationale\n\n"
	require.NoError(t, os.WriteFile(path, []byte(header+"old.go:60:1\n"), 0o644))

	entries, err := writeAllowlist(path, []finding{{Path: "new.go", Line: 7, Literal: "90"}})
	require.NoError(t, err)
	require.Equal(t, 1, entries)
	body, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, header+"new.go:90:1\n", string(body))
}

// TestViolations_EditStableAcrossLineShifts is the regression guard for the
// path:line → path:value:count re-key: moving an allowlisted literal to a
// different line (any edit above it) must NOT create a phantom violation.
func TestViolations_EditStableAcrossLineShifts(t *testing.T) {
	allow := map[string]int{"foo.go:60": 1}

	before := []finding{{Path: "foo.go", Line: 10, Literal: "60"}}
	after := []finding{{Path: "foo.go", Line: 9999, Literal: "60"}}

	require.Empty(t, newViolationGroups(before, allow))
	require.Empty(t, newViolationGroups(after, allow),
		"the same literal at a shifted line must still be covered")
}

func TestViolations_CountExceededFlagsDelta(t *testing.T) {
	allow := map[string]int{"foo.go:240": 2}
	findings := []finding{
		{Path: "foo.go", Line: 10, Literal: "240"},
		{Path: "foo.go", Line: 20, Literal: "240"},
		{Path: "foo.go", Line: 30, Literal: "240"}, // third occurrence — 1 over
	}
	got := newViolationGroups(findings, allow)
	require.Len(t, got, 1)
	require.Equal(t, "foo.go", got[0].path)
	require.Equal(t, "240", got[0].literal)
	require.Equal(t, 2, got[0].allowed)
	require.Equal(t, 1, got[0].delta, "exactly the occurrences beyond the allowance are new")
	require.Equal(t, []int{10, 20, 30}, got[0].lines,
		"all occurrences are reported so the developer can locate the new one")
}

func TestViolations_UnlistedValueFlagsAll(t *testing.T) {
	got := newViolationGroups([]finding{
		{Path: "foo.go", Line: 5, Literal: "777"},
		{Path: "foo.go", Line: 6, Literal: "777"},
	}, map[string]int{})
	require.Len(t, got, 1)
	require.Equal(t, 2, got[0].delta)
}
