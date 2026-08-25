package hermes

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/hermesdb"
)

// herToolCall mirrors the shape OpenAI's chat-completions / Anthropic /
// Hermes-compatible APIs serialize for tool_calls. Hermes stores this
// verbatim in messages.tool_calls as a JSON string.
type herToolCall struct {
	ID       string          `json:"id"`
	Type     string          `json:"type,omitempty"` // usually "function"
	Function herToolFunction `json:"function"`
}

type herToolFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON-encoded args, per OpenAI convention
}

// contentJSONPrefix mirrors Hermes's _CONTENT_JSON_PREFIX (hermes_state.py):
// multimodal message content (a list of parts) is stored as this NUL sentinel
// followed by JSON. Plain text content has no prefix.
const contentJSONPrefix = "\x00json:"

// hermesNativeExtras carries per-message fidelity that has no first-class home
// in the canonical event model but must survive a hermes→canonical→hermes
// round trip. Stored opaquely in ConversationEvent.NativeExtras.
//
//   - RawContent: the verbatim (sentinel-encoded) content string, so multimodal
//     messages re-export byte-for-byte (the canonical Content blocks are a
//     readable projection for cross-agent consumers; RawContent is the source
//     of truth for same-agent export).
//   - reasoning fields: Hermes-specific reasoning traces, dropped by the
//     canonical model but preserved here for same-agent fidelity.
//   - ToolName: the messages.tool_name column. The canonical model only carries
//     a tool name on tool_call events; tool-role results (and any other row that
//     populated tool_name) would otherwise lose it, so it rides here.
//   - RawToolCalls: the verbatim messages.tool_calls string, stamped ONLY when
//     it fails to parse into []herToolCall. Well-formed tool_calls expand into
//     canonical tool_call events; an unparseable string can't, so rather than
//     silently dropping it (lossless-replication violation) it rides here and
//     is restored on decode.
type hermesNativeExtras struct {
	RawContent          *string `json:"raw_content,omitempty"`
	Reasoning           *string `json:"reasoning,omitempty"`
	ReasoningContent    *string `json:"reasoning_content,omitempty"`
	ReasoningDetails    *string `json:"reasoning_details,omitempty"`
	CodexReasoningItems *string `json:"codex_reasoning_items,omitempty"`
	CodexMessageItems   *string `json:"codex_message_items,omitempty"`
	ToolName            *string `json:"tool_name,omitempty"`
	RawToolCalls        *string `json:"raw_tool_calls,omitempty"`
}

// messageNativeExtras builds the NativeExtras payload for a message, or nil if
// there is nothing to preserve beyond the canonical projection.
func messageNativeExtras(m hermesdb.MessageRow) json.RawMessage {
	ex := hermesNativeExtras{
		Reasoning:           m.Reasoning,
		ReasoningContent:    m.ReasoningContent,
		ReasoningDetails:    m.ReasoningDetails,
		CodexReasoningItems: m.CodexReasoningItems,
		CodexMessageItems:   m.CodexMessageItems,
		ToolName:            m.ToolName,
	}
	if m.Content != nil && strings.HasPrefix(*m.Content, contentJSONPrefix) {
		ex.RawContent = m.Content
	}
	// Preserve unparseable tool_calls verbatim: well-formed ones expand into
	// canonical tool_call events (and must NOT be duplicated here), but a
	// non-empty string that fails to parse has no canonical home and would
	// otherwise be silently dropped, violating the lossless-replication
	// contract.
	if m.ToolCalls != nil && *m.ToolCalls != "" {
		if _, ok := parseHermesToolCalls(m.ToolCalls); !ok {
			ex.RawToolCalls = m.ToolCalls
		}
	}
	if ex.RawContent == nil && ex.Reasoning == nil && ex.ReasoningContent == nil &&
		ex.ReasoningDetails == nil && ex.CodexReasoningItems == nil && ex.CodexMessageItems == nil &&
		ex.ToolName == nil && ex.RawToolCalls == nil {
		return nil
	}
	b, err := json.Marshal(ex)
	if err != nil {
		return nil
	}
	return b
}

// parseHermesToolCalls decodes a messages.tool_calls string into typed calls.
// Returns ok=false when tc is nil/empty OR fails to unmarshal — the single
// place that decides "this tool_calls string can become canonical events." A
// nil/empty string is not a fidelity loss (nothing to preserve), so ok=false
// with calls=nil signals "no events" without requesting raw preservation; the
// caller distinguishes the two via the original pointer.
func parseHermesToolCalls(tc *string) ([]herToolCall, bool) {
	if tc == nil || *tc == "" {
		return nil, false
	}
	var calls []herToolCall
	if err := json.Unmarshal([]byte(*tc), &calls); err != nil {
		return nil, false
	}
	return calls, true
}

