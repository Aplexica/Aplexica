package claudecode

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
	"github.com/stretchr/testify/require"
)

// inspectRow is one UUID-bearing row with explicit parenting, so a test can
// build an origin-session fork in which the second
// conversational child of the fork node reached through a non-conversational
// system bridge row.
type inspectRow struct {
	kind   string // "user", "assistant", "system"
	text   string
	uuid   string
	parent string
}

func writeInspectSession(t *testing.T, path, sessionID, cwd string, rows []inspectRow, leaf string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	base := time.Date(2026, 1, 2, 3, 4, 0, 0, time.UTC)
	var lines []string
	for i, spec := range rows {
		row := map[string]any{
			"type": spec.kind, "uuid": spec.uuid, "parentUuid": parentOrNil(spec.parent),
			"sessionId": sessionID, "timestamp": base.Add(time.Duration(i) * time.Second).Format(time.RFC3339Nano),
			"cwd": cwd, "isSidechain": false, "version": "2.1.0",
		}
		switch spec.kind {
		case "user":
			row["message"] = map[string]any{"role": "user", "content": spec.text}
			row["userType"] = "external"
		case "assistant":
			row["message"] = map[string]any{
				"role": "assistant", "type": "message", "model": "claude-opus-4-8",
				"content": []any{map[string]any{"type": "text", "text": spec.text}},
			}
		case "system":
			row["subtype"] = "stop_hook_summary"
			row["isMeta"] = true
		default:
			t.Fatalf("unsupported inspect fixture row kind %q", spec.kind)
		}
		lines = append(lines, mustJSONLine(t, row))
	}
	lines = append(lines, mustJSONLine(t, map[string]any{
		"type": "last-prompt", "lastPrompt": "", "leafUuid": leaf, "sessionId": sessionID,
	}))
	require.NoError(t, os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), claudeSessionFileMode))
}

func newInspectFixture(t *testing.T) (*Adapter, acf.Artifact, string, string) {
	t.Helper()
	home := t.TempDir()
	a := New()
	a.HomeDir = home
	sessionID := "7b2e1c4d-88a3-4b5f-8e22-3a1b2c3d4e5f"
	dest := filepath.Join(home, ".claude", "projects", encodeProjectDir(home), sessionID+".jsonl")
	art := acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion,
		ArtifactID:       acf.NewID(),
		Kind:             acf.KindConversation,
		Scope:            acf.ScopeGlobal,
		SourcePath:       dest,
		CreatedAt:        time.Date(2026, 1, 2, 3, 4, 0, 0, time.UTC),
		UpdatedAt:        time.Date(2026, 1, 2, 3, 34, 0, 0, time.UTC),
	}
	return a, art, dest, sessionID
}

var inspectTurns = []acf.TextTurn{
	{Role: "user", Text: "What is the capital of Canada?"},
	{Role: "assistant", Text: "Ottawa."},
	{Role: "user", Text: "How many provinces does Canada have?"},
	{Role: "assistant", Text: "Ten, plus three territories."},
	{Role: "user", Text: "How big is Canada?"},
	{Role: "assistant", Text: "About 9.98 million square kilometres."},
}

// The trigger's whole reason to exist: a fork whose PHYSICAL rows equal the
// canonical plan reads as a perfectly writable EXACT plan in file order — only
// the resume walk exposes it. The inspector must report it non-reusable as
// forked_mirror, whatever the repair flag says, and must report the healthy
// exact file reusable — the loop-termination proof after a repair.
func TestInspectConversationSessionSource_ForkedFileIsNotReusable(t *testing.T) {
	for _, repair := range []bool{false, true} {
		a, art, dest, sessionID := newInspectFixture(t)
		a.RepairForkedMirrors = repair
		// The synthetic shape: u1<-a1, the foreign pair appended under a1, and the
		// user's next turn hanging from a1 THROUGH a system bridge row.
		writeInspectSession(t, dest, sessionID, a.HomeDir, []inspectRow{
			{kind: "user", text: inspectTurns[0].Text, uuid: "u1", parent: ""},
			{kind: "assistant", text: inspectTurns[1].Text, uuid: "a1", parent: "u1"},
			{kind: "user", text: inspectTurns[2].Text, uuid: "u2", parent: "a1"},
			{kind: "assistant", text: inspectTurns[3].Text, uuid: "a2", parent: "u2"},
			{kind: "system", uuid: "sys1", parent: "a1"},
			{kind: "user", text: inspectTurns[4].Text, uuid: "u3", parent: "sys1"},
			{kind: "assistant", text: inspectTurns[5].Text, uuid: "a3", parent: "u3"},
		}, "a3")
		head := canonicalConversationHead(t, art.ArtifactID, inspectTurns...)

		// The planner really is blind to this shape: file order equals the
		// plan, so it reports a writable exact plan. That blindness is why the
		// inspector exists, so pin it.
		plan, ok, err := a.conversationSessionPlan(art, head)
		require.NoError(t, err)
		require.True(t, ok)
		require.True(t, plan.nativeOrigin)
		require.True(t, plan.nativeWritable,
			"repair=%v: the file-order planner must see an exact plan — the blindness under test", repair)

		reusable, applicable, reason, err := a.InspectConversationSessionSource(art, head)
		require.NoError(t, err)
		require.True(t, applicable)
		require.False(t, reusable,
			"repair=%v: a forked origin session must never be reported reusable", repair)
		require.Equal(t, adapter.SessionDeclineForkedMirror, reason)
	}
}

