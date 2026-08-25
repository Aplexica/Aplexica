// Package hermesdb is a narrow Go wrapper around Hermes's session SQLite
// database (~/.hermes/state.db). It exposes only the operations Aplexica
// needs: list/read sessions, insert sessions. It does NOT speak the full
// SessionDB API — Hermes itself owns schema migrations.
//
// Driver: modernc.org/sqlite (pure Go, CGO-free, FTS5 included).
package hermesdb

import (
	"database/sql"
	_ "embed"
	"fmt"
	"os"

	_ "modernc.org/sqlite"
)

// embeddedSchema holds the canonical Hermes empty-state schema as text so
// InitTestDB works regardless of the test's working directory (which differs
// between in-package tests and sibling-package tests like
// internal/adapter/hermes).
//
//go:embed testdata/empty-state-schema.sql
var embeddedSchema string

// SessionBundle is the in-memory + on-disk JSON representation of one Hermes
// session: the session row plus all its messages. The field set is the
// canonical superset hermesdb knows how to map; queries adapt to the columns
// the installed Hermes schema actually has (see columns.go). Columns present in
// the struct but absent from the live schema (e.g. PlatformMessageID, which
// shipped Hermes does not have) are skipped on read and write — never assumed.
type SessionBundle struct {
	Session  SessionRow   `json:"session"`
	Messages []MessageRow `json:"messages"`
}

type SessionRow struct {
	ID               string   `json:"id"`
	Source           string   `json:"source"`
	UserID           *string  `json:"user_id,omitempty"`
	Model            *string  `json:"model,omitempty"`
	ModelConfig      *string  `json:"model_config,omitempty"` // raw JSON text
	SystemPrompt     *string  `json:"system_prompt,omitempty"`
	ParentSessionID  *string  `json:"parent_session_id,omitempty"`
	StartedAt        float64  `json:"started_at"`
	EndedAt          *float64 `json:"ended_at,omitempty"`
	EndReason        *string  `json:"end_reason,omitempty"`
	MessageCount     int64    `json:"message_count"`
	ToolCallCount    int64    `json:"tool_call_count"`
	InputTokens      int64    `json:"input_tokens"`
	OutputTokens     int64    `json:"output_tokens"`
	CacheReadTokens  int64    `json:"cache_read_tokens"`
	CacheWriteTokens int64    `json:"cache_write_tokens"`
	ReasoningTokens  int64    `json:"reasoning_tokens"`
	BillingProvider  *string  `json:"billing_provider,omitempty"`
	BillingBaseURL   *string  `json:"billing_base_url,omitempty"`
	BillingMode      *string  `json:"billing_mode,omitempty"`
	EstimatedCostUSD *float64 `json:"estimated_cost_usd,omitempty"`
	ActualCostUSD    *float64 `json:"actual_cost_usd,omitempty"`
	CostStatus       *string  `json:"cost_status,omitempty"`
	CostSource       *string  `json:"cost_source,omitempty"`
	PricingVersion   *string  `json:"pricing_version,omitempty"`
	Title            *string  `json:"title,omitempty"`
	APICallCount     int64    `json:"api_call_count"`
	HandoffState     *string  `json:"handoff_state,omitempty"`
	HandoffPlatform  *string  `json:"handoff_platform,omitempty"`
	HandoffError     *string  `json:"handoff_error,omitempty"`
	CWD              *string  `json:"cwd,omitempty"`
}

type MessageRow struct {
	Role                string  `json:"role"`
	Content             *string `json:"content,omitempty"`
	ToolCallID          *string `json:"tool_call_id,omitempty"`
	ToolCalls           *string `json:"tool_calls,omitempty"` // raw JSON text
	ToolName            *string `json:"tool_name,omitempty"`
	Timestamp           float64 `json:"timestamp"`
	TokenCount          *int64  `json:"token_count,omitempty"`
	FinishReason        *string `json:"finish_reason,omitempty"`
	Reasoning           *string `json:"reasoning,omitempty"`
	ReasoningContent    *string `json:"reasoning_content,omitempty"`
	ReasoningDetails    *string `json:"reasoning_details,omitempty"`
	CodexReasoningItems *string `json:"codex_reasoning_items,omitempty"`
	CodexMessageItems   *string `json:"codex_message_items,omitempty"`
	PlatformMessageID   *string `json:"platform_message_id,omitempty"`
}

// busyTimeoutMS is the SQLite busy_timeout (in milliseconds) applied to every
// connection we open against a state.db owned by a live Hermes process. A brief
// exclusive-lock window (e.g. a WAL checkpoint) then makes a read/write wait
// rather than fail immediately with SQLITE_BUSY.
const busyTimeoutMS = 5000

// OpenRO opens the DB for reading. URI mode=ro lets us read a DB owned by a
// live Hermes process without holding any write lock (WAL gives snapshot
// isolation per read transaction). The busy_timeout pragma keeps a momentary
// lock (e.g. a WAL checkpoint) from surfacing as a spurious SQLITE_BUSY on the
// hot poll path, matching OpenRW.
func OpenRO(path string) (*sql.DB, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("hermesdb: open %s: %w", path, err)
	}
	db, err := sql.Open("sqlite", fmt.Sprintf("file:%s?mode=ro&_pragma=busy_timeout(%d)", path, busyTimeoutMS))
	if err != nil {
		return nil, fmt.Errorf("hermesdb: open ro %s: %w", path, err)
	}
	db.SetMaxOpenConns(1) // serialize for predictability
	return db, nil
}

// OpenRW opens the DB for read-write. Caller must own/be able to write the
// file. Does NOT create the schema — that is Hermes's job.
func OpenRW(path string) (*sql.DB, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("hermesdb: open %s (does it exist? run hermes once to init): %w", path, err)
	}
	db, err := sql.Open("sqlite", fmt.Sprintf("file:%s?_pragma=busy_timeout(%d)", path, busyTimeoutMS))
	if err != nil {
		return nil, fmt.Errorf("hermesdb: open rw %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

// InitTestDB applies the empty-state-schema fixture to a fresh file. Test-only
// helper — production code never creates the schema. The schema fixture is
// embedded via //go:embed so callers in sibling packages work without any
// working-directory gymnastics.
func InitTestDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(embeddedSchema); err != nil {
		return nil, fmt.Errorf("hermesdb: apply schema: %w", err)
	}
	return db, nil
}
