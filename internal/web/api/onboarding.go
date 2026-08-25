package api

import (
	"net/http"

	"github.com/aplexica/aplexica/internal/onboarding"
)

// OnboardingAccessor is the seam between the daemon's runtime
// (adapter count + last-sync activity) and the API handler. The
// daemon's wiring layer derives onboarding.Inputs from its
// orchestrator state and returns the computed State; tests pass a
// fixed State.
type OnboardingAccessor interface {
	State() onboarding.State
}

// OnboardingHandler serves the single onboarding endpoint.
type OnboardingHandler struct {
	acc OnboardingAccessor
}

// NewOnboardingHandler returns an OnboardingHandler bound to acc.
func NewOnboardingHandler(acc OnboardingAccessor) *OnboardingHandler {
	return &OnboardingHandler{acc: acc}
}

// Register attaches the onboarding route.
func (h *OnboardingHandler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/onboarding/state", h.Get)
}

// Get serves GET /api/onboarding/state.
func (h *OnboardingHandler) Get(w http.ResponseWriter, _ *http.Request) {
	WriteJSON(w, http.StatusOK, h.acc.State())
}
