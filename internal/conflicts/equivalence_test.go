package conflicts

import (
	"testing"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/stretchr/testify/require"
)

func TestSemanticallyEquivalent_ConversationHermesBundleIgnoresHiddenMetadata(t *testing.T) {
	headA := hermesConversationEvent(t, "event-a", `{"session":{"id":"s1","system_prompt":"one"},"messages":[
		{"role":"user","content":"What is the capital of France?"},
		{"role":"assistant","content":"Paris."}
	]}`)
	headB := hermesConversationEvent(t, "event-b", `{"session":{"id":"s1","system_prompt":"two","cost_status":"changed"},"messages":[
		{"role":"user","content":"What is the capital of France?"},
		{"role":"assistant","content":"Paris."}
	]}`)

	require.True(t, SemanticallyEquivalent(acf.KindConversation, headA, headB))
}

func hermesConversationEvent(t *testing.T, id, content string) acf.Event {
	t.Helper()
	payload, err := acf.EncodePayload(acf.ConversationPayload{
		Format:  acf.ConversationFormatHermesBundle,
		Content: content,
	})
	require.NoError(t, err)
	return acf.Event{
		EventID: id,
		Type:    acf.EventTypeUpdate,
		Payload: payload,
	}
}
