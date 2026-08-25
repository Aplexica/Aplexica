package openclaw

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/aplexica/aplexica/internal/adapter/adaptertest"
)

func TestDiscover_DirectoryWithoutExecutableIsNotInstalled(t *testing.T) {
	adaptertest.WithoutCommands(t)
	home := t.TempDir()
	ws := filepath.Join(home, ".openclaw", "workspace")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "MEMORY"), []byte("synced"), 0o644); err != nil {
		t.Fatal(err)
	}
	d, err := (&Adapter{HomeDir: home}).Discover()
	if err != nil {
		t.Fatal(err)
	}
	if d.Installed {
		t.Errorf("openclaw should not be Installed when only Aplexica-created native files exist: %+v", d)
	}
	if len(d.GlobalRoots) != 0 {
		t.Errorf("GlobalRoots = %v, want empty when executable is missing", d.GlobalRoots)
	}
}

func TestDiscover_Present(t *testing.T) {
	adaptertest.WithCommand(t, "openclaw")
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".openclaw"), 0o755); err != nil {
		t.Fatal(err)
	}
	d, err := (&Adapter{HomeDir: home}).Discover()
	if err != nil {
		t.Fatal(err)
	}
	if !d.Installed || len(d.GlobalRoots) != 1 || d.GlobalRoots[0] != filepath.Join(home, ".openclaw") {
		t.Errorf("unexpected discovery: %+v", d)
	}
}

func TestDiscover_Absent(t *testing.T) {
	d, err := (&Adapter{HomeDir: t.TempDir()}).Discover()
	if err != nil {
		t.Fatal(err)
	}
	if d.Installed {
		t.Errorf("openclaw should not be Installed when ~/.openclaw is missing")
	}
}

// Regression for E2E F2: ~/.openclaw is watched FLAT, but the memory and
// config artifacts live in the workspace/ SUBDIR — without advertising it
// as its own root, edits made by openclaw never import (its memory sync
// was silently export-only, the same gap hermes had with memories/).
func TestDiscover_IncludesWorkspaceSubdir(t *testing.T) {
	adaptertest.WithCommand(t, "openclaw")
	home := t.TempDir()
	ws := filepath.Join(home, ".openclaw", "workspace")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	d, err := (&Adapter{HomeDir: home}).Discover()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range d.GlobalRoots {
		if r == ws {
			found = true
		}
	}
	if !found {
		t.Errorf("workspace subdir must be a global root (flat watcher can't see into it); got %+v", d.GlobalRoots)
	}
}

// OpenClaw is commonly installed into an app-managed node prefix (e.g. the
// Hermes-bundled runtime), invisible to the daemon's minimal PATH. Its own
// config file must count as the install signal.
func TestDiscover_AgentAuthoredMarkerWithoutPathExecutable(t *testing.T) {
	adaptertest.WithoutCommands(t)
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".openclaw"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".openclaw", "openclaw.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	d, err := (&Adapter{HomeDir: home}).Discover()
	if err != nil {
		t.Fatal(err)
	}
	if !d.Installed {
		t.Fatalf("openclaw-authored openclaw.json must count as installed on a bare PATH: %+v", d)
	}
}
