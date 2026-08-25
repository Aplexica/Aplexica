package main

import (
	"os"
	"path/filepath"
	"testing"
)

// The companion test that asserted persistentExecutable rejected an
// untrusted stable-launcher environment value is gone with the
// direct-install launcher: there is no environment override left to
// reject. What remains worth pinning is that the path is absolute, as the
// writers (daemon install, tray install) embed it verbatim and a relative
// path would resolve against whatever working directory the service
// manager happens to start in.
func TestPersistentExecutableReportsAbsoluteRunningProgram(t *testing.T) {
	got, err := persistentExecutable()
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(got) {
		t.Fatalf("persistentExecutable = %q, want an absolute path", got)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.Abs(exe)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("persistentExecutable = %q, want %q", got, want)
	}
}
