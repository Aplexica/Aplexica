package onboarding

import (
	"testing"
	"time"
)

func TestComputeFreshDaemonNoAgents(t *testing.T) {
	state := Compute(Inputs{AdapterCount: 0})
	if len(state.Steps) != 3 {
		t.Fatalf("Steps count = %d, want 3", len(state.Steps))
	}
	// install-daemon is always complete (we're answering the API).
	if !state.Steps[0].Complete {
		t.Error("install-daemon should always be complete")
	}
	if state.Steps[0].ID != StepInstallDaemon {
		t.Errorf("Steps[0].ID = %q, want %q", state.Steps[0].ID, StepInstallDaemon)
	}
	// detect-agents is false with zero adapters.
	if state.Steps[1].Complete {
		t.Error("detect-agents should be false with AdapterCount=0")
	}
	// first-sync is false with zero LastSyncActivity.
	if state.Steps[2].Complete {
		t.Error("first-sync should be false without sync activity")
	}
}

func TestComputeAgentsDetected(t *testing.T) {
	state := Compute(Inputs{AdapterCount: 2})
	if !state.Steps[1].Complete {
		t.Error("detect-agents should be true with AdapterCount=2")
	}
}

func TestComputeFirstSyncObserved(t *testing.T) {
	stamp := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	state := Compute(Inputs{AdapterCount: 1, LastSyncActivity: stamp})
	if !state.Steps[2].Complete {
		t.Error("first-sync should be true with non-zero LastSyncActivity")
	}
	if !state.Steps[2].CompletedAt.Equal(stamp) {
		t.Errorf("first-sync CompletedAt = %v, want %v", state.Steps[2].CompletedAt, stamp)
	}
}

func TestComputeStepIDsAreStable(t *testing.T) {
	// The SPA keys its step labels off these IDs; verify they don't
	// drift across releases.
	state := Compute(Inputs{})
	want := []StepID{StepInstallDaemon, StepDetectAgents, StepFirstSync}
	for i, w := range want {
		if state.Steps[i].ID != w {
			t.Errorf("Steps[%d].ID = %q, want %q", i, state.Steps[i].ID, w)
		}
	}
}

func TestStepIDConstantsMatchSpecStrings(t *testing.T) {
	// Spec §9 W7 names these explicitly; verify the wire strings.
	if StepInstallDaemon != "install-daemon" {
		t.Errorf("StepInstallDaemon = %q", StepInstallDaemon)
	}
	if StepDetectAgents != "detect-agents" {
		t.Errorf("StepDetectAgents = %q", StepDetectAgents)
	}
	if StepFirstSync != "first-sync" {
		t.Errorf("StepFirstSync = %q", StepFirstSync)
	}
}
