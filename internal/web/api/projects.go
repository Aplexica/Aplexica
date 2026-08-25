package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"

	"github.com/aplexica/aplexica/internal/pending"
	"github.com/aplexica/aplexica/internal/project"
)

// ProjectsHandler serves the project-management endpoints: listing the
// registered projects, registering a folder manually, and approving a
// discovered/pending folder with a chosen scope.
//
// Unlike the older PendingHandler (which speaks to the daemon through a
// small accessor interface), this handler depends directly on the
// project *Registry plus an injected onRegister callback. onRegister is
// invoked AFTER a successful AddOrUpdate so the daemon can start
// watching the folder and backfill its parked artifacts; the live
// callback is wired in the daemon-wiring task, and tests pass a stub.
type ProjectsHandler struct {
	reg           *project.Registry
	denied        *pending.DeniedStore
	suggDismissed *pending.DeniedStore
	onRegister    func(projectID, path string) error
	onUnregister  func(projectID, path string) error
	projectMemory func(projectID string) ([]ProjectMemoryFile, error)
}

// ProjectMemoryFile is one agent's effective (fully composed) memory for a
// project — e.g. Claude's CLAUDE.md base + folded auto-memories, or Codex's
// AGENTS.md — so the user can confirm cross-agent parity at a glance.
type ProjectMemoryFile struct {
	Name         string   `json:"name"`
	SourcePath   string   `json:"sourcePath"`
	Content      string   `json:"content"`
	SyncedAgents []string `json:"syncedAgents"`
}

// WithSuggestionsDismissed wires the store backing the dismiss-suggestion route
// (keyed by pending.SuggestionKey). Returns the handler for chaining. When nil,
// DismissSuggestion returns 503.
func (h *ProjectsHandler) WithSuggestionsDismissed(s *pending.DeniedStore) *ProjectsHandler {
	h.suggDismissed = s
	return h
}

// WithProjectMemory wires the resolver for GET /api/projects/{id}/memory.
// When nil, that route returns 503.
func (h *ProjectsHandler) WithProjectMemory(fn func(projectID string) ([]ProjectMemoryFile, error)) *ProjectsHandler {
	h.projectMemory = fn
	return h
}

// NewProjectsHandler binds a ProjectsHandler to the registry and the
// post-registration callback. onRegister may be nil (treated as a
// no-op) so callers that don't yet have a watcher wired can still
// register the routes. The deny store and unregister callback are nil
// (Deny/Restore return 503, Remove just unregisters); use
// NewProjectsHandlerWithDeny to wire them.
func NewProjectsHandler(reg *project.Registry, onRegister func(projectID, path string) error) *ProjectsHandler {
	return &ProjectsHandler{reg: reg, onRegister: onRegister}
}

// NewProjectsHandlerWithDeny binds the full handler: registry, denied-projects
// store, and the post-(un)registration callbacks. onUnregister fires after a
// successful Remove so the daemon can stop watching the folder; the deny store
// backs the deny/restore routes and is cleared when a denied folder is approved.
func NewProjectsHandlerWithDeny(
	reg *project.Registry,
	denied *pending.DeniedStore,
	onRegister func(projectID, path string) error,
	onUnregister func(projectID, path string) error,
) *ProjectsHandler {
	return &ProjectsHandler{reg: reg, denied: denied, onRegister: onRegister, onUnregister: onUnregister}
}

// Register attaches the three project routes. The approve route lives
// under /api/pending/{id}/approve so the SPA's pending-list view can
// promote a discovered folder in place; the existing
// /api/pending/{id}/link route remains owned by PendingHandler.
func (h *ProjectsHandler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/projects", h.List)
	mux.HandleFunc("GET /api/projects/{id}/memory", h.ProjectMemory)
	mux.HandleFunc("POST /api/projects", h.Create)
	mux.HandleFunc("DELETE /api/projects/{id}", h.Remove)
	mux.HandleFunc("POST /api/pending/{id}/approve", h.Approve)
	mux.HandleFunc("POST /api/pending/{id}/deny", h.Deny)
	mux.HandleFunc("POST /api/pending/{id}/restore", h.Restore)
	mux.HandleFunc("POST /api/projects/{id}/dismiss-suggestion", h.DismissSuggestion)
}

// projectView is the wire shape for a registered project. Scope is the
// EffectiveScope (empty defaults to "local") so the SPA never sees a
// blank scope.
type projectView struct {
	ID          string   `json:"id"`
	Path        string   `json:"path"`
	Scope       string   `json:"scope"`
	Agents      []string `json:"agents"`
	DisplayName string   `json:"displayName"`
	VCS         string   `json:"vcs"`
}

