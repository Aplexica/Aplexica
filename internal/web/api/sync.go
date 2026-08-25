package api

import (
	"encoding/json"
	"net/http"
)

// SyncAccessor is the seam to the daemon's await-config fan-out gate
// (FR-03.3). The portal toggles per-agent or global fan-out enablement;
// the daemon persists it to config AND applies it live (rebuilds the
// orchestrator's SyncGate) so no daemon restart is needed.
type SyncAccessor interface {
	// State returns the current global flag + per-agent overrides.
	State() (all bool, agents map[string]bool, err error)
	// SetAll enables/disables cross-agent fan-out to every installed agent.
	SetAll(enabled bool) error
	// SetAgent enables/disables fan-out to a single agent (the per-agent
	// override wins over the global flag).
	SetAgent(name string, enabled bool) error
}

// SyncHandler serves the fan-out-gate endpoints.
type SyncHandler struct{ acc SyncAccessor }

// NewSyncHandler binds a SyncHandler to acc.
func NewSyncHandler(acc SyncAccessor) *SyncHandler { return &SyncHandler{acc: acc} }

// Register attaches GET /api/sync + the toggle routes.
func (h *SyncHandler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/sync", h.Get)
	mux.HandleFunc("POST /api/sync/all", h.SetAll)
	mux.HandleFunc("POST /api/sync/agents/{name}", h.SetAgent)
}

// SyncStateResponse is the GET /api/sync body + the response of each toggle.
type SyncStateResponse struct {
	All    bool            `json:"all"`
	Agents map[string]bool `json:"agents"`
}

func (h *SyncHandler) writeState(w http.ResponseWriter) {
	all, agents, err := h.acc.State()
	if err != nil {
		WriteError(w, http.StatusInternalServerError, err.Error(), "internal")
		return
	}
	if agents == nil {
		agents = map[string]bool{}
	}
	WriteJSON(w, http.StatusOK, SyncStateResponse{All: all, Agents: agents})
}

// Get serves GET /api/sync.
func (h *SyncHandler) Get(w http.ResponseWriter, _ *http.Request) { h.writeState(w) }

type syncToggleRequest struct {
	Enabled bool `json:"enabled"`
}

// SetAll serves POST /api/sync/all — enable/disable fan-out to all agents.
func (h *SyncHandler) SetAll(w http.ResponseWriter, r *http.Request) {
	var req syncToggleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		WriteError(w, http.StatusBadRequest, "invalid JSON: "+err.Error(), "validation")
		return
	}
	if err := h.acc.SetAll(req.Enabled); err != nil {
		WriteError(w, http.StatusInternalServerError, err.Error(), "internal")
		return
	}
	h.writeState(w)
}

// SetAgent serves POST /api/sync/agents/{name} — per-agent override.
func (h *SyncHandler) SetAgent(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		WriteError(w, http.StatusBadRequest, "agent name required", "validation")
		return
	}
	var req syncToggleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		WriteError(w, http.StatusBadRequest, "invalid JSON: "+err.Error(), "validation")
		return
	}
	if err := h.acc.SetAgent(name, req.Enabled); err != nil {
		WriteError(w, http.StatusInternalServerError, err.Error(), "internal")
		return
	}
	h.writeState(w)
}
