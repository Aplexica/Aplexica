package api

import (
	"net/http"
	"time"
)

// DaemonAccessor is the minimal surface DaemonHandler needs to assemble
// the GET /api/daemon response and trigger pause/resume. The daemon
// wires the live ControlServer + Activity + pause helpers behind this
// interface so handlers stay testable without spinning up a real
// daemon process.
//
// Naming follows the wire shape: Version + PID + WatchedDir + Paused +
// StartedAt + State + PendingImports correspond 1:1 to the response
// JSON. State is a free-form bucket — "active" / "paused" / "idle" —
// derived by the caller from whatever live state best maps to a user-
// facing summary.
type DaemonAccessor interface {
	Version() string
	PID() int
	WatchedDir() string
	Paused() bool
	StartedAt() time.Time
	State() string
	PendingImports() int

	// Pause flips the pause flag and stops fan-out for the duration.
	// Returns an error if the daemon's pause store rejects the write
	// (e.g. permission error on the state file).
	Pause() error

	// Resume clears the pause flag and re-arms fan-out. Idempotent
	// against an already-running daemon.
	Resume() error
}

// RemoteStatusAccessor is an optional sibling interface a daemon may
// satisfy alongside DaemonAccessor to surface remote-plugin
// connectivity via GET /api/daemon. When the configured DaemonAccessor
// also implements RemoteStatusAccessor, the daemon handler's response
// body includes a `remote` sub-object; otherwise the field is omitted.
//
// Decoupled from DaemonAccessor so existing tests and the
// OSS-only configuration don't need to know about the cloud plugin.
type RemoteStatusAccessor interface {
	// RemoteConfigured returns true when a remote plugin is set up
	// (executable path stored in config; not necessarily running).
	RemoteConfigured() bool

	// RemoteEnabled returns true when the configured plugin is
	// enabled in config (separate from runtime connectivity).
	RemoteEnabled() bool

	// RemoteConnState returns the latest connectivity label cached
	// by the RemoteRunner: "starting" | "connecting" | "connected" |
	// "disconnected" | "paired_but_idle" | "unpaired" | "unknown".
	RemoteConnState() string

	// RemoteRestartCount returns the cumulative number of plugin
	// crashes since daemon startup. Useful for surfacing flapping
	// plugins.
	RemoteRestartCount() uint64
}

// DaemonHandler serves the /api/daemon{,/pause,/resume} endpoints.
// Construct with NewDaemonHandler and pass to web.Server.UseProtected
// so the routes mount behind RequireSession + RequireCSRF.
type DaemonHandler struct {
	acc DaemonAccessor
}

// NewDaemonHandler returns a DaemonHandler bound to acc.
func NewDaemonHandler(acc DaemonAccessor) *DaemonHandler {
	return &DaemonHandler{acc: acc}
}

// Register attaches the three daemon routes to mux. Implements
// web.HandlerRegistrar so the daemon wires it in one line.
func (h *DaemonHandler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/daemon", h.Get)
	mux.HandleFunc("POST /api/daemon/pause", h.Pause)
	mux.HandleFunc("POST /api/daemon/resume", h.Resume)
}

// daemonStatusBody is the wire shape returned by GET /api/daemon.
// uptime is reported in seconds (float, since since-startup precision
// below 1s is useful for tests but irrelevant in production).
//
// Remote is the optional remote-plugin status sub-object; omitted
// from the JSON when the accessor doesn't implement RemoteStatusAccessor.
type daemonStatusBody struct {
	Version        string            `json:"version"`
	PID            int               `json:"pid"`
	WatchedDir     string            `json:"watchedDir"`
	Paused         bool              `json:"paused"`
	Uptime         float64           `json:"uptime"`
	State          string            `json:"state"`
	PendingImports int               `json:"pendingImports"`
	Remote         *remoteStatusBody `json:"remote,omitempty"`
}

// remoteStatusBody is the remote-plugin status sub-object.
type remoteStatusBody struct {
	Configured   bool   `json:"configured"`
	Enabled      bool   `json:"enabled"`
	ConnState    string `json:"conn_state"`
	RestartCount uint64 `json:"restart_count"`
}

// Get serves GET /api/daemon — the SPA's overview snapshot.
func (h *DaemonHandler) Get(w http.ResponseWriter, _ *http.Request) {
	startedAt := h.acc.StartedAt()
	var uptime float64
	if !startedAt.IsZero() {
		uptime = time.Since(startedAt).Seconds()
		if uptime < 0 {
			uptime = 0
		}
	}
	body := daemonStatusBody{
		Version:        h.acc.Version(),
		PID:            h.acc.PID(),
		WatchedDir:     h.acc.WatchedDir(),
		Paused:         h.acc.Paused(),
		Uptime:         uptime,
		State:          h.acc.State(),
		PendingImports: h.acc.PendingImports(),
	}
	if rsa, ok := h.acc.(RemoteStatusAccessor); ok {
		body.Remote = &remoteStatusBody{
			Configured:   rsa.RemoteConfigured(),
			Enabled:      rsa.RemoteEnabled(),
			ConnState:    rsa.RemoteConnState(),
			RestartCount: rsa.RemoteRestartCount(),
		}
	}
	WriteJSON(w, http.StatusOK, body)
}

// Pause serves POST /api/daemon/pause.
func (h *DaemonHandler) Pause(w http.ResponseWriter, _ *http.Request) {
	if err := h.acc.Pause(); err != nil {
		WriteError(w, http.StatusInternalServerError, err.Error(), "pause_failed")
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"paused": true})
}

// Resume serves POST /api/daemon/resume.
func (h *DaemonHandler) Resume(w http.ResponseWriter, _ *http.Request) {
	if err := h.acc.Resume(); err != nil {
		WriteError(w, http.StatusInternalServerError, err.Error(), "resume_failed")
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"paused": false})
}