func toView(e project.Entry) projectView {
	agents := e.Agents
	if agents == nil {
		agents = []string{}
	}
	return projectView{
		ID:          e.ID,
		Path:        e.Path,
		Scope:       e.EffectiveScope(),
		Agents:      agents,
		DisplayName: e.DisplayName,
		VCS:         e.VCS,
	}
}

// List serves GET /api/projects.
func (h *ProjectsHandler) List(w http.ResponseWriter, _ *http.Request) {
	entries := h.reg.List()
	out := make([]projectView, 0, len(entries))
	for _, e := range entries {
		out = append(out, toView(e))
	}
	WriteJSON(w, http.StatusOK, out)
}

// ProjectMemory serves GET /api/projects/{id}/memory — the effective composed
// memory each agent holds for the project, so cross-agent parity is visible
// (Claude's CLAUDE.md + folded auto-memories vs Codex's AGENTS.md, etc.).
func (h *ProjectsHandler) ProjectMemory(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		WriteError(w, http.StatusBadRequest, "project id is required", "validation")
		return
	}
	if h.projectMemory == nil {
		WriteError(w, http.StatusServiceUnavailable, "memory resolver not configured", "internal")
		return
	}
	files, err := h.projectMemory(id)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, err.Error(), "internal")
		return
	}
	if files == nil {
		files = []ProjectMemoryFile{}
	}
	WriteJSON(w, http.StatusOK, files)
}

// projectCreateReq is the POST /api/projects body. Scope is optional
// (empty → "local"); Agents is optional (nil → "all installed agents").
type projectCreateReq struct {
	Path   string   `json:"path"`
	Scope  string   `json:"scope"`
	Agents []string `json:"agents"`
}

// Create serves POST /api/projects — register a folder manually.
func (h *ProjectsHandler) Create(w http.ResponseWriter, r *http.Request) {
	var req projectCreateReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		WriteError(w, http.StatusBadRequest, "invalid JSON: "+err.Error(), "validation")
		return
	}
	h.register(w, req.Path, req.Scope, req.Agents, http.StatusCreated)
}

// projectApproveReq is the POST /api/pending/{id}/approve body. Path is
// the folder to register — for a discovered entry the SPA carries the
// pending list's SamplePath through here, since this handler doesn't
// hold the pending list itself.
type projectApproveReq struct {
	Scope  string   `json:"scope"`
	Agents []string `json:"agents"`
	Path   string   `json:"path"`
}

// Approve serves POST /api/pending/{id}/approve — promote a
// discovered/pending folder to a registered project with a chosen
// scope. The {id} path segment is informational (the canonical ID is
// re-derived from the resolved path); the folder path comes from the
// request body.
func (h *ProjectsHandler) Approve(w http.ResponseWriter, r *http.Request) {
	var req projectApproveReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		WriteError(w, http.StatusBadRequest, "invalid JSON: "+err.Error(), "validation")
		return
	}
	h.register(w, req.Path, req.Scope, req.Agents, http.StatusOK)
}

// validScope reports whether s is an accepted scope value. Empty is
// accepted and defaulted to "local" by the caller.
func validScope(s string) bool {
	return s == "" || s == "local" || s == "global"
}

