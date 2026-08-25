package project

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNormalizeHgURL(t *testing.T) {
	cases := map[string]string{
		// SSH forms
		"hg@bitbucket.org:Example-User/Sample-Repo":      "bitbucket.org/example-user/sample-repo",
		"ssh://hg@bitbucket.org/owner/repo":              "bitbucket.org/owner/repo",
		"ssh://hg@selfhosted.example.com/group/sub/repo": "selfhosted.example.com/group/sub/repo",

		// HTTPS form
		"https://bitbucket.org/Example-User/Sample-Repo": "bitbucket.org/example-user/sample-repo",
		"https://hg.example.com/group/repo":              "hg.example.com/group/repo",

		// HTTP form
		"http://hg.example.com:8080/group/repo": "hg.example.com:8080/group/repo",

		// Plain form (no scheme)
		"bitbucket.org/owner/repo": "bitbucket.org/owner/repo",

		// Trailing slash
		"https://bitbucket.org/owner/repo/": "bitbucket.org/owner/repo",

		// Manually-appended .hg
		"https://bitbucket.org/owner/repo.hg": "bitbucket.org/owner/repo",

		// Empty / garbage
		"": "",
	}
	for in, want := range cases {
		got := normalizeHgURL(in)
		require.Equal(t, want, got, "normalizeHgURL(%q)", in)
	}
}

// TestReadHgDefaultURL_OverlongLine_ReturnsReadError is the hg twin of
// TestScanForOriginURL_OverlongLine_ReturnsReadError (Finding 1): a hgrc line
// past the RAISED scanner cap must surface a real read error, NOT be folded
// into the ErrNoHgDefault "no default path" sentinel.
func TestReadHgDefaultURL_OverlongLine_ReturnsReadError(t *testing.T) {
	dir := t.TempDir()
	huge := strings.Repeat("a", (2*1024*1024)+16) // past the raised 1 MiB cap
	mkHgConfig(t, dir, "[paths]\ndefault = ssh://hg@bitbucket.org/"+huge+"\n")

	got, err := readHgDefaultURL(dir)
	require.Error(t, err, "an overlong hgrc line must surface a read error")
	require.NotErrorIs(t, err, ErrNoHgDefault,
		"an overlong-line read failure must be distinguishable from the no-default sentinel")
	require.Equal(t, "", got)
}

// TestReadHgDefaultURL_LongButUnderRaisedCap_ParsesURL verifies the buffer bump
// for hg: a default line long enough to overflow the OLD 64 KiB default but
// under the new 1 MiB cap must now parse.
func TestReadHgDefaultURL_LongButUnderRaisedCap_ParsesURL(t *testing.T) {
	dir := t.TempDir()
	longPath := strings.Repeat("x", 128*1024) // over old default, under new cap
	want := "https://bitbucket.org/" + longPath
	mkHgConfig(t, dir, "[paths]\ndefault = "+want+"\n")

	got, err := readHgDefaultURL(dir)
	require.NoError(t, err)
	require.Equal(t, want, got)
}

func mkHgConfig(t *testing.T, dir, contents string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".hg"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, ".hg", "hgrc"),
		[]byte(contents), 0o644))
}

func TestDetect_HgRepo_WithDefault_ReturnsNormalizedURL(t *testing.T) {
	dir := t.TempDir()
	mkHgConfig(t, dir, `[ui]
username = Test User <test@example.com>

[paths]
default = ssh://hg@bitbucket.org/Example-User/Repo
default-push = ssh://hg@bitbucket.org/Example-User/Repo
`)
	got, err := Detect(dir)
	require.NoError(t, err)
	physical, err := filepath.EvalSymlinks(dir)
	require.NoError(t, err)
	require.Equal(t, "hg", got.VCS)
	require.Equal(t, "bitbucket.org/example-user/repo", got.ID)
	require.Equal(t, physical, got.Path)
}

func TestDetect_HgRepo_NoHgrc_FallsBackToPathID(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".hg"), 0o755))
	// No hgrc file — fresh local repo.
	got, err := Detect(dir)
	require.NoError(t, err)
	require.Equal(t, "hg", got.VCS, "VCS should still report hg")
	require.True(t, strings.HasPrefix(got.ID, "local:"),
		"missing hgrc should fall back to local:* ID, got %q", got.ID)
}

func TestDetect_HgRepo_HgrcWithoutPathsSection_FallsBackToPathID(t *testing.T) {
	dir := t.TempDir()
	mkHgConfig(t, dir, `[ui]
username = Test User <test@example.com>
`)
	got, err := Detect(dir)
	require.NoError(t, err)
	require.Equal(t, "hg", got.VCS)
	require.True(t, strings.HasPrefix(got.ID, "local:"),
		"hgrc without [paths] should fall back to local:* ID, got %q", got.ID)
}

func TestDetect_HgRepo_PathsWithoutDefault_FallsBackToPathID(t *testing.T) {
	dir := t.TempDir()
	// [paths] has default-push but no plain "default" — should fall
	// back, NOT match default-push (default is hg's canonical
	// upstream identifier).
	mkHgConfig(t, dir, `[paths]
default-push = ssh://hg@bitbucket.org/owner/repo
secondary = https://other.example.com/owner/repo
`)
	got, err := Detect(dir)
	require.NoError(t, err)
	require.Equal(t, "hg", got.VCS)
	require.True(t, strings.HasPrefix(got.ID, "local:"),
		"hgrc with no `default =` should fall back to local:* ID, got %q", got.ID)
}

func TestDetect_HgRepo_DefaultCaseInsensitive(t *testing.T) {
	dir := t.TempDir()
	// Mercurial INI parsing is case-insensitive on section + key.
	mkHgConfig(t, dir, `[PATHS]
DEFAULT = https://bitbucket.org/owner/repo
`)
	got, err := Detect(dir)
	require.NoError(t, err)
	require.Equal(t, "hg", got.VCS)
	require.Equal(t, "bitbucket.org/owner/repo", got.ID)
}

func TestDetect_HgRepo_CommentsAndBlanksIgnored(t *testing.T) {
	dir := t.TempDir()
	mkHgConfig(t, dir, `# top comment
; semicolon comment

[paths]
# inside-section comment
default = https://bitbucket.org/owner/repo

; trailing
`)
	got, err := Detect(dir)
	require.NoError(t, err)
	require.Equal(t, "bitbucket.org/owner/repo", got.ID)
}
