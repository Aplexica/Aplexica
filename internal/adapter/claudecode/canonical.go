// Package claudecode — canonical conversation translator (V0.15.0).
//
// EncodeCanonical converts a Claude Code session.jsonl byte stream into a
// list of canonical acf.ConversationEvent values. DecodeCanonical does the
// reverse. Both are LOSSY but stable:
//
//   - Preserved: user turns (text + image blocks), assistant turns (text +
//     thinking blocks), tool_use blocks, user tool_result blocks (their
//     `content` payload, string or array), system notes. Image data and
//     thinking text are carried in ContentBlock.Data / .Text respectively.
//   - Dropped on encode: queue-operation, last-prompt, attachment, and any
//     unknown row types (Claude Code's session format evolves; these are
//     considered metadata that does not round-trip).
//   - Emitted on decode: only the four event kinds above. redaction /
//     amendment events (added by the canonical layer post-import) are skipped.
//
// Round-trip is semantically stable, not byte-identical: an assistant row
// with both text + tool_use blocks comes back from EncodeCanonical as
// (turn, tool_call), which DecodeCanonical re-emits as two assistant rows
// (one text-only, one tool_use). Re-encoding those produces the SAME
// canonical events — the fixed point is what matters.
//
// Real Claude Code rows wrap their content under "message":{...}; we accept
// both that shape and the simpler top-level shape used by the test fixture.
package claudecode

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
)

// rawClaudeRow models one line of Claude Code's session.jsonl. Fields are
// best-effort — Claude Code's format evolves. Unknown fields are dropped on
// import; we never claim a lossless round-trip for non-turn metadata.
type rawClaudeRow struct {
	Type             string          `json:"type"`
	Timestamp        string          `json:"timestamp,omitempty"`
	Role             string          `json:"role,omitempty"`
	Content          json.RawMessage `json:"content,omitempty"` // either string or array of blocks
	Message          *rawClaudeMsg   `json:"message,omitempty"` // real Claude Code wraps content here
	Operation        string          `json:"operation,omitempty"`
	SessionID        string          `json:"sessionId,omitempty"`
	UUID             string          `json:"uuid,omitempty"`
	CWD              string          `json:"cwd,omitempty"`
	AplexicaThreadID string          `json:"aplexicaThreadId,omitempty"`
}

// claudeCanonicalState is both the parser accumulator and the small metadata
// projection cached for source-compatibility checks. It deliberately contains
// no raw prompt/answer bytes beyond the canonical events the adapter already
// imports.
type claudeCanonicalState struct {
	events                 []acf.ConversationEvent
	sessionID              string
	lastCWD                string
	hasExplicitThreadStamp bool
}

// rawClaudeMsg is the inner message object on real Claude Code user/assistant
// rows. The test fixture uses the top-level Content shape so this is optional.
type rawClaudeMsg struct {
	Role    string          `json:"role,omitempty"`
	Content json.RawMessage `json:"content,omitempty"`
	Model   string          `json:"model,omitempty"`
}

// rawClaudeBlock models one element of the content[] array on an
// assistant/user row with structured content.
type rawClaudeBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	Thinking  string          `json:"thinking,omitempty"`    // thinking
	ID        string          `json:"id,omitempty"`          // tool_use
	Name      string          `json:"name,omitempty"`        // tool_use
	Input     json.RawMessage `json:"input,omitempty"`       // tool_use
	ToolUseID string          `json:"tool_use_id,omitempty"` // tool_result
	IsError   bool            `json:"is_error,omitempty"`    // tool_result
	Content   json.RawMessage `json:"content,omitempty"`     // tool_result: string OR array of blocks
	Source    json.RawMessage `json:"source,omitempty"`      // image: {type,media_type,data} or {type,url}
}

// EncodeCanonical parses a Claude Code session.jsonl byte stream into
// canonical conversation events. Lossy: drops queue-operation, last-prompt,
// attachment rows. Translates user/assistant/system rows into turn /
// tool_call / tool_result / system_note as appropriate.
func EncodeCanonical(jsonl []byte) ([]acf.ConversationEvent, error) {
	state, _ := encodeCanonicalInto(jsonl, 0, claudeCanonicalState{})
	return state.events, nil
}