// applyHermesExtras restores RawContent + reasoning fields onto a decoded
// MessageRow. RawContent (when present) overrides the text-projected Content so
// multimodal messages round-trip exactly.
func applyHermesExtras(mr *hermesdb.MessageRow, raw json.RawMessage) {
	if len(raw) == 0 {
		return
	}
	var ex hermesNativeExtras
	if err := json.Unmarshal(raw, &ex); err != nil {
		return
	}
	if ex.RawContent != nil {
		mr.Content = ex.RawContent
	}
	if ex.Reasoning != nil {
		mr.Reasoning = ex.Reasoning
	}
	if ex.ReasoningContent != nil {
		mr.ReasoningContent = ex.ReasoningContent
	}
	if ex.ReasoningDetails != nil {
		mr.ReasoningDetails = ex.ReasoningDetails
	}
	if ex.CodexReasoningItems != nil {
		mr.CodexReasoningItems = ex.CodexReasoningItems
	}
	if ex.CodexMessageItems != nil {
		mr.CodexMessageItems = ex.CodexMessageItems
	}
	if ex.ToolName != nil {
		mr.ToolName = ex.ToolName
	}
	if ex.RawToolCalls != nil {
		mr.ToolCalls = ex.RawToolCalls
	}
}

// decodeHermesContent converts a stored message content string into canonical
// ContentBlocks. Plain text yields a single text block. Sentinel-prefixed
// multimodal content (a JSON parts list) is decoded into typed blocks (text +
// image), mirroring Hermes's _decode_content.
func decodeHermesContent(s string) []acf.ContentBlock {
	if !strings.HasPrefix(s, contentJSONPrefix) {
		return []acf.ContentBlock{{Type: "text", Text: s}}
	}
	payload := s[len(contentJSONPrefix):]
	var parts []map[string]json.RawMessage
	if err := json.Unmarshal([]byte(payload), &parts); err != nil {
		// Not a parts array — surface the decoded JSON as text (still better
		// than leaking the raw NUL-prefixed sentinel string).
		return []acf.ContentBlock{{Type: "text", Text: payload}}
	}
	var out []acf.ContentBlock
	for _, p := range parts {
		out = append(out, hermesPartToBlocks(p)...)
	}
	if len(out) == 0 {
		out = []acf.ContentBlock{{Type: "text", Text: payload}}
	}
	return out
}

// hermesPartToBlocks maps one multimodal content part to canonical blocks.
// Handles OpenAI-style ({"type":"text"|"image_url"}) and Anthropic-style
// ({"type":"image","source":{...}}) parts; unknown part types are skipped.
func hermesPartToBlocks(p map[string]json.RawMessage) []acf.ContentBlock {
	var typ string
	_ = json.Unmarshal(p["type"], &typ)
	switch typ {
	case "text":
		var text string
		_ = json.Unmarshal(p["text"], &text)
		return []acf.ContentBlock{{Type: "text", Text: text}}
	case "image_url":
		// image_url is either a bare string or {"url": "..."}.
		var url string
		if json.Unmarshal(p["image_url"], &url) != nil {
			var obj struct {
				URL string `json:"url"`
			}
			_ = json.Unmarshal(p["image_url"], &obj)
			url = obj.URL
		}
		return []acf.ContentBlock{{Type: "image", Data: url}}
	case "image":
		var src struct {
			URL  string `json:"url"`
			Data string `json:"data"`
		}
		_ = json.Unmarshal(p["source"], &src)
		ref := src.URL
		if ref == "" {
			ref = src.Data
		}
		return []acf.ContentBlock{{Type: "image", Data: ref}}
	}
	return nil
}

