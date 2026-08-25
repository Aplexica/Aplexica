package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
)

// ErrRBACUnavailable is the sentinel an RBACAccessor returns when the role
// could not be resolved transiently (e.g. the remote plugin is reconnecting).
// Mapped to a 503 with code "rbac_unavailable" so the SPA retries rather than
// rendering a misleading "no access". It is distinct from a no-membership
// answer, which the accessor reports as an empty role with a nil error.
var ErrRBACUnavailable = errors.New("api: namespace role temporarily unavailable")

// RBACAccessor is the seam between the local web handler and the daemon-side
// RoleService. The wiring layer (cmd/aplexica/cmd_daemon_web_api.go)
// implements it by resolving the caller's role for the namespace over the
// remote plugin and deriving its capabilities. A caller with no membership
// (or an unpaired daemon) is reported as an empty role + empty capability
// slice with a nil error — the endpoint is total. A transient resolution
// failure returns ErrRBACUnavailable; any other error is surfaced as a 500.
type RBACAccessor interface {
	NamespaceRole(ctx context.Context, namespaceID string) (role string, capabilities []string, err error)
}

// RBACHandler serves the client-side RBAC introspection endpoint. Construct
// with NewRBACHandler and pass to web.Server.UseProtected — it runs behind
// RequireSession + RequireCSRF and so does not re-check auth.
type RBACHandler struct {
	acc RBACAccessor
}

// NewRBACHandler returns an RBACHandler bound to acc.
func NewRBACHandler(acc RBACAccessor) *RBACHandler {
	return &RBACHandler{acc: acc}
}

// Register attaches the RBAC route.
func (h *RBACHandler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/rbac/namespace/{id}", h.Get)
}

// Get serves GET /api/rbac/namespace/{id}. It returns the caller's resolved
// role and capability list for the namespace so the local UI can reflect what
// the user may do. Deny-by-default: a no-membership / unpaired caller gets
// role:"" and capabilities:[] (200), never a silent grant.
func (h *RBACHandler) Get(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		WriteError(w, http.StatusBadRequest, "namespace id is required", "validation")
		return
	}
	role, capabilities, err := h.acc.NamespaceRole(r.Context(), id)
	if err != nil {
		switch {
		case errors.Is(err, ErrRBACUnavailable):
			WriteError(w, http.StatusServiceUnavailable, err.Error(), "rbac_unavailable")
		default:
			WriteError(w, http.StatusInternalServerError, err.Error(), "internal")
		}
		return
	}
	// Never emit a null capabilities array — the SPA iterates it
	// unconditionally.
	if capabilities == nil {
		capabilities = []string{}
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"role":         role,
		"capabilities": capabilities,
	})
}
