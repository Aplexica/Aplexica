//go:build windows

package daemon

import (
	"os/exec"
	"testing"
)

func TestHideChildWindowSuppressesConsole(t *testing.T) {
	cmd := exec.Command("aplexica-cloud-plugin.exe")

	hideChildWindow(cmd)

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