// EncodeBundleAsCanonical converts a Hermes SessionBundle into canonical
// conversation events. Lossy on session-level metadata (title, model,
// billing fields) — those don't have a canonical home. Messages are
// translated row-by-row in their natural order.
func EncodeBundleAsCanonical(b hermesdb.SessionBundle) []acf.ConversationEvent {
	var events []acf.ConversationEvent
	for _, m := range b.Messages {
		ts := unixToTime(m.Timestamp)
		text := ""
		if m.Content != nil {
			text = *m.Content
		}
		extras := messageNativeExtras(m)
		switch m.Role {
		case "user":
			if m.ToolCallID != nil && *m.ToolCallID != "" {
				// user-role messages with tool_call_id are some flows'
				// representation of tool results.
				events = append(events, acf.ConversationEvent{
					Type:         acf.EventTypeToolResult,
					Timestamp:    ts,
					CallID:       *m.ToolCallID,
					Content:      decodeHermesContent(text),
					NativeExtras: extras,
				})
				continue
			}
			if text == "" {
				continue
			}
			visible, ok := acf.NormalizeTextTurn("user", text)
			if !ok {
				continue
			}
			events = append(events, acf.ConversationEvent{
				Type:         acf.EventTypeTurn,
				Timestamp:    ts,
				Role:         "user",
				Content:      decodeHermesContent(visible),
				NativeExtras: extras,
			})
		case "assistant":
			// Emit a turn when there is content OR reasoning to carry (a
			// reasoning-only assistant message must still surface so its
			// reasoning round-trips via NativeExtras).
			if text != "" || extras != nil {
				var content []acf.ContentBlock
				if text != "" {
					content = decodeHermesContent(text)
				}
				events = append(events, acf.ConversationEvent{
					Type:         acf.EventTypeTurn,
					Timestamp:    ts,
					Role:         "assistant",
					Content:      content,
					NativeExtras: extras,
				})
			}
			// Expand tool_calls AFTER the text turn — temporal "text first,
			// then call" order matches both claudecode/codex translators.
			// A non-empty string that fails to parse is NOT dropped here: it
			// rode along verbatim in the turn's NativeExtras above (see
			// messageNativeExtras), so it round-trips instead of vanishing.
			if calls, ok := parseHermesToolCalls(m.ToolCalls); ok {
				for _, c := range calls {
					events = append(events, acf.ConversationEvent{
						Type:      acf.EventTypeToolCall,
						Timestamp: ts,
						CallID:    c.ID,
						ToolName:  c.Function.Name,
						Input:     json.RawMessage(c.Function.Arguments),
					})
				}
			}
		case "tool":
			callID := ""
			if m.ToolCallID != nil {
				callID = *m.ToolCallID
			}
			events = append(events, acf.ConversationEvent{
				Type:         acf.EventTypeToolResult,
				Timestamp:    ts,
				CallID:       callID,
				Content:      decodeHermesContent(text),
				NativeExtras: extras,
			})
		case "system":
			if text == "" {
				continue
			}
			events = append(events, acf.ConversationEvent{
				Type:         acf.EventTypeSystemNote,
				Timestamp:    ts,
				Content:      decodeHermesContent(text),
				NativeExtras: extras,
			})
		}
	}
	return events
}

// DecodeBundleFromCanonical converts canonical conversation events back into
// a Hermes SessionBundle. sessionID is supplied by the caller; StartedAt is
// the first event's timestamp.
//
// Session metadata is populated so hermes' own session list renders the
// synced conversation recognizably (without it, /resume showed "—" rows
// with message_count=0 — users couldn't tell their cross-agent sessions
// arrived at all): Title = "↪ <Origin>: <first user message>" (mirrors the
// claude-side transcode convention; system preamble skipped), MessageCount,
// and EndedAt = the last message's timestamp. originAgent may be "" when
// provenance is unknown; the title then omits the prefix.
func DecodeBundleFromCanonical(sessionID, originAgent string, events []acf.ConversationEvent) hermesdb.SessionBundle {
	var msgs []hermesdb.MessageRow
	var startedAt float64
	if len(events) > 0 && !events[0].Timestamp.IsZero() {
		startedAt = float64(events[0].Timestamp.Unix()) + float64(events[0].Timestamp.Nanosecond())/1e9
	}

	for _, e := range events {
		ts := float64(0)
		if !e.Timestamp.IsZero() {
			ts = float64(e.Timestamp.Unix()) + float64(e.Timestamp.Nanosecond())/1e9
		}
		switch e.Type {
		case acf.EventTypeTurn:
			role := e.Role
			if role != "user" && role != "assistant" && role != "system" {
				role = "user"
			}
			content := joinTexts(e.Content)
			if role == "user" {
				visible, ok := acf.NormalizeTextTurn(role, content)
				if !ok {
					continue
				}
				content = visible
			}
			mr := hermesdb.MessageRow{
				Role:      role,
				Content:   &content,
				Timestamp: ts,
			}
			applyHermesExtras(&mr, e.NativeExtras)
			msgs = append(msgs, mr)
		case acf.EventTypeToolCall:
			// Emit a separate assistant message with tool_calls JSON populated.
			// Not merging into the prior turn keeps decode logic O(N) and
			// self-contained; round-trip via Encode is semantically stable
			// either way (the empty-content assistant row is dropped on
			// re-encode, the tool_calls produce tool_call events).
			tc := []herToolCall{{
				ID:       e.CallID,
				Type:     "function",
				Function: herToolFunction{Name: e.ToolName, Arguments: string(e.Input)},
			}}
			tcJSON, _ := json.Marshal(tc)
			tcStr := string(tcJSON)
			empty := ""
			toolName := e.ToolName
			msgs = append(msgs, hermesdb.MessageRow{
				Role:      "assistant",
				Content:   &empty,
				ToolCalls: &tcStr,
				ToolName:  &toolName,
				Timestamp: ts,
			})
		case acf.EventTypeToolResult:
			callID := e.CallID
			content := joinTexts(e.Content)
			mr := hermesdb.MessageRow{
				Role:       "tool",
				Content:    &content,
				ToolCallID: &callID,
				Timestamp:  ts,
			}
			applyHermesExtras(&mr, e.NativeExtras)
			msgs = append(msgs, mr)
		case acf.EventTypeSystemNote:
			content := joinTexts(e.Content)
			mr := hermesdb.MessageRow{
				Role:      "system",
				Content:   &content,
				Timestamp: ts,
			}
			applyHermesExtras(&mr, e.NativeExtras)
			msgs = append(msgs, mr)
		}
	}

	source := canonicalImportSource
	row := hermesdb.SessionRow{
		ID:           sessionID,
		Source:       source,
		StartedAt:    startedAt,
		MessageCount: int64(len(msgs)),
	}
	if title := syncedSessionTitle(originAgent, msgs); title != "" {
		row.Title = &title
	}
	if len(msgs) > 0 {
		if last := msgs[len(msgs)-1].Timestamp; last > 0 {
			ended := last
			row.EndedAt = &ended
		}
	}
	return hermesdb.SessionBundle{
		Session:  row,
		Messages: msgs,
	}
}

