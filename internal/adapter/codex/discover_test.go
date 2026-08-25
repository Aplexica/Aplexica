package codex

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aplexica/aplexica/internal/adapter/adaptertest"
)

func TestDiscover_DirectoryWithoutExecutableIsNotInstalled(t *testing.T) {
	adaptertest.WithoutCommands(t)
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".codex", "AGENTS.md"), []byte("synced"), 0o644); err != nil {
		t.Fatal(err)
	}
	d, err := (&Adapter{HomeDir: home}).Discover()
	if err != nil {
		t.Fatal(err)
	}
	if d.Installed {
		t.Errorf("codex should not be Installed when only Aplexica-created native files exist: %+v", d)
	}
	if len(d.GlobalRoots) != 0 || len(d.RecursiveRoots) != 0 {
		t.Errorf("roots = %v / %v, want empty when executable is missing", d.GlobalRoots, d.RecursiveRoots)
	}
}

func TestDiscover_Present(t *testing.T) {
	codexPath := adaptertest.WithCommand(t, "codex")
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	d, err := (&Adapter{HomeDir: home, CLIExecutablePaths: []string{codexPath}}).Discover()
	if err != nil {
		t.Fatal(err)
	}
	if !d.Installed || len(d.GlobalRoots) != 1 || d.GlobalRoots[0] != filepath.Join(home, ".codex") {
		t.Errorf("unexpected discovery: %+v", d)
	}
}

func TestDiscover_SessionsIsRecursiveRoot(t *testing.T) {
	codexPath := adaptertest.WithCommand(t, "codex")
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".codex", "sessions", "2026", "06", "01"), 0o755); err != nil {
		t.Fatal(err)
	}
	d, err := (&Adapter{HomeDir: home, CLIExecutablePaths: []string{codexPath}}).Discover()
	if err != nil {
		t.Fatal(err)
	}
	if !d.Installed {
		t.Fatalf("expected installed")
	}
	want := filepath.Join(home, ".codex", "sessions")
	found := false
	for _, r := range d.RecursiveRoots {
		if r == want {
			found = true
		}
	}
	if !found {
		t.Errorf("RecursiveRoots %v should contain %q (codex sessions are date-nested)", d.RecursiveRoots, want)
	}
	// sessions must NOT also be a flat GlobalRoot.
	for _, r := range d.GlobalRoots {
		if r == want {
			t.Errorf("sessions should be a RecursiveRoot, not a flat GlobalRoot")
		}
	}
}

func TestDiscover_ReportsDesktopWorktreesWithoutWatchingThem(t *testing.T) {
	adaptertest.WithoutCommands(t)
	home := t.TempDir()
	root := filepath.Join(home, ".codex", "worktrees")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".codex", "auth.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	d, err := (&Adapter{HomeDir: home}).Discover()
	if err != nil {
		t.Fatal(err)
	}
	if !d.Installed || !strings.Contains(d.Detail, "Desktop worktrees present") {
		t.Fatalf("expected Codex Desktop diagnostic, got %+v", d)
	}
	for _, watched := range append(append([]string(nil), d.GlobalRoots...), d.RecursiveRoots...) {
		if watched == root {
			t.Fatalf("app-owned worktrees must not be watched as an import root: %+v", d)
		}
	}
}

func TestDiscover_Absent(t *testing.T) {
	d, err := (&Adapter{HomeDir: t.TempDir()}).Discover()
	if err != nil {
		t.Fatal(err)
	}
	if d.Installed {
		t.Errorf("codex should not be Installed when ~/.codex is missing")
	}
}

// The daemon's launchd/scheduled-task PATH is minimal, so a real codex
// install (npm/standalone in a user-level bin) is invisible to LookPath.
// Codex-authored files under ~/.codex must count as the install signal —
// otherwise the adapter silently drops out of the whole sync pipeline.
func TestDiscover_AgentAuthoredMarkerWithoutPathExecutable(t *testing.T) {
	adaptertest.WithoutCommands(t)
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".codex", "sessions", "2026", "07", "08"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".codex", "auth.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	d, err := (&Adapter{HomeDir: home}).Discover()
	if err != nil {
		t.Fatal(err)
	}
	if !d.Installed {
		t.Fatalf("codex-authored auth.json must count as installed on a bare PATH: %+v", d)
	}
	want := filepath.Join(home, ".codex", "sessions")
	found := false
	for _, r := range d.RecursiveRoots {
		if r == want {
			found = true
		}
	}
	if !found {
		t.Errorf("RecursiveRoots %v should contain %q", d.RecursiveRoots, want)
	}
}