// encodeCanonicalFrom decodes the rows in jsonl starting at byte offset start
// and returns the events plus the absolute byte offset just past the last
// FULLY-decoded row — the resume point for the next incremental parse.
//
// Ordinary session rows are independent, so a caller can concatenate the
// result for a freshly-appended tail with its cached prefix.
// A partial trailing row (mid-write, not yet a complete JSON object) is left
// unconsumed: resume stays at the end of the last complete row so the partial
// row is re-attempted once the rest is flushed.
func encodeCanonicalFrom(jsonl []byte, start int64) (events []acf.ConversationEvent, resume int64) {
	state, resume := encodeCanonicalInto(jsonl, start, claudeCanonicalState{})
	return state.events, resume
}

// encodeCanonicalInto incrementally parses complete rows into state while also
// retaining the content-free native metadata needed by the source planner.
func encodeCanonicalInto(jsonl []byte, start int64, state claudeCanonicalState) (claudeCanonicalState, int64) {
	if start < 0 || start > int64(len(jsonl)) {
		start = 0
		state = claudeCanonicalState{}
	}
	dec := json.NewDecoder(bytes.NewReader(jsonl[start:]))
	var consumed int64
	for {
		// session.jsonl uses one object per line, but json.Decoder will
		// happily read whitespace-separated objects too.
		var row rawClaudeRow
		if err := dec.Decode(&row); err != nil {
			// EOF or a partial trailing object: stop at the last complete row.
			break
		}
		// InputOffset (within jsonl[start:]) marks the end of the row just
		// decoded — i.e. the resume point should the stream end here.
		consumed = dec.InputOffset()
		if state.sessionID == "" && row.SessionID != "" {
			state.sessionID = row.SessionID
		}
		if row.CWD != "" {
			state.lastCWD = row.CWD
		}
		if row.AplexicaThreadID != "" {
			state.hasExplicitThreadStamp = true
		}
		ts := parseClaudeTime(row.Timestamp)
		// Real Claude Code wraps user/assistant content under .message. The
		// test fixture uses top-level .content. Normalize here so the rest
		// of the switch can read from row.Content uniformly.
		if row.Message != nil && len(row.Content) == 0 {
			row.Content = row.Message.Content
		}
		var rowEvents []acf.ConversationEvent
		switch row.Type {
		case "user":
			emitUserRow(&rowEvents, row, ts)
		case "assistant":
			// Claude Desktop can append a local bookkeeping reply with the
			// reserved <synthetic> model when it indexes an Aplexica-materialized
			// transcript whose newest turn is still a user prompt. It is not an
			// agent answer and must never become a competing canonical turn.
			if row.Message != nil && row.Message.Model == "<synthetic>" {
				continue
			}
			emitAssistantRow(&rowEvents, row, ts)
		case "system":
			emitSystemRow(&rowEvents, row, ts)
		default:
			// queue-operation, last-prompt, attachment, etc. — dropped.
			continue
		}
		state.events = append(state.events, rowEvents...)
	}
	return state, start + consumed
}

func cloneClaudeCanonicalState(state claudeCanonicalState) claudeCanonicalState {
	state.events = append([]acf.ConversationEvent(nil), state.events...)
	return state
}

func parseClaudeTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

func emitUserRow(events *[]acf.ConversationEvent, row rawClaudeRow, ts time.Time) {
	if len(row.Content) == 0 {
		return
	}
	// User content may be a string OR an array. Try string first.
	var asString string
	if err := json.Unmarshal(row.Content, &asString); err == nil {
		text, ok := acf.NormalizeTextTurn("user", asString)
		if !ok {
			return
		}
		*events = append(*events, acf.ConversationEvent{
			Type:      acf.EventTypeTurn,
			Timestamp: ts,
			Role:      "user",
			Content:   []acf.ContentBlock{{Type: "text", Text: text}},
		})
		return
	}
	// Array of blocks — text, image, and/or tool_result entries.
	var blocks []rawClaudeBlock
	if err := json.Unmarshal(row.Content, &blocks); err != nil {
		return
	}
	var turnBlocks []acf.ContentBlock
	for _, b := range blocks {
		switch b.Type {
		case "text":
			turnBlocks = append(turnBlocks, acf.ContentBlock{Type: "text", Text: b.Text})
		case "image":
			turnBlocks = append(turnBlocks, acf.ContentBlock{Type: "image", Data: claudeImageRef(b.Source)})
		case "tool_result":
			text := b.Text
			if text == "" {
				text = stringifyToolResultPayload(b)
			}
			*events = append(*events, acf.ConversationEvent{
				Type:      acf.EventTypeToolResult,
				Timestamp: ts,
				CallID:    b.ToolUseID,
				IsError:   b.IsError,
				Content:   []acf.ContentBlock{{Type: "text", Text: text}},
			})
		}
	}
	if len(turnBlocks) > 0 {
		var ok bool
		turnBlocks, ok = normalizeClaudeUserTurnBlocks(turnBlocks)
		if !ok {
			return
		}
		*events = append(*events, acf.ConversationEvent{
			Type:      acf.EventTypeTurn,
			Timestamp: ts,
			Role:      "user",
			Content:   turnBlocks,
		})
	}
}

