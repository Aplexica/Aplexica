package project

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDetect_NonVCSPath_ReturnsPathDerivedID(t *testing.T) {
	dir := t.TempDir()
	got, err := Detect(dir)
	require.NoError(t, err)
	physical, err := filepath.EvalSymlinks(dir)
	require.NoError(t, err)
	require.Equal(t, "none", got.VCS)
	require.Equal(t, physical, got.Path)
	require.True(t, strings.HasPrefix(got.ID, "local:"),
		"non-VCS path should produce local:* ID, got %q", got.ID)
	require.False(t, got.Ephemeral)
}

func TestDetect_GitRepo_WithOrigin_ReturnsNormalizedURL(t *testing.T) {
	dir := t.TempDir()
	mkGitConfig(t, dir, `[core]
	repositoryformatversion = 0
[remote "origin"]
	url = git@github.com:Example-User/Sample-Project.git
	fetch = +refs/heads/*:refs/remotes/origin/*
`)
	got, err := Detect(dir)
	require.NoError(t, err)
	physical, err := filepath.EvalSymlinks(dir)
	require.NoError(t, err)
	require.Equal(t, "git", got.VCS)
	require.Equal(t, "github.com/example-user/sample-project", got.ID)
	require.Equal(t, physical, got.Path)
}

func TestDetect_GitRepo_NoOrigin_FallsBackToPathID(t *testing.T) {
	dir := t.TempDir()
	mkGitConfig(t, dir, `[core]
	repositoryformatversion = 0
`)
	got, err := Detect(dir)
	require.NoError(t, err)
	require.Equal(t, "git", got.VCS, "VCS should still report git")
	require.True(t, strings.HasPrefix(got.ID, "local:"),
		"missing origin should fall back to local:* ID, got %q", got.ID)
}

func TestDetect_HgRepo(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".hg"), 0o755))
	got, err := Detect(dir)
	require.NoError(t, err)
	require.Equal(t, "hg", got.VCS)
	require.True(t, strings.HasPrefix(got.ID, "local:"))
}

func TestDetect_WalksUp_SubdirReturnsRepoID(t *testing.T) {
	dir := t.TempDir()
	mkGitConfig(t, dir, `[remote "origin"]
	url = https://gitlab.com/group/sub/repo.git
`)
	sub := filepath.Join(dir, "deeply", "nested", "subdir")
	require.NoError(t, os.MkdirAll(sub, 0o755))
	got, err := Detect(sub)
	require.NoError(t, err)
	physical, err := filepath.EvalSymlinks(dir)
	require.NoError(t, err)
	require.Equal(t, "git", got.VCS)
	require.Equal(t, "gitlab.com/group/sub/repo", got.ID)
	require.Equal(t, physical, got.Path, "Path should be the repo root, not the input subdir")
}

func TestDetect_GitWorktreeFile(t *testing.T) {
	// A git worktree has .git as a FILE, not a directory.
	// The file contains "gitdir: <abs-path-to-real-gitdir>".
	main := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(main, "real-gitdir"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(main, "real-gitdir", "config"),
		[]byte(`[remote "origin"]
	url = git@github.com:org/repo.git
`), 0o644))

	worktree := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(worktree, ".git"),
		[]byte("gitdir: "+filepath.Join(main, "real-gitdir")+"\n"),
		0o644))

	got, err := Detect(worktree)
	require.NoError(t, err)
	require.Equal(t, "git", got.VCS)
	require.Equal(t, "github.com/org/repo", got.ID)
}

func mkGitConfig(t *testing.T, dir, contents string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".git"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, ".git", "config"),
		[]byte(contents), 0o644))
}

func TestPathDerivedID_StableAndSlugified(t *testing.T) {
	a := pathDerivedID("/Users/me/code/My Project!")
	b := pathDerivedID("/Users/me/code/My Project!")
	require.Equal(t, a, b, "same path should produce same ID")
	require.Contains(t, a, "local:")
	require.Contains(t, a, ":my-project")

	c := pathDerivedID("/Users/me/code/different")
	require.NotEqual(t, a, c)
}

func TestSanitizeDirname(t *testing.T) {
	cases := map[string]string{
		"hello":           "hello",
		"Hello World":     "hello-world",
		"!!@@##":          "",
		"a__b__c":         "a-b-c",
		"My Project!":     "my-project",
		"---leading---":   "leading",
		"trailing---":     "trailing",
		"with.dots.in.it": "with-dots-in-it",
		"under_score_too": "under-score-too",
	}
	for in, want := range cases {
		got := sanitizeDirname(in)
		require.Equal(t, want, got, "sanitizeDirname(%q)", in)
	}
}
