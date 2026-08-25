package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/aplexica/aplexica/internal/conflicts"
)

// ConflictsAccessor is the seam between the live conflicts.Store +
// orchestrator and the API handler. Resolve receives a free-form
// manualBody string (interpreted by the accessor per the artifact's
// kind — markdown for memory, JSON for skills, etc.); empty for
// accept-a / accept-b.
type ConflictsAccessor interface {
	List() ([]conflicts.Conflict, error)
	Get(id string) (conflicts.Conflict, bool, error)
	Resolve(id, action, manualBody string) error
}

// ConflictListSummary optionally enriches the compact conflicts list with a
// reader-facing name for artifacts whose raw ID is not meaningful in the UI.
type ConflictListSummary struct {
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
}

type conflictListSummarizer interface {
	ConflictListSummary(conflicts.Conflict) (ConflictListSummary, bool)
}

type conflictAnalyzer interface {
	Analyze(conflicts.Conflict) (*ConflictAnalysis, error)
}

// ErrConflictNotFound is the sentinel an accessor returns when Resolve
// is called against a conflict that doesn't exist.
var ErrConflictNotFound = errors.New("api: conflict not found")

// Allowed resolve actions per spec §6.6.
const (
	ResolveAcceptA = "accept-a"
	ResolveAcceptB = "accept-b"
	ResolveManual  = "manual"
)

// ConflictsHandler serves the three /api/conflicts endpoints.
type ConflictsHandler struct {
	acc ConflictsAccessor
}

// NewConflictsHandler returns a ConflictsHandler bound to acc.
func NewConflictsHandler(acc ConflictsAccessor) *ConflictsHandler {
	return &ConflictsHandler{acc: acc}
}

// Register attaches the three conflicts routes.
func (h *ConflictsHandler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/conflicts", h.List)
	mux.HandleFunc("GET /api/conflicts/{id}", h.Get)
	mux.HandleFunc("POST /api/conflicts/{id}/resolve", h.Resolve)
}

// List serves GET /api/conflicts.
func (h *ConflictsHandler) List(w http.ResponseWriter, _ *http.Request) {
	out, err := h.acc.List()
	if err != nil {
		WriteError(w, http.StatusInternalServerError, err.Error(), "internal")
		return
	}
	if out == nil {
		out = []conflicts.Conflict{}
	}
	out = summarizeConflictsForList(out)
	WriteJSON(w, http.StatusOK, conflictListResponse(out, h.acc))
}

func summarizeConflictsForList(list []conflicts.Conflict) []conflicts.Conflict {
	if len(list) == 0 {
		return list
	}
	out := make([]conflicts.Conflict, len(list))
	for i, c := range list {
		out[i] = c
		if len(c.Heads) == 0 {
			continue
		}
		out[i].Heads = make([]conflicts.Head, len(c.Heads))
		copy(out[i].Heads, c.Heads)
		for j := range out[i].Heads {
			out[i].Heads[j].PayloadPreview = ""
			out[i].Heads[j].FullPayload = nil
		}
	}
	return out
}

type conflictListItem struct {
	conflicts.Conflict
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
}

func conflictListResponse(list []conflicts.Conflict, acc ConflictsAccessor) []conflictListItem {
	out := make([]conflictListItem, len(list))
	var summarizer conflictListSummarizer
	if s, ok := acc.(conflictListSummarizer); ok {
		summarizer = s
	}
	for i, c := range list {
		out[i] = conflictListItem{Conflict: c}
		if summarizer == nil {
			continue
		}
		summary, ok := summarizer.ConflictListSummary(c)
		if !ok {
			continue
		}
		out[i].Title = summary.Title
		out[i].Description = summary.Description
	}
	return out
}

// Get serves GET /api/conflicts/{id}.
func (h *ConflictsHandler) Get(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		WriteError(w, http.StatusBadRequest, "conflict id required", "validation")
		return
	}
	c, ok, err := h.acc.Get(id)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, err.Error(), "internal")
		return
	}
	if !ok {
		WriteError(w, http.StatusNotFound, "conflict not found: "+id, "not_found")
		return
	}
	out := conflictDetailResponse{Conflict: summarizeConflictForDetail(c)}
	if analyzer, ok := h.acc.(conflictAnalyzer); ok {
		analysis, err := analyzer.Analyze(c)
		if err != nil {
			WriteError(w, http.StatusInternalServerError, err.Error(), "internal")
			return
		}
		out.Analysis = analysis
	}
	WriteJSON(w, http.StatusOK, out)
}

type conflictDetailResponse struct {
	conflicts.Conflict
	Analysis *ConflictAnalysis `json:"analysis,omitempty"`
}

func summarizeConflictForDetail(c conflicts.Conflict) conflicts.Conflict {
	if len(c.Heads) == 0 {
		return c
	}
	out := c
	out.Heads = make([]conflicts.Head, len(c.Heads))
	copy(out.Heads, c.Heads)
	for i := range out.Heads {
		out.Heads[i].FullPayload = nil
	}
	return out
}

// resolveReq is the POST body shape. action is required; manualBody
// is required iff action == "manual" and ignored otherwise.
type resolveReq struct {
	Action     string `json:"action"`
	ManualBody string `json:"manualBody"`
}

// Resolve serves POST /api/conflicts/{id}/resolve.
func (h *ConflictsHandler) Resolve(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		WriteError(w, http.StatusBadRequest, "conflict id required", "validation")
		return
	}
	var req resolveReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		WriteError(w, http.StatusBadRequest, "invalid JSON: "+err.Error(), "validation")
		return
	}
	switch req.Action {
	case ResolveAcceptA, ResolveAcceptB:
		// no body required
	case ResolveManual:
		if req.ManualBody == "" {
			WriteError(w, http.StatusBadRequest, "manualBody is required when action=manual", "validation")
			return
		}
	default:
		WriteError(w, http.StatusBadRequest,
			"action must be one of accept-a, accept-b, manual", "validation")
		return
	}
	if err := h.acc.Resolve(id, req.Action, req.ManualBody); err != nil {
		if errors.Is(err, ErrConflictNotFound) {
			WriteError(w, http.StatusNotFound, "conflict not found: "+id, "not_found")
			return
		}
		WriteError(w, http.StatusInternalServerError, err.Error(), "internal")
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"resolved": id, "action": req.Action})
}
