//go:build unix

package privatefs

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestHardlinkIdentityFailureIsNotRetryable(t *testing.T) {
	dir := t.TempDir()
	original := filepath.Join(dir, "original")
	linked := filepath.Join(dir, "linked")
	if err := os.WriteFile(original, []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(original, linked); err != nil {
		t.Fatal(err)
	}

	f, err := os.Open(linked)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	for name, validate := range map[string]func() error{
		"repair":  func() error { return validateRepairNode(info, false) },
		"regular": func() error { return validateRegularFile(f, false) },
	} {
		t.Run(name, func(t *testing.T) {
			err := validate()
			if err == nil {
				t.Fatal("hardlink unexpectedly accepted")
			}
			if errors.Is(err, ErrOpenedFileUnlinked) {
				t.Fatalf("hardlink identity failure must not be retryable: %v", err)
			}
		})
	}
}
