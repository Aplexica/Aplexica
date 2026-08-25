//go:build !windows

package main

import (
	"strings"
	"testing"
)

// TestRefusePrivilegedStartup_AllowsNormalUser asserts the happy path:
// under a normal (non-zero) euid, refusePrivilegedStartup returns nil so a
// legitimate user start is never blocked. CI does not run as root, so this
// exercises the real os.Geteuid source.
func TestRefusePrivilegedStartup_AllowsNormalUser(t *testing.T) {
	if err := refusePrivilegedStartup(); err != nil {
		t.Fatalf("refusePrivilegedStartup() under normal uid = %v, want nil", err)
	}
}

// TestRefusePrivilegedStartup_RejectsRoot stubs the euid source to 0 and
// asserts the function refuses startup with an error mentioning root and the
// FR-09.12 spec clause. This encodes the spec violation: euid 0 MUST error.
func TestRefusePrivilegedStartup_RejectsRoot(t *testing.T) {
	orig := geteuidFn
	geteuidFn = func() int { return 0 }
	defer func() { geteuidFn = orig }()

	err := refusePrivilegedStartup()
	if err == nil {
		t.Fatal("refusePrivilegedStartup() with euid 0 = nil, want error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "root") {
		t.Errorf("error %q does not mention root", msg)
	}
	if !strings.Contains(msg, "FR-09.12") {
		t.Errorf("error %q does not mention FR-09.12", msg)
	}
}
