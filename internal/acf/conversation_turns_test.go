package acf

import "testing"

func TestTurnsFromHermesBundleJSON(t *testing.T) {
	bundle := `{"session":{"id":"s1","source":"cli","started_at":100},"messages":[
		{"role":"system","content":"sys prompt","timestamp":100},
		{"role":"user","content":"What is the sample answer?","timestamp":101},
		{"role":"assistant","content":"The sample answer is ready.","timestamp":102},
		{"role":"tool","content":"{}","timestamp":103}
	]}`
	turns := TurnsFromHermesBundleJSON(bundle)
	if len(turns) != 2 {
		t.Fatalf("want 2 user/assistant turns, got %d: %+v", len(turns), turns)
	}
	if turns[0].Role != "user" || turns[0].Text != "What is the sample answer?" {
		t.Fatalf("unexpected first turn: %+v", turns[0])
	}
	if TurnsFromHermesBundleJSON("not json") != nil {
		t.Fatal("malformed JSON must yield nil")
	}
}

func TestConversationTextTurns_SupportsHermesBundlePayload(t *testing.T) {
	payload := ConversationPayload{
		Format: ConversationFormatHermesBundle,
		Content: `{"session":{"id":"s1","system_prompt":"hidden"},"messages":[
			{"role":"user","content":"What is the capital of France?"},
			{"role":"assistant","content":"Paris."}
		]}`,
	}
	turns, ok := ConversationTextTurns(payload)
	if !ok {
		t.Fatal("Hermes bundle payload should be a supported conversation text source")
	}
	if len(turns) != 2 || turns[0].Text != "What is the capital of France?" || turns[1].Text != "Paris." {
		t.Fatalf("unexpected turns: %+v", turns)
	}
}

// The synthetic preamble includes a literal "[SILENT]" inside the block. The
// matcher must run to the bracket that ends the block, not the first "]".
const scheduledPreambleFixture = `[IMPORTANT: You are running as a scheduled cron job. DELIVERY: Your final response will be automatically delivered to the user — do NOT use send_message or try to deliver the output yourself. SILENT: If there is genuinely nothing new to report, respond with exactly "[SILENT]" (nothing else) to suppress delivery. Never combine [SILENT] with content — either report your findings normally, or say [SILENT] and nothing more.]`

