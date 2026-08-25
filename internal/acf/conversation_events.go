package acf

import (
	"encoding/json"
	"time"
)

// ConversationFormatV1 is the Format string used by ConversationPayload when
// the structured Events field is the source of truth.
//
// Legacy formats (claude-code.session.jsonl, codex.session.jsonl,
// acf.hermes.session.v1) continue to use the opaque Content field and are
// not migrated.
const ConversationFormatV1 = "acf.conversation.v1"

// ConversationDeltaFormatV1 is an append-only update payload for canonical
// conversations. Events contains only newly appended canonical events; readers
// must replay the artifact log to materialize the full ConversationFormatV1
// state.
const ConversationDeltaFormatV1 = "acf.conversation.delta.v1"

// ConversationEvent kinds. The first four implement the V1 event-log contract.
// fork / merge / snapshot
// support conversation-branching workflows (added in v0.25.0).
const (
	EventTypeTurn       = "turn"
	EventTypeToolCall   = "tool_call"
	EventTypeToolResult = "tool_result"
	EventTypeSystemNote = "system_note"

	// Added in v0.25.0 for conversation-branching workflows.
	EventTypeFork     = "fork"
	EventTypeMerge    = "merge"
	EventTypeSnapshot = "snapshot"

	// EventTypeResolution is appended by `aplexica conflicts resolve` to
	// re-assert the winning payload as canonical. Distinct from
	// EventTypeUpdate so conflict resolution is auditable in the event log
	// (the provenance is "aplexica:resolve" but the type is the clear
	// signal). Replay paths (ExportOpaque, all adapter replay switches,
	// LatestEventFormat, retention/snapshots) treat it identically to
	// EventTypeCreate/EventTypeUpdate — it re-asserts a full payload.
	// Added v0.34.0.
	EventTypeResolution = "resolution"
)

// ConversationEvent is one event in a canonical conversation. Fields are
// mostly optional and grouped by which event Types use them — see comments.
// JSON serialization uses omitempty so events stay compact.
type ConversationEvent struct {
	Type      string    `json:"type"`
	Timestamp time.Time `json:"timestamp,omitempty"`

	// Turn / tool_result / system_note: the content blocks for this event.
	Content []ContentBlock `json:"content,omitempty"`

	// Turn only: "user" | "assistant" | (rarely) "system".
	Role string `json:"role,omitempty"`

	// Tool_call: model-issued invocation.
	CallID   string          `json:"call_id,omitempty"`
	ToolName string          `json:"tool_name,omitempty"`
	Input    json.RawMessage `json:"input,omitempty"`

	// Tool_result: matches a prior tool_call.CallID.
	IsError bool `json:"is_error,omitempty"`

	// Optional event tags per the wiki event-log concept.
	Tags []string `json:"tags,omitempty"`

	// NativeExtras: per-adapter metadata that doesn't fit the canonical model
	// but a re-export back into the SAME adapter wants preserved. Opaque to
	// the canonical layer. Adapters are free to ignore it on import or
	// populate it for fidelity.
	NativeExtras json.RawMessage `json:"native_extras,omitempty"`

	// BranchID names the branch this event belongs to; empty = "main".
	// Used by fork / merge / snapshot events and by any event that wants
	// to attribute itself to a non-main branch (added in v0.25.0).
	BranchID string `json:"branch_id,omitempty"`

	// SourceEventID — fork only: the event ID this branch diverges from.
	SourceEventID string `json:"source_event_id,omitempty"`

	// MergedBranchIDs — merge only: the set of branches being merged into
	// this branch (named by BranchID).
	MergedBranchIDs []string `json:"merged_branch_ids,omitempty"`

	// SnapshotState — snapshot only: a pointer to materialized state at
	// this point (content hash or external URI). The state itself is not
	// embedded — snapshots bound replay cost but don't necessarily inline
	// the bytes.
	SnapshotState string `json:"snapshot_state,omitempty"`
}

// ContentBlock is a typed content fragment. "text" carries Text; other types
// (e.g. "image") use Data — bytes or refs depending on context.
type ContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
	Data string `json:"data,omitempty"` // for non-text blocks; base64 or URI ref
}
