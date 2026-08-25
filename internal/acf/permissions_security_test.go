//go:build unix

package acf

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStoreInitRepairsPrivateModesEveryRun(t *testing.T) {
	root := filepath.Join(t.TempDir(), "store")
	s := &Store{Root: root}
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(root, "VERSION")
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	ri, _ := os.Stat(root)
	fi, _ := os.Stat(p)
	if ri.Mode().Perm() != 0o700 || fi.Mode().Perm() != 0o600 {
		t.Fatalf("modes root=%o file=%o", ri.Mode().Perm(), fi.Mode().Perm())
	}
}
func TestStoreInitRejectsLinkWithoutTouchingCanary(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "store")
	if err := os.MkdirAll(filepath.Join(root, "acf"), 0o700); err != nil {
		t.Fatal(err)
	}
	canary := filepath.Join(base, "canary")
	if err := os.WriteFile(canary, []byte("unchanged"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(canary, filepath.Join(root, "acf", "escape")); err != nil {
		t.Fatal(err)
	}
	if err := (&Store{Root: root}).Init(); err == nil {
		t.Fatal("store accepted linked node")
	}
	got, _ := os.ReadFile(canary)
	if string(got) != "unchanged" {
		t.Fatal("external canary changed")
	}
}
func TestStoreInitRejectsHardLinkedFile(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "store")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	canary := filepath.Join(base, "canary")
	if err := os.WriteFile(canary, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(canary, filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}
	if err := (&Store{Root: root}).Init(); err == nil {
		t.Fatal("store accepted hard-linked file")
	}
	fi, _ := os.Stat(canary)
	if fi.Mode().Perm() != 0o644 {
		t.Fatal("external hard-link target was chmodded")
	}
}
