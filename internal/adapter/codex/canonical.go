// Package codex — canonical conversation translator (V0.16.0).
//
// EncodeCanonical converts a Codex session.jsonl byte stream into a list of
// canonical acf.ConversationEvent values. DecodeCanonical does the reverse.
// Both are LOSSY but stable, mirroring the V0.15.0 claudecode translator:
//
//   - Preserved: response_item rows of types message (user/final assistant,
//     including input_image content blocks), function_call,
//     custom_tool_call (its `input` patch/command string), web_search_call,
//     function_call_output, custom_tool_call_output.
//   - Dropped on encode: session_meta, turn_context, event_msg, response_item/
//     reasoning (encrypted_content is not portable), and any unknown wrapper
//     types. Codex's wrapper-level metadata does not round-trip.
//   - `compacted` rows are intentionally dropped: the rollout is append-only,
//     so the pre-compaction turns are already present as earlier response_item
//     rows in the SAME file; the compaction summary in replacement_history is
//     not re-emitted (doing so would duplicate/condense already-captured turns).
//   - Emitted on decode: response_item rows for the four canonical event
//     kinds. redaction / amendment events (added by the canonical layer post-
//     import) are skipped.
//
// Developer/system messages are Codex execution policy rather than conversation
// turns and are never imported. Re-encoding a decoded stream produces the SAME
// canonical events — the fixed point is what matters.
//
// Each Codex row wraps its payload under {"timestamp","type","payload"}.
// Only payloads under type=="response_item" carry conversation content.
package codex

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
)

// rawCodexRow models the outer wrapper of each Codex session.jsonl line.
type rawCodexRow struct {
	Timestamp string          `json:"timestamp,omitempty"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

// rawCodexPayload models the inner payload shapes we care about. Unused
// fields are dropped silently — codex's payload schema evolves.
type rawCodexPayload struct {
	Type      string          `json:"type"`
	Role      string          `json:"role,omitempty"`
	Phase     string          `json:"phase,omitempty"`
	Content   []rawCodexBlock `json:"content,omitempty"`
	Name      string          `json:"name,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"` // function_call: JSON-encoded args
	Input     json.RawMessage `json:"input,omitempty"`     // custom_tool_call: a JSON string (e.g. apply_patch text)
	Action    json.RawMessage `json:"action,omitempty"`    // web_search_call: {type,queries,...}
	CallID    string          `json:"call_id,omitempty"`
	Output    json.RawMessage `json:"output,omitempty"` // function_call_output: string or object
}

type rawCodexBlock struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"` // input_image: data: URI or https URL
}

// EncodeCanonical parses Codex session.jsonl bytes into canonical events.
// Lossy: drops session_meta, turn_context, event_msg, and response_item/
// reasoning rows.
func EncodeCanonical(jsonl []byte) ([]acf.ConversationEvent, error) {
	events, _ := encodeCanonicalFrom(jsonl, 0)
	return events, nil
}

// generatedCodexSessionHasFilteredInternals reports whether a generated
// rollout contains execution-local rows that the portable encoder removes.
// Dispatch uses this as adapter-authenticated evidence for repairing a legacy
// canonical head that an older release polluted with those same rows.
func generatedCodexSessionHasFilteredInternals(jsonl []byte) bool {
	if !generatedCodexSession(jsonl) {
		return false
	}
	dec := json.NewDecoder(bytes.NewReader(jsonl))
	for {
		var row rawCodexRow
		if err := dec.Decode(&row); err != nil {
			return false
		}
		if row.Type != "response_item" {
			continue
		}
		var payload rawCodexPayload
		if json.Unmarshal(row.Payload, &payload) != nil {
			continue
		}
		switch payload.Type {
		case "message":
			role := normalizeCodexRole(payload.Role)
			if role == "system" || (role == "assistant" && !codexFinalAnswerPhase(payload.Phase)) {
				return true
			}
		case "function_call", "custom_tool_call", "web_search_call",
			"function_call_output", "custom_tool_call_output":
			return true
		}
	}
}

