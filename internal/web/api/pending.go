package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/aplexica/aplexica/internal/pending"
)

// PendingAccessor is the seam between the live pending.List + project
// registry + orchestrator refanout pipeline and the API handler. Link
// is equivalent to `aplexica project link <id> <path>` — it adds the
// project to the registry and triggers refanout of any waiting
// artifacts.
type PendingAccessor interface {
	List() ([]pending.Project, error)
	Link(projectID, localPath string) error
}

// ErrPendingNotFound is the sentinel an accessor returns when Link is
// called against a project ID with no waiting artifacts. Mapped to 404.
var ErrPendingNotFound = errors.New("api: pending project not found")

// PendingHandler serves the two /api/pending endpoints.
type PendingHandler struct {
	acc PendingAccessor
}

// NewPendingHandler returns a PendingHandler bound to acc.
func NewPendingHandler(acc PendingAccessor) *PendingHandler {
	return &PendingHandler{acc: acc}
}

// Register attaches the two pending routes.
func (h *PendingHandler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/pending", h.List)
	mux.HandleFunc("POST /api/pending/{id}/link", h.Link)
}

// List serves GET /api/pending.
func (h *PendingHandler) List(w http.ResponseWriter, _ *http.Request) {
	out, err := h.acc.List()
	if err != nil {
		WriteError(w, http.StatusInternalServerError, err.Error(), "internal")
		return
	}
	if out == nil {
		out = []pending.Project{}
	}
	WriteJSON(w, http.StatusOK, out)
}

// linkReq is the POST body shape.
type linkReq struct {
	LocalPath string `json:"localPath"`
}

// Link serves POST /api/pending/{id}/link.
func (h *PendingHandler) Link(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		WriteError(w, http.StatusBadRequest, "project id required", "validation")
		return
	}
	var req linkReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		WriteError(w, http.StatusBadRequest, "invalid JSON: "+err.Error(), "validation")
		return
	}
	if req.LocalPath == "" {
		WriteError(w, http.StatusBadRequest, "localPath is required", "validation")
		return
	}
	if err := h.acc.Link(id, req.LocalPath); err != nil {
		if errors.Is(err, ErrPendingNotFound) {
			WriteError(w, http.StatusNotFound, "pending project not found: "+id, "not_found")
			return
		}
		WriteError(w, http.StatusBadRequest, err.Error(), "validation")
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"linked": id, "localPath": req.LocalPath})
}
