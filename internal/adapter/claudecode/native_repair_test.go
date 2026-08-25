package claudecode

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
	"github.com/stretchr/testify/require"
)

// divergedNativeFixture reproduces the population R4 exists for, and the one
// the shipped code could not heal at all: the user's OWN Claude transcript,
// continued in place, against a canonical plan that already holds those turns
// plus foreign ones the file has never seen. Neither side is a prefix of the
// other, so the native writer can only ever report SessionDeclineDiverged.
//
// The file is built with the row shapes a real transcript carries — uuid-less
// preamble rows, uuid-bearing attachment/system bridges between conversational
// rows — because both are exactly what a rebuild can destroy.
type divergedNativeFixture struct {
	adapter   *Adapter
	art       acf.Artifact
	dest      string
	sessionID string
	canonical []acf.TextTurn
	native    []acf.TextTurn
}

type nativeRowSpec struct {
	kind string // "user", "assistant", "attachment", "system"
	text string
}

// writeNativeClaudeTranscriptRows writes a Claude-shaped native transcript: two
// uuid-less preamble rows, then the requested chain (every uuid-bearing row
// parented on the previous uuid-bearing row), then a last-prompt naming the
// final conversational uuid.
func writeNativeClaudeTranscriptRows(t *testing.T, path, sessionID, cwd string, rows []nativeRowSpec) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	base := time.Date(2026, 7, 31, 10, 0, 0, 0, time.UTC)
	lines := []string{
		mustJSONLine(t, map[string]any{"type": "mode", "mode": "default", "sessionId": sessionID}),
		mustJSONLine(t, map[string]any{
			"type": "file-history-snapshot", "messageId": "msg-history", "isSnapshotUpdate": false,
			"snapshot": map[string]any{"trackedFileBackups": map[string]any{}},
		}),
	}
	parent := ""
	lastConversational := ""
	for i, spec := range rows {
		uuid := deterministicUUID(sessionID+":native-fixture:"+spec.kind+":"+spec.text, i)
		ts := base.Add(time.Duration(i) * time.Second).UTC().Format(time.RFC3339Nano)
		row := map[string]any{
			"type": spec.kind, "uuid": uuid, "parentUuid": parentOrNil(parent),
			"sessionId": sessionID, "timestamp": ts, "cwd": cwd, "isSidechain": false,
			"version": "2.1.0",
		}
		switch spec.kind {
		case "user":
			row["message"] = map[string]any{"role": "user", "content": spec.text}
			row["userType"] = "external"
		case "assistant":
			row["message"] = map[string]any{
				"role": "assistant", "type": "message",
				"content": []any{map[string]any{"type": "text", "text": spec.text}},
				"model":   "claude-opus-4-8",
			}
		case "attachment":
			// A real attachment row carries no message and no top-level content,
			// so it encodes no canonical event — it is a pure parent bridge.
			row["attachment"] = map[string]any{"type": "queued_command", "text": spec.text}
			row["userType"] = "external"
		case "system":
			row["subtype"] = "stop_hook_summary"
			row["isMeta"] = true
		default:
			t.Fatalf("unsupported native fixture row kind %q", spec.kind)
		}
		lines = append(lines, mustJSONLine(t, row))
		parent = uuid
		if spec.kind == "user" || spec.kind == "assistant" {
			lastConversational = uuid
		}
	}
	lines = append(lines, mustJSONLine(t, map[string]any{
		"type": "last-prompt", "lastPrompt": "", "leafUuid": lastConversational, "sessionId": sessionID,
	}))
	require.NoError(t, os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), claudeSessionFileMode))
}

func mustJSONLine(t *testing.T, row map[string]any) string {
	t.Helper()
	encoded, err := json.Marshal(row)
	require.NoError(t, err)
	return string(encoded)
}

