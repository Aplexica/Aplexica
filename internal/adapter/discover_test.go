package adapter

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/aplexica/aplexica/internal/adapter/adaptertest"
)

func TestProbeGlobalRoot_Present(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	d := ProbeGlobalRoot(home, ".claude")
	if !d.Installed {
		t.Fatalf("expected Installed=true for existing dir")
	}
	if len(d.GlobalRoots) != 1 || d.GlobalRoots[0] != filepath.Join(home, ".claude") {
		t.Errorf("GlobalRoots = %v, want [%s]", d.GlobalRoots, filepath.Join(home, ".claude"))
	}
}

func TestProbeGlobalRoot_Absent(t *testing.T) {
	home := t.TempDir() // no .codex created
	d := ProbeGlobalRoot(home, ".codex")
	if d.Installed {
		t.Errorf("expected Installed=false for missing dir")
	}
	if len(d.GlobalRoots) != 0 {
		t.Errorf("GlobalRoots = %v, want empty", d.GlobalRoots)
	}
}

func TestProbeGlobalRoot_FileNotDir(t *testing.T) {
	home := t.TempDir()
	// A regular file at the root path must NOT count as installed.
	if err := os.WriteFile(filepath.Join(home, ".hermes"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if ProbeGlobalRoot(home, ".hermes").Installed {
		t.Errorf("a file (not dir) at root must not count as installed")
	}
}

func TestProbeGlobalRoot_EmptyHome(t *testing.T) {
	if ProbeGlobalRoot("", ".claude").Installed {
		t.Errorf("empty home must yield Installed=false")
	}
}

func TestProbeGlobalRootWithExecutable_MissingExecutableClearsRoots(t *testing.T) {
	adaptertest.WithoutCommands(t)
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	d := ProbeGlobalRootWithExecutable(home, ".codex", "codex")
	if d.Installed {
		t.Fatalf("expected missing executable to force Installed=false: %+v", d)
	}
	if len(d.GlobalRoots) != 0 {
		t.Fatalf("GlobalRoots = %v, want empty", d.GlobalRoots)
	}
}

func TestProbeGlobalRootWithExecutable_Present(t *testing.T) {
	adaptertest.WithCommand(t, "codex")
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	d := ProbeGlobalRootWithExecutable(home, ".codex", "codex")
	if !d.Installed {
		t.Fatalf("expected Installed=true with root and executable: %+v", d)
	}
}
