//go:build linux

package secureexec

import (
	"context"
	"crypto/sha256"
	"os"
	"path/filepath"
	"testing"
)

func TestSecureExecChild(t *testing.T) {
	if os.Getenv("APLEXICA_SECUREEXEC_TEST_CHILD") != "1" {
		return
	}
	if err := os.WriteFile(os.Getenv("APLEXICA_SECUREEXEC_TEST_MARKER"), []byte("verified"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLinuxSealedMemfdExecutesAuthenticatedBytesAfterPathReplacement(t *testing.T) {
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	verifiedBytes, err := os.ReadFile(testBinary)
	if err != nil {
		t.Fatal(err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	directory, err := os.MkdirTemp(home, ".aplexica-secureexec-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	configured := filepath.Join(directory, "configured-plugin")
	if err := os.WriteFile(configured, verifiedBytes, 0o700); err != nil {
		t.Fatal(err)
	}
	prepared, err := Prepare(context.Background(), configured, sha256.Sum256(verifiedBytes), "-test.run=^TestSecureExecChild$")
	if err != nil {
		t.Fatalf("prepare sealed memfd: %v", err)
	}
	defer func() { _ = prepared.Close() }()

	malicious, err := os.ReadFile("/bin/false")
	if err != nil {
		t.Fatal(err)
	}
	replacement := filepath.Join(directory, "replacement")
	if err := os.WriteFile(replacement, malicious, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, configured); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(directory, "verified-ran")
	prepared.Cmd().Env = append(os.Environ(), "APLEXICA_SECUREEXEC_TEST_CHILD=1", "APLEXICA_SECUREEXEC_TEST_MARKER="+marker)
	if err := prepared.Cmd().Run(); err != nil {
		t.Fatalf("run sealed authenticated bytes after replacement: %v", err)
	}
	if value, err := os.ReadFile(marker); err != nil || string(value) != "verified" {
		t.Fatalf("authenticated image did not execute: marker=%q err=%v", value, err)
	}
}