func newDivergedNativeFixture(t *testing.T, repair bool) divergedNativeFixture {
	t.Helper()
	home := t.TempDir()
	a := New()
	a.HomeDir = home
	a.RepairForkedMirrors = repair

	sessionID := "6a1f0f3a-77c2-4a2f-9f1b-2f0d0c0b0a01"
	dest := filepath.Join(home, ".claude", "projects", encodeProjectDir(home), sessionID+".jsonl")
	writeNativeClaudeTranscriptRows(t, dest, sessionID, home, []nativeRowSpec{
		{kind: "attachment", text: "queued"},
		{kind: "user", text: "What is the size of Neptune?"},
		{kind: "system", text: ""},
		{kind: "assistant", text: "About four times Earth's diameter."},
		{kind: "user", text: "What is the closest planet to Neptune?"},
		{kind: "assistant", text: "Uranus."},
	})

	native := []acf.TextTurn{
		{Role: "user", Text: "What is the size of Neptune?"},
		{Role: "assistant", Text: "About four times Earth's diameter."},
		{Role: "user", Text: "What is the closest planet to Neptune?"},
		{Role: "assistant", Text: "Uranus."},
	}
	// Canonical after the import absorb: the two foreign turns are linearized
	// BEFORE the native continuation, which is precisely why neither side is a
	// prefix of the other and why appending can never converge them.
	canonical := []acf.TextTurn{
		native[0], native[1],
		{Role: "user", Text: "What is the temperature on Neptune?"},
		{Role: "assistant", Text: "Around -214 C."},
		native[2], native[3],
	}
	art := acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion,
		ArtifactID:       acf.NewID(),
		Kind:             acf.KindConversation,
		Scope:            acf.ScopeGlobal,
		SourcePath:       dest,
		CreatedAt:        time.Date(2026, 7, 31, 10, 0, 0, 0, time.UTC),
		UpdatedAt:        time.Date(2026, 7, 31, 10, 30, 0, 0, time.UTC),
	}
	return divergedNativeFixture{
		adapter: a, art: art, dest: dest, sessionID: sessionID,
		canonical: canonical, native: native,
	}
}

// The fixture must actually be the diverged native shape, or every assertion
// below tests something else.
func TestDivergedNativeFixture_IsTheOwnersShape(t *testing.T) {
	fx := newDivergedNativeFixture(t, false)
	plan, ok, err := fx.adapter.conversationSessionPlan(
		fx.art, canonicalConversationHead(t, fx.art.ArtifactID, fx.canonical...))
	require.NoError(t, err)
	require.True(t, ok)
	require.True(t, plan.nativeOrigin)
	require.True(t, plan.nativeSource, "the file must authenticate as its own session")
	require.False(t, plan.nativeWritable)
	require.Equal(t, adapter.SessionDeclineDiverged, plan.declineReason)
	require.Equal(t, fx.dest, plan.dest)
}

// R4, the red test: with the repair authorized, a diverged NATIVE session whose
// every conversational row is provably reproducible from the canonical plan is
// rebuilt in place. Before this change mirror_repair.go refused every
// native-origin plan outright, so this population had no repair route at all.
func TestMaterializeConversationSession_RepairsDivergedNativeSession(t *testing.T) {
	fx := newDivergedNativeFixture(t, true)
	before, err := os.ReadFile(fx.dest)
	require.NoError(t, err)
	beforeInfo := fileIdentity(t, fx.dest)

	path, ok, reason, err := fx.adapter.materializeConversationSession(
		fx.art, canonicalConversationHead(t, fx.art.ArtifactID, fx.canonical...), "codex", nil, nil)
	require.NoError(t, err)
	require.True(t, ok, "a containment-proven native divergence must be repaired, not declined")
	require.Equal(t, adapter.SessionDeclineUnspecified, reason)
	require.Equal(t, fx.dest, path)

	after, err := os.ReadFile(fx.dest)
	require.NoError(t, err)
	require.NotEqual(t, string(before), string(after))
	require.True(t, os.SameFile(beforeInfo, fileIdentity(t, fx.dest)),
		"the rebuild must preserve the inode Claude Code opened")

	// The user's own transcript now holds every turn from every agent.
	projection, err := parseClaudeVisibleLeaf(after)
	require.NoError(t, err)
	require.True(t, projection.spans())
	require.Equal(t, fx.canonical, projection.turns)

	// An Aplexica thread stamp on a pristine native source is a permanent
	// contradiction (claudeNativeSourceSessionPlan reports graph_malformed for
	// it), so a stamped rebuild would convert a repairable file into an
	// unrepairable one on the very next pass.
	require.NotContains(t, string(after), "aplexicaThreadId")
	state, _ := encodeCanonicalInto(after, 0, claudeCanonicalState{})
	require.False(t, state.hasExplicitThreadStamp)
	require.Equal(t, fx.sessionID, state.sessionID)

	// The repaired file must re-plan as a WRITABLE native source, or the next
	// pass would decline it again for a different reason and the repair would
	// have moved the artifact from one permanent decline to another.
	plan, ok, err := fx.adapter.conversationSessionPlan(
		fx.art, canonicalConversationHead(t, fx.art.ArtifactID, fx.canonical...))
	require.NoError(t, err)
	require.True(t, ok)
	require.True(t, plan.nativeSource)
	require.True(t, plan.nativeWritable)

	// The user's file-undo history for this session is a user-visible feature
	// the rebuild has no way to regenerate. Carrying the uuid-less rows through
	// verbatim is what keeps it.
	require.Contains(t, string(after), `"file-history-snapshot"`)
	require.Contains(t, string(after), `"mode"`)

	// Claude Code appends a child of its IN-MEMORY leaf, so every row the
	// containment proof matched must keep the uuid it already carried.
	for _, uuid := range nativeConversationalUUIDs(t, before) {
		require.Contains(t, string(after), uuid,
			"a contained row's uuid must survive the rebuild")
	}

	// Idempotent: the next pass matches and writes nothing.
	secondInfo := fileIdentity(t, fx.dest)
	_, ok, reason, err = fx.adapter.materializeConversationSession(
		fx.art, canonicalConversationHead(t, fx.art.ArtifactID, fx.canonical...), "codex", nil, nil)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, adapter.SessionDeclineUnspecified, reason)
	repeat, err := os.ReadFile(fx.dest)
	require.NoError(t, err)
	require.Equal(t, string(after), string(repeat), "a successful repair must be terminal")
	require.True(t, os.SameFile(secondInfo, fileIdentity(t, fx.dest)))
}

