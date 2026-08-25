package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/onboarding"
)

type fakeOnboardingAccessor struct {
	state onboarding.State
}

func (f *fakeOnboardingAccessor) State() onboarding.State { return f.state }

func TestOnboardingState_HappyPath(t *testing.T) {
	stamp := time.Now()
	acc := &fakeOnboardingAccessor{
		state: onboarding.Compute(onboarding.Inputs{
			AdapterCount:     2,
			LastSyncActivity: stamp,
		}),
	}
	h := NewOnboardingHandler(acc)

	req := httptest.NewRequest(http.MethodGet, "/api/onboarding/state", nil)
	rr := httptest.NewRecorder()
	h.Get(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	var got map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	steps, ok := got["steps"].([]any)
	if !ok {
		t.Fatalf("steps not an array: %v", got["steps"])
	}
	if len(steps) != 3 {
		t.Errorf("got %d steps, want 3", len(steps))
	}
	first := steps[0].(map[string]any)
	if first["id"] != "install-daemon" {
		t.Errorf("steps[0].id = %v, want install-daemon", first["id"])
	}
	if first["complete"] != true {
		t.Errorf("steps[0].complete = %v, want true", first["complete"])
	}
}

func TestOnboardingState_EmptyDefault(t *testing.T) {
	acc := &fakeOnboardingAccessor{
		state: onboarding.Compute(onboarding.Inputs{}),
	}
	h := NewOnboardingHandler(acc)

	req := httptest.NewRequest(http.MethodGet, "/api/onboarding/state", nil)
	rr := httptest.NewRecorder()
	h.Get(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	var st onboarding.State
	if err := json.Unmarshal(rr.Body.Bytes(), &st); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// install-daemon always complete; detect-agents + first-sync false on a fresh install.
	if !st.Steps[0].Complete {
		t.Error("install-daemon must be complete")
	}
	if st.Steps[1].Complete {
		t.Error("detect-agents must be incomplete on a fresh install")
	}
}
