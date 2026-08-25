// Package openclaw — canonical conversation translator (v0.24.1).
//
// EncodeCanonical / DecodeCanonical convert OpenClaw session transcript JSONL
// to/from canonical acf.ConversationEvent values. OpenClaw sessions run on the
// pi-coding-agent runtime (a Claude Code-derived SDK), so the transcript shape
// is essentially the same as Claude Code's session.jsonl — this package
// delegates to claudecode.EncodeCanonical / claudecode.DecodeCanonical.
//
// If pi-coding-agent's transcript format diverges from Claude Code's in
// practice (e.g. additional OpenClaw-specific event types), these wrappers
// can be specialized to handle the differences without changing callers.
//
// Lossiness mirrors claudecode: queue-operation, last-prompt, and attachment
// rows are dropped; user/assistant turns + tool_call/tool_result/system_note
// are preserved.
package openclaw

import (
	"bytes"
	"encoding/json"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter/claudecode"
)

// EncodeCanonical parses an OpenClaw session transcript JSONL stream into
// canonical conversation events. pi-coding-agent transcripts are Claude
// Code-derived but wrap each turn as {"type":"message","message":{role,
// content}} rather than keying the row type on the role. We rewrite that
// discriminator into Claude Code's shape, then delegate to
// claudecode.EncodeCanonical for the shared block-parsing logic.
func EncodeCanonical(jsonl []byte) ([]acf.ConversationEvent, error) {
	return claudecode.EncodeCanonical(rewriteOpenClawTranscript(jsonl))
}

// rewriteOpenClawTranscript maps OpenClaw's {"type":"message","message":{...}}
// rows to the Claude Code shape claudecode.EncodeCanonical understands: the
// row's top-level "type" is set to the inner message.role ("user"/"assistant"/
// "system"), with the "message" object left intact (claudecode reads content
// from .message). All other rows (session-meta, custom, session, …) pass
// through unchanged and are dropped by claudecode's unknown-type default.
func rewriteOpenClawTranscript(jsonl []byte) []byte {
	dec := json.NewDecoder(bytes.NewReader(jsonl))
	var out bytes.Buffer
	for {
		var row map[string]json.RawMessage
		if err := dec.Decode(&row); err != nil {
			break
		}
		var typ string
		if raw, ok := row["type"]; ok {
			_ = json.Unmarshal(raw, &typ)
		}
		if typ == "message" {
			if msgRaw, ok := row["message"]; ok {
				var msg struct {
					Role string `json:"role"`
				}
				if json.Unmarshal(msgRaw, &msg) == nil && msg.Role != "" {
					if roleJSON, err := json.Marshal(msg.Role); err == nil {
						row["type"] = roleJSON
					}
				}
			}
		}
		b, err := json.Marshal(row)
		if err != nil {
			continue
		}
		out.Write(b)
		out.WriteByte('\n')
	}
	return out.Bytes()
}

// DecodeCanonical emits an OpenClaw session transcript JSONL stream from
// canonical events. Delegates to claudecode.DecodeCanonical.
func DecodeCanonical(events []acf.ConversationEvent) ([]byte, error) {
	return claudecode.DecodeCanonical(events)
}