func normalizeClaudeUserTurnBlocks(blocks []acf.ContentBlock) ([]acf.ContentBlock, bool) {
	textOnly := true
	var text bytes.Buffer
	for i, block := range blocks {
		if block.Type != "text" {
			textOnly = false
			break
		}
		if i > 0 {
			text.WriteString("\n\n")
		}
		text.WriteString(block.Text)
	}
	if !textOnly {
		return blocks, true
	}
	normalized, ok := acf.NormalizeTextTurn("user", text.String())
	if !ok {
		return nil, false
	}
	return []acf.ContentBlock{{Type: "text", Text: normalized}}, true
}

// stringifyToolResultPayload extracts text from a tool_result block's `content`
// field, which Claude Code serializes as either a JSON string or an array of
// content blocks (e.g. [{"type":"text","text":"..."}]). Text fragments are
// concatenated. Non-text content blocks (rare in tool_results) are skipped.
func stringifyToolResultPayload(b rawClaudeBlock) string {
	if len(b.Content) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(b.Content, &s); err == nil {
		return s
	}
	var blocks []rawClaudeBlock
	if err := json.Unmarshal(b.Content, &blocks); err == nil {
		var buf bytes.Buffer
		for _, sub := range blocks {
			if sub.Type == "text" {
				buf.WriteString(sub.Text)
			}
		}
		return buf.String()
	}
	return ""
}

// claudeImageRef extracts a stable reference from a Claude image block's
// `source` object: a URL when present, else the base64 `data`, else the raw
// source JSON. Stored in ContentBlock.Data per the ACF non-text-block contract.
func claudeImageRef(source json.RawMessage) string {
	if len(source) == 0 {
		return ""
	}
	var s struct {
		URL  string `json:"url,omitempty"`
		Data string `json:"data,omitempty"`
	}
	if err := json.Unmarshal(source, &s); err == nil {
		if s.URL != "" {
			return s.URL
		}
		if s.Data != "" {
			return s.Data
		}
	}
	return string(source)
}

func emitAssistantRow(events *[]acf.ConversationEvent, row rawClaudeRow, ts time.Time) {
	if len(row.Content) == 0 {
		return
	}
	// Assistant content is usually an array (text + tool_use blocks); also
	// supports the legacy string form for simple text.
	var asString string
	if err := json.Unmarshal(row.Content, &asString); err == nil {
		*events = append(*events, acf.ConversationEvent{
			Type:      acf.EventTypeTurn,
			Timestamp: ts,
			Role:      "assistant",
			Content:   []acf.ContentBlock{{Type: "text", Text: asString}},
		})
		return
	}
	var blocks []rawClaudeBlock
	if err := json.Unmarshal(row.Content, &blocks); err != nil {
		return
	}
	var turnBlocks []acf.ContentBlock
	for _, b := range blocks {
		switch b.Type {
		case "text":
			turnBlocks = append(turnBlocks, acf.ContentBlock{Type: "text", Text: b.Text})
		case "thinking":
			// Preserve extended-thinking content as a typed block. Previously
			// dropped, which erased thinking-only assistant turns entirely.
			// The cryptographic `signature` is not carried (not content).
			//
			// EMPTY thinking is skipped: Claude Code >=2.1.204 splits one API
			// message across JSONL records (same message.id) and writes an
			// interleaved-thinking record whose thinking text is "" (signature
			// only). With the signature not carried, that block holds zero
			// content - encoding it created a phantom empty assistant turn in
			// the canonical thread, materialized on every synced device.
			if b.Thinking == "" {
				continue
			}
			turnBlocks = append(turnBlocks, acf.ContentBlock{Type: "thinking", Text: b.Thinking})
		}
	}
	if len(turnBlocks) > 0 {
		*events = append(*events, acf.ConversationEvent{
			Type:      acf.EventTypeTurn,
			Timestamp: ts,
			Role:      "assistant",
			Content:   turnBlocks,
		})
	}
	// Emit tool_call events AFTER the turn so the temporal order is
	// preserved (the model said text first, THEN issued tool calls).
	for _, b := range blocks {
		if b.Type == "tool_use" {
			*events = append(*events, acf.ConversationEvent{
				Type:      acf.EventTypeToolCall,
				Timestamp: ts,
				CallID:    b.ID,
				ToolName:  b.Name,
				Input:     b.Input,
			})
		}
	}
}

