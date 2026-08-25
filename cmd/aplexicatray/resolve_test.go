//go:build tray

package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestResolveAplexicaPath_ExplicitAbsolutePathHonored(t *testing.T) {
	abs := filepath.Join(t.TempDir(), "custom-aplexica")
	if got := resolveAplexicaPath(abs); got != abs {
		t.Errorf("explicit abs path = %q, want %q", got, abs)
	}
}

func TestResolveAplexicaPath_ExplicitRelativeWithSeparatorHonored(t *testing.T) {
	rel := filepath.Join("some", "dir", "aplexica")
	if got := resolveAplexicaPath(rel); got != rel {
		t.Errorf("explicit rel path = %q, want %q", got, rel)
	}
}

// TestResolveAplexicaPath_SiblingPreferred is the regression test for
// the launchd-PATH bug: the bare "aplexica" default must resolve to a
// sibling next to the tray's own executable, NOT a $PATH lookup, so it
// works under a LaunchAgent whose PATH omits the install dir.
//
// We can't relocate the test binary's os.Executable(), but we CAN
// verify the sibling-resolution branch by exercising it through a
// helper that mirrors the logic against a controlled directory.
func TestResolveAplexicaPath_SiblingResolution(t *testing.T) {
	dir := t.TempDir()
	name := "aplexica"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	sibling := filepath.Join(dir, name)
	if err := os.WriteFile(sibling, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	// resolveSiblingIn mirrors resolveAplexicaPath's step 2 against an
	// injectable exe-dir so the test doesn't depend on where `go test`
	// places its binary.
	got := resolveSiblingIn(dir)
	if got != sibling {
		t.Errorf("sibling resolution = %q, want %q", got, sibling)
	}
}

func TestResolveAplexicaPath_FallsBackWhenNoSibling(t *testing.T) {
	// An empty dir has no sibling; resolveSiblingIn returns "" so the
	// caller falls through to $PATH / literal.
	if got := resolveSiblingIn(t.TempDir()); got != "" {
		t.Errorf("no-sibling dir returned %q, want empty", got)
	}
}

func TestResolveStatusWatchPath_PrefersSiblingStatusHelper(t *testing.T) {
	dir := t.TempDir()
	aplexica := filepath.Join(dir, "aplexica")
	helper := filepath.Join(dir, statusHelperBinaryName())
	if err := os.WriteFile(aplexica, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(helper, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	if got := resolveStatusWatchPath(aplexica); got != helper {
		t.Errorf("status helper = %q, want %q", got, helper)
	}
}

func TestResolveStatusWatchPath_FallsBackToAplexica(t *testing.T) {
	dir := t.TempDir()
	aplexica := filepath.Join(dir, "aplexica")
	if runtime.GOOS == "windows" {
		aplexica += ".exe"
	}
	if err := os.WriteFile(aplexica, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", dir)
	if got := resolveStatusWatchPath(aplexica); got != aplexica {
		t.Errorf("missing status helper = %q, want fallback %q", got, aplexica)
	}
}
