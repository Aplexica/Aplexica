package projectdiscovery

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestPathPolicyRejectsSymlinkAliasIntoExcludedRoot(t *testing.T) {
	base := t.TempDir()
	excluded := filepath.Join(base, ".agent")
	project := filepath.Join(excluded, "sessions", "repo")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(base, "looks-safe")
	if err := os.Symlink(project, alias); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("Windows runner cannot create the reparse-point fixture: %v", err)
		}
		t.Fatal(err)
	}
	_, err := (PathPolicy{StateDir: filepath.Join(base, "state"), ExcludeRoots: []string{excluded}}).ResolveCandidate(alias)
	if err == nil {
		t.Fatal("symlink alias into excluded root accepted")
	}
}

func TestPathPolicyDedupIdentityAndRevalidate(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "project")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	got, err := (PathPolicy{StateDir: filepath.Join(base, "state")}).ResolveCandidate(real)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(real)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := (PathPolicy{}).RevalidateOpenedDirectory(f, got.Identity); err != nil {
		t.Fatal(err)
	}
	wrong := got.Identity
	wrong.UnixInode++
	if err := (PathPolicy{}).RevalidateOpenedDirectory(f, wrong); err == nil {
		t.Fatal("identity change accepted")
	}
}
