//go:build unix

package adapter

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteSecretConfigNarrowsAndPreservesStricterMode(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	if err := os.WriteFile(p, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteSecretConfig(p, []byte("new")); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(p)
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode=%o", fi.Mode().Perm())
	}
	if err := os.Chmod(p, 0o400); err != nil {
		t.Fatal(err)
	}
	if err := WriteSecretConfig(p, []byte("again")); err != nil {
		t.Fatal(err)
	}
	fi, _ = os.Stat(p)
	if fi.Mode().Perm() != 0o400 {
		t.Fatalf("stricter mode widened to %o", fi.Mode().Perm())
	}
}
func TestWriteSecretConfigRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("canary"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "config.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := WriteSecretConfig(link, []byte("secret")); err == nil {
		t.Fatal("symlink destination accepted")
	}
	b, _ := os.ReadFile(target)
	if string(b) != "canary" {
		t.Fatal("symlink target changed")
	}
}
