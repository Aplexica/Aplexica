package syncd

import (
	"fmt"
	"strings"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
)

// renderConversationMarkdown renders a conversation artifact's HEAD event into a
// deterministic, human-readable markdown transcript. Canonical
// (acf.conversation.v1) events render as role-labelled turns; an opaque payload
// (e.g. a legacy codex.session.jsonl) falls back to a fenced raw block so
// nothing is ever lost. Deterministic: same events → same markdown.
func renderConversationMarkdown(art acf.Artifact, sourceAgent string, head acf.Event) (string, error) {
	p, err := acf.DecodeConversationPayload(head)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	title := art.Name
	if title == "" {
		title = art.ArtifactID
	}
	title = adapter.ConversationBranchDisplayTitle(title, head.Branch)
	fmt.Fprintf(&b, "# Conversation — %s\n\n", title)
	if sourceAgent != "" {
		fmt.Fprintf(&b, "> Synced by Aplexica from **%s**. Read-only transcript — edits here are not synced back.\n", sourceAgent)
	}
	if art.SourcePath != "" {
		fmt.Fprintf(&b, "> Source: `%s`\n", art.SourcePath)
	}
	fmt.Fprintf(&b, "> Artifact: `%s`\n\n---\n\n", art.ArtifactID)

	if p.Format == acf.ConversationFormatV1 {
		for _, ev := range p.Events {
			renderCanonicalEvent(&b, ev)
		}
	} else {
		b.WriteString("```\n")
		b.WriteString(strings.TrimRight(p.Content, "\n"))
		b.WriteString("\n```\n")
	}
	return strings.TrimRight(b.String(), "\n") + "\n", nil
}

func renderCanonicalEvent(b *strings.Builder, ev acf.ConversationEvent) {
	switch ev.Type {
	case "turn", "message", "":
		b.WriteString("### " + roleTitle(ev.Role))
		if !ev.Timestamp.IsZero() {
			b.WriteString("  ·  " + ev.Timestamp.UTC().Format("2006-01-02 15:04:05 MST"))
		}
		b.WriteString("\n\n")
		writeTextBlocks(b, ev.Content)
	case "tool_call":
		name := ev.ToolName
		if name == "" {
			name = "tool"
		}
		fmt.Fprintf(b, "> 🔧 **tool call** `%s`\n\n", name)
	case "tool_result":
		b.WriteString("> ↳ **tool result**\n\n")
		writeTextBlocks(b, ev.Content)
	case "system_note":
		var parts []string
		for _, c := range ev.Content {
			if c.Type == "text" && c.Text != "" {
				parts = append(parts, c.Text)
			}
		}
		if len(parts) > 0 {
			fmt.Fprintf(b, "_%s_\n\n", strings.Join(parts, " "))
		}
	}
}

func writeTextBlocks(b *strings.Builder, blocks []acf.ContentBlock) {
	for _, c := range blocks {
		if c.Type == "text" && c.Text != "" {
			b.WriteString(strings.TrimRight(c.Text, "\n"))
			b.WriteString("\n\n")
		}
	}
}

func roleTitle(role string) string {
	switch role {
	case "user":
		return "User"
	case "assistant":
		return "Assistant"
	case "system":
		return "System"
	case "":
		return "Note"
	default:
		return strings.ToUpper(role[:1]) + role[1:]
	}
}

// conversationDocFilename returns a stable, collision-resistant transcript
// filename: "<sourceAgent>-<shortid>.md".
func conversationDocFilename(sourceAgent, artifactID string) string {
	return conversationDocFilenameForBranch(sourceAgent, artifactID, acf.MainBranch)
}

func conversationDocFilenameForBranch(sourceAgent, artifactID, branch string) string {
	short := artifactID
	if len(short) > 12 {
		short = short[:12]
	}
	src := sourceAgent
	if src == "" {
		src = "agent"
	}
	branch = ruleBranchName(branch)
	if branch != acf.MainBranch {
		return src + "-" + short + "-" + branch + ".md"
	}
	return src + "-" + short + ".md"
}
