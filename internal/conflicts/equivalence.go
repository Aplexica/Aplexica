package conflicts

import "github.com/aplexica/aplexica/internal/acf"

// SemanticallyEquivalent reports whether two different raw payloads carry the
// same user-visible artifact content. It is intentionally narrower than hash
// equality: metadata, formatting, timestamps, and injected agent context can
// differ without requiring a human conflict decision.
func SemanticallyEquivalent(kind acf.Kind, a, b acf.Event) bool {
	switch kind {
	case acf.KindConversation:
		return equivalentConversation(a, b)
	case acf.KindMemory, acf.KindSkill, acf.KindTool:
		textA, okA := payloadText(kind, a)
		textB, okB := payloadText(kind, b)
		return okA && okB && textA == textB
	default:
		return false
	}
}

func equivalentConversation(a, b acf.Event) bool {
	payloadA, err := acf.DecodeConversationPayload(a)
	if err != nil {
		return false
	}
	payloadB, err := acf.DecodeConversationPayload(b)
	if err != nil {
		return false
	}
	turnsA, okA := acf.ConversationTextTurns(payloadA)
	turnsB, okB := acf.ConversationTextTurns(payloadB)
	return okA && okB && acf.TextTurnsEqual(turnsA, turnsB)
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
