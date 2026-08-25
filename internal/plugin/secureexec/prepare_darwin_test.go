//go:build darwin

package secureexec

import (
	"bytes"
	"context"
	"crypto/sha256"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/aplexica/aplexica/internal/privatefs"
)

func TestDarwinLaunchRequiresPrivilegedImmutablePath(t *testing.T) {
	original, err := os.ReadFile("/bin/echo")
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(original)
	if _, err := Prepare(context.Background(), "/bin/echo", digest, "authenticated"); err == nil {
		t.Fatal("root-owned executable outside the exact Aplexica runtime root was accepted")
	}
	want := "/Library/Aplexica/RemotePlugins/aplexica-cloud/v1.2.3/aplexica-cloud-plugin"
	if err := validateDarwinRemotePluginPathShape(want, "aplexica-cloud", "v1.2.3"); err != nil {
		t.Fatalf("valid direct versioned path rejected: %v", err)
	}
	input, err := privatefs.OpenTrustedInput("/bin/echo", privatefs.TrustedInputPolicy{
		MaxBytes: maxExecutableBytes, RequireExecutable: true, AllowSystemOwner: true,
	})
	if err != nil {
		t.Fatalf("open immutable system executable fixture: %v", err)
	}
	command, resources, err := preparePlatformCommand(context.Background(), "/bin/echo", digest, input, []string{"authenticated"})
	if err != nil {
		t.Fatalf("retain and re-hash immutable executable fixture: %v", err)
	}
	defer func() { _ = closeResources(resources) }()
	output, err := command.Output()
	if err != nil || !bytes.Equal(output, []byte("authenticated\n")) {
		t.Fatalf("retained immutable launch output=%q err=%v", output, err)
	}

	mutable := filepath.Join(t.TempDir(), "plugin")
	if err := os.WriteFile(mutable, original, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Prepare(context.Background(), mutable, digest); err == nil {
		t.Fatal("mutable current-user-owned executable was accepted on macOS")
	}
}

func TestDarwinImmutableMetadataDoesNotRequireExecutableMode(t *testing.T) {
	metadata := "/System/Library/CoreServices/SystemVersion.plist"
	if err := validatePrivilegedImmutableDarwinFile(metadata, false); err != nil {
		t.Fatalf("root-owned immutable non-executable metadata rejected: %v", err)
	}
	if err := validatePrivilegedImmutableDarwinFile(metadata, true); err == nil {
		t.Fatal("non-executable metadata accepted as a launch executable")
	}
}

func TestDarwinExtendedACLIsRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plugin")
	if err := os.WriteFile(path, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := requireNoDarwinExtendedACL(path); err != nil {
		t.Fatalf("ordinary ACL-free file rejected: %v", err)
	}
	if output, err := exec.Command("/bin/chmod", "+a", "everyone allow read", path).CombinedOutput(); err != nil {
		t.Fatalf("add test ACL: %v: %s", err, output)
	}
	if err := requireNoDarwinExtendedACL(path); err == nil {
		t.Fatal("extended ACL was accepted")
	}
}
