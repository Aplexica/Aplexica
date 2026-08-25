package codex

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
)

func TestMaterializeExtendsOriginatingRolloutInsteadOfCreatingATwin(t *testing.T) {
	home := t.TempDir()
	a := &Adapter{HomeDir: home}
	day := filepath.Join(home, ".codex", "sessions", "2026", "01", "02")
	if err := os.MkdirAll(day, 0o755); err != nil {
		t.Fatal(err)
	}
	native := filepath.Join(day, "rollout-2026-01-02T03-04-05-019e0000-0000-7000-8000-000000000101.jsonl")
	raw := `{"timestamp":"2026-01-02T03:04:05.000Z","type":"session_meta","payload":{"session_id":"019e0000-0000-7000-8000-000000000101","id":"019e0000-0000-7000-8000-000000000101","cwd":"/Users/exampleuser","originator":"codex-tui"}}
{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"text","text":"first question"}]}}
{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"text","text":"First answer."}]}}
`
	if err := os.WriteFile(native, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	art, head := codexNativeOriginArtifact(t, "019e0000-0000-7000-8000-000000000102", native,
		[]acf.TextTurn{
			{Role: "user", Text: "first question"},
			{Role: "assistant", Text: "First answer."},
			{Role: "user", Text: "follow-up question"},
		})

	dest, ok, err := a.MaterializeConversationSession(art, head, "claude-code")
	if err != nil || !ok {
		t.Fatalf("materialize: dest=%q ok=%v err=%v", dest, ok, err)
	}
	if dest != native {
		t.Fatalf("materialized into %q, want the originating rollout %q", dest, native)
	}
	// Recursively count under the whole sessions root, not just day: the
	// fixture's native rollout lives under 2026/01/02 but the artifact's
	// CreatedAt (and therefore a generated twin's would-be day directory)
	// is 2026/01/03 — a sibling directory a plain ReadDir(day) would never
	// inspect, silently missing a stray twin created there.
	rollouts := codexRolloutFilesUnder(t, a.sessionsDir())
	if len(rollouts) != 1 {
		t.Fatalf("thread produced %d rollouts %v, want exactly 1", len(rollouts), rollouts)
	}
	after, err := os.ReadFile(native)
	if err != nil {
		t.Fatal(err)
	}
	got := acf.ExtractTextTurns(mustEncodeCanonical(t, after))
	want := []acf.TextTurn{
		{Role: "user", Text: "first question"},
		{Role: "assistant", Text: "First answer."},
		{Role: "user", Text: "follow-up question"},
	}
	if !acf.TextTurnsEqual(got, want) {
		t.Fatalf("rollout turns = %+v, want %+v", got, want)
	}
}

func TestMaterializeDeclinesWhenOriginatingRolloutIsAhead(t *testing.T) {
	home := t.TempDir()
	a := &Adapter{HomeDir: home}
	day := filepath.Join(home, ".codex", "sessions", "2026", "01", "02")
	if err := os.MkdirAll(day, 0o755); err != nil {
		t.Fatal(err)
	}
	native := filepath.Join(day, "rollout-2026-01-02T03-04-05-019e0000-0000-7000-8000-000000000101.jsonl")
	raw := `{"timestamp":"2026-01-02T03:04:05.000Z","type":"session_meta","payload":{"session_id":"019e0000-0000-7000-8000-000000000101","id":"019e0000-0000-7000-8000-000000000101","cwd":"/Users/exampleuser","originator":"codex-tui"}}
{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"text","text":"an unimported native turn"}]}}
`
	if err := os.WriteFile(native, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	art, head := codexNativeOriginArtifact(t, "019e0000-0000-7000-8000-000000000102", native,
		[]acf.TextTurn{{Role: "user", Text: "a different canonical turn"}})

	before, err := os.ReadFile(native)
	if err != nil {
		t.Fatal(err)
	}
	_, ok, err := a.MaterializeConversationSession(art, head, "claude-code")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("a divergent originating rollout must decline, not be overwritten")
	}
	after, err := os.ReadFile(native)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("declining must leave the native rollout byte-identical")
	}
	rollouts := codexRolloutFilesUnder(t, a.sessionsDir())
	if len(rollouts) != 1 {
		t.Fatalf("declining must not create a second rollout, found %d %v", len(rollouts), rollouts)
	}
}

