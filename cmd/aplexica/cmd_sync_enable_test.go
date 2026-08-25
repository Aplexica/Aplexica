package main

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/aplexica/aplexica/internal/daemon"
)

func TestSetSyncEnabled_PerAgent(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	// Seed an empty config file.
	if err := daemon.WriteConfig(cfgPath, &daemon.Config{}); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := setSyncEnabled(cfgPath, []string{"codex"}, false, true, &buf); err != nil {
		t.Fatalf("enable codex: %v", err)
	}

	cfg, err := daemon.LoadConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Sync.Agents["codex"] {
		t.Errorf("sync.agents.codex = %v, want true; cfg=%+v", cfg.Sync.Agents["codex"], cfg.Sync)
	}
	if cfg.Sync.All {
		t.Errorf("sync.all should remain false for a per-agent enable")
	}
}

func TestSetSyncEnabled_All(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	if err := daemon.WriteConfig(cfgPath, &daemon.Config{}); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := setSyncEnabled(cfgPath, nil, true, true, &buf); err != nil {
		t.Fatalf("enable --all: %v", err)
	}
	cfg, _ := daemon.LoadConfig(cfgPath)
	if !cfg.Sync.All {
		t.Errorf("sync.all = false, want true after enable --all")
	}
}

func TestSetSyncEnabled_DisableExcludes(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	if err := daemon.WriteConfig(cfgPath, &daemon.Config{}); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := setSyncEnabled(cfgPath, []string{"hermes"}, false, false, &buf); err != nil {
		t.Fatalf("disable hermes: %v", err)
	}
	cfg, _ := daemon.LoadConfig(cfgPath)
	if v, ok := cfg.Sync.Agents["hermes"]; !ok || v {
		t.Errorf("sync.agents.hermes = (%v,%v), want explicit false", v, ok)
	}
}

func TestSetSyncEnabled_RequiresTarget(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	if err := daemon.WriteConfig(cfgPath, &daemon.Config{}); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := setSyncEnabled(cfgPath, nil, false, true, &buf); err == nil {
		t.Errorf("expected an error when neither agents nor --all is given")
	}
}

// TestSyncGateConfig_BridgesDaemonConfig verifies the daemon->syncgate bridge
// the orchestrator relies on.
func TestSyncGateConfig_BridgesDaemonConfig(t *testing.T) {
	c := daemon.Config{Sync: daemon.SyncConfig{All: true, Agents: map[string]bool{"codex": false}}}
	gc := daemon.SyncGateConfig(c)
	if !gc.All || gc.Agents["codex"] {
		t.Errorf("SyncGateConfig bridge wrong: %+v", gc)
	}
}
