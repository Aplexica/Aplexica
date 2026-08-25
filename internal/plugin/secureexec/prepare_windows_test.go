//go:build windows

package secureexec

import (
	"context"
	"crypto/sha256"
	"os"
	"path/filepath"
	"testing"
)

func TestSecureExecWindowsChild(t *testing.T) {
	if os.Getenv("APLEXICA_SECUREEXEC_WINDOWS_TEST_CHILD") != "1" {
		return
	}
	if err := os.WriteFile(os.Getenv("APLEXICA_SECUREEXEC_TEST_MARKER"), []byte("verified"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestWindowsLaunchLocksRejectPathReplacementUntilStart(t *testing.T) {
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	verifiedBytes, err := os.ReadFile(testBinary)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	configured := filepath.Join(directory, "configured-plugin.exe")
	if err := os.WriteFile(configured, verifiedBytes, 0o700); err != nil {
		t.Fatal(err)
	}
	prepared, err := Prepare(context.Background(), configured, sha256.Sum256(verifiedBytes), "-test.run=^TestSecureExecWindowsChild$")
	if err != nil {
		t.Fatalf("prepare retained Windows launch locks: %v", err)
	}
	defer func() { _ = prepared.Close() }()

	replacement := filepath.Join(directory, "replacement.exe")
	if err := os.WriteFile(replacement, []byte("unverified replacement"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, configured); err == nil {
		t.Fatal("Windows executable replacement succeeded while launch locks were retained")
	}
	if err := os.Rename(directory, directory+"-renamed"); err == nil {
		t.Fatal("Windows executable ancestor rename succeeded while launch locks were retained")
	}
	marker := filepath.Join(directory, "verified-ran")
	prepared.Cmd().Env = append(os.Environ(), "APLEXICA_SECUREEXEC_WINDOWS_TEST_CHILD=1", "APLEXICA_SECUREEXEC_TEST_MARKER="+marker)
	if err := prepared.Cmd().Run(); err != nil {
		t.Fatalf("run retained authenticated Windows image: %v", err)
	}
	if value, err := os.ReadFile(marker); err != nil || string(value) != "verified" {
		t.Fatalf("authenticated Windows image did not execute: marker=%q err=%v", value, err)
	}
}