func TestMaterializeIgnoresRemoteOriginArtifactSourcePath(t *testing.T) {
	home := t.TempDir()
	a := &Adapter{HomeDir: home}
	day := filepath.Join(home, ".codex", "sessions", "2026", "01", "02")
	if err := os.MkdirAll(day, 0o755); err != nil {
		t.Fatal(err)
	}
	native := filepath.Join(day, "rollout-2026-01-02T03-04-05-019e0000-0000-7000-8000-000000000101.jsonl")
	raw := `{"timestamp":"2026-01-02T03:04:05.000Z","type":"session_meta","payload":{"session_id":"019e0000-0000-7000-8000-000000000101","id":"019e0000-0000-7000-8000-000000000101","cwd":"/Users/exampleuser","originator":"codex-tui"}}
{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"text","text":"first question"}]}}
{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"text","text":"First answer."}]}}
`
	if err := os.WriteFile(native, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(native)
	if err != nil {
		t.Fatal(err)
	}

	art, head := codexNativeOriginArtifact(t, "019e0000-0000-7000-8000-000000000102", native,
		[]acf.TextTurn{
			{Role: "user", Text: "first question"},
			{Role: "assistant", Text: "First answer."},
			{Role: "user", Text: "follow-up question"},
		})
	// A remote artifact carries the ORIGINATING device's own absolute path.
	// On two machines with the same account name that path is lexically
	// identical to a path on this device, so containment alone (the old,
	// insufficient gate) would wrongly let it claim this device's unrelated
	// local rollout that just happens to sit at the same pathname.
	// RemoteOriginDeviceID is what proves SourcePath is not this device's
	// own identity.
	art.RemoteOriginDeviceID = "peer-mac-mini"

	dest, ok, err := a.MaterializeConversationSession(art, head, "claude-code")
	if err != nil {
		t.Fatal(err)
	}
	if dest == native {
		t.Fatalf("remote-origin artifact must not resolve to the local rollout %q (ok=%v)", native, ok)
	}

	after, err := os.ReadFile(native)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("a remote-origin artifact must leave an unrelated local rollout byte-identical")
	}
}