func TestInspectConversationSessionSource_ExactAndAppendableAreReusable(t *testing.T) {
	linear := []inspectRow{
		{kind: "user", text: inspectTurns[0].Text, uuid: "u1", parent: ""},
		{kind: "assistant", text: inspectTurns[1].Text, uuid: "a1", parent: "u1"},
		{kind: "system", uuid: "sys1", parent: "a1"},
		{kind: "user", text: inspectTurns[2].Text, uuid: "u2", parent: "sys1"},
		{kind: "assistant", text: inspectTurns[3].Text, uuid: "a2", parent: "u2"},
	}

	t.Run("exact", func(t *testing.T) {
		a, art, dest, sessionID := newInspectFixture(t)
		writeInspectSession(t, dest, sessionID, a.HomeDir, linear, "a2")
		head := canonicalConversationHead(t, art.ArtifactID, inspectTurns[:4]...)
		reusable, applicable, reason, err := a.InspectConversationSessionSource(art, head)
		require.NoError(t, err)
		require.True(t, applicable)
		require.True(t, reusable, "an exact spanning file is the repaired end state and must not re-queue")
		require.Equal(t, adapter.SessionDeclineUnspecified, reason)
	})

	t.Run("appendable", func(t *testing.T) {
		a, art, dest, sessionID := newInspectFixture(t)
		writeInspectSession(t, dest, sessionID, a.HomeDir, linear, "a2")
		head := canonicalConversationHead(t, art.ArtifactID, inspectTurns...)
		reusable, applicable, reason, err := a.InspectConversationSessionSource(art, head)
		require.NoError(t, err)
		require.True(t, applicable)
		require.True(t, reusable, "a spanning prefix converges through the ordinary append")
		require.Equal(t, adapter.SessionDeclineUnspecified, reason)
	})
}

func TestInspectConversationSessionSource_ClassifiesNonTriggerStates(t *testing.T) {
	t.Run("native ahead", func(t *testing.T) {
		a, art, dest, sessionID := newInspectFixture(t)
		writeInspectSession(t, dest, sessionID, a.HomeDir, []inspectRow{
			{kind: "user", text: inspectTurns[0].Text, uuid: "u1", parent: ""},
			{kind: "assistant", text: inspectTurns[1].Text, uuid: "a1", parent: "u1"},
			{kind: "user", text: inspectTurns[2].Text, uuid: "u2", parent: "a1"},
			{kind: "assistant", text: inspectTurns[3].Text, uuid: "a2", parent: "u2"},
		}, "a2")
		head := canonicalConversationHead(t, art.ArtifactID, inspectTurns[:2]...)
		reusable, applicable, reason, err := a.InspectConversationSessionSource(art, head)
		require.NoError(t, err)
		require.True(t, applicable)
		require.False(t, reusable)
		require.Equal(t, adapter.SessionDeclineNativeAhead, reason)
	})

	t.Run("remote shell is not applicable", func(t *testing.T) {
		a, art, dest, sessionID := newInspectFixture(t)
		writeInspectSession(t, dest, sessionID, a.HomeDir, []inspectRow{
			{kind: "user", text: inspectTurns[0].Text, uuid: "u1", parent: ""},
		}, "u1")
		art.RemoteOriginDeviceID = "remote-device"
		head := canonicalConversationHead(t, art.ArtifactID, inspectTurns[:1]...)
		_, applicable, _, err := a.InspectConversationSessionSource(art, head)
		require.NoError(t, err)
		require.False(t, applicable,
			"a remote artifact never names a local origin session")
	})

	t.Run("oversized transcript is not judged", func(t *testing.T) {
		a, art, dest, sessionID := newInspectFixture(t)
		writeInspectSession(t, dest, sessionID, a.HomeDir, []inspectRow{
			{kind: "user", text: inspectTurns[0].Text, uuid: "u1", parent: ""},
			{kind: "assistant", text: inspectTurns[1].Text, uuid: "a1", parent: "u1"},
		}, "a1")
		restore := nativeRepairMaxBytes
		nativeRepairMaxBytes = 16
		defer func() { nativeRepairMaxBytes = restore }()
		head := canonicalConversationHead(t, art.ArtifactID, inspectTurns[:2]...)
		_, applicable, _, err := a.InspectConversationSessionSource(art, head)
		require.NoError(t, err)
		require.False(t, applicable,
			"a transcript past the repair's own size bound must not be judged read-only")
	})

	t.Run("foreign source path is not applicable", func(t *testing.T) {
		a, art, _, _ := newInspectFixture(t)
		art.SourcePath = filepath.Join(a.HomeDir, "elsewhere", "rollout.jsonl")
		head := canonicalConversationHead(t, art.ArtifactID, inspectTurns[:1]...)
		_, applicable, _, err := a.InspectConversationSessionSource(art, head)
		require.NoError(t, err)
		require.False(t, applicable,
			"a source outside ~/.claude/projects is not this adapter's origin session")
	})
}
