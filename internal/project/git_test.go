package project

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestScanForOriginURL_OverlongLine_ReturnsReadError verifies Finding 1: a
// config line at/over the default bufio.Scanner token cap (64 KiB) must NOT be
// silently swallowed into the ErrNoOrigin "absent origin" sentinel. A legit
// long url.<base>.insteadOf rewrite, an embedded credential blob, or a
// corrupted config can produce such a line; before the fix Scan() returns false
// with bufio.ErrTooLong, the loop exits as if origin were absent, and the
// caller falls back to a path-derived ID with no signal that a read failed.
//
// Post-fix the scanner buffer is raised to 1 MiB, so an "ordinary" long url
// line parses fine; only a line beyond the RAISED cap surfaces a real error
// (distinct from ErrNoOrigin) so the caller can tell a read failure apart from
// a genuinely origin-less repo.
func TestScanForOriginURL_OverlongLine_ReturnsReadError(t *testing.T) {
	// A url value longer than the RAISED 1 MiB buffer guarantees the scanner
	// still trips ErrTooLong even after the buffer bump.
	huge := strings.Repeat("a", (2*1024*1024)+16)
	cfg := "[remote \"origin\"]\n\turl = https://example.com/" + huge + "\n"

	got, err := scanForOriginURL(strings.NewReader(cfg))
	require.Error(t, err, "an overlong config line must surface a read error")
	require.NotErrorIs(t, err, ErrNoOrigin,
		"an overlong-line read failure must be distinguishable from the absent-origin sentinel")
	require.Equal(t, "", got)
}

// TestScanForOriginURL_LongButUnderRaisedCap_ParsesURL verifies the buffer
// bump itself: a url line that is long (well over the OLD 64 KiB default) but
// under the new 1 MiB cap must now parse correctly rather than being truncated.
func TestScanForOriginURL_LongButUnderRaisedCap_ParsesURL(t *testing.T) {
	// ~128 KiB path component: over the old 64 KiB default, under the new cap.
	longPath := strings.Repeat("x", 128*1024)
	want := "https://example.com/" + longPath
	cfg := "[remote \"origin\"]\n\turl = " + want + "\n"

	got, err := scanForOriginURL(strings.NewReader(cfg))
	require.NoError(t, err)
	require.Equal(t, want, got)
}

// errReader returns a non-EOF error partway through, simulating a truncated /
// unreadable .git/config. The scanner must propagate this rather than treating
// it as "origin absent".
type errReader struct {
	data []byte
	off  int
	err  error
}

func (r *errReader) Read(p []byte) (int, error) {
	if r.off < len(r.data) {
		n := copy(p, r.data[r.off:])
		r.off += n
		return n, nil
	}
	return 0, r.err
}

func TestScanForOriginURL_ReadError_NotSwallowed(t *testing.T) {
	boom := errors.New("disk read failure")
	r := &errReader{data: []byte("[remote \"origin\"]\n"), err: boom}
	got, err := scanForOriginURL(r)
	require.ErrorIs(t, err, boom, "a mid-stream read error must propagate")
	require.NotErrorIs(t, err, ErrNoOrigin)
	require.Equal(t, "", got)
}

func TestNormalizeGitURL(t *testing.T) {
	cases := map[string]string{
		// SSH form (most common GitHub default)
		"git@github.com:Example-User/Sample-Project.git": "github.com/example-user/sample-project",
		"git@gitlab.com:group/sub/repo.git":              "gitlab.com/group/sub/repo",

		// HTTPS form
		"https://github.com/Example-User/Sample-Project.git": "github.com/example-user/sample-project",
		"https://gitlab.com/group/sub/repo.git":              "gitlab.com/group/sub/repo",
		"https://gitlab.com/group/sub/repo":                  "gitlab.com/group/sub/repo",

		// HTTP form (self-hosted gitlab on plain http)
		"http://gitlab.example.com/group/repo.git": "gitlab.example.com/group/repo",

		// git:// protocol
		"git://github.com/owner/repo.git": "github.com/owner/repo",

		// ssh:// explicit
		"ssh://git@github.com/owner/repo.git": "github.com/owner/repo",

		// git+ssh:// variant
		"git+ssh://git@host.example.com/owner/repo.git": "host.example.com/owner/repo",

		// Port number in URL (don't treat as host:path)
		"https://gitlab.example.com:8080/group/repo.git": "gitlab.example.com:8080/group/repo",

		// Trailing slash
		"https://github.com/owner/repo/": "github.com/owner/repo",

		// Empty / garbage
		"": "",
	}
	for in, want := range cases {
		got := normalizeGitURL(in)
		require.Equal(t, want, got, "normalizeGitURL(%q)", in)
	}
}
