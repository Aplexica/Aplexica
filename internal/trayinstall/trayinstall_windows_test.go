//go:build windows

package trayinstall

import (
	"strings"
	"testing"
)

func TestWindowsInstallScriptRemovesLegacyScheduledTask(t *testing.T) {
	script := legacyScheduledTaskCleanupScript()
	if !strings.Contains(script, "Get-ScheduledTask") ||
		!strings.Contains(script, legacyScheduledTaskName) ||
		!strings.Contains(script, "Unregister-ScheduledTask") {
		t.Fatalf("legacy cleanup script does not remove %q: %s",
			legacyScheduledTaskName, script)
	}
}
