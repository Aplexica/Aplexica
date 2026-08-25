package api

import (
	"encoding/json"
	"net/http"

	"github.com/aplexica/aplexica/internal/transport"
)

// TransportAccessor is the seam between remote-transport plumbing and the API
// handler. The local-only edition reports transport.LocalOnly; a supporting
// plugin can provide a richer
// implementation that actually toggles relays.
//
// Set + SetBYO ARE wired in the V1 handler — Set accepts "local"
// (no-op success); SetBYO returns 501 regardless of payload.
type TransportAccessor interface {
	Get() transport.Info
	Set(mode string) error
	SetBYO(opts transport.BYORelayOpts) error
}

// TransportHandler serves the three /api/transport endpoints.
type TransportHandler struct {
	acc TransportAccessor
}

// NewTransportHandler returns a TransportHandler bound to acc.
func NewTransportHandler(acc TransportAccessor) *TransportHandler {
	return &TransportHandler{acc: acc}
}

// Register attaches the three transport routes.
func (h *TransportHandler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/transport", h.Get)
	mux.HandleFunc("PUT /api/transport", h.Set)
	mux.HandleFunc("POST /api/transport/byo", h.SetBYO)
}

// Get serves GET /api/transport.
func (h *TransportHandler) Get(w http.ResponseWriter, _ *http.Request) {
	WriteJSON(w, http.StatusOK, h.acc.Get())
}

// setReq is the PUT body shape.
type setReq struct {
	Mode string `json:"mode"`
}

// Set serves PUT /api/transport. Accepts "local" (no-op success) and
// "local-only" (the spec's wire alias). Returns 501 for "byo-relay" when the
// current edition does not provide it.
func (h *TransportHandler) Set(w http.ResponseWriter, r *http.Request) {
	var req setReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		WriteError(w, http.StatusBadRequest, "invalid JSON: "+err.Error(), "validation")
		return
	}
	switch req.Mode {
	case "local", "local-only":
		if err := h.acc.Set(string(transport.ModeLocal)); err != nil {
			WriteError(w, http.StatusBadRequest, err.Error(), "validation")
			return
		}
		WriteJSON(w, http.StatusOK, h.acc.Get())
	case "byo-relay":
		WriteError(w, http.StatusNotImplemented,
			"BYO relay is not yet implemented", "not_yet_implemented")
	default:
		WriteError(w, http.StatusBadRequest,
			"mode must be one of local, local-only, byo-relay", "validation")
	}
}

// SetBYO serves POST /api/transport/byo. Validates the basic shape
// (url required) so the SPA can exercise the wire contract today,
// but returns 501 because BYO transport setup is not yet implemented.
func (h *TransportHandler) SetBYO(w http.ResponseWriter, r *http.Request) {
	var opts transport.BYORelayOpts
	if err := json.NewDecoder(r.Body).Decode(&opts); err != nil {
		WriteError(w, http.StatusBadRequest, "invalid JSON: "+err.Error(), "validation")
		return
	}
	if opts.URL == "" {
		WriteError(w, http.StatusBadRequest, "url is required", "validation")
		return
	}
	WriteError(w, http.StatusNotImplemented,
		"BYO relay setup is not yet implemented", "not_yet_implemented")
}
