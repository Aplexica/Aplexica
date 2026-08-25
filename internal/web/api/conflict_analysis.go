package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/conflicts"
)

const (
	maxConflictDifferences  = 5
	maxConflictPayloadRunes = 4000
)

// ConflictEventLookup loads the full event behind a conflict head.
type ConflictEventLookup func(ctx context.Context, kind acf.Kind, artifactID, eventID string) (acf.Event, bool, error)

// ConflictAnalysis is the human-readable companion to a raw conflict record.
// It is intentionally compact so the SPA can present the decision without
// parsing full ACF payloads in the browser.
type ConflictAnalysis struct {
	Summary        string                 `json:"summary"`
	Recommendation string                 `json:"recommendation"`
	AutoResolvable bool                   `json:"autoResolvable,omitempty"`
	PreferredHead  string                 `json:"preferredHead,omitempty"`
	Heads          []ConflictHeadAnalysis `json:"heads,omitempty"`
	Differences    []ConflictDifference   `json:"differences,omitempty"`
}

type ConflictHeadAnalysis struct {
	Label       string `json:"label"`
	SourceAgent string `json:"sourceAgent"`
	Summary     string `json:"summary"`
	PrimaryText string `json:"primaryText,omitempty"`
	PayloadJSON string `json:"payloadJson,omitempty"`
}

type ConflictDifference struct {
	Label  string `json:"label"`
	Status string `json:"status"`
	HeadA  string `json:"headA,omitempty"`
	HeadB  string `json:"headB,omitempty"`
}

// AnalyzeConflict turns full conflict-head events into a reader-facing summary.
// Missing/unknown event bodies degrade to a metadata-only summary instead of
// blocking the conflict detail page.
func AnalyzeConflict(c conflicts.Conflict, lookup ConflictEventLookup) (*ConflictAnalysis, error) {
	if len(c.Heads) < 2 || lookup == nil {
		return fallbackConflictAnalysis(c), nil
	}
	headA, okA, err := lookup(context.Background(), c.Kind, c.ArtifactID, c.Heads[0].EventID)
	if err != nil {
		return nil, err
	}
	headB, okB, err := lookup(context.Background(), c.Kind, c.ArtifactID, c.Heads[1].EventID)
	if err != nil {
		return nil, err
	}
	// A remote inbound conflict head is recorded but never appended to any local
	// branch (B3), so the canonical EventID lookup returns ok=false for it. Its
	// full content is preserved in the conflict sidecar's FullPayload — fall back
	// to a synthetic event from those bytes so the side-by-side diff can still
	// render the remote content instead of degrading to a preview-only summary.
	if !okA {
		headA, okA = eventFromHeadPayload(c.Heads[0])
	}
	if !okB {
		headB, okB = eventFromHeadPayload(c.Heads[1])
	}
	if !okA || !okB {
		return fallbackConflictAnalysis(c), nil
	}

	switch c.Kind {
	case acf.KindConversation:
		return analyzeConversationConflict(c, headA, headB)
	case acf.KindMemory, acf.KindSkill, acf.KindTool:
		return analyzeTextConflict(c, headA, headB)
	default:
		return fallbackConflictAnalysis(c), nil
	}
}

func analyzeConversationConflict(c conflicts.Conflict, headA, headB acf.Event) (*ConflictAnalysis, error) {
	payloadA, err := acf.DecodeConversationPayload(headA)
	if err != nil {
		return fallbackConflictAnalysis(c), nil
	}
	payloadB, err := acf.DecodeConversationPayload(headB)
	if err != nil {
		return fallbackConflictAnalysis(c), nil
	}
	turnsA, okA := acf.ConversationTextTurns(payloadA)
	turnsB, okB := acf.ConversationTextTurns(payloadB)
	if !okA || !okB {
		return fallbackConflictAnalysis(c), nil
	}
	diffs := compareTurns(turnsA, turnsB)

	summary := fmt.Sprintf("Visible conversation differs across %d highlighted turn(s). Head A has %d visible turns; Head B has %d.",
		len(diffs), len(turnsA), len(turnsB))
	if len(diffs) == 0 {
		summary = fmt.Sprintf("The visible conversation turns match. The raw payload differs in hidden context, metadata, timestamps, or formatting. Both heads have %d visible turns.", len(turnsA))
	} else {
		summary = fmt.Sprintf("Visible conversation differs at %s. Head A has %d visible turns; Head B has %d.",
			strings.ToLower(diffs[0].Label), len(turnsA), len(turnsB))
	}

	return &ConflictAnalysis{
		Summary:        summary,
		Recommendation: conversationRecommendation(len(diffs) == 0),
		AutoResolvable: conflicts.SemanticallyEquivalent(c.Kind, headA, headB),
		PreferredHead:  preferredHeadLabel(c),
		Heads: []ConflictHeadAnalysis{
			withPayloadJSON(headConversationSummary("A", c.Heads[0].SourceAgent, turnsA), headA),
			withPayloadJSON(headConversationSummary("B", c.Heads[1].SourceAgent, turnsB), headB),
		},
		Differences: diffs,
	}, nil
}

