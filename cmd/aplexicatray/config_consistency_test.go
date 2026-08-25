//go:build tray

package main

import (
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/config"
)

// TestTrayFlagDefaultsMatchPublishedConfig is the regression test for the
// "tray timing defaults contradict published config defaults" finding
// (FR-10.7 / BRD-10 §10). defaults.toml + internal/config.Schema publish
// tray.active_window=60s and tray.paused_window=10m (and
// tray.refresh_interval=5s) as the shipped defaults. The tray binary's
// own flag defaults (main.go) MUST equal those published values so that a
// tray launched from the autostart entry with no explicit --active-window
// / --paused-threshold / --interval reproduces the documented behavior.
//
// internal/config is already in the tray's dependency graph (via
// internal/daemon), so consulting config.Schema as the source of truth
// here adds no new dependency while pinning the two sides together. If a
// future edit drifts either the flag default or the schema default, this
// test fails instead of letting the binary silently honor a value the
// docs say is not in effect.
func TestTrayFlagDefaultsMatchPublishedConfig(t *testing.T) {
	cases := []struct {
		schemaKey  string
		flagName   string
		flagActual time.Duration
	}{
		{"tray.active_window", "active-window", *flagActiveWindow},
		{"tray.paused_window", "paused-threshold", *flagPausedThreshold},
		{"tray.refresh_interval", "interval", *flagInterval},
	}
	for _, tc := range cases {
		t.Run(tc.schemaKey, func(t *testing.T) {
			want := schemaDefaultDuration(t, tc.schemaKey)
			if tc.flagActual != want {
				t.Errorf("tray flag --%s default = %v, but published %s default = %v; "+
					"the tray binary must ship the same timing defaults as defaults.toml/config.Schema",
					tc.flagName, tc.flagActual, tc.schemaKey, want)
			}
		})
	}
}

// schemaDefaultDuration returns the parsed Default of the named duration
// schema entry, failing the test if the key is missing or unparseable.
func schemaDefaultDuration(t *testing.T, key string) time.Duration {
	t.Helper()
	for _, e := range config.Schema {
		if e.Key == key {
			d, err := config.ParseDuration(e.Default)
			if err != nil {
				t.Fatalf("config.Schema[%q].Default = %q is not a valid duration: %v",
					key, e.Default, err)
			}
			return d
		}
	}
	t.Fatalf("config.Schema has no entry for %q", key)
	return 0
}