// register is the shared body for Create + Approve: validate the path
// + scope, resolve the canonical ID/VCS, upsert the registry entry,
// fire onRegister, and write the entry as JSON at successStatus.
func (h *ProjectsHandler) register(w http.ResponseWriter, path, scope string, agents []string, successStatus int) {
	if path == "" {
		WriteError(w, http.StatusBadRequest, "path is required", "validation")
		return
	}
	if !validScope(scope) {
		WriteError(w, http.StatusBadRequest, "scope must be \"local\" or \"global\"", "validation")
		return
	}

	abs, err := filepath.Abs(path)
	if err != nil {
		WriteError(w, http.StatusBadRequest, "resolve path: "+err.Error(), "validation")
		return
	}
	fi, err := os.Stat(abs)
	if err != nil || !fi.IsDir() {
		WriteError(w, http.StatusBadRequest, "path does not exist or is not a directory: "+abs, "validation")
		return
	}
	physical, err := filepath.EvalSymlinks(abs)
	if err != nil {
		WriteError(w, http.StatusBadRequest, "resolve physical path: "+err.Error(), "validation")
		return
	}
	abs = filepath.Clean(physical)

	// Derive the canonical ID + VCS from the folder. project.Detect
	// already returns a path-derived ID with VCS "none" for a non-VCS
	// folder and only errors on path resolution itself; on that rare
	// error we fall back to a stable path-derived ID so registration is
	// never blocked on identity edge cases.
	var id, vcs string
	if info, derr := project.Detect(abs); derr == nil {
		id = info.ID
		vcs = info.VCS
	} else {
		id = project.PathDerivedID(abs)
		vcs = "none"
	}

	if scope == "" {
		scope = "local"
	}

	entry := project.Entry{
		ID:          id,
		Path:        abs,
		VCS:         vcs,
		Scope:       scope,
		Agents:      agents,
		DisplayName: filepath.Base(abs),
	}
	if err := h.reg.AddOrUpdate(entry); err != nil {
		// The displacement refusal (a second live clone of an already
		// registered repository) is a deterministic, user-actionable
		// conflict, not a server fault — surface it as 409 so the SPA can
		// present it and 5xx monitoring does not count it as a backend
		// failure.
		if errors.Is(err, project.ErrLocationDisplacement) {
			WriteError(w, http.StatusConflict, err.Error(), "conflict")
			return
		}
		WriteError(w, http.StatusInternalServerError, err.Error(), "internal")
		return
	}

	if persisted, ok := h.reg.Get(id); ok {
		entry = persisted
	}

	if h.onRegister != nil {
		if err := h.onRegister(id, entry.Path); err != nil {
			WriteError(w, http.StatusInternalServerError, "register callback: "+err.Error(), "internal")
			return
		}
	}

	// Approving (or re-adding) a folder clears any prior denial of it, so a
	// re-approved folder leaves the denied list.
	if h.denied != nil {
		_ = h.denied.Remove(id)
	}

	WriteJSON(w, successStatus, toView(entry))
}

// Remove serves DELETE /api/projects/{id} — unregister a project so it stops
// being watched and re-surfaces as a discovered pending folder on the next
// discovery pass. Idempotent: removing an unknown id still returns 204.
func (h *ProjectsHandler) Remove(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		WriteError(w, http.StatusBadRequest, "project id is required", "validation")
		return
	}
	entry, existed := h.reg.Get(id)
	if err := h.reg.Remove(id); err != nil {
		WriteError(w, http.StatusInternalServerError, err.Error(), "internal")
		return
	}
	if existed && h.onUnregister != nil {
		if err := h.onUnregister(id, entry.Path); err != nil {
			WriteError(w, http.StatusInternalServerError, "unregister callback: "+err.Error(), "internal")
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// denyReq is the POST /api/pending/{id}/deny body. Path lets the denied entry
// render even when discovery later stops surfacing the folder.
type denyReq struct {
	Path string `json:"path"`
}

// Deny serves POST /api/pending/{id}/deny — dismiss a discovered folder. It
// moves to the denied list (out of the active pending count) until the user
// re-approves or restores it.
func (h *ProjectsHandler) Deny(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		WriteError(w, http.StatusBadRequest, "pending id is required", "validation")
		return
	}
	if h.denied == nil {
		WriteError(w, http.StatusServiceUnavailable, "deny store not configured", "internal")
		return
	}
	var req denyReq
	_ = json.NewDecoder(r.Body).Decode(&req) // path is best-effort metadata
	if err := h.denied.Add(id, req.Path); err != nil {
		WriteError(w, http.StatusInternalServerError, err.Error(), "internal")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Restore serves POST /api/pending/{id}/restore — un-deny a folder so it
// returns to the active pending list (without registering it).
func (h *ProjectsHandler) Restore(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		WriteError(w, http.StatusBadRequest, "pending id is required", "validation")
		return
	}
	if h.denied == nil {
		WriteError(w, http.StatusServiceUnavailable, "deny store not configured", "internal")
		return
	}
	if err := h.denied.Remove(id); err != nil {
		WriteError(w, http.StatusInternalServerError, err.Error(), "internal")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// dismissSuggestionReq is the POST /api/projects/{id}/dismiss-suggestion body.
type dismissSuggestionReq struct {
	Agent string `json:"agent"`
}

// DismissSuggestion serves POST /api/projects/{id}/dismiss-suggestion —
// suppress the "add <agent> to this project" suggestion so it stops appearing.
// Keyed per (project, agent) so dismissing one agent doesn't hide others.
func (h *ProjectsHandler) DismissSuggestion(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		WriteError(w, http.StatusBadRequest, "project id is required", "validation")
		return
	}
	if h.suggDismissed == nil {
		WriteError(w, http.StatusServiceUnavailable, "suggestions store not configured", "internal")
		return
	}
	var req dismissSuggestionReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Agent == "" {
		WriteError(w, http.StatusBadRequest, "agent is required", "validation")
		return
	}
	if err := h.suggDismissed.Add(pending.SuggestionKey(id, req.Agent), ""); err != nil {
		WriteError(w, http.StatusInternalServerError, err.Error(), "internal")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
