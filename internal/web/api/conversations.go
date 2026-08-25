package api

import (
	"net/http"
	"strconv"
	"time"
)

// ConversationSummary is the picker-facing summary of one conversation
// artifact. Title is derived from the same visible text turns Aplexica syncs
// across agents, so users can search by the short prompt-like description they
// recognize from native agent resume screens.
type ConversationSummary struct {
	ArtifactID     string    `json:"artifactId"`
	Title          string    `json:"title"`
	Description    string    `json:"description,omitempty"`
	SourceAgent    string    `json:"sourceAgent,omitempty"`
	SourcePath     string    `json:"sourcePath,omitempty"`
	CreatedAt      time.Time `json:"createdAt,omitzero"`
	UpdatedAt      time.Time `json:"updatedAt,omitzero"`
	TurnCount      int       `json:"turnCount"`
	BranchCount    int       `json:"branchCount"`
	MaterializedIn []string  `json:"materializedIn"`
	SearchText     string    `json:"-"`
}

// ConversationSearchQuery carries GET /api/conversations filters.
type ConversationSearchQuery struct {
	Query string
	Limit int
}

// ConversationSearchResponse is returned by GET /api/conversations.
type ConversationSearchResponse struct {
	Query         string                `json:"query,omitempty"`
	Conversations []ConversationSummary `json:"conversations"`
}

// ConversationsAccessor is the daemon-side seam for conversation discovery.
type ConversationsAccessor interface {
	SearchConversations(q ConversationSearchQuery) (ConversationSearchResponse, error)
}

// ConversationsHandler serves the conversation picker endpoint.
type ConversationsHandler struct {
	acc ConversationsAccessor
}

// NewConversationsHandler returns a handler bound to acc.
func NewConversationsHandler(acc ConversationsAccessor) *ConversationsHandler {
	return &ConversationsHandler{acc: acc}
}

// Register attaches the conversation list/search route.
func (h *ConversationsHandler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/conversations", h.Search)
}

// conversationSearchDefaultLimit is the page size when the request carries no
// limit= parameter (mirrors the daemon-side accessor default of the same
// name).
const conversationSearchDefaultLimit = 25

// Search serves GET /api/conversations?q=&limit=.
func (h *ConversationsHandler) Search(w http.ResponseWriter, r *http.Request) {
	limit := conversationSearchDefaultLimit
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			WriteError(w, http.StatusBadRequest, "limit must be a positive integer", "validation")
			return
		}
		limit = n
	}
	out, err := h.acc.SearchConversations(ConversationSearchQuery{
		Query: r.URL.Query().Get("q"),
		Limit: limit,
	})
	if err != nil {
		WriteError(w, http.StatusInternalServerError, err.Error(), "internal")
		return
	}
	if out.Conversations == nil {
		out.Conversations = []ConversationSummary{}
	}
	WriteJSON(w, http.StatusOK, out)
}
