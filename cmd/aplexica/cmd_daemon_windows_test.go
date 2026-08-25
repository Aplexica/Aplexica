//go:build windows

package main

import (
	"os/exec"
	"strings"
	"testing"
)

// TestRefusePrivilegedStartup_RejectsElevated stubs the elevation probe to
// report an elevated token and asserts the daemon refuses startup with an
// error mentioning the FR-09.12 spec clause. This encodes the spec violation:
// an elevated/administrator process MUST error.
func TestRefusePrivilegedStartup_RejectsElevated(t *testing.T) {
	orig := isProcessElevated
	isProcessElevated = func() bool { return true }
	defer func() { isProcessElevated = orig }()

	err := refusePrivilegedStartup()
	if err == nil {
		t.Fatal("refusePrivilegedStartup() while elevated = nil, want error")
	}
	if !strings.Contains(err.Error(), "FR-09.12") {
		t.Errorf("error %q does not mention FR-09.12", err.Error())
	}
}

// TestRefusePrivilegedStartup_AllowsNonElevated stubs the probe to report a
// non-elevated token and asserts a legitimate non-admin start is allowed.
func TestRefusePrivilegedStartup_AllowsNonElevated(t *testing.T) {
	orig := isProcessElevated
	isProcessElevated = func() bool { return false }
	defer func() { isProcessElevated = orig }()

	if err := refusePrivilegedStartup(); err != nil {
		t.Fatalf("refusePrivilegedStartup() while non-elevated = %v, want nil", err)
	}
}

func TestHideRemotePluginWindowSuppressesConsole(t *testing.T) {
	cmd := exec.Command("aplexica-cloud-plugin.exe", "--connect-check")

	hideRemotePluginWindow(cmd)

	if cmd.SysProcAttr == nil {
		t.Fatal("SysProcAttr is nil")
	}
	wantFlags := uint32(_DETACHED_PROCESS | _CREATE_NO_WINDOW)
	if got := uint32(cmd.SysProcAttr.CreationFlags); got&wantFlags != wantFlags {
		t.Fatalf("CreationFlags = 0x%x, want both hidden-window flags 0x%x", got, wantFlags)
	}
	if !cmd.SysProcAttr.HideWindow {
		t.Fatal("HideWindow = false, want true")
	}
}

func TestDetachSysProcAttrSuppressesConsole(t *testing.T) {
	attr := detachSysProcAttr()
	if attr == nil {
		t.Fatal("SysProcAttr is nil")
	}
	wantFlags := uint32(_DETACHED_PROCESS | _CREATE_NO_WINDOW)
	if got := uint32(attr.CreationFlags); got&wantFlags != wantFlags {
		t.Fatalf("CreationFlags = 0x%x, want both hidden-window flags 0x%x", got, wantFlags)
	}
	if !attr.HideWindow {
		t.Fatal("HideWindow = false, want true")
	}
}