func analyzeTextConflict(c conflicts.Conflict, headA, headB acf.Event) (*ConflictAnalysis, error) {
	textA, okA := payloadText(c.Kind, headA)
	textB, okB := payloadText(c.Kind, headB)
	if !okA || !okB {
		return fallbackConflictAnalysis(c), nil
	}
	linesA := splitLines(textA)
	linesB := splitLines(textB)
	diffs := compareLines(linesA, linesB)

	summary := fmt.Sprintf("Text differs across %d highlighted line(s). Head A has %d lines; Head B has %d.",
		len(diffs), len(linesA), len(linesB))
	if len(diffs) == 0 {
		summary = fmt.Sprintf("The text content matches. The raw payload differs in metadata or formatting. Both heads have %d lines.", len(linesA))
	} else {
		summary = fmt.Sprintf("Text differs at %s. Head A has %d lines; Head B has %d.",
			strings.ToLower(diffs[0].Label), len(linesA), len(linesB))
	}

	return &ConflictAnalysis{
		Summary:        summary,
		Recommendation: "Review the highlighted line. Accept the head with the text you want to keep, or use manual merge if both sides contain useful edits.",
		AutoResolvable: conflicts.SemanticallyEquivalent(c.Kind, headA, headB),
		PreferredHead:  preferredHeadLabel(c),
		Heads: []ConflictHeadAnalysis{
			withPayloadJSON(headTextSummary("A", c.Heads[0].SourceAgent, linesA), headA),
			withPayloadJSON(headTextSummary("B", c.Heads[1].SourceAgent, linesB), headB),
		},
		Differences: diffs,
	}, nil
}

func compareTurns(a, b []acf.TextTurn) []ConflictDifference {
	limit := max(len(a), len(b))
	out := make([]ConflictDifference, 0, maxConflictDifferences)
	for i := 0; i < limit && len(out) < maxConflictDifferences; i++ {
		var left, right string
		status := "changed"
		if i < len(a) {
			left = formatTurn(a[i])
		} else {
			status = "only-b"
		}
		if i < len(b) {
			right = formatTurn(b[i])
		} else {
			status = "only-a"
		}
		if left == right {
			continue
		}
		out = append(out, ConflictDifference{
			Label:  fmt.Sprintf("Turn %d", i+1),
			Status: status,
			HeadA:  left,
			HeadB:  right,
		})
	}
	return out
}

func compareLines(a, b []string) []ConflictDifference {
	limit := max(len(a), len(b))
	out := make([]ConflictDifference, 0, maxConflictDifferences)
	for i := 0; i < limit && len(out) < maxConflictDifferences; i++ {
		var left, right string
		status := "changed"
		if i < len(a) {
			left = strings.TrimSpace(a[i])
		} else {
			status = "only-b"
		}
		if i < len(b) {
			right = strings.TrimSpace(b[i])
		} else {
			status = "only-a"
		}
		if left == right {
			continue
		}
		out = append(out, ConflictDifference{
			Label:  fmt.Sprintf("Line %d", i+1),
			Status: status,
			HeadA:  truncateText(left, 240),
			HeadB:  truncateText(right, 240),
		})
	}
	return out
}

func payloadText(kind acf.Kind, e acf.Event) (string, bool) {
	switch kind {
	case acf.KindMemory:
		p, err := acf.DecodeMemoryPayload(e)
		return p.Content, err == nil
	case acf.KindSkill:
		p, err := acf.DecodeSkillPayload(e)
		return p.Content, err == nil
	case acf.KindTool:
		p, err := acf.DecodeToolPayload(e)
		return p.Content, err == nil
	default:
		return "", false
	}
}

func headConversationSummary(label, sourceAgent string, turns []acf.TextTurn) ConflictHeadAnalysis {
	return ConflictHeadAnalysis{
		Label:       label,
		SourceAgent: sourceAgent,
		Summary:     fmt.Sprintf("%d visible turns", len(turns)),
		PrimaryText: firstTurnText(turns),
	}
}

