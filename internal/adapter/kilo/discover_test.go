package kilo

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/aplexica/aplexica/internal/adapter/adaptertest"
)

func TestDiscover_AbsentByDefault(t *testing.T) {
	adaptertest.WithoutCommands(t)
	d, err := (&Adapter{HomeDir: t.TempDir()}).Discover()
	if err != nil {
		t.Fatal(err)
	}
	if d.Installed {
		t.Errorf("kilo should not be Installed when ~/.kilo is missing (project-scoped only)")
	}
	if d.Detail == "" {
		t.Errorf("expected a Detail note explaining kilo's project-scoped nature")
	}
}

func TestDiscover_DirectoryWithoutExecutableIsNotInstalled(t *testing.T) {
	adaptertest.WithoutCommands(t)
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".kilo", "skills", "from-sync"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".kilo", "skills", "from-sync", "SKILL.md"), []byte("synced"), 0o644); err != nil {
		t.Fatal(err)
	}
	d, err := (&Adapter{HomeDir: home}).Discover()
	if err != nil {
		t.Fatal(err)
	}
	if d.Installed {
		t.Errorf("kilo should not be Installed when only Aplexica-created native files exist: %+v", d)
	}
	if len(d.GlobalRoots) != 0 || len(d.RecursiveRoots) != 0 {
		t.Errorf("roots = %v / %v, want empty when executable is missing", d.GlobalRoots, d.RecursiveRoots)
	}
}

func TestDiscover_PresentWhenKiloDirExists(t *testing.T) {
	adaptertest.WithCommand(t, "kilo")
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".kilo"), 0o755); err != nil {
		t.Fatal(err)
	}
	d, err := (&Adapter{HomeDir: home}).Discover()
	if err != nil {
		t.Fatal(err)
	}
	if !d.Installed {
		t.Errorf("kilo should be Installed when ~/.kilo exists")
	}
}

func TestDiscover_PresentWhenConfigDirExists(t *testing.T) {
	adaptertest.WithCommand(t, "kilo")
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".config", "kilo"), 0o755); err != nil {
		t.Fatal(err)
	}
	d, err := (&Adapter{HomeDir: home}).Discover()
	if err != nil {
		t.Fatal(err)
	}
	if !d.Installed {
		t.Errorf("kilo should be Installed when ~/.config/kilo exists")
	}
	if len(d.GlobalRoots) == 0 {
		t.Fatalf("expected global roots for Kilo config")
	}
}

func TestDiscover_IncludesDataRootWhenDBExists(t *testing.T) {
	adaptertest.WithCommand(t, "kilo")
	home := t.TempDir()
	dataRoot := filepath.Join(home, ".local", "share", "kilo")
	if err := os.MkdirAll(dataRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataRoot, "kilo.db"), []byte("sqlite"), 0o644); err != nil {
		t.Fatal(err)
	}

	d, err := (&Adapter{HomeDir: home}).Discover()
	if err != nil {
		t.Fatal(err)
	}
	if !d.Installed {
		t.Fatalf("kilo should be Installed when kilo.db exists")
	}
	var found bool
	for _, root := range d.GlobalRoots {
		if root == dataRoot {
			found = true
		}
	}
	if !found {
		t.Fatalf("GlobalRoots = %v, want data root %s", d.GlobalRoots, dataRoot)
	}
}
