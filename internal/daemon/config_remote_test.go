package daemon

import (
	"testing"
	"time"
)

func TestRemoteEnabled_NilConfig(t *testing.T) {
	if RemoteEnabled(nil) {
		t.Error("RemoteEnabled(nil) should return false")
	}
}

func TestRemoteEnabled_EmptyExecutable(t *testing.T) {
	if RemoteEnabled(&Config{}) {
		t.Error("RemoteEnabled with empty Executable should return false")
	}
}

func TestRemoteEnabled_NilEnabledDefaultsTrue(t *testing.T) {
	cfg := &Config{Remote: RemoteConfig{Executable: "/usr/bin/aplexica-cloud-plugin"}}
	if !RemoteEnabled(cfg) {
		t.Error("Executable set + Enabled nil should default to true")
	}
}

func TestRemoteEnabled_ExplicitFalse(t *testing.T) {
	f := false
	cfg := &Config{Remote: RemoteConfig{Executable: "/p", Enabled: &f}}
	if RemoteEnabled(cfg) {
		t.Error("explicit Enabled=false should override")
	}
}

func TestRemoteEnabled_ExplicitTrue(t *testing.T) {
	tt := true
	cfg := &Config{Remote: RemoteConfig{Executable: "/p", Enabled: &tt}}
	if !RemoteEnabled(cfg) {
		t.Error("explicit Enabled=true should be honored")
	}
}

func TestRemoteSyncMode_DefaultsToScheduled(t *testing.T) {
	if RemoteSyncMode(nil) != "scheduled" {
		t.Error("nil config should default to scheduled")
	}
	if RemoteSyncMode(&Config{}) != "scheduled" {
		t.Error("empty config should default to scheduled")
	}
	if RemoteSyncMode(&Config{Remote: RemoteConfig{SyncMode: "manual"}}) != "manual" {
		t.Error("explicit SyncMode should be honored")
	}
}

func TestRemoteScheduledInterval_Defaults(t *testing.T) {
	if got := RemoteScheduledInterval(nil); got != 15*time.Minute {
		t.Errorf("nil = %v, want 15m", got)
	}
	if got := RemoteScheduledInterval(&Config{Remote: RemoteConfig{ScheduledInterval: 0}}); got != 15*time.Minute {
		t.Errorf("zero = %v, want 15m default", got)
	}
	if got := RemoteScheduledInterval(&Config{Remote: RemoteConfig{ScheduledInterval: 30 * time.Minute}}); got != 30*time.Minute {
		t.Errorf("30m = %v", got)
	}
}

func TestRemoteScheduledIntervalDefaultMatchesContract(t *testing.T) {
	// Scheduled mode publishes and fetches on a cron-style cadence whose
	// default is 15 minutes.
	if RemoteScheduledIntervalDefault != 15*time.Minute {
		t.Errorf("RemoteScheduledIntervalDefault = %v, want 15m", RemoteScheduledIntervalDefault)
	}
}
