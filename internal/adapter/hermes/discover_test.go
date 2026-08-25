package hermes

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/aplexica/aplexica/internal/adapter/adaptertest"
)

func TestDiscover_DirectoryWithoutExecutableIsNotInstalled(t *testing.T) {
	adaptertest.WithoutCommands(t)
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".hermes", "memories"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".hermes", "memories", "MEMORY.md"), []byte("synced"), 0o644); err != nil {
		t.Fatal(err)
	}
	d, err := (&Adapter{HomeDir: home}).Discover()
	if err != nil {
		t.Fatal(err)
	}
	if d.Installed {
		t.Errorf("hermes should not be Installed when only Aplexica-created native files exist: %+v", d)
	}
	if len(d.GlobalRoots) != 0 {
		t.Errorf("GlobalRoots = %v, want empty when executable is missing", d.GlobalRoots)
	}
}

func TestDiscover_Present(t *testing.T) {
	adaptertest.WithCommand(t, "hermes")
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".hermes"), 0o755); err != nil {
		t.Fatal(err)
	}
	d, err := (&Adapter{HomeDir: home}).Discover()
	if err != nil {
		t.Fatal(err)
	}
	if !d.Installed || len(d.GlobalRoots) != 1 || d.GlobalRoots[0] != filepath.Join(home, ".hermes") {
		t.Errorf("unexpected discovery: %+v", d)
	}
}

func TestDiscover_Absent(t *testing.T) {
	d, err := (&Adapter{HomeDir: t.TempDir()}).Discover()
	if err != nil {
		t.Fatal(err)
	}
	if d.Installed {
		t.Errorf("hermes should not be Installed when ~/.hermes is missing")
	}
}

// Hermes is a GUI app whose CLI shim lives in an app-managed node prefix, not
// on the daemon's minimal PATH. Hermes-authored files under ~/.hermes must
// count as the install signal so the adapter stays in the sync pipeline.
func TestDiscover_AgentAuthoredMarkerWithoutPathExecutable(t *testing.T) {
	adaptertest.WithoutCommands(t)
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".hermes"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".hermes", "auth.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	d, err := (&Adapter{HomeDir: home}).Discover()
	if err != nil {
		t.Fatal(err)
	}
	if !d.Installed {
		t.Fatalf("hermes-authored auth.json must count as installed on a bare PATH: %+v", d)
	}
}
