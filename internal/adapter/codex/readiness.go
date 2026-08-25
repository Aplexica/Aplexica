package codex

import (
	"encoding/json"
	"io"
	"strings"
)

// SessionReadyForImport reports whether a Codex rollout contains at least one
// complete portable user turn or final assistant answer. User turns are
// intentionally importable before their answer: cross-agent sync is a
// two-phase stream, so a target can show the question while Codex is working
// and receive the final answer as a later append.
//
// This gate is deliberately shared by the fast live scanner and the adapter's
// generated-thread merge path. The latter is the correctness boundary: native
// filesystem watcher events can bypass the scanner, and importing an incomplete
// Aplexica-generated continuation would allow fan-out to replace the same file
// while Codex still has its old inode open for appends.
func SessionReadyForImport(r io.Reader) (bool, error) {
	dec := json.NewDecoder(r)
	hasPortableTurn := false
	generated := false

	for {
		var row rawCodexRow
		if err := dec.Decode(&row); err != nil {
			if err == io.EOF {
				break
			}
			// A partial trailing row does not invalidate complete turns before
			// it. The next append changes the fingerprint and retries the tail.
			return hasPortableTurn, nil
		}
		switch row.Type {
		case "session_meta":
			var metadata struct {
				AplexicaThreadID string `json:"aplexica_thread_id"`
			}
			if json.Unmarshal(row.Payload, &metadata) == nil && metadata.AplexicaThreadID != "" {
				generated = true
			}

		case "response_item":
			var payload rawCodexPayload
			if json.Unmarshal(row.Payload, &payload) != nil || payload.Type != "message" {
				continue
			}
			role := normalizeCodexRole(payload.Role)
			content := codexContent(payload.Content)
			if len(content) == 0 {
				continue
			}
			switch role {
			case "user":
				if _, ok := normalizeCodexTurnContent(role, content); ok {
					hasPortableTurn = true
				}
			case "assistant":
				if generated && syntheticNoResponse(content) {
					continue
				}
				if codexFinalAnswerPhase(payload.Phase) {
					hasPortableTurn = true
				}
			}
		}
	}

	return hasPortableTurn, nil
}

// Older Codex rollouts did not stamp phases, and their assistant message rows
// were final by construction. Current rollouts distinguish commentary from the
// final answer; only the latter completes a turn.
func codexFinalAnswerPhase(phase string) bool {
	phase = strings.ToLower(strings.ReplaceAll(strings.TrimSpace(phase), "-", "_"))
	return phase == "" || phase == "final_answer" || phase == "finalanswer"
}