// generatedCodexLegacyVisibleTurns reconstructs only the role/text sequence
// that the pre-v1.0.39 encoder would have exposed from these same authenticated
// generated-session bytes. In particular, assistant commentary had no phase
// filter then. The merge layer uses this sequence solely as exact repair proof;
// it is never persisted or materialized.
func generatedCodexLegacyVisibleTurns(jsonl []byte) []acf.TextTurn {
	if !generatedCodexSession(jsonl) {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(jsonl))
	var events []acf.ConversationEvent
	for {
		var row rawCodexRow
		if err := dec.Decode(&row); err != nil {
			break
		}
		if row.Type != "response_item" {
			continue
		}
		var payload rawCodexPayload
		if json.Unmarshal(row.Payload, &payload) != nil || payload.Type != "message" {
			continue
		}
		role := normalizeCodexRole(payload.Role)
		if role != "user" && role != "assistant" {
			continue
		}
		content := codexContent(payload.Content)
		if len(content) == 0 {
			continue
		}
		var ok bool
		content, ok = normalizeCodexTurnContent(role, content)
		if !ok || (role == "assistant" && syntheticNoResponse(content)) {
			continue
		}
		events = append(events, acf.ConversationEvent{
			Type: acf.EventTypeTurn, Role: role, Content: content,
		})
	}
	return acf.ExtractTextTurns(events)
}

// sanitizeGeneratedMaterializedEchoes repairs the two boundary echoes Codex
// can write when it resumes an Aplexica-generated rollout:
//
//   - Codex can replay the final materialized user prompt before its answer.
//   - Codex and Aplexica can then append the same final answer concurrently.
//
// The session marker stamps the exact materialized base count+hash. Only a
// suffix of that authenticated base repeated immediately at the native
// boundary, and adjacent identical assistant answers after that boundary, are
// collapsible. Distinct turns and anything inside the stamped base remain
// untouched.
func sanitizeGeneratedMaterializedEchoes(ref adapter.ThreadRef, events []acf.ConversationEvent) ([]acf.ConversationEvent, bool) {
	baseCount := ref.MaterializedTurnCount
	if baseCount <= 0 || ref.MaterializedTurnsHash == "" || baseCount >= len(events) {
		return events, false
	}
	turns := acf.ExtractTextTurns(events)
	if len(turns) != len(events) || baseCount > len(turns) ||
		ref.MaterializedTurnsHash != adapter.ConversationTurnsHash(turns[:baseCount]) {
		return events, false
	}

	// Find the longest suffix of the authenticated materialized base that Codex
	// repeated at the start of its native append. A prompt-only generated
	// rollout commonly produces [U1 | U1 A1] after resume; retaining both U1
	// rows makes the next fan-out look like a divergent conversation.
	boundaryEcho := 0
	maxOverlap := min(baseCount, len(turns)-baseCount)
	for overlap := maxOverlap; overlap > 0; overlap-- {
		if acf.TextTurnsEqual(
			turns[baseCount-overlap:baseCount],
			turns[baseCount:baseCount+overlap],
		) {
			boundaryEcho = overlap
			break
		}
	}

	out := append([]acf.ConversationEvent(nil), events[:baseCount]...)
	changed := boundaryEcho > 0
	for i := baseCount + boundaryEcho; i < len(events); i++ {
		turn := turns[i]
		if turn.Role == "assistant" && len(out) > baseCount {
			previous := acf.ExtractTextTurns(out[len(out)-1:])
			if len(previous) == 1 && previous[0].Role == "assistant" && previous[0].Text == turn.Text {
				changed = true
				continue
			}
		}
		out = append(out, events[i])
	}
	if !changed {
		return events, false
	}
	return out, true
}