func emitSystemRow(events *[]acf.ConversationEvent, row rawClaudeRow, ts time.Time) {
	// Real Claude Code "system" rows are metadata (subtype:stop_hook_summary
	// etc.) with no top-level string content; the test fixture's simpler
	// shape uses a string. Only emit a system_note for the string case.
	if len(row.Content) == 0 {
		return
	}
	var asString string
	if err := json.Unmarshal(row.Content, &asString); err != nil {
		return
	}
	if acf.IsLocalCommandContext(asString) {
		return
	}
	*events = append(*events, acf.ConversationEvent{
		Type:      acf.EventTypeSystemNote,
		Timestamp: ts,
		Content:   []acf.ContentBlock{{Type: "text", Text: asString}},
	})
}

// DecodeCanonical emits a Claude Code session.jsonl byte stream from the
// canonical events. Lossy but stable: produces user/assistant/system rows.
// Each output line is a valid JSON object terminated by "\n".
func DecodeCanonical(events []acf.ConversationEvent) ([]byte, error) {
	var out bytes.Buffer
	for _, e := range events {
		var row map[string]any
		switch e.Type {
		case acf.EventTypeTurn:
			row = map[string]any{
				"type":      e.Role, // "user" or "assistant"
				"timestamp": fmtTime(e.Timestamp),
				"content":   turnContent(e.Content),
			}
		case acf.EventTypeToolCall:
			// Attach to the most recent assistant turn? For simplicity in
			// v0.15.0 we emit a standalone assistant row with a single
			// tool_use block. Round-trip through EncodeCanonical produces
			// the same canonical events — semantically equivalent even if
			// not byte-identical to the original JSONL.
			toolUse := map[string]any{
				"type": "tool_use",
				"id":   e.CallID,
				"name": e.ToolName,
			}
			if len(e.Input) > 0 {
				toolUse["input"] = json.RawMessage(e.Input)
			}
			row = map[string]any{
				"type":      "assistant",
				"timestamp": fmtTime(e.Timestamp),
				"content":   []map[string]any{toolUse},
			}
		case acf.EventTypeToolResult:
			row = map[string]any{
				"type":      "user",
				"timestamp": fmtTime(e.Timestamp),
				"content": []map[string]any{{
					"type":        "tool_result",
					"tool_use_id": e.CallID,
					"content":     contentToString(e.Content),
					"is_error":    e.IsError,
				}},
			}
		case acf.EventTypeSystemNote:
			row = map[string]any{
				"type":      "system",
				"timestamp": fmtTime(e.Timestamp),
				"content":   contentToString(e.Content),
			}
		default:
			continue // redaction / amendment / unknown → skip
		}
		b, err := json.Marshal(row)
		if err != nil {
			return nil, fmt.Errorf("claudecode: marshal canonical row: %w", err)
		}
		out.Write(b)
		out.WriteByte('\n')
	}
	return out.Bytes(), nil
}

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func contentToString(blocks []acf.ContentBlock) string {
	for _, b := range blocks {
		if b.Type == "text" {
			return b.Text
		}
	}
	return ""
}

// turnContent renders a turn's content blocks back into Claude Code's content
// field. A turn carrying a single text block decodes to a bare string (the
// legacy/simple shape EncodeCanonical also accepts), keeping the common case
// compact. Otherwise it emits the structured block array so EVERY block —
// text, thinking, and image — survives the round trip. Emitting only the
// first text block here (the old contentToString behaviour) silently erased
// thinking-only and image-only turns, breaking the documented fixed point.
func turnContent(blocks []acf.ContentBlock) any {
	if len(blocks) == 1 && blocks[0].Type == "text" {
		return blocks[0].Text
	}
	out := make([]map[string]any, 0, len(blocks))
	for _, b := range blocks {
		switch b.Type {
		case "thinking":
			out = append(out, map[string]any{"type": "thinking", "thinking": b.Text})
		case "image":
			// Carry the stable reference back under source.data; EncodeCanonical's
			// claudeImageRef recovers it (URL-less source → data) on re-import.
			out = append(out, map[string]any{
				"type":   "image",
				"source": map[string]any{"data": b.Data},
			})
		default:
			// text and any other block fall back to a text block so nothing is
			// dropped; unknown block types degrade to their carried Text.
			out = append(out, map[string]any{"type": "text", "text": b.Text})
		}
	}
	return out
}
