package project

import (
	"bufio"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// ErrNoHgDefault is returned by readHgDefaultURL when the repo has no
// `[paths] default = ...` entry in `.hg/hgrc`. Equivalent to git's
// "no origin remote configured" case. Callers fall back to
// path-derived IDs.
var ErrNoHgDefault = errors.New("project: hg default path not configured")

// readHgDefaultURL reads .hg/hgrc in repoDir and returns the URL of
// the `default` entry in the `[paths]` section. Implements a minimal
// INI parser hand-rolled here (same approach as readGitOriginURL —
// no third-party dep).
//
// `.hg/hgrc` syntax (mercurial.ini-style):
//
//	[paths]
//	default = ssh://hg@bitbucket.org/owner/repo
//	default-push = ssh://hg@bitbucket.org/owner/repo
//
// We only care about [paths] default. Other paths (default-push,
// named upstreams) are ignored — `default` is the canonical
// pull/push target.
func readHgDefaultURL(repoDir string) (string, error) {
	f, err := os.Open(filepath.Join(repoDir, ".hg", "hgrc"))
	if err != nil {
		return "", err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	// Same buffer raise as scanForOriginURL (scanBufInit/scanBufMax live in
	// git.go, same package): an hg default path can carry a long URL.
	sc.Buffer(make([]byte, 0, scanBufInit), scanBufMax)
	inPaths := false
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			inPaths = strings.EqualFold(line, "[paths]")
			continue
		}
		if !inPaths {
			continue
		}
		// Match "default = X" / "default=X" / "default   =   X". Reject
		// "default-push = ..." and other prefixed keys — we want the
		// exact "default" identifier on the LHS.
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		if !strings.EqualFold(key, "default") {
			continue
		}
		val := strings.TrimSpace(line[eq+1:])
		if val != "" {
			return val, nil
		}
	}
	// Report scanner errors (overlong line / read failure) instead of folding
	// them into ErrNoHgDefault — see the matching note in scanForOriginURL.
	if err := sc.Err(); err != nil {
		return "", err
	}
	return "", ErrNoHgDefault
}

// normalizeHgURL canonicalizes a mercurial remote URL into the same
// "host/owner/repo" shape git remotes get from normalizeGitURL —
// uniform identity across VCS types so a tool reading a
// project.ProjectInfo doesn't have to special-case hg vs git in its
// downstream code.
//
// Supported input forms (mirror normalizeGitURL plus hg-specific):
//
//	ssh:    hg@bitbucket.org:owner/repo
//	ssh+:   ssh://hg@bitbucket.org/owner/repo
//	https:  https://bitbucket.org/owner/repo
//	http:   http://hg.example.com/owner/repo
//	plain:  bitbucket.org/owner/repo  (no scheme)
//
// Returns empty string on unparseable input.
func normalizeHgURL(u string) string {
	normalized, err := normalizeRemoteURL(u, VCSHg)
	if err != nil {
		return ""
	}
	return normalized
}