func TestStripScheduledTaskPreamble(t *testing.T) {
	task := "Process pending sample action notes."
	cases := []struct {
		name, in, want string
	}{
		{"preamble + task prompt", scheduledPreambleFixture + "\n\n" + task, task},
		{"pure preamble drops to empty", scheduledPreambleFixture, ""},
		{"no preamble unchanged", task, task},
		{"unrelated bracket lead unchanged", "[NOTE: x] hello", "[NOTE: x] hello"},
	}
	for _, c := range cases {
		if got := StripScheduledTaskPreamble(c.in); got != c.want {
			t.Fatalf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

// Scheduled sessions must surface their task prompt as the first turn —
// otherwise every synced scheduled session's title/slug/preview reads
// "[IMPORTANT: You are running as a sc…" in every agent's session list.
func TestExtractTextTurns_StripsScheduledPreamble(t *testing.T) {
	events := []ConversationEvent{
		{Type: EventTypeTurn, Role: "user", Content: []ContentBlock{{Type: "text", Text: scheduledPreambleFixture + "\n\nCheck the build status."}}},
		{Type: EventTypeTurn, Role: "assistant", Content: []ContentBlock{{Type: "text", Text: "All green."}}},
	}
	turns := ExtractTextTurns(events)
	if len(turns) != 2 || turns[0].Text != "Check the build status." {
		t.Fatalf("unexpected turns: %+v", turns)
	}
	// Stability: stripping is idempotent, so re-extracting materialized
	// (already-stripped) content reproduces the same turns (loop-safety).
	if got := StripScheduledTaskPreamble(turns[0].Text); got != turns[0].Text {
		t.Fatalf("strip not idempotent: %q", got)
	}
}

func TestExtractTextTurns_SkipsLocalCommandContext(t *testing.T) {
	events := []ConversationEvent{
		{Type: EventTypeTurn, Role: "user", Content: []ContentBlock{{Type: "text", Text: "<local-command-caveat>do not answer</local-command-caveat>"}}},
		{Type: EventTypeTurn, Role: "user", Content: []ContentBlock{{Type: "text", Text: "<command-name>/model</command-name>"}}},
		{Type: EventTypeTurn, Role: "user", Content: []ContentBlock{{Type: "text", Text: "<command-message>model</command-message>"}}},
		{Type: EventTypeTurn, Role: "user", Content: []ContentBlock{{Type: "text", Text: "<command-args></command-args>"}}},
		{Type: EventTypeTurn, Role: "user", Content: []ContentBlock{{Type: "text", Text: "<local-command-stdout>Set model to Opus</local-command-stdout>"}}},
		{Type: EventTypeTurn, Role: "user", Content: []ContentBlock{{Type: "text", Text: "What is the distance to Sun?"}}},
		{Type: EventTypeTurn, Role: "assistant", Content: []ContentBlock{{Type: "text", Text: "About 149.6 million kilometers."}}},
	}
	turns := ExtractTextTurns(events)
	if len(turns) != 2 {
		t.Fatalf("want 2 visible turns, got %d: %+v", len(turns), turns)
	}
	if turns[0].Text != "What is the distance to Sun?" {
		t.Fatalf("local command rows leaked into visible turns: %+v", turns)
	}
}

func TestExtractTextTurns_SkipsCodexHarnessAndKeepsActualAttachmentPrompt(t *testing.T) {
	events := []ConversationEvent{
		{Type: EventTypeTurn, Role: "user", Content: []ContentBlock{{Type: "text", Text: "You are /root, the primary agent in a team of agents collaborating to fulfill the user's goals."}}},
		{Type: EventTypeTurn, Role: "user", Content: []ContentBlock{{Type: "text", Text: "You are `/root`, the primary agent in a team of agents collaborating to fulfill the user's goals."}}},
		{Type: EventTypeTurn, Role: "user", Content: []ContentBlock{{Type: "text", Text: "<app-context>Injected desktop application context.</app-context>"}}},
		{Type: EventTypeTurn, Role: "user", Content: []ContentBlock{{Type: "text", Text: "<collaboration_mode>Injected collaboration mode.</collaboration_mode>"}}},
		{Type: EventTypeTurn, Role: "user", Content: []ContentBlock{{Type: "text", Text: "<multi_agent_mode>Proactive multi-agent delegation is active.</multi_agent_mode>"}}},
		{Type: EventTypeTurn, Role: "user", Content: []ContentBlock{{Type: "text", Text: "<recommended_plugins>Injected plugin inventory.</recommended_plugins>"}}},
		{Type: EventTypeTurn, Role: "user", Content: []ContentBlock{{Type: "text", Text: "<skills_instructions>Injected skill inventory.</skills_instructions>"}}},
		{Type: EventTypeTurn, Role: "user", Content: []ContentBlock{{Type: "text", Text: "# Files mentioned by the user:\n\n## screenshot.png\n\n## My request for Codex:\nFix the synchronized conversation subjects."}}},
		{Type: EventTypeTurn, Role: "assistant", Content: []ContentBlock{{Type: "text", Text: "Fixed."}}},
	}
	turns := ExtractTextTurns(events)
	if len(turns) != 2 {
		t.Fatalf("want 2 visible turns, got %d: %+v", len(turns), turns)
	}
	if turns[0].Text != "Fix the synchronized conversation subjects." {
		t.Fatalf("Codex harness or attachment inventory leaked into visible turns: %+v", turns)
	}
	if got := StripCodexAttachmentPreamble(turns[0].Text); got != turns[0].Text {
		t.Fatalf("attachment preamble stripping is not idempotent: %q", got)
	}
}

func TestTurnsFromHermesBundleJSON_SkipsLocalCommandContext(t *testing.T) {
	bundle := `{"session":{"id":"s1","source":"aplexica:canonical-import","started_at":100},"messages":[
		{"role":"user","content":"<command-name>/model</command-name>","timestamp":100},
		{"role":"user","content":"<local-command-stdout>Set model to Opus</local-command-stdout>","timestamp":101},
		{"role":"user","content":"What is the distance to Sun?","timestamp":102},
		{"role":"assistant","content":"About 149.6 million kilometers.","timestamp":103}
	]}`
	turns := TurnsFromHermesBundleJSON(bundle)
	if len(turns) != 2 {
		t.Fatalf("want 2 visible turns, got %d: %+v", len(turns), turns)
	}
	if turns[0].Text != "What is the distance to Sun?" {
		t.Fatalf("local command rows leaked into Hermes visible turns: %+v", turns)
	}
}