// The pre-image is MANDATORY on this path, unlike the synthetic rebuild where
// it is insurance. The rebuild preserves every row and every uuid, but it does
// FLATTEN the graph — a file that branched comes back as one chain — and the
// copy under ~/.aplexica/quarantine is the only way back to the original
// topology. It must never land inside ~/.claude, where it would appear as a
// second /resume entry for the same thread.
func TestRepairDivergedNativeSession_PreservesAPreimageOutsideClaude(t *testing.T) {
	fx := newDivergedNativeFixture(t, true)
	before, err := os.ReadFile(fx.dest)
	require.NoError(t, err)

	_, ok, _, err := fx.adapter.materializeConversationSession(
		fx.art, canonicalConversationHead(t, fx.art.ArtifactID, fx.canonical...), "codex", nil, nil)
	require.NoError(t, err)
	require.True(t, ok)

	root := filepath.Join(fx.adapter.HomeDir, ".aplexica", "quarantine", "claude-conversations")
	var preimages []string
	require.NoError(t, filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !info.IsDir() && strings.HasSuffix(path, ".diverged.jsonl") {
			preimages = append(preimages, path)
		}
		return nil
	}))
	require.Len(t, preimages, 1, "exactly one pre-image of the pre-repair bytes")
	saved, err := os.ReadFile(preimages[0])
	require.NoError(t, err)
	require.Equal(t, string(before), string(saved))
}

// Flag OFF must reproduce today's behaviour byte for byte. This is the whole
// safety story for shipping the repair: on a stock device nothing above fires.
func TestMaterializeConversationSession_DivergedNativeUnchangedWhenRepairDisabled(t *testing.T) {
	fx := newDivergedNativeFixture(t, false)
	before, err := os.ReadFile(fx.dest)
	require.NoError(t, err)

	path, ok, reason, err := fx.adapter.materializeConversationSession(
		fx.art, canonicalConversationHead(t, fx.art.ArtifactID, fx.canonical...), "codex", nil, nil)
	require.NoError(t, err)
	require.False(t, ok)
	require.Equal(t, adapter.SessionDeclineDiverged, reason)
	require.Equal(t, fx.dest, path)

	after, err := os.ReadFile(fx.dest)
	require.NoError(t, err)
	require.Equal(t, string(before), string(after))
	require.NoDirExists(t, filepath.Join(fx.adapter.HomeDir, ".aplexica", "quarantine"))
}

