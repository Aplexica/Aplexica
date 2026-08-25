package main

import (
	"path/filepath"
	"testing"

	"github.com/aplexica/aplexica/internal/daemon"
)

func TestSetDaemonWatchDirInConfigPreservesUnrelatedSettings(t *testing.T) {
	stateDir := t.TempDir()
	configPath := filepath.Join(stateDir, "config.json")
	enabled := true
	before := &daemon.Config{
		LogLevel: "warn",
		Tray:     daemon.TrayConfig{Enabled: &enabled},
		Remote:   daemon.RemoteConfig{Executable: "/existing/plugin"},
	}
	if err := daemon.WriteConfig(configPath, before); err != nil {
		t.Fatal(err)
	}

	if err := setDaemonWatchDirInConfig(stateDir, "/watched"); err != nil {
		t.Fatal(err)
	}

	after, err := daemon.LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if after.Dir != "/watched" {
		t.Fatalf("Dir = %q, want /watched", after.Dir)
	}
	if after.LogLevel != before.LogLevel ||
		after.Tray.Enabled == nil || !*after.Tray.Enabled ||
		after.Remote.Executable != before.Remote.Executable {
		t.Fatalf("unrelated config changed: before=%+v after=%+v", before, after)
	}
}
