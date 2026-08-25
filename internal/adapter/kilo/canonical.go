package kilo

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
)

const kiloInternalCheckpointTag = "internal-checkpoint"

func encodeKiloBundleAsCanonical(b kiloSessionBundle) ([]acf.ConversationEvent, error) {
	var events []acf.ConversationEvent
	for _, mb := range b.Messages {
		if len(mb.Parts) == 0 {
			if mb.Message.Role == "assistant" && kiloMessageHasError(mb.Message.Data) {
				events = append(events, acf.ConversationEvent{
					Type:         acf.EventTypeSystemNote,
					Timestamp:    kiloMillis(mb.Message.TimeCreated),
					Content:      textBlocks("Kilo assistant message ended with an error"),
					Tags:         []string{"kilo:error", kiloInternalCheckpointTag},
					NativeExtras: kiloNativeExtras(b.Session, &mb.Message, nil),
				})
			}
			continue
		}
		for _, p := range mb.Parts {
			next, err := kiloPartToEvents(b.Session, mb.Message, p)
			if err != nil {
				return nil, err
			}
			events = append(events, next...)
		}
	}
	return events, nil
}

func kiloPartToEvents(session kiloSession, message kiloMessage, part kiloPart) ([]acf.ConversationEvent, error) {
	switch part.Type {
	case "text":
		return kiloTextPartToEvents(session, message, part)
	case "reasoning":
		return kiloReasoningPartToEvents(session, message, part)
	case "tool":
		return kiloToolPartToEvents(session, message, part)
	case "step-start":
		return []acf.ConversationEvent{{
			Type:         acf.EventTypeSystemNote,
			Timestamp:    kiloPartTimestamp(part, part.Data, true),
			Content:      textBlocks("Kilo step started"),
			Tags:         []string{"kilo:step-start", kiloInternalCheckpointTag},
			NativeExtras: kiloNativeExtras(session, &message, &part),
		}}, nil
	case "step-finish":
		reason := kiloOptionalString(part.Data, "reason")
		text := "Kilo step finished"
		if reason != "" {
			text += ": " + reason
		}
		return []acf.ConversationEvent{{
			Type:         acf.EventTypeSystemNote,
			Timestamp:    kiloPartTimestamp(part, part.Data, false),
			Content:      textBlocks(text),
			Tags:         []string{"kilo:step-finish", kiloInternalCheckpointTag},
			NativeExtras: kiloNativeExtras(session, &message, &part),
		}}, nil
	case "compaction":
		return []acf.ConversationEvent{{
			Type:         acf.EventTypeSystemNote,
			Timestamp:    kiloPartTimestamp(part, part.Data, true),
			Content:      textBlocks("Kilo compaction checkpoint"),
			Tags:         []string{"kilo:compaction", kiloInternalCheckpointTag},
			NativeExtras: kiloNativeExtras(session, &message, &part),
		}}, nil
	default:
		tag := "kilo:unknown-part"
		if part.Type != "" {
			tag = "kilo:" + part.Type
		}
		return []acf.ConversationEvent{{
			Type:         acf.EventTypeSystemNote,
			Timestamp:    kiloPartTimestamp(part, part.Data, true),
			Content:      textBlocks(fmt.Sprintf("Kilo %s part preserved as an internal checkpoint", part.Type)),
			Tags:         []string{tag, kiloInternalCheckpointTag},
			NativeExtras: kiloNativeExtras(session, &message, &part),
		}}, nil
	}
}

func kiloTextPartToEvents(session kiloSession, message kiloMessage, part kiloPart) ([]acf.ConversationEvent, error) {
	var data struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(part.Data, &data); err != nil {
		return nil, fmt.Errorf("kilo: decode text part %s: %w", part.ID, err)
	}
	if data.Text == "" {
		return nil, nil
	}
	role := message.Role
	if role == "" {
		role = "assistant"
	}
	return []acf.ConversationEvent{{
		Type:         acf.EventTypeTurn,
		Timestamp:    kiloPartTimestamp(part, part.Data, true),
		Role:         role,
		Content:      textBlocks(data.Text),
		NativeExtras: kiloNativeExtras(session, &message, &part),
	}}, nil
}

func kiloReasoningPartToEvents(session kiloSession, message kiloMessage, part kiloPart) ([]acf.ConversationEvent, error) {
	var data struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(part.Data, &data); err != nil {
		return nil, fmt.Errorf("kilo: decode reasoning part %s: %w", part.ID, err)
	}
	if data.Text == "" {
		return nil, nil
	}
	return []acf.ConversationEvent{{
		Type:         acf.EventTypeSystemNote,
		Timestamp:    kiloPartTimestamp(part, part.Data, true),
		Content:      textBlocks(data.Text),
		Tags:         []string{"kilo:reasoning", kiloInternalCheckpointTag},
		NativeExtras: kiloNativeExtras(session, &message, &part),
	}}, nil
}

