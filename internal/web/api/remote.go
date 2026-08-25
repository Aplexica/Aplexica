package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

// ErrRemoteNotConfigured is the sentinel a RemoteAccessor returns when
// no remote-plugin executable is configured for this daemon. Mapped to
// a 400 with code "remote_not_configured" by the Pair + Verify handlers.
var ErrRemoteNotConfigured = errors.New("api: remote plugin not configured")

// ErrNotPaired is the sentinel a RemoteAccessor returns from Verify when
// the plugin is configured but this device has not yet been paired with
// the cloud account. Mapped to a 400 with code "not_paired".
var ErrNotPaired = errors.New("api: device not paired")

// ErrPairFailed is the sentinel a RemoteAccessor returns from Pair when
// the plugin's --pair invocation exited non-zero. Mapped to a 502 with
// code "pair_failed"; the wrapped error carries the plugin's output so
// the SPA can surface a useful diagnostic.
var ErrPairFailed = errors.New("api: remote pairing failed")

// RemoteAccessor is the seam between the local web handler and the
// daemon-side logic that execs the configured remote (cloud) plugin's
// CLI. The daemon's wiring layer (cmd/aplexica/cmd_daemon_web_api.go)
// implements this by shelling out to the plugin binary's --pair /
// --status / --connect-check entry points. The handlers below never
// touch the plugin directly; they only translate accessor results +
// sentinel errors into the documented wire shapes.
type RemoteAccessor interface {
	// Pair runs the plugin's --pair flow with the user-supplied pairing
	// token (from the cloud portal) and an optional device name. On
	// success it returns the cloud-assigned device + account identifiers
	// and (best-effort) restarts the plugin so it re-reads credentials
	// and connects. Returns ErrRemoteNotConfigured when no plugin is
	// configured, or an error wrapping ErrPairFailed when the plugin
	// exited non-zero.
	Pair(ctx context.Context, token, deviceName string) (deviceID, accountID string, err error)

	// Status reports the configured/enabled/paired state plus the live
	// connection label + restart count. It is total: it never returns
	// ErrRemoteNotConfigured — an unconfigured daemon reports
	// configured=false with the remaining fields zeroed.
	Status(ctx context.Context) (configured, enabled, paired bool, deviceID, accountID, connState string, restartCount uint64, err error)

	// Verify runs the plugin's --connect-check and reports whether the
	// device currently has a working connection to the cloud. Returns
	// ErrRemoteNotConfigured when no plugin is configured, or
	// ErrNotPaired when the plugin reports the device is not paired.
	Verify(ctx context.Context) (connected bool, message string, err error)

	// Unpair runs the plugin's --unpair flow: it clears the device's
	// stored credentials + cached cloud rules and restarts the plugin so
	// it disconnects from the broker. It does NOT revoke the device in the
	// cloud account (that is the account owner's action via the cloud
	// portal). Returns ErrRemoteNotConfigured when no plugin is configured.
	Unpair(ctx context.Context) error
}

// RemoteHandler serves the three /api/remote endpoints (pair, status,
// verify). Construct with NewRemoteHandler and pass to
// web.Server.UseProtected — the handlers run behind RequireSession +
// RequireCSRF and so do not re-check auth.
type RemoteHandler struct {
	acc RemoteAccessor
}

// NewRemoteHandler returns a RemoteHandler bound to acc.
func NewRemoteHandler(acc RemoteAccessor) *RemoteHandler {
	return &RemoteHandler{acc: acc}
}

// Register attaches the three remote routes.
func (h *RemoteHandler) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/remote/pair", h.Pair)
	mux.HandleFunc("GET /api/remote/status", h.Status)
	mux.HandleFunc("POST /api/remote/verify", h.Verify)
	mux.HandleFunc("POST /api/remote/unpair", h.Unpair)
}

// pairReq is the POST /api/remote/pair body shape.
type pairReq struct {
	Token      string `json:"token"`
	DeviceName string `json:"device_name"`
}

// Pair serves POST /api/remote/pair.
func (h *RemoteHandler) Pair(w http.ResponseWriter, r *http.Request) {
	var req pairReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		WriteError(w, http.StatusBadRequest, "invalid JSON: "+err.Error(), "validation")
		return
	}
	req.Token = strings.TrimSpace(req.Token)
	if req.Token == "" {
		WriteError(w, http.StatusBadRequest, "token is required", "validation")
		return
	}
	deviceID, accountID, err := h.acc.Pair(r.Context(), req.Token, strings.TrimSpace(req.DeviceName))
	if err != nil {
		switch {
		case errors.Is(err, ErrRemoteNotConfigured):
			WriteError(w, http.StatusBadRequest, err.Error(), "remote_not_configured")
		case errors.Is(err, ErrPairFailed):
			WriteError(w, http.StatusBadGateway, err.Error(), "pair_failed")
		default:
			WriteError(w, http.StatusBadGateway, err.Error(), "pair_failed")
		}
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"paired":     true,
		"device_id":  deviceID,
		"account_id": accountID,
	})
}

// Status serves GET /api/remote/status. It is always 200: an
// unconfigured or unreachable plugin still yields a well-formed status
// object (configured=false / paired=false) rather than an error, so the
// SPA can render the "not connected" state uniformly.
func (h *RemoteHandler) Status(w http.ResponseWriter, r *http.Request) {
	configured, enabled, paired, deviceID, accountID, connState, restartCount, err := h.acc.Status(r.Context())
	if err != nil {
		WriteError(w, http.StatusInternalServerError, err.Error(), "internal")
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"configured":    configured,
		"enabled":       enabled,
		"paired":        paired,
		"device_id":     deviceID,
		"account_id":    accountID,
		"conn_state":    connState,
		"restart_count": restartCount,
	})
}

// Verify serves POST /api/remote/verify.
func (h *RemoteHandler) Verify(w http.ResponseWriter, r *http.Request) {
	connected, message, err := h.acc.Verify(r.Context())
	if err != nil {
		switch {
		case errors.Is(err, ErrRemoteNotConfigured):
			WriteError(w, http.StatusBadRequest, err.Error(), "remote_not_configured")
		case errors.Is(err, ErrNotPaired):
			WriteError(w, http.StatusBadRequest, err.Error(), "not_paired")
		default:
			WriteError(w, http.StatusInternalServerError, err.Error(), "internal")
		}
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"connected": connected,
		"message":   message,
	})
}

// Unpair serves POST /api/remote/unpair. Clears the device's local cloud
// credentials + cached rules and disconnects it; the cloud account record
// is unaffected (revoke it from the cloud portal to remove it there).
func (h *RemoteHandler) Unpair(w http.ResponseWriter, r *http.Request) {
	if err := h.acc.Unpair(r.Context()); err != nil {
		switch {
		case errors.Is(err, ErrRemoteNotConfigured):
			WriteError(w, http.StatusBadRequest, err.Error(), "remote_not_configured")
		default:
			WriteError(w, http.StatusInternalServerError, err.Error(), "internal")
		}
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"unpaired": true})
}