// Only SessionDeclineDiverged opens the door. native_ahead is transient and its
// pending import is the authority; race must never be terminaled;
// graph_malformed means the file could not be authenticated as this session at
// all. Each keeps declining exactly as it does today.
func TestMaterializeConversationSession_NativeRepairDoorIsDivergedOnly(t *testing.T) {
	t.Run("native ahead", func(t *testing.T) {
		fx := newDivergedNativeFixture(t, true)
		before, err := os.ReadFile(fx.dest)
		require.NoError(t, err)
		// Canonical is a strict prefix of the file: the file is genuinely ahead.
		ahead := fx.native[:2]
		_, ok, reason, err := fx.adapter.materializeConversationSession(
			fx.art, canonicalConversationHead(t, fx.art.ArtifactID, ahead...), "codex", nil, nil)
		require.NoError(t, err)
		require.False(t, ok)
		require.Equal(t, adapter.SessionDeclineNativeAhead, reason)
		after, err := os.ReadFile(fx.dest)
		require.NoError(t, err)
		require.Equal(t, string(before), string(after))
	})

	t.Run("graph malformed", func(t *testing.T) {
		fx := newDivergedNativeFixture(t, true)
		// A thread stamp on a pristine native source is the contradiction
		// claudeNativeSourceSessionPlan refuses outright.
		require.NoError(t, appendClaudeTestRows(fx.dest, []byte(mustJSONLine(t, map[string]any{
			"type": "ai-title", "aiTitle": "x", "sessionId": fx.sessionID,
			"aplexicaThreadId": fx.art.ArtifactID, "aplexicaBranchId": acf.MainBranch,
		})+"\n")))
		before, err := os.ReadFile(fx.dest)
		require.NoError(t, err)
		_, ok, reason, err := fx.adapter.materializeConversationSession(
			fx.art, canonicalConversationHead(t, fx.art.ArtifactID, fx.canonical...), "codex", nil, nil)
		require.NoError(t, err)
		require.False(t, ok)
		require.Equal(t, adapter.SessionDeclineGraphMalformed, reason)
		after, err := os.ReadFile(fx.dest)
		require.NoError(t, err)
		require.Equal(t, string(before), string(after))
	})
}

// The containment proof is the entire safety argument, so every shape it
// refuses must leave the user's transcript byte-identical. These are the same
// classes claudeMirrorRowsContained already enumerates, asserted here through
// the NATIVE door, where the file being protected is the user's own.
func TestRepairDivergedNativeSession_DeclinesWhatContainmentRefuses(t *testing.T) {
	for _, tc := range []struct {
		name string
		row  func(fx divergedNativeFixture) map[string]any
	}{
		{
			name: "row canonical never saw",
			row: func(divergedNativeFixture) map[string]any {
				return map[string]any{"message": map[string]any{
					"role": "user", "content": "a question nobody imported"}, "type": "user"}
			},
		},
		{
			name: "row normalizing to empty",
			row: func(divergedNativeFixture) map[string]any {
				return map[string]any{"message": map[string]any{
					"role": "user", "content": "# Project memory\nnever regenerate me"}, "type": "user"}
			},
		},
		{
			name: "image beside its caption",
			row: func(fx divergedNativeFixture) map[string]any {
				return map[string]any{"type": "user", "message": map[string]any{
					"role": "user", "content": []any{
						map[string]any{"type": "text", "text": fx.canonical[2].Text},
						map[string]any{"type": "image", "source": map[string]any{"data": "AAAA"}},
					}}}
			},
		},
		{
			name: "assistant row with real thinking",
			row: func(divergedNativeFixture) map[string]any {
				return map[string]any{"type": "assistant", "message": map[string]any{
					"role": "assistant", "model": "claude-opus-4-8", "content": []any{
						map[string]any{"type": "thinking", "thinking": "weighing it up"},
					}}}
			},
		},
		{
			name: "sidechain row",
			row: func(divergedNativeFixture) map[string]any {
				return map[string]any{"type": "user", "isSidechain": true, "message": map[string]any{
					"role": "user", "content": "sub-agent prompt"}}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newDivergedNativeFixture(t, true)
			row := tc.row(fx)
			row["uuid"] = "extra-row-uuid"
			row["parentUuid"] = lastClaudeConversationalUUID(mustReadFile(t, fx.dest))
			row["sessionId"] = fx.sessionID
			require.NoError(t, appendClaudeTestRows(fx.dest, []byte(mustJSONLine(t, row)+"\n")))
			before := mustReadFile(t, fx.dest)

			_, ok, _, err := fx.adapter.materializeConversationSession(
				fx.art, canonicalConversationHead(t, fx.art.ArtifactID, fx.canonical...), "codex", nil, nil)
			require.NoError(t, err)
			require.False(t, ok)
			require.Equal(t, string(before), string(mustReadFile(t, fx.dest)),
				"a refused containment proof must leave the transcript untouched")
		})
	}
}

