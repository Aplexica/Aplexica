package adaptertest

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// WithoutCommands isolates PATH so discovery tests do not depend on commands
// installed on the developer or CI machine.
func WithoutCommands(t testing.TB) {
	t.Helper()
	t.Setenv("PATH", t.TempDir())
}

// WithCommand isolates PATH and installs a fake command with the requested
// name. exec.LookPath only stats the file, so the Windows file does not need to
// be a real PE executable.
func WithCommand(t testing.TB, name string) string {
	t.Helper()
	dir := t.TempDir()
	fileName := name
	content := []byte("#!/bin/sh\nexit 0\n")
	if runtime.GOOS == "windows" {
		fileName += ".exe"
		content = nil
	}
	path := filepath.Join(dir, fileName)
	if err := os.WriteFile(path, content, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	return path
}
