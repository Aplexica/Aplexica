package syncd

import (
	"strings"
	"testing"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/stretchr/testify/require"
)

func convEvent(t *testing.T, p acf.ConversationPayload) acf.Event {
	t.Helper()
	raw, err := acf.EncodePayload(p)
	if err != nil {
		t.Fatalf("encode payload: %v", err)
	}
	return acf.Event{Type: acf.EventTypeCreate, Payload: raw}
}

func TestRenderConversationMarkdown_Canonical(t *testing.T) {
	art := acf.Artifact{ArtifactID: "019e848b-ffde-7110", Name: "rollout-2026-06-01.jsonl", SourcePath: "/Users/testuser/.codex/sessions/2026/06/01/rollout-2026-06-01.jsonl"}
	head := convEvent(t, acf.ConversationPayload{
		Format: acf.ConversationFormatV1,
		Events: []acf.ConversationEvent{
			{Type: "turn", Role: "user", Content: []acf.ContentBlock{{Type: "text", Text: "what is the project name?"}}},
			{Type: "tool_call", ToolName: "shell"},
			{Type: "turn", Role: "assistant", Content: []acf.ContentBlock{{Type: "text", Text: "No project name is specified."}}},
		},
	})
	md, err := renderConversationMarkdown(art, "codex", head)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"# Conversation — rollout-2026-06-01.jsonl",
		"from **codex**",
		"### User",
		"what is the project name?",
		"### Assistant",
		"No project name is specified.",
		"tool call",
		"`shell`",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("rendered transcript missing %q\n---\n%s", want, md)
		}
	}
}

func TestRenderConversationMarkdown_ShowsNonMainBranch(t *testing.T) {
	art := acf.Artifact{
		ArtifactID: "019e848b-ffde-7110-9cf9-03a729063a1e",
		Name:       "What is the temperature on Mercury?",
	}
	payload, err := acf.EncodePayload(acf.ConversationPayload{
		Format: acf.ConversationFormatV1,
		Events: []acf.ConversationEvent{{
			Type:    acf.EventTypeTurn,
			Role:    "user",
			Content: []acf.ContentBlock{{Type: "text", Text: "What is the temperature on Mercury?"}},
		}},
	})
	require.NoError(t, err)

	md, err := renderConversationMarkdown(art, "codex", acf.Event{Branch: "Test2", Payload: payload})
	require.NoError(t, err)
	require.Contains(t, md, "# Conversation — [test2] What is the temperature on Mercury?")
}

func TestRenderConversationMarkdown_OpaqueFallback(t *testing.T) {
	art := acf.Artifact{ArtifactID: "abc", Name: "legacy.jsonl"}
	head := convEvent(t, acf.ConversationPayload{
		Format:  "codex.session.jsonl",
		Content: `{"type":"event_msg","payload":{"type":"task_started"}}`,
	})
	md, err := renderConversationMarkdown(art, "codex", head)
	if err != nil {
		t.Fatal(err)
	}
	// Opaque payloads are preserved verbatim inside a fenced block.
	if !strings.Contains(md, "```") || !strings.Contains(md, "task_started") {
		t.Errorf("opaque fallback should fence the raw content; got:\n%s", md)
	}
}

func TestConversationDocFilename(t *testing.T) {
	got := conversationDocFilename("codex", "019e848b-ffde-7110-9cf9-03a729063a1e")
	if got != "codex-019e848b-ffd.md" {
		t.Errorf("conversationDocFilename = %q, want codex-019e848b-ffd.md", got)
	}
	if conversationDocFilename("", "x") != "agent-x.md" {
		t.Errorf("empty source agent should fall back to 'agent'")
	}
}