// encodeCanonicalFrom decodes complete Codex JSONL rows beginning at start and
// returns the absolute byte offset immediately after the last complete row.
// A partial trailing row is deliberately left for the next append. Codex rows
// are independent, so concatenating cached prefix events with events decoded
// from the returned resume offset is equivalent to parsing the full file.
func encodeCanonicalFrom(jsonl []byte, start int64) (events []acf.ConversationEvent, resume int64) {
	return encodeCanonicalFromMode(jsonl, start, generatedCodexSession(jsonl))
}

func encodeCanonicalFromMode(jsonl []byte, start int64, generatedSession bool) (events []acf.ConversationEvent, resume int64) {
	return encodeCanonicalFromPolicy(jsonl, start, generatedSession, false)
}

// encodePortableCanonicalFromMode is the cross-agent conversation boundary.
// Tool invocations/results are local execution transcript, just like system
// policy and commentary, and must not enter Hermes/Claude portable history.
// EncodeCanonical retains its richer native translator contract for explicit
// round trips; file imports use this projection instead.
func encodePortableCanonicalFromMode(jsonl []byte, start int64, generatedSession bool) ([]acf.ConversationEvent, int64) {
	events, resume := encodeCanonicalFromMode(jsonl, start, generatedSession)
	portable := events[:0]
	for _, event := range events {
		if event.Type == acf.EventTypeToolCall || event.Type == acf.EventTypeToolResult {
			continue
		}
		portable = append(portable, event)
	}
	return portable, resume
}

// encodeCanonicalLegacyNativeForRepair reproduces the pre-v1.0.39 native
// Codex projection from authenticated source bytes. It is used only to prove
// that an existing canonical head consists exclusively of rows emitted by the
// old encoder before replacing that head with the clean projection.
func encodeCanonicalLegacyNativeForRepair(jsonl []byte) []acf.ConversationEvent {
	events, _ := encodeCanonicalFromPolicy(jsonl, 0, false, true)
	return events
}

func encodeCanonicalFromPolicy(jsonl []byte, start int64, generatedSession, legacyNative bool) (events []acf.ConversationEvent, resume int64) {
	if start < 0 || start > int64(len(jsonl)) {
		start = 0
	}
	dec := json.NewDecoder(bytes.NewReader(jsonl[start:]))
	var consumed int64
	for {
		var row rawCodexRow
		if err := dec.Decode(&row); err != nil {
			break
		}
		consumed = dec.InputOffset()
		if row.Type != "response_item" {
			continue
		}
		ts := parseCodexTime(row.Timestamp)
		var p rawCodexPayload
		if err := json.Unmarshal(row.Payload, &p); err != nil {
			continue
		}
		switch p.Type {
		case "message":
			role := normalizeCodexRole(p.Role)
			if role == "" {
				continue
			}
			// Codex writes the complete developer/system execution harness into
			// every native rollout. It is local policy and product context, not a
			// message the user or assistant said, so it must never cross the ACF
			// conversation boundary (native and generated sessions alike).
			if !legacyNative && role == "system" {
				continue
			}
			// Commentary is transient progress UI. Only a final assistant answer
			// belongs in the cross-agent transcript.
			if !legacyNative && role == "assistant" && !codexFinalAnswerPhase(p.Phase) {
				continue
			}
			content := codexContent(p.Content)
			if len(content) == 0 {
				continue
			}
			var ok bool
			content, ok = normalizeCodexTurnContent(role, content)
			if !ok {
				continue
			}
			// Codex/ChatGPT Desktop writes this exact bookkeeping reply when it
			// indexes an imported transcript ending in a user turn. It is not a
			// model response. Importing it creates a false concurrent answer and
			// can make a real answer from another device miss the branch head.
			if !legacyNative && generatedSession && role == "assistant" && syntheticNoResponse(content) {
				continue
			}
			events = append(events, acf.ConversationEvent{
				Type:      acf.EventTypeTurn,
				Timestamp: ts,
				Role:      role,
				Content:   content,
			})
		case "function_call":
			if generatedSession {
				continue
			}
			events = append(events, acf.ConversationEvent{
				Type:      acf.EventTypeToolCall,
				Timestamp: ts,
				CallID:    p.CallID,
				ToolName:  p.Name,
				Input:     normalizeCodexArguments(p.Arguments),
			})
		case "custom_tool_call":
			if generatedSession {
				continue
			}
			// custom_tool_call carries its invocation under `input` (a JSON
			// string — patch text, shell commands), NOT `arguments`. Keep it
			// verbatim as a JSON value (a JSON string is a valid Input). Fall
			// back to `arguments` only if a stray older shape ever sets it.
			in := p.Input
			if len(in) == 0 {
				in = normalizeCodexArguments(p.Arguments)
			}
			events = append(events, acf.ConversationEvent{
				Type:      acf.EventTypeToolCall,
				Timestamp: ts,
				CallID:    p.CallID,
				ToolName:  p.Name,
				Input:     in,
			})
		case "web_search_call":
			if generatedSession {
				continue
			}
			// Built-in web search invocation. Preserve the search action
			// (queries) as the tool Input under a synthetic tool name.
			events = append(events, acf.ConversationEvent{
				Type:      acf.EventTypeToolCall,
				Timestamp: ts,
				CallID:    p.CallID,
				ToolName:  "web_search",
				Input:     p.Action,
			})
		case "function_call_output", "custom_tool_call_output":
			if generatedSession {
				continue
			}
			out := normalizeCodexOutput(p.Output)
			events = append(events, acf.ConversationEvent{
				Type:      acf.EventTypeToolResult,
				Timestamp: ts,
				CallID:    p.CallID,
				Content:   []acf.ContentBlock{{Type: "text", Text: out}},
			})
			// reasoning, anything else → drop
		}
	}
	return events, start + consumed
}