// A torn trailing row is a writer mid-append, never a permanent state. The
// repair must decline it as a race rather than rewriting over a live writer.
func TestRepairDivergedNativeSession_DeclinesTornTrailingRow(t *testing.T) {
	fx := newDivergedNativeFixture(t, true)
	f, err := os.OpenFile(fx.dest, os.O_WRONLY|os.O_APPEND, 0)
	require.NoError(t, err)
	_, err = f.WriteString(`{"type":"user","uuid":"torn"`)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	before := mustReadFile(t, fx.dest)

	_, ok, reason, err := fx.adapter.materializeConversationSession(
		fx.art, canonicalConversationHead(t, fx.art.ArtifactID, fx.canonical...), "codex", nil, nil)
	require.NoError(t, err)
	require.False(t, ok)
	require.Equal(t, adapter.SessionDeclineRace, reason)
	require.Equal(t, string(before), string(mustReadFile(t, fx.dest)))
}

// nativeRepairPolicy fails closed on every axis, and the scope limit is
// enforced at the commit site rather than inferred from control flow.
func TestNativeRepairPolicy_FailsClosed(t *testing.T) {
	fx := newDivergedNativeFixture(t, true)
	plan, ok, err := fx.adapter.conversationSessionPlan(
		fx.art, canonicalConversationHead(t, fx.art.ArtifactID, fx.canonical...))
	require.NoError(t, err)
	require.True(t, ok)

	policy := fx.adapter.nativeRepairPolicy(plan, fx.art.ArtifactID)
	require.True(t, policy.repairDivergedNative)
	require.Equal(t, plan.dest, policy.nativeDest)
	require.False(t, policy.repairForkedMirror, "the native policy authorizes no synthetic rewrite")

	off := New()
	off.HomeDir = fx.adapter.HomeDir
	require.Equal(t, claudeMirrorRepairPolicy{}, off.nativeRepairPolicy(plan, fx.art.ArtifactID),
		"the feature switch alone must reduce the policy to its inert zero value")

	noHome := &Adapter{RepairForkedMirrors: true}
	require.Equal(t, claudeMirrorRepairPolicy{}, noHome.nativeRepairPolicy(plan, fx.art.ArtifactID))

	notNative := plan
	notNative.nativeSource = false
	require.Equal(t, claudeMirrorRepairPolicy{},
		fx.adapter.nativeRepairPolicy(notNative, fx.art.ArtifactID))

	// The commit site re-checks the destination, so a policy naming a different
	// pathname than the one being written authorizes nothing.
	other := filepath.Join(filepath.Dir(fx.dest), "somebody-elses-session.jsonl")
	writeNativeClaudeTranscriptRows(t, other, "somebody-elses-session", fx.adapter.HomeDir,
		[]nativeRowSpec{{kind: "user", text: "hello"}})
	before := mustReadFile(t, other)
	repaired, err := fx.adapter.repairDivergedNativeSession(
		other, fx.canonical, fx.sessionID, fx.adapter.HomeDir, fx.art.UpdatedAt, policy)
	require.NoError(t, err)
	require.False(t, repaired)
	require.Equal(t, string(before), string(mustReadFile(t, other)))
}

// claudeNativeSessionMatches is the read-back verification for the native
// rebuild. Identity is the native sessionId and the ABSENCE of a thread stamp,
// so it can never accept a synthetic mirror.
func TestClaudeNativeSessionMatches(t *testing.T) {
	fx := newDivergedNativeFixture(t, false)
	raw := mustReadFile(t, fx.dest)

	matches, err := claudeNativeSessionMatches(raw, fx.native, fx.sessionID)
	require.NoError(t, err)
	require.True(t, matches)

	matches, err = claudeNativeSessionMatches(raw, fx.canonical, fx.sessionID)
	require.NoError(t, err)
	require.False(t, matches, "turn equality is required")

	matches, err = claudeNativeSessionMatches(raw, fx.native, "a-different-session")
	require.NoError(t, err)
	require.False(t, matches, "identity is the native sessionId")

	stamped := append(append([]byte(nil), raw...), []byte(mustJSONLine(t, map[string]any{
		"type": "custom-title", "customTitle": "x", "sessionId": fx.sessionID,
		"aplexicaThreadId": "some-thread", "aplexicaBranchId": acf.MainBranch,
	})+"\n")...)
	matches, err = claudeNativeSessionMatches(stamped, fx.native, fx.sessionID)
	require.NoError(t, err)
	require.False(t, matches, "a thread-stamped file is not a native session")
}

func nativeConversationalUUIDs(t *testing.T, raw []byte) []string {
	t.Helper()
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var row struct {
			Type string `json:"type"`
			UUID string `json:"uuid"`
		}
		require.NoError(t, json.Unmarshal([]byte(line), &row))
		if (row.Type == "user" || row.Type == "assistant") && row.UUID != "" {
			out = append(out, row.UUID)
		}
	}
	return out
}
