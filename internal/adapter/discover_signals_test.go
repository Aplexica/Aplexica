package adapter

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/aplexica/aplexica/internal/adapter/adaptertest"
)

// The daemon runs under launchd / a Windows scheduled task with a minimal
// PATH (`/usr/bin:/bin:/usr/sbin:/sbin`), so exec.LookPath alone misses
// agents installed in user-level locations (~/.local/bin, npm prefixes,
// app-managed dirs), silently dropping an adapter from the whole sync pipeline.
// Install detection must also accept agent-authored
// marker files under the global root and executables in <home>/.local/bin.

func TestProbeGlobalRootWithInstallSignals_MarkerFileCountsAsInstalled(t *testing.T) {
	adaptertest.WithoutCommands(t)
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".codex", "auth.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	d := ProbeGlobalRootWithInstallSignals(home, ".codex", []string{"auth.json"}, "codex")
	if !d.Installed {
		t.Fatalf("agent-authored marker file must count as installed even with a bare PATH: %+v", d)
	}
	if len(d.GlobalRoots) != 1 || d.GlobalRoots[0] != filepath.Join(home, ".codex") {
		t.Errorf("GlobalRoots = %v, want the probed root", d.GlobalRoots)
	}
}

func TestProbeGlobalRootWithInstallSignals_NoSignalsIsNotInstalled(t *testing.T) {
	adaptertest.WithoutCommands(t)
	home := t.TempDir()
	// Only files Aplexica itself materializes — no agent-authored markers.
	if err := os.MkdirAll(filepath.Join(home, ".codex", "sessions"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".codex", "AGENTS.md"), []byte("synced"), 0o644); err != nil {
		t.Fatal(err)
	}
	d := ProbeGlobalRootWithInstallSignals(home, ".codex", []string{"auth.json"}, "codex")
	if d.Installed {
		t.Fatalf("Aplexica-created native folders alone must not count as installed: %+v", d)
	}
}

func TestProbeGlobalRootWithInstallSignals_HomeLocalBinExecutable(t *testing.T) {
	adaptertest.WithoutCommands(t)
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	binDir := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	name := "codex"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if err := os.WriteFile(filepath.Join(binDir, name), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	d := ProbeGlobalRootWithInstallSignals(home, ".codex", nil, "codex")
	if !d.Installed {
		t.Fatalf("an executable in <home>/.local/bin must count as installed: %+v", d)
	}
}

func TestProbeGlobalRootWithInstallSignals_PathExecutableStillWorks(t *testing.T) {
	adaptertest.WithCommand(t, "codex")
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	d := ProbeGlobalRootWithInstallSignals(home, ".codex", nil, "codex")
	if !d.Installed {
		t.Fatalf("a PATH-resolvable executable must count as installed: %+v", d)
	}
}
