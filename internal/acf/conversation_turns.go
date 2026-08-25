// SPDX-License-Identifier: AGPL-3.0-or-later
package acf

import (
	"encoding/json"
	"regexp"
	"strings"
)

// TextTurn is the round-trip-stable unit of a cross-agent conversation: a single
// user or assistant message reduced to its plain text. Tool calls, reasoning,
// and injected system/context turns are intentionally excluded — they don't
// survive materialization into another agent's native format, so the synced
// "thread" is the human-visible user/assistant exchange. This is the
// representation the bidirectional merge compares on, which is what makes
// Aplexica's own re-materializations inert (no spurious changes → no loops).
type TextTurn struct {
	Role string `json:"role"` // "user" | "assistant"
	Text string `json:"text"`
}

// ExtractTextTurns reduces a canonical conversation event log to its ordered
// user/assistant text turns, dropping tool calls/results, reasoning, empty
// turns, and hidden user scaffolding (NormalizeTextTurn). Stable: the same
// events always yield the same turns.
func ExtractTextTurns(events []ConversationEvent) []TextTurn {
	var out []TextTurn
	for _, ev := range events {
		if ev.Type != EventTypeTurn {
			continue
		}
		if ev.Role != "user" && ev.Role != "assistant" {
			continue
		}
		text, ok := NormalizeTextTurn(ev.Role, joinTextBlocks(ev.Content))
		if !ok {
			continue
		}
		out = append(out, TextTurn{Role: ev.Role, Text: text})
	}
	return out
}

// TextTurnsEqual reports whether two turn sequences are identical. Used by the
// merge to decide whether an imported session actually changed the thread.
func TextTurnsEqual(a, b []TextTurn) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Role != b[i].Role || a[i].Text != b[i].Text {
			return false
		}
	}
	return true
}

// IsInjectedContext reports whether a user-role turn is actually injected
// environment/system context (Codex prepends its system prompt, AGENTS.md, and
// environment as user-role turns) rather than a real user prompt. Filtering
// these is essential: an unfiltered injected turn would read as a "new turn"
// every cycle and never converge.
func IsInjectedContext(text string) bool {
	t := strings.TrimSpace(text)
	for _, m := range []string{
		"<permissions instructions>",
		"<app-context>",
		"<collaboration_mode>",
		"<environment_context>",
		"<multi_agent_mode>",
		"<recommended_plugins>",
		"<skills_instructions>",
		"<apps_instructions>",
		"<plugins_instructions>",
		"<user_instructions>",
		"<INSTRUCTIONS>",
		"# AGENTS.md instructions",
		"# Project memory",
		"You are /root, the primary agent",
		"You are `/root`, the primary agent in a team of agents",
		"You are an agent in a team of agents collaborating",
	} {
		if strings.HasPrefix(t, m) {
			return true
		}
	}
	return false
}

// IsLocalCommandContext reports whether a user-role row is local command
// telemetry from an agent UI rather than a prompt the human asked to sync.
//
// Claude Code writes rows like "<command-name>/model</command-name>" and
// "<local-command-stdout>..." into session.jsonl for commands such as /model.
// If those rows are allowed into the text-turn comparator, cross-agent sync
// treats local UI state changes as conversations and can create command-only
// forks before the real prompt arrives.
func IsLocalCommandContext(text string) bool {
	t := strings.TrimSpace(text)
	for _, m := range []string{
		"<local-command-caveat>",
		"<local-command-stdout>",
		"<command-name>",
		"<command-message>",
		"<command-args>",
	} {
		if strings.HasPrefix(t, m) {
			return true
		}
	}
	return false
}

// IsHiddenUserContext reports whether a user-role row is adapter or harness
// scaffolding rather than a human-visible prompt.
func IsHiddenUserContext(text string) bool {
	return IsInjectedContext(text) || IsLocalCommandContext(text)
}

// NormalizeTextTurn trims a user/assistant turn into the stable text that is
// visible across agents. It returns ok=false when the row should not
// participate in cross-agent conversation sync.
func NormalizeTextTurn(role, text string) (string, bool) {
	text = strings.TrimSpace(text)
	if role == "user" {
		text = StripScheduledTaskPreamble(text)
		text = StripCodexAttachmentPreamble(text)
	}
	if text == "" {
		return "", false
	}
	if role == "user" && IsHiddenUserContext(text) {
		return "", false
	}
	return text, true
}

