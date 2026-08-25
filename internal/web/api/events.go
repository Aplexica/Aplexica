package api

import (
	"net/http"
	"strconv"
	"time"
)

// EventRecord is the wire shape of one event in the /api/events
// backfill response. Seq is a monotonic global cursor — the daemon
// derives it by sorting per-artifact event timestamps (Unix-ms) into a
// total order; the SPA only treats it as opaque (pass the response's
// `nextBefore` back as `before=` to page backward into older history).
//
// Type follows the SSE event-name vocabulary (BRD-03 / W5):
// "daemon.state", "agent.activity", "artifact.imported",
// "artifact.synced", "artifact.checkpoint", "conflict.created",
// "conflict.resolved", "pending.added", "pending.linked",
// "rule.fired". For artifact events the backfill distinguishes a local
// native import ("artifact.imported"), a cross-device sync
// ("artifact.synced"), and an internal retention snapshot
// ("artifact.checkpoint").
type EventRecord struct {
	Seq        int64     `json:"seq"`
	Type       string    `json:"type"`
	Timestamp  time.Time `json:"timestamp"`
	ArtifactID string    `json:"artifactId,omitempty"`
	Kind       string    `json:"kind,omitempty"`
	Agent      string    `json:"agent,omitempty"`
	// Name is the artifact's human label (e.g. "CLAUDE.md") so the portal
	// can render "<agent> synced <kind> <name>" instead of a bare UUID.
	Name string `json:"name,omitempty"`
	// Action is a compact user-facing verb key ("imported", "synced",
	// "checkpointed", "refused") derived by the daemon from the event type and
	// available routing metadata. The portal localizes this key rather than
	// showing raw event-log filenames or UUIDs as the primary label.
	Action string `json:"action,omitempty"`
	// SourcePath is a display-safe, home-redacted path label for the native file
	// that produced the artifact. It is metadata only; artifact body content is
	// never exposed here.
	SourcePath string `json:"sourcePath,omitempty"`
	// TargetAgents are the local agents that currently have this artifact
	// materialized through fan-out. For historical events this is best-effort
	// current metadata, not a per-event immutable routing log.
	TargetAgents []string `json:"targetAgents,omitempty"`
	Scope        string   `json:"scope,omitempty"`
	ProjectID    string   `json:"projectId,omitempty"`
	ProjectPath  string   `json:"projectPath,omitempty"`
	Origin       string   `json:"origin,omitempty"` // local, remote, or system
	Data         any      `json:"data,omitempty"`
}

// EventQuery is the parsed and validated query-string shape that
// reaches the accessor. The events feed is newest-first: Before is an
// exclusive UPPER bound on Seq (return events with Seq < Before), and
// Before <= 0 means "from the newest". Limit is always in [1, 1000]
// after parsing.
type EventQuery struct {
	Before int64
	Limit  int
}

// EventPage is the wire shape returned by GET /api/events. The feed is
// newest-first; NextBefore is the cursor for the next (older) page — pass
// it back as the `before` query param. When the page is short of Limit,
// the tail (oldest history) has been reached.
type EventPage struct {
	Events []EventRecord `json:"events"`
	// NextBefore is the cursor for the next (older) page: pass it back as
	// the `before` query param. When the page is short of Limit, the tail
	// (oldest history) has been reached.
	NextBefore int64 `json:"nextBefore"`
}

// EventsAccessor is the seam between the live event log and the handler. The
// daemon's wiring layer returns the requested newest-first page as EventRecords
// with opaque Seq cursors; implementations may use metadata indexes or bounded
// log reads and do not need to materialize the full history for every request.
//
// Implementations must be safe for concurrent use; the handler does
// not serialise calls.
type EventsAccessor interface {
	Backfill(q EventQuery) (EventPage, error)
}

// Limit clamps for the cursor-based pagination (per the spec).
const (
	defaultEventsLimit = 100
	maxEventsLimit     = 1000
)

// EventsHandler serves GET /api/events.
type EventsHandler struct {
	acc EventsAccessor
}

// NewEventsHandler returns an EventsHandler bound to acc.
func NewEventsHandler(acc EventsAccessor) *EventsHandler {
	return &EventsHandler{acc: acc}
}

// Register attaches the events route.
func (h *EventsHandler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/events", h.Backfill)
}

// Backfill serves GET /api/events?before=&limit=. The feed is newest-first:
// omit `before` for the most recent page, then pass the response's
// `nextBefore` back as `before` to page backward into older history.
func (h *EventsHandler) Backfill(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var before int64
	if s := q.Get("before"); s != "" {
		v, err := strconv.ParseInt(s, 10, 64)
		if err != nil || v < 0 {
			WriteError(w, http.StatusBadRequest, "before must be a non-negative integer", "validation")
			return
		}
		before = v
	}
	limit := defaultEventsLimit
	if s := q.Get("limit"); s != "" {
		v, err := strconv.Atoi(s)
		if err != nil || v <= 0 {
			WriteError(w, http.StatusBadRequest, "limit must be a positive integer", "validation")
			return
		}
		limit = v
	}
	if limit > maxEventsLimit {
		limit = maxEventsLimit
	}

	page, err := h.acc.Backfill(EventQuery{Before: before, Limit: limit})
	if err != nil {
		WriteError(w, http.StatusInternalServerError, err.Error(), "internal")
		return
	}
	if page.Events == nil {
		page.Events = []EventRecord{}
	}
	WriteJSON(w, http.StatusOK, page)
}