// DecodePortableBundleFromCanonical projects a foreign coding-agent transcript
// onto the only messages portable across agents: visible user and assistant
// text turns. It deliberately excludes system/developer harness content,
// reasoning, tool invocations, and tool results from Hermes's chat UI.
func DecodePortableBundleFromCanonical(sessionID, originAgent string, events []acf.ConversationEvent) hermesdb.SessionBundle {
	portable := make([]acf.ConversationEvent, 0, len(events))
	for _, event := range events {
		if event.Type != acf.EventTypeTurn || (event.Role != "user" && event.Role != "assistant") {
			continue
		}
		text, ok := acf.NormalizeTextTurn(event.Role, joinTexts(event.Content))
		if !ok {
			continue
		}
		portable = append(portable, acf.ConversationEvent{
			Type:      acf.EventTypeTurn,
			Timestamp: event.Timestamp,
			Role:      event.Role,
			Content:   []acf.ContentBlock{{Type: "text", Text: text}},
		})
	}
	return DecodeBundleFromCanonical(sessionID, originAgent, portable)
}

// syncedSessionTitle derives a human-recognizable title for a synced
// session: "↪ <Origin>: <first user message>" — the same convention the
// claude-code transcode stamps on sessions it materializes. Returns "" when
// there is no usable human-visible user text.
func syncedSessionTitle(originAgent string, msgs []hermesdb.MessageRow) string {
	// Prefer the first HUMAN-looking user message. Coding-agent
	// transcripts (claude-code, codex) carry harness meta as user-role
	// turns — "<permissions instructions>…", "<command-name>/clear…" —
	// so hidden content must never become the session title.
	base := ""
	for _, m := range msgs {
		if m.Role != "user" || m.Content == nil {
			continue
		}
		// Same filters the cross-agent turn extraction applies: harness
		// preambles ride along as user-role messages (tag-prefixed meta,
		// "# AGENTS.md instructions…", "# Project memory") and must not
		// become the session title — and scheduled-task boilerplate
		// ("[IMPORTANT: You are running as a scheduled…") is stripped so
		// the title shows the real task prompt behind it.
		c, ok := acf.NormalizeTextTurn("user", *m.Content)
		if !ok {
			continue
		}
		base = c
		break
	}
	if base == "" {
		return ""
	}
	base = strings.TrimSpace(strings.ReplaceAll(base, "\n", " "))
	r := []rune(base)
	const titleMax = 56
	if len(r) > titleMax {
		base = string(r[:titleMax]) + "…"
	}
	prefix := "↪ "
	if originAgent != "" {
		prefix += strings.ToUpper(originAgent[:1]) + originAgent[1:] + ": "
	}
	return prefix + base
}

func joinTexts(blocks []acf.ContentBlock) string {
	var b strings.Builder
	for _, blk := range blocks {
		if blk.Type == "text" {
			b.WriteString(blk.Text)
		}
	}
	return b.String()
}

func unixToTime(seconds float64) time.Time {
	if seconds == 0 {
		return time.Time{}
	}
	sec := int64(seconds)
	nsec := int64((seconds - float64(sec)) * 1e9)
	return time.Unix(sec, nsec).UTC()
}