// StripCodexAttachmentPreamble removes Codex Desktop's rendered attachment
// inventory while preserving the actual prompt following "My request for
// Codex". The inventory is UI context, not conversation text, and otherwise
// becomes the repeated subject in target agents that title from turn one.
func StripCodexAttachmentPreamble(text string) string {
	if !strings.HasPrefix(text, "# Files mentioned by the user:") {
		return text
	}
	for _, marker := range []string{"\n## My request for Codex:\n", "\n# My request for Codex:\n"} {
		if idx := strings.Index(text, marker); idx >= 0 {
			return strings.TrimSpace(text[idx+len(marker):])
		}
	}
	return text
}

// scheduledPreambleRe matches the bracketed harness preamble Claude Code
// prepends to scheduled/cron task prompts:
//
//	[IMPORTANT: You are running as a scheduled cron job. … nothing more.]
//
//	<the actual task prompt>
//
// Non-greedy to the first "]" that ends a line followed by a blank line —
// the block's BODY can itself contain brackets (the real preamble documents
// a literal "[SILENT]" sentinel), so stopping at the first "]" would leave
// half the boilerplate behind. The trailing-only variant (whole message is
// the block) is handled by the $ alternative.
var scheduledPreambleRe = regexp.MustCompile(`(?s)^\[IMPORTANT: You are running as a scheduled.*?\](\s*\n\s*\n|\s*$)`)

// StripScheduledTaskPreamble removes the leading scheduled-task harness
// preamble from a user turn, keeping the real task prompt that follows.
// Without this, every synced scheduled session's title, key slug, and
// first-message preview reads "[IMPORTANT: You are running as a sc…" in the
// agent's session list. Returns
// the input unchanged when no preamble is present; returns "" when the whole
// message is preamble (the caller drops empty turns).
func StripScheduledTaskPreamble(text string) string {
	if !strings.HasPrefix(text, "[IMPORTANT: You are running as a scheduled") {
		return text
	}
	return strings.TrimSpace(scheduledPreambleRe.ReplaceAllString(text, ""))
}

func joinTextBlocks(blocks []ContentBlock) string {
	var parts []string
	for _, c := range blocks {
		if c.Type == "text" && c.Text != "" {
			parts = append(parts, c.Text)
		}
	}
	return strings.Join(parts, "\n\n")
}

// ConversationFormatHermesBundle is the payload format hermes-native
// conversations carry (a fidelity-preserving SessionBundle JSON). Defined
// here so cross-agent materializers can recognize it without importing the
// hermes adapter; hermes' own SessionBundleFormat aliases this constant.
const ConversationFormatHermesBundle = "acf.hermes.session.v1"

// hermesBundleEnvelope mirrors just enough of hermesdb.SessionBundle to
// extract the user/assistant thread; everything else in the bundle is
// hermes-private fidelity data the cross-agent view doesn't need.
type hermesBundleEnvelope struct {
	Messages []struct {
		Role    string  `json:"role"`
		Content *string `json:"content"`
	} `json:"messages"`
}

// TurnsFromHermesBundleJSON reduces a hermes SessionBundle payload to its
// ordered user/assistant text turns — the same reduction ExtractTextTurns
// performs for canonical events. Without this, hermes-native conversations
// silently never materialized into other agents' session lists (claude
// /resume showed Codex and Kilo threads but no Hermes ones). Returns nil on
// malformed JSON; the caller treats that as "nothing to materialize".
func TurnsFromHermesBundleJSON(content string) []TextTurn {
	turns, ok := turnsFromHermesBundleJSON(content)
	if !ok {
		return nil
	}
	return turns
}

func turnsFromHermesBundleJSON(content string) ([]TextTurn, bool) {
	var env hermesBundleEnvelope
	if err := json.Unmarshal([]byte(content), &env); err != nil {
		return nil, false
	}
	var out []TextTurn
	for _, m := range env.Messages {
		if m.Role != "user" && m.Role != "assistant" {
			continue
		}
		if m.Content == nil {
			continue
		}
		text, ok := NormalizeTextTurn(m.Role, *m.Content)
		if !ok {
			continue
		}
		out = append(out, TextTurn{Role: m.Role, Text: text})
	}
	return out, true
}

// ConversationTextTurns reduces any supported conversation payload shape to
// the stable user/assistant turn sequence used for merge and conflict
// equivalence. The bool reports whether the payload format is understood; an
// understood payload may still legitimately contain zero visible turns.
func ConversationTextTurns(p ConversationPayload) ([]TextTurn, bool) {
	switch p.Format {
	case ConversationFormatV1, ConversationDeltaFormatV1:
		return ExtractTextTurns(p.Events), true
	case ConversationFormatHermesBundle:
		return turnsFromHermesBundleJSON(p.Content)
	default:
		return nil, false
	}
}
