package project

import (
	"bufio"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// ErrNoOrigin is returned by readGitOriginURL when a git repo has no
// remote origin configured (a fresh local repo, or one with only
// non-origin remotes). Callers fall back to path-derived IDs.
var ErrNoOrigin = errors.New("project: git remote origin not configured")

// scanBufInit / scanBufMax size the bufio.Scanner used to read VCS config
// files. The default scanner caps a token (line) at 64 KiB and returns
// bufio.ErrTooLong past that; a legitimate `url.<base>.insteadOf` rewrite, an
// embedded credential blob, or a corrupted config can exceed it. We start at
// the same 64 KiB and allow growth to 1 MiB so ordinary-but-long url lines
// parse, while a truly pathological line still surfaces a real error (which the
// caller distinguishes from the absent-origin sentinel) instead of being
// silently treated as "no origin".
const (
	scanBufInit = 64 * 1024 // initial scanner buffer (bytes)
	scanBufMax  = 1 << 20   // max scanner token size: 1 MiB
)

// readGitOriginURL reads .git/config in repoDir and returns the URL
// of the [remote "origin"] section's url= line. Returns ErrNoOrigin
// when the file exists but origin isn't configured. Handles both
// regular `.git/` directories and worktree `.git` pointer files.
//
// Implementation uses a minimal hand-rolled INI parser (no third-
// party dep). .git/config has a well-defined section format:
//
//	[section "subsection"]
//	  key = value
//
// We only care about [remote "origin"]'s url=.
func readGitOriginURL(repoDir string) (string, error) {
	f, err := os.Open(filepath.Join(repoDir, ".git", "config"))
	if err != nil {
		// .git might be a file (worktree pointer) — follow it.
		return readGitOriginURLViaWorktree(repoDir)
	}
	defer f.Close()
	return scanForOriginURL(f)
}

// readGitOriginURLViaWorktree handles the .git-as-file case: the file
// contains `gitdir: <path-to-real-gitdir>`; we open <path>/config.
func readGitOriginURLViaWorktree(repoDir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(repoDir, ".git"))
	if err != nil {
		return "", err
	}
	s := strings.TrimSpace(string(data))
	if !strings.HasPrefix(s, "gitdir:") {
		return "", ErrNoOrigin
	}
	gitDir := strings.TrimSpace(s[len("gitdir:"):])
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(repoDir, gitDir)
	}
	f, err := os.Open(filepath.Join(gitDir, "config"))
	if err != nil {
		return "", err
	}
	defer f.Close()
	return scanForOriginURL(f)
}

// scanForOriginURL is the shared INI scan. Reads lines, tracks
// section header, returns the url= value when inside [remote "origin"].
func scanForOriginURL(r interface {
	Read(p []byte) (int, error)
}) (string, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, scanBufInit), scanBufMax)
	inOrigin := false
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "[") {
			inOrigin = line == `[remote "origin"]`
			continue
		}
		if !inOrigin {
			continue
		}
		// Match "url = X" / "url=X" / "url   =   X" but NOT "urlspecial=...".
		if strings.HasPrefix(line, "url") && len(line) > 3 {
			next := line[3]
			if next != ' ' && next != '\t' && next != '=' {
				continue
			}
			if eq := strings.IndexByte(line, '='); eq >= 0 {
				return strings.TrimSpace(line[eq+1:]), nil
			}
		}
	}
	// A scanner error (e.g. bufio.ErrTooLong on a line past scanBufMax, or an
	// underlying read failure) must be reported, NOT folded into ErrNoOrigin —
	// the caller has to tell a real read failure apart from a genuinely
	// origin-less repo, otherwise a corrupted config silently downgrades to a
	// path-derived ID.
	if err := sc.Err(); err != nil {
		return "", err
	}
	return "", ErrNoOrigin
}

// normalizeGitURL canonicalizes a git remote URL into the form
// "host/owner/repo" per BRD-02 §4.13.1: lowercase host, no .git
// suffix, no protocol.
//
// Supported input forms:
//
//	ssh:    git@github.com:owner/repo.git
//	ssh+:   ssh://git@github.com/owner/repo.git
//	https:  https://github.com/owner/repo.git
//	http:   http://gitlab.example.com/group/sub/repo.git
//	git:    git://host/owner/repo
//	git+ssh: git+ssh://host/owner/repo.git
//
// Returns empty string on unparseable input.
func normalizeGitURL(u string) string {
	normalized, err := normalizeRemoteURL(u, VCSGit)
	if err != nil {
		return ""
	}
	return normalized
}