func headTextSummary(label, sourceAgent string, lines []string) ConflictHeadAnalysis {
	return ConflictHeadAnalysis{
		Label:       label,
		SourceAgent: sourceAgent,
		Summary:     fmt.Sprintf("%d lines", len(lines)),
		PrimaryText: firstNonEmptyLine(lines),
	}
}

// eventFromHeadPayload reconstructs a minimal acf.Event from a conflict head's
// preserved FullPayload (B3). It is the fallback for a head whose event is not
// in the local canonical log (a remote inbound head). Returns false when the
// head carries no full payload, so callers degrade to the preview-only summary.
func eventFromHeadPayload(h conflicts.Head) (acf.Event, bool) {
	if len(h.FullPayload) == 0 {
		return acf.Event{}, false
	}
	return acf.Event{
		EventID:    h.EventID,
		Hash:       h.ContentSHA256,
		Provenance: acf.Provenance{SourceAgent: h.SourceAgent},
		Payload:    append(json.RawMessage(nil), h.FullPayload...),
	}, true
}

func fallbackConflictAnalysis(c conflicts.Conflict) *ConflictAnalysis {
	return &ConflictAnalysis{
		Summary:        "Aplexica could not load enough payload detail to highlight the exact content difference.",
		Recommendation: "Use the head metadata and payload previews to choose a side, or use manual merge if neither preview is enough.",
		Heads:          fallbackHeadSummaries(c),
	}
}

func fallbackHeadSummaries(c conflicts.Conflict) []ConflictHeadAnalysis {
	out := make([]ConflictHeadAnalysis, 0, len(c.Heads))
	for i, h := range c.Heads {
		label := string(rune('A' + i))
		out = append(out, ConflictHeadAnalysis{
			Label:       label,
			SourceAgent: h.SourceAgent,
			Summary:     "Preview only",
			PrimaryText: truncateText(h.PayloadPreview, 240),
		})
	}
	return out
}

func conversationRecommendation(visibleMatches bool) string {
	if visibleMatches {
		return "The human-visible turns match. Aplexica keeps the newest equivalent head automatically; no manual decision is needed."
	}
	return "Review the highlighted turns. Accept the head with the transcript you want to keep, or use manual merge if both sides contain useful messages."
}

func preferredHeadLabel(c conflicts.Conflict) string {
	if len(c.Heads) == 0 {
		return ""
	}
	bestIdx := 0
	best := c.Heads[0].AbsTimestamp
	for i := 1; i < len(c.Heads); i++ {
		if c.Heads[i].AbsTimestamp >= best {
			bestIdx = i
			best = c.Heads[i].AbsTimestamp
		}
	}
	return string(rune('A' + bestIdx))
}

func withPayloadJSON(head ConflictHeadAnalysis, event acf.Event) ConflictHeadAnalysis {
	head.PayloadJSON = prettyPayloadJSON(event)
	return head
}

func prettyPayloadJSON(event acf.Event) string {
	if len(event.Payload) == 0 {
		return ""
	}
	var decoded any
	if err := json.Unmarshal(event.Payload, &decoded); err == nil {
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		if err := enc.Encode(decoded); err == nil {
			return truncatePayloadJSON(strings.TrimRight(buf.String(), "\n"))
		}
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, event.Payload, "", "  "); err != nil {
		return truncatePayloadJSON(string(event.Payload))
	}
	return truncatePayloadJSON(buf.String())
}

func truncatePayloadJSON(s string) string {
	runes := []rune(s)
	if len(runes) <= maxConflictPayloadRunes {
		return s
	}
	return string(runes[:maxConflictPayloadRunes]) + "\n… truncated raw payload preview …"
}

func firstTurnText(turns []acf.TextTurn) string {
	for _, turn := range turns {
		if turn.Text != "" {
			return truncateText(formatTurn(turn), 240)
		}
	}
	return ""
}

func firstNonEmptyLine(lines []string) string {
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" {
			return truncateText(line, 240)
		}
	}
	return ""
}

func formatTurn(turn acf.TextTurn) string {
	role := turn.Role
	if role == "" {
		role = "turn"
	}
	return role + ": " + truncateText(turn.Text, 240)
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func truncateText(s string, maxRunes int) string {
	s = strings.TrimSpace(strings.Join(strings.Fields(s), " "))
	if s == "" {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	if maxRunes <= 1 {
		return string(runes[:maxRunes])
	}
	return string(runes[:maxRunes-1]) + "…"
}
