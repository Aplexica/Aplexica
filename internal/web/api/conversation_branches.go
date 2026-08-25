package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"
)

// ErrConversationNotFound is returned by ConversationBranchesAccessor when the
// requested conversation artifact is absent from the canonical store.
var ErrConversationNotFound = errors.New("api: conversation not found")

// ErrConversationBranchNotFound is returned when checkout targets an unknown
// branch on an existing conversation.
var ErrConversationBranchNotFound = errors.New("api: conversation branch not found")

// ConversationBranch is the UI-facing summary of one branch on a conversation.
type ConversationBranch struct {
	Name               string    `json:"name"`
	CreatedAt          time.Time `json:"createdAt,omitzero"`
	LastEventAt        time.Time `json:"lastEventAt,omitzero"`
	Head               string    `json:"head,omitempty"`
	ForkedFrom         string    `json:"forkedFrom,omitempty"`
	ForkedFromHash     string    `json:"forkedFromHash,omitempty"`
	OriginAgent        string    `json:"originAgent,omitempty"`
	Rationale          string    `json:"rationale,omitempty"`
	Archived           bool      `json:"archived,omitempty"`
	MergedInto         string    `json:"mergedInto,omitempty"`
	EventCount         int       `json:"eventCount"`
	MaterializedAgents []string  `json:"materializedAgents"`
}

// ConversationBranchesResponse is returned by GET
// /api/conversations/{id}/branches.
type ConversationBranchesResponse struct {
	ArtifactID string               `json:"artifactId"`
	Branches   []ConversationBranch `json:"branches"`
}

// ConversationForkRequest is the POST /api/conversations/{id}/fork body.
type ConversationForkRequest struct {
	FromEventID string `json:"fromEventId"`
	TargetAgent string `json:"targetAgent"`
	Branch      string `json:"branch,omitempty"`
	Rationale   string `json:"rationale,omitempty"`
}

// ConversationCheckoutRequest is the POST /api/conversations/{id}/checkout body.
type ConversationCheckoutRequest struct {
	Agent  string `json:"agent"`
	Branch string `json:"branch"`
}

// ConversationBranchMutationResponse is returned by fork and checkout.
type ConversationBranchMutationResponse struct {
	ArtifactID    string `json:"artifactId"`
	Branch        string `json:"branch"`
	Agent         string `json:"agent"`
	Path          string `json:"path,omitempty"`
	Materialized  bool   `json:"materialized"`
	Warning       string `json:"warning,omitempty"`
	Operation     string `json:"operation"`
	CreatedBranch bool   `json:"createdBranch,omitempty"`
}

// ConversationBranchesAccessor is the daemon-side seam for conversation branch
// operations. Implementations own canonical-store mutation and immediate
// materialization.
type ConversationBranchesAccessor interface {
	ListConversationBranches(id string) (ConversationBranchesResponse, bool, error)
	ForkConversation(id string, req ConversationForkRequest) (ConversationBranchMutationResponse, error)
	CheckoutConversation(id string, req ConversationCheckoutRequest) (ConversationBranchMutationResponse, error)
}

// ConversationBranchesHandler serves the conversation branch control endpoints.
type ConversationBranchesHandler struct {
	acc ConversationBranchesAccessor
}

// NewConversationBranchesHandler returns a handler bound to acc.
func NewConversationBranchesHandler(acc ConversationBranchesAccessor) *ConversationBranchesHandler {
	return &ConversationBranchesHandler{acc: acc}
}

// Register attaches the conversation branch routes.
func (h *ConversationBranchesHandler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/conversations/{id}/branches", h.List)
	mux.HandleFunc("POST /api/conversations/{id}/fork", h.Fork)
	mux.HandleFunc("POST /api/conversations/{id}/checkout", h.Checkout)
}

// List serves GET /api/conversations/{id}/branches.
func (h *ConversationBranchesHandler) List(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		WriteError(w, http.StatusBadRequest, "conversation id required", "validation")
		return
	}
	out, ok, err := h.acc.ListConversationBranches(id)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, err.Error(), "internal")
		return
	}
	if !ok {
		WriteError(w, http.StatusNotFound, "conversation not found: "+id, "not_found")
		return
	}
	if out.Branches == nil {
		out.Branches = []ConversationBranch{}
	}
	for i := range out.Branches {
		if out.Branches[i].MaterializedAgents == nil {
			out.Branches[i].MaterializedAgents = []string{}
		}
	}
	WriteJSON(w, http.StatusOK, out)
}

// Fork serves POST /api/conversations/{id}/fork.
func (h *ConversationBranchesHandler) Fork(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		WriteError(w, http.StatusBadRequest, "conversation id required", "validation")
		return
	}
	var req ConversationForkRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		WriteError(w, http.StatusBadRequest, "invalid JSON: "+err.Error(), "validation")
		return
	}
	if req.FromEventID == "" {
		WriteError(w, http.StatusBadRequest, "fromEventId is required", "validation")
		return
	}
	if req.TargetAgent == "" {
		WriteError(w, http.StatusBadRequest, "targetAgent is required", "validation")
		return
	}
	out, err := h.acc.ForkConversation(id, req)
	if err != nil {
		writeConversationBranchMutationError(w, id, err)
		return
	}
	WriteJSON(w, http.StatusOK, out)
}

// Checkout serves POST /api/conversations/{id}/checkout.
func (h *ConversationBranchesHandler) Checkout(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		WriteError(w, http.StatusBadRequest, "conversation id required", "validation")
		return
	}
	var req ConversationCheckoutRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		WriteError(w, http.StatusBadRequest, "invalid JSON: "+err.Error(), "validation")
		return
	}
	if req.Agent == "" {
		WriteError(w, http.StatusBadRequest, "agent is required", "validation")
		return
	}
	if req.Branch == "" {
		WriteError(w, http.StatusBadRequest, "branch is required", "validation")
		return
	}
	out, err := h.acc.CheckoutConversation(id, req)
	if err != nil {
		writeConversationBranchMutationError(w, id, err)
		return
	}
	WriteJSON(w, http.StatusOK, out)
}

func writeConversationBranchMutationError(w http.ResponseWriter, id string, err error) {
	switch {
	case errors.Is(err, ErrConversationNotFound):
		WriteError(w, http.StatusNotFound, "conversation not found: "+id, "not_found")
	case errors.Is(err, ErrConversationBranchNotFound):
		WriteError(w, http.StatusNotFound, err.Error(), "not_found")
	default:
		WriteError(w, http.StatusInternalServerError, err.Error(), "internal")
	}
}
