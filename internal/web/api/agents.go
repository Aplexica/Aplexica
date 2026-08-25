package api

import (
	"net/http"
	"time"
)

// AgentSummary is the per-agent shape returned by GET /api/agents. The
// LastActivity field omits when the daemon's per-adapter touched-map
// has the zero time, signalling "never seen".
type AgentSummary struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	// Surfaces are the user-facing runtimes backed by this logical adapter
	// (for example, CLI and desktop sharing the same native storage). This is
	// the supported set; ActiveSurfaces is the independently detected subset.
	Surfaces       []string  `json:"surfaces,omitempty"`
	ActiveSurfaces []string  `json:"activeSurfaces,omitempty"`
	SyncState      string    `json:"syncState"`
	LastActivity   time.Time `json:"lastActivity,omitzero"`
	// Installed reports whether the agent was discovered on this machine
	// (BRD-03 FR-03.3). When false, GlobalRoots is empty and the UI should
	// render the agent as "not installed" rather than implying it is synced.
	Installed bool `json:"installed"`
	// GlobalRoots are the native global-storage paths the daemon watches for
	// this agent. Empty when !Installed.
	GlobalRoots []string `json:"globalRoots,omitempty"`
	// ArtifactCount is the number of canonical-store artifacts currently
	// attributed to this agent (FR-01.28). 0 when none / not installed.
	ArtifactCount int `json:"artifactCount"`
	// SyncEnabled reports whether this agent may participate in cross-agent
	// fan-out via the FR-03.3 await-config gate. Discovered agents import
	// regardless; this controls whether artifacts feed from/to this agent.
	// Toggled from the portal (POST /api/sync/agents/{name}).
	SyncEnabled bool `json:"syncEnabled"`
}

// AgentEvent is one item in AgentDetail.RecentEvents. V1 always
// returns an empty slice — the SSE event bus lands in W5 and the
// /api/agents/:name response surfaces that history through the same
// shape once W5 wires its in-memory ring buffer in.
type AgentEvent struct {
	Timestamp time.Time `json:"timestamp"`
	Type      string    `json:"type"`
	Detail    string    `json:"detail,omitempty"`
}

// AgentDetail is the GET /api/agents/:name response. Embeds
// AgentSummary so the summary fields are at the top level.
type AgentDetail struct {
	AgentSummary
	Namespaces   []string     `json:"namespaces"`
	RecentEvents []AgentEvent `json:"recentEvents"`
}

// AgentsAccessor is the seam between the live daemon adapters slice
// + AdapterStates map and the API handler. The daemon's wiring layer
// is responsible for mapping its internal adapter slice (with
// adapter.Capabilities + sync orchestrator state) into the JSON shapes
// declared above.
//
// V1 RecentEvents is always empty — W5's SSE bus will flesh it out by
// attaching the in-memory ring buffer to AgentDetail via this same
// accessor.
type AgentsAccessor interface {
	List() []AgentSummary
	Get(name string) (AgentDetail, bool)
}

// AgentsHandler serves the two agents endpoints. Construct with
// NewAgentsHandler and pass to web.Server.UseProtected.
type AgentsHandler struct {
	acc AgentsAccessor
}

// NewAgentsHandler returns an AgentsHandler bound to acc.
func NewAgentsHandler(acc AgentsAccessor) *AgentsHandler {
	return &AgentsHandler{acc: acc}
}

// Register attaches the two agents routes.
func (h *AgentsHandler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/agents", h.List)
	mux.HandleFunc("GET /api/agents/{name}", h.Get)
}

// List serves GET /api/agents.
func (h *AgentsHandler) List(w http.ResponseWriter, _ *http.Request) {
	out := h.acc.List()
	if out == nil {
		out = []AgentSummary{}
	}
	WriteJSON(w, http.StatusOK, out)
}

// Get serves GET /api/agents/{name}.
func (h *AgentsHandler) Get(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		WriteError(w, http.StatusBadRequest, "name path segment required", "validation")
		return
	}
	detail, ok := h.acc.Get(name)
	if !ok {
		WriteError(w, http.StatusNotFound, "agent not found: "+name, "not_found")
		return
	}
	if detail.Namespaces == nil {
		detail.Namespaces = []string{}
	}
	if detail.RecentEvents == nil {
		detail.RecentEvents = []AgentEvent{}
	}
	WriteJSON(w, http.StatusOK, detail)
}
