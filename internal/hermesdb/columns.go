package hermesdb

import (
	"database/sql"
	"fmt"
	"strings"
)

// Hermes grows its sessions/messages tables over time and reconciles columns
// declaratively at startup (see hermes_state.py _reconcile_columns), so the
// installed schema is not pinned to a version number we can rely on. An older
// state.db may be missing trailing columns we model (e.g. platform_message_id,
// which shipped Hermes does not actually have); a newer one may carry columns
// we don't. Rather than hard-code a column list — which hard-fails the instant
// the live schema differs — we introspect the live table and use only the
// columns that actually exist, intersected with the ones we know how to map.
//
// The orders below are the canonical superset, listed in scan/insert order.
var sessionColumnOrder = []string{
	"id", "source", "user_id", "model", "model_config", "system_prompt",
	"parent_session_id", "started_at", "ended_at", "end_reason", "message_count",
	"tool_call_count", "input_tokens", "output_tokens", "cache_read_tokens",
	"cache_write_tokens", "reasoning_tokens", "billing_provider", "billing_base_url",
	"billing_mode", "estimated_cost_usd", "actual_cost_usd", "cost_status",
	"cost_source", "pricing_version", "title", "api_call_count",
	"handoff_state", "handoff_platform", "handoff_error",
	"cwd",
}

var messageColumnOrder = []string{
	"role", "content", "tool_call_id", "tool_calls", "tool_name", "timestamp",
	"token_count", "finish_reason", "reasoning", "reasoning_content", "reasoning_details",
	"codex_reasoning_items", "codex_message_items", "platform_message_id",
}

// Core columns must exist in any genuine Hermes schema; their absence means the
// file isn't a Hermes state.db (or is corrupt), so we fail loudly rather than
// silently scanning zero values. The downstream pipeline assumes these.
var (
	sessionCoreColumns = []string{"id", "started_at"}
	messageCoreColumns = []string{"role", "timestamp"}
)

// sessionScanDest maps each session column name to the pointer that both
// rows.Scan (read) and Exec (write) bind to. One map serves both paths because
// database/sql accepts a *T as a Scan destination and as an arg value.
func sessionScanDest(s *SessionRow) map[string]any {
	return map[string]any{
		"id": &s.ID, "source": &s.Source, "user_id": &s.UserID, "model": &s.Model,
		"model_config": &s.ModelConfig, "system_prompt": &s.SystemPrompt,
		"parent_session_id": &s.ParentSessionID, "started_at": &s.StartedAt,
		"ended_at": &s.EndedAt, "end_reason": &s.EndReason, "message_count": &s.MessageCount,
		"tool_call_count": &s.ToolCallCount, "input_tokens": &s.InputTokens,
		"output_tokens": &s.OutputTokens, "cache_read_tokens": &s.CacheReadTokens,
		"cache_write_tokens": &s.CacheWriteTokens, "reasoning_tokens": &s.ReasoningTokens,
		"billing_provider": &s.BillingProvider, "billing_base_url": &s.BillingBaseURL,
		"billing_mode": &s.BillingMode, "estimated_cost_usd": &s.EstimatedCostUSD,
		"actual_cost_usd": &s.ActualCostUSD, "cost_status": &s.CostStatus,
		"cost_source": &s.CostSource, "pricing_version": &s.PricingVersion,
		"title": &s.Title, "api_call_count": &s.APICallCount,
		"handoff_state": &s.HandoffState, "handoff_platform": &s.HandoffPlatform,
		"handoff_error": &s.HandoffError, "cwd": &s.CWD,
	}
}

// messageScanDest mirrors sessionScanDest for MessageRow. session_id is handled
// separately by the caller (it's the FK, not a MessageRow field).
func messageScanDest(m *MessageRow) map[string]any {
	return map[string]any{
		"role": &m.Role, "content": &m.Content, "tool_call_id": &m.ToolCallID,
		"tool_calls": &m.ToolCalls, "tool_name": &m.ToolName, "timestamp": &m.Timestamp,
		"token_count": &m.TokenCount, "finish_reason": &m.FinishReason,
		"reasoning": &m.Reasoning, "reasoning_content": &m.ReasoningContent,
		"reasoning_details": &m.ReasoningDetails, "codex_reasoning_items": &m.CodexReasoningItems,
		"codex_message_items": &m.CodexMessageItems, "platform_message_id": &m.PlatformMessageID,
	}
}

// bindColumns returns the scan/exec targets for cols, in order, from dest.
func bindColumns(dest map[string]any, cols []string) []any {
	out := make([]any, len(cols))
	for i, c := range cols {
		out[i] = dest[c]
	}
	return out
}

// tableColumns returns the set of column names present in table, via
// PRAGMA table_info. table is always a package constant ("sessions" /
// "messages"), so interpolation carries no injection risk.
func tableColumns(db *sql.DB, table string) (map[string]bool, error) {
	rows, err := db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return nil, fmt.Errorf("hermesdb: introspect %s columns: %w", table, err)
	}
	defer rows.Close()
	cols := make(map[string]bool)
	for rows.Next() {
		var (
			cid     int
			name    string
			ctype   string
			notnull int
			dflt    sql.NullString
			pk      int
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return nil, fmt.Errorf("hermesdb: scan %s column info: %w", table, err)
		}
		cols[name] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("hermesdb: iterate %s column info: %w", table, err)
	}
	return cols, nil
}

// presentColumns returns the subset of order present in have, preserving order.
func presentColumns(order []string, have map[string]bool) []string {
	out := make([]string, 0, len(order))
	for _, c := range order {
		if have[c] {
			out = append(out, c)
		}
	}
	return out
}

// requireCore returns an error naming the first core column missing from have.
// The error wraps ErrNotHermesDB so callers (e.g. the hermeswatch loop) can
// distinguish this permanent "wrong file" condition from a transient DB error
// and stop polling instead of logging the same failure every tick.
func requireCore(table string, core []string, have map[string]bool) error {
	for _, c := range core {
		if !have[c] {
			return fmt.Errorf("hermesdb: %s table missing required column %q: %w", table, c, ErrNotHermesDB)
		}
	}
	return nil
}

// liveColumns introspects the installed Hermes schema and returns the ordered
// subset of session and message columns that actually exist. This is the single
// point that adapts every query to the live schema.
func liveColumns(db *sql.DB) (sessionCols, messageCols []string, err error) {
	sHave, err := tableColumns(db, "sessions")
	if err != nil {
		return nil, nil, err
	}
	if err := requireCore("sessions", sessionCoreColumns, sHave); err != nil {
		return nil, nil, err
	}
	mHave, err := tableColumns(db, "messages")
	if err != nil {
		return nil, nil, err
	}
	if err := requireCore("messages", messageCoreColumns, mHave); err != nil {
		return nil, nil, err
	}
	return presentColumns(sessionColumnOrder, sHave), presentColumns(messageColumnOrder, mHave), nil
}

// placeholders returns "?, ?, ..." with n placeholders.
func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.Repeat("?, ", n-1) + "?"
}
