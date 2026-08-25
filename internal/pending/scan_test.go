package pending

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/aplexica/aplexica/internal/project"
	"github.com/stretchr/testify/require"
)

func TestScan_EmptyRoot_ReturnsEmpty(t *testing.T) {
	dir := t.TempDir()
	got, err := Scan([]string{dir}, 5)
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestScan_FindsGitRepo(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "myrepo")
	require.NoError(t, os.MkdirAll(filepath.Join(repo, ".git"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(repo, ".git", "config"),
		[]byte(`[remote "origin"]
	url = git@github.com:owner/repo.git
`), 0o644))

	got, err := Scan([]string{root}, 5)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, repo, got[0].Path)
	require.Equal(t, "github.com/owner/repo", got[0].Info.ID)
	require.Equal(t, "git", got[0].Info.VCS)
}

func TestScan_FindsHgRepo(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "hgrepo")
	require.NoError(t, os.MkdirAll(filepath.Join(repo, ".hg"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(repo, ".hg", "hgrc"),
		[]byte(`[paths]
default = https://bitbucket.org/owner/repo
`), 0o644))

	got, err := Scan([]string{root}, 5)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "bitbucket.org/owner/repo", got[0].Info.ID)
	require.Equal(t, "hg", got[0].Info.VCS)
}

func TestScan_StopsAtVCSBoundary(t *testing.T) {
	// A repo with a nested non-repo dir — Scan must NOT descend into
	// the repo and emit a phantom result for the inner dir.
	root := t.TempDir()
	repo := filepath.Join(root, "outer")
	require.NoError(t, os.MkdirAll(filepath.Join(repo, ".git"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(repo, "subdir", "deeper"), 0o755))

	got, err := Scan([]string{root}, 10)
	require.NoError(t, err)
	require.Len(t, got, 1, "scan must stop at the repo boundary, not recurse into subdirs")
	require.Equal(t, repo, got[0].Path)
}

func TestScan_SkipsDotDirs(t *testing.T) {
	root := t.TempDir()
	// A repo inside a dot-directory MUST be skipped (~/.cache,
	// ~/.npm, etc. can have huge trees and irrelevant repos).
	hidden := filepath.Join(root, ".cache", "irrelevant")
	require.NoError(t, os.MkdirAll(filepath.Join(hidden, ".git"), 0o755))
	got, err := Scan([]string{root}, 5)
	require.NoError(t, err)
	require.Empty(t, got, "repos under dot-dirs must be skipped")
}

func TestScan_DotPrefixedRoot_IsScanned(t *testing.T) {
	// A scan root whose own basename is dot-prefixed (e.g. ~/.config,
	// a checked-out dotfiles dir) must still be entered — shouldSkip's
	// doc comment promises "allow the root itself if it happens to be
	// one". A repo one level under the dot-root must be detected.
	root := filepath.Join(t.TempDir(), ".config")
	repo := filepath.Join(root, "myrepo")
	require.NoError(t, os.MkdirAll(filepath.Join(repo, ".git"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(repo, ".git", "config"),
		[]byte(`[remote "origin"]
	url = git@github.com:owner/repo.git
`), 0o644))

	got, err := Scan([]string{root}, 5)
	require.NoError(t, err)
	require.Len(t, got, 1, "a dot-prefixed scan root must be entered, not skipped")
	require.Equal(t, repo, got[0].Path)
	require.Equal(t, "github.com/owner/repo", got[0].Info.ID)
}

func TestScan_DotfilesRepoAsRoot_IsDetected(t *testing.T) {
	// A dotfiles repo passed directly as the scan root (the root itself
	// is the VCS root AND is dot-prefixed) must be detected, not
	// silently skipped before the VCS-marker check.
	root := filepath.Join(t.TempDir(), ".dotfiles")
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".git"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".git", "config"),
		[]byte(`[remote "origin"]
	url = git@github.com:owner/dotfiles.git
`), 0o644))

	got, err := Scan([]string{root}, 5)
	require.NoError(t, err)
	require.Len(t, got, 1, "a dot-prefixed dotfiles repo passed as the root must be detected")
	require.Equal(t, root, got[0].Path)
	require.Equal(t, "github.com/owner/dotfiles", got[0].Info.ID)
}