// TestAuthenticatedGeneratedConversationPathNeverAuthenticatesNativeOriginPlan
// pins the invariant that a native-origin plan can never be authenticated as
// a generated path. For a native-origin plan, plan.dest is overridden to the
// artifact's own recorded SourcePath, but plan.sessionID is still the derived
// codexSessionID(artifactID, branchID) — never the native rollout's own
// session_meta id. Nothing else in the code enforces that those two id spaces
// stay disjoint; this test is that enforcement. Even handed a ref that (by
// bug or future change) names this artifact/branch at this exact native path,
// the id-space mismatch alone must still refuse authentication.
func TestAuthenticatedGeneratedConversationPathNeverAuthenticatesNativeOriginPlan(t *testing.T) {
	home := t.TempDir()
	day := filepath.Join(home, ".codex", "sessions", "2026", "01", "02")
	if err := os.MkdirAll(day, 0o755); err != nil {
		t.Fatal(err)
	}
	native := filepath.Join(day, "rollout-2026-01-02T03-04-05-019e0000-0000-7000-8000-000000000101.jsonl")
	raw := []byte(`{"timestamp":"2026-01-02T03:04:05.000Z","type":"session_meta","payload":{"session_id":"019e0000-0000-7000-8000-000000000101","id":"019e0000-0000-7000-8000-000000000101","cwd":"/Users/exampleuser","originator":"codex-tui"}}
{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"text","text":"first question"}]}}
{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"text","text":"First answer."}]}}
`)
	if err := os.WriteFile(native, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	const artifactID = "019e0000-0000-7000-8000-000000000102"
	art, head := codexNativeOriginArtifact(t, artifactID, native,
		[]acf.TextTurn{
			{Role: "user", Text: "first question"},
			{Role: "assistant", Text: "First answer."},
		})
	art.AcfSchemaVersion = acf.SchemaVersion
	art.Scope = acf.ScopeGlobal
	art.UpdatedAt = art.CreatedAt

	store := &acf.Store{Root: filepath.Join(home, "store")}
	if err := store.Init(); err != nil {
		t.Fatal(err)
	}
	if err := store.WriteArtifact(art); err != nil {
		t.Fatal(err)
	}
	head.EventID = acf.NewID()
	head.Type = acf.EventTypeCreate
	head.Provenance = acf.Provenance{SourceAgent: "claude-code"}
	if err := store.AppendEvent(acf.KindConversation, head); err != nil {
		t.Fatal(err)
	}

	a := &Adapter{HomeDir: home}
	plan, ok, err := a.conversationSessionPlan(art, head)
	if err != nil || !ok {
		t.Fatalf("conversationSessionPlan: ok=%v err=%v", ok, err)
	}
	if !plan.nativeOrigin {
		t.Fatal("fixture must exercise the native-origin path")
	}
	if plan.dest != native {
		t.Fatalf("plan.dest = %q, want the originating rollout %q", plan.dest, native)
	}
	if nativeID := codexNativeSessionID(raw); nativeID == plan.sessionID {
		t.Fatalf("fixture is invalid: native session id %q must differ from the derived session id %q",
			nativeID, plan.sessionID)
	}

	ref := adapter.ThreadRef{ArtifactID: artifactID, BranchID: plan.branchID}
	if a.authenticatedGeneratedConversationPath(store, native, raw, ref) {
		t.Fatal("a native-origin plan's own rollout must never authenticate as a generated path")
	}
}

func codexNativeOriginArtifact(t *testing.T, artifactID, sourcePath string, turns []acf.TextTurn) (acf.Artifact, acf.Event) {
	t.Helper()
	events := make([]acf.ConversationEvent, 0, len(turns))
	for _, turn := range turns {
		events = append(events, acf.ConversationEvent{
			Type: "turn", Role: turn.Role,
			Content: []acf.ContentBlock{{Type: "text", Text: turn.Text}},
		})
	}
	payload, err := acf.EncodePayload(acf.ConversationPayload{Format: acf.ConversationFormatV1, Events: events})
	if err != nil {
		t.Fatal(err)
	}
	created := time.Date(2026, 1, 3, 3, 4, 6, 0, time.UTC)
	return acf.Artifact{
			ArtifactID: artifactID, Kind: acf.KindConversation,
			SourcePath: sourcePath, Name: filepath.Base(sourcePath), CreatedAt: created,
		}, acf.Event{
			ArtifactID: artifactID, Branch: acf.MainBranch, Timestamp: created, Payload: payload,
		}
}

// codexRolloutFilesUnder walks the whole sessions root (not a single day
// directory) so a stray twin materialized under any Y/M/D folder is caught,
// regardless of which day the fixture's native rollout happens to live in
// versus which day the artifact's CreatedAt would place a generated twin in.
func codexRolloutFilesUnder(t *testing.T, root string) []string {
	t.Helper()
	var found []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		name := info.Name()
		if strings.HasPrefix(name, "rollout-") && strings.HasSuffix(name, ".jsonl") {
			found = append(found, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return found
}

func mustEncodeCanonical(t *testing.T, raw []byte) []acf.ConversationEvent {
	t.Helper()
	events, err := EncodeCanonical(raw)
	if err != nil {
		t.Fatal(err)
	}
	return events
}