func generatedCodexSession(jsonl []byte) bool {
	generated, _ := generatedCodexSessionState(jsonl)
	return generated
}

// generatedCodexSessionState distinguishes a native transcript from a partial
// first row. The append cache must not permanently classify an incomplete
// Aplexica session_meta row as native before its remaining bytes arrive.
func generatedCodexSessionState(jsonl []byte) (generated, known bool) {
	dec := json.NewDecoder(bytes.NewReader(jsonl))
	decoded := false
	for {
		var row rawCodexRow
		if err := dec.Decode(&row); err != nil {
			return false, decoded
		}
		decoded = true
		if row.Type != "session_meta" {
			continue
		}
		var metadata struct {
			CLIVersion       string `json:"cli_version"`
			AplexicaThreadID string `json:"aplexica_thread_id"`
		}
		if json.Unmarshal(row.Payload, &metadata) != nil {
			return false, true
		}
		return metadata.CLIVersion == syntheticCodexCLIVersion || metadata.AplexicaThreadID != "", true
	}
}

func syntheticNoResponse(content []acf.ContentBlock) bool {
	return len(content) == 1 && content[0].Type == "text" &&
		content[0].Text == "No response requested."
}

// normalizeCodexRole maps codex's role taxonomy onto the canonical one.
// Developer rows are classified as system so the encoder can discard them at
// the conversation boundary while the legacy-repair parser can reconstruct the
// exact projection written by older releases.
func normalizeCodexRole(r string) string {
	switch r {
	case "user", "assistant":
		return r
	case "developer":
		return "system"
	case "system":
		return "system"
	}
	return ""
}

// codexContent converts a Codex message's content blocks into canonical
// ContentBlocks. Consecutive text fragments are concatenated into one text
// block (preserving prior behavior); input_image blocks are preserved as
// image blocks carrying their URL/data URI in ContentBlock.Data, interleaved
// in source order. Previously image blocks were silently dropped and
// image-only messages produced no event at all.
func codexContent(blocks []rawCodexBlock) []acf.ContentBlock {
	var out []acf.ContentBlock
	var text bytes.Buffer
	flush := func() {
		if text.Len() > 0 {
			out = append(out, acf.ContentBlock{Type: "text", Text: text.String()})
			text.Reset()
		}
	}
	for _, blk := range blocks {
		switch blk.Type {
		case "input_text", "output_text", "text":
			text.WriteString(blk.Text)
		case "input_image":
			flush()
			out = append(out, acf.ContentBlock{Type: "image", Data: blk.ImageURL})
		}
	}
	flush()
	return out
}

