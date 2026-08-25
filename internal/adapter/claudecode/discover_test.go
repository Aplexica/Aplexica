package claudecode

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDiscover_Present(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	a := &Adapter{HomeDir: home}
	d, err := a.Discover()
	if err != nil {
		t.Fatal(err)
	}
	if !d.Installed {
		t.Fatalf("claude-code should be Installed when ~/.claude exists")
	}
	if len(d.GlobalRoots) != 1 || d.GlobalRoots[0] != filepath.Join(home, ".claude") {
		t.Errorf("GlobalRoots = %v", d.GlobalRoots)
	}
}

func TestDiscover_ReportsDesktopSurfaceWithoutClaimingCatalog(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, ".claude")
	catalog := filepath.Join(home, "desktop-sessions")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(catalog, 0o755); err != nil {
		t.Fatal(err)
	}

	d, err := (&Adapter{HomeDir: home, DesktopSessionRoots: []string{catalog}}).Discover()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(d.Detail, "Desktop surface present") {
		t.Fatalf("Detail = %q, want Desktop surface diagnostic", d.Detail)
	}
	for _, watched := range append(append([]string{}, d.GlobalRoots...), d.RecursiveRoots...) {
		if watched == catalog {
			t.Fatalf("app-owned Desktop catalog must stay metadata-only/unbacked, roots = %v / %v", d.GlobalRoots, d.RecursiveRoots)
		}
	}
}

func TestDiscover_Absent(t *testing.T) {
	a := &Adapter{HomeDir: t.TempDir()}
	d, err := a.Discover()
	if err != nil {
		t.Fatal(err)
	}
	if d.Installed {
		t.Errorf("claude-code should not be Installed when ~/.claude is missing")
	}
}