func TestScan_SkipsCommonLargeDirs(t *testing.T) {
	root := t.TempDir()
	// node_modules under root: must skip
	nmRepo := filepath.Join(root, "node_modules", "vendored-pkg")
	require.NoError(t, os.MkdirAll(filepath.Join(nmRepo, ".git"), 0o755))
	// Real repo at root: must find
	realRepo := filepath.Join(root, "real")
	require.NoError(t, os.MkdirAll(filepath.Join(realRepo, ".git"), 0o755))

	got, err := Scan([]string{root}, 5)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, realRepo, got[0].Path)
}

func TestScan_DepthLimit(t *testing.T) {
	root := t.TempDir()
	// Deep repo at depth=3: foo/bar/baz/repo
	repo := filepath.Join(root, "foo", "bar", "baz", "deepRepo")
	require.NoError(t, os.MkdirAll(filepath.Join(repo, ".git"), 0o755))

	// maxDepth=2 should NOT find it
	got, err := Scan([]string{root}, 2)
	require.NoError(t, err)
	require.Empty(t, got, "depth=2 must not reach depth-4 repo")

	// maxDepth=5 finds it
	got, err = Scan([]string{root}, 5)
	require.NoError(t, err)
	require.Len(t, got, 1)
}

func TestScan_DedupRoots(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "r")
	require.NoError(t, os.MkdirAll(filepath.Join(repo, ".git"), 0o755))

	// Pass the same root twice — must not duplicate the detection.
	got, err := Scan([]string{root, root}, 5)
	require.NoError(t, err)
	require.Len(t, got, 1)
}

func TestMatchPending_NoMatches(t *testing.T) {
	d := []Detected{{Path: "/x", Info: project.ProjectInfo{ID: "github.com/x/y"}}}
	p := []Project{{ID: "github.com/other/zzz"}}
	require.Empty(t, MatchPending(d, p))
}

func TestMatchPending_OneMatch(t *testing.T) {
	d := []Detected{
		{Path: "/a", Info: project.ProjectInfo{ID: "github.com/x/y"}},
		{Path: "/b", Info: project.ProjectInfo{ID: "github.com/other/yy"}},
	}
	p := []Project{{ID: "github.com/x/y", ArtifactCount: 3}}
	got := MatchPending(d, p)
	require.Len(t, got, 1)
	require.Equal(t, "/a", got[0].Path)
}

func TestMatchPending_NilInputs(t *testing.T) {
	require.Nil(t, MatchPending(nil, nil))
	require.Nil(t, MatchPending(nil, []Project{{ID: "x"}}))
	require.Nil(t, MatchPending([]Detected{{Info: project.ProjectInfo{ID: "x"}}}, nil))
}

// TestMacProtectedHomeDir verifies the macOS TCC-protected home folders are
// recognized (so the scan never descends into them and triggers a privacy
// prompt), while real code locations like ~/Documents and project subdirs
// are NOT excluded.
func TestMacProtectedHomeDir(t *testing.T) {
	home := filepath.Join("/Users", "alice")
	protected := []string{"Desktop", "Downloads", "Music", "Movies", "Pictures", "Library", "Public", "Applications"}
	for _, name := range protected {
		require.Truef(t, macProtectedHomeDir(filepath.Join(home, name), home),
			"%s must be treated as a protected home dir", name)
	}
	// Documents is a common code location — kept scannable.
	require.False(t, macProtectedHomeDir(filepath.Join(home, "Documents"), home),
		"Documents must remain scannable")
	// Only the exact top-level dir, never children or a same-named project dir.
	require.False(t, macProtectedHomeDir(filepath.Join(home, "Music", "myrepo"), home),
		"a child of a protected dir is not itself the protected dir")
	require.False(t, macProtectedHomeDir(filepath.Join(home, "code", "Music"), home),
		"a same-named dir elsewhere is not protected")
	require.False(t, macProtectedHomeDir(filepath.Join(home, "Desktop"), ""),
		"empty home -> nothing protected")
}