func normalizeCodexTurnContent(role string, content []acf.ContentBlock) ([]acf.ContentBlock, bool) {
	if role != "user" {
		return content, true
	}
	for _, block := range content {
		if block.Type != "text" {
			return content, true
		}
	}
	normalized, ok := acf.NormalizeTextTurn(role, contentToText(content))
	if !ok {
		return nil, false
	}
	return []acf.ContentBlock{{Type: "text", Text: normalized}}, true
}

// normalizeCodexArguments unwraps Codex's string-encoded arguments. Codex
// stores the arguments field as a JSON-encoded STRING in some shapes (e.g.
// "{\"cmd\":\"ls\"}") and as a JSON object in others. We unwrap the string
// form so canonical Input is always a JSON value — but ONLY when the unwrapped
// value is itself valid JSON. If a string's value is not valid JSON (e.g.
// "ls -la"), unwrapping would leave Input holding the bare bytes `ls -la`,
// which is an invalid json.RawMessage that aborts the entire conversation
// import when acf.EncodePayload (json.Marshal) later validates it. In that
// case we keep the original quoted string value, which is itself a valid JSON
// value and marshals cleanly.
func normalizeCodexArguments(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return raw
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if json.Valid([]byte(s)) {
			return json.RawMessage(s)
		}
		return raw
	}
	return raw
}

// normalizeCodexOutput mirrors normalizeCodexArguments for the output field:
// codex sometimes JSON-encodes a string, sometimes ships an object. Return
// the unwrapped string when possible, else the canonical JSON text.
func normalizeCodexOutput(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return string(raw)
}

func parseCodexTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// DecodeCanonical emits Codex session.jsonl bytes from the canonical events.
// Wraps each canonical event in the Codex {timestamp,type,payload} shape.
// All output rows use type="response_item" — wrapper-level metadata
// (session_meta, turn_context, event_msg) is not synthesized.
func DecodeCanonical(events []acf.ConversationEvent) ([]byte, error) {
	var out bytes.Buffer
	for _, e := range events {
		var payload map[string]any
		switch e.Type {
		case acf.EventTypeTurn:
			role := e.Role
			if role != "user" && role != "assistant" && role != "system" {
				role = "user"
			}
			blockType := "input_text"
			if role == "assistant" {
				blockType = "output_text"
			}
			payload = map[string]any{
				"type": "message",
				"role": role,
				"content": []map[string]any{{
					"type": blockType,
					"text": contentToText(e.Content),
				}},
			}
		case acf.EventTypeToolCall:
			payload = map[string]any{
				"type":      "function_call",
				"name":      e.ToolName,
				"call_id":   e.CallID,
				"arguments": string(e.Input),
			}
		case acf.EventTypeToolResult:
			payload = map[string]any{
				"type":    "function_call_output",
				"call_id": e.CallID,
				"output":  contentToText(e.Content),
			}
		case acf.EventTypeSystemNote:
			payload = map[string]any{
				"type": "message",
				"role": "developer",
				"content": []map[string]any{{
					"type": "input_text",
					"text": contentToText(e.Content),
				}},
			}
		default:
			continue // redaction / amendment / unknown → skip
		}
		row := map[string]any{
			"timestamp": fmtCodexTime(e.Timestamp),
			"type":      "response_item",
			"payload":   payload,
		}
		b, err := json.Marshal(row)
		if err != nil {
			return nil, fmt.Errorf("codex: marshal canonical row: %w", err)
		}
		out.Write(b)
		out.WriteByte('\n')
	}
	return out.Bytes(), nil
}

func fmtCodexTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func contentToText(blocks []acf.ContentBlock) string {
	var b bytes.Buffer
	for _, blk := range blocks {
		if blk.Type == "text" {
			b.WriteString(blk.Text)
		}
	}
	return b.String()
}