func kiloToolPartToEvents(session kiloSession, message kiloMessage, part kiloPart) ([]acf.ConversationEvent, error) {
	var data struct {
		CallID string `json:"callID"`
		Tool   string `json:"tool"`
		State  struct {
			Status string          `json:"status"`
			Input  json.RawMessage `json:"input"`
			Output string          `json:"output"`
			Error  string          `json:"error"`
			Time   struct {
				Start int64 `json:"start"`
				End   int64 `json:"end"`
			} `json:"time"`
		} `json:"state"`
	}
	if err := json.Unmarshal(part.Data, &data); err != nil {
		return nil, fmt.Errorf("kilo: decode tool part %s: %w", part.ID, err)
	}
	callID := data.CallID
	if callID == "" {
		callID = part.ID
	}
	input := data.State.Input
	if len(input) == 0 {
		input = json.RawMessage(`{}`)
	}
	toolName := data.Tool
	if toolName == "" {
		toolName = "unknown"
	}
	call := acf.ConversationEvent{
		Type:         acf.EventTypeToolCall,
		Timestamp:    kiloMillisOrPart(data.State.Time.Start, part),
		CallID:       callID,
		ToolName:     toolName,
		Input:        input,
		NativeExtras: kiloNativeExtras(session, &message, &part),
	}
	switch data.State.Status {
	case "completed":
		return []acf.ConversationEvent{call, acf.ConversationEvent{
			Type:         acf.EventTypeToolResult,
			Timestamp:    kiloMillisOrPart(data.State.Time.End, part),
			CallID:       callID,
			Content:      textBlocks(data.State.Output),
			NativeExtras: kiloNativeExtras(session, &message, &part),
		}}, nil
	case "error":
		return []acf.ConversationEvent{call, acf.ConversationEvent{
			Type:         acf.EventTypeToolResult,
			Timestamp:    kiloMillisOrPart(data.State.Time.End, part),
			CallID:       callID,
			Content:      textBlocks(data.State.Error),
			IsError:      true,
			NativeExtras: kiloNativeExtras(session, &message, &part),
		}}, nil
	default:
		call.Tags = []string{"kilo:tool-incomplete", kiloInternalCheckpointTag}
		return []acf.ConversationEvent{call}, nil
	}
}

func kiloPartTimestamp(part kiloPart, raw json.RawMessage, preferStart bool) time.Time {
	var data struct {
		Time struct {
			Start   int64 `json:"start"`
			End     int64 `json:"end"`
			Created int64 `json:"created"`
		} `json:"time"`
	}
	_ = json.Unmarshal(raw, &data)
	if preferStart {
		if data.Time.Start > 0 {
			return kiloMillis(data.Time.Start)
		}
	} else if data.Time.End > 0 {
		return kiloMillis(data.Time.End)
	}
	if data.Time.Created > 0 {
		return kiloMillis(data.Time.Created)
	}
	if data.Time.End > 0 {
		return kiloMillis(data.Time.End)
	}
	return kiloMillis(part.TimeCreated)
}

func kiloMillisOrPart(ms int64, part kiloPart) time.Time {
	if ms > 0 {
		return kiloMillis(ms)
	}
	return kiloMillis(part.TimeCreated)
}

func kiloOptionalString(raw json.RawMessage, key string) string {
	value, err := kiloRawString(raw, key)
	if err != nil {
		return ""
	}
	return value
}

func kiloMessageHasError(raw json.RawMessage) bool {
	var data struct {
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		return false
	}
	return len(data.Error) > 0 && string(data.Error) != "null"
}

func textBlocks(s string) []acf.ContentBlock {
	return []acf.ContentBlock{{Type: "text", Text: s}}
}

func kiloNativeExtras(session kiloSession, message *kiloMessage, part *kiloPart) json.RawMessage {
	// Deliberately omit the volatile session-activity timestamps
	// (time_updated, time_compacting, time_archived): Kilo bumps them on
	// essentially any interaction, even when no message content changed.
	// Embedding them here made every re-read byte-different from the stored
	// payload, so the dedup in kiloPayloadMatchesLatest appended a new Update
	// event whose only delta was an mtime (unbounded event-log churn). The
	// session's last-activity time is preserved on the artifact header
	// (Artifact.UpdatedAt), where churn is expected and where the conversation
	// materializer reads it from. time_created stays: it is a stable creation
	// stamp, not volatile activity.
	extras := map[string]any{
		"session_id":   session.ID,
		"project_id":   session.ProjectID,
		"parent_id":    session.ParentID,
		"directory":    session.Directory,
		"title":        session.Title,
		"time_created": session.TimeCreated,
	}
	if message != nil {
		extras["message_id"] = message.ID
		extras["message_role"] = message.Role
		extras["message_data"] = message.Data
	}
	if part != nil {
		extras["part_id"] = part.ID
		extras["part_type"] = part.Type
		extras["part_data"] = part.Data
	}
	b, err := json.Marshal(extras)
	if err != nil {
		return nil
	}
	return b
}
